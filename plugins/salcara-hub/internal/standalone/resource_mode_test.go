package standalone

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

func TestStandaloneResourceModePersistRestartAndFailurePreservesPrior(t *testing.T) {
	s, cfg, admin := serverFixture(t)
	state := adminRequest(t, s, http.MethodGet, Prefix+"/_admin/v1/state", admin, nil)
	if state.Code != 200 || !strings.Contains(state.Body.String(), `"resource_mode":"economy"`) || debug.SetMemoryLimit(-1) != 96<<20 {
		t.Fatal("standalone default economy missing")
	}
	w := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/resource-mode", admin, map[string]any{"mode": "balanced", "confirm": true})
	if w.Code != 200 || s.hub.CurrentResourceMode().ID != "balanced" || debug.SetMemoryLimit(-1) != 192<<20 {
		t.Fatal("confirmed balanced mode was not applied")
	}
	raw, err := os.ReadFile(filepath.Join(cfg.DataDir, resourceModeFile))
	if err != nil || string(raw) != "balanced\n" {
		t.Fatal("mode persistence contained more than the public name")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.ResourceMode = "economy" // saved operator selection wins over startup fallback
	restarted, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if restarted.hub.CurrentResourceMode().ID != "balanced" || debug.SetMemoryLimit(-1) != 192<<20 {
		t.Fatal("restart lost selected mode")
	}
	path := filepath.Join(cfg.DataDir, resourceModeFile)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	} // test-only exact file
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	w = adminRequest(t, restarted, http.MethodPost, Prefix+"/_admin/v1/resource-mode", admin, map[string]any{"mode": "performance", "confirm": true})
	if w.Code != 500 || restarted.hub.CurrentResourceMode().ID != "balanced" || debug.SetMemoryLimit(-1) != 192<<20 {
		t.Fatal("failed persistent write changed current mode")
	}
}

func TestResourceModeAdminStrictPermissionAndSchema(t *testing.T) {
	s, _, admin := serverFixture(t)
	if w := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/resource-mode", "", map[string]any{"mode": "performance", "confirm": true}); w.Code != 401 {
		t.Fatal("public resource change accepted")
	}
	for _, body := range []any{map[string]any{"mode": "performance", "confirm": false}, map[string]any{"mode": "custom", "confirm": true}, map[string]any{"mode": "performance", "confirm": true, "url": "ignored"}, map[string]any{"mode": "performance", "confirm": true, "memory_limit_mib": 1}} {
		if w := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/resource-mode", admin, body); w.Code != 400 {
			t.Fatal("arbitrary resource configuration accepted")
		}
	}
	if s.hub.CurrentResourceMode().ID != "economy" {
		t.Fatal("rejected mode requests changed policy")
	}
}
