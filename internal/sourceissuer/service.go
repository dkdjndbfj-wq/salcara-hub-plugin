package sourceissuer

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"salcara/hub/internal/hub"
)

var requestID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type Service struct {
	cfg            Config
	key            ed25519.PrivateKey
	http           *http.Client
	path           string
	enrollmentPath string
	capacity       chan struct{}
	mu             sync.Mutex
	attempts       map[[32]byte]attempt
}

type attempt struct {
	count int
	start time.Time
}

// New copies all configuration and key material. The caller may supply a TLS
// transport (for an operator's private CA); redirects and timeouts stay fixed.
func New(cfg Config, privateKey ed25519.PrivateKey, transport http.RoundTripper) (*Service, error) {
	if cfg.validate() != nil || len(privateKey) != ed25519.PrivateKeySize {
		return nil, errConfig
	}
	// Reject malformed seed/public-key pairs before publishing an unusable signer.
	derived := ed25519.NewKeyFromSeed(privateKey.Seed())
	if !derived.Equal(privateKey) {
		return nil, errConfig
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, errConfig
	}
	var owned Config
	if json.Unmarshal(raw, &owned) != nil {
		return nil, errConfig
	}
	public := owned.public(privateKey.Public().(ed25519.PublicKey))
	policyRaw, _ := json.Marshal(hub.RemoteAccessPolicy{Mode: "restricted", Revision: "issuer-validation", Audience: "issuer-validation", Issuers: []hub.SourceIssuer{public}})
	if _, err := hub.ParseRemoteAccessPolicy(policyRaw); err != nil {
		return nil, errConfig
	}
	u, _ := url.Parse(cfg.AuthorizationURL)
	enrollmentPath := ""
	if owned.EnrollmentURL != "" {
		u, _ := url.Parse(owned.EnrollmentURL)
		enrollmentPath = u.Path
	}
	if transport == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		// Station credentials must not follow ambient HTTP(S)_PROXY settings.
		t.Proxy = nil
		transport = t
	}
	return &Service{cfg: owned, key: append(ed25519.PrivateKey(nil), privateKey...), path: u.Path, enrollmentPath: enrollmentPath,
		http:     &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		capacity: make(chan struct{}, 32), attempts: map[[32]byte]attempt{}}, nil
}

func (s *Service) PublicIssuer() hub.SourceIssuer {
	return s.cfg.public(s.key.Public().(ed25519.PublicKey))
}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func failure(w http.ResponseWriter, status int, code string) {
	reply(w, status, map[string]any{"code": code, "error": "无法签发远程来源授权"})
}

// allow bounds repeated credential checks. Unknown keys cannot evict a known
// key's counter, and a bounded table never retains API credentials themselves.
func (s *Service) allow(key string, now time.Time) bool {
	hash := sha256.Sum256([]byte(key))
	s.mu.Lock()
	defer s.mu.Unlock()
	old, exists := s.attempts[hash]
	if !exists || now.Sub(old.start) >= time.Minute {
		if len(s.attempts) >= 4096 {
			for k, entry := range s.attempts {
				if now.Sub(entry.start) >= time.Minute {
					delete(s.attempts, k)
				}
			}
			if !exists && len(s.attempts) >= 4096 {
				return false
			}
		}
		old = attempt{start: now}
	}
	if old.count >= 120 {
		return false
	}
	old.count++
	s.attempts[hash] = old
	return true
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	deviceEnrollment := s.enrollmentPath != "" && r.URL.Path == s.enrollmentPath
	if r.URL.Path != s.path && !deviceEnrollment || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		failure(w, 404, "issuer_not_found")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		failure(w, 405, "issuer_method_invalid")
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || r.Header.Get("Content-Encoding") != "" {
		failure(w, 415, "issuer_content_invalid")
		return
	}
	select {
	case s.capacity <- struct{}{}:
		defer func() { <-s.capacity }()
	default:
		failure(w, 429, "issuer_capacity")
		return
	}
	// Also bound slow request bodies when embedded in a server without deadlines.
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(8 * time.Second))
	defer controller.SetReadDeadline(time.Time{}) //nolint:errcheck
	// Snapshot plus a 4096-byte credential, including JSON escape expansion.
	limit := int64(hub.MaxSignedSourceGrantBytes)
	if deviceEnrollment {
		limit = 48 << 10
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		_ = r.Body.Close()
		w.Header().Set("Connection", "close")
		failure(w, 400, "issuer_request_invalid")
		return
	}
	if deviceEnrollment {
		s.serveEnrollment(w, r, raw)
		return
	}
	var envelope struct {
		Version    int             `json:"version"`
		Snapshot   json.RawMessage `json:"snapshot"`
		Credential json.RawMessage `json:"credential"`
	}
	var credential struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	if exact(raw, []string{"version", "snapshot", "credential"}, &envelope) != nil || envelope.Version != 1 || exact(envelope.Credential, []string{"type", "value"}, &credential) != nil || credential.Type != "api_key" || !validCredential(credential.Value) {
		failure(w, 400, "issuer_request_invalid")
		return
	}
	snapshot, err := hub.ParseSourceSnapshot(envelope.Snapshot)
	if err != nil || !s.validSnapshot(snapshot) {
		failure(w, 403, "issuer_scope_denied")
		return
	}
	var route *Route
	for i := range s.cfg.Routes {
		if s.cfg.Routes[i].APIBase == snapshot.APIBase {
			route = &s.cfg.Routes[i]
			break
		}
	}
	if route == nil {
		failure(w, 403, "issuer_scope_denied")
		return
	}
	if !s.allow(credential.Value, time.Now()) {
		failure(w, 429, "issuer_rate_limited")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	groupID, status := s.authenticate(ctx, route.VerifierURL, credential.Value)
	if status != http.StatusOK {
		failure(w, status, "issuer_account_unavailable")
		return
	}
	var allowed bool
	for _, group := range route.Groups {
		if group.ID == groupID && group.SourceType == snapshot.SourceType && contains(group.Audiences, snapshot.Audience) && contains(group.Agents, snapshot.Agent) && (len(group.Models) == 0 || contains(group.Models, snapshot.Model)) {
			allowed = true
			break
		}
	}
	if !allowed {
		failure(w, 403, "issuer_scope_denied")
		return
	}
	if r.Context().Err() != nil || ctx.Err() != nil {
		failure(w, 503, "issuer_account_unavailable")
		return
	}
	snapshot.IssuedAt = time.Now().Unix()
	snapshot.ExpiresAt = snapshot.IssuedAt + 120
	payload, err := json.Marshal(snapshot)
	if err != nil || len(payload) > hub.MaxSourceSnapshotBytes {
		failure(w, 500, "issuer_failed")
		return
	}
	reply(w, 200, hub.SignedSourceGrant{Payload: base64.RawURLEncoding.EncodeToString(payload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.key, payload))})
}

func validCredential(key string) bool {
	if len(key) < 1 || len(key) > 4096 {
		return false
	}
	for _, ch := range []byte(key) {
		if ch < 33 || ch > 126 {
			return false
		}
	}
	return true
}

func (s *Service) validSnapshot(v hub.SourceSnapshot) bool {
	public, _ := url.Parse(s.cfg.AuthorizationURL)
	if v.Version != 1 || v.Issuer != s.cfg.ID || v.ControlSurface != "cli" || v.Origin != public.Scheme+"://"+public.Host || (v.SourceType != "site_api" && v.SourceType != "verified_proxy") || v.IssuedAt != 0 || v.ExpiresAt != 0 || !requestID.MatchString(v.RequestID) || !strings.HasPrefix(v.ChallengeID, "challenge-") || v.ChallengeID == "challenge-" || len(v.CommandHash) != 64 || strings.ToLower(v.CommandHash) != v.CommandHash || v.Model != "" && !modelName(v.Model) {
		return false
	}
	if _, err := hex.DecodeString(v.CommandHash); err != nil {
		return false
	}
	for _, id := range []string{v.Audience, v.DeviceID, v.BindingID, v.CredentialHandle, v.Agent, v.ConfigRevision, v.PolicyRevision, v.ChallengeID} {
		if !identifier(id) {
			return false
		}
	}
	return true
}

func (s *Service) authenticate(ctx context.Context, endpoint, key string) (int64, int) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, http.StatusServiceUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, http.StatusServiceUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return 0, http.StatusForbidden
	}
	if resp.StatusCode != 200 {
		return 0, http.StatusServiceUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	var identity struct {
		GroupID int64 `json:"group_id"`
	}
	if err != nil || len(raw) > 4096 || exact(raw, []string{"group_id"}, &identity) != nil || identity.GroupID <= 0 {
		return 0, http.StatusServiceUnavailable
	}
	return identity.GroupID, http.StatusOK
}

// ReadPrivateKey accepts a bounded, unpadded base64url Ed25519 seed file.
// It returns constant errors so malformed private material cannot reach logs.
func ReadPrivateKey(r io.Reader) (ed25519.PrivateKey, error) {
	raw, err := io.ReadAll(io.LimitReader(r, 129))
	if err != nil || len(raw) > 128 {
		return nil, errors.New("invalid issuer signing key")
	}
	seed, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("invalid issuer signing key")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}
