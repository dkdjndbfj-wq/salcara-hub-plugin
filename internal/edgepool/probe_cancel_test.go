package edgepool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitProbeFinished(t *testing.T, p *Pool) ProbeRun {
	t.Helper()
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		if s := p.State(); s.ProbeRun != nil && !s.ProbeRun.Running {
			return *s.ProbeRun
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("probe did not complete")
	return ProbeRun{}
}

func TestProbeCancellationIsTaskBoundAndPreservesHealth(t *testing.T) {
	entered := make(chan struct{}, 8)
	var block atomic.Bool
	block.Store(true)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		if block.Load() {
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte(`{"status":"available"}`))
	}))
	defer origin.Close()
	p := testPool(t, origin.URL)
	p.mu.Lock()
	latency := 7.0
	before := Health{Status: "healthy", LatencyMS: &latency, CheckedAt: time.Now().UnixMilli(), LastProbeAt: time.Now().UnixMilli()}
	p.routes["direct"].health = before
	p.mu.Unlock()
	if _, err := p.RequestProbe("enabled", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("probe never started")
	}
	id := p.State().ProbeRun.ID
	w := controlRequest(p, "/v1/probe/cancel", map[string]any{"run_id": "old-task"})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "probe_run_not_found") {
		t.Fatal("stale task cancellation accepted")
	}
	p.mu.Lock()
	p.cfg.DisablePeriodicProbe = false
	p.mu.Unlock()
	w = controlRequest(p, "/v1/probe/cancel", map[string]any{"run_id": id})
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	run := waitProbeFinished(t, p)
	if !run.Cancelled || run.Failed != 0 || run.Skipped != 1 || run.Completed != run.Total {
		t.Fatalf("cancellation progress: %+v", run)
	}
	s := p.State()
	if s.Nodes[0].Health.Status != before.Status || s.Nodes[0].Health.LastProbeAt != before.LastProbeAt || s.Nodes[0].Health.ConsecutiveFailures != 0 || s.NextProbeAt == 0 {
		t.Fatal("cancellation changed health or disabled periodic scheduling")
	}
	block.Store(false)
	if _, err := p.RequestProbe("enabled", nil); err != nil {
		t.Fatal(err)
	}
	if err := p.CancelProbe(id); err == nil {
		t.Fatal("old task ID cancelled the new task")
	}
	run = waitProbeFinished(t, p)
	if run.Cancelled || run.Succeeded != 1 || run.Failed != 0 {
		t.Fatalf("subsequent probe broken: %+v", run)
	}
}

func TestSharedProbeBudgetAcrossNormalReleaseAndSource(t *testing.T) {
	gate := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(gate) })
	entered := make(chan struct{}, 32)
	var active, maximum atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); current > old && !maximum.CompareAndSwap(old, current); old = maximum.Load() {
		}
		entered <- struct{}{}
		select {
		case <-gate:
			_, _ = w.Write([]byte(`{"status":"available"}`))
		case <-r.Context().Done():
		}
	}))
	defer origin.Close()
	p := testPool(t, origin.URL)
	for i := 0; i < 3; i++ {
		addNode(t, p, "direct", "")
	}
	if _, err := p.RequestProbe("all", nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("normal probe did not fill budget")
		}
	}
	w := controlRequest(p, "/v1/releases/prepare", map[string]any{"expected_revision": p.State().Revision, "action": "policy", "payload": map[string]any{"mode": "balance"}})
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	p.mu.Lock()
	nodes := append([]nodeConfig{}, p.saved.Nodes...)
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()
	sourceDone := make(chan error, 1)
	go func() { sourceDone <- probeSourceCandidates(ctx, p, nodes) }()
	select {
	case <-entered:
		t.Fatal("another task exceeded the shared probe budget")
	case <-time.After(50 * time.Millisecond):
	}
	once.Do(func() { close(gate) })
	select {
	case err := <-sourceDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("source checker lost budget slots")
	}
	waitProbeFinished(t, p)
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		s := p.State()
		if len(s.Release.Previews) > 0 && s.Release.Previews[0].Status == "ready" {
			if maximum.Load() > 4 || len(p.probeSlots) != 0 {
				t.Fatalf("shared budget leaked or exceeded: max=%d held=%d", maximum.Load(), len(p.probeSlots))
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("release preflight lost budget slots")
}
