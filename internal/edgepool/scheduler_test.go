package edgepool

import (
	"bufio"
	"context"
	"encoding/json"
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

func configureScheduling(t *testing.T, p *Pool, mode string, limits Limits, update func(*savedConfig)) {
	t.Helper()
	revision := p.State().Revision
	_, status, code := p.commitConfig(&revision, func(s *savedConfig) string {
		s.Mode = mode
		s.Limits = limits
		if update != nil {
			update(s)
		}
		return ""
	})
	if status != 200 {
		t.Fatalf("scheduler configuration: %d %s", status, code)
	}
}

func awaitRuntime(t *testing.T, p *Pool, match func(State) bool) State {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		state := p.State()
		if match(state) {
			return state
		}
		if time.Now().After(deadline) {
			t.Fatalf("runtime did not settle: %+v", state.Runtime)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBalanceReservesConcurrentProxyCapacityAtomically(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
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
	var connectionsA, connectionsB atomic.Int32
	a := testHTTPProxy(t, origin.URL, &connectionsA)
	b := testHTTPProxy(t, origin.URL, &connectionsB)
	idA := addNode(t, p, "A", a.URL)
	idB := addNode(t, p, "B", b.URL)
	configureScheduling(t, p, "balance", Limits{MaxRequests: 4, MaxStreams: 2, QueueLimit: 1, QueueTimeoutMS: 2000}, func(s *savedConfig) {
		for i := range s.Nodes {
			n := &s.Nodes[i]
			n.Enabled = n.ID != "direct"
			n.MaxRequests = 2
			n.MaxStreams = 1
		}
		s.SelectedID = idA
	})
	waitProbe(t, p)
	edge := httptest.NewServer(p.Handler())
	t.Cleanup(edge.Close)
	results := make(chan int, 4)
	for range 4 {
		go func() {
			response, err := http.Post(edge.URL+"/salcara-hub/v1/app/commands", "application/json", strings.NewReader(`{}`))
			if err != nil {
				results <- 0
				return
			}
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			results <- response.StatusCode
		}()
	}
	state := awaitRuntime(t, p, func(s State) bool { return s.Runtime.ActiveRequests == 4 })
	if state.ActiveID != "" {
		t.Fatal("balance exposes a single active route")
	}
	for _, node := range state.Nodes {
		if node.ID == idA || node.ID == idB {
			if node.Runtime.ActiveRequests != 2 {
				t.Fatalf("per-node slot oversubscription: %#v", node)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queuedDone := make(chan struct{})
	go func() {
		defer close(queuedDone)
		req, _ := http.NewRequestWithContext(ctx, "GET", edge.URL+"/salcara-hub/v1/ping", nil)
		response, err := http.DefaultClient.Do(req)
		if err == nil {
			response.Body.Close()
		}
	}()
	awaitRuntime(t, p, func(s State) bool { return s.Runtime.QueuedRequests == 1 })
	response, err := http.Get(edge.URL + "/salcara-hub/v1/ping")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 429 {
		t.Fatalf("full queue accepted request: %d", response.StatusCode)
	}
	cancel()
	<-queuedDone
	awaitRuntime(t, p, func(s State) bool { return s.Runtime.QueuedRequests == 0 })
	releaseOnce.Do(func() { close(release) })
	for range 4 {
		if status := <-results; status != 200 {
			t.Fatalf("request failed: %d", status)
		}
	}
	state = awaitRuntime(t, p, func(s State) bool { return s.Runtime.ActiveRequests == 0 })
	if state.Runtime.CompletedRequests != 4 || state.Runtime.RejectedRequests != 1 || state.Runtime.FailedRequests != 0 {
		t.Fatalf("wrong completion accounting: %#v", state.Runtime)
	}
	if connectionsA.Load() < 3 || connectionsB.Load() < 3 {
		t.Fatal("requests did not traverse both real proxies")
	}
}

func TestBalanceWeightsAndSeparateStreamSlots(t *testing.T) {
	origin := healthyOrigin(t)
	p := testPool(t, origin.URL)
	other := addNode(t, p, "weighted", "")
	configureScheduling(t, p, "balance", Limits{MaxRequests: 8, MaxStreams: 2, QueueLimit: 2, QueueTimeoutMS: 200}, func(s *savedConfig) {
		for i := range s.Nodes {
			s.Nodes[i].MaxRequests = 8
			s.Nodes[i].MaxStreams = 1
			if s.Nodes[i].ID == other {
				s.Nodes[i].Weight = 3
			}
		}
	})
	waitProbe(t, p)
	leases := make([]*requestLease, 0, 6)
	defer func() {
		for _, lease := range leases {
			lease.finish("cancelled", false)
		}
	}()
	for range 4 {
		lease, err := p.acquire(context.Background(), false)
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, lease)
	}
	state := p.State()
	if state.Nodes[0].Runtime.ActiveRequests != 1 || state.Nodes[1].Runtime.ActiveRequests != 3 {
		t.Fatalf("weights ignored: %#v", state.Nodes)
	}
	for range 2 {
		lease, err := p.acquire(context.Background(), true)
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, lease)
	}
	state = p.State()
	if state.Runtime.ActiveStreams != 2 || state.Runtime.ActiveRequests != 4 {
		t.Fatal("short and streaming quotas are not independent")
	}
	for _, node := range state.Nodes {
		if node.Runtime.ActiveStreams != 1 {
			t.Fatal("node stream capacity exceeded")
		}
	}
}

func TestQueueFIFOTimeoutAndCloseWakeup(t *testing.T) {
	p := testPool(t, "http://127.0.0.1:1")
	configureScheduling(t, p, "manual", Limits{MaxRequests: 1, MaxStreams: 1, QueueLimit: 2, QueueTimeoutMS: 60}, nil)
	first, err := p.acquire(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer first.finish("cancelled", false)
	_, err = p.acquire(context.Background(), false)
	if e, ok := err.(*admissionError); !ok || e.code != "queue_timeout" {
		t.Fatalf("queue timeout missing: %v", err)
	}
	if p.State().Runtime.QueuedRequests != 0 || p.State().Runtime.RejectedRequests != 1 {
		t.Fatal("timed out waiter leaked")
	}
	configureScheduling(t, p, "manual", Limits{MaxRequests: 1, MaxStreams: 1, QueueLimit: 2, QueueTimeoutMS: 2000}, nil)
	second := make(chan *requestLease, 1)
	third := make(chan *requestLease, 1)
	go func() { lease, _ := p.acquire(context.Background(), false); second <- lease }()
	awaitRuntime(t, p, func(s State) bool { return s.Runtime.QueuedRequests == 1 })
	go func() { lease, _ := p.acquire(context.Background(), false); third <- lease }()
	awaitRuntime(t, p, func(s State) bool { return s.Runtime.QueuedRequests == 2 })
	first.finish("completed", false)
	var secondLease *requestLease
	select {
	case secondLease = <-second:
		if secondLease == nil {
			t.Fatal("first waiter rejected")
		}
	case <-time.After(time.Second):
		t.Fatal("first queued request not resumed")
	}
	select {
	case <-third:
		t.Fatal("later waiter jumped capacity or FIFO")
	default:
	}
	p.Close()
	select {
	case lease := <-third:
		if lease != nil {
			t.Fatal("queued request admitted after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not wake queue")
	}
	secondLease.finish("cancelled", false)
	if state := p.State(); state.Runtime.ActiveRequests != 0 || state.Runtime.QueuedRequests != 0 {
		t.Fatal("Close/finish leaked counters")
	}
}

func TestDrainDisableDeleteAndTransportReplaceKeepInflightCounts(t *testing.T) {
	origin := healthyOrigin(t)
	p := testPool(t, origin.URL)
	backup := addNode(t, p, "backup", "")
	configureScheduling(t, p, "balance", Limits{MaxRequests: 4, MaxStreams: 4, QueueLimit: 1, QueueTimeoutMS: 200}, nil)
	waitProbe(t, p)
	lease, err := p.acquire(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.finish("cancelled", false)
	id := lease.route.config.ID
	revision := p.State().Revision
	_, status, code := p.commitConfig(&revision, func(s *savedConfig) string {
		for i := range s.Nodes {
			if s.Nodes[i].ID == id {
				s.Nodes[i].Draining = true
			}
		}
		return ""
	})
	if status != 200 {
		t.Fatal(code)
	}
	next, err := p.acquire(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if next.route.config.ID == id {
		t.Fatal("draining node accepted new work")
	}
	next.finish("completed", false)
	if lease.route.runtime.ActiveRequests != 1 {
		t.Fatal("draining erased in-flight count")
	}
	// Replacing the proxy transport shares the node's accounting until the old
	// transport drains, rather than giving the new version a fresh full quota.
	revision = p.State().Revision
	_, status, code = p.commitConfig(&revision, func(s *savedConfig) string {
		for i := range s.Nodes {
			if s.Nodes[i].ID == id {
				s.Nodes[i].ProxyURL = "http://127.0.0.1:7890"
				s.Nodes[i].Draining = false
				s.Nodes[i].Enabled = false
			}
		}
		s.SelectedID = firstEnabled(*s)
		return ""
	})
	if status != 200 {
		t.Fatal(code)
	}
	p.mu.Lock()
	current := p.routes[id]
	shared := current.runtime == lease.route.runtime
	p.mu.Unlock()
	if !shared || current.runtime.ActiveRequests != 1 {
		t.Fatal("transport replacement lost in-flight accounting")
	}
	w := controlRequest(p, "/v1/nodes/delete", map[string]any{"expected_revision": p.State().Revision, "id": id})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if p.State().Runtime.ActiveRequests != 1 {
		t.Fatal("delete erased global in-flight count")
	}
	lease.finish("completed", false)
	lease.finish("failed", true)
	if p.State().Runtime.ActiveRequests != 0 || p.State().Runtime.FailedRequests != 0 {
		t.Fatal("retired request release duplicated")
	}
	_ = backup
}

func TestRealSSECancellationAndTruncatedBodyReleaseSlots(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/salcara-hub/statusz" {
			io.WriteString(w, `{"status":"available"}`)
			return
		}
		if r.URL.RawQuery == "broken" {
			conn, buffer, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			buffer.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 100\r\n\r\ndata: partial\n\n")
			buffer.Flush()
			conn.Close()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	registerTestOrigin(t, origin)
	p := testPool(t, origin.URL)
	waitProbe(t, p)
	request := func(ctx context.Context, query string) (*http.Response, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", origin.URL+"/salcara-hub/v1/app/stream"+query, nil)
		req.Header.Set("Accept", "text/event-stream")
		return (fixedTransport{pool: p}).RoundTrip(req)
	}
	ctx, cancel := context.WithCancel(context.Background())
	response, err := request(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = bufio.NewReader(response.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if p.State().Runtime.ActiveStreams != 1 {
		t.Fatal("SSE not classified as stream")
	}
	cancel()
	awaitRuntime(t, p, func(s State) bool { return s.Runtime.ActiveStreams == 0 })
	response.Body.Close()
	if p.State().Nodes[0].Health.Status != "healthy" || p.State().Runtime.CancelledRequests != 1 {
		t.Fatal("caller cancellation penalized node")
	}
	response, err = request(context.Background(), "?broken")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err == nil {
		t.Fatal("truncated SSE was not detected")
	}
	state := p.State()
	if state.Runtime.ActiveStreams != 0 || state.Runtime.FailedRequests != 1 || state.Nodes[0].Health.Status != "unhealthy" {
		t.Fatalf("SSE read failure not accounted: %#v", state)
	}
}

func TestRecoveredRouteAdmissionIsLimited(t *testing.T) {
	p := testPool(t, "http://127.0.0.1:1")
	configureScheduling(t, p, "manual", Limits{MaxRequests: 4, MaxStreams: 4, QueueLimit: 0, QueueTimeoutMS: 100}, nil)
	p.mu.Lock()
	r := p.routes["direct"]
	r.health.Status = "healthy"
	r.recovering = true
	p.mu.Unlock()
	lease, err := p.acquire(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.finish("cancelled", false)
	if _, err = p.acquire(context.Background(), true); err == nil {
		t.Fatal("recovering node accepted concurrent trial")
	}
	lease.finish("completed", false)
	stream, err := p.acquire(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	stream.finish("completed", false)
}

func TestSchedulingConfigMigrationAndPartialUpdates(t *testing.T) {
	dir := privateDataDir(t)
	legacy := `{"revision":7,"mode":"manual","selected_id":"direct","nodes":[{"id":"direct","name":"direct","proxy_url":"","enabled":true}]}`
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := New(Config{Origin: "http://127.0.0.1:1", DataDir: dir, ControlToken: testToken, DisablePeriodicProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	state := p.State()
	if state.Limits.QueueLimit != 64 || state.Limits.MaxRequests != 256 || state.Nodes[0].Weight != 1 || state.Nodes[0].MaxStreams != 16 {
		t.Fatal("legacy defaults not migrated")
	}
	w := controlRequest(p, "/v1/nodes", map[string]any{"expected_revision": state.Revision, "node": map[string]any{"id": "direct", "name": "direct", "enabled": true, "weight": 4, "max_requests": 3, "max_streams": 2, "draining": true}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = controlRequest(p, "/v1/policy", map[string]any{"expected_revision": p.State().Revision, "mode": "balance", "selected_id": "", "limits": map[string]any{"queue_limit": 0}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	state = p.State()
	if state.Limits.QueueLimit != 0 || state.Limits.MaxRequests != 256 || state.Nodes[0].Weight != 4 || !state.Nodes[0].Draining {
		t.Fatal("partial update reset settings")
	}
	w = controlRequest(p, "/v1/policy", map[string]any{"expected_revision": state.Revision, "mode": "balance", "selected_id": "", "limits": map[string]any{"max_requests": 0, "max_streams": 0, "queue_limit": 0, "queue_timeout_ms": 0}})
	if w.Code != 400 {
		t.Fatal("explicit invalid zeros migrated as old config")
	}
	raw, _ := json.Marshal(p.State())
	if !strings.Contains(string(raw), `"runtime"`) {
		t.Fatal("runtime missing from wire state")
	}
}
