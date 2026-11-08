package edgepool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const capacitySourceA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const capacitySourceB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func capacityGroup(t *testing.T, p *Pool, id string) SourceGroupView {
	t.Helper()
	for _, g := range p.State().SourceGroups {
		if g.SourceID == id {
			return g
		}
	}
	t.Fatalf("missing source group %s", id)
	return SourceGroupView{}
}
func capacityHealthy(p *Pool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.routes {
		r.health.Status = "healthy"
		r.health.CheckedAt = time.Now().UnixMilli()
		r.recovering = false
	}
}
func capacitySetup(t *testing.T, p *Pool, requests, streams int) {
	t.Helper()
	configureScheduling(t, p, "balance", Limits{MaxRequests: 8, MaxStreams: 8, QueueLimit: 0, QueueTimeoutMS: 200}, func(s *savedConfig) {
		s.Nodes = []nodeConfig{{ID: "a1", Name: "A1", Enabled: true, MaxRequests: 6, MaxStreams: 6, SourceID: capacitySourceA}, {ID: "a2", Name: "A2", Enabled: true, MaxRequests: 6, MaxStreams: 6, SourceID: capacitySourceA}}
		s.SelectedID = "a1"
		s.SourceLimits = []SourceLimit{{SourceID: capacitySourceA, MaxRequests: requests, MaxStreams: streams}}
	})
	capacityHealthy(p)
}
func capacityAcquire(t *testing.T, p *Pool, stream bool) *requestLease {
	t.Helper()
	l, err := p.acquire(context.Background(), stream)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.finish("cancelled", false) })
	return l
}
func capacityChange(t *testing.T, p *Pool, change func(*savedConfig)) {
	t.Helper()
	revision := p.State().Revision
	_, status, code := p.commitConfig(&revision, func(s *savedConfig) string { change(s); return "" })
	if status != 200 {
		t.Fatal(status, code)
	}
}

func TestSourceCapacityRealConcurrentAdmissionEnforcesAllThreeLevels(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/salcara-hub/statusz" {
			io.WriteString(w, `{"status":"available"}`)
			return
		}
		select {
		case <-release:
			io.WriteString(w, "ok")
		case <-r.Context().Done():
		}
	}))
	registerTestOrigin(t, origin)
	p := testPool(t, origin.URL)
	var countA, countB atomic.Int32
	proxyA := testHTTPProxy(t, origin.URL, &countA)
	proxyB := testHTTPProxy(t, origin.URL, &countB)
	a := addNode(t, p, "A1", proxyA.URL)
	a2 := addNode(t, p, "A2", proxyA.URL)
	b := addNode(t, p, "B", proxyB.URL)
	configureScheduling(t, p, "balance", Limits{MaxRequests: 3, MaxStreams: 4, QueueLimit: 0, QueueTimeoutMS: 200}, func(s *savedConfig) {
		s.SelectedID = a
		s.SourceLimits = []SourceLimit{{capacitySourceA, 2, 2}, {capacitySourceB, 1, 2}}
		for i := range s.Nodes {
			n := &s.Nodes[i]
			n.Enabled = n.ID != "direct"
			n.MaxRequests = 2
			n.MaxStreams = 2
			if n.ID == a || n.ID == a2 {
				n.SourceID = capacitySourceA
			}
			if n.ID == b {
				n.SourceID = capacitySourceB
			}
		}
	})
	waitProbe(t, p)
	if capacityGroup(t, p, capacitySourceA).Runtime.CompletedRequests != 0 {
		t.Fatal("health probes consumed business quota")
	}
	edge := httptest.NewServer(p.Handler())
	t.Cleanup(edge.Close)
	results := make(chan int, 20)
	for i := 0; i < 20; i++ {
		go func() {
			r, err := http.Post(edge.URL+"/salcara-hub/v1/app/commands", "application/json", strings.NewReader(`{}`))
			if err != nil {
				results <- 0
				return
			}
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
			results <- r.StatusCode
		}()
	}
	s := awaitRuntime(t, p, func(s State) bool { return s.Runtime.ActiveRequests == 3 })
	if capacityGroup(t, p, capacitySourceA).Runtime.ActiveRequests != 2 || capacityGroup(t, p, capacitySourceB).Runtime.ActiveRequests != 1 {
		t.Fatal("source quota exceeded", s.SourceGroups)
	}
	for _, n := range s.Nodes {
		if n.Runtime.ActiveRequests > 2 {
			t.Fatal("node quota exceeded")
		}
	}
	for i := 0; i < 17; i++ {
		select {
		case status := <-results:
			if status != 429 {
				t.Fatal("overload accepted", status)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("admission stalled")
		}
	}
	once.Do(func() { close(release) })
	for i := 0; i < 3; i++ {
		if status := <-results; status != 200 {
			t.Fatal(status)
		}
	}
	awaitRuntime(t, p, func(s State) bool { return s.Runtime.ActiveRequests == 0 })
	if capacityGroup(t, p, capacitySourceA).Runtime.CompletedRequests != 2 || capacityGroup(t, p, capacitySourceB).Runtime.CompletedRequests != 1 {
		t.Fatal("source completion accounting")
	}
}

func TestSourceCapacitySeparateKindsNoLimitAndDecisionReasons(t *testing.T) {
	p := testPool(t, "https://hub.example.com")
	capacitySetup(t, p, 2, 1)
	a := capacityAcquire(t, p, false)
	b := capacityAcquire(t, p, false)
	d := p.Observability("", time.Hour).Decision
	for _, c := range d.Candidates {
		if c.SourceRequestCapacity || !c.SourceStreamCapacity || c.Reason != "source_request_capacity_full" {
			t.Fatal("missing shared request reason", c)
		}
	}
	stream := capacityAcquire(t, p, true)
	for _, kind := range []bool{false, true} {
		if _, err := p.acquire(context.Background(), kind); err == nil {
			t.Fatal("shared quota exceeded")
		}
	}
	g := capacityGroup(t, p, capacitySourceA)
	if g.Runtime.ActiveRequests != 2 || g.Runtime.ActiveStreams != 1 || g.RequestAvailable || g.StreamAvailable {
		t.Fatal(g)
	}
	stream.finish("completed", false)
	a.finish("completed", false)
	b.finish("completed", false)
	capacityChange(t, p, func(s *savedConfig) { s.SourceLimits = nil })
	for i := 0; i < 5; i++ {
		capacityAcquire(t, p, false)
	}
	g = capacityGroup(t, p, capacitySourceA)
	if g.Limited || g.MaxRequests != 0 || g.MaxStreams != 0 || g.Runtime.ActiveRequests != 5 {
		t.Fatal("absent limits changed old behavior", g)
	}
}

func TestSourceCapacityCapturesLedgerAcrossMoveReplacementAndDelete(t *testing.T) {
	p := testPool(t, "https://hub.example.com")
	capacitySetup(t, p, 1, 1)
	capacityChange(t, p, func(s *savedConfig) {
		s.Mode = "manual"
		s.Nodes = s.Nodes[:1]
		s.SourceLimits = append(s.SourceLimits, SourceLimit{capacitySourceB, 1, 1})
	})
	old := capacityAcquire(t, p, true)
	oldLedger := old.source
	capacityChange(t, p, func(s *savedConfig) { s.Nodes[0].SourceID = capacitySourceB })
	current := capacityAcquire(t, p, true)
	if old.source != oldLedger || old.source == current.source || capacityGroup(t, p, capacitySourceA).Runtime.ActiveStreams != 1 || capacityGroup(t, p, capacitySourceB).Runtime.ActiveStreams != 1 {
		t.Fatal("moving route reassigned old lease")
	}
	capacityChange(t, p, func(s *savedConfig) { s.Nodes[0].ProxyURL = "http://user:changed@proxy.example:8080" })
	capacityHealthy(p)
	if _, err := p.acquire(context.Background(), true); err == nil {
		t.Fatal("credential generation reset group occupancy")
	}
	capacityChange(t, p, func(s *savedConfig) { s.Nodes = nil; s.SelectedID = ""; s.SourceLimits = nil })
	for _, id := range []string{capacitySourceA, capacitySourceB} {
		g := capacityGroup(t, p, id)
		if !g.Retired || g.Runtime.ActiveStreams != 1 {
			t.Fatal("deleted group lost draining stream", g)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); old.finish("completed", false); current.finish("completed", false) }()
	}
	wg.Wait()
	if len(p.State().SourceGroups) != 0 || p.State().Runtime.ActiveStreams != 0 || oldLedger.ActiveStreams != 0 {
		t.Fatal("retired ledgers leaked or released twice")
	}
	p.mu.Lock()
	retained := len(p.sourceRuntime)
	p.mu.Unlock()
	if retained != 0 {
		t.Fatal("idle historic ledgers were not reclaimed")
	}
}

func TestSourceCapacityDownsizeQueuesWithoutKickingExistingWork(t *testing.T) {
	p := testPool(t, "https://hub.example.com")
	capacitySetup(t, p, 3, 2)
	leases := []*requestLease{capacityAcquire(t, p, false), capacityAcquire(t, p, false), capacityAcquire(t, p, false)}
	capacityChange(t, p, func(s *savedConfig) {
		s.SourceLimits[0].MaxRequests = 1
		s.Limits.QueueLimit = 2
		s.Limits.QueueTimeoutMS = 2000
	})
	if capacityGroup(t, p, capacitySourceA).Runtime.ActiveRequests != 3 {
		t.Fatal("downsize discarded live requests")
	}
	queued := make(chan *requestLease, 1)
	go func() { l, _ := p.acquire(context.Background(), false); queued <- l }()
	awaitRuntime(t, p, func(s State) bool { return s.Runtime.QueuedRequests == 1 })
	leases[0].finish("completed", false)
	leases[1].finish("completed", false)
	select {
	case <-queued:
		t.Fatal("queue admitted while aggregate remained full")
	case <-time.After(20 * time.Millisecond):
	}
	leases[2].finish("completed", false)
	var resumed *requestLease
	select {
	case resumed = <-queued:
		if resumed == nil {
			t.Fatal("queued request rejected")
		}
	case <-time.After(time.Second):
		t.Fatal("queue did not resume")
	}
	defer resumed.finish("cancelled", false)
	ctx, cancel := context.WithCancel(context.Background())
	cancelled := make(chan error, 1)
	go func() { _, err := p.acquire(ctx, false); cancelled <- err }()
	awaitRuntime(t, p, func(s State) bool { return s.Runtime.QueuedRequests == 1 })
	cancel()
	if !errors.Is(<-cancelled, context.Canceled) {
		t.Fatal("cancel failed")
	}
	g := capacityGroup(t, p, capacitySourceA)
	if g.Runtime.ActiveRequests != 1 || g.Runtime.QueuedRequests != 0 || g.Runtime.CancelledRequests != 0 {
		t.Fatal("unassigned waiter charged to source", g)
	}
}

func TestSourceCapacityReleaseRollbackPersistenceAndValidation(t *testing.T) {
	p := testPool(t, healthyOrigin(t).URL)
	id := addSourceFixture(t, p)
	capacityChange(t, p, func(s *savedConfig) { s.Nodes[0].SourceID = id; s.Nodes[0].MaxRequests = 5; s.Nodes[0].MaxStreams = 5 })
	capacityHealthy(p)
	a := capacityAcquire(t, p, false)
	b := capacityAcquire(t, p, false)
	ledger := a.source
	limit := []SourceLimit{{id, 1, 2}}
	w := controlRequest(p, "/v1/policy", map[string]any{"expected_revision": p.State().Revision, "source_limits": limit})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "source_limits_require_release") {
		t.Fatal("unsafe direct quota change", w.Code, w.Body.String())
	}
	preview := prepareReleaseTest(t, p, "policy", map[string]any{"source_limits": limit})
	if preview.Status != "ready" {
		t.Fatal(preview)
	}
	s := startReleaseTest(t, p, preview)
	if len(s.SourceLimits) != 1 || capacityGroup(t, p, id).Runtime.ActiveRequests != 2 {
		t.Fatal("trial lost live source accounting")
	}
	_, status, code := p.rollbackRelease(&s.Revision, s.Release.Trial.ID, "")
	if status != 200 {
		t.Fatal(code)
	}
	if len(p.State().SourceLimits) != 0 || capacityGroup(t, p, id).Runtime.ActiveRequests != 2 {
		t.Fatal("rollback reset ledger")
	}
	p.mu.Lock()
	sameLedger := p.routes["direct"].source == ledger
	p.mu.Unlock()
	if !sameLedger {
		t.Fatal("rollback replaced source ledger")
	}
	a.finish("completed", false)
	b.finish("completed", false)
	preview = prepareReleaseTest(t, p, "policy", map[string]any{"source_limits": limit})
	s = startReleaseTest(t, p, preview)
	s, status, code = p.confirmRelease(&s.Revision, s.Release.Trial.ID)
	if status != 200 {
		t.Fatal(code)
	}
	dir, origin := p.cfg.DataDir, p.origin.String()
	p.Close()
	reloaded, err := New(Config{Origin: origin, DataDir: dir, ControlToken: testToken, DisablePeriodicProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	if len(reloaded.State().SourceLimits) != 1 || reloaded.State().SourceLimits[0].MaxRequests != 1 {
		t.Fatal("confirmed quota not persisted")
	}
	for _, bad := range []any{[]SourceLimit{{id, 0, 2}}, []SourceLimit{{"wrong", 1, 1}}, []SourceLimit{{id, 1, 1}, {id, 2, 2}}} {
		w = controlRequest(reloaded, "/v1/releases/prepare", map[string]any{"expected_revision": reloaded.State().Revision, "action": "policy", "payload": map[string]any{"source_limits": bad}})
		if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_source_limits") {
			t.Fatal("invalid limit accepted", w.Code, w.Body.String())
		}
	}
	tooMany := make([]SourceLimit, 33)
	for i := range tooMany {
		tooMany[i] = SourceLimit{fmt.Sprintf("%032x", i), 1, 1}
	}
	if validateSourceLimits(tooMany) == nil {
		t.Fatal("unbounded limits")
	}
}

func TestSourceCapacityDeadlineAndRestartRollback(t *testing.T) {
	p := testPool(t, healthyOrigin(t).URL)
	id := addSourceFixture(t, p)
	capacityChange(t, p, func(s *savedConfig) {
		s.Nodes[0].SourceID = id
		s.Nodes[0].MaxRequests = 5
		s.SourceLimits = []SourceLimit{{id, 3, 2}}
	})
	capacityHealthy(p)
	first := capacityAcquire(t, p, false)
	second := capacityAcquire(t, p, false)
	preview := prepareReleaseTest(t, p, "policy", map[string]any{"source_limits": []SourceLimit{{id, 1, 2}}})
	startReleaseTest(t, p, preview)
	p.mu.Lock()
	p.trialDeadline = time.Now().Add(-time.Second)
	p.mu.Unlock()
	if !p.expireRelease() {
		t.Fatal("deadline rollback failed")
	}
	if capacityGroup(t, p, id).MaxRequests != 3 || capacityGroup(t, p, id).Runtime.ActiveRequests != 2 {
		t.Fatal("deadline reset live ledger")
	}
	third := capacityAcquire(t, p, false)
	if third.source != first.source {
		t.Fatal("rollback changed source pointer")
	}
	first.finish("completed", false)
	second.finish("completed", false)
	third.finish("completed", false)
	preview = prepareReleaseTest(t, p, "policy", map[string]any{"source_limits": []SourceLimit{{id, 1, 2}}})
	startReleaseTest(t, p, preview)
	dir, origin := p.cfg.DataDir, p.origin.String()
	p.Close()
	reloaded, err := New(Config{Origin: origin, DataDir: dir, ControlToken: testToken, DisablePeriodicProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	if reloaded.State().Release.Trial != nil || capacityGroup(t, reloaded, id).MaxRequests != 3 || capacityGroup(t, reloaded, id).Runtime.ActiveRequests != 0 {
		t.Fatal("restart did not restore previous quota")
	}
}

func TestSourceCapacitySnapshotIsolationAndRemovalReferences(t *testing.T) {
	s := savedConfig{Revision: 1, SourceLimits: []SourceLimit{{capacitySourceA, 3, 4}}}
	for _, copy := range []savedConfig{cloneConfig(s), snapshotOf(s).config(2)} {
		copy.SourceLimits[0].MaxRequests = 1
		if s.SourceLimits[0].MaxRequests != 3 {
			t.Fatal("quota snapshot aliased")
		}
	}
	for _, state := range []savedConfig{s, {Trial: &pendingRelease{Previous: poolSnapshot{SourceLimits: s.SourceLimits}}}, {History: []releaseVersion{{Snapshot: poolSnapshot{SourceLimits: s.SourceLimits}}}}} {
		if !sourceReferenced(state, capacitySourceA) {
			t.Fatal("source quota reference omitted")
		}
	}
	p := testPool(t, "https://hub.example.com")
	id := addSourceFixture(t, p)
	capacityChange(t, p, func(s *savedConfig) { s.SourceLimits = []SourceLimit{{id, 3, 4}} })
	if err := p.sources.remove(id, p.sources.snapshot().SourceRevision); err == nil || err.Error() != "source_in_use" {
		t.Fatal("configured source was deleted", err)
	}
	capacityChange(t, p, func(s *savedConfig) { s.SourceLimits = nil })
	if err := p.sources.remove(id, p.sources.snapshot().SourceRevision); err == nil || err.Error() != "source_in_use" {
		t.Fatal("historic source was deleted", err)
	}
	for i := 0; i < releaseHistoryLimit; i++ {
		capacityChange(t, p, func(s *savedConfig) {})
	}
	if err := p.sources.remove(id, p.sources.snapshot().SourceRevision); err != nil {
		t.Fatal("expired reference blocked deletion", err)
	}
}

func TestSourceCapacityRetiredLeaseBlocksSourceRemovalAfterHistoryExpires(t *testing.T) {
	p := testPool(t, "https://hub.example.com")
	id := addSourceFixture(t, p)
	capacityChange(t, p, func(s *savedConfig) { s.Nodes[0].SourceID = id })
	capacityHealthy(p)
	lease := capacityAcquire(t, p, true)
	capacityChange(t, p, func(s *savedConfig) { s.Nodes = nil; s.SelectedID = "" })
	for i := 0; i < releaseHistoryLimit; i++ {
		capacityChange(t, p, func(s *savedConfig) {})
	}
	p.mu.Lock()
	historic := sourceReferenced(p.saved, id)
	p.mu.Unlock()
	if historic {
		t.Fatal("fixture did not expire history references")
	}
	if err := p.sources.remove(id, p.sources.snapshot().SourceRevision); err == nil || err.Error() != "source_in_use" {
		t.Fatal("retired live source was deleted", err)
	}
	g := capacityGroup(t, p, id)
	if !g.Retired || g.Runtime.ActiveStreams != 1 {
		t.Fatal("retired connection missing", g)
	}
	lease.finish("completed", false)
	if err := p.sources.remove(id, p.sources.snapshot().SourceRevision); err != nil {
		t.Fatal("drained source still blocked", err)
	}
	if len(p.State().SourceGroups) != 0 {
		t.Fatal("drained source group retained")
	}
}

func TestSourceCapacityRefreshKeepsAggregateAcrossCredentials(t *testing.T) {
	p := testPool(t, "https://hub.example.com")
	m := p.sources
	m.probe = func(context.Context, *Pool, []nodeConfig) error { return nil }
	id := addSourceFixture(t, p)
	apply := func(password string) {
		t.Helper()
		candidateFixture(t, m, id, "socks5://alice:"+password+"@proxy.example:1080#Node")
		s := m.snapshot()
		if err := m.apply(id, s.Sources[0].CandidateID, s.SourceRevision, p.State().Revision, []string{s.Sources[0].Nodes[0].Key}, false); err != nil {
			t.Fatal(err)
		}
		if waitSourceIdle(t, m).Sources[0].Status != "applied" {
			t.Fatal("source update failed")
		}
	}
	apply("old")
	capacityChange(t, p, func(s *savedConfig) {
		for i := range s.Nodes {
			if s.Nodes[i].SourceID == id {
				s.SelectedID = s.Nodes[i].ID
			} else {
				s.Nodes[i].Enabled = false
			}
		}
		s.Mode = "manual"
		s.SourceLimits = []SourceLimit{{id, 1, 1}}
	})
	capacityHealthy(p)
	lease := capacityAcquire(t, p, true)
	ledger := lease.source
	apply("new")
	capacityHealthy(p)
	if len(p.State().SourceLimits) != 1 || capacityGroup(t, p, id).Runtime.ActiveStreams != 1 {
		t.Fatal("source refresh reset quota")
	}
	if _, err := p.acquire(context.Background(), true); err == nil {
		t.Fatal("new credentials bypassed shared stream cap")
	}
	lease.finish("completed", false)
	next := capacityAcquire(t, p, true)
	if next.source != ledger {
		t.Fatal("source identity did not preserve ledger")
	}
}

func TestSourceCapacityPersistenceFailureRetainsOldQuota(t *testing.T) {
	p := testPool(t, releaseOrigin(t).URL)
	id := addSourceFixture(t, p)
	preview := prepareReleaseTest(t, p, "policy", map[string]any{"source_limits": []SourceLimit{{id, 2, 2}}})
	path := filepath.Join(p.cfg.DataDir, stateFile)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	defer func() { os.Remove(path); os.WriteFile(path, original, 0600) }()
	rev := p.State().Revision
	_, status, _ := p.startRelease(&rev, preview.ID)
	if status != 500 || len(p.State().SourceLimits) != 0 || p.State().Revision != rev {
		t.Fatal("failed write changed quota")
	}
	state := p.State()
	raw, _ := json.Marshal(state)
	if strings.Contains(string(raw), "source-private-secret") {
		t.Fatal("source quota view leaked secrets")
	}
}
