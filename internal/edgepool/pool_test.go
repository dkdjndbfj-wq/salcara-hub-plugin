package edgepool

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "01234567890123456789012345678901"

var testOriginRoots sync.Map

func privateDataDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private-data")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func trustTestOrigin(p *Pool) {
	if value, ok := testOriginRoots.Load(p.origin.String()); ok {
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, route := range p.routes {
			route.normal.TLSClientConfig = &tls.Config{RootCAs: value.(*x509.CertPool)}
			route.once.TLSClientConfig = &tls.Config{RootCAs: value.(*x509.CertPool)}
		}
	}
}

func registerTestOrigin(t *testing.T, s *httptest.Server) {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(s.Certificate())
	testOriginRoots.Store(s.URL, roots)
	t.Cleanup(func() { testOriginRoots.Delete(s.URL); s.Close() })
}

func testPool(t *testing.T, origin string) *Pool {
	t.Helper()
	p, err := New(Config{Origin: origin, DataDir: privateDataDir(t), ControlToken: testToken, DisablePeriodicProbe: true, ProbeTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	trustTestOrigin(p)
	return p
}

func controlRequest(p *Pool, path string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	p.ControlHandler().ServeHTTP(w, r)
	return w
}

func addNode(t *testing.T, p *Pool, name, proxy string) string {
	t.Helper()
	w := controlRequest(p, "/v1/nodes", map[string]any{"expected_revision": p.State().Revision, "node": map[string]any{"name": name, "proxy_url": proxy, "enabled": true}})
	if w.Code != 200 {
		t.Fatalf("add node: %d %s", w.Code, w.Body.String())
	}
	trustTestOrigin(p)
	s := p.State()
	return s.Nodes[len(s.Nodes)-1].ID
}

func setPolicy(t *testing.T, p *Pool, mode, id string) {
	t.Helper()
	w := controlRequest(p, "/v1/policy", map[string]any{"expected_revision": p.State().Revision, "mode": mode, "selected_id": id})
	if w.Code != 200 {
		t.Fatalf("policy: %d %s", w.Code, w.Body.String())
	}
}

func waitProbe(t *testing.T, p *Pool) {
	t.Helper()
	p.Probe()
	deadline := time.Now().Add(3 * time.Second)
	for p.State().ProbeRunning {
		if time.Now().After(deadline) {
			t.Fatal("probe did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func healthyOrigin(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/salcara-hub/statusz" {
			io.WriteString(w, `{"status":"available"}`)
			return
		}
		io.WriteString(w, "ok")
	}))
	registerTestOrigin(t, s)
	return s
}

func testHTTPProxy(t *testing.T, origin string, requests *atomic.Int32) *httptest.Server {
	t.Helper()
	target, _ := url.Parse(origin)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != "CONNECT" || r.Host != target.Host {
			t.Error("unexpected proxy destination")
			http.Error(w, "invalid destination", 400)
			return
		}
		upstream, err := net.Dial("tcp", target.Host)
		if err != nil {
			http.Error(w, "unavailable", 502)
			return
		}
		client, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		defer client.Close()
		defer upstream.Close()
		io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
		done := make(chan struct{})
		go func() { io.Copy(upstream, buffer); close(done) }()
		io.Copy(client, upstream)
		client.Close()
		<-done
	}))
	t.Cleanup(s.Close)
	return s
}

func TestControlPersistenceRedactionAndRevision(t *testing.T) {
	origin := healthyOrigin(t)
	p := testPool(t, origin.URL)
	id := addNode(t, p, "gateway", "http://test-user:test-secret@127.0.0.1:3128")
	state, _ := json.Marshal(p.State())
	if bytes.Contains(state, []byte("test-user")) || bytes.Contains(state, []byte("test-secret")) {
		t.Fatal("credentials leaked")
	}
	if !p.State().Nodes[1].HasCredentials {
		t.Fatal("missing credential indicator")
	}
	w := controlRequest(p, "/v1/nodes", map[string]any{"expected_revision": p.State().Revision, "node": map[string]any{"id": id, "name": "renamed", "enabled": true}})
	if w.Code != 200 || !p.State().Nodes[1].HasCredentials {
		t.Fatal("omission must preserve credentials")
	}
	revision := p.State().Revision
	var successes, conflicts atomic.Int32
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			w := controlRequest(p, "/v1/policy", map[string]any{"expected_revision": revision, "mode": "failover", "selected_id": id})
			if w.Code == 200 {
				successes.Add(1)
			} else if w.Code == 409 {
				conflicts.Add(1)
			}
		}()
	}
	workers.Wait()
	if successes.Load() != 1 || conflicts.Load() != 1 {
		t.Fatalf("concurrent update lost: %d %d", successes.Load(), conflicts.Load())
	}
	dir := p.cfg.DataDir
	p.Close()
	reopened, err := New(Config{Origin: origin.URL, DataDir: dir, ControlToken: testToken, DisablePeriodicProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.State().Revision != revision+1 || !reopened.State().Nodes[1].HasCredentials {
		t.Fatal("configuration not retained")
	}
	w = controlRequest(reopened, "/v1/nodes", map[string]any{"expected_revision": reopened.State().Revision, "node": map[string]any{"id": id, "name": "direct alternative", "proxy_url": "", "enabled": true}})
	if w.Code != 200 || reopened.State().Nodes[1].HasCredentials || reopened.State().Nodes[1].ProxyURL != "" {
		t.Fatal("explicit empty must select direct")
	}
}

func TestStorageLocksAndRejectedPersistence(t *testing.T) {
	origin := healthyOrigin(t)
	p := testPool(t, origin.URL)
	_, err := New(Config{Origin: origin.URL, DataDir: p.cfg.DataDir, ControlToken: testToken, DisablePeriodicProbe: true})
	if err == nil {
		t.Fatal("second writer acquired data directory")
	}
	revision := p.State().Revision
	path := filepath.Join(p.cfg.DataDir, stateFile)
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	w := controlRequest(p, "/v1/policy", map[string]any{"expected_revision": revision, "mode": "failover", "selected_id": "direct"})
	if w.Code != 500 || p.State().Revision != revision || p.State().Mode != "manual" {
		t.Fatal("failed persistence changed runtime")
	}
}

func TestStorageRejectsLinksAndWidePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission and unprivileged symlink checks; Windows uses OS locks and inherited ACLs")
	}
	dir := privateDataDir(t)
	target := filepath.Join(privateDataDir(t), "state")
	os.WriteFile(target, []byte(`{}`), 0600)
	if err := os.Symlink(target, filepath.Join(dir, stateFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Origin: "http://127.0.0.1:1", DataDir: dir, ControlToken: testToken, DisablePeriodicProbe: true}); err == nil {
		t.Fatal("symlink accepted")
	}
	os.Remove(filepath.Join(dir, stateFile))
	os.WriteFile(filepath.Join(dir, stateFile), []byte(`{}`), 0644)
	if err := os.Chmod(filepath.Join(dir, stateFile), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Origin: "http://127.0.0.1:1", DataDir: dir, ControlToken: testToken, DisablePeriodicProbe: true}); err == nil {
		t.Fatal("wide state permissions accepted")
	}
	if err := os.Chmod(filepath.Join(dir, stateFile), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Origin: "http://127.0.0.1:1", DataDir: dir, ControlToken: testToken, DisablePeriodicProbe: true}); err == nil || !strings.Contains(err.Error(), "data directory must be private") {
		t.Fatal("wide directory permissions accepted")
	}
}

func TestProxyRequiresHTTPSOriginAtMutationAndRestart(t *testing.T) {
	p := testPool(t, "http://127.0.0.1:8787")
	revision := p.State().Revision
	w := controlRequest(p, "/v1/nodes", map[string]any{"expected_revision": revision, "node": map[string]any{"name": "proxy", "proxy_url": "socks5://127.0.0.1:7891", "enabled": true}})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "https_origin_required") || p.State().Revision != revision {
		t.Fatal("HTTP origin accepted proxy or changed state")
	}
	origin := healthyOrigin(t)
	secure := testPool(t, origin.URL)
	addNode(t, secure, "proxy", "http://127.0.0.1:7890")
	dir := secure.cfg.DataDir
	secure.Close()
	if _, err := New(Config{Origin: "http://127.0.0.1:8787", DataDir: dir, ControlToken: testToken, DisablePeriodicProbe: true}); err == nil || err.Error() != "https_origin_required" {
		t.Fatal("restart downgraded proxy origin TLS")
	}
}

func TestProbeDisplayDoesNotRelabelOldSamples(t *testing.T) {
	var fail atomic.Bool
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "unavailable", 503)
			return
		}
		io.WriteString(w, `{"status":"available"}`)
	}))
	registerTestOrigin(t, origin)
	p := testPool(t, origin.URL)
	if p.State().Status != "unknown" || p.State().Nodes[0].Health.CheckedAt != 0 {
		t.Fatal("unmeasured route displayed ready")
	}
	setPolicy(t, p, "failover", "direct")
	waitProbe(t, p)
	first := p.State().Nodes[0].Health
	if p.State().Status != "ready" || first.Status != "healthy" || first.CheckedAt == 0 || first.LatencyMS == nil {
		t.Fatal("successful sample not recorded")
	}
	time.Sleep(2 * time.Millisecond)
	fail.Store(true)
	for range 2 {
		waitProbe(t, p)
		s := p.State()
		health := s.Nodes[0].Health
		if s.Status != "degraded" || health.Status != "degraded" || health.CheckedAt != first.CheckedAt || health.LastProbeAt <= first.CheckedAt || health.LatencyMS == nil || *health.LatencyMS != *first.LatencyMS || s.ActiveID != "direct" {
			t.Fatalf("failure misrepresented freshness or prematurely removed route: %#v", s)
		}
	}
	waitProbe(t, p)
	if p.State().Nodes[0].Health.Status != "unhealthy" || p.State().ActiveID != "" || p.State().Status != "unavailable" {
		t.Fatal("third failure did not remove route")
	}
	fail.Store(false)
	waitProbe(t, p)
	if p.State().Status != "ready" || p.State().ActiveID != "direct" {
		t.Fatal("successful probe did not recover route")
	}
}

func TestControlBoundaryAndPathRestriction(t *testing.T) {
	p := testPool(t, "http://127.0.0.1:1")
	for _, path := range []string{"/v1/state", "/v1/state?token=" + testToken} {
		w := httptest.NewRecorder()
		p.ControlHandler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 401 {
			t.Fatal("unauthenticated control access")
		}
	}
	for _, path := range []string{"/v1/state", "/other", "/salcara-hub/../other", "/salcara-hub/%2fother", "/salcara-hubish", "http://other.example/salcara-hub/statusz"} {
		w := httptest.NewRecorder()
		p.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Fatalf("unsafe path accepted: %s (%d)", path, w.Code)
		}
	}
	w := controlRequest(p, "/v1/nodes", map[string]any{"expected_revision": p.State().Revision, "node": map[string]any{"id": "direct", "name": "direct", "enabled": false}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = controlRequest(p, "/v1/policy", map[string]any{"expected_revision": p.State().Revision, "mode": "manual", "selected_id": "direct"})
	if w.Code != 400 {
		t.Fatal("disabled manual node accepted")
	}
}

func TestHTTPProxyProbeFailoverAndFixedOrigin(t *testing.T) {
	origin := healthyOrigin(t)
	var requests atomic.Int32
	forward := testHTTPProxy(t, origin.URL, &requests)
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "failed", 502) }))
	defer broken.Close()
	p := testPool(t, origin.URL)
	bad := addNode(t, p, "bad", broken.URL)
	good := addNode(t, p, "good", forward.URL)
	controlRequest(p, "/v1/nodes", map[string]any{"expected_revision": p.State().Revision, "node": map[string]any{"id": "direct", "name": "direct", "enabled": false}})
	setPolicy(t, p, "failover", bad)
	waitProbe(t, p)
	waitProbe(t, p)
	waitProbe(t, p)
	if p.State().ActiveID != good {
		t.Fatalf("failed node retained: %#v", p.State())
	}
	for _, n := range p.State().Nodes {
		if n.ID == bad && n.Health.Status != "unhealthy" {
			t.Fatal("bad node healthy")
		}
		if n.ID == good && n.Health.Status != "healthy" {
			t.Fatal("good node unhealthy")
		}
	}
	edge := httptest.NewServer(p.Handler())
	defer edge.Close()
	resp, err := http.Get(edge.URL + "/salcara-hub/v1/ping?x=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || requests.Load() < 2 {
		t.Fatal("requests did not use proxy")
	}
}

func TestSSEFlushesAndForwardingHeadersAreRebuilt(t *testing.T) {
	finish := make(chan struct{})
	seen := make(chan http.Header, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/salcara-hub/v1/app/stream" || r.URL.RawQuery != "after=7" {
			t.Error("path changed")
		}
		seen <- r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-finish:
		case <-r.Context().Done():
		}
	}))
	defer origin.Close()
	p := testPool(t, origin.URL)
	edge := httptest.NewServer(p.Handler())
	defer edge.Close()
	defer close(finish)
	req, _ := http.NewRequest("GET", edge.URL+"/salcara-hub/v1/app/stream?after=7", nil)
	req.Header.Set("X-Forwarded-For", "198.51.100.2")
	req.Header.Set("Forwarded", "for=198.51.100.2")
	req.Header.Set("X-Real-IP", "198.51.100.2")
	req.Header.Set("X-Forwarded-Prefix", "/attacker")
	req.Header.Set("Connection", "X-Leak")
	req.Header.Set("X-Leak", "secret")
	req.Header.Set("Authorization", "Bearer device-credential")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("SSE buffered: %q %v", line, err)
	}
	h := <-seen
	if h.Get("X-Forwarded-For") != "127.0.0.1" || h.Get("Forwarded") != "" || h.Get("X-Real-IP") != "" || h.Get("X-Leak") != "" || h.Get("X-Forwarded-Prefix") != "" || h.Get("Authorization") != "Bearer device-credential" {
		t.Fatalf("incorrect forwarded headers: %#v", h)
	}
}

func TestTrustedForwardedChain(t *testing.T) {
	p, err := New(Config{Origin: "http://127.0.0.1:1", DataDir: privateDataDir(t), ControlToken: testToken, DisablePeriodicProbe: true, TrustedProxyCIDRs: []string{"127.0.0.0/8", "10.0.0.0/8"}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	r := httptest.NewRequest("GET", "/salcara-hub", nil)
	r.RemoteAddr = "127.0.0.1:4000"
	r.Header.Set("X-Forwarded-For", "198.51.100.99, 203.0.113.4, 10.0.0.1")
	r.Header.Set("X-Forwarded-Proto", "https")
	client, proto := p.forwardedClient(r)
	if client != "203.0.113.4" || proto != "https" {
		t.Fatalf("bad trusted chain: %s %s", client, proto)
	}
	r.RemoteAddr = "192.0.2.1:4000"
	client, proto = p.forwardedClient(r)
	if client != "192.0.2.1" || proto != "http" {
		t.Fatal("untrusted peer supplied client identity")
	}
}

func TestPOSTIsNotRetriedAndNextRequestCanFailOver(t *testing.T) {
	var attempts atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			io.WriteString(w, "ok")
			return
		}
		attempts.Add(1)
		io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	registerTestOrigin(t, origin)
	var proxyRequests atomic.Int32
	broken := testHTTPProxy(t, origin.URL, &proxyRequests)
	p := testPool(t, origin.URL)
	bad := addNode(t, p, "drop", broken.URL)
	setPolicy(t, p, "failover", bad)
	edge := httptest.NewServer(p.Handler())
	defer edge.Close()
	req, _ := http.NewRequest("POST", edge.URL+"/salcara-hub/v1/app/commands", strings.NewReader(`{"type":"execute"}`))
	req.Header.Set("Idempotency-Key", "test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 || attempts.Load() != 1 {
		t.Fatalf("POST was replayed: %d %d", resp.StatusCode, attempts.Load())
	}
	if p.State().ActiveID != "direct" {
		t.Fatal("new requests did not fail over")
	}
	resp, err = http.Get(edge.URL + "/salcara-hub/v1/ping")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("fallback not usable")
	}
}

func TestLatencyHysteresisAndImmediateFailure(t *testing.T) {
	p := testPool(t, "http://127.0.0.1:1")
	other := addNode(t, p, "alternate", "")
	setPolicy(t, p, "latency", "direct")
	p.mu.Lock()
	a, b := 100.0, 90.0
	p.routes["direct"].health = Health{Status: "healthy", LatencyMS: &a}
	p.routes[other].health = Health{Status: "healthy", LatencyMS: &b}
	p.switched = time.Now().Add(-3 * time.Minute)
	p.chooseLocked("probe")
	if p.active != "direct" {
		t.Error("small gain caused flapping")
	}
	b = 40
	p.chooseLocked("probe")
	if p.active != other {
		t.Error("large stable gain not selected")
	}
	a = 1
	p.chooseLocked("probe")
	if p.active != other {
		t.Error("cooldown ignored")
	}
	p.routes[other].health.Status = "unhealthy"
	p.chooseLocked("failed")
	if p.active != "direct" {
		t.Error("cooldown blocked failure recovery")
	}
	p.mu.Unlock()
}

func TestTLSVerificationRemainsEnabled(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"status":"available"}`) }))
	defer origin.Close()
	p := testPool(t, origin.URL)
	waitProbe(t, p)
	waitProbe(t, p)
	waitProbe(t, p)
	if p.State().Nodes[0].Health.Status != "unhealthy" {
		t.Fatal("untrusted TLS certificate accepted")
	}
}

func TestSOCKS5AuthenticationAndDestination(t *testing.T) {
	origin := healthyOrigin(t)
	u, _ := url.Parse(origin.URL)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	seen := make(chan string, 4)
	var workers sync.WaitGroup
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(3 * time.Second))
				reader := bufio.NewReader(conn)
				head := make([]byte, 2)
				if _, err := io.ReadFull(reader, head); err != nil {
					return
				}
				methods := make([]byte, int(head[1]))
				io.ReadFull(reader, methods)
				conn.Write([]byte{5, 2})
				io.ReadFull(reader, head)
				username := make([]byte, int(head[1]))
				io.ReadFull(reader, username)
				length, _ := reader.ReadByte()
				password := make([]byte, int(length))
				io.ReadFull(reader, password)
				if string(username) != "user" || string(password) != "pass" {
					return
				}
				conn.Write([]byte{1, 0})
				request := make([]byte, 4)
				io.ReadFull(reader, request)
				var host string
				switch request[3] {
				case 1:
					ip := make([]byte, 4)
					io.ReadFull(reader, ip)
					host = net.IP(ip).String()
				case 3:
					length, _ := reader.ReadByte()
					name := make([]byte, int(length))
					io.ReadFull(reader, name)
					host = string(name)
				default:
					return
				}
				port := make([]byte, 2)
				io.ReadFull(reader, port)
				destination := net.JoinHostPort(host, fmt.Sprint(binary.BigEndian.Uint16(port)))
				seen <- destination
				upstream, err := net.Dial("tcp", destination)
				if err != nil {
					return
				}
				defer upstream.Close()
				conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				done := make(chan struct{})
				go func() { io.Copy(upstream, reader); close(done) }()
				io.Copy(conn, upstream)
				conn.Close()
				<-done
			}()
		}
	}()
	p := testPool(t, origin.URL)
	id := addNode(t, p, "socks", "socks5h://user:pass@"+listener.Addr().String())
	setPolicy(t, p, "manual", id)
	waitProbe(t, p)
	if p.State().Nodes[1].Health.Status != "healthy" {
		t.Fatalf("SOCKS probe failed: %#v", p.State().Nodes[1].Health)
	}
	select {
	case destination := <-seen:
		if destination != u.Host {
			t.Fatalf("wrong destination %s", destination)
		}
	case <-time.After(time.Second):
		t.Fatal("no SOCKS connect")
	}
	p.Close()
	listener.Close()
	workers.Wait()
}

func TestProbeCannotUndoLaterRequestFailure(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var startedOnce, releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/salcara-hub/statusz" {
			startedOnce.Do(func() { close(started) })
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			io.WriteString(w, `{"status":"available"}`)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	registerTestOrigin(t, origin)
	p := testPool(t, origin.URL)
	p.Probe()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("probe did not start")
	}
	// Add the backup while the first probe is in flight; the new route was not
	// included in that probe pass and cannot affect its deterministic ordering.
	w := controlRequest(p, "/v1/nodes", map[string]any{"expected_revision": p.State().Revision, "node": map[string]any{"name": "backup", "proxy_url": "", "enabled": true}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	backup := p.State().Nodes[1].ID
	setPolicy(t, p, "failover", "direct")
	edge := httptest.NewServer(p.Handler())
	t.Cleanup(edge.Close)
	response, err := http.Get(edge.URL + "/salcara-hub/v1/ping")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 502 || p.State().ActiveID != backup {
		t.Fatal("actual request failure did not move to backup")
	}
	releaseOnce.Do(func() { close(release) })
	deadline := time.Now().Add(time.Second)
	for p.State().ProbeRunning {
		if time.Now().After(deadline) {
			t.Fatal("probe did not complete")
		}
		time.Sleep(time.Millisecond)
	}
	state := p.State()
	if state.ActiveID != backup || state.Nodes[0].Health.Status != "unhealthy" {
		t.Fatalf("stale probe success revived the failed route: active=%s health=%s", state.ActiveID, state.Nodes[0].Health.Status)
	}
}

type blockedManagementWriter struct {
	header  http.Header
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (w *blockedManagementWriter) Header() http.Header { return w.header }
func (w *blockedManagementWriter) WriteHeader(int)     {}
func (w *blockedManagementWriter) Write(body []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(body), nil
}

func TestSlowManagementResponseDoesNotHoldRoutingLock(t *testing.T) {
	origin := healthyOrigin(t)
	p := testPool(t, origin.URL)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	writer := &blockedManagementWriter{header: make(http.Header), entered: make(chan struct{}), release: release}
	body, _ := json.Marshal(map[string]any{"expected_revision": p.State().Revision, "mode": "failover", "selected_id": "direct"})
	r := httptest.NewRequest("POST", "/v1/policy", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testToken)
	managementDone := make(chan struct{})
	go func() { p.ControlHandler().ServeHTTP(writer, r); close(managementDone) }()
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("management response did not begin")
	}
	edge := httptest.NewServer(p.Handler())
	t.Cleanup(edge.Close)
	requestDone := make(chan error, 1)
	go func() {
		response, err := http.Get(edge.URL + "/salcara-hub/v1/ping")
		if err == nil {
			response.Body.Close()
			if response.StatusCode != 200 {
				err = fmt.Errorf("unexpected response %d", response.StatusCode)
			}
		}
		requestDone <- err
	}()
	select {
	case err := <-requestDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Error("slow management response blocked a new business request")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-managementDone:
	case <-time.After(time.Second):
		t.Fatal("management did not finish")
	}
}

func TestPendingConfigCommitKeepsRoutingResponsiveAndCloseWaits(t *testing.T) {
	origin := healthyOrigin(t)
	p := testPool(t, origin.URL)
	revision := p.State().Revision
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	type result struct {
		status int
		code   string
	}
	committed := make(chan result, 1)
	go func() {
		_, status, code := p.commitConfig(&revision, func(next *savedConfig) string { close(entered); <-release; next.Mode = "failover"; return "" })
		committed <- result{status, code}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("configuration commit did not begin")
	}
	// A pending durable transaction must leave the previous routing snapshot
	// usable. The HTTP response and the file lock have separate lifetimes.
	stateReady := make(chan State, 1)
	go func() { stateReady <- p.State() }()
	select {
	case state := <-stateReady:
		if state.Revision != revision {
			t.Fatal("uncommitted revision became visible")
		}
	case <-time.After(time.Second):
		t.Fatal("configuration transaction held routing mutex")
	}
	closing := make(chan struct{})
	go func() { p.Close(); close(closing) }()
	select {
	case <-closing:
		t.Fatal("Close released storage during a pending commit")
	case <-time.After(50 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case result := <-committed:
		if result.status != 200 || result.code != "" {
			t.Fatalf("commit was lost: %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("commit did not finish")
	}
	select {
	case <-closing:
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after commit")
	}
	reopened, err := New(Config{Origin: origin.URL, DataDir: p.cfg.DataDir, ControlToken: testToken, DisablePeriodicProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.State().Revision != revision+1 || reopened.State().Mode != "failover" {
		t.Fatal("Close lost the durable committed configuration")
	}
}
