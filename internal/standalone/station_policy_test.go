package standalone

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"salcara/hub/internal/hub"
)

func stationAdminChange(revision string, station *hub.StationPolicy) map[string]any {
	return map[string]any{"mode": "restricted", "confirm": true, "expectedRevision": revision, "station": station}
}

func TestStationAdminReadinessRequiresAnEnabledStationSource(t *testing.T) {
	s, _, admin := serverFixture(t)
	for _, tc := range []struct {
		name, mode            string
		api, official, issuer bool
		ready                 bool
	}{
		{"restricted-empty-station", "restricted", false, false, true, false},
		{"restricted-api", "restricted", true, false, true, true},
		{"restricted-api-without-issuer", "restricted", true, false, false, false},
		{"restricted-official", "restricted", false, true, false, true},
		{"open-empty-station", "open", false, false, false, false},
		{"open-api", "open", true, false, false, true},
		{"open-official", "open", false, true, false, true},
		{"disabled", "disabled", true, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := hub.RemoteAccessPolicy{Mode: tc.mode, Station: &hub.StationPolicy{Version: 1, APIBases: []string{}, AllowOfficialAccounts: tc.official}}
			if tc.api {
				p.Station.APIBases = []string{"https://api.example.com/team"}
			}
			if tc.issuer {
				p.Audience = "test-station"
				p.Issuers = []hub.SourceIssuer{{ID: "synthetic-issuer", PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", SourceTypes: []string{"site_api"}}}
			}
			if err := s.hub.ApplyRemoteAccessPolicy(p, nil); err != nil {
				t.Fatal(err)
			}
			w := adminRequest(t, s, http.MethodGet, Prefix+"/_admin/v1/state", admin, nil)
			var state struct {
				Ready bool `json:"remote_access_ready"`
			}
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &state) != nil || state.Ready != tc.ready {
				t.Fatalf("station readiness = %d %s; want %v", w.Code, w.Body.String(), tc.ready)
			}
		})
	}
}

func TestStationAdminNormalizesPersistsAndPreventsStaleWidening(t *testing.T) {
	s, cfg, admin := serverFixture(t)
	initial := s.hub.CurrentRemoteAccessPolicy()
	station := &hub.StationPolicy{Version: 1, APIBases: []string{"https://API.EXAMPLE.com:443/team/v1/"}, AllowOfficialAccounts: true}
	w := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/remote-access", admin, stationAdminChange(initial.Revision, station))
	if w.Code != 200 {
		t.Fatalf("station save: %d %s", w.Code, w.Body.String())
	}
	p := s.hub.CurrentRemoteAccessPolicy()
	if p.Revision == initial.Revision || p.Audience != "station" || !p.Station.AllowOfficialAccounts || !reflect.DeepEqual(p.Station.APIBases, []string{"https://api.example.com/team"}) {
		t.Fatalf("station not normalized: %#v", p)
	}
	state := adminRequest(t, s, http.MethodGet, Prefix+"/_admin/v1/state", admin, nil)
	if !strings.Contains(state.Body.String(), `"remote_access_ready":true`) || !strings.Contains(state.Body.String(), `"remote_access_station"`) {
		t.Fatalf("official-only readiness missing: %s", state.Body.String())
	}
	if stale := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/remote-access", admin, stationAdminChange(initial.Revision, station)); stale.Code != http.StatusConflict {
		t.Fatalf("stale station overwrite: %d", stale.Code)
	}
	if missing := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/remote-access", admin, stationAdminChange("", station)); missing.Code != http.StatusConflict {
		t.Fatalf("station save omitted current revision: %d", missing.Code)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if restored := restarted.hub.CurrentRemoteAccessPolicy(); !reflect.DeepEqual(p, restored) {
		t.Fatalf("restart lost exact policy: before=%#v after=%#v", p, restored)
	}
	// Turning the official switch off also resets its derived audience exactly
	// as a restart would, while preserving deployment-owned explicit audiences.
	station.AllowOfficialAccounts = false
	w = adminRequest(t, restarted, http.MethodPost, Prefix+"/_admin/v1/remote-access", admin, stationAdminChange(p.Revision, station))
	if w.Code != 200 || restarted.hub.CurrentRemoteAccessPolicy().Audience != "" {
		t.Fatalf("official default audience outlived switch: %d", w.Code)
	}
}

func TestStationAdminStrictSchemaAndFailureKeepCurrentPolicy(t *testing.T) {
	s, cfg, admin := serverFixture(t)
	before := s.hub.CurrentRemoteAccessPolicy()
	for _, station := range []*hub.StationPolicy{
		{Version: 3, APIBases: []string{}},
		{Version: 1, APIBases: []string{"https://example.com/a/../b"}},
		{Version: 1, APIBases: []string{"https://example.com/a%2fb"}},
		{Version: 1, APIBases: []string{"https://example.com", "https://EXAMPLE.com:443/v1"}},
	} {
		w := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/remote-access", admin, stationAdminChange(before.Revision, station))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid station: %d %s", w.Code, w.Body.String())
		}
	}
	for _, raw := range []string{
		`{"mode":"open","confirm":true,"station":null}`,
		`{"mode":"open","Mode":"restricted","confirm":true}`,
		`{"mode":"open","confirm":true,"station":{"version":1,"apiBases":[],"allowOfficialAccounts":false,"AllowOfficialAccounts":true}}`,
		`{"mode":"open","confirm":true,"station":{"version":1,"apiBases":[],"apiBases":["https://evil.example"]}}`,
	} {
		w := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/remote-access", admin, json.RawMessage(raw))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("ambiguous station schema: %d %s", w.Code, w.Body.String())
		}
	}
	station := &hub.StationPolicy{Version: 1, APIBases: []string{"https://api.example.com"}, AllowOfficialAccounts: true}
	if w := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/remote-access", "", stationAdminChange(before.Revision, station)); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated station overwrite: %d", w.Code)
	}
	if err := os.Mkdir(filepath.Join(cfg.DataDir, remoteAccessModeFile), 0o700); err != nil {
		t.Fatal(err)
	}
	w := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/remote-access", admin, stationAdminChange(before.Revision, station))
	if w.Code != http.StatusInternalServerError || !reflect.DeepEqual(before, s.hub.CurrentRemoteAccessPolicy()) {
		t.Fatalf("failed station persistence changed policy: %d", w.Code)
	}
}

func TestStationDeploymentContentInvalidatesSavedOverrideWithoutManualRevisionChange(t *testing.T) {
	cfg := testConfig(t)
	deploy := hub.RemoteAccessPolicy{Mode: "open", Revision: "unchanged", Station: &hub.StationPolicy{Version: 1, APIBases: []string{"https://deployed.example/team-a"}}}
	writeDeployment := func() {
		t.Helper()
		raw, _ := json.Marshal(deploy)
		if err := os.WriteFile(filepath.Join(cfg.DataDir, remoteAccessPolicyName), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeDeployment()
	if err := InitAdminAccount(cfg); err != nil {
		t.Fatal(err)
	}
	admin := initialPassword(t, cfg)
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	before := s.hub.CurrentRemoteAccessPolicy()
	station := &hub.StationPolicy{Version: 1, APIBases: []string{"https://older-admin.example"}, AllowOfficialAccounts: true}
	w := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/remote-access", admin, stationAdminChange(before.Revision, station))
	if w.Code != 200 {
		t.Fatalf("save override: %d %s", w.Code, w.Body.String())
	}
	savedRevision := s.hub.CurrentRemoteAccessPolicy().Revision
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	deploy.Station.APIBases = []string{"https://deployed.example/team-c"}
	writeDeployment()
	restarted, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	p := restarted.hub.CurrentRemoteAccessPolicy()
	if p.Revision == savedRevision || p.Revision == before.Revision || p.Station.AllowOfficialAccounts || !reflect.DeepEqual(p.Station.APIBases, deploy.Station.APIBases) {
		t.Fatalf("new deployment revived saved scope: %#v", p)
	}
}

func TestStationSavedSettingRejectsAmbiguityAndPublicURLAffectsDeploymentEpoch(t *testing.T) {
	dir := t.TempDir()
	for _, raw := range []string{
		`{"mode":"open","revision":"r","baseRevision":"b","station":{"version":1,"apiBases":null}}`,
		`{"mode":"open","revision":"r","baseRevision":"b","station":{"version":1,"apiBases":[],"allowOfficialAccounts":false,"allowOfficialAccounts":true}}`,
		`{"mode":"open","Mode":"disabled","revision":"r","baseRevision":"b"}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, remoteAccessModeFile), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadRemoteAccessModeSetting(dir, "open", "b"); err == nil {
			t.Fatalf("ambiguous saved station accepted: %s", raw)
		}
	}
	a, err := hub.ResolveRemoteAccessPolicy(hub.RemoteAccessPolicy{}, "https://a.example/salcara-hub", Prefix)
	if err != nil {
		t.Fatal(err)
	}
	b, err := hub.ResolveRemoteAccessPolicy(hub.RemoteAccessPolicy{}, "https://b.example/salcara-hub", Prefix)
	if err != nil {
		t.Fatal(err)
	}
	if deploymentRemoteAccessRevision(a) == deploymentRemoteAccessRevision(b) {
		t.Fatal("default station address did not affect deployment epoch")
	}
}
