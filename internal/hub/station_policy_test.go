package hub

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestStationAPIBaseNormalizesOnlyUnambiguousEquivalentAddresses(t *testing.T) {
	for raw, want := range map[string]string{
		"HTTPS://EXAMPLE.COM:443/team/v1/": "https://example.com/team",
		"https://example.com/Team/":        "https://example.com/Team",
		"https://example.com/v1":           "https://example.com",
		"https://example.com/team/V1/":     "https://example.com/team",
		"http://LOCALHOST:80/v1/":          "http://localhost",
		"http://[::1]:80/team":             "http://[::1]/team",
		"http://[0:0:0:0:0:0:0:1]/team":    "http://[::1]/team",
		"https://example.com:8443/team":    "https://example.com:8443/team",
	} {
		if got, err := NormalizeStationAPIBase(raw); err != nil || got != want {
			t.Errorf("normalize %q = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{
		"", "http://example.com", "https://user@example.com", "https://example.com:",
		"https://example.com:65536", "https://example.com./team", "https://example.com/a//b",
		"https://example.com/a/../b", "https://example.com/a/./b", "https://example.com/%61",
		"https://example.com/a%2fb", "https://example.com/a%252fb", "https://example.com/%25",
		"https://example.com/a\\b", "https://example.com/a?", "https://example.com/a?q=1",
		"https://example.com/a#frag", "https://example.com/a#", "https://example.com/#", "https://example.com/a/v1/v1", "https://example.com/a\nb", "https://example.com/a b", "https://example.com/a\tb",
	} {
		if got, err := NormalizeStationAPIBase(raw); err == nil {
			t.Errorf("ambiguous base %q accepted as %q", raw, got)
		}
	}
}

func TestStationDefaultUsesOnlyTrustedPublicURLAndCompletePrefix(t *testing.T) {
	for _, tc := range []struct{ public, prefix, want string }{
		{"https://A.example:443/team/salcara-hub", "/salcara-hub", "https://a.example/team"},
		{"https://a.example/team/custom-hub", "/custom-hub", "https://a.example/team"},
		{"", "/salcara-hub", ""},
		{"https://a.example/salcara-hub-evil", "/salcara-hub", ""},
		{"https://a.example/%74eam/salcara-hub", "/salcara-hub", ""},
		{"https://a.example/teamsalcara-hub", "/salcara-hub", ""},
	} {
		p, err := ResolveRemoteAccessPolicy(RemoteAccessPolicy{}, tc.public, tc.prefix)
		if err != nil || p.Station == nil || p.Station.Version != 1 || p.Station.AllowOfficialAccounts {
			t.Fatalf("invalid station defaults: %#v %v", p, err)
		}
		if tc.want == "" && len(p.Station.APIBases) != 0 || tc.want != "" && !reflect.DeepEqual(p.Station.APIBases, []string{tc.want}) {
			t.Errorf("default %q/%q = %#v", tc.public, tc.prefix, p.Station)
		}
	}
	h := newDeviceHub(t, Config{})
	r := httptest.NewRequest("GET", "https://attacker.example/salcara-hub/v1/ping", nil)
	r.Header.Set("X-Forwarded-Host", "trusted.example")
	w := httptest.NewRecorder()
	h.handlePing(w, r, "")
	if len(h.CurrentRemoteAccessPolicy().Station.APIBases) != 0 || !strings.Contains(w.Body.String(), `"apiBases":[]`) {
		t.Fatal("untrusted request host supplied station authority")
	}
}

func TestStationPolicyExactJSONAndReservedOfficialIssuer(t *testing.T) {
	for _, raw := range []string{
		`{"station":{"version":1,"apiBases":[],"allowOfficialAccounts":false,"AllowOfficialAccounts":true}}`,
		`{"station":{"version":1,"apiBases":[],"apiBases":["https://evil.example"]}}`,
		`{"station":{"version":1,"apiBases":null}}`,
		`{"station":{"version":3,"apiBases":[]}}`,
		`{"station":null}`, `{"mode":"disabled","Mode":"open"}`,
		`{"station":{"version":1,"apiBases":["https://example.com","https://EXAMPLE.COM:443/v1"]}}`,
	} {
		if _, err := ParseRemoteAccessPolicy([]byte(raw)); err == nil {
			t.Errorf("ambiguous station configuration accepted: %s", raw)
		}
	}
	h, _, _, _, _ := sourceContractFixture(t)
	p := h.CurrentRemoteAccessPolicy()
	p.Issuers[0].ID = stationOfficialIssuer
	if err := h.ApplyRemoteAccessPolicy(p, nil); err == nil {
		t.Fatal("external issuer occupied the process-signer identity")
	}
}

func TestStationOpenGateAndModelsRequireScopeAndCurrentBridge(t *testing.T) {
	h, pair, req, _, _ := sourceContractFixture(t)
	p := h.CurrentRemoteAccessPolicy()
	p.Mode = "open"
	if err := h.ApplyRemoteAccessPolicy(p, nil); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{`{"type":"session.send"}`, `{"type":"models.list"}`, `{"type":"models.list","controlSurface":"desktop"}`} {
		req.Command, req.SourceGrant = json.RawMessage(command), nil
		code, source := h.executionSourceFor("receipt-owner", pair, req)
		if code != "" || source == nil || source.Mode != "open" || source.Station == nil || !reflect.DeepEqual(source.Station.APIBases, []string{"https://example.com"}) {
			t.Fatalf("open execution lost station scope: %s %#v", code, source)
		}
		source.Station.APIBases[0] = "https://evil.example"
		if h.CurrentRemoteAccessPolicy().Station.APIBases[0] != "https://example.com" {
			t.Fatal("execution envelope aliases policy")
		}
	}
	h.mu.Lock()
	h.accounts["receipt-owner"].devices["pc"].info.Capabilities = []string{"source.guard.v1"}
	h.mu.Unlock()
	if code := h.evaluateSourcePolicy("receipt-owner", pair, req); code != "station_guard_unsupported" {
		t.Fatalf("old Bridge accepted: %s", code)
	}
	h.mu.Lock()
	h.accounts["receipt-owner"].devices["pc"].info.Capabilities = []string{"station.guard.v1"}
	h.mu.Unlock()
	p = h.CurrentRemoteAccessPolicy()
	p.Station.APIBases = []string{}
	if err := h.ApplyRemoteAccessPolicy(p, nil); err != nil {
		t.Fatal(err)
	}
	if code := h.evaluateSourcePolicy("receipt-owner", pair, req); code != "station_policy_unavailable" {
		t.Fatalf("empty station executed: %s", code)
	}
	p = h.CurrentRemoteAccessPolicy()
	p.Mode = "disabled"
	if err := h.ApplyRemoteAccessPolicy(p, nil); err != nil {
		t.Fatal(err)
	}
	if code := h.evaluateSourcePolicy("receipt-owner", pair, req); code != "remote_execution_disabled" {
		t.Fatalf("disabled model query accepted: %s", code)
	}
	req.Command = json.RawMessage(`{"type":"sessions.list"}`)
	if code, source := h.executionSourceFor("receipt-owner", pair, req); code != "" || source != nil {
		t.Fatalf("safe history denied: %s", code)
	}
}

func TestStationGrantCanOnlyNarrowCompleteBase(t *testing.T) {
	h, pair, req, snapshot, private := sourceContractFixture(t)
	p := h.CurrentRemoteAccessPolicy()
	p.Station.APIBases = []string{"https://example.com/team-a"}
	if err := h.ApplyRemoteAccessPolicy(p, nil); err != nil {
		t.Fatal(err)
	}
	snapshot.PolicyRevision = h.CurrentRemoteAccessPolicy().Revision
	for _, tc := range []struct{ base, origin, want string }{
		{"https://example.com/team-a", "https://example.com", ""},
		{"https://example.com/team-b", "https://example.com", "source_station_denied"},
		{"https://example.com/team-a/child", "https://example.com", "source_station_denied"},
		{"https://example.com/team-a.evil", "https://example.com", "source_station_denied"},
		{"https://other.example/team-a", "https://other.example", "source_station_denied"},
		{"", "https://example.com", "source_station_denied"},
	} {
		snapshot.APIBase, snapshot.Origin = tc.base, tc.origin
		signSourceFixture(t, &req, snapshot, private)
		if code := h.evaluateSourcePolicy("receipt-owner", pair, req); code != tc.want {
			t.Errorf("base %q: %s, want %s", tc.base, code, tc.want)
		}
	}
}

func TestStationHandoverWithoutGrantIsTransportOnly(t *testing.T) {
	h, pair, req, snapshot, private := sourceContractFixture(t)
	req.Command = json.RawMessage(`{"type":"remote.station.switch","targetHubUrl":"https://b.example/salcara-hub","targetDeviceId":"pc","operationId":"10000000-0000-4000-8000-000000000001","agent":"codex","accountId":"credential-1","model":"gpt"}`)
	canonical, _ := canonicalCommand(req.Command)
	snapshot.CommandHash = shaHex(canonical)
	snapshot.APIBase, snapshot.Origin = "https://b.example", "https://b.example"
	signSourceFixture(t, &req, snapshot, private)
	if code, source := h.executionSourceFor("receipt-owner", pair, req); code != "" || source.Mode != "restricted" {
		t.Fatalf("signed A-to-B handover denied: %s", code)
	}
	p := h.CurrentRemoteAccessPolicy()
	p.Station.APIBases = []string{}
	if err := h.ApplyRemoteAccessPolicy(p, nil); err != nil {
		t.Fatal(err)
	}
	if code := h.evaluateSourcePolicy("receipt-owner", pair, req); code != "source_policy_stale" {
		t.Fatalf("stale attached handover grant accepted: %s", code)
	}
	p = h.CurrentRemoteAccessPolicy()
	p.Issuers = nil
	p.Audience = ""
	if err := h.ApplyRemoteAccessPolicy(p, nil); err != nil {
		t.Fatal(err)
	}
	req.SourceGrant = nil
	if code, source := h.executionSourceFor("receipt-owner", pair, req); code != "" || source == nil || source.Mode != "open" || source.Station == nil || len(source.Station.APIBases) != 0 {
		t.Fatalf("transport required target issuer: %s %#v", code, source)
	}
	req.Command = json.RawMessage(`{"type":"models.list","accountId":"credential-1","agent":"codex"}`)
	if code := h.evaluateSourcePolicy("receipt-owner", pair, req); code != "station_policy_unavailable" {
		t.Fatalf("transport exception widened to model query: %s", code)
	}
}

func TestStationPolicyChangesRotateEpochAndCopiesRemainPrivate(t *testing.T) {
	h, _, _, _, _ := sourceContractFixture(t)
	for _, mutate := range []func(*RemoteAccessPolicy){
		func(p *RemoteAccessPolicy) { p.Station.APIBases = []string{"https://example.com/team"} },
		func(p *RemoteAccessPolicy) { p.Station.AllowOfficialAccounts = true },
		func(p *RemoteAccessPolicy) { p.Audience = "new-audience" },
		func(p *RemoteAccessPolicy) { p.Issuers[0].AgentIDs = []string{"codex"} },
		func(p *RemoteAccessPolicy) { p.Issuers[0].APIBases = []string{"https://example.com/team"} },
	} {
		p := h.CurrentRemoteAccessPolicy()
		old := p.Revision
		mutate(&p)
		if err := h.ApplyRemoteAccessPolicy(p, nil); err != nil {
			t.Fatal(err)
		}
		current := h.CurrentRemoteAccessPolicy()
		if current.Revision == old {
			t.Fatal("security content change did not rotate revision")
		}
		if err := h.ApplyRemoteAccessPolicy(current, nil); err != nil {
			t.Fatal(err)
		}
		if h.CurrentRemoteAccessPolicy().Revision != current.Revision {
			t.Fatal("identical content rotated revision")
		}
		p.Station.APIBases[0] = "https://caller-mutated.example"
		current.Station.APIBases[0] = "https://returned-mutated.example"
		if h.CurrentRemoteAccessPolicy().Station.APIBases[0] != "https://example.com/team" {
			t.Fatal("caller changed live station scope")
		}
	}
}

func TestStationPingPublishesEffectivePolicyAndCapabilities(t *testing.T) {
	h, _, _, _, _ := sourceContractFixture(t)
	w := httptest.NewRecorder()
	h.handlePing(w, httptest.NewRequest("GET", "/ping", nil), "")
	var got struct {
		Capabilities []string `json:"capabilities"`
		RemoteAccess struct {
			Mode, Revision string
			IssuerCount    int `json:"issuerCount"`
			Station        *StationPolicy
		} `json:"remoteAccess"`
	}
	if json.Unmarshal(w.Body.Bytes(), &got) != nil || got.RemoteAccess.Station == nil || got.RemoteAccess.Mode != "restricted" || got.RemoteAccess.Revision != "r2" || got.RemoteAccess.IssuerCount != 1 {
		t.Fatalf("missing station discovery: %s", w.Body.String())
	}
	for _, required := range []string{"station.guard.v1", "station.handover.v2", "session.steer.v1"} {
		found := false
		for _, c := range got.Capabilities {
			found = found || c == required
		}
		if !found {
			t.Errorf("capability %s missing", required)
		}
	}
}
