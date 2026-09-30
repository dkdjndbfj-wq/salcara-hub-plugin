package hub

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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

type rejectingRoundTripper struct{ calls atomic.Int32 }

func (t *rejectingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return nil, fmt.Errorf("model API must not be used")
}

func newDeviceHub(t *testing.T, cfg Config) *Hub {
	t.Helper()
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

func deviceRequest(t *testing.T, h *Hub, path, id, secret, token string, body any, ip string) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	method := http.MethodPost
	if body == nil {
		method = http.MethodGet
	}
	r := httptest.NewRequest(method, "http://example.test/salcara-hub/v1"+path, bytes.NewReader(encoded))
	if ip == "" {
		ip = "192.0.2.1"
	}
	r.RemoteAddr = ip + ":1234"
	r.Header.Set("X-Salcara-Device-Id", id)
	r.Header.Set("X-Salcara-Device-Secret", secret)
	r.Header.Set("X-Salcara-Pair-Token", token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func enrollDevice(t *testing.T, h *Hub, id, secret string) {
	t.Helper()
	w := deviceRequest(t, h, "/device/register", id, secret, "", Device{DeviceID: id, Name: "Fixture PC", Projects: json.RawMessage(`[]`), Tools: json.RawMessage(`[]`)}, "")
	if w.Code != 200 {
		t.Fatalf("enroll %s: %d %s", id, w.Code, w.Body)
	}
}

func onlineDevice(t *testing.T, h *Hub, id string) *device {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	dev := h.accounts[deviceAccount(id)].devices[id]
	dev.conn = &bridgeConn{cmds: make(chan []byte, 64), closed: make(chan struct{})}
	return dev
}

func issueQR(t *testing.T, h *Hub, id, secret string) string {
	t.Helper()
	w := deviceRequest(t, h, "/bridge/pair/start", id, secret, "", map[string]string{"deviceId": id}, "")
	if w.Code != 200 {
		t.Fatalf("issue QR: %d %s", w.Code, w.Body)
	}
	var out struct {
		Ticket  string `json:"ticket"`
		Expires int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Ticket) != 64 || out.Expires <= time.Now().UnixMilli() {
		t.Fatal("invalid QR ticket")
	}
	return out.Ticket
}

func claimQR(t *testing.T, h *Hub, id, ticket string) string {
	t.Helper()
	w := deviceRequest(t, h, "/app/pair/qr", "", "", "", map[string]string{"deviceId": id, "ticket": ticket}, "")
	if w.Code != 200 {
		t.Fatalf("claim: %d %s", w.Code, w.Body)
	}
	var out struct {
		Token string `json:"pair_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Token) != 64 {
		t.Fatal("invalid phone token")
	}
	return out.Token
}

func TestDevicePairingDoesNotConsultStationUserOrModelKey(t *testing.T) {
	transport := &rejectingRoundTripper{}
	h := newDeviceHub(t, Config{Sub2APIURL: "http://unused.invalid", HTTPClient: &http.Client{Transport: transport}})
	id, secret := "pc-one", strings.Repeat("a", 64)
	enrollDevice(t, h, id, secret)
	onlineDevice(t, h, id)
	token := claimQR(t, h, id, issueQR(t, h, id, secret))
	w := deviceRequest(t, h, "/app/devices", "", "", token, nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), id) {
		t.Fatalf("phone access: %d %s", w.Code, w.Body)
	}
	if transport.calls.Load() != 0 {
		t.Fatal("device pairing invoked relay user eligibility validation")
	}
	ping := requestHub(t, h, http.MethodGet, "/ping", "", "", "", nil)
	if !strings.Contains(ping.Body.String(), `"device-pairing"`) || !strings.Contains(ping.Body.String(), `"protocolVersion":1`) {
		t.Fatalf("missing discovery %s", ping.Body)
	}
}

func TestDeviceNamespacesAndPhoneTokensCannotAuthenticateBridge(t *testing.T) {
	h := newDeviceHub(t, Config{})
	secret1, secret2 := strings.Repeat("a", 64), strings.Repeat("b", 64)
	enrollDevice(t, h, "pc-one", secret1)
	enrollDevice(t, h, "pc-two", secret2)
	onlineDevice(t, h, "pc-one")
	onlineDevice(t, h, "pc-two")
	token1 := claimQR(t, h, "pc-one", issueQR(t, h, "pc-one", secret1))
	token2 := claimQR(t, h, "pc-two", issueQR(t, h, "pc-two", secret2))
	list := deviceRequest(t, h, "/app/devices", "", "", token1, nil, "")
	if strings.Contains(list.Body.String(), "pc-two") {
		t.Fatal("phone can enumerate unpaired computer")
	}
	for _, tc := range []struct {
		path, id, secret, token string
		body                    any
	}{
		{"/app/events", "", "", token1, nil},
		{"/app/commands", "", "", token1, map[string]any{"deviceId": "pc-two", "command": map[string]string{"type": "test"}}},
		{"/bridge/register", "", "", token1, Device{DeviceID: "pc-one"}},
		{"/bridge/events", "pc-one", secret2, "", map[string]any{"deviceId": "pc-one", "events": []any{}}},
		{"/bridge/events", "pc-one", secret1, "", map[string]any{"deviceId": "pc-two", "events": []any{}}},
	} {
		path := tc.path
		if path == "/app/events" {
			path += "?deviceId=pc-two&sessionKey=test"
		}
		w := deviceRequest(t, h, path, tc.id, tc.secret, tc.token, tc.body, "")
		if w.Code == 200 {
			t.Fatalf("cross-device request accepted: %s", path)
		}
	}
	other := deviceRequest(t, h, "/app/devices", "", "", token2, nil, "")
	if other.Code != 200 {
		t.Fatal("denial of first phone damaged second namespace")
	}
	if len(h.accounts) != 2 {
		t.Fatalf("auth created orphan namespaces: %d", len(h.accounts))
	}
}

func TestQRSingleUseExpiryRotationAndRevoke(t *testing.T) {
	h := newDeviceHub(t, Config{})
	id, secret := "pc-one", strings.Repeat("c", 64)
	enrollDevice(t, h, id, secret)
	dev := onlineDevice(t, h, id)
	oldTicket := issueQR(t, h, id, secret)
	ticket := issueQR(t, h, id, secret)
	if deviceRequest(t, h, "/app/pair/qr", "", "", "", map[string]string{"deviceId": id, "ticket": oldTicket}, "").Code != 403 {
		t.Fatal("superseded QR still valid")
	}
	first := claimQR(t, h, id, ticket)
	if deviceRequest(t, h, "/app/pair/qr", "", "", "", map[string]string{"deviceId": id, "ticket": ticket}, "").Code != 403 {
		t.Fatal("QR replay accepted")
	}
	second := claimQR(t, h, id, issueQR(t, h, id, secret))
	if first == second || deviceRequest(t, h, "/app/devices", "", "", first, nil, "").Code != 403 {
		t.Fatal("phone token not rotated")
	}
	expired := issueQR(t, h, id, secret)
	h.mu.Lock()
	h.pairCodes[pairKey(deviceAccount(id), id)].expires = time.Now().Add(-time.Second)
	h.mu.Unlock()
	if deviceRequest(t, h, "/app/pair/qr", "", "", "", map[string]string{"deviceId": id, "ticket": expired}, "").Code != 403 {
		t.Fatal("expired QR accepted")
	}
	activeTicket := issueQR(t, h, id, secret)
	sub := &appSub{ch: make(chan sseMsg, 1), closed: make(chan struct{}), deviceID: id}
	h.mu.Lock()
	h.accounts[deviceAccount(id)].apps[sub] = struct{}{}
	h.mu.Unlock()
	revoke := deviceRequest(t, h, "/bridge/pair/revoke", id, secret, "", map[string]string{"deviceId": id}, "")
	if revoke.Code != 200 {
		t.Fatalf("revoke %d %s", revoke.Code, revoke.Body)
	}
	select {
	case <-sub.closed:
	default:
		t.Fatal("revoke did not close existing phone stream")
	}
	if deviceRequest(t, h, "/app/devices", "", "", second, nil, "").Code != 403 {
		t.Fatal("revoked phone token valid")
	}
	if deviceRequest(t, h, "/app/pair/qr", "", "", "", map[string]string{"deviceId": id, "ticket": activeTicket}, "").Code != 403 {
		t.Fatal("revoke did not invalidate pending QR")
	}
	h.mu.Lock()
	retained := dev.hasSecret && dev.conn != nil
	h.mu.Unlock()
	if !retained {
		t.Fatal("phone revoke removed computer identity/connection")
	}
}

func TestPhoneCanDetachOnlyItsBoundDevice(t *testing.T) {
	h := newDeviceHub(t, Config{})
	for _, id := range []string{"pc-one", "pc-two"} {
		enrollDevice(t, h, id, strings.Repeat("d", 64))
		onlineDevice(t, h, id)
	}
	first := claimQR(t, h, "pc-one", issueQR(t, h, "pc-one", strings.Repeat("d", 64)))
	second := claimQR(t, h, "pc-two", issueQR(t, h, "pc-two", strings.Repeat("d", 64)))
	revoke := deviceRequest(t, h, "/app/pair/revoke", "", "", first, map[string]string{"deviceId": "pc-two"}, "")
	if revoke.Code != 200 {
		t.Fatalf("phone revoke: %d %s", revoke.Code, revoke.Body)
	}
	if deviceRequest(t, h, "/app/devices", "", "", first, nil, "").Code != 403 {
		t.Fatal("own token retained")
	}
	if deviceRequest(t, h, "/app/devices", "", "", second, nil, "").Code != 200 {
		t.Fatal("phone revoked other computer")
	}
}

func TestEnrollmentAndTicketClaimsAreAtomic(t *testing.T) {
	h := newDeviceHub(t, Config{AuthFailLimit: 1000})
	var successes atomic.Int32
	var winner string
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			secret := fmt.Sprintf("%064x", i+1)
			w := deviceRequest(t, h, "/device/register", "race-pc", secret, "", Device{DeviceID: "race-pc"}, "")
			if w.Code == 200 {
				successes.Add(1)
				mu.Lock()
				winner = secret
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if successes.Load() != 1 || len(h.accounts) != 1 {
		t.Fatalf("enrollment race winners=%d namespaces=%d", successes.Load(), len(h.accounts))
	}
	onlineDevice(t, h, "race-pc")
	ticket := issueQR(t, h, "race-pc", winner)
	successes.Store(0)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if deviceRequest(t, h, "/app/pair/qr", "", "", "", map[string]string{"deviceId": "race-pc", "ticket": ticket}, "").Code == 200 {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("QR concurrent claims=%d", successes.Load())
	}
}

func TestHashOnlyPersistenceAndRestart(t *testing.T) {
	dir := t.TempDir()
	h := newDeviceHub(t, Config{DataDir: dir})
	secret := strings.Repeat("e", 64)
	enrollDevice(t, h, "persist-pc", secret)
	onlineDevice(t, h, "persist-pc")
	ticket := issueQR(t, h, "persist-pc", secret)
	token := claimQR(t, h, "persist-pc", ticket)
	h.markDirty()
	h.Close()
	stored, err := os.ReadFile(filepath.Join(dir, devicesFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{secret, ticket, token} {
		if bytes.Contains(stored, []byte(raw)) {
			t.Fatal("raw credential persisted")
		}
	}
	if !bytes.Contains(stored, []byte(`"secretHash"`)) || !bytes.Contains(stored, []byte(`"pairHash"`)) {
		t.Fatal("hash persistence missing")
	}
	restarted := newDeviceHub(t, Config{DataDir: dir})
	access := deviceRequest(t, restarted, "/app/devices", "", "", token, nil, "")
	if access.Code != 200 || strings.Contains(access.Body.String(), `"online":true`) {
		t.Fatalf("restart access: %d %s", access.Code, access.Body)
	}
	enrollDevice(t, restarted, "persist-pc", secret)
	if len(restarted.pairTokens) != 1 {
		t.Fatal("phone token index not rebuilt")
	}
}

func TestEnrollmentAndQRFailuresAreIPRateLimitedWithoutOrphans(t *testing.T) {
	h := newDeviceHub(t, Config{AuthFailLimit: 3})
	for i := 0; i < 11; i++ {
		w := deviceRequest(t, h, "/device/register", fmt.Sprintf("pc-%d", i), strings.Repeat("f", 64), "", Device{DeviceID: fmt.Sprintf("pc-%d", i)}, "198.51.100.1")
		if i < 10 && w.Code != 200 {
			t.Fatalf("premature enrollment rejection %d", i)
		}
		if i == 10 && w.Code != 429 {
			t.Fatal("new enrollment not rate limited")
		}
	}
	for i := 0; i < 4; i++ {
		w := deviceRequest(t, h, "/app/pair/qr", "", "", "", map[string]string{"deviceId": "missing", "ticket": strings.Repeat("a", 64)}, "203.0.113.1")
		if i < 3 && w.Code != 403 {
			t.Fatalf("unexpected failure code %d", w.Code)
		}
		if i == 3 && w.Code != 429 {
			t.Fatal("QR guessing not limited")
		}
	}
	if len(h.accounts) != 10 || len(h.pairTickets) != 0 {
		t.Fatal("failed requests created orphan state")
	}
	w := deviceRequest(t, h, "/device/register", "other-ip", strings.Repeat("f", 64), "", Device{DeviceID: "other-ip"}, "198.51.100.2")
	if w.Code != 200 {
		t.Fatal("one IP limit disabled all other computers")
	}
}

func TestQRCodeCannotBindOtherDeviceOrOfflineComputer(t *testing.T) {
	h := newDeviceHub(t, Config{})
	id, secret := "pc-one", strings.Repeat("a", 64)
	enrollDevice(t, h, id, secret)
	dev := onlineDevice(t, h, id)
	ticket := issueQR(t, h, id, secret)
	wrong := deviceRequest(t, h, "/app/pair/qr", "", "", "", map[string]string{"deviceId": "pc-two", "ticket": ticket}, "")
	if wrong.Code != 403 {
		t.Fatal("ticket bound wrong device")
	}
	h.mu.Lock()
	dev.conn = nil
	h.mu.Unlock()
	offline := deviceRequest(t, h, "/app/pair/qr", "", "", "", map[string]string{"deviceId": id, "ticket": ticket}, "")
	if offline.Code != 403 || dev.hasPair {
		t.Fatal("offline computer reported paired success")
	}
	hash := sha256.Sum256([]byte(ticket))
	if len(h.pairTickets) != 1 || h.pairTickets[hash] == "" {
		t.Fatal("wrong/offline claim damaged valid ticket")
	}
}

func TestShutdownRejectsNewEnrollmentAndDoesNotWriteCredentialsToLogs(t *testing.T) {
	var logs bytes.Buffer
	h, err := New(Config{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	id, secret := "pc-log", strings.Repeat("1", 64)
	enrollDevice(t, h, id, secret)
	onlineDevice(t, h, id)
	ticket := issueQR(t, h, id, secret)
	token := claimQR(t, h, id, ticket)
	for _, credential := range []string{secret, ticket, token} {
		if strings.Contains(logs.String(), credential) {
			t.Fatal("credential written to request logs")
		}
	}
	h.Close()
	w := deviceRequest(t, h, "/device/register", "after-close", strings.Repeat("2", 64), "", Device{DeviceID: "after-close"}, "")
	if w.Code != 503 || len(h.accounts) != 1 {
		t.Fatal("closed Hub accepted new namespace")
	}
}
