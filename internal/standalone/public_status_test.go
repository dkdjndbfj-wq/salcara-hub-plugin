package standalone

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"salcara/hub/internal/hub"
)

func TestPublicSourceStatusesFollowEffectivePolicyWithoutPrivateDetails(t *testing.T) {
	s, _, _ := serverFixture(t)
	policy := hub.RemoteAccessPolicy{
		Mode: "open", Audience: "private-test-audience",
		Station: &hub.StationPolicy{Version: 2, APIBases: []string{"https://local.example/team"}, ExternalAPIBases: []string{"https://external.example/private-team"}, ReverseProxyAPIBases: []string{"https://proxy.example/private-team"}, AllowOfficialAccounts: true},
	}
	check := func(want [4]string) {
		t.Helper()
		if err := s.hub.ApplyRemoteAccessPolicy(policy, nil); err != nil {
			t.Fatal(err)
		}
		w := adminRequest(t, s, http.MethodGet, Prefix+"/statusz", "", nil)
		var result publicConnectionStatus
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Status != "available" {
			t.Fatalf("public status failed: %d %s", w.Code, w.Body.String())
		}
		var raw struct {
			Sources []map[string]json.RawMessage `json:"sources"`
		}
		if json.Unmarshal(w.Body.Bytes(), &raw) != nil || len(raw.Sources) != 4 {
			t.Fatal("source categories were not bounded")
		}
		ids := [4]string{"api", "external_api", "reverse_proxy", "official_account"}
		for i, source := range result.Sources {
			if source.ID != ids[i] || source.Status != want[i] || len(raw.Sources[i]) != 2 {
				t.Fatalf("category %d: got %+v, expected %s/%s with exactly two public fields", i, source, ids[i], want[i])
			}
		}
		for _, private := range []string{"https:", "private-team", "private-test-audience", "issuer-private-id", "apiBases", "publicKey", "addressCount", "issuerCoveredCount", "credential", "device"} {
			if strings.Contains(w.Body.String(), private) {
				t.Fatalf("private deployment details escaped: %s", private)
			}
		}
		if w.Body.Len() > 512 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("public status was oversized or cacheable")
		}
		ping := adminRequest(t, s, http.MethodGet, Prefix+"/v1/ping", "", nil)
		var discovery struct {
			Sources [4]publicSourceStatus `json:"source_status"`
		}
		if ping.Code != http.StatusOK || json.Unmarshal(ping.Body.Bytes(), &discovery) != nil || !reflect.DeepEqual(result.Sources, discovery.Sources) {
			t.Fatal("homepage and status page reported different effective authorization")
		}
	}
	check([4]string{"allowlist", "allowlist", "allowlist", "allowlist"})
	policy.Mode = "restricted"
	check([4]string{"issuer_missing", "issuer_missing", "issuer_missing", "computer_verification"})
	policy.Issuers = []hub.SourceIssuer{{ID: "issuer-private-id", PublicKey: base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("a", 32))), SourceTypes: []string{"site_api"}, Origins: []string{"https://external.example"}, APIBases: []string{"https://external.example/private-team"}, AuthorizationURL: "https://external.example/source/authorize"}}
	check([4]string{"issuer_missing", "issuer_configured", "issuer_missing", "computer_verification"})
	policy.Station.ExternalAPIBases = nil
	check([4]string{"issuer_missing", "disabled", "issuer_missing", "computer_verification"})
	policy.Mode = "disabled"
	check([4]string{"paused", "disabled", "paused", "paused"})
	policy.Mode = "open"
	policy.Station = &hub.StationPolicy{Version: 1, APIBases: []string{"https://legacy.example/team"}}
	check([4]string{"allowlist", "disabled", "allowlist", "disabled"})
}
