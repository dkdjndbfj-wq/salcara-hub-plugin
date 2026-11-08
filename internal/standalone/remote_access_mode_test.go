package standalone

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoteAccessModeAdminPersistsWithoutEditingTrustPolicy(t *testing.T) {
	s, cfg, admin := serverFixture(t)
	initialRevision := s.hub.CurrentRemoteAccessPolicy().Revision
	state := adminRequest(t, s, http.MethodGet, Prefix+"/_admin/v1/state", admin, nil)
	if state.Code != http.StatusOK || !strings.Contains(state.Body.String(), `"remote_access_mode":"open"`) || !strings.Contains(state.Body.String(), `"remote_access_locked":false`) {
		t.Fatalf("remote access diagnostics missing: %d %s", state.Code, state.Body.String())
	}
	if w := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/remote-access", admin, map[string]any{"mode": "restricted", "confirm": true}); w.Code != http.StatusOK {
		t.Fatalf("restricted switch failed: %d %s", w.Code, w.Body.String())
	}
	if got := s.hub.CurrentRemoteAccessPolicy().Mode; got != "restricted" {
		t.Fatalf("active mode: %s", got)
	}
	revision := s.hub.CurrentRemoteAccessPolicy().Revision
	if revision == initialRevision {
		t.Fatal("mode switch did not rotate policy revision")
	}
	raw, err := os.ReadFile(filepath.Join(cfg.DataDir, remoteAccessModeFile))
	var saved remoteAccessModeSetting
	if err != nil || json.Unmarshal(raw, &saved) != nil || saved.Mode != "restricted" || saved.Revision != revision || saved.BaseRevision != initialRevision {
		t.Fatalf("saved mode: %q %v", string(raw), err)
	}
	// The deployment policy file is not created or rewritten by a mode switch.
	if _, err := os.Stat(filepath.Join(cfg.DataDir, remoteAccessPolicyName)); !os.IsNotExist(err) {
		t.Fatalf("mode switch unexpectedly wrote trust policy: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if got := restarted.hub.CurrentRemoteAccessPolicy().Mode; got != "restricted" {
		t.Fatalf("restart mode: %s", got)
	}
	if got := restarted.hub.CurrentRemoteAccessPolicy().Revision; got != revision {
		t.Fatalf("restart lost policy epoch: %s, want %s", got, revision)
	}
}

func TestRemoteAccessModeCycleAndRepeatedModeEpoch(t *testing.T) {
	s, cfg, admin := serverFixture(t)
	initial := s.hub.CurrentRemoteAccessPolicy().Revision
	seen := map[string]bool{initial: true}
	for _, mode := range []string{"restricted", "disabled", "open", "restricted"} {
		w := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/remote-access", admin, map[string]any{"mode": mode, "confirm": true})
		if w.Code != http.StatusOK {
			t.Fatalf("mode %s failed: %d %s", mode, w.Code, w.Body.String())
		}
		revision := s.hub.CurrentRemoteAccessPolicy().Revision
		if seen[revision] {
			t.Fatalf("mode %s revived an old revision: %s", mode, revision)
		}
		seen[revision] = true
		if w := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/remote-access", admin, map[string]any{"mode": mode, "confirm": true}); w.Code != http.StatusOK || s.hub.CurrentRemoteAccessPolicy().Revision != revision {
			t.Fatalf("idempotent mode %s changed the epoch", mode)
		}
	}
	finalRevision := s.hub.CurrentRemoteAccessPolicy().Revision
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if p := restarted.hub.CurrentRemoteAccessPolicy(); p.Mode != "restricted" || p.Revision != finalRevision || p.Revision == initial {
		t.Fatalf("restart revived old policy: %#v", p)
	}
}

func TestRemoteAccessModeLegacyAndDeploymentRevision(t *testing.T) {
	dir := t.TempDir()
	if _, err := saveRemoteAccessMode(dir, "restricted"); err != nil {
		t.Fatal(err)
	}
	legacy, err := loadRemoteAccessModeSetting(dir, "open", "deployment-1")
	if err != nil || legacy.Mode != "restricted" || legacy.Revision != "deployment-1" {
		t.Fatalf("legacy mode migration failed: %#v %v", legacy, err)
	}
	if _, err := saveRemoteAccessModeSetting(dir, remoteAccessModeSetting{Mode: "disabled", Revision: "mode-epoch", BaseRevision: "deployment-1"}); err != nil {
		t.Fatal(err)
	}
	current, err := loadRemoteAccessModeSetting(dir, "open", "deployment-1")
	if err != nil || current.Mode != "disabled" || current.Revision != "mode-epoch" {
		t.Fatalf("current epoch not restored: %#v %v", current, err)
	}
	redeployed, err := loadRemoteAccessModeSetting(dir, "open", "deployment-2")
	if err != nil || redeployed.Mode != "disabled" || redeployed.Revision != "deployment-2" {
		t.Fatalf("saved mode overrode deployed policy revision: %#v %v", redeployed, err)
	}
}

func TestRemoteAccessModePersistenceFailureKeepsEpoch(t *testing.T) {
	s, cfg, admin := serverFixture(t)
	before := s.hub.CurrentRemoteAccessPolicy()
	if err := os.Mkdir(filepath.Join(cfg.DataDir, remoteAccessModeFile), 0o700); err != nil {
		t.Fatal(err)
	}
	w := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/remote-access", admin, map[string]any{"mode": "restricted", "confirm": true})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("failed persistence accepted: %d %s", w.Code, w.Body.String())
	}
	if after := s.hub.CurrentRemoteAccessPolicy(); after.Mode != before.Mode || after.Revision != before.Revision {
		t.Fatalf("failed commit changed policy epoch: %#v", after)
	}
}

func TestRemoteAccessModeEnvironmentLocksAdminSwitch(t *testing.T) {
	cfg := testConfig(t)
	cfg.RemoteAccessMode = "disabled"
	if err := InitAdminAccount(cfg); err != nil {
		t.Fatal(err)
	}
	admin := initialPassword(t, cfg)
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state := adminRequest(t, s, http.MethodGet, Prefix+"/_admin/v1/state", admin, nil)
	if state.Code != http.StatusOK || !strings.Contains(state.Body.String(), `"remote_access_mode":"disabled"`) || !strings.Contains(state.Body.String(), `"remote_access_locked":true`) {
		t.Fatalf("locked diagnostics missing: %d %s", state.Code, state.Body.String())
	}
	if w := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/remote-access", admin, map[string]any{"mode": "open", "confirm": true}); w.Code != http.StatusConflict {
		t.Fatalf("environment lock bypassed: %d %s", w.Code, w.Body.String())
	}
	if got := s.hub.CurrentRemoteAccessPolicy().Mode; got != "disabled" {
		t.Fatalf("locked mode changed: %s", got)
	}
}

func TestRemoteAccessModeAdminStrictSchema(t *testing.T) {
	s, _, admin := serverFixture(t)
	for _, body := range []any{
		map[string]any{"mode": "custom", "confirm": true},
		map[string]any{"mode": "restricted", "confirm": false},
		map[string]any{"mode": "restricted", "confirm": true, "issuer": "changed"},
	} {
		if w := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/remote-access", admin, body); w.Code != http.StatusBadRequest {
			t.Fatalf("invalid remote access request accepted: %d %s", w.Code, w.Body.String())
		}
	}
	if got := s.hub.CurrentRemoteAccessPolicy().Mode; got != "open" {
		t.Fatalf("invalid request changed mode: %s", got)
	}
}
