package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDiscoveryRootMountCanonicalPreservesExplicitAPI(t *testing.T) {
	for _, tc := range []struct{ public, want string }{
		{"https://hub.example", "https://hub.example/v1"},
		{"https://hub.example/v1", "https://hub.example/v1"},
		{"https://hub.example/tenant/v1", "https://hub.example/tenant/v1"},
	} {
		t.Run(tc.public, func(t *testing.T) {
			h := newDeviceHub(t, Config{DataDir: t.TempDir(), Prefix: "/", PublicURL: tc.public})
			if got := h.IngressState().CanonicalURL; got != tc.want {
				t.Fatalf("root mount canonical = %q; want %q", got, tc.want)
			}
			if w := discoveryRequest(h, "/v1/ping", nil); w.Code != http.StatusOK {
				t.Fatalf("root discovery: %d %s", w.Code, w.Body)
			}
		})
	}
}

func TestDiscoveryHTTPRestartAndResetKeepServiceSeparateFromBinding(t *testing.T) {
	dir := t.TempDir()
	transport := &rejectingRoundTripper{}
	config := Config{DataDir: dir, PublicURL: "https://hub.example/salcara-hub", DisableLegacyAPIKey: true,
		HTTPClient: &http.Client{Transport: transport}, Sub2APIURL: "https://unused.invalid"}
	h := newDeviceHub(t, config)
	id, secret := "restart-reset-pc", strings.Repeat("a", 64)
	enrollDevice(t, h, id, secret)
	original := h.IngressState()
	var current atomic.Pointer[Hub]
	current.Store(h)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { current.Load().ServeHTTP(w, r) }))
	defer server.Close()
	request := func(path string, credentials bool) (int, map[string]any) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, server.URL+"/salcara-hub/v1"+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if credentials {
			req.Header.Set("X-Salcara-Device-Id", id)
			req.Header.Set("X-Salcara-Device-Secret", secret)
		}
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, body
	}
	if status, body := request("/discovery/policy", true); status != 200 || body["scope"] != "device" {
		t.Fatalf("initial policy: %d %v", status, body)
	}
	h.Close()
	if status, body := request("/ping", true); status != 503 || body["code"] != "remote_unavailable" {
		t.Fatalf("stopped service: %d %v", status, body)
	}
	h = newDeviceHub(t, config)
	current.Store(h)
	if got := h.IngressState(); got.HubInstanceID != original.HubInstanceID || got.IdentityPublicKey != original.IdentityPublicKey {
		t.Fatal("ordinary restart changed ingress identity")
	}
	if status, body := request("/discovery/policy", true); status != 200 || body["scope"] != "device" {
		t.Fatalf("restart lost valid binding: %d %v", status, body)
	}
	h.Close()
	config.DataDir = t.TempDir()
	h = newDeviceHub(t, config)
	current.Store(h)
	if got := h.IngressState(); got.HubInstanceID == original.HubInstanceID || got.IdentityPublicKey == original.IdentityPublicKey {
		t.Fatal("fresh data directory reused old ingress identity")
	}
	for _, credentials := range []bool{false, true} {
		if status, body := request("/ping", credentials); status != 200 || body["service"] != "salcara-hub" {
			t.Fatalf("reset discovery with credentials=%v: %d %v", credentials, status, body)
		}
	}
	if status, body := request("/discovery/policy", true); status != 403 || body["code"] != "device_credentials_invalid" || body["remoteAccess"] != nil {
		t.Fatalf("reset retained invalid old binding: %d %v", status, body)
	}
	if transport.calls.Load() != 0 {
		t.Fatal("remote discovery or binding validation called model authentication")
	}
}
