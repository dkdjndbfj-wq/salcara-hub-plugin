package standalone

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeviceCleanupSettingPersistsAcrossRestartAndValidates(t *testing.T) {
	s, cfg, admin := serverFixture(t)
	state := adminRequest(t, s, http.MethodGet, Prefix+"/_admin/v1/state", admin, nil)
	if state.Code != 200 || !strings.Contains(state.Body.String(), `"device_cleanup":{"unpaired_days":`) {
		t.Fatalf("state does not report the cleanup policy: %s", state.Body.String())
	}
	w := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/device-cleanup", admin, map[string]any{"unpaired_days": 14, "paired_days": 90, "confirm": true})
	if w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	if c := s.hub.CurrentDeviceCleanup(); c.UnpairedDays != 14 || c.PairedDays != 90 {
		t.Fatalf("policy not applied: %+v", c)
	}
	for _, body := range []map[string]any{
		{"unpaired_days": 14, "paired_days": 90},                            // not confirmed
		{"unpaired_days": -1, "paired_days": 0, "confirm": true},            // negative
		{"unpaired_days": 4000, "paired_days": 0, "confirm": true},          // too long
		{"unpaired_days": 7, "confirm": true},                               // one value only
		{"unpaired_days": 7, "paired_days": 0, "extra": 1, "confirm": true}, // unknown field
	} {
		if w := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/device-cleanup", admin, body); w.Code != 400 {
			t.Fatalf("invalid body accepted: %v -> %d", body, w.Code)
		}
	}
	if w := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/device-cleanup", "", map[string]any{"run_now": true, "confirm": true}); w.Code != 401 {
		t.Fatal("cleanup endpoint must require the admin token")
	}
	if w := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/device-cleanup", admin, map[string]any{"run_now": true, "confirm": true}); w.Code != 200 || !strings.Contains(w.Body.String(), `"removed":0`) {
		t.Fatalf("run now: %d %s", w.Code, w.Body.String())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.CleanupUnpairedDays, cfg.CleanupPairedDays = 3, 0 // the saved admin choice wins over env defaults
	restarted, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if c := restarted.hub.CurrentDeviceCleanup(); c.UnpairedDays != 14 || c.PairedDays != 90 {
		t.Fatalf("restart lost the cleanup policy: %+v", c)
	}
	// A failed write leaves the running policy unchanged.
	path := filepath.Join(cfg.DataDir, deviceCleanupFile)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if w := adminRequest(t, restarted, http.MethodPost, Prefix+"/_admin/v1/device-cleanup", admin, map[string]any{"unpaired_days": 1, "paired_days": 1, "confirm": true}); w.Code != 500 || restarted.hub.CurrentDeviceCleanup().UnpairedDays != 14 {
		t.Fatal("failed save changed the running policy")
	}
}

func TestCleanupDaysFromEnvironment(t *testing.T) {
	base := map[string]string{"SALCARA_HUB_DATA_DIR": t.TempDir()}
	base["SALCARA_HUB_ADMIN_ACCOUNT_FILE"] = filepath.Join(base["SALCARA_HUB_DATA_DIR"], defaultAccountFile)
	get := func(extra map[string]string) func(string) string {
		return func(k string) string {
			if v, ok := extra[k]; ok {
				return v
			}
			return base[k]
		}
	}
	c, err := ConfigFromEnv(get(nil))
	if err != nil || c.CleanupUnpairedDays != 7 || c.CleanupPairedDays != 0 {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	c, err = ConfigFromEnv(get(map[string]string{"SALCARA_HUB_CLEANUP_UNPAIRED_DAYS": "0", "SALCARA_HUB_CLEANUP_PAIRED_DAYS": "180"}))
	if err != nil || c.CleanupUnpairedDays != 0 || c.CleanupPairedDays != 180 {
		t.Fatalf("explicit: %+v %v", c, err)
	}
	for _, bad := range []string{"-1", "7d", "3651", "1.5"} {
		if _, err := ConfigFromEnv(get(map[string]string{"SALCARA_HUB_CLEANUP_UNPAIRED_DAYS": bad})); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
