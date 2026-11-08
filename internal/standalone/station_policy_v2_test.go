package standalone

import (
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"salcara/hub/internal/hub"
)

func TestStationV2AdminPersistsIndependentScopesAndRejectsStaleUpdate(t *testing.T) {
	s, cfg, admin := serverFixture(t)
	initial := s.hub.CurrentRemoteAccessPolicy()
	station := &hub.StationPolicy{Version: 2, APIBases: []string{}, ExternalAPIBases: []string{"https://EXTERNAL.example:443/team/v1/"}, ReverseProxyAPIBases: []string{"https://proxy.example/team"}}
	change := stationAdminChange(initial.Revision, station)
	change["mode"] = "open"
	if w := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/remote-access", admin, change); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	p := s.hub.CurrentRemoteAccessPolicy()
	if p.Station.ExternalAPIBases[0] != "https://external.example/team" || p.Station.ReverseProxyAPIBases[0] != "https://proxy.example/team" || p.Station.AllowOfficialAccounts {
		t.Fatal(p.Station)
	}
	state := adminRequest(t, s, http.MethodGet, Prefix+"/_admin/v1/state", admin, nil)
	if !strings.Contains(state.Body.String(), `"remote_access_ready":true`) || !strings.Contains(state.Body.String(), `"id":"reverse_proxy"`) {
		t.Fatal(state.Body.String())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if !reflect.DeepEqual(p, restarted.hub.CurrentRemoteAccessPolicy()) {
		t.Fatal("restart changed the saved v2 scopes")
	}
	station = p.Station
	station.ExternalAPIBases = nil
	change = stationAdminChange(p.Revision, station)
	change["mode"] = "open"
	if w := adminRequest(t, restarted, http.MethodPost, Prefix+"/_admin/v1/remote-access", admin, change); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	current := restarted.hub.CurrentRemoteAccessPolicy()
	if current.Revision == p.Revision || len(current.Station.ExternalAPIBases) != 0 || len(current.Station.ReverseProxyAPIBases) != 1 {
		t.Fatal("external revocation changed another category or kept the old epoch")
	}
	station.ExternalAPIBases = []string{"https://external.example/team"}
	if w := adminRequest(t, restarted, http.MethodPost, Prefix+"/_admin/v1/remote-access", admin, change); w.Code != http.StatusConflict {
		t.Fatalf("stale policy revived access: %d %s", w.Code, w.Body.String())
	}
}

func TestStationV2AdminReportsMissingSignatureCoverage(t *testing.T) {
	s, _, admin := serverFixture(t)
	station := &hub.StationPolicy{Version: 2, APIBases: []string{}, ExternalAPIBases: []string{"https://external.example"}, ReverseProxyAPIBases: []string{"https://proxy.example"}, AllowOfficialAccounts: true}
	if w := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/remote-access", admin, stationAdminChange(s.hub.CurrentRemoteAccessPolicy().Revision, station)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w := adminRequest(t, s, http.MethodGet, Prefix+"/_admin/v1/state", admin, nil)
	if !strings.Contains(w.Body.String(), `"remote_access_ready":false`) || !strings.Contains(w.Body.String(), `"status":"issuer_missing"`) || !strings.Contains(w.Body.String(), `"status":"computer_verification"`) {
		t.Fatal(w.Body.String())
	}
}
