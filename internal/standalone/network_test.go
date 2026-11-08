package standalone

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"salcara/hub/internal/edgepool"
	"strings"
	"sync/atomic"
	"testing"
)

func networkFixture(t *testing.T, target string) (*Server, string) {
	t.Helper()
	cfg := testConfig(t)
	cfg.NetworkControlURL = target
	cfg.NetworkControlTokenFile = filepath.Join(cfg.DataDir, "network-control-token")
	if err := os.WriteFile(cfg.NetworkControlTokenFile, []byte(strings.Repeat("a", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := InitAdminAccount(cfg); err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, initialPassword(t, cfg)
}

func TestNetworkLocalSubscriptionDetectionRejectsPrivateTargetsWithoutAnEdge(t *testing.T) {
	s, _, password := serverFixture(t)
	path := Prefix + "/_admin/v1/network/subscriptions/inspect"
	w := adminRequest(t, s, "POST", path, password, map[string]string{"url": "https://127.0.0.1/private?token=must-not-echo"})
	if w.Code != 400 || strings.Contains(w.Body.String(), "must-not-echo") || !strings.Contains(w.Body.String(), "公网 HTTPS") {
		t.Fatalf("local inspector: %d %s", w.Code, w.Body.String())
	}
	// More than the old 4 KiB JSON limit reaches the inspector's bounded URL
	// validation, rather than being accidentally rejected by the admin decoder.
	w = adminRequest(t, s, "POST", path, password, map[string]string{"url": "https://127.0.0.1/" + strings.Repeat("a", 5000)})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "公网 HTTPS") {
		t.Fatalf("body budget: %d %s", w.Code, w.Body.String())
	}
	w = adminRequest(t, s, "POST", path, "", map[string]string{"url": "https://example.com"})
	if w.Code != 401 {
		t.Fatal("anonymous subscription inspection permitted")
	}
}

func TestNetworkBridgeToRealEdgeController(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"status":"available"}`) }))
	defer origin.Close()
	pool, err := edgepool.New(edgepool.Config{Origin: origin.URL, DataDir: filepath.Join(t.TempDir(), "edge"), ControlToken: strings.Repeat("a", 64), DisablePeriodicProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	controller := httptest.NewServer(pool.ControlHandler())
	defer controller.Close()
	s, password := networkFixture(t, controller.URL)
	w := adminRequest(t, s, "GET", Prefix+"/_admin/v1/network/state", password, nil)
	var snapshot struct {
		Revision   int64 `json:"revision"`
		Configured bool  `json:"configured"`
	}
	if json.Unmarshal(w.Body.Bytes(), &snapshot) != nil || w.Code != 200 || !snapshot.Configured {
		t.Fatalf("state: %d", w.Code)
	}
	w = adminRequest(t, s, "POST", Prefix+"/_admin/v1/network/nodes", password, map[string]any{"expected_revision": snapshot.Revision, "node": map[string]any{"name": "Mihomo 本机入口", "proxy_url": "socks5://test-user:test-password@127.0.0.1:17890", "enabled": true}})
	if w.Code != 200 || strings.Contains(w.Body.String(), "test-password") || strings.Contains(w.Body.String(), "test-user") {
		t.Fatalf("node: %d %s", w.Code, w.Body.String())
	}
	w = adminRequest(t, s, "POST", Prefix+"/_admin/v1/network/policy", password, map[string]any{"expected_revision": snapshot.Revision, "mode": "failover", "selected_id": "direct"})
	if w.Code != 409 || pool.State().Mode != "manual" {
		t.Fatal("stale policy not atomic")
	}
	w = adminRequest(t, s, "GET", Prefix+"/_admin/v1/network/sources/state", password, nil)
	var sources struct {
		SourceRevision int64  `json:"source_revision"`
		ControllerID   string `json:"controller_id"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &sources) != nil || sources.SourceRevision < 1 || sources.ControllerID == "" {
		t.Fatal("source state missing")
	}
	w = adminRequest(t, s, "POST", Prefix+"/_admin/v1/network/sources/save", password, map[string]any{"expected_controller_id": sources.ControllerID, "expected_source_revision": sources.SourceRevision, "name": "Provider", "url": "https://example.com/sub?token=private-source-marker", "interval_ms": 21600000})
	if w.Code != 202 || strings.Contains(w.Body.String(), "private-source-marker") {
		t.Fatalf("source save: %d", w.Code)
	}
	w = adminRequest(t, s, "GET", Prefix+"/_admin/v1/network/observability", password, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"decision"`) || !strings.Contains(w.Body.String(), `"samples"`) {
		t.Fatal("observability not bridged")
	}
	w = adminRequest(t, s, "POST", Prefix+"/_admin/v1/network/observability/query", password, map[string]any{"expected_controller_id": sources.ControllerID, "window_ms": 3600000, "node_id": "direct"})
	if w.Code != 200 || strings.Contains(w.Body.String(), "test-password") {
		t.Fatal("node trend failed")
	}
	w = adminRequest(t, s, "POST", Prefix+"/_admin/v1/network/sources/refresh", password, map[string]any{"expected_controller_id": "previous-controller", "id": "unused"})
	if w.Code != 409 {
		t.Fatal("stale source controller accepted")
	}
}

func TestNetworkSourceReleaseAndTrendProjection(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"source_revision":2,"sources":[{"id":"one","url":"PRIVATE_SOURCE","nodes":[{"key":"node","proxy":{"password":"PRIVATE_PROXY"}}]}],"kernel":{"status":"ready","config":{"secret":"PRIVATE_KERNEL"}},"release":{"history":[{"id":"old","snapshot":{"password":"PRIVATE_HISTORY"}}]},"decision":{"mode":"balance","secret":"PRIVATE_DECISION","candidates":[{"id":"node","proxy_url":"PRIVATE_CANDIDATE"}]},"samples":[{"at":1,"nodes":[{"id":"node","secret":"PRIVATE_SAMPLE"}]}],"raw":"PRIVATE_RAW"}`)
	}))
	defer upstream.Close()
	s, password := networkFixture(t, upstream.URL)
	for _, path := range []string{"sources/state", "observability", "state"} {
		w := adminRequest(t, s, "GET", Prefix+"/_admin/v1/network/"+path, password, nil)
		if w.Code != 200 || strings.Contains(w.Body.String(), "PRIVATE_") {
			t.Fatalf("public projection failed: %s", path)
		}
	}
}

func TestNetworkGeoProjectionAndIdentityFence(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"revision":7,"installed":true,"enabled":true,"groups":[{"asn":13335,"node_ids":["n"],"secret":"PRIVATE_GROUP"}],"raw":"PRIVATE_DB","nodes":[{"id":"n","network":{"status":"observed","country":"HK","config_key":"PRIVATE_KEY"},"effective_region":"HK"}],"geo_available":true,"source_limits":[{"source_id":"s","max_requests":5,"max_streams":3,"token":"PRIVATE_SOURCE"}],"source_groups":[{"source_id":"s","limited":true,"runtime":{"active_streams":2,"token":"PRIVATE_RUNTIME"},"private":"PRIVATE_LEDGER"}]}`)
	}))
	defer upstream.Close()
	s, password := networkFixture(t, upstream.URL)
	for _, path := range []string{"geodata/state", "state"} {
		w := adminRequest(t, s, "GET", Prefix+"/_admin/v1/network/"+path, password, nil)
		if w.Code != 200 || strings.Contains(w.Body.String(), "PRIVATE_") {
			t.Fatalf("projection: %s", w.Body.String())
		}
		if path == "state" && (!strings.Contains(w.Body.String(), `"source_limits"`) || !strings.Contains(w.Body.String(), `"active_streams":2`)) {
			t.Fatal("source capacity projection lost public counters")
		}
	}
	before := calls.Load()
	w := adminRequest(t, s, "POST", Prefix+"/_admin/v1/network/geodata/install", password, map[string]any{"expected_controller_id": "old-controller", "expected_revision": 7})
	if w.Code != 409 || calls.Load() != before {
		t.Fatal("old target installation was not fenced")
	}
	w = adminRequest(t, s, "GET", Prefix+"/_admin/v1/network/geodata/state", "", nil)
	if w.Code != 401 || calls.Load() != before {
		t.Fatal("geodata state bypassed admin authentication")
	}
}

func TestNetworkNotConfiguredAndRequiresAdmin(t *testing.T) {
	s, _, password := serverFixture(t)
	path := Prefix + "/_admin/v1/network/state"
	if w := adminRequest(t, s, "GET", path, "", nil); w.Code != 401 {
		t.Fatalf("anonymous: %d", w.Code)
	}
	w := adminRequest(t, s, "GET", path, password, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"configured":false`) {
		t.Fatalf("state: %d %s", w.Code, w.Body.String())
	}
	if w = adminRequest(t, s, "POST", Prefix+"/_admin/v1/network/probe", password, map[string]any{}); w.Code != 503 {
		t.Fatalf("unconfigured probe: %d", w.Code)
	}
	if w = adminRequest(t, s, "GET", path+"?token=untrusted", password, nil); w.Code != 400 {
		t.Fatalf("query: %d", w.Code)
	}
}

func TestNetworkControlSeparatesSecretsAndRequiresCSRF(t *testing.T) {
	var called int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("a", 64) || r.Header.Get("Cookie") != "" || r.Header.Get("X-Salcara-CSRF") != "" {
			t.Error("browser identity reached controller or token missing")
		}
		if r.URL.Path != "/v1/state" && r.URL.Path != "/v1/probe" {
			t.Errorf("path: %s", r.URL.Path)
		}
		if r.Method == "POST" {
			w.WriteHeader(202)
		}
		_, _ = io.WriteString(w, `{"revision":1,"origin":"https://origin.example","nodes":[{"id":"one","name":"test","proxy_url":"socks5://operator:hidden@proxy.example:1080","has_credentials":true,"password":"never-send"}],"control_token":"never-send","status":"ready"}`)
	}))
	defer upstream.Close()
	s, password := networkFixture(t, upstream.URL)
	w := adminRequest(t, s, "GET", Prefix+"/_admin/v1/network/state", password, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"configured":true`) || strings.Contains(w.Body.String(), "hidden") || strings.Contains(w.Body.String(), "never-send") || strings.Contains(w.Body.String(), "operator") {
		t.Fatalf("bad public state: %d %s", w.Code, w.Body.String())
	}
	session := fixtureLogin(t, s, password)
	r := httptest.NewRequest("POST", fixtureOrigin(s)+Prefix+"/_admin/v1/network/probe", strings.NewReader(`{}`))
	r.Header.Set("Origin", fixtureOrigin(s))
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(session.cookie)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 || called != 1 {
		t.Fatal("CSRF missing did not stop upstream mutation")
	}
	w = adminRequest(t, s, "POST", Prefix+"/_admin/v1/network/probe", password, map[string]any{})
	if w.Code != 202 || called != 2 {
		t.Fatalf("probe: %d", w.Code)
	}
}

func TestNetworkControlRejectsRedirectAndScrubsErrorBodies(t *testing.T) {
	var leaked bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true; w.WriteHeader(200) }))
	defer target.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/state" {
			http.Redirect(w, r, target.URL, 307)
			return
		}
		w.WriteHeader(409)
		_, _ = io.WriteString(w, `{"error":"https://secret:password@example/?token=private"}`)
	}))
	defer upstream.Close()
	s, password := networkFixture(t, upstream.URL)
	w := adminRequest(t, s, "GET", Prefix+"/_admin/v1/network/state", password, nil)
	if w.Code != 502 || leaked {
		t.Fatal("controller redirect followed")
	}
	w = adminRequest(t, s, "POST", Prefix+"/_admin/v1/network/policy", password, map[string]any{"expected_revision": 1, "mode": "manual", "selected_id": "direct"})
	if w.Code != 409 || strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), "password") {
		t.Fatalf("unsafe error: %d %s", w.Code, w.Body.String())
	}
}

func TestNetworkControlConfigurationAndBoundedResponses(t *testing.T) {
	for _, target := range []string{"https://u:p@example.com", "http://public.example", "https://example.com/path", "https://example.com?token=x", "ftp://example.com", "http://0.0.0.0:10", "https://example.com:0"} {
		cfg := testConfig(t)
		cfg.NetworkControlURL = target
		cfg.NetworkControlTokenFile = filepath.Join(cfg.DataDir, "network-token")
		if err := cfg.Validate(); err == nil {
			t.Fatalf("accepted %s", target)
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", (256<<10)+1))
	}))
	defer upstream.Close()
	s, password := networkFixture(t, upstream.URL)
	w := adminRequest(t, s, "GET", Prefix+"/_admin/v1/network/state", password, nil)
	if w.Code != 502 {
		t.Fatal("oversized response accepted")
	}
	var out map[string]any
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out) != 1 {
		t.Fatal("unexpected error schema")
	}
}

func TestNetworkPolicyProgressWhitelistAndScopedProbeForwarding(t *testing.T) {
	var calls atomic.Int64
	var probeBody map[string]json.RawMessage
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method == http.MethodPost {
			if r.URL.Path != "/v1/probe" || json.NewDecoder(r.Body).Decode(&probeBody) != nil {
				t.Error("unexpected forwarded probe")
			}
			w.WriteHeader(http.StatusAccepted)
		}
		io.WriteString(w, `{"revision":9,"status":"ready","health_policy":{"enabled":false,"interval_ms":5000,"timeout_ms":100,"concurrency":2,"failure_threshold":3,"recovery_successes":2,"recovery_hold_ms":0,"stale_after_ms":6000,"password":"never-forward-secret"},"selection_policy":{"switch_cooldown_ms":0,"min_improvement_ms":30,"min_improvement_percent":20,"preferred_regions":["HK"],"region_mode":"only","allow_unknown":false,"private_key":"never-forward-secret"},"next_probe_at":123,"probe_run":{"id":"run-one","scope":"all","trigger":"manual","started_at":100,"finished_at":0,"total":2,"completed":1,"succeeded":1,"failed":0,"skipped":0,"running":true,"pending":false,"token":"never-forward-secret"},"nodes":[{"id":"node-one","name":"node one","region":"HK","health":{"status":"healthy","latency_ms":20,"latency_ewma_ms":25,"checked_at":100,"last_probe_at":100,"sample_age_ms":23,"stale":false,"consecutive_failures":0,"consecutive_successes":2,"secret":"never-forward-secret"}}]}`)
	}))
	defer upstream.Close()
	s, password := networkFixture(t, upstream.URL)
	w := adminRequest(t, s, "GET", Prefix+"/_admin/v1/network/state", password, nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "never-forward-secret") {
		t.Fatalf("unfiltered policy response: %d %s", w.Code, w.Body.String())
	}
	var state struct {
		edgepool.State
		ControllerID string `json:"controller_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.HealthPolicy.IntervalMS != 5000 || state.SelectionPolicy.RegionMode != "only" || state.NextProbeAt != 123 || state.ProbeRun == nil || state.ProbeRun.Completed != 1 || state.Nodes[0].Region != "HK" || *state.Nodes[0].Health.LatencyEWMAMS != 25 || *state.Nodes[0].Health.SampleAgeMS != 23 {
		t.Fatal("typed projection removed public fields")
	}
	w = adminRequest(t, s, "POST", Prefix+"/_admin/v1/network/probe", password, map[string]any{"expected_controller_id": "stale-controller", "scope": "all", "node_ids": []string{"node-one"}})
	if w.Code != 409 || calls.Load() != 1 {
		t.Fatal("stale controller probe reached the remote endpoint")
	}
	w = adminRequest(t, s, "POST", Prefix+"/_admin/v1/network/probe", password, map[string]any{"expected_controller_id": state.ControllerID, "scope": "all", "node_ids": []string{"node-one"}})
	if w.Code != 202 || calls.Load() != 2 {
		t.Fatalf("scoped probe failed: %d %s", w.Code, w.Body.String())
	}
	if string(probeBody["scope"]) != `"all"` || string(probeBody["node_ids"]) != `["node-one"]` || probeBody["expected_controller_id"] != nil {
		t.Fatalf("incorrect probe forwarding: %s", probeBody)
	}
}

func TestNetworkRejectsMalformedTypedPolicyState(t *testing.T) {
	for _, body := range []string{`{"health_policy":{"concurrency":"invalid"}}`, `{"selection_policy":{"preferred_regions":{"secret":"hidden"}}}`, `{"probe_run":{"total":[]}}`, `{"next_probe_at":"tomorrow"}`, `{"nodes":[{"region":{"secret":"hidden"}}]}`, `{"nodes":[{"health":{"latency_ewma_ms":"hidden"}}]}`} {
		t.Run(body, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
			defer upstream.Close()
			s, password := networkFixture(t, upstream.URL)
			w := adminRequest(t, s, "GET", Prefix+"/_admin/v1/network/state", password, nil)
			if w.Code != 502 || strings.Contains(w.Body.String(), "hidden") {
				t.Fatalf("invalid typed state reached browser: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
