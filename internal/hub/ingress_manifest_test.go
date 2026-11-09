package hub

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func ingressSignedManifest(t *testing.T, h *Hub, req *http.Request, pinnedKey string) (IngressManifest, []byte, []byte) {
	t.Helper()
	w := httptest.NewRecorder()
	h.handleIngressManifest(w, req, "untrusted-request-principal")
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("manifest response: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	var m IngressManifest
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	key, err := base64.StdEncoding.Strict().DecodeString(pinnedKey)
	if err != nil || len(key) != ed25519.PublicKeySize || m.IdentityPublicKey != pinnedKey {
		t.Fatal("manifest does not retain the independently pinned public key")
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(m.SignedPayload)
	if err != nil || len(payload) == 0 || base64.StdEncoding.EncodeToString(payload) != m.SignedPayload {
		t.Fatal("payload is not standard padded base64")
	}
	sig, err := base64.StdEncoding.Strict().DecodeString(m.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize || base64.StdEncoding.EncodeToString(sig) != m.Signature {
		t.Fatal("signature is not standard padded base64")
	}
	if !ed25519.Verify(key, append([]byte("salcara-ingress-manifest-v1\n"), payload...), sig) {
		t.Fatal("manifest signature does not authenticate the exact payload bytes")
	}
	var signedFields, flatFields map[string]json.RawMessage
	if json.Unmarshal(payload, &signedFields) != nil || json.Unmarshal(w.Body.Bytes(), &flatFields) != nil {
		t.Fatal("invalid JSON")
	}
	delete(flatFields, "signed_payload")
	delete(flatFields, "signature")
	if !reflect.DeepEqual(signedFields, flatFields) {
		t.Fatal("signed payload and backward-compatible flat fields disagree")
	}
	for _, forbidden := range []string{"signed_payload", "signature", "signing_seed", "authorization", "cookie", "device_secret"} {
		if _, found := signedFields[forbidden]; found {
			t.Fatalf("unexpected signed field %q", forbidden)
		}
	}
	return m, payload, sig
}

func TestIngressManifestSignatureBindsExactEndpointList(t *testing.T) {
	h := ingressFixture(t)
	pin := h.IngressState().IdentityPublicKey
	before := time.Now().UnixMilli()
	m, payload, sig := ingressSignedManifest(t, h, httptest.NewRequest(http.MethodGet, "/v1/ingress/manifest", nil), pin)
	if m.Version != 1 || m.IssuedAt < before || m.IssuedAt > time.Now().UnixMilli() || m.ExpiresAt-m.IssuedAt != 24*60*60*1000 {
		t.Fatalf("invalid version or manifest validity: %#v", m)
	}
	key, _ := base64.StdEncoding.DecodeString(pin)
	if ed25519.Verify(key, payload, sig) || ed25519.Verify(key, append([]byte("salcara-ingress-proof-v1\n"), payload...), sig) {
		t.Fatal("signature is not separated from other protocols")
	}
	for _, kind := range []string{"replace endpoint", "append forwarding proxy", "change canonical", "change identity", "change revision", "extend expiry"} {
		t.Run(kind, func(t *testing.T) {
			var changed IngressManifest
			if err := json.Unmarshal(payload, &changed); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "replace endpoint":
				changed.Endpoints[0].URL = "https://proxy.example.com/tenant/v1"
			case "append forwarding proxy":
				changed.Endpoints = append(changed.Endpoints, IngressManifestEndpoint{ID: "proxy", URL: "https://proxy.example.com/tenant/v1", Priority: 1})
			case "change canonical":
				changed.CanonicalURL = "https://proxy.example.com/tenant/v1"
			case "change identity":
				changed.HubInstanceID = strings.Repeat("f", 32)
			case "change revision":
				changed.Revision++
			case "extend expiry":
				changed.ExpiresAt += 24 * 60 * 60 * 1000
			}
			tampered, err := json.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(payload, tampered) || ed25519.Verify(key, append([]byte("salcara-ingress-manifest-v1\n"), tampered...), sig) {
				t.Fatal("tampered manifest retained a valid signature")
			}
		})
	}
}

func TestIngressSignedManifestPublishesOnlyEnabledVerifiedEndpoints(t *testing.T) {
	h := ingressFixture(t)
	inputs := []IngressEndpointInput{
		{Name: "Verified", URL: "https://edge.example.com/Tenant-A/salcara-hub/v1", Enabled: true, Priority: 10},
		{Name: "Disabled", URL: "https://disabled.example.com/v1", Enabled: true, Priority: 20},
		{Name: "Failed", URL: "https://failed.example.com/v1", Enabled: true, Priority: 30},
		{Name: "Changed", URL: "https://changed.example.com/v1", Enabled: true, Priority: 40},
	}
	s, err := h.SaveIngress(IngressSave{ExpectedRevision: ingressRevision(h.IngressState().Revision), Enabled: true, DefaultPriority: 100, Endpoints: inputs})
	if err != nil {
		t.Fatal(err)
	}
	h.ingress.client = &http.Client{Transport: ingressRoundTrip(func(req *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		if req.URL.Hostname() == "failed.example.com" {
			w.WriteHeader(http.StatusBadGateway)
		} else {
			h.handleIngressIdentity(w, req, "")
		}
		return w.Result(), nil
	})}
	if _, err = h.VerifyIngress(&s.Revision, ""); err != nil {
		t.Fatal(err)
	}
	s = ingressWait(t, h)
	if s.Verification.Succeeded != 3 || s.Verification.Failed != 1 {
		t.Fatalf("unexpected verification outcome: %#v", s)
	}
	for i := range inputs {
		inputs[i].ID = s.Endpoints[i].ID
	}
	inputs[1].Enabled = false
	inputs[3].URL = "https://changed.example.com/sibling/v1"
	s, err = h.SaveIngress(IngressSave{ExpectedRevision: &s.Revision, Enabled: true, DefaultPriority: 100, Endpoints: inputs})
	if err != nil || s.Endpoints[1].Status != "verified" || s.Endpoints[2].Status != "failed" || s.Endpoints[3].Status != "unverified" {
		t.Fatalf("invalid endpoint setup: %#v %v", s, err)
	}
	m, _, _ := ingressSignedManifest(t, h, httptest.NewRequest(http.MethodGet, "/v1/ingress/manifest", nil), s.IdentityPublicKey)
	want := []IngressManifestEndpoint{{ID: inputs[0].ID, URL: inputs[0].URL, Priority: 10}, {ID: "canonical", URL: s.CanonicalURL, Priority: 100}}
	if !reflect.DeepEqual(m.Endpoints, want) {
		t.Fatalf("signed list includes disabled, failed, or unverified endpoints: %#v", m.Endpoints)
	}
	if _, err = h.SaveIngress(IngressSave{ExpectedRevision: &s.Revision, Enabled: false, DefaultPriority: 100, Endpoints: inputs}); err != nil {
		t.Fatal(err)
	}
	m, _, _ = ingressSignedManifest(t, h, httptest.NewRequest(http.MethodGet, "/v1/ingress/manifest", nil), s.IdentityPublicKey)
	if !reflect.DeepEqual(m.Endpoints, want[1:]) {
		t.Fatalf("disabled ingress leaked aliases: %#v", m.Endpoints)
	}
}

func TestIngressSignedManifestRetainsPinAcrossRestartAndRequestIdentity(t *testing.T) {
	h := ingressFixture(t)
	s := ingressSaveFixture(t, h)
	h.ingress.client = &http.Client{Transport: ingressRoundTrip(func(req *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		h.handleIngressIdentity(w, req, "")
		return w.Result(), nil
	})}
	if _, err := h.VerifyIngress(&s.Revision, ""); err != nil {
		t.Fatal(err)
	}
	s = ingressWait(t, h)
	before, _, _ := ingressSignedManifest(t, h, httptest.NewRequest(http.MethodGet, "/v1/ingress/manifest", nil), s.IdentityPublicKey)
	h.ingress.close()
	restarted := &Hub{ingress: newIngressRegistry(h.ingress.dir, "https://hub.example.com/salcara-hub")}
	t.Cleanup(restarted.ingress.close)
	for _, kind := range []string{"anonymous", "foreign host", "credentials", "forwarded headers"} {
		t.Run(kind, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "https://hub.example.com/salcara-hub/v1/ingress/manifest", nil)
			switch kind {
			case "foreign host":
				req.Host = "proxy.example.com"
				req.URL.Host = "proxy.example.com"
			case "credentials":
				req.Header.Set("Authorization", "Bearer synthetic-private-token")
				req.Header.Set("Cookie", "session=synthetic-private-cookie")
				req.Header.Set("X-Salcara-Device-Secret", "synthetic-private-secret")
			case "forwarded headers":
				req.Header.Set("Forwarded", "host=proxy.example.com;proto=http")
				req.Header.Set("X-Forwarded-Host", "proxy.example.com")
				req.Header.Set("X-Forwarded-Prefix", "/other-tenant")
				req.Header.Set("X-Original-URL", "https://proxy.example.com/other-tenant/v1")
			}
			got, payload, _ := ingressSignedManifest(t, restarted, req, s.IdentityPublicKey)
			if got.HubInstanceID != before.HubInstanceID || got.CanonicalURL != before.CanonicalURL || got.Revision != before.Revision || !reflect.DeepEqual(got.Endpoints, before.Endpoints) {
				t.Fatalf("restart or request identity changed the trusted manifest: %#v", got)
			}
			if bytes.Contains(payload, []byte("synthetic-private")) || bytes.Contains(payload, []byte("proxy.example.com")) || bytes.Contains(payload, []byte("other-tenant")) {
				t.Fatal("request credentials or forwarded host influenced signed payload")
			}
		})
	}
}

func TestIngressVerifiedTransportAliasesDoNotExpandStationSourcePolicy(t *testing.T) {
	h, err := New(Config{
		DataDir:   t.TempDir(),
		PublicURL: "https://hub.example.com/team/salcara-hub",
		RemoteAccess: RemoteAccessPolicy{Mode: "open", Station: &StationPolicy{
			Version:              2,
			APIBases:             []string{"https://hub.example.com/team"},
			ExternalAPIBases:     []string{"https://external.example.com/allowed"},
			ReverseProxyAPIBases: []string{"https://proxy.example.com/allowed"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	before := h.CurrentRemoteAccessPolicy()
	aliases := []IngressEndpointInput{
		{Name: "Same origin sibling", URL: "https://hub.example.com/sibling/salcara-hub/v1", Enabled: true, Priority: 10},
		{Name: "Alternate host", URL: "https://edge.example.com/team/salcara-hub/v1", Enabled: true, Priority: 20},
	}
	s, err := h.SaveIngress(IngressSave{ExpectedRevision: ingressRevision(h.IngressState().Revision), Enabled: true, DefaultPriority: 100, Endpoints: aliases})
	if err != nil {
		t.Fatal(err)
	}
	h.ingress.client = &http.Client{Transport: ingressRoundTrip(func(req *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		h.handleIngressIdentity(w, req, "")
		return w.Result(), nil
	})}
	if _, err = h.VerifyIngress(&s.Revision, ""); err != nil {
		t.Fatal(err)
	}
	s = ingressWait(t, h)
	if s.Verification.Succeeded != len(aliases) || s.Verification.Failed != 0 {
		t.Fatalf("aliases were not verified: %#v", s)
	}
	m, _, _ := ingressSignedManifest(t, h, httptest.NewRequest(http.MethodGet, "/v1/ingress/manifest", nil), s.IdentityPublicKey)
	if len(m.Endpoints) != len(aliases)+1 {
		t.Fatalf("verified aliases were not published: %#v", m.Endpoints)
	}
	after := h.CurrentRemoteAccessPolicy()
	if !reflect.DeepEqual(after, before) {
		t.Fatal("transport alias registration or verification changed the model source policy")
	}
	for _, allowed := range []struct{ kind, base string }{
		{"site_api", "https://hub.example.com/team"},
		{"site_api", "https://external.example.com/allowed"},
		{"verified_proxy", "https://proxy.example.com/allowed"},
	} {
		origin, _ := canonicalAPIBase(allowed.base)
		if !after.Station.allowsSnapshot(SourceSnapshot{SourceType: allowed.kind, Origin: origin, APIBase: allowed.base}) {
			t.Fatalf("existing source lost authorization: %s %s", allowed.kind, allowed.base)
		}
	}
	for _, raw := range []string{
		aliases[0].URL, aliases[1].URL,
		"https://hub.example.com/sibling", "https://edge.example.com/team",
		"https://hub.example.com/team/child", "https://hub.example.com/team-evil",
		"https://external.example.com/allowed/child", "https://proxy.example.com/allowed-evil",
	} {
		base, err := NormalizeStationAPIBase(raw)
		if err != nil {
			t.Fatal(err)
		}
		origin, _ := canonicalAPIBase(base)
		for _, kind := range []string{"site_api", "verified_proxy"} {
			if after.Station.allowsSnapshot(SourceSnapshot{SourceType: kind, Origin: origin, APIBase: base}) {
				t.Fatalf("transport alias or same-origin sibling gained model permission: %s %s", kind, base)
			}
		}
	}
}
