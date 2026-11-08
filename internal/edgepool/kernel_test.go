package edgepool

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestKernelRejectsPrivateOutboundAndUnknownOptions(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "::1"} {
		if _, err := pinKernelProxy(context.Background(), map[string]any{"type": "trojan", "server": host, "port": 443, "password": "secret"}); err == nil {
			t.Fatalf("private outbound accepted: %s", host)
		}
	}
	before := map[string]any{"type": "trojan", "server": "proxy.example", "port": 443, "password": "secret", "skip-cert-verify": true}
	if _, reason := normalizeKernelProxy(before); reason != "insecure_tls" {
		t.Fatal("unsafe TLS option not rejected")
	}
}

// Opt-in real-core integration: the caller supplies a locally verified official
// binary; the ordinary suite never downloads or silently requires an executable.
func TestRealMihomoFixedListeners(t *testing.T) {
	binary := os.Getenv("SALCARA_TEST_MIHOMO")
	if binary == "" {
		t.Skip("set SALCARA_TEST_MIHOMO for actual core integration")
	}
	raw, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := newKernelManager(privateDataDir(t), ctx)
	defer m.Close()
	if err = writeSourcePrivate(m.binary(), raw); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(m.binary(), 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	manifest, _ := json.Marshal(kernelInstallation{kernelVersion, hex.EncodeToString(sum[:])})
	if err = writeSourcePrivate(filepath.Join(m.dir, "installation.json"), manifest); err != nil {
		t.Fatal(err)
	}
	m.installed = true
	// Local fixture dialing is injected only here. Production always pins public
	// DNS results and rejects private outbound endpoints.
	m.pin = func(_ context.Context, v map[string]any) (map[string]any, error) {
		copy := map[string]any{}
		for k, x := range v {
			copy[k] = x
		}
		return copy, nil
	}
	origin := healthyOrigin(t)
	var countA, countB atomic.Int32
	proxyA := testHTTPProxy(t, origin.URL, &countA)
	proxyB := testHTTPProxy(t, origin.URL, &countB)
	node := func(raw string) map[string]any {
		u, _ := url.Parse(raw)
		port, _ := strconv.Atoi(u.Port())
		return map[string]any{"type": "http", "server": u.Hostname(), "port": port}
	}
	generation, err := m.Prepare(ctx, map[string]map[string]any{"one": node(proxyA.URL), "two": node(proxyB.URL)})
	if err != nil {
		t.Fatal(err)
	}
	m.Activate(generation)
	roots := x509.NewCertPool()
	roots.AddCert(origin.Certificate())
	var wg sync.WaitGroup
	failures := make(chan error, 12)
	for key := range generation.URLs {
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func(key string) {
				defer wg.Done()
				address, _ := url.Parse(generation.URLs[key])
				tr := &http.Transport{Proxy: http.ProxyURL(address), TLSClientConfig: &tls.Config{RootCAs: roots}, DisableKeepAlives: true}
				defer tr.CloseIdleConnections()
				client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
				resp, err := client.Get(origin.URL + "/salcara-hub/statusz")
				if err != nil {
					failures <- err
					return
				}
				defer resp.Body.Close()
				_, err = io.Copy(io.Discard, resp.Body)
				if err != nil {
					failures <- err
				}
			}(key)
		}
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if countA.Load() != 6 || countB.Load() != 6 {
		t.Fatalf("listener mapping mixed concurrent requests: %d/%d", countA.Load(), countB.Load())
	}
	noAuth, _ := url.Parse(generation.URLs["one"])
	noAuth.User = nil
	tr := &http.Transport{Proxy: http.ProxyURL(noAuth), TLSClientConfig: &tls.Config{RootCAs: roots}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 2 * time.Second}
	if resp, err := client.Get(origin.URL); err == nil {
		resp.Body.Close()
		t.Fatal("local core listener accepted missing credentials")
	}
	bad := map[string]map[string]any{"broken": {"type": "not-a-protocol", "server": "127.0.0.1", "port": 1}}
	if _, err = m.Prepare(ctx, bad); err == nil {
		t.Fatal("invalid core candidate accepted")
	}
	if m.active != generation.ID {
		t.Fatal("bad candidate replaced active generation")
	}
	entries, _ := filepath.Glob(filepath.Join(m.dir, "generation-*"))
	if len(entries) != 1 {
		t.Fatalf("failed candidate left runtime credentials: %d", len(entries))
	}
	// The process supervisor restores the exact listeners after a crash.
	_ = generation.cmd.Process.Kill()
	<-generation.done
	m.reap()
	if _, err := kernelControl(ctx, generation, "/version"); err != nil {
		t.Fatal("core did not recover after crash", err)
	}
	// Exercise an actual encrypted protocol, in addition to HTTP listener
	// isolation, against a local Shadowsocks server implemented by the core.
	ssListener, ssPort, err := reserveKernelPort()
	if err != nil {
		t.Fatal(err)
	}
	ssDir := privateDataDir(t)
	ssConfig := map[string]any{"mode": "rule", "log-level": "silent", "rules": []string{"MATCH,DIRECT"}, "listeners": []any{map[string]any{"name": "ss-fixture", "type": "shadowsocks", "listen": "127.0.0.1", "port": ssPort, "cipher": "aes-128-gcm", "password": "test-only-key"}}}
	ssRaw, _ := json.Marshal(ssConfig)
	if err := writeSourcePrivate(filepath.Join(ssDir, "config.json"), ssRaw); err != nil {
		t.Fatal(err)
	}
	ssListener.Close()
	ssCmd := kernelCommand(ctx, m.binary(), "-d", ssDir, "-f", filepath.Join(ssDir, "config.json"))
	if err := ssCmd.Start(); err != nil {
		t.Fatal(err)
	}
	ssDone := make(chan struct{})
	go func() { _ = ssCmd.Wait(); close(ssDone) }()
	defer func() { _ = ssCmd.Process.Kill(); <-ssDone }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(ssPort)), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("SS fixture failed to listen")
		}
		time.Sleep(20 * time.Millisecond)
	}
	second, err := m.Prepare(ctx, map[string]map[string]any{"one": {"type": "ss", "server": "127.0.0.1", "port": ssPort, "cipher": "aes-128-gcm", "password": "test-only-key"}})
	if err != nil {
		t.Fatal(err)
	}
	ssProxy, _ := url.Parse(second.URLs["one"])
	ssTransport := &http.Transport{Proxy: http.ProxyURL(ssProxy), TLSClientConfig: &tls.Config{RootCAs: roots}, DisableKeepAlives: true}
	defer ssTransport.CloseIdleConnections()
	ssClient := &http.Client{Transport: ssTransport, Timeout: 5 * time.Second}
	ssResponse, err := ssClient.Get(origin.URL + "/salcara-hub/statusz")
	if err != nil {
		t.Fatal("real SS transport failed", err)
	}
	io.Copy(io.Discard, ssResponse.Body)
	ssResponse.Body.Close()
	if ssResponse.StatusCode != 200 {
		t.Fatal("real SS transport wrong response")
	}
	m.Activate(second)
	m.reap()
	if _, err := os.Stat(filepath.Join(m.dir, "generation-"+generation.ID)); !os.IsNotExist(err) {
		t.Fatal("drained generation credentials retained")
	}
	restored, created, err := m.Restore(ctx, generation)
	if err != nil || !created {
		t.Fatal("history generation could not be reconstructed", err)
	}
	m.Activate(restored)
	m.ReleaseRestore(restored)
	m.reap()
	m.Close()
	select {
	case <-restored.done:
	default:
		t.Fatal("Close returned before process reaping")
	}
	entries, _ = filepath.Glob(filepath.Join(m.dir, "generation-*"))
	if len(entries) != 0 {
		t.Fatal("Close retained plaintext generation configs")
	}
}

func TestKernelStateDoesNotWaitForPreparation(t *testing.T) {
	m := newKernelManager(privateDataDir(t), context.Background())
	defer m.Close()
	raw := []byte("fixture never executed")
	if err := writeSourcePrivate(m.binary(), raw); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(m.binary(), 0700); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	manifest, _ := json.Marshal(kernelInstallation{kernelVersion, hex.EncodeToString(hash[:])})
	if err := writeSourcePrivate(filepath.Join(m.dir, "installation.json"), manifest); err != nil {
		t.Fatal(err)
	}
	m.installed = true
	entered := make(chan struct{})
	m.pin = func(ctx context.Context, _ map[string]any) (map[string]any, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = m.Prepare(context.Background(), map[string]map[string]any{"node": {}})
	}()
	<-entered
	view := make(chan KernelView, 1)
	go func() { view <- m.View() }()
	select {
	case <-view:
	case <-time.After(time.Second):
		t.Fatal("state blocked on node DNS")
	}
	m.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel preparation")
	}
}
