package launcher

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type fakeRunner struct {
	mu          sync.Mutex
	stops       int
	starts      []string
	failVersion string
}

func (p *fakeRunner) Stop() error { p.mu.Lock(); defer p.mu.Unlock(); p.stops++; return nil }
func (p *fakeRunner) Start(_ context.Context, r release) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.starts = append(p.starts, r.Version)
	if r.Version == p.failVersion {
		return errors.New("fixture unhealthy")
	}
	return nil
}
func (p *fakeRunner) counts() (int, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stops, append([]string(nil), p.starts...)
}
func fixtureFeed(t *testing.T, private ed25519.PrivateKey, payload any) []byte {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(envelope{Schema: 1, Publisher: PublisherID, Payload: base64.StdEncoding.EncodeToString(raw), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(private, raw))})
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func testFeed(data []byte) Feed {
	hash := sha256.Sum256(data)
	b := Binary{URL: "https://updates.example.com/hub-amd64", SHA256: hex.EncodeToString(hash[:]), Size: int64(len(data))}
	arm := b
	arm.URL = "https://updates.example.com/hub-arm64"
	return Feed{Product: Product, Version: "0.4.0", LauncherProtocol: 1, DataSchema: 1, ReleaseNotes: "fixture release", Binaries: map[string]Binary{"linux/amd64": b, "linux/arm64": arm}}
}

type fixture struct {
	manager  *manager
	store    *store
	runner   *fakeRunner
	private  ed25519.PrivateKey
	public   ed25519.PublicKey
	feed     []byte
	binary   []byte
	requests int
	cancel   context.CancelFunc
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bootstrap := filepath.Join(dir, "bootstrap")
	if err = os.WriteFile(bootstrap, []byte("trusted image fixture"), 0500); err != nil {
		t.Fatal(err)
	}
	cfg := Config{DataDir: filepath.Join(dir, "data"), Listen: ":8787", FeedURL: "https://updates.example.com/feed.json", BootstrapPath: bootstrap, BootstrapVersion: "0.4.0-dev"}
	s, err := newStore(cfg, pub)
	if err != nil {
		t.Fatal(err)
	}
	s.platform = "linux/amd64"
	state, err := s.load()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runner := &fakeRunner{}
	m := newManager(ctx, cfg, s, state, runner)
	m.key = pub
	m.ready = true
	f := &fixture{manager: m, store: s, runner: runner, private: priv, public: pub, binary: []byte("verified fixture executable"), cancel: cancel}
	f.feed = fixtureFeed(t, priv, testFeed(f.binary))
	m.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		f.requests++
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("credentials forwarded to update host")
		}
		var data []byte
		if r.URL.Path == "/feed.json" {
			data = f.feed
		} else {
			data = f.binary
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), ContentLength: int64(len(data)), Body: io.NopCloser(bytes.NewReader(data))}, nil
	})}
	t.Cleanup(func() { cancel(); m.job.Wait() })
	return f
}
func TestSignedFeedStrictnessAndProtocol(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	f := testFeed([]byte("body"))
	raw := fixtureFeed(t, priv, f)
	if got, err := verifyFeed(raw, pub); err != nil || got.Version != "0.4.0" {
		t.Fatal("valid fixture signature rejected", err)
	}
	if _, err := verifyFeed(raw, fixedPublicKey()); err == nil {
		t.Fatal("production publisher accepted arbitrary test key")
	}
	for name, alter := range map[string]func(*Feed){"schema": func(f *Feed) { f.DataSchema = 2 }, "launcher": func(f *Feed) { f.LauncherProtocol = 2 }, "size": func(f *Feed) {
		b := f.Binaries["linux/amd64"]
		b.Size = maxBinaryBytes + 1
		f.Binaries["linux/amd64"] = b
	}, "http": func(f *Feed) {
		b := f.Binaries["linux/amd64"]
		b.URL = "http://updates.example.com/a"
		f.Binaries["linux/amd64"] = b
	}, "missing platform": func(f *Feed) { delete(f.Binaries, "linux/arm64") }} {
		t.Run(name, func(t *testing.T) {
			candidate := testFeed([]byte("body"))
			alter(&candidate)
			if _, err := verifyFeed(fixtureFeed(t, priv, candidate), pub); err == nil {
				t.Fatal("unsafe feed accepted")
			}
		})
	}
	var e envelope
	json.Unmarshal(raw, &e)
	e.Payload = base64.StdEncoding.EncodeToString([]byte(`{"version":"99.0.0"}`))
	tampered, _ := json.Marshal(e)
	if _, err := verifyFeed(tampered, pub); err == nil {
		t.Fatal("tampered payload accepted")
	}
	if strictDecode([]byte(`{"schema_version":1,"schema_version":1}`), new(envelope)) == nil {
		t.Fatal("duplicate keys accepted")
	}
	if strictDecode(append(raw, []byte(" {}")...), new(envelope)) == nil {
		t.Fatal("trailing JSON accepted")
	}
	if strictDecode([]byte(`{"unknown":true}`), new(envelope)) == nil {
		t.Fatal("unknown fields accepted")
	}
	if strictDecode([]byte(strings.Repeat("[", 70)+"0"+strings.Repeat("]", 70)), new(any)) == nil {
		t.Fatal("excessive depth accepted")
	}
}
func TestSemanticVersionBoundsAndOrdering(t *testing.T) {
	for _, v := range []string{"v0.4.0", "0.4.0+build", "01.4.0", "4294967296.0.0", "0.4.0-01", "0.4.0-18446744073709551616"} {
		if _, err := parseVersion(v); err == nil {
			t.Fatalf("unsafe version accepted: %s", v)
		}
	}
	for _, pair := range [][2]string{{"0.4.0", "0.4.0-dev"}, {"0.4.1", "0.4.0"}, {"0.4.0-rc.10", "0.4.0-rc.2"}, {"1.0.0", "0.99.0"}} {
		if !newer(pair[0], pair[1]) || newer(pair[1], pair[0]) {
			t.Fatal("incorrect version ordering", pair)
		}
	}
}
func TestPublicNetworkingAndPinnedDNS(t *testing.T) {
	for _, u := range []string{"http://example.com/a", "https://localhost/a", "https://relay/a", "https://10.0.0.1/a", "https://127.0.0.1/a", "https://[::1]/a", "https://[2001:db8::1]/a", "https://100.64.0.1/a", "https://user:pass@example.com/a", "https://example.com:8443/a", "https://example.com/a#x"} {
		if _, err := validateURL(u); err == nil {
			t.Fatalf("unsafe URL accepted: %s", u)
		}
	}
	for _, ip := range []string{"127.0.0.1", "192.168.0.1", "169.254.169.254", "192.0.2.1", "100.64.0.1", "2001:db8::1", "::ffff:127.0.0.1", "fc00::1"} {
		if publicIP(netip.MustParseAddr(ip)) {
			t.Fatal("non-public IP accepted", ip)
		}
	}
	if !publicIP(netip.MustParseAddr("8.8.8.8")) {
		t.Fatal("public fixture IP rejected")
	}
	dialed := false
	lookup := func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}, nil
	}
	_, err := dialPublic(context.Background(), "tcp", "updates.example.com:443", lookup, func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, errors.New("fixture")
	})
	if err == nil || dialed {
		t.Fatal("mixed public/private DNS dialed")
	}
	lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	_, _ = dialPublic(context.Background(), "tcp", "updates.example.com:443", lookup, func(_ context.Context, _ string, address string) (net.Conn, error) {
		if address != "8.8.8.8:443" {
			t.Error("DNS was not pinned")
		}
		return nil, errors.New("fixture")
	})
	client := updateClient()
	r, _ := http.NewRequest("GET", "https://cdn.example.com/a", nil)
	r.Header.Set("Authorization", "fixture-not-real")
	r.Header.Set("Cookie", "fixture")
	if err = client.CheckRedirect(r, []*http.Request{{}}); err != nil || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
		t.Fatal("redirect preserved credentials")
	}
	r, _ = http.NewRequest("GET", "https://127.0.0.1/a", nil)
	if client.CheckRedirect(r, []*http.Request{{}}) == nil {
		t.Fatal("private redirect accepted")
	}
}
func acceptedFixture(t *testing.T, f *fixture) (Status, func(bool)) {
	t.Helper()
	if f.requests != 0 {
		t.Fatal("status performed unsolicited network request")
	}
	if f.manager.snapshot().CurrentVersion != "0.4.0-dev" {
		t.Fatal("wrong bootstrap status")
	}
	s := f.manager.check()
	if s.Status != "available" {
		t.Fatal("fixture update not available", s.Status)
	}
	s, code, start := f.manager.apply(applyRequest{Version: s.LatestVersion, SHA256: s.SHA256, Confirm: true})
	if code != 202 || start == nil || s.Status != "updating" || s.JobID == "" {
		t.Fatal("update was not asynchronously accepted")
	}
	return s, start
}
func TestUpdateReceiptBarrierAndSuccessfulRestore(t *testing.T) {
	f := newFixture(t)
	_, start := acceptedFixture(t, f)
	time.Sleep(10 * time.Millisecond)
	if stops, _ := f.runner.counts(); stops != 0 {
		t.Fatal("child stopped before accepted response was flushed")
	}
	_, code, again := f.manager.apply(applyRequest{Version: "0.4.0", Confirm: true})
	if code != 409 || again != nil {
		t.Fatal("concurrent update accepted")
	}
	start(true)
	f.manager.job.Wait()
	s := f.manager.snapshot()
	if s.Status != "current" || s.CurrentVersion != "0.4.0" {
		t.Fatal("successful transaction state wrong", s)
	}
	stops, starts := f.runner.counts()
	if stops != 1 || len(starts) != 1 || starts[0] != "0.4.0" {
		t.Fatal("unexpected process transaction", stops, starts)
	}
	state, err := f.store.load()
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.store.resolve(state.Current)
	if err != nil || r.Version != "0.4.0" {
		t.Fatal("signed confirmed release did not restore", err)
	}
	if state.Previous == nil || !state.Previous.Bootstrap {
		t.Fatal("old image reference not preserved")
	}
	if err = os.Chmod(r.Path, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(r.Path, []byte("tampered executable fixture"), 0500); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.load(); err == nil {
		t.Fatal("restore trusted tampered cached executable")
	}
}
func TestReceiptFailureDoesNotStopChild(t *testing.T) {
	f := newFixture(t)
	_, start := acceptedFixture(t, f)
	start(false)
	f.manager.job.Wait()
	if stops, _ := f.runner.counts(); stops != 0 {
		t.Fatal("response failure stopped child")
	}
	if f.manager.snapshot().Status != "failed" {
		t.Fatal("failed receipt reported successful")
	}
}
func TestInvalidDownloadOrChangedFeedKeepsChild(t *testing.T) {
	for _, kind := range []string{"hash", "feedchanged", "signature", "schema"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			_, start := acceptedFixture(t, f)
			switch kind {
			case "hash":
				f.binary = []byte("wrong size fixture")
			case "feedchanged":
				candidate := testFeed(f.binary)
				candidate.Version = "0.4.1"
				f.feed = fixtureFeed(t, f.private, candidate)
			case "signature":
				f.feed = []byte(`{"schema_version":1}`)
			case "schema":
				candidate := testFeed(f.binary)
				candidate.DataSchema = 2
				f.feed = fixtureFeed(t, f.private, candidate)
			}
			start(true)
			f.manager.job.Wait()
			if stops, _ := f.runner.counts(); stops != 0 {
				t.Fatal("unverified release stopped child")
			}
			if f.manager.snapshot().Status != "failed" {
				t.Fatal("invalid release reported success")
			}
		})
	}
}
func TestHealthFailureRollsBackApplicationOnly(t *testing.T) {
	f := newFixture(t)
	keep := filepath.Join(f.manager.cfg.DataDir, "pairing-fixture.json")
	original := []byte(`{"fixture":"pairing data preserved"}`)
	if err := os.WriteFile(keep, original, 0600); err != nil {
		t.Fatal(err)
	}
	f.runner.failVersion = "0.4.0"
	_, start := acceptedFixture(t, f)
	start(true)
	f.manager.job.Wait()
	s := f.manager.snapshot()
	if s.Status != "failed" || s.CurrentVersion != "0.4.0-dev" || !strings.Contains(s.Message, "已回退") {
		t.Fatal("rollback falsely reported", s)
	}
	_, starts := f.runner.counts()
	if len(starts) != 2 || starts[1] != "0.4.0-dev" {
		t.Fatal("old confirmed program not restarted", starts)
	}
	raw, err := os.ReadFile(keep)
	if err != nil || !bytes.Equal(raw, original) {
		t.Fatal("updater changed pairing file")
	}
	state, err := f.store.load()
	if err != nil || !state.Current.Bootstrap {
		t.Fatal("rollback pointer not bootstrap", err)
	}
}
func TestDiskPointersFailClosed(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.store.dir, "state.json")
	for _, raw := range []string{`{"schema_version":1,"current":{"path":"/bin/sh"}}`, `{"schema_version":1,"current":{"signed_feed":"AA=="}}`, `{"schema_version":2,"current":{"bootstrap":true}}`} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.load(); err == nil {
			t.Fatal("arbitrary/unsigned disk reference accepted")
		}
	}
}
func TestPrivateControlAndShutdownSeal(t *testing.T) {
	f := newFixture(t)
	secret := strings.Repeat("a", 64)
	handler := &control{manager: f.manager, tokenDigest: sha256.Sum256([]byte(secret))}
	request := func(method, path, body, auth string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if request("GET", "/status", "", "").Code != 401 {
		t.Fatal("control accepted unauthenticated request")
	}
	if request("GET", "/status", "", "Bearer "+secret).Code != 200 {
		t.Fatal("control rejected independent token")
	}
	if f.requests != 0 {
		t.Fatal("status made network request")
	}
	for _, body := range []string{`{"url":"https://example.com"}`, `{} {}`, `null`, `[]`} {
		if request("POST", "/check", body, "Bearer "+secret).Code != 400 {
			t.Fatal("check accepted caller URL/invalid body")
		}
	}
	for _, body := range []string{`{"version":"0.4.0","sha256":"x","confirm":false}`, `{"version":"0.4.0","sha256":"x","confirm":true,"url":"https://example.com"}`} {
		if request("POST", "/apply", body, "Bearer "+secret).Code != 400 {
			t.Fatal("apply accepted unknown fields or missing confirmation")
		}
	}
	f.manager.check()
	f.manager.mu.Lock()
	f.manager.closed = true
	f.manager.ready = false
	f.manager.mu.Unlock()
	s := f.manager.snapshot()
	_, code, start := f.manager.apply(applyRequest{Version: s.LatestVersion, SHA256: s.SHA256, Confirm: true})
	if code != 503 || start != nil {
		t.Fatal("shutdown accepted new job")
	}
	f.manager.job.Wait()
}
