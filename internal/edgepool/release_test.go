package edgepool

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func releaseOrigin(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/salcara-hub/statusz" {
			t.Errorf("unexpected preflight %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"status":"available"}`))
	}))
	t.Cleanup(s.Close)
	return s
}
func prepareReleaseTest(t *testing.T, p *Pool, action string, payload any) ReleasePreview {
	t.Helper()
	w := controlRequest(p, "/v1/releases/prepare", map[string]any{"expected_revision": p.State().Revision, "action": action, "payload": payload})
	if w.Code != 202 {
		t.Fatalf("prepare: %d %s", w.Code, w.Body.String())
	}
	var state State
	if json.Unmarshal(w.Body.Bytes(), &state) != nil {
		t.Fatal("invalid state")
	}
	id := state.Release.Previews[len(state.Release.Previews)-1].ID
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, v := range p.State().Release.Previews {
			if v.ID == id && v.Status != "checking" {
				return v
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("preflight did not finish")
	return ReleasePreview{}
}
func startReleaseTest(t *testing.T, p *Pool, preview ReleasePreview) State {
	t.Helper()
	revision := p.State().Revision
	state, status, code := p.startRelease(&revision, preview.ID)
	if status != 200 || code != "" {
		t.Fatalf("start: %d %s (%+v)", status, code, preview)
	}
	return state
}

func TestReleasePreflightTrialConfirmAndHistory(t *testing.T) {
	p := testPool(t, releaseOrigin(t).URL)
	preview := prepareReleaseTest(t, p, "policy", map[string]any{"mode": "latency"})
	if preview.Status != "ready" || preview.Completed != 1 || preview.Healthy != 1 {
		t.Fatalf("preflight: %+v", preview)
	}
	if p.State().Mode != "manual" || p.State().Revision != 1 {
		t.Fatal("prepare changed live config")
	}
	s := startReleaseTest(t, p, preview)
	if s.Mode != "latency" || s.Release.Trial == nil || s.Release.Trial.ExpiresAt-s.Release.Trial.StartedAt != 120000 {
		t.Fatalf("trial: %+v", s.Release)
	}
	w := controlRequest(p, "/v1/policy", map[string]any{"expected_revision": s.Revision, "mode": "balance"})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "trial_in_progress") {
		t.Fatalf("ordinary mutation during trial: %d %s", w.Code, w.Body.String())
	}
	s, status, code := p.confirmRelease(&s.Revision, s.Release.Trial.ID)
	if status != 200 || code != "" || s.Release.Trial != nil || s.Mode != "latency" || s.Revision != 3 {
		t.Fatalf("confirm: %d %s %+v", status, code, s)
	}
	if len(s.Release.History) != 1 || s.Release.History[0].Revision != 1 {
		t.Fatalf("history: %+v", s.Release.History)
	}
	s, status, code = p.rollbackRelease(&s.Revision, "", s.Release.History[0].ID)
	if status != 200 || code != "" || s.Mode != "manual" || s.Revision != 4 {
		t.Fatalf("history restore: %d %s", status, code)
	}
}

func TestReleaseRejectedAndStaleCandidatesLeaveLiveState(t *testing.T) {
	p := testPool(t, healthyOrigin(t).URL)
	preview := prepareReleaseTest(t, p, "nodes", map[string]any{"node": map[string]any{"name": "bad", "proxy_url": "http://private-user:private-password@127.0.0.1:1", "enabled": true}})
	if preview.Status != "rejected" || preview.Reason != "changed_node_unreachable" {
		t.Fatalf("bad preflight accepted: %+v", preview)
	}
	s := p.State()
	if len(s.Nodes) != 1 || s.Revision != 1 || len(s.Release.History) != 0 {
		t.Fatal("failed candidate changed live config")
	}
	public, _ := json.Marshal(s)
	if strings.Contains(string(public), "private-") {
		t.Fatal("candidate secret leaked")
	}
	preview = prepareReleaseTest(t, p, "policy", map[string]any{"mode": "failover"})
	setPolicy(t, p, "balance", "direct")
	rev := p.State().Revision
	if _, status, code := p.startRelease(&rev, preview.ID); status != 409 || code != "revision_conflict" {
		t.Fatalf("stale preview: %d %s", status, code)
	}
}

func TestReleaseRestartAndDeadlineRollback(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "restart"}[restart], func(t *testing.T) {
			p := testPool(t, releaseOrigin(t).URL)
			preview := prepareReleaseTest(t, p, "policy", map[string]any{"mode": "failover"})
			started := startReleaseTest(t, p, preview)
			if restart {
				cfg := p.cfg
				cfg.ControlToken = testToken
				p.Close()
				var err error
				p, err = New(cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer p.Close()
			} else {
				p.mu.Lock()
				p.trialDeadline = time.Now().Add(-time.Second)
				p.mu.Unlock()
				if _, status, code := p.confirmRelease(&started.Revision, started.Release.Trial.ID); status != 409 || code != "trial_expired" {
					t.Fatalf("late confirm: %d %s", status, code)
				}
				p.wakeRelease()
				until := time.Now().Add(2 * time.Second)
				for time.Now().Before(until) && p.State().Release.Trial != nil {
					time.Sleep(5 * time.Millisecond)
				}
			}
			s := p.State()
			if s.Release.Trial != nil || s.Mode != "manual" || s.Revision != started.Revision+1 {
				t.Fatalf("recovery: %+v", s)
			}
		})
	}
}

func TestReleaseRollbackCASAndDirectGuard(t *testing.T) {
	p := testPool(t, releaseOrigin(t).URL)
	preview := prepareReleaseTest(t, p, "policy", map[string]any{"mode": "failover"})
	s := startReleaseTest(t, p, preview)
	old := s.Revision - 1
	if _, status, code := p.rollbackRelease(&old, s.Release.Trial.ID, ""); status != 409 || code != "revision_conflict" {
		t.Fatal("stale rollback accepted")
	}
	s, status, code := p.rollbackRelease(&s.Revision, s.Release.Trial.ID, "")
	if status != 200 || code != "" || s.Mode != "manual" {
		t.Fatal("explicit rollback failed")
	}
	w := controlRequest(p, "/v1/nodes", map[string]any{"expected_revision": s.Revision, "node": map[string]any{"id": "direct", "name": "direct", "enabled": false}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	s = p.State()
	s, status, code = p.rollbackRelease(&s.Revision, "", s.Release.History[0].ID)
	if status != 200 || code != "" || s.Nodes[0].Enabled || s.Release.LastError != "rollback_preserved_direct_block" {
		t.Fatalf("direct prohibition lost: %+v %d %s", s, status, code)
	}
}

func TestReleaseSupersededResponsibilityNeverOverwritesLaterConfig(t *testing.T) {
	p := testPool(t, releaseOrigin(t).URL)
	preview := prepareReleaseTest(t, p, "policy", map[string]any{"mode": "failover"})
	startReleaseTest(t, p, preview)
	// Simulate a future writer or externally repaired journal bypassing the normal
	// trial block. Recovery must compare the pending ownership revision itself.
	p.mu.Lock()
	p.saved.Revision++
	p.saved.Mode = "balance"
	p.trialDeadline = time.Now().Add(-time.Second)
	p.mu.Unlock()
	if !p.expireRelease() {
		t.Fatal("superseded recovery failed")
	}
	s := p.State()
	if s.Mode != "balance" || s.Release.Trial != nil || s.Release.LastError != "trial_superseded" {
		t.Fatalf("overwrote newer config: %+v", s)
	}
}

func TestReleaseHistoryBoundedPrivateAndSourceDetach(t *testing.T) {
	p := testPool(t, "https://origin.example.test")
	for i := 0; i < 12; i++ {
		setPolicy(t, p, "failover", "direct")
	}
	s := p.State()
	if len(s.Release.History) != releaseHistoryLimit {
		t.Fatal("unbounded history")
	}
	rev := s.Revision
	_, status, code := p.commitConfig(&rev, func(s *savedConfig) string {
		s.Nodes = append(s.Nodes, nodeConfig{ID: "owned", Name: "owned", ProxyURL: "http://private-user:private-password@proxy.example:8080", SourceID: "source", SourceKey: "key", Enabled: false})
		return ""
	})
	if status != 200 || code != "" {
		t.Fatal(code)
	}
	w := controlRequest(p, "/v1/nodes", map[string]any{"expected_revision": p.State().Revision, "node": map[string]any{"id": "owned", "name": "manual", "proxy_url": "http://new:credential@proxy.example:8080", "enabled": false}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	p.mu.Lock()
	owned := p.saved.Nodes[1]
	p.mu.Unlock()
	if owned.SourceID != "" || owned.SourceKey != "" {
		t.Fatal("manual connection edit kept source ownership")
	}
	raw, _ := json.Marshal(p.State())
	if strings.Contains(string(raw), "private-") || strings.Contains(string(raw), "new:credential") {
		t.Fatal("history secret leaked in state")
	}
	stored, err := readSaved(p.cfg.DataDir)
	if err != nil || len(stored.History) != releaseHistoryLimit {
		t.Fatalf("history roundtrip: %v", err)
	}
	info, err := os.Stat(filepath.Join(p.cfg.DataDir, stateFile))
	if err != nil || info.Size() > maxStateBytes {
		t.Fatal("unbounded state file")
	}
}

func TestReleasePreflightCancellationAndBounds(t *testing.T) {
	entered := make(chan struct{}, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer origin.Close()
	p := testPool(t, origin.URL)
	w := controlRequest(p, "/v1/releases/prepare", map[string]any{"expected_revision": 1, "action": "policy", "payload": map[string]any{"mode": "failover"}})
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("preflight never reached fixed origin")
	}
	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel preflight")
	}
}

func TestReleasePreflightRejectsRedirectAndOversize(t *testing.T) {
	for _, mode := range []string{"redirect", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "redirect" {
					http.Redirect(w, r, "http://127.0.0.1:1/", 302)
				} else {
					_, _ = w.Write([]byte(`{"status":"available","padding":"` + strings.Repeat("x", 4096) + `"}`))
				}
			}))
			defer origin.Close()
			p := testPool(t, origin.URL)
			preview := prepareReleaseTest(t, p, "policy", map[string]any{"mode": "failover"})
			if preview.Status != "rejected" || preview.Healthy != 0 || p.State().Revision != 1 {
				t.Fatalf("invalid health accepted: %+v", preview)
			}
		})
	}
}

func TestReleaseDeadlinePersistenceFailureKeepsResponsibility(t *testing.T) {
	p := testPool(t, releaseOrigin(t).URL)
	preview := prepareReleaseTest(t, p, "policy", map[string]any{"mode": "failover"})
	started := startReleaseTest(t, p, preview)
	path := filepath.Join(p.cfg.DataDir, stateFile)
	backup := filepath.Join(p.cfg.DataDir, "pending-release-backup")
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.trialDeadline = time.Now().Add(-time.Second)
	p.mu.Unlock()
	if p.expireRelease() {
		t.Fatal("rollback falsely reported persisted success")
	}
	state := p.State()
	if state.Release.Trial == nil || state.Release.Trial.ID != started.Release.Trial.ID || state.Mode != "failover" || state.Release.LastError != "rollback_pending" {
		t.Fatal("pending responsibility was lost")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	if !p.expireRelease() || p.State().Release.Trial != nil || p.State().Mode != "manual" {
		t.Fatal("recovery did not retry after persistence became available")
	}
}

func TestReleasePreflightConcurrencyBound(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 16)
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
	for i := 0; i < 7; i++ {
		addNode(t, p, "direct", "")
	}
	w := controlRequest(p, "/v1/releases/prepare", map[string]any{"expected_revision": p.State().Revision, "action": "policy", "payload": map[string]any{"mode": "balance"}})
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	for i := 0; i < 4; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("preflight workers did not start")
		}
	}
	if maximum.Load() != 4 {
		t.Fatalf("unexpected preflight concurrency: %d", maximum.Load())
	}
	select {
	case <-entered:
		t.Fatal("preflight exceeded concurrency limit")
	case <-time.After(30 * time.Millisecond):
	}
	close(gate)
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		s := p.State()
		if s.Release.Previews[0].Status == "ready" {
			if s.Release.Previews[0].Completed != 8 || maximum.Load() > 4 {
				t.Fatal("invalid completed preflight")
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("preflight did not complete")
}

func TestReleaseDependencyFailurePreservesPendingTrial(t *testing.T) {
	p := testPool(t, releaseOrigin(t).URL)
	preview := prepareReleaseTest(t, p, "policy", map[string]any{"mode": "failover"})
	started := startReleaseTest(t, p, preview)
	p.sources.mu.Lock()
	p.sources.busy = true
	p.sources.mu.Unlock()
	p.mu.Lock()
	p.trialDeadline = time.Now().Add(-time.Second)
	p.mu.Unlock()
	if p.expireRelease() {
		t.Fatal("busy restore dependency incorrectly reported success")
	}
	state := p.State()
	if state.Revision != started.Revision || state.Release.Trial == nil || state.Release.LastError != "rollback_pending" || state.Mode != "failover" {
		t.Fatal("failed dependency lost recovery responsibility")
	}
	p.sources.mu.Lock()
	p.sources.busy = false
	p.sources.mu.Unlock()
	if !p.expireRelease() || p.State().Release.Trial != nil || p.State().Mode != "manual" {
		t.Fatal("dependency recovery was not retried")
	}
}

func TestReleaseHistoryMissingSourceCannotReportSuccessfulRestore(t *testing.T) {
	p := testPool(t, "https://origin.example.test")
	revision := p.State().Revision
	_, status, code := p.commitConfig(&revision, func(s *savedConfig) string {
		s.Nodes = append(s.Nodes, nodeConfig{ID: "owned", Name: "owned", ProxyURL: "http://user:secret@proxy.example.test:8080", Imported: true, SourceID: "missing", SourceKey: "key", Enabled: false})
		return ""
	})
	if status != 200 || code != "" {
		t.Fatal(code)
	}
	w := controlRequest(p, "/v1/nodes/delete", map[string]any{"expected_revision": p.State().Revision, "id": "owned"})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	before := p.State()
	_, status, code = p.rollbackRelease(&before.Revision, "", before.Release.History[0].ID)
	if status != 409 || code != "rollback_dependency_unavailable" || p.State().Revision != before.Revision || len(p.State().Nodes) != 1 {
		t.Fatal("missing source restore published a false success")
	}
}
