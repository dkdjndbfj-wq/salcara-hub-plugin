package edgepool

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestObservationDeltasAndRuntimeReconstruction(t *testing.T) {
	p := testPool(t, "https://origin.example.test")
	now := time.Now().Truncate(time.Minute)
	p.mu.Lock()
	p.observability = &poolObservability{samples: []TrendSample{}, nodes: map[string]runtimeBaseline{}}
	p.runtime.CompletedRequests = 10
	p.routes["direct"].runtime.CompletedRequests = 7
	latency := 12.0
	p.routes["direct"].health = Health{Status: "healthy", CheckedAt: now.UnixMilli(), LatencyEWMAMS: &latency}
	p.recordObservationLocked(now.Add(-2 * time.Minute))
	p.runtime.CompletedRequests = 15
	p.routes["direct"].runtime.CompletedRequests = 10
	p.recordObservationLocked(now.Add(-time.Minute))
	// A rebuilt runtime with the same node ID has a new identity. Its first
	// counters are all new traffic, even if the value exceeds the old baseline.
	p.routes["direct"].runtime = &Runtime{CompletedRequests: 20}
	p.runtime.CompletedRequests = 3 // defensive process counter reset
	p.recordObservationLocked(now)
	p.mu.Unlock()
	s := p.Observability("direct", 0)
	if len(s.Samples) != 3 {
		t.Fatal("missing samples")
	}
	for i, want := range []uint64{10, 5, 3} {
		if s.Samples[i].Runtime.CompletedRequests != want {
			t.Fatalf("pool delta %d: %d", i, s.Samples[i].Runtime.CompletedRequests)
		}
	}
	for i, want := range []uint64{7, 3, 20} {
		if s.Samples[i].Nodes[0].Runtime.CompletedRequests != want {
			t.Fatalf("node delta %d: %d", i, s.Samples[i].Nodes[0].Runtime.CompletedRequests)
		}
	}
	*s.Samples[0].Nodes[0].LatencyEWMAMS = 999
	s.Decision.Candidates[0].ID = "changed"
	again := p.Observability("direct", 0)
	if *again.Samples[0].Nodes[0].LatencyEWMAMS != 12 || again.Decision.Candidates[0].ID != "direct" {
		t.Fatal("caller mutated telemetry state")
	}
	if len(p.Observability("", time.Minute).Samples) != 1 || len(p.Observability("", time.Minute).Samples[0].Nodes) != 0 {
		t.Fatal("window or pool projection mismatch")
	}
}

func TestObservationRestartMergesBucketWithoutCounterSpike(t *testing.T) {
	p := testPool(t, "https://origin.example.test")
	p.mu.Lock()
	p.runtime.CompletedRequests = 7
	p.routes["direct"].runtime.CompletedRequests = 4
	p.mu.Unlock()
	p.persistObservation(time.Now())
	cfg := p.cfg
	cfg.ControlToken = testToken
	p.Close()
	reopened, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.State().Runtime.CompletedRequests != 0 {
		t.Fatal("restored telemetry changed process counters")
	}
	reopened.mu.Lock()
	reopened.runtime.CompletedRequests = 2
	reopened.routes["direct"].runtime.CompletedRequests = 1
	reopened.mu.Unlock()
	reopened.persistObservation(time.Now())
	s := reopened.Observability("direct", 0)
	var total, node uint64
	for _, sample := range s.Samples {
		total += sample.Runtime.CompletedRequests
		for _, n := range sample.Nodes {
			node += n.Runtime.CompletedRequests
		}
	}
	if total != 9 || node != 5 {
		t.Fatalf("restart fabricated or lost deltas: pool %d node %d", total, node)
	}
}

func TestObservationBoundedRetentionAndSecretFreeFile(t *testing.T) {
	p := testPool(t, "https://origin.example.test")
	for i := 0; i < 4; i++ {
		addNode(t, p, "node", "http://private-user:private-password@proxy.example.test:8080")
	}
	now := time.Now().Truncate(time.Minute)
	p.mu.Lock()
	p.observability = &poolObservability{samples: []TrendSample{}, nodes: map[string]runtimeBaseline{}}
	for i := 0; i < 1500; i++ {
		p.runtime.CompletedRequests++
		p.recordObservationLocked(now.Add(time.Duration(i-1499) * time.Minute))
	}
	if len(p.observability.samples) != 1440 {
		t.Fatal("retention is unbounded")
	}
	p.mu.Unlock()
	p.persistObservation(now)
	raw, err := os.ReadFile(filepath.Join(p.cfg.DataDir, observationFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > observationFileLimit || strings.Contains(string(raw), "private-") || strings.Contains(string(raw), "proxy.example") || strings.Contains(string(raw), "proxy_url") {
		t.Fatal("telemetry exposed connection details or exceeded bound")
	}
	var disk observationDisk
	if json.Unmarshal(raw, &disk) != nil || validateObservations(disk) != nil {
		t.Fatal("cannot reload bounded telemetry")
	}
	if disk.Samples[0].At != now.Add(-1439*time.Minute).UnixMilli() {
		t.Fatal("wrong 24h cutoff")
	}
	public, _ := json.Marshal(p.Observability("", 0))
	if len(public) > 1<<20 {
		t.Fatalf("pool summary unexpectedly large: %d", len(public))
	}
	bad := disk
	bad.Samples = append([]TrendSample{}, disk.Samples...)
	bad.Samples[0].Nodes = make([]NodeTrend, maxNodes+1)
	if validateObservations(bad) == nil {
		t.Fatal("unbounded node data accepted")
	}
}

func TestObservationErrorsDoNotStopRoutingOrClose(t *testing.T) {
	p := testPool(t, "https://origin.example.test")
	path := filepath.Join(p.cfg.DataDir, observationFile)
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	p.persistObservation(time.Now())
	if p.Observability("", 0).LastError != "telemetry_persistence_failed" || p.State().Status == "stopped" {
		t.Fatal("telemetry failure changed service availability")
	}
	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("telemetry failure hung Close")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"samples":[{"bad":"value"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := p.cfg
	cfg.ControlToken = testToken
	reopened, err := New(cfg)
	if err != nil {
		t.Fatal("corrupt telemetry prevented startup", err)
	}
	defer reopened.Close()
	if reopened.Observability("", 0).LastError != "telemetry_load_failed" {
		t.Fatal("corrupt telemetry not reported")
	}
}

func TestObservationDecisionExplainsPolicyAndCapacity(t *testing.T) {
	p := testPool(t, "https://origin.example.test")
	other := addNode(t, p, "other", "")
	setPolicy(t, p, "latency", "direct")
	p.mu.Lock()
	now := time.Now()
	slow, fast := 100.0, 90.0
	p.routes["direct"].health = Health{Status: "healthy", LatencyEWMAMS: &slow, CheckedAt: now.UnixMilli()}
	p.routes[other].health = Health{Status: "healthy", LatencyEWMAMS: &fast, CheckedAt: now.UnixMilli()}
	p.active = "direct"
	p.switched = now.Add(-time.Hour)
	p.mu.Unlock()
	if s := p.Observability("", 0); s.Decision.Reason != "improvement_below_threshold" || s.Decision.Explanation == "" {
		t.Fatalf("missing hysteresis reason: %+v", s.Decision)
	}
	p.mu.Lock()
	fast = 10
	p.switched = time.Now()
	p.mu.Unlock()
	if p.Observability("", 0).Decision.Reason != "switch_cooldown" {
		t.Fatal("missing cooldown reason")
	}
	p.mu.Lock()
	p.switched = time.Now().Add(-time.Hour)
	p.mu.Unlock()
	if s := p.Observability("", 0); s.Decision.Reason != "lowest_latency" || s.Decision.ActiveID != other {
		t.Fatal("incorrect lowest latency decision")
	}
	p.mu.Lock()
	p.saved.Mode = "balance"
	p.saved.SelectionPolicy.RegionMode = "only"
	p.saved.SelectionPolicy.PreferredRegions = []string{"CN"}
	p.routes["direct"].config.Region = "US"
	p.routes[other].config.Region = "US"
	p.mu.Unlock()
	s := p.Observability("", 0)
	if s.Decision.Reason != "no_eligible_route" {
		t.Fatal("region exclusion did not explain unavailable route")
	}
	for _, n := range s.Decision.Candidates {
		if n.Eligible || n.Reason != "region_excluded" {
			t.Fatal("incorrect region reason")
		}
	}
}

func TestObservationControlAuthAndQuery(t *testing.T) {
	p := testPool(t, "https://origin.example.test")
	for _, auth := range []bool{false, true} {
		r := httptest.NewRequest(http.MethodGet, "/v1/observability", nil)
		if auth {
			r.Header.Set("Authorization", "Bearer "+testToken)
		}
		w := httptest.NewRecorder()
		p.ControlHandler().ServeHTTP(w, r)
		want := 401
		if auth {
			want = 200
		}
		if w.Code != want {
			t.Fatalf("auth %v status %d", auth, w.Code)
		}
	}
	for _, body := range []map[string]any{{"window_ms": 1}, {"window_ms": 86400001}, {"node_id": "../../secret"}, {"unknown": true}} {
		if w := controlRequest(p, "/v1/observability/query", body); w.Code != 400 {
			t.Fatalf("invalid query accepted: %v", body)
		}
	}
	w := controlRequest(p, "/v1/observability/query", map[string]any{"node_id": "direct", "window_ms": 60000})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var state ObservabilityState
	if json.Unmarshal(w.Body.Bytes(), &state) != nil || len(state.Samples) != 1 || len(state.Samples[0].Nodes) != 1 {
		t.Fatal("missing node projection")
	}
}
