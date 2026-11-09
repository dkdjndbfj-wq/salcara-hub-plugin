package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIngressAPIURLPreservesCompleteMountPrefix(t *testing.T) {
	for raw, want := range map[string]string{
		"https://edge.example.com":                                  "https://edge.example.com/salcara-hub/v1",
		"https://edge.example.com/":                                 "https://edge.example.com/salcara-hub/v1",
		"https://edge.example.com/salcara-hub":                      "https://edge.example.com/salcara-hub/v1",
		"https://edge.example.com/salcara-hub/":                     "https://edge.example.com/salcara-hub/v1",
		"https://edge.example.com/salcara-hub/v1":                   "https://edge.example.com/salcara-hub/v1",
		"https://edge.example.com/salcara-hub/v1/":                  "https://edge.example.com/salcara-hub/v1",
		"https://edge.example.com/v1":                               "https://edge.example.com/v1",
		"https://edge.example.com/v1/":                              "https://edge.example.com/v1",
		"https://edge.example.com/tenant/v1":                        "https://edge.example.com/tenant/v1",
		"https://edge.example.com/tenant/plugin/v1/":                "https://edge.example.com/tenant/plugin/v1",
		"https://edge.example.com/tenant/salcara-hub.evil/v1":       "https://edge.example.com/tenant/salcara-hub.evil/v1",
		"https://edge.example.com/tenant/Salcara-hub/v1":            "https://edge.example.com/tenant/Salcara-hub/v1",
		"https://edge.example.com/tenant~_-/plugin.v2/v1":           "https://edge.example.com/tenant~_-/plugin.v2/v1",
		"https://EDGE.example.com:443/tenant/salcara-hub":           "https://edge.example.com/tenant/salcara-hub/v1",
		"https://edge.example.com/tenant/salcara-hub/":              "https://edge.example.com/tenant/salcara-hub/v1",
		"https://edge.example.com:8443/tenant/salcara-hub/v1/":      "https://edge.example.com:8443/tenant/salcara-hub/v1",
		"https://edge.example.com/Tenant-A/team.v2/salcara-hub/v1":  "https://edge.example.com/Tenant-A/team.v2/salcara-hub/v1",
		"https://[2606:4700:4700::1111]:443/tenant/salcara-hub/v1/": "https://[2606:4700:4700::1111]/tenant/salcara-hub/v1",
	} {
		t.Run(raw, func(t *testing.T) {
			got, err := ingressAPIURL(raw)
			if err != nil || got != want {
				t.Fatalf("normalize %q = %q, %v; want %q", raw, got, err, want)
			}
			if repeated, err := ingressAPIURL(got); err != nil || repeated != got {
				t.Fatalf("normalized URL is not stable: %q, %v", repeated, err)
			}
		})
	}
}

func TestIngressAPIURLRejectsAmbiguousMountPrefixes(t *testing.T) {
	for _, raw := range []string{
		"https://edge.example.com/tenant",
		"https://edge.example.com/tenantsalcara-hub",
		"https://edge.example.com/tenant/salcara-hub-evil",
		"https://edge.example.com/tenant/salcara-hub/v10",
		"https://edge.example.com/tenant/salcara-hub/v1/extra",
		"https://edge.example.com/tenant/salcara-hub/V1",
		"https://edge.example.com/tenant/../salcara-hub/v1",
		"https://edge.example.com/tenant/./salcara-hub/v1",
		"https://edge.example.com//tenant/salcara-hub/v1",
		"https://edge.example.com/tenant//salcara-hub/v1",
		"https://edge.example.com/tenant/salcara-hub/v1//",
		"https://edge.example.com/tenant\\other/salcara-hub/v1",
		"https://edge.example.com/%74enant/salcara-hub/v1",
		"https://edge.example.com/tenant%2fother/salcara-hub/v1",
		"https://edge.example.com/tenant%252fother/salcara-hub/v1",
		"https://edge.example.com/tenant%25/salcara-hub/v1",
		"https://edge.example.com/tenant/%2e%2e/salcara-hub/v1",
		"https://edge.example.com/tenant/%5c/salcara-hub/v1",
		"https://edge.example.com/tenant/\u8def\u5f84/salcara-hub/v1",
		"https://edge.example.com/tenant;other/v1",
		"https://edge.example.com/tenant@other/v1",
		"https://edge.example.com/tenant:other/v1",
		"https://edge.example.com/tenant+other/v1",
		"https://edge.example.com/tenant=other/v1",
		"https://edge.example.com/tenant&other/v1",
		"https://edge.example.com/tenant/salcara-hub/v1?",
		"https://edge.example.com/tenant/salcara-hub/v1?q=1",
		"https://edge.example.com/tenant/salcara-hub/v1#",
		"https://edge.example.com/tenant/salcara-hub/v1#fragment",
		"https://user:pass@edge.example.com/tenant/salcara-hub/v1",
		"http://edge.example.com/tenant/salcara-hub/v1",
		"https://localhost/tenant/salcara-hub/v1",
		"https://127.0.0.1/tenant/salcara-hub/v1",
		"https://[::ffff:127.0.0.1]/tenant/salcara-hub/v1",
		"https://192.168.1.2/tenant/salcara-hub/v1",
		"https://edge.example.com/" + strings.Repeat("a", 474) + "/salcara-hub",
	} {
		t.Run(raw, func(t *testing.T) {
			if got, err := ingressAPIURL(raw); err == nil || got != "" {
				t.Fatalf("ambiguous/private URL accepted: %q as %q", raw, got)
			}
		})
	}
}

func TestIngressPrefixVerificationAndPersistenceKeepExactEndpoint(t *testing.T) {
	h := ingressFixture(t)
	const endpoint = "https://edge.example.com/Tenant-A/salcara-hub/v1"
	s, err := h.SaveIngress(IngressSave{ExpectedRevision: ingressRevision(h.IngressState().Revision), Enabled: true, DefaultPriority: 100, Endpoints: []IngressEndpointInput{{Name: "Tenant A", URL: endpoint, Enabled: true, Priority: 10}}})
	if err != nil || s.Endpoints[0].URL != endpoint {
		t.Fatalf("save: %#v, %v", s, err)
	}
	h.ingress.client = &http.Client{Transport: ingressRoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.URL.Scheme+"://"+req.URL.Host+req.URL.Path != endpoint+"/ingress/identity" {
			t.Errorf("verification changed the complete endpoint: %s", req.URL)
		}
		if req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" || req.Header.Get("X-Salcara-Device-Secret") != "" {
			t.Error("verification sent credentials")
		}
		w := httptest.NewRecorder()
		h.handleIngressIdentity(w, req, "")
		return w.Result(), nil
	})}
	if _, err := h.VerifyIngress(&s.Revision, ""); err != nil {
		t.Fatal(err)
	}
	done := ingressWait(t, h)
	if done.Verification.Succeeded != 1 || done.Endpoints[0].URL != endpoint || done.Endpoints[0].Status != "verified" {
		t.Fatalf("verification: %#v", done)
	}
	w := httptest.NewRecorder()
	h.handleIngressManifest(w, httptest.NewRequest(http.MethodGet, "/v1/ingress/manifest", nil), "")
	var manifest IngressManifest
	if json.Unmarshal(w.Body.Bytes(), &manifest) != nil || len(manifest.Endpoints) != 2 || manifest.Endpoints[0].URL != endpoint {
		t.Fatalf("manifest lost the path: %s", w.Body.String())
	}
	h.ingress.close()
	restored := newIngressRegistry(h.ingress.dir, "https://hub.example.com/salcara-hub")
	defer restored.close()
	if !restored.configured() || len(restored.saved.Endpoints) != 1 || restored.saved.Endpoints[0].URL != endpoint || restored.saved.Endpoints[0].Status != "verified" {
		t.Fatalf("persisted endpoint changed: %#v", restored.saved.Endpoints)
	}
	h.ingress = restored
	changed, err := h.SaveIngress(IngressSave{ExpectedRevision: &done.Revision, Enabled: true, DefaultPriority: 100, Endpoints: []IngressEndpointInput{{ID: done.Endpoints[0].ID, Name: "Tenant A sibling", URL: "https://edge.example.com/Tenant-A.evil/salcara-hub/v1", Enabled: true, Priority: 10}}})
	if err != nil || changed.Endpoints[0].Status != "unverified" || changed.Endpoints[0].VerifiedAt != 0 {
		t.Fatalf("sibling path reused proof: %#v, %v", changed, err)
	}
}
