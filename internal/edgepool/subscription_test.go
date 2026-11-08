package edgepool

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

const validSubscription = `proxies:
  - name: Native SOCKS
    type: socks5
    server: proxy.example.com
    port: 1080
    username: node-user
    password: node-secret
  - name: Needs core
    type: trojan
    server: other.example.com
    port: 443
    password: secret-value
rules:
  - MATCH,DIRECT
dns:
  enable: true
`

func inspectorFixture(t *testing.T, handler http.Handler) (*SubscriptionInspector, *atomic.Int32) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	var dials atomic.Int32
	client := subscriptionClient(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		if address != "8.8.8.8:443" {
			t.Errorf("DNS address not pinned: %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	})
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: roots}
	inspector := NewSubscriptionInspector()
	inspector.client = client
	t.Cleanup(inspector.Close)
	return inspector, &dials
}

func TestSubscriptionFormatsAndCapabilityDetection(t *testing.T) {
	uri := "socks5://user:pass@proxy.example.com:1080#One\ntrojan://secret@other.example.com:443#Two"
	for _, test := range []struct {
		name, body, format string
		supported, total   int
	}{
		{"yaml", validSubscription, "clash_yaml", 1, 2},
		{"json", `{"proxies":[{"name":"A","type":"http","server":"proxy.example.com","port":443,"tls":true}]}`, "clash_json", 1, 1},
		{"yaml uppercase boolean", "proxies:\n - {type: http, server: proxy.example.com, port: 443, tls: TRUE}", "clash_yaml", 1, 1},
		{"uris", uri, "uri_list", 1, 2},
		{"base64", base64.StdEncoding.EncodeToString([]byte(uri)), "base64_uri_list", 1, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := parseSubscription([]byte(test.body))
			if err != nil {
				t.Fatal(err)
			}
			if parsed.format != test.format || len(parsed.configs) != test.supported || len(parsed.nodes) != test.total {
				t.Fatalf("incorrect capability counts: %#v", parsed)
			}
			for _, node := range parsed.configs {
				if !node.Imported {
					t.Fatal("missing imported trust boundary")
				}
			}
		})
	}
	parsed, _ := parseSubscription([]byte(validSubscription))
	if parsed.nodes[1].Reason != "requires_mihomo" || parsed.nodes[1].Supported {
		t.Fatal("complex protocol falsely supported")
	}
	tlsParsed, err := parseSubscription([]byte("proxies:\n - {type: http, server: proxy.example.com, port: 443, tls: TRUE}"))
	if err != nil || !strings.HasPrefix(tlsParsed.configs[0].ProxyURL, "https://") {
		t.Fatal("TLS boolean silently downgraded")
	}
}

func TestSubscriptionRejectsMalformedAndUnsafeConfigurations(t *testing.T) {
	for name, body := range map[string]string{
		"html":          "<!DOCTYPE html><html>login</html>",
		"alias":         "proxies: &nodes [{type: socks5, server: proxy.example.com, port: 1080}]\nagain: *nodes",
		"duplicate key": "proxies: []\nproxies: []",
		"deep":          "proxies: " + strings.Repeat("[", 30) + strings.Repeat("]", 30),
		"multi doc":     "proxies: []\n---\nproxies: []",
		"scalar":        "not a subscription",
		"broken json":   `{"proxies":[`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSubscription([]byte(body)); err == nil {
				t.Fatal("malformed subscription accepted")
			}
		})
	}
	parsed, err := parseSubscription([]byte("http://127.0.0.1:7890\nhttp://169.254.169.254:80\nsocks5://localhost:7890\nhttp://[::1]:1080\nhttp://proxy.example.com:8080\nhttp://proxy.example.com:8080"))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.configs) != 1 || parsed.nodes[0].Reason != "unsafe_proxy_address" || parsed.nodes[5].Reason != "duplicate_node" {
		t.Fatalf("unsafe or duplicate proxy imported: %#v", parsed.nodes)
	}
	parsed, err = parseSubscription([]byte("proxies:\n - {name: insecure, type: http, server: proxy.example.com, port: 443, tls: true, skip-cert-verify: true}\n - {name: tunnel, type: socks5, server: proxy.example.com, port: 443, tls: true}"))
	if err != nil || len(parsed.configs) != 0 {
		t.Fatal("unsupported TLS options imported")
	}
}

func TestSubscriptionNamesFitSavedUTF8ByteLimit(t *testing.T) {
	parsed, err := parseSubscription([]byte("proxies:\n - {name: " + strings.Repeat("中文", 90) + ", type: socks5, server: proxy.example.com, port: 1080}"))
	if err != nil || len(parsed.configs) != 1 {
		t.Fatal("valid unicode node rejected")
	}
	node := parsed.configs[0]
	if len(node.Name) > 160 || !utf8.ValidString(node.Name) {
		t.Fatal("name does not fit persisted UTF8 byte limit")
	}
	node.ID = "unicode"
	if err := validateSaved(savedConfig{Revision: 1, Mode: "manual", SelectedID: "unicode", Nodes: []nodeConfig{node}}); err != nil {
		t.Fatal("preview-compatible name failed save", err)
	}
}

func TestSubscriptionURLAndDNSProtection(t *testing.T) {
	for _, raw := range []string{"http://public.example/a", "https://u:p@public.example/a", "https://localhost/a", "https://metadata.google.internal/a#x", "https://127.0.0.1/a", "https://[::ffff:127.0.0.1]/a", "https://169.254.169.254/a", "https://10.0.0.2/a", "https://[fc00::1]/a", "https://public.example/a#fragment"} {
		if _, err := subscriptionURL(raw); err == nil {
			t.Errorf("unsafe subscription URL accepted: %s", raw)
		}
	}
	var dials atomic.Int32
	dial := func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("fixture")
	}
	for _, answers := range [][]netip.Addr{{netip.MustParseAddr("127.0.0.1")}, {netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.0.0.1")}, {netip.MustParseAddr("169.254.169.254")}} {
		_, err := dialPublicSubscription(context.Background(), "tcp", "public.example:443", func(context.Context, string, string) ([]netip.Addr, error) { return answers, nil }, dial)
		if err == nil {
			t.Fatal("unsafe DNS accepted")
		}
	}
	if dials.Load() != 0 {
		t.Fatal("unsafe DNS caused network dial")
	}
}

func TestImportedTransportRejectsPrivateProxyAtDial(t *testing.T) {
	var hits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer proxy.Close()
	for _, scheme := range []string{"http", "socks5h"} {
		u, _ := url.Parse(proxy.URL)
		u.Scheme = scheme
		transport := transportFor(u, false, true)
		req, _ := http.NewRequest("GET", "http://example.com/salcara-hub/statusz", nil)
		response, err := transport.RoundTrip(req)
		if err == nil {
			response.Body.Close()
			t.Fatal("imported proxy reached a private address")
		}
		transport.CloseIdleConnections()
	}
	if hits.Load() != 0 {
		t.Fatal("private proxy received traffic")
	}
}

func TestSubscriptionFetchRedirectsHeadersAndSize(t *testing.T) {
	var sawCredentials atomic.Bool
	inspector, dials := inspectorFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Proxy-Authorization") != "" {
			sawCredentials.Store(true)
		}
		switch r.URL.Path {
		case "/private":
			http.Redirect(w, r, "https://127.0.0.1/private", 302)
		case "/redirect":
			w.Header().Set("Set-Cookie", "secret=cookie")
			http.Redirect(w, r, "https://example.com/nodes", 302)
		case "/loop":
			http.Redirect(w, r, "https://example.com/loop", 302)
		case "/large":
			io.WriteString(w, strings.Repeat("a", subscriptionBytes+1))
		default:
			io.WriteString(w, validSubscription)
		}
	}))
	preview, err := inspector.Inspect(context.Background(), "https://example.com/redirect?token=subscription-secret")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(preview)
	for _, secret := range []string{"subscription-secret", "node-secret", "node-user", "secret-value", "https://example.com"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("secret leaked: %s", secret)
		}
	}
	before := dials.Load()
	if _, err = inspector.Inspect(context.Background(), "https://example.com/private"); err == nil {
		t.Fatal("private redirect followed")
	}
	if dials.Load() != before+1 {
		t.Fatal("private redirect dialed")
	}
	if _, err = inspector.Inspect(context.Background(), "https://example.com/loop"); err == nil {
		t.Fatal("unbounded redirects")
	}
	if _, err = inspector.Inspect(context.Background(), "https://example.com/large"); err == nil {
		t.Fatal("oversize accepted")
	}
	if sawCredentials.Load() {
		t.Fatal("credentials forwarded to subscription provider")
	}
}

func TestSubscriptionPreviewCacheExpiryAndConcurrency(t *testing.T) {
	inspector, _ := inspectorFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, validSubscription) }))
	now := time.Now()
	inspector.now = func() time.Time { return now }
	var first SubscriptionPreview
	for i := 0; i < 8; i++ {
		preview, err := inspector.Inspect(context.Background(), "https://example.com/nodes")
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = preview
		}
	}
	if _, err := inspector.Inspect(context.Background(), "https://example.com/nodes"); err == nil || err.Error() != "subscription_limit" {
		t.Fatal("cache limit missing")
	}
	now = now.Add(11 * time.Minute)
	if _, err := inspector.previewNodes(first.PreviewID); err == nil || err.Error() != "preview_expired" {
		t.Fatal("expired preview usable")
	}
	if _, err := inspector.Inspect(context.Background(), "https://example.com/nodes"); err != nil {
		t.Fatal("expired cache not reclaimed")
	}
	inspector.limit <- struct{}{}
	inspector.limit <- struct{}{}
	if _, err := inspector.Inspect(context.Background(), "https://example.com/nodes"); err == nil || err.Error() != "subscription_limit" {
		t.Fatal("concurrency unbounded")
	}
	<-inspector.limit
	<-inspector.limit
}

func TestSubscriptionImportIsAtomicAndConflictPreservesPreview(t *testing.T) {
	origin := healthyOrigin(t)
	p := testPool(t, origin.URL)
	p.subscriptions.Close()
	inspector, _ := inspectorFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, validSubscription) }))
	p.subscriptions = inspector
	w := controlRequest(p, "/v1/subscriptions/inspect", map[string]any{"url": "https://example.com/nodes?token=private"})
	if w.Code != 200 {
		t.Fatalf("inspect failed %d %s", w.Code, w.Body.String())
	}
	var preview SubscriptionPreview
	if json.Unmarshal(w.Body.Bytes(), &preview) != nil {
		t.Fatal("bad preview")
	}
	w = controlRequest(p, "/v1/subscriptions/import", map[string]any{"preview_id": preview.PreviewID, "expected_revision": 0})
	if w.Code != 409 {
		t.Fatal("stale revision accepted")
	}
	if _, err := inspector.previewNodes(preview.PreviewID); err != nil {
		t.Fatal("conflict consumed preview")
	}
	revision := p.State().Revision
	w = controlRequest(p, "/v1/subscriptions/import", map[string]any{"preview_id": preview.PreviewID, "expected_revision": revision})
	if w.Code != 200 {
		t.Fatalf("import failed %s", w.Body.String())
	}
	if len(p.State().Nodes) != 2 || p.State().Revision != revision+1 {
		t.Fatal("import not atomic")
	}
	if _, err := inspector.previewNodes(preview.PreviewID); err == nil {
		t.Fatal("success did not consume preview")
	}
	if strings.Contains(w.Body.String(), "node-secret") || strings.Contains(w.Body.String(), "node-user") {
		t.Fatal("import response leaked credentials")
	}
	p.mu.Lock()
	if !p.saved.Nodes[1].Imported {
		t.Error("subscription proxy lost restricted trust")
	}
	p.mu.Unlock()
}

func TestSubscriptionImportNoSupportedOrTooManyKeepsPool(t *testing.T) {
	p := testPool(t, "http://127.0.0.1:1")
	revision := p.State().Revision
	put := func(id string, nodes []nodeConfig) {
		p.subscriptions.mu.Lock()
		defer p.subscriptions.mu.Unlock()
		p.subscriptions.cache[id] = &subscriptionEntry{nodes: nodes, expires: time.Now().Add(time.Minute), timer: time.NewTimer(time.Minute)}
	}
	put("unsupported", nil)
	w := controlRequest(p, "/v1/subscriptions/import", map[string]any{"preview_id": "unsupported", "expected_revision": revision})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "subscription_no_supported_nodes") {
		t.Fatal("unsupported import allowed")
	}
	nodes := make([]nodeConfig, 32)
	for i := range nodes {
		nodes[i] = nodeConfig{Name: "proxy", ProxyURL: "http://proxy.example.com:" + strconvItoa(1000+i), Enabled: true, Imported: true}
	}
	put("many", nodes)
	w = controlRequest(p, "/v1/subscriptions/import", map[string]any{"preview_id": "many", "expected_revision": revision})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "node_limit") {
		t.Fatal("node limit not enforced")
	}
	if p.State().Revision != revision || len(p.State().Nodes) != 1 {
		t.Fatal("failed import partially changed pool")
	}
	if _, err := p.subscriptions.previewNodes("many"); err != nil {
		t.Fatal("failed import consumed preview")
	}
}

func TestSubscriptionImportExplicitIndexes(t *testing.T) {
	origin := healthyOrigin(t)
	p := testPool(t, origin.URL)
	parsed, err := parseSubscription([]byte("http://one.example.com:8080#One\ntrojan://secret@complex.example.com:443#Complex\nsocks5://two.example.com:1080#Two"))
	if err != nil {
		t.Fatal(err)
	}
	for i, node := range parsed.nodes {
		if node.Index != i {
			t.Fatal("preview lost original row index")
		}
	}
	p.subscriptions.mu.Lock()
	p.subscriptions.cache["selection"] = &subscriptionEntry{nodes: parsed.configs, expires: time.Now().Add(time.Minute), timer: time.NewTimer(time.Minute)}
	p.subscriptions.mu.Unlock()
	revision := p.State().Revision
	for _, indexes := range [][]int{{0, 0}, {1}, {-1}, {3}} {
		w := controlRequest(p, "/v1/subscriptions/import", map[string]any{"preview_id": "selection", "expected_revision": revision, "node_indexes": indexes})
		if w.Code != 400 || p.State().Revision != revision {
			t.Fatalf("invalid indexes accepted: %v", indexes)
		}
		if _, err := p.subscriptions.previewNodes("selection"); err != nil {
			t.Fatal("invalid selection consumed preview")
		}
	}
	w := controlRequest(p, "/v1/subscriptions/import", map[string]any{"preview_id": "selection", "expected_revision": revision, "node_indexes": []int{2}})
	if w.Code != 200 || len(p.State().Nodes) != 2 || p.State().Nodes[1].Name != "Two" {
		t.Fatalf("explicit selection was not honored: %d %s", w.Code, w.Body.String())
	}
}

func strconvItoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestSubscriptionCacheConcurrentInspectBounded(t *testing.T) {
	inspector, _ := inspectorFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, validSubscription) }))
	var group sync.WaitGroup
	var successful atomic.Int32
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := inspector.Inspect(context.Background(), "https://example.com/nodes"); err == nil {
				successful.Add(1)
			}
		}()
	}
	group.Wait()
	inspector.mu.Lock()
	cached := len(inspector.cache)
	inspector.mu.Unlock()
	if cached > 8 || cached != int(successful.Load()) {
		t.Fatal("concurrent cache exceeded bound")
	}
}
