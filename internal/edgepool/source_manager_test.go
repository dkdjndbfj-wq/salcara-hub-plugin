package edgepool

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func waitSourceIdle(t *testing.T, m *sourceManager) SourcesState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s := m.snapshot()
		if !s.Busy {
			return s
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("source job did not finish")
	return SourcesState{}
}
func addSourceFixture(t *testing.T, p *Pool) string {
	t.Helper()
	m := p.sources
	if err := m.save(m.snapshot().SourceRevision, sourceRecord{Name: "Provider", URL: "https://subscription.example/list?token=source-private-secret", IntervalMS: 21600000}); err != nil {
		t.Fatal(err)
	}
	return m.snapshot().Sources[0].ID
}
func candidateFixture(t *testing.T, m *sourceManager, id, raw string) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.index(id)
	candidate, err := m.parse([]byte(raw), m.saved.Sources[i].Active)
	if err != nil {
		t.Fatal(err)
	}
	next := m.clone()
	next.Sources[i].Candidate = candidate
	next.Sources[i].Status = "review"
	if err = m.persistLocked(next); err != nil {
		t.Fatal(err)
	}
}

func TestSourceRegistryEncryptionPrivacyAndCAS(t *testing.T) {
	p := testPool(t, "https://hub.example.com")
	id := addSourceFixture(t, p)
	m := p.sources
	candidateFixture(t, m, id, validSubscription)
	data, err := os.ReadFile(filepath.Join(p.cfg.DataDir, "sources.enc"))
	if err != nil {
		t.Fatal(err)
	}
	public, _ := json.Marshal(m.snapshot())
	for _, secret := range []string{"source-private-secret", "node-secret", "secret-value", "subscription.example"} {
		if strings.Contains(string(data), secret) || strings.Contains(string(public), secret) {
			t.Fatalf("source secret leaked: %s", secret)
		}
	}
	before := m.snapshot().SourceRevision
	if err = m.save(before-1, sourceRecord{ID: id, Name: "bad", IntervalMS: 21600000}); err == nil || m.snapshot().Sources[0].Name != "Provider" {
		t.Fatal("stale source draft applied")
	}
	if err = m.save(before, sourceRecord{ID: id, Name: "Renamed", IntervalMS: 21600000}); err != nil {
		t.Fatal(err)
	}
	dir := p.cfg.DataDir
	p.Close()
	again, err := New(Config{Origin: "https://hub.example.com", DataDir: dir, ControlToken: testToken, DisablePeriodicProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	again.sources.mu.Lock()
	defer again.sources.mu.Unlock()
	if again.sources.saved.Sources[0].URL != "https://subscription.example/list?token=source-private-secret" {
		t.Fatal("omitted URL was not preserved")
	}
}
func TestSourceStableIdentityCredentialsAndUnsupportedFields(t *testing.T) {
	m := &sourceManager{}
	before, err := m.parse([]byte("socks5://alice:old@proxy.example:1080#First"), nil)
	if err != nil {
		t.Fatal(err)
	}
	after, err := m.parse([]byte("socks5://alice:new@proxy.example:1080#Renamed"), before.Nodes)
	if err != nil {
		t.Fatal(err)
	}
	if after.Nodes[0].Key != before.Nodes[0].Key || after.Nodes[0].Change != "updated" || !after.Nodes[0].Selected {
		t.Fatal("rotation lost identity")
	}
	if _, err = m.parse([]byte("socks5://alice:old@proxy.example:1080#One\nsocks5://alice:new@proxy.example:1080#Two"), nil); err == nil || err.Error() != "source_identity_ambiguous" {
		t.Fatal("ambiguous accounts were silently merged")
	}
	for _, extra := range []string{"    skip-cert-verify: true\n", "    dialer-proxy: DIRECT\n", "    certificate: /private/key\n", "    interface-name: eth0\n"} {
		nodes, err := parseSourceNodes([]byte("proxies:\n  - name: unsafe\n    type: trojan\n    server: proxy.example\n    port: 443\n    password: secret\n" + extra))
		if err != nil || len(nodes) != 1 || nodes[0].Supported {
			t.Fatalf("unsafe option accepted: %s", extra)
		}
	}
	nodes, err := parseSourceNodes([]byte(validSubscription))
	if err != nil || len(nodes) != 2 || !nodes[1].Supported || nodes[1].Reason != "managed_kernel" {
		t.Fatalf("managed protocol not classified: %v %+v", err, nodes)
	}
}
func TestSourceRefreshFailureRetainsCandidateAndRedactsError(t *testing.T) {
	p := testPool(t, "https://hub.example.com")
	id := addSourceFixture(t, p)
	m := p.sources
	candidateFixture(t, m, id, "socks5://alice:secret@proxy.example:1080")
	before := m.snapshot().Sources[0].CandidateID
	inspector, _ := inspectorFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503); io.WriteString(w, "secret response") }))
	p.subscriptions.client = inspector.client
	m.mu.Lock()
	m.saved.Sources[0].URL = "https://example.com/list?token=source-private-secret"
	m.mu.Unlock()
	if err := m.refresh(id); err != nil {
		t.Fatal(err)
	}
	after := waitSourceIdle(t, m)
	if after.Sources[0].CandidateID != before || after.Sources[0].Error != "subscription_fetch_failed" || after.Sources[0].NextRefresh <= time.Now().UnixMilli() {
		t.Fatalf("failed refresh destroyed prior candidate: %+v", after)
	}
}

func TestManagedURINeverSilentlyDropsConnectionOptions(t *testing.T) {
	base := map[string]any{"v": "2", "add": "proxy.example", "port": "443", "id": "12345678-1234-1234-1234-123456789abc", "aid": "0", "net": "ws", "type": "none", "tls": "tls", "host": "proxy.example", "path": "/ws"}
	uri := func(v map[string]any) string {
		raw, _ := json.Marshal(v)
		return "vmess://" + base64.StdEncoding.EncodeToString(raw)
	}
	for key, value := range map[string]any{"aid": "bad", "tls": "reality", "net": true, "type": "http", "allowInsecure": true, "packetEncoding": "xudp"} {
		v := map[string]any{}
		for k, x := range base {
			v[k] = x
		}
		v[key] = value
		if _, err := parseManagedURI(uri(v)); err == nil {
			t.Fatalf("silently accepted %s", key)
		}
	}
	base["aid"] = float64(64)
	parsed, err := parseManagedURI(uri(base))
	if err != nil || parsed["alterId"] != 64 || parsed["tls"] != true {
		t.Fatal("valid VMess settings lost")
	}
	for _, raw := range []string{
		"vless://12345678-1234-1234-1234-123456789abc@proxy.example:443?security=reality",
		"trojan://secret@proxy.example:443?host=ignored.example",
		"trojan://secret@proxy.example:443?sni=a&peer=b",
		"trojan://secret@proxy.example:443?security=tls;insecure=1",
	} {
		if _, err := parseManagedURI(raw); err == nil {
			t.Fatal("unmapped URI fields accepted")
		}
	}
}

func TestSourceRemovalRetainsRollbackDependencies(t *testing.T) {
	p := testPool(t, "https://hub.example.com")
	id := addSourceFixture(t, p)
	p.mu.Lock()
	p.saved.History = []releaseVersion{{Snapshot: poolSnapshot{Nodes: []nodeConfig{{SourceID: id}}}}}
	p.mu.Unlock()
	if err := p.sources.remove(id, p.sources.snapshot().SourceRevision); err == nil || err.Error() != "source_in_use" {
		t.Fatal("deleted credentials referenced by history")
	}
	p.mu.Lock()
	p.saved.History = nil
	p.mu.Unlock()
	if err := p.sources.remove(id, p.sources.snapshot().SourceRevision); err != nil {
		t.Fatal(err)
	}
}

func TestSourcePublishRejectsEmptyRegionPolicy(t *testing.T) {
	p := testPool(t, "https://hub.example.com")
	m := p.sources
	m.probe = func(context.Context, *Pool, []nodeConfig) error { return nil }
	id := addSourceFixture(t, p)
	candidateFixture(t, m, id, "socks5://alice:secret@proxy.example:1080")
	p.mu.Lock()
	p.saved.Mode = "balance"
	p.saved.SelectionPolicy.RegionMode = "only"
	p.saved.SelectionPolicy.PreferredRegions = []string{"cn"}
	p.mu.Unlock()
	s := m.snapshot()
	rev := p.State().Revision
	if err := m.apply(id, s.Sources[0].CandidateID, s.SourceRevision, rev, []string{s.Sources[0].Nodes[0].Key}, false); err != nil {
		t.Fatal(err)
	}
	state := waitSourceIdle(t, m)
	if p.State().Revision != rev || state.Sources[0].Error != "no_eligible_route" {
		t.Fatal("source update published an unusable policy")
	}
}
func TestSourceApplyPreservesManualSettingsAndFailedCandidate(t *testing.T) {
	p := testPool(t, "https://hub.example.com")
	m := p.sources
	m.probe = func(context.Context, *Pool, []nodeConfig) error { return nil }
	id := addSourceFixture(t, p)
	candidateFixture(t, m, id, "socks5://alice:old@proxy.example:1080#Imported")
	s := m.snapshot()
	if err := m.apply(id, s.Sources[0].CandidateID, s.SourceRevision, p.State().Revision, []string{s.Sources[0].Nodes[0].Key}, false); err != nil {
		t.Fatal(err)
	}
	if waitSourceIdle(t, m).Sources[0].Status != "applied" {
		t.Fatal("initial candidate not applied")
	}
	var node Node
	for _, n := range p.State().Nodes {
		if n.SourceID == id {
			node = n
		}
	}
	if node.ID == "" {
		t.Fatal("no owned node")
	}
	w := controlRequest(p, "/v1/nodes", map[string]any{"expected_revision": p.State().Revision, "node": map[string]any{"id": node.ID, "name": "My override", "enabled": false, "weight": 7, "region": "香港"}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	candidateFixture(t, m, id, "socks5://alice:new@proxy.example:1080#Supplier rename")
	s = m.snapshot()
	if err := m.apply(id, s.Sources[0].CandidateID, s.SourceRevision, p.State().Revision, []string{s.Sources[0].Nodes[0].Key}, false); err != nil {
		t.Fatal(err)
	}
	waitSourceIdle(t, m)
	p.mu.Lock()
	for _, n := range p.saved.Nodes {
		if n.ID == node.ID {
			if n.Enabled || n.Name != "My override" || n.Weight != 7 || n.Region != "香港" || !strings.Contains(n.ProxyURL, ":new@") {
				t.Fatalf("manual metadata lost: %+v", n)
			}
		}
	}
	p.mu.Unlock()
	before := p.State().Revision
	m.probe = func(context.Context, *Pool, []nodeConfig) error { return errors.New("source_candidate_unreachable") }
	candidateFixture(t, m, id, "socks5://alice:third@proxy.example:1080#Next")
	s = m.snapshot()
	if err := m.apply(id, s.Sources[0].CandidateID, s.SourceRevision, before, []string{s.Sources[0].Nodes[0].Key}, false); err != nil {
		t.Fatal(err)
	}
	after := waitSourceIdle(t, m)
	if p.State().Revision != before || after.Sources[0].Error != "source_candidate_unreachable" || after.Sources[0].CandidateID == "" {
		t.Fatal("failed candidate changed active config")
	}
}
func TestSourcePublicationCASAndRestartReconciliation(t *testing.T) {
	p := testPool(t, "https://hub.example.com")
	m := p.sources
	id := addSourceFixture(t, p)
	candidateFixture(t, m, id, "socks5://alice:new@proxy.example:1080#Node")
	s := m.snapshot()
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce sync.Once
	m.probe = func(ctx context.Context, _ *Pool, _ []nodeConfig) error {
		enteredOnce.Do(func() { close(entered) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := m.apply(id, s.Sources[0].CandidateID, s.SourceRevision, p.State().Revision, []string{s.Sources[0].Nodes[0].Key}, false); err != nil {
		t.Fatal(err)
	}
	<-entered
	w := controlRequest(p, "/v1/policy", map[string]any{"expected_revision": p.State().Revision, "mode": "latency"})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	revision := p.State().Revision
	close(release)
	after := waitSourceIdle(t, m)
	if after.Sources[0].Error != "revision_conflict" || p.State().Revision != revision {
		t.Fatal("late source publisher overwrote new policy")
	}
	// Simulate the exact crash window: pool committed, encrypted candidate still
	// pending and its active metadata has not yet been finalized.
	m.mu.Lock()
	raw := m.saved.Sources[0].Candidate.Nodes[0]
	m.mu.Unlock()
	_, _, code := p.commitConfig(&revision, func(s *savedConfig) string {
		s.Nodes = append(s.Nodes, nodeConfig{ID: "owned", Name: "Owned", Enabled: true, Imported: true, ProxyURL: raw.Native, SourceID: id, SourceKey: raw.Key, Weight: 1, MaxRequests: 64, MaxStreams: 16})
		return ""
	})
	if code != "" {
		t.Fatal(code)
	}
	dir := p.cfg.DataDir
	p.Close()
	again, err := New(Config{Origin: "https://hub.example.com", DataDir: dir, ControlToken: testToken, DisablePeriodicProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	again.sources.mu.Lock()
	defer again.sources.mu.Unlock()
	if len(again.sources.saved.Sources[0].Active) != 1 || again.sources.saved.Sources[0].Active[0].Native != raw.Native {
		t.Fatal("restart did not recover committed source snapshot")
	}
}
