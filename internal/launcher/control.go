package launcher

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

type control struct {
	manager     *manager
	tokenDigest [32]byte
}

func controlReply(w http.ResponseWriter, status int, value Status) bool {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(value) == nil
}
func (c *control) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if len(r.Header.Values("Authorization")) != 1 {
		controlReply(w, 401, Status{CurrentVersion: c.manager.cfg.BootstrapVersion, Status: "failed", Message: "private control authorization required"})
		return
	}
	method, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	digest := sha256.Sum256([]byte(token))
	if !ok || method != "Bearer" || len(token) != 64 || subtle.ConstantTimeCompare(digest[:], c.tokenDigest[:]) != 1 {
		controlReply(w, 401, Status{CurrentVersion: c.manager.cfg.BootstrapVersion, Status: "failed", Message: "private control authorization required"})
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		controlReply(w, 400, c.manager.snapshot())
		return
	}
	if r.URL.Path == "/status" && r.Method == http.MethodGet {
		controlReply(w, 200, c.manager.snapshot())
		return
	}
	if r.Method != http.MethodPost || (r.URL.Path != "/check" && r.URL.Path != "/apply") {
		controlReply(w, 404, c.manager.snapshot())
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(15 * time.Second))
	defer http.NewResponseController(w).SetReadDeadline(time.Time{})
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		controlReply(w, 400, c.manager.snapshot())
		return
	}
	if r.URL.Path == "/check" {
		if strings.TrimSpace(string(raw)) != "" {
			var empty struct{}
			if !strings.HasPrefix(strings.TrimSpace(string(raw)), "{") || strictDecode(raw, &empty) != nil {
				controlReply(w, 400, c.manager.snapshot())
				return
			}
		}
		controlReply(w, 200, c.manager.check())
		return
	}
	var in applyRequest
	if strictDecode(raw, &in) != nil || !in.Confirm {
		controlReply(w, 400, c.manager.snapshot())
		return
	}
	s, status, start := c.manager.apply(in)
	accepted := controlReply(w, status, s)
	if start != nil {
		accepted = accepted && http.NewResponseController(w).Flush() == nil
		start(accepted)
	}
}
