package standalone

import (
	"encoding/json"
	"fmt"
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

func TestStationAdminSizeEscapedListsSaveReloadAndRevoke(t *testing.T) {
	s, cfg, admin := serverFixture(t)
	station := &hub.StationPolicy{Version: 2, APIBases: []string{}}
	for i := 0; i < 32; i++ {
		prefix := fmt.Sprintf("https://api.example/team%d/", i)
		base := prefix + strings.Repeat("&", 4096-len(prefix))
		if i < 16 {
			station.APIBases = append(station.APIBases, base)
		} else {
			station.ExternalAPIBases = append(station.ExternalAPIBases, base)
		}
		station.ReverseProxyAPIBases = append(station.ReverseProxyAPIBases, base)
	}
	change := stationAdminChange(s.hub.CurrentRemoteAccessPolicy().Revision, station)
	change["mode"] = "open"
	w := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/remote-access", admin, change)
	if w.Code != 200 {
		t.Fatalf("full escaped policy save: %d %s", w.Code, w.Body.String())
	}
	saved := s.hub.CurrentRemoteAccessPolicy()
	raw, err := os.ReadFile(filepath.Join(cfg.DataDir, remoteAccessModeFile))
	if err != nil || len(raw) <= 160<<10 || len(raw) > hub.MaxSourcePolicyWireBytes {
		t.Fatalf("saved bytes=%d err=%v", len(raw), err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if !reflect.DeepEqual(saved, restarted.hub.CurrentRemoteAccessPolicy()) {
		t.Fatal("restart lost a saved source scope")
	}
	station.ReverseProxyAPIBases = nil
	change = stationAdminChange(saved.Revision, station)
	change["mode"] = "open"
	if w := adminRequest(t, restarted, http.MethodPost, Prefix+"/_admin/v1/remote-access", admin, change); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	now := restarted.hub.CurrentRemoteAccessPolicy()
	if now.Revision == saved.Revision || len(now.Station.ReverseProxyAPIBases) != 0 || len(now.Station.APIBases) != 16 || len(now.Station.ExternalAPIBases) != 16 {
		t.Fatal("large-policy revocation changed another scope")
	}
	before, _ := os.ReadFile(filepath.Join(cfg.DataDir, remoteAccessModeFile))
	if _, err := saveRemoteAccessModeData(cfg.DataDir, []byte(strings.Repeat(" ", hub.MaxSourcePolicyWireBytes+1))); err == nil {
		t.Fatal("wrote data the loader cannot read")
	}
	after, _ := os.ReadFile(filepath.Join(cfg.DataDir, remoteAccessModeFile))
	if string(before) != string(after) || !json.Valid(after) {
		t.Fatal("oversized save corrupted previous setting")
	}
}
