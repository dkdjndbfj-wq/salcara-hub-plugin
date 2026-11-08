package standalone

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"salcara/hub/internal/hub"
)

func TestIngressAdminRequiresSessionAndCAS(t *testing.T) {
	cfg := testConfig(t)
	cfg.PublicURL = "https://hub.example.com/salcara-hub"
	if err := InitAdminAccount(cfg); err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	password := initialPassword(t, cfg)
	path := Prefix + "/_admin/v1/ingress/"
	if w := adminRequest(t, s, "GET", path+"state", "", nil); w.Code != 401 {
		t.Fatal("anonymous access", w.Code)
	}
	w := adminRequest(t, s, "GET", path+"state", password, nil)
	var state hub.IngressAdminState
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &state) != nil || !state.Configured {
		t.Fatal(w.Code, w.Body.String())
	}
	body := map[string]any{"expected_revision": state.Revision, "enabled": true, "default_priority": 100, "endpoints": []map[string]any{{"name": "Backup", "url": "https://edge.example.com", "enabled": true, "priority": 10}}}
	w = adminRequest(t, s, "POST", path+"save", password, body)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = adminRequest(t, s, "POST", path+"save", password, body)
	if w.Code != 409 {
		t.Fatal("missing revision conflict", w.Code)
	}
	w = adminRequest(t, s, "POST", path+"save", password, map[string]any{"expected_revision": state.Revision + 1, "signing_seed": "unsafe"})
	if w.Code != 400 {
		t.Fatal("unknown private field accepted", w.Code)
	}
	w = adminRequest(t, s, "GET", path+"save", password, nil)
	if w.Code != 405 {
		t.Fatal(w.Code)
	}
	w = adminRequest(t, s, "GET", path+"state?x=1", password, nil)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
}
func TestIngressUnconfiguredKeepsOriginalSingleURLMode(t *testing.T) {
	s, _, password := serverFixture(t)
	path := Prefix + "/_admin/v1/ingress/"
	w := adminRequest(t, s, "GET", path+"state", password, nil)
	var state hub.IngressAdminState
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &state) != nil || state.Configured {
		t.Fatal(w.Code, w.Body.String())
	}
	w = adminRequest(t, s, "POST", path+"save", password, map[string]any{"expected_revision": state.Revision, "enabled": true, "default_priority": 100})
	if w.Code != 503 {
		t.Fatal(w.Code, w.Body.String())
	}
}
