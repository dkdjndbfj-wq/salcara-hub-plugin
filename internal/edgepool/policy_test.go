package edgepool

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func patchPolicy(t *testing.T, p *Pool, fields map[string]any) {
	t.Helper()
	fields["expected_revision"] = p.State().Revision
	w := controlRequest(p, "/v1/policy", fields)
	if w.Code != 200 {
		t.Fatalf("policy patch: %d %s", w.Code, w.Body.String())
	}
}
func tagNode(t *testing.T, p *Pool, id, region string, enabled bool) {
	t.Helper()
	w := controlRequest(p, "/v1/nodes", map[string]any{"expected_revision": p.State().Revision, "node": map[string]any{"id": id, "name": id, "enabled": enabled, "region": region}})
	if w.Code != 200 {
		t.Fatalf("tag node: %d %s", w.Code, w.Body.String())
	}
}
func awaitProbeDone(t *testing.T, p *Pool) State {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		s := p.State()
		if !s.ProbeRunning {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe did not complete: %+v", s.ProbeRun)
		}
		time.Sleep(time.Millisecond)
	}
}
func quietLivePool(t *testing.T, origin string) *Pool {
	t.Helper()
	dir := privateDataDir(t)
	s, err := readSaved(dir)
	if err != nil {
		t.Fatal(err)
	}
	normalizeConfig(&s)
	s.HealthPolicy.Enabled = false
	if _, err = writeSaved(dir, s); err != nil {
		t.Fatal(err)
	}
	p, err := New(Config{Origin: origin, DataDir: dir, ControlToken: testToken})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func TestPolicyMigrationPersistenceAndOptionalMerge(t *testing.T) {
	dir := privateDataDir(t)
	old := `{"revision":7,"mode":"manual","selected_id":"direct","nodes":[{"id":"direct","name":"direct","proxy_url":"","enabled":true}]}`
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := New(Config{Origin: "http://127.0.0.1:1", DataDir: dir, ControlToken: testToken, DisablePeriodicProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if s := p.State(); s.HealthPolicy != defaultHealthPolicy() || !reflect.DeepEqual(s.SelectionPolicy, defaultSelectionPolicy()) {
		t.Fatalf("old configuration did not migrate: %+v", s)
	}
	patchPolicy(t, p, map[string]any{"health_policy": map[string]any{"enabled": false, "interval_ms": 5000, "timeout_ms": 500, "concurrency": 2, "failure_threshold": 2, "recovery_hold_ms": 0, "stale_after_ms": 10000}, "selection_policy": map[string]any{"preferred_regions": []string{" 华东 ", "HK"}, "region_mode": "prefer", "allow_unknown": false, "switch_cooldown_ms": 0}})
	patchPolicy(t, p, map[string]any{"selection_policy": map[string]any{"min_improvement_ms": 5}})
	tagNode(t, p, "direct", " 华东 ", true)
	expected := p.State()
	if expected.Mode != "manual" || expected.HealthPolicy.Concurrency != 2 || expected.HealthPolicy.RecoverySuccesses != 2 || expected.SelectionPolicy.PreferredRegions[0] != "华东" || expected.Nodes[0].Region != "华东" {
		t.Fatalf("optional patch lost saved values: %+v", expected)
	}
	copyState := p.State()
	copyState.SelectionPolicy.PreferredRegions[0] = "external mutation"
	if p.State().SelectionPolicy.PreferredRegions[0] != "华东" {
		t.Fatal("public region slice aliases saved configuration")
	}
	p.Close()
	reopened, err := New(Config{Origin: "http://127.0.0.1:1", DataDir: dir, ControlToken: testToken, DisablePeriodicProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got := reopened.State()
	if got.HealthPolicy != expected.HealthPolicy || !reflect.DeepEqual(got.SelectionPolicy, expected.SelectionPolicy) || got.Nodes[0].Region != "华东" {
		t.Fatal("new policy did not survive restart")
	}
}

func TestPolicyAndRegionValidationPreservesPreviousState(t *testing.T) {
	p := testPool(t, "http://127.0.0.1:1")
	for _, fields := range []map[string]any{
		{"health_policy": map[string]any{"interval_ms": 4999}},
		{"health_policy": map[string]any{"interval_ms": 5000, "timeout_ms": 6000}},
		{"health_policy": map[string]any{"stale_after_ms": 30000}},
		{"health_policy": map[string]any{"concurrency": 17}},
		{"health_policy": map[string]any{"failure_threshold": 0}},
		{"selection_policy": map[string]any{"region_mode": "only"}},
		{"selection_policy": map[string]any{"preferred_regions": []string{"hk", "HK"}}},
		{"selection_policy": map[string]any{"preferred_regions": []string{"\nHK"}}},
		{"selection_policy": map[string]any{"min_improvement_percent": 101}},
	} {
		revision := p.State().Revision
		fields["expected_revision"] = revision
		w := controlRequest(p, "/v1/policy", fields)
		if w.Code != 400 || p.State().Revision != revision {
			t.Fatalf("accepted invalid policy: %v, %d", fields, w.Code)
		}
	}
	for _, region := range []string{"\nHK", "a\x00b", "HK\u202e", strings.Repeat("中", 11)} {
		revision := p.State().Revision
		w := controlRequest(p, "/v1/nodes", map[string]any{"expected_revision": revision, "node": map[string]any{"id": "direct", "name": "direct", "enabled": true, "region": region}})
		if w.Code != 400 || p.State().Revision != revision {
			t.Fatalf("accepted invalid region %q", region)
		}
	}
}

func TestRegionPreferenceStrictModeAndManualSelection(t *testing.T) {
	p := testPool(t, "http://127.0.0.1:1")
	cn := addNode(t, p, "CN", "")
	hk := addNode(t, p, "HK", "")
	tagNode(t, p, cn, "华东", true)
	tagNode(t, p, hk, "HK", true)
	patchPolicy(t, p, map[string]any{"mode": "balance", "selection_policy": map[string]any{"region_mode": "only", "preferred_regions": []string{"华东"}}})
	lease, err := p.acquire(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if lease.route.config.ID != cn {
		t.Fatal("strict region escaped its candidate set")
	}
	lease.finish("completed", false)
	patchPolicy(t, p, map[string]any{"selection_policy": map[string]any{"preferred_regions": []string{"missing"}}})
	if _, err := p.acquire(context.Background(), false); err == nil || err.(*admissionError).code != "no_available_route" {
		t.Fatalf("strict empty region must refuse traffic: %v", err)
	}
	patchPolicy(t, p, map[string]any{"mode": "manual", "selected_id": "direct"})
	lease, err = p.acquire(context.Background(), false)
	if err != nil || lease.route.config.ID != "direct" {
		t.Fatal("geography overrode explicit manual selection")
	}
	lease.finish("completed", false)
	patchPolicy(t, p, map[string]any{"mode": "balance", "selection_policy": map[string]any{"region_mode": "prefer", "preferred_regions": []string{"hk"}}})
	preferred, err := p.acquire(context.Background(), false)
	if err != nil || preferred.route.config.ID != hk {
		t.Fatal("case-insensitive preferred region was not selected")
	}
	// An occupied unknown-node trial cannot force balance to queue while another
	// allowed region has capacity; strict mode still never escapes its region.
	fallback, err := p.acquire(context.Background(), false)
	if err != nil || fallback.route.config.ID == hk {
		t.Fatal("saturated preferred region prevented allowed fallback")
	}
	fallback.finish("completed", false)
	preferred.finish("completed", false)
	tagNode(t, p, hk, "HK", false)
	lease, err = p.acquire(context.Background(), false)
	if err != nil || lease.route.config.ID == hk {
		t.Fatal("disabled preferred region did not fall back")
	}
	lease.finish("completed", false)
}

func TestProbeAllIncludesDisabledWithoutEnablingAndDeduplicates(t *testing.T) {
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
			io.WriteString(w, `{"status":"available"}`)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(origin.Close)
	p := testPool(t, origin.URL)
	id := addNode(t, p, "disabled", "")
	tagNode(t, p, id, "HK", false)
	if started, err := p.RequestProbe("all", nil); err != nil || !started {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("all-node probe missed a disabled node")
		}
	}
	run := p.State().ProbeRun
	if run.Total != 2 || run.Completed != 0 || !run.Running {
		t.Fatalf("bad active progress: %+v", run)
	}
	if started, err := p.RequestProbe("all", nil); err != nil || started || p.State().ProbeRun.ID != run.ID || p.State().ProbeRun.Pending {
		t.Fatal("duplicate request was not coalesced")
	}
	releaseOnce.Do(func() { close(release) })
	s := awaitProbeDone(t, p)
	if s.ProbeRun.Completed != 2 || s.ProbeRun.Succeeded != 2 || s.ProbeRun.Failed != 0 || s.ProbeRun.FinishedAt < s.ProbeRun.StartedAt || s.Nodes[1].Enabled {
		t.Fatalf("bad final diagnostic: %+v", s)
	}
	if p.saved.Nodes[1].Enabled {
		t.Fatal("diagnostic enabled a disabled route")
	}
	if _, err := p.RequestProbe("enabled", nil); err != nil {
		t.Fatal(err)
	}
	if got := awaitProbeDone(t, p); got.ProbeRun.Total != 1 {
		t.Fatal("enabled scope included a disabled node")
	}
	if _, err := p.RequestProbe("all", []string{"missing"}); err == nil {
		t.Fatal("unknown diagnostic target accepted")
	}
}

func TestConfigurationChangedDuringProbeGetsPendingPass(t *testing.T) {
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	var releaseOnce sync.Once
	var hits atomic.Int64
	defer releaseOnce.Do(func() { close(release) })
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
			io.WriteString(w, `{"status":"available"}`)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(origin.Close)
	p := quietLivePool(t, origin.URL)
	p.Probe()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("initial probe not started")
	}
	id := addNode(t, p, "new while probing", "")
	if !p.State().ProbeRun.Pending {
		t.Fatal("new route was lost behind active probe")
	}
	releaseOnce.Do(func() { close(release) })
	s := awaitProbeDone(t, p)
	if hits.Load() != 2 || s.Nodes[1].ID != id || s.Nodes[1].Health.CheckedAt == 0 || s.ProbeRun.Trigger != "configuration" || s.ProbeRun.Pending {
		t.Fatalf("missing bounded follow-up: hits=%d run=%+v", hits.Load(), s.ProbeRun)
	}
	if s.NextProbeAt != 0 {
		t.Fatal("disabled periodic policy scheduled another pass")
	}
}

func TestProbeRecoveryRequiresConsecutiveSuccessesAndFreshEWMA(t *testing.T) {
	var fail atomic.Bool
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(503)
			return
		}
		io.WriteString(w, `{"status":"available"}`)
	}))
	defer origin.Close()
	p := testPool(t, origin.URL)
	patchPolicy(t, p, map[string]any{"health_policy": map[string]any{"failure_threshold": 1, "recovery_successes": 2, "recovery_hold_ms": 0}})
	waitProbe(t, p)
	first := *p.State().Nodes[0].Health.LatencyEWMAMS
	waitProbe(t, p)
	h := p.State().Nodes[0].Health
	if math.Abs(*h.LatencyEWMAMS-(.3**h.LatencyMS+.7*first)) > 0.000001 {
		t.Fatal("EWMA does not reflect successful samples")
	}
	fail.Store(true)
	waitProbe(t, p)
	fail.Store(false)
	waitProbe(t, p)
	if n := p.State().Nodes[0]; !n.Recovering || n.Health.ConsecutiveSuccesses != 1 {
		t.Fatal("first recovery sample restored full capacity")
	}
	fail.Store(true)
	waitProbe(t, p)
	if p.State().Nodes[0].Health.ConsecutiveSuccesses != 0 {
		t.Fatal("failed recovery did not reset success streak")
	}
	fail.Store(false)
	waitProbe(t, p)
	if !p.State().Nodes[0].Recovering {
		t.Fatal("nonconsecutive successes completed recovery")
	}
	waitProbe(t, p)
	if p.State().Nodes[0].Recovering {
		t.Fatal("consecutive successes did not complete recovery")
	}
	patchPolicy(t, p, map[string]any{"mode": "latency", "selection_policy": map[string]any{"allow_unknown": false}})
	p.mu.Lock()
	p.routes["direct"].health.CheckedAt = time.Now().Add(-3 * time.Minute).UnixMilli()
	p.mu.Unlock()
	s := p.State()
	if !s.Nodes[0].Health.Stale || s.Nodes[0].Health.SampleAgeMS == nil || s.ActiveID != "" {
		t.Fatal("expired measurement remained an eligible automatic route")
	}
	waitProbe(t, p)
	h = p.State().Nodes[0].Health
	if h.Stale || *h.LatencyMS != *h.LatencyEWMAMS {
		t.Fatal("fresh recovery retained stale EWMA")
	}
}

func TestProbeCompletionTimestampAndCloseCancellation(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		entered <- struct{}{}
		select {
		case <-release:
			io.WriteString(w, `{"status":"available"}`)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(origin.Close)
	p := testPool(t, origin.URL)
	p.Probe()
	<-entered
	releasedAt := time.Now().Add(30 * time.Millisecond)
	time.Sleep(time.Until(releasedAt))
	once.Do(func() { close(release) })
	s := awaitProbeDone(t, p)
	if s.Nodes[0].Health.CheckedAt < releasedAt.UnixMilli() {
		t.Fatal("timestamp reflects headers rather than completed status body")
	}
	blocking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { entered <- struct{}{}; <-r.Context().Done() }))
	t.Cleanup(blocking.Close)
	p2 := testPool(t, blocking.URL)
	p2.Probe()
	<-entered
	closed := make(chan struct{})
	go func() { p2.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel active probes")
	}
	if state := p2.State(); state.ProbeRunning || state.NextProbeAt != 0 || state.ProbeRun.Running || state.ProbeRun.Completed != state.ProbeRun.Total {
		t.Fatalf("closed progress not settled: %+v", state.ProbeRun)
	}
}

func TestProbePolicyHotUpdateSchedulesAndStopsPeriodicWork(t *testing.T) {
	var hits atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); io.WriteString(w, `{"status":"available"}`) }))
	defer origin.Close()
	p := quietLivePool(t, origin.URL)
	patchPolicy(t, p, map[string]any{"health_policy": map[string]any{"enabled": true, "interval_ms": 5000, "timeout_ms": 100, "stale_after_ms": 6000}})
	scheduled := p.State().NextProbeAt
	if scheduled < time.Now().Add(4*time.Second).UnixMilli() || scheduled > time.Now().Add(6*time.Second).UnixMilli() {
		t.Fatalf("hot interval not applied: %d", scheduled)
	}
	deadline := time.Now().Add(7 * time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if hits.Load() != 1 {
		t.Fatal("updated periodic scheduler did not run")
	}
	s := awaitProbeDone(t, p)
	if s.ProbeRun.Trigger != "automatic" || s.NextProbeAt < s.ProbeRun.FinishedAt+5000 {
		t.Fatal("next batch was not based on completion")
	}
	patchPolicy(t, p, map[string]any{"health_policy": map[string]any{"enabled": false}})
	if p.State().NextProbeAt != 0 {
		t.Fatal("disabling periodic probes left a timer scheduled")
	}
	p.Probe()
	awaitProbeDone(t, p)
	if hits.Load() != 2 {
		t.Fatal("manual probe was disabled with the periodic timer")
	}
	data, err := json.Marshal(p.State())
	if err != nil || !strings.Contains(string(data), `"probe_run"`) {
		t.Fatal("probe progress missing from JSON")
	}
}

func TestPausedPeriodicPolicyStillExpiresMeasurementsAndLimitsTrials(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"status":"available"}`) }))
	defer origin.Close()
	p := quietLivePool(t, origin.URL)
	p.Probe()
	awaitProbeDone(t, p)
	patchPolicy(t, p, map[string]any{"mode": "balance", "selection_policy": map[string]any{"allow_unknown": false}})
	p.mu.Lock()
	p.routes["direct"].health.CheckedAt = time.Now().Add(-3 * time.Minute).UnixMilli()
	p.mu.Unlock()
	if s := p.State(); s.NextProbeAt != 0 || !s.Nodes[0].Health.Stale || s.Status != "unavailable" {
		t.Fatalf("paused probe retained a fresh route: %+v", s)
	}
	if _, err := p.acquire(context.Background(), false); err == nil {
		t.Fatal("stale route admitted with allow_unknown=false")
	}
	patchPolicy(t, p, map[string]any{"limits": map[string]any{"queue_limit": 0}, "selection_policy": map[string]any{"allow_unknown": true}})
	trial, err := p.acquire(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.acquire(context.Background(), true); err == nil {
		t.Fatal("stale route admitted parallel request and stream trials")
	}
	trial.finish("completed", false)
	p.Probe()
	awaitProbeDone(t, p)
	if p.State().Nodes[0].Health.Stale {
		t.Fatal("manual probe did not refresh a paused pool")
	}
}

func TestProbeConcurrencyAndProgressAreBounded(t *testing.T) {
	var active, maxActive, total atomic.Int64
	entered := make(chan struct{}, 16)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		total.Add(1)
		for {
			old := maxActive.Load()
			if current <= old || maxActive.CompareAndSwap(old, current) {
				break
			}
		}
		entered <- struct{}{}
		select {
		case <-release:
			io.WriteString(w, `{"status":"available"}`)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(origin.Close)
	p := testPool(t, origin.URL)
	for range 5 {
		addNode(t, p, "direct fixture", "")
	}
	patchPolicy(t, p, map[string]any{"health_policy": map[string]any{"concurrency": 2}})
	p.Probe()
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("probe workers did not start")
		}
	}
	if got := p.State().ProbeRun; got.Total != 6 || got.Completed != 0 || active.Load() != 2 {
		t.Fatalf("wrong active batch: %+v", got)
	}
	once.Do(func() { close(release) })
	s := awaitProbeDone(t, p)
	if total.Load() != 6 || maxActive.Load() > 2 || s.ProbeRun.Completed != 6 || s.ProbeRun.Succeeded != 6 {
		t.Fatalf("unbounded or incomplete batch: max=%d total=%d progress=%+v", maxActive.Load(), total.Load(), s.ProbeRun)
	}
}
