package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// errUpstream means sub2api could not give a definitive answer (network
// error, 5xx, its own rate limit). Such results are never cached.
var errUpstream = errors.New("sub2api unavailable")

type authEntry struct {
	verdict authVerdict
	exp     time.Time
}

type authCall struct {
	done    chan struct{}
	verdict authVerdict
	err     error
}

type authVerdict uint8

const (
	authInvalid authVerdict = iota
	authAllowed
	authGroupDenied
)

// authenticator validates relay keys against the custom Sub2API key-group
// endpoint and caches
// the verdict keyed by sha256(key). The key itself is never stored.
type authenticator struct {
	cfg *Config

	mu       sync.Mutex
	cache    map[[32]byte]authEntry
	inflight map[[32]byte]*authCall
}

func newAuthenticator(cfg *Config) *authenticator {
	return &authenticator{cfg: cfg, cache: map[[32]byte]authEntry{}, inflight: map[[32]byte]*authCall{}}
}

// AccountID returns hex(sha256(key))[:24].
func AccountID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:24]
}

// cachedValid reports whether key is cached as valid.
func (a *authenticator) cachedValid(key string) bool {
	h := sha256.Sum256([]byte(key))
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.cache[h]
	return ok && e.verdict == authAllowed && time.Now().Before(e.exp)
}

// check returns whether key is a valid relay key. Concurrent checks of the same
// key share one upstream request.
func (a *authenticator) check(ctx context.Context, key string) (authVerdict, error) {
	h := sha256.Sum256([]byte(key))
	a.mu.Lock()
	if e, ok := a.cache[h]; ok && time.Now().Before(e.exp) {
		a.mu.Unlock()
		return e.verdict, nil
	}
	call, ok := a.inflight[h]
	if !ok {
		call = &authCall{done: make(chan struct{})}
		a.inflight[h] = call
		a.mu.Unlock()
		// Detached from the caller's context so one impatient client does not
		// fail the shared lookup for everyone else.
		call.verdict, call.err = a.query(key)
		a.mu.Lock()
		delete(a.inflight, h)
		if call.err == nil {
			ttl := a.cfg.InvalidTTL
			if call.verdict == authAllowed {
				ttl = a.cfg.ValidTTL
			}
			a.cache[h] = authEntry{verdict: call.verdict, exp: time.Now().Add(ttl)}
		}
		a.mu.Unlock()
		close(call.done)
		return call.verdict, call.err
	}
	a.mu.Unlock()
	select {
	case <-call.done:
		return call.verdict, call.err
	case <-ctx.Done():
		return authInvalid, ctx.Err()
	}
}

func (a *authenticator) query(key string) (authVerdict, error) {
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.AuthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.Sub2APIURL+"/v1/salcara-hub/key", nil)
	if err != nil {
		return authInvalid, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := a.cfg.HTTPClient.Do(req)
	if err != nil {
		return authInvalid, errUpstream
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		var identity struct {
			GroupID *int64 `json:"group_id"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&identity); err != nil {
			return authInvalid, errUpstream
		}
		if identity.GroupID == nil {
			return authGroupDenied, nil
		}
		for _, allowed := range a.cfg.AllowedGroupIDs {
			if allowed == *identity.GroupID {
				return authAllowed, nil
			}
		}
		return authGroupDenied, nil
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return authInvalid, nil
	default:
		// 429 (sub2api's own invalid-auth limiter), 5xx, anything unexpected.
		return authInvalid, errUpstream
	}
}

func (a *authenticator) sweep(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, e := range a.cache {
		if now.After(e.exp) {
			delete(a.cache, k)
		}
	}
}

// failLimiter counts failed authentications per client IP in fixed windows.
type failLimiter struct {
	limit  int
	window time.Duration

	mu sync.Mutex
	m  map[string]*failWindow
}

type failWindow struct {
	start time.Time
	count int
}

func newFailLimiter(limit int, window time.Duration) *failLimiter {
	return &failLimiter{limit: limit, window: window, m: map[string]*failWindow{}}
}

// limiterKey groups IPv6 clients by /64: one host usually owns a whole /64,
// so per-address counting would let a single attacker spray fresh addresses.
func limiterKey(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() != nil {
		return ip
	}
	return parsed.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// blocked reports whether this client exceeded its failure budget. A full
// table never blocks unknown clients: an attacker filling the table must not
// lock every legitimate phone and computer out.
func (l *failLimiter) blocked(ip string) bool {
	key := limiterKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.m[key]
	return ok && time.Since(w.start) < l.window && w.count >= l.limit
}

func (l *failLimiter) fail(ip string) {
	key := limiterKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	w, ok := l.m[key]
	if !ok || now.Sub(w.start) >= l.window {
		if !ok && len(l.m) >= 65536 {
			for k, old := range l.m {
				if now.Sub(old.start) >= l.window {
					delete(l.m, k)
				}
			}
			if len(l.m) >= 65536 {
				return
			}
		}
		l.m[key] = &failWindow{start: now, count: 1}
		return
	}
	w.count++
}

func (l *failLimiter) sweep(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, w := range l.m {
		if now.Sub(w.start) >= l.window {
			delete(l.m, ip)
		}
	}
}

// bearerKey extracts the key from "Authorization: Bearer <key>".
func bearerKey(r *http.Request) string {
	parts := strings.SplitN(r.Header.Get("Authorization"), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

// clientIP returns the peer IP, or the right-most X-Forwarded-For entry when
// the hub sits behind a trusted proxy (nginx appends $remote_addr last).
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
