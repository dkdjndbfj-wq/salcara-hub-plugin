package sourceissuer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"salcara/hub/internal/hub"
)

func fixtureConfig(verifier string) Config {
	return Config{ID: "station-a", AuthorizationURL: "https://station.example/source-grants", Routes: []Route{{APIBase: "https://station.example/tenant-a", VerifierURL: verifier + "/v1/salcara-hub/key", Groups: []Group{{ID: 7, SourceType: "site_api", Audiences: []string{"hub-a"}, Agents: []string{"codex", "claude"}}, {ID: 8, SourceType: "verified_proxy", Audiences: []string{"hub-a"}, Agents: []string{"codex"}}}}}}
}

func fixtureSnapshot() hub.SourceSnapshot {
	return hub.SourceSnapshot{Version: 1, Issuer: "station-a", Audience: "hub-a", DeviceID: "device-a", BindingID: "binding-a", CredentialHandle: "handle-a", Agent: "codex", ControlSurface: "cli", SourceType: "site_api", Origin: "https://station.example", APIBase: "https://station.example/tenant-a", ConfigRevision: "config-a", PolicyRevision: "policy-a", CommandHash: strings.Repeat("a", 64), RequestID: "10000000-0000-4000-8000-000000000001", ChallengeID: "challenge-a", Model: "model-a"}
}

func fixtureService(t *testing.T, handler http.Handler) (*Service, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(fixtureConfig(upstream.URL), key, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, upstream
}

func requestBody(v hub.SourceSnapshot, key string) []byte {
	raw, _ := json.Marshal(map[string]any{"version": 1, "snapshot": v, "credential": map[string]any{"type": "api_key", "value": key}})
	return raw
}

func call(s *Service, body []byte) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "https://station.example/source-grants", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(w, r)
	return w
}

func TestGrantAuthenticatesAndMatchesHubParser(t *testing.T) {
	var checks atomic.Int32
	s, _ := fixtureService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checks.Add(1)
		if r.Method != "GET" || r.URL.Path != "/v1/salcara-hub/key" || r.Header.Get("Authorization") != "Bearer synthetic-station-key" || r.Header.Get("X-Salcara-Device-Id") != "" {
			t.Error("wrong identity request")
		}
		_, _ = io.WriteString(w, `{"group_id":7}`)
	}))
	expected := fixtureSnapshot()
	w := call(s, requestBody(expected, "synthetic-station-key"))
	if w.Code != 200 || checks.Load() != 1 {
		t.Fatalf("grant failed: %d %s", w.Code, w.Body.String())
	}
	grant, err := hub.ParseSignedSourceGrant(w.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(grant.Payload)
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := base64.RawURLEncoding.Strict().DecodeString(grant.Signature)
	public, _ := base64.RawURLEncoding.Strict().DecodeString(s.PublicIssuer().PublicKey)
	if !ed25519.Verify(public, payload, sig) {
		t.Fatal("signature invalid")
	}
	actual, err := hub.ParseSourceSnapshot(payload)
	if err != nil {
		t.Fatal(err)
	}
	if actual.IssuedAt < time.Now().Unix()-2 || actual.ExpiresAt-actual.IssuedAt != 120 {
		t.Fatal("invalid grant lifetime")
	}
	actual.IssuedAt, actual.ExpiresAt = 0, 0
	if actual != expected {
		t.Fatal("changed signed request")
	}
	if bytes.Contains(payload, []byte("synthetic-station-key")) || strings.Contains(w.Body.String(), "synthetic-station-key") {
		t.Fatal("credential disclosed")
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("grant can be cached")
	}
	policy, _ := json.Marshal(hub.RemoteAccessPolicy{Mode: "restricted", Revision: "revision", Audience: "hub-a", Issuers: []hub.SourceIssuer{s.PublicIssuer()}})
	if _, err := hub.ParseRemoteAccessPolicy(policy); err != nil {
		t.Fatal(err)
	}
}

func TestSourceClaimsCannotExpandStationAuthority(t *testing.T) {
	var checks atomic.Int32
	s, _ := fixtureService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checks.Add(1)
		_, _ = io.WriteString(w, `{"group_id":7}`)
	}))
	cases := []struct {
		name    string
		change  func(*hub.SourceSnapshot)
		network bool
	}{
		{"other-origin", func(v *hub.SourceSnapshot) { v.Origin = "https://other.example" }, false},
		{"other-tenant", func(v *hub.SourceSnapshot) { v.APIBase = "https://station.example/tenant-b" }, false},
		{"label-proxy", func(v *hub.SourceSnapshot) { v.SourceType = "verified_proxy" }, true},
		{"official-label", func(v *hub.SourceSnapshot) { v.SourceType = "official_account_proxy" }, false},
		{"other-audience", func(v *hub.SourceSnapshot) { v.Audience = "hub-b" }, true},
		{"other-agent", func(v *hub.SourceSnapshot) { v.Agent = "hermes" }, true},
		{"desktop", func(v *hub.SourceSnapshot) { v.ControlSurface = "desktop" }, false},
		{"expiry-injection", func(v *hub.SourceSnapshot) { v.ExpiresAt = time.Now().Unix() + 10000 }, false},
		{"hash-invalid", func(v *hub.SourceSnapshot) { v.CommandHash = strings.Repeat("G", 64) }, false},
		{"missing-nonce", func(v *hub.SourceSnapshot) { v.ChallengeID = "" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := fixtureSnapshot()
			tc.change(&v)
			before := checks.Load()
			w := call(s, requestBody(v, "synthetic-station-key"))
			if w.Code != 403 {
				t.Fatalf("accepted unauthorized claim: %d", w.Code)
			}
			if (checks.Load() > before) != tc.network {
				t.Fatal("unexpected identity request")
			}
		})
	}
}

func TestVerifiedProxyRequiresAuthenticatedGroup(t *testing.T) {
	s, _ := fixtureService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"group_id":8}`) }))
	v := fixtureSnapshot()
	if w := call(s, requestBody(v, "synthetic-proxy-key")); w.Code != 403 {
		t.Fatal("proxy group claimed API group")
	}
	v.SourceType = "verified_proxy"
	if w := call(s, requestBody(v, "synthetic-proxy-key")); w.Code != 200 {
		t.Fatal("authorized proxy denied", w.Code)
	}
}

func TestRevocationIsNotCachedAndFailuresNeverLeak(t *testing.T) {
	var revoked atomic.Bool
	s, _ := fixtureService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if revoked.Load() {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, "synthetic-secret-error")
			return
		}
		_, _ = io.WriteString(w, `{"group_id":7}`)
	}))
	if w := call(s, requestBody(fixtureSnapshot(), "synthetic-key")); w.Code != 200 {
		t.Fatal(w.Code)
	}
	revoked.Store(true)
	w := call(s, requestBody(fixtureSnapshot(), "synthetic-key"))
	if w.Code != 403 || strings.Contains(w.Body.String(), "synthetic") {
		t.Fatal("revocation or error privacy failure")
	}
}

func TestVerifierRedirectAndAmbiguousResponsesDenied(t *testing.T) {
	var leaked atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Add(1)
		_, _ = io.WriteString(w, `{"group_id":7}`)
	}))
	defer foreign.Close()
	for _, body := range []string{`{"group_id":7,"group_id":8}`, `{"group_id":7,"Group_id":8}`, `{"group_id":7} {}`, `{"group_id":null}`, `{"group_id":7,"key":"synthetic"}`, strings.Repeat("x", 4097), "redirect"} {
		t.Run(fmt.Sprint(len(body), body[:min(len(body), 12)]), func(t *testing.T) {
			s, _ := fixtureService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if body == "redirect" {
					http.Redirect(w, r, foreign.URL, 307)
					return
				}
				_, _ = io.WriteString(w, body)
			}))
			w := call(s, requestBody(fixtureSnapshot(), "synthetic-key"))
			if w.Code != 503 || strings.Contains(w.Body.String(), "synthetic") {
				t.Fatal("unsafe verifier response", w.Code)
			}
		})
	}
	if leaked.Load() != 0 {
		t.Fatal("credential sent through redirect")
	}
}

func TestMalformedBodyNeverReachesVerifier(t *testing.T) {
	var calls atomic.Int32
	s, _ := fixtureService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	valid := requestBody(fixtureSnapshot(), "synthetic-key")
	for _, raw := range [][]byte{
		[]byte(strings.Replace(string(valid), `"version":1`, `"version":1,"version":1`, 1)),
		[]byte(strings.Replace(string(valid), `"sourceType":"site_api"`, `"sourceType":"site_api","SourceType":"verified_proxy"`, 1)),
		[]byte(strings.Replace(string(valid), `"value":"synthetic-key"`, `"value":"synthetic-key","value":"other"`, 1)),
		append(append([]byte{}, valid...), []byte(" {}")...),
		requestBody(fixtureSnapshot(), "synthetic-key\nX-Header: value"),
		[]byte(strings.Repeat("x", (24<<10)+1)),
	} {
		if w := call(s, raw); w.Code == 200 {
			t.Fatal("ambiguous request accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("malformed body disclosed credential")
	}
}

func TestConcurrentIssuanceAndCancellation(t *testing.T) {
	s, _ := fixtureService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"group_id":7}`) }))
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := call(s, requestBody(fixtureSnapshot(), "synthetic-key")); w.Code != 200 {
				t.Errorf("concurrent grant %d", w.Code)
			}
		}()
	}
	wg.Wait()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("POST", "https://station.example/source-grants", bytes.NewReader(requestBody(fixtureSnapshot(), "synthetic-key"))).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code == 200 {
		t.Fatal("cancelled request signed")
	}
}

func TestCapacityAndCredentialRateAreBounded(t *testing.T) {
	s, _ := fixtureService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("capacity should stop authentication") }))
	for i := 0; i < cap(s.capacity); i++ {
		s.capacity <- struct{}{}
	}
	if w := call(s, requestBody(fixtureSnapshot(), "synthetic-key")); w.Code != 429 {
		t.Fatal("capacity not enforced")
	}
	for len(s.capacity) > 0 {
		<-s.capacity
	}
	now := time.Now()
	for i := 0; i < 120; i++ {
		if !s.allow("synthetic-key", now) {
			t.Fatal("early rate rejection")
		}
	}
	if s.allow("synthetic-key", now) || !s.allow("synthetic-key", now.Add(time.Minute)) {
		t.Fatal("rate window invalid")
	}
	for i := 0; i < 4096; i++ {
		s.allow(fmt.Sprint("key-", i), now)
	}
	if len(s.attempts) > 4096 {
		t.Fatal("unbounded credential cache")
	}
}
