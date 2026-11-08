package hub

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type ingressRoundTrip func(*http.Request) (*http.Response, error)

func (f ingressRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func ingressFixture(t *testing.T) *Hub {
	t.Helper()
	h := &Hub{ingress: newIngressRegistry(t.TempDir(), "https://hub.example.com/salcara-hub")}
	t.Cleanup(h.ingress.close)
	if !h.IngressState().Configured {
		t.Fatal(h.IngressState())
	}
	return h
}
func ingressRevision(n int64) *int64 { return &n }
func ingressSaveFixture(t *testing.T, h *Hub) IngressAdminState {
	t.Helper()
	s, err := h.SaveIngress(IngressSave{ExpectedRevision: ingressRevision(h.IngressState().Revision), Enabled: true, DefaultPriority: 100, Endpoints: []IngressEndpointInput{{Name: "East", URL: "https://edge.example.com", Enabled: true, Priority: 10}, {Name: "West", URL: "https://west.example.com", Enabled: true, Priority: 20}}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func ingressWait(t *testing.T, h *Hub) IngressAdminState {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s := h.IngressState()
		if s.Verification != nil && !s.Verification.Running {
			return s
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("verification did not finish")
	return IngressAdminState{}
}
func TestIngressIdentityPersistsAndDoesNotTrustRequestHost(t *testing.T) {
	h := ingressFixture(t)
	s := h.IngressState()
	h.ingress.close()
	r := newIngressRegistry(h.ingress.dir, "https://hub.example.com/salcara-hub")
	defer r.close()
	if r.saved.HubInstanceID != s.HubInstanceID || base64.StdEncoding.EncodeToString(r.key.Public().(ed25519.PublicKey)) != s.IdentityPublicKey {
		t.Fatal("identity changed on restart")
	}
	w := httptest.NewRecorder()
	h.handleIngressManifest(w, httptest.NewRequest("GET", "https://attacker.example/ingress/manifest", nil), "")
	var m IngressManifest
	if json.Unmarshal(w.Body.Bytes(), &m) != nil || m.CanonicalURL != "https://hub.example.com/salcara-hub/v1" || strings.Contains(w.Body.String(), "signing_seed") {
		t.Fatal(w.Body.String())
	}
	nonce := strings.Repeat("a", 64)
	w = httptest.NewRecorder()
	h.handleIngressIdentity(w, httptest.NewRequest("GET", "https://other.example/ingress/identity?nonce="+nonce, nil), "")
	var proof IngressProof
	if json.Unmarshal(w.Body.Bytes(), &proof) != nil {
		t.Fatal(w.Body.String())
	}
	sig, _ := base64.StdEncoding.DecodeString(proof.Signature)
	if proof.Nonce != nonce || proof.ExpiresAt-proof.IssuedAt != 30000 || !ed25519.Verify(r.key.Public().(ed25519.PublicKey), ingressProofMessage(proof), sig) {
		t.Fatal("invalid proof")
	}
	w = httptest.NewRecorder()
	h.handleIngressIdentity(w, httptest.NewRequest("GET", "https://hub.example/ingress/identity?nonce="+nonce+"&nonce="+nonce, nil), "")
	if w.Code != 400 {
		t.Fatal("duplicate nonce accepted")
	}
}
func TestIngressConfigurationCASAndValidation(t *testing.T) {
	h := ingressFixture(t)
	s := ingressSaveFixture(t, h)
	if _, err := h.SaveIngress(IngressSave{ExpectedRevision: ingressRevision(1)}); err == nil || err.Error() != "revision_conflict" {
		t.Fatal(err)
	}
	for _, address := range []string{"http://edge.example.com", "https://localhost", "https://127.0.0.1", "https://[::ffff:127.0.0.1]", "https://192.168.1.2", "https://user:pass@edge.example.com", "https://edge.example.com/path", "https://edge.example.com/?token=secret", "https://hub.example.com"} {
		if _, err := h.SaveIngress(IngressSave{ExpectedRevision: &s.Revision, Enabled: true, DefaultPriority: 100, Endpoints: []IngressEndpointInput{{Name: "Bad", URL: address, Enabled: true, Priority: 10}}}); err == nil {
			t.Fatal("accepted", address)
		}
	}
	if h.IngressState().Revision != s.Revision {
		t.Fatal("invalid writes changed state")
	}
	if got, err := ingressAPIURL("https://[2606:4700:4700::1111]:443/"); err != nil || got != "https://[2606:4700:4700::1111]/salcara-hub/v1" {
		t.Fatal(got, err)
	}
}
func TestIngressVerificationPublishesOnlyProofVerifiedEndpoints(t *testing.T) {
	h := ingressFixture(t)
	s := ingressSaveFixture(t, h)
	var calls atomic.Int32
	h.ingress.client = &http.Client{Transport: ingressRoundTrip(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" {
			t.Error("credential sent")
		}
		w := httptest.NewRecorder()
		h.handleIngressIdentity(w, req, "")
		if req.URL.Hostname() == "west.example.com" {
			var p IngressProof
			json.Unmarshal(w.Body.Bytes(), &p)
			p.Nonce = strings.Repeat("b", 64)
			raw, _ := json.Marshal(p)
			w = httptest.NewRecorder()
			w.Write(raw)
		}
		return w.Result(), nil
	})}
	if _, err := h.VerifyIngress(&s.Revision, ""); err != nil {
		t.Fatal(err)
	}
	done := ingressWait(t, h)
	if calls.Load() != 2 || done.Verification.Succeeded != 1 || done.Verification.Failed != 1 {
		t.Fatal(done)
	}
	w := httptest.NewRecorder()
	h.handleIngressManifest(w, httptest.NewRequest("GET", "https://hub.example.com/ingress/manifest", nil), "")
	var m IngressManifest
	json.Unmarshal(w.Body.Bytes(), &m)
	if len(m.Endpoints) != 2 || m.Endpoints[0].URL != "https://edge.example.com/salcara-hub/v1" {
		t.Fatal(w.Body.String())
	}
	changed, err := h.SaveIngress(IngressSave{ExpectedRevision: &done.Revision, Enabled: true, DefaultPriority: 100, Endpoints: []IngressEndpointInput{{ID: done.Endpoints[0].ID, Name: "Changed", URL: "https://new.example.com", Enabled: true, Priority: 10}}})
	if err != nil || changed.Endpoints[0].Status != "unverified" || changed.Endpoints[0].VerifiedAt != 0 {
		t.Fatal(changed, err)
	}
}
func TestIngressFailedPersistenceKeepsOldConfiguration(t *testing.T) {
	h := ingressFixture(t)
	s := ingressSaveFixture(t, h)
	if err := os.Remove(filepath.Join(h.ingress.dir, ingressStateFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(h.ingress.dir, ingressStateFile), 0700); err != nil {
		t.Fatal(err)
	}
	_, err := h.SaveIngress(IngressSave{ExpectedRevision: &s.Revision, Enabled: false, DefaultPriority: 100})
	if err == nil || h.IngressState().Revision != s.Revision || !h.IngressState().Enabled {
		t.Fatal("failed save changed live state", err)
	}
}
func TestIngressCloseCancelsVerificationAndCorruptRegistryDisablesOnlyIngress(t *testing.T) {
	h := ingressFixture(t)
	s := ingressSaveFixture(t, h)
	h.ingress.client = &http.Client{Transport: ingressRoundTrip(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	if _, err := h.VerifyIngress(&s.Revision, ""); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { h.ingress.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("close stalled")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ingressStateFile), []byte(`{"version":1}`), 0600)
	r := newIngressRegistry(dir, "https://hub.example.com")
	defer r.close()
	if r.configured() || r.lastError == "" {
		t.Fatal("invalid registry accepted")
	}
	if _, err := ingressClient().Get("https://127.0.0.1/salcara-hub/v1/ingress/identity?nonce=" + strings.Repeat("a", 64)); err == nil {
		t.Fatal("private dial permitted")
	}
}
func TestIngressVerificationRejectsWrongKeyExpiredAndRedirect(t *testing.T) {
	h := ingressFixture(t)
	for _, kind := range []string{"key", "expired", "redirect", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			h.ingress.client = &http.Client{Transport: ingressRoundTrip(func(req *http.Request) (*http.Response, error) {
				p := IngressProof{Version: 1, HubInstanceID: h.IngressState().HubInstanceID, Nonce: req.URL.Query().Get("nonce"), IssuedAt: time.Now().UnixMilli(), ExpiresAt: time.Now().Add(30 * time.Second).UnixMilli()}
				p.ExpiresAt = p.IssuedAt + 30000
				if kind == "expired" {
					p.IssuedAt -= 60000
					p.ExpiresAt -= 60000
				}
				p.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(h.ingress.key, ingressProofMessage(p)))
				if kind == "key" {
					p.Signature = base64.StdEncoding.EncodeToString(make([]byte, 64))
				}
				raw, _ := json.Marshal(p)
				status := 200
				if kind == "redirect" {
					status = 302
				}
				if kind == "oversized" {
					raw = []byte(strings.Repeat(" ", 17000))
				}
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
			})}
			if h.ingress.checkEndpoint(context.Background(), "https://edge.example.com/salcara-hub/v1") == "" {
				t.Fatal("untrusted proof accepted")
			}
		})
	}
}
