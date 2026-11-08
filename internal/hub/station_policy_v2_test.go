package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStationV2SeparatesAPIAndReverseProxySources(t *testing.T) {
	p := &StationPolicy{Version: 2, APIBases: []string{"https://own.example/team"}, ExternalAPIBases: []string{"https://other.example/team"}, ReverseProxyAPIBases: []string{"https://proxy.example/team"}}
	for _, tc := range []struct {
		kind, base string
		allow      bool
	}{
		{"site_api", "https://own.example/team", true}, {"site_api", "https://other.example/team", true},
		{"verified_proxy", "https://proxy.example/team", true}, {"site_api", "https://proxy.example/team", false},
		{"verified_proxy", "https://own.example/team", false}, {"verified_proxy", "https://other.example/team", false},
		{"site_api", "https://other.example/team/child", false}, {"site_api", "https://other.example/team-evil", false},
		{"unknown", "https://own.example/team", false}, {"official_account_proxy", officialAccountAPIBase, false},
	} {
		origin, _ := canonicalAPIBase(tc.base)
		if got := p.allowsSnapshot(SourceSnapshot{SourceType: tc.kind, APIBase: tc.base, Origin: origin}); got != tc.allow {
			t.Errorf("%s %s allowed=%v", tc.kind, tc.base, got)
		}
	}
	p.ExternalAPIBases = nil
	if p.allowsSnapshot(SourceSnapshot{SourceType: "site_api", APIBase: "https://other.example/team"}) {
		t.Fatal("disabled external scope stayed enabled")
	}
	if !p.allowsSnapshot(SourceSnapshot{SourceType: "verified_proxy", APIBase: "https://proxy.example/team"}) {
		t.Fatal("external switch disabled proxy scope")
	}
	p.AllowOfficialAccounts = true
	if !p.allowsSnapshot(SourceSnapshot{SourceType: "official_account_proxy", Origin: officialAccountOrigin, APIBase: officialAccountAPIBase}) {
		t.Fatal("official connector was not independently enabled")
	}
}

func TestStationV2StrictSchemaAndLegacyMigrationBudget(t *testing.T) {
	for _, raw := range []string{
		`{"version":1,"apiBases":[],"externalApiBases":[]}`,
		`{"version":1,"apiBases":[],"reverseProxyApiBases":[]}`,
		`{"version":2,"apiBases":[],"externalApiBases":null}`,
		`{"version":2,"apiBases":[],"externalApiBases":[],"externalApiBases":["https://evil.example"]}`,
		`{"version":2,"apiBases":[],"ExternalApiBases":["https://evil.example"]}`,
		`{"version":2,"apiBases":[],"allowAllAccounts":true}`,
		`{"version":2,"apiBases":["https://same.example"],"externalApiBases":["https://SAME.example:443/v1/"]}`,
	} {
		if _, err := ParseStationPolicy([]byte(raw)); err == nil {
			t.Errorf("ambiguous policy accepted: %s", raw)
		}
	}
	p := &StationPolicy{Version: 2}
	for i := 0; i < 32; i++ {
		p.APIBases = append(p.APIBases, fmt.Sprintf("https://api.example/team%d", i))
	}
	p.ReverseProxyAPIBases = append([]string{}, p.APIBases...)
	if err := p.normalize(); err != nil {
		t.Fatalf("legacy 32-address policy could not migrate: %v", err)
	}
	p.ExternalAPIBases = []string{"https://new.example"}
	if err := p.normalize(); err == nil {
		t.Fatal("33 distinct addresses accepted")
	}
}

func TestStationV2RequiresCapableBridgeAndRejectsRevokedSignedSource(t *testing.T) {
	h, pair, req, snapshot, private := sourceContractFixture(t)
	p := h.CurrentRemoteAccessPolicy()
	p.Station = &StationPolicy{Version: 2, APIBases: []string{}, ExternalAPIBases: []string{"https://example.com"}, ReverseProxyAPIBases: []string{"https://example.com"}}
	p.Issuers[0].SourceTypes = []string{"site_api", "verified_proxy"}
	if err := h.ApplyRemoteAccessPolicy(p, nil); err != nil {
		t.Fatal(err)
	}
	if code := h.evaluateSourcePolicy("receipt-owner", pair, req); code != "station_guard_unsupported" {
		t.Fatalf("old Bridge passed v2 policy: %s", code)
	}
	h.mu.Lock()
	h.accounts["receipt-owner"].devices["pc"].info.Capabilities = append(h.accounts["receipt-owner"].devices["pc"].info.Capabilities, "station.guard.v2")
	h.mu.Unlock()
	snapshot.PolicyRevision = h.CurrentRemoteAccessPolicy().Revision
	for _, kind := range []string{"site_api", "verified_proxy"} {
		snapshot.SourceType = kind
		signSourceFixture(t, &req, snapshot, private)
		if code := h.evaluateSourcePolicy("receipt-owner", pair, req); code != "" {
			t.Fatalf("allowed %s rejected: %s", kind, code)
		}
	}
	p = h.CurrentRemoteAccessPolicy()
	p.Station.ReverseProxyAPIBases = nil
	if err := h.ApplyRemoteAccessPolicy(p, nil); err != nil {
		t.Fatal(err)
	}
	if code := h.evaluateSourcePolicy("receipt-owner", pair, req); code == "" {
		t.Fatal("old proxy grant survived revocation")
	}
	snapshot.PolicyRevision = h.CurrentRemoteAccessPolicy().Revision
	signSourceFixture(t, &req, snapshot, private)
	if code := h.evaluateSourcePolicy("receipt-owner", pair, req); code != "source_station_denied" {
		t.Fatalf("proxy used API-only permission: %s", code)
	}
	snapshot.SourceType = "site_api"
	signSourceFixture(t, &req, snapshot, private)
	if code := h.evaluateSourcePolicy("receipt-owner", pair, req); code != "" {
		t.Fatalf("proxy revoke disabled API: %s", code)
	}
	p = h.CurrentRemoteAccessPolicy()
	p.Mode = "disabled"
	if err := h.ApplyRemoteAccessPolicy(p, nil); err != nil {
		t.Fatal(err)
	}
	if code := h.evaluateSourcePolicy("receipt-owner", pair, req); code != "remote_execution_disabled" {
		t.Fatal(code)
	}
	for _, typ := range []string{"session.interrupt", "session.open"} {
		req.Command = json.RawMessage(`{"type":"` + typ + `"}`)
		req.SourceGrant = nil
		if code := h.evaluateSourcePolicy("receipt-owner", pair, req); code != "" {
			t.Fatalf("safety/read operation rejected: %s", code)
		}
	}
}

func TestStationV2SnapshotsDoNotAliasAndPublishCapability(t *testing.T) {
	h, _, _, _, _ := sourceContractFixture(t)
	p := h.CurrentRemoteAccessPolicy()
	p.Station = &StationPolicy{Version: 2, APIBases: []string{}, ExternalAPIBases: []string{"https://external.example"}, ReverseProxyAPIBases: []string{"https://proxy.example"}}
	if err := h.ApplyRemoteAccessPolicy(p, nil); err != nil {
		t.Fatal(err)
	}
	p.Station.ExternalAPIBases[0] = "https://changed.example"
	copy := h.CurrentRemoteAccessPolicy()
	copy.Station.ReverseProxyAPIBases[0] = "https://changed.example"
	w := httptest.NewRecorder()
	h.handlePing(w, httptest.NewRequest("GET", "/ping", nil), "")
	if !strings.Contains(w.Body.String(), `"station.guard.v2"`) || !strings.Contains(w.Body.String(), `"externalApiBases":["https://external.example"]`) || strings.Contains(w.Body.String(), "changed.example") {
		t.Fatal(w.Body.String())
	}
}

func TestStationV2SourceStatusDoesNotEquateIssuerCountWithCoverage(t *testing.T) {
	p := RemoteAccessPolicy{Mode: "restricted", Audience: "test", Station: &StationPolicy{Version: 2, ExternalAPIBases: []string{"https://external.example/team"}, ReverseProxyAPIBases: []string{"https://proxy.example/team"}}, Issuers: []SourceIssuer{{ID: "unrelated", SourceTypes: []string{"site_api"}}}}
	status := p.SourceAuthorizationStatus()
	if status[1]["status"] != "issuer_missing" || status[2]["status"] != "issuer_missing" {
		t.Fatal(status)
	}
	p.Issuers = []SourceIssuer{{ID: "external", SourceTypes: []string{"site_api"}, Origins: []string{"https://external.example"}, APIBases: []string{"https://external.example/team"}, AuthorizationURL: "https://external.example/remote/grant"}}
	status = p.SourceAuthorizationStatus()
	if status[1]["status"] != "issuer_configured" || status[2]["status"] != "issuer_missing" {
		t.Fatal(status)
	}
	p.Issuers = append(p.Issuers, p.Issuers[0])
	p.Issuers[1].ID = "ambiguous"
	if p.SourceAuthorizationStatus()[1]["status"] != "issuer_missing" {
		t.Fatal("ambiguous issuer falsely ready")
	}
}

func TestStationGlobalDisableAppliesToAllBotPolicyVersions(t *testing.T) {
	h, _, _ := botFixture(t)
	defer h.Close()
	pack, err := h.CreateBotBindingPackage()
	if err != nil {
		t.Fatal(err)
	}
	device := botDevice("33333333-3333-4333-8333-333333333333", "test")
	device.Capabilities = []string{"station.guard.v1", "station.guard.v2"}
	if w := botRegisterRequest(t, h, pack, device, strings.Repeat("d", 64)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	bound := botRequest(t, h, http.MethodPost, "/v1/bot/phone/bind", map[string]any{"binding": pack, "phoneId": strings.Repeat("e", 64)}, nil)
	var phone struct {
		BotToken string `json:"botToken"`
	}
	if json.Unmarshal(bound.Body.Bytes(), &phone) != nil || phone.BotToken == "" {
		t.Fatal(bound.Body.String())
	}
	r := httptest.NewRequest("POST", "/app/commands", nil)
	r.Header.Set("X-Salcara-Bot-Token", phone.BotToken)
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			p := h.CurrentRemoteAccessPolicy()
			p.Mode = "disabled"
			p.Station = &StationPolicy{Version: version, APIBases: []string{}}
			if err := h.ApplyRemoteAccessPolicy(p, nil); err != nil {
				t.Fatal(err)
			}
			for _, typ := range []string{"session.start", "session.send", "approval.respond", "models.list"} {
				req := commandReq{DeviceID: device.DeviceID, Command: json.RawMessage(`{"type":"` + typ + `"}`)}
				if code := h.evaluateSourcePolicy("bot:"+pack.GroupID, r, req); code != "remote_execution_disabled" {
					t.Errorf("Bot %s bypassed global disable: %q", typ, code)
				}
			}
			for _, typ := range []string{"device.ping", "session.open", "session.interrupt"} {
				req := commandReq{DeviceID: device.DeviceID, Command: json.RawMessage(`{"type":"` + typ + `"}`)}
				if code := h.evaluateSourcePolicy("bot:"+pack.GroupID, r, req); code != "" {
					t.Errorf("global disable blocked safe %s: %s", typ, code)
				}
			}
		})
	}
	// Turning the global switch back on retains the existing v1 Bot source
	// contract; migrating source categories is a separate administrator action.
	p := h.CurrentRemoteAccessPolicy()
	p.Mode = "open"
	p.Station = &StationPolicy{Version: 1, APIBases: []string{}}
	if err := h.ApplyRemoteAccessPolicy(p, nil); err != nil {
		t.Fatal(err)
	}
	req := commandReq{DeviceID: device.DeviceID, Command: json.RawMessage(`{"type":"session.send"}`)}
	if code := h.evaluateSourcePolicy("bot:"+pack.GroupID, r, req); code != "" {
		t.Fatalf("enabled legacy Bot contract regressed: %s", code)
	}
}

func TestStationV2SourceStatusRespectsIssuerAgentScopes(t *testing.T) {
	base := "https://external.example/team"
	issuer := SourceIssuer{ID: "codex", SourceTypes: []string{"site_api"}, AgentIDs: []string{"codex", "codex"}, Origins: []string{"https://external.example"}, APIBases: []string{base}, AuthorizationURL: "https://external.example/source-grants"}
	p := RemoteAccessPolicy{Mode: "restricted", Audience: "test", Station: &StationPolicy{Version: 2, ExternalAPIBases: []string{base}}, Issuers: []SourceIssuer{issuer}}
	other := issuer
	other.ID, other.AgentIDs = "claude", []string{"claude"}
	p.Issuers = append(p.Issuers, other)
	if status := p.SourceAuthorizationStatus()[1]; status["status"] != "issuer_configured" || status["issuerCoveredCount"] != 1 {
		t.Fatalf("disjoint Agent issuers were treated as ambiguous: %v", status)
	}
	p.Issuers[1].AgentIDs = []string{"codex", "claude"}
	if status := p.SourceAuthorizationStatus()[1]; status["status"] != "issuer_missing" || status["issuerCoveredCount"] != 0 {
		t.Fatalf("overlapping Agent issuers appeared ready: %v", status)
	}
	p.Issuers[1].AgentIDs = nil
	if p.SourceAuthorizationStatus()[1]["status"] != "issuer_missing" {
		t.Fatal("unrestricted issuer overlap appeared ready")
	}
	p.Issuers = p.Issuers[1:]
	if p.SourceAuthorizationStatus()[1]["status"] != "issuer_configured" {
		t.Fatal("single unrestricted issuer appeared unconfigured")
	}
}
