package standalone

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"salcara/hub/internal/hub"
)

func serverFixture(t *testing.T) (*Server, Config, string) {
	t.Helper()
	cfg := testConfig(t)
	if err := InitAdminAccount(cfg); err != nil {
		t.Fatal(err)
	}
	password := initialPassword(t, cfg)
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, cfg, password
}

func initialPassword(t *testing.T, cfg Config) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cfg.DataDir, initialLoginFile))
	if err != nil {
		t.Fatal(err)
	}
	password := strings.TrimSuffix(string(raw), "\n")
	if len(password) != 43 || strings.ContainsAny(password, "\r\n=") {
		t.Fatal("initial password was not privately saved as exactly one raw line")
	}
	return password
}

type fixtureSession struct {
	cookie *http.Cookie
	csrf   string
}

var fixtureSessions sync.Map

func fixtureLogin(t *testing.T, s *Server, password string) fixtureSession {
	t.Helper()
	if cached, ok := fixtureSessions.Load(s); ok {
		return cached.(fixtureSession)
	}
	body, _ := json.Marshal(map[string]string{"password": password})
	r := httptest.NewRequest(http.MethodPost, "http://example.test"+Prefix+"/_admin/v1/auth/login", bytes.NewReader(body))
	r.Header.Set("Origin", "http://example.test")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.CSRF) != 43 || len(w.Result().Cookies()) != 1 {
		t.Fatalf("fixture account login failed: %d", w.Code)
	}
	session := fixtureSession{cookie: w.Result().Cookies()[0], csrf: out.CSRF}
	fixtureSessions.Store(s, session)
	t.Cleanup(func() { fixtureSessions.Delete(s) })
	return session
}

func TestHealthReportsExactProcessIdentityWithoutCredential(t *testing.T) {
	s, _, admin := serverFixture(t)
	w := adminRequest(t, s, http.MethodGet, "/healthz", "", nil)
	var health struct {
		OK      bool   `json:"ok"`
		Service string `json:"service"`
		Version string `json:"version"`
		PID     int    `json:"pid"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &health) != nil || !health.OK || health.Service != "salcara-hub" || health.Version != Version || health.PID != os.Getpid() || strings.Contains(w.Body.String(), admin) {
		t.Fatal("health process identity missing or credential exposed")
	}
}

func adminRequest(t *testing.T, s *Server, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(method, "http://example.test"+path, bytes.NewReader(b))
	r.Header.Set("Origin", "http://example.test")
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		session := fixtureLogin(t, s, token)
		r.AddCookie(session.cookie)
		r.Header.Set("X-Salcara-CSRF", session.csrf)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestStaticPagesStripInjectedCredentialsAndAdminCannotUseQueryCookies(t *testing.T) {
	s, _, token := serverFixture(t)
	for _, path := range []string{Prefix + "/", Prefix + "/admin/", Prefix + "/admin/app.js", Prefix + "/help.js", Prefix + "/app.css"} {
		w := adminRequest(t, s, http.MethodGet, path+"?auth_token=relay-token&user_id=42", "", nil)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != path || strings.Contains(w.Body.String(), "relay-token") {
			t.Fatalf("credential query retained in static page %s", path)
		}
		if w.Header().Get("Referrer-Policy") != "no-referrer" || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'self'") {
			t.Fatal("iframe privacy headers missing")
		}
	}
	for _, path := range []string{"/v1/ping", Prefix + "/_admin/snapshot", Prefix + "/_admin/v1/state?auth_token=" + token} {
		w := adminRequest(t, s, http.MethodGet, path, "", nil)
		if w.Code == 200 {
			t.Fatal("unprefixed/legacy/query admin route leaked")
		}
	}
	r := httptest.NewRequest(http.MethodGet, "http://example.test"+Prefix+"/_admin/v1/state", nil)
	r.AddCookie(&http.Cookie{Name: "auth_token", Value: token})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("relay cookie became Hub admin authority")
	}
	r.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("legacy bearer credential became Hub admin authority")
	}
	r.Header.Del("Authorization")
	r.AddCookie(fixtureLogin(t, s, token).cookie)
	r.Header.Set("Origin", "https://attacker.example")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-origin admin request accepted")
	}
	r.Header.Set("Origin", "http://example.test")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("Access-Control-Allow-Origin") != "" || strings.Contains(w.Body.String(), token) {
		t.Fatal("safe authenticated state failed or exposed credentials")
	}
}

func TestAdminStrictActionAndPagination(t *testing.T) {
	s, _, token := serverFixture(t)
	for _, body := range []any{
		map[string]any{"action": "ban", "device_ref": "missing", "reason": "test", "confirm": true, "actor": "host-admin:fake"},
		map[string]any{"action": "ban", "device_ref": "missing", "reason": "test", "confirm": true, "unknown": "value"},
		map[string]any{"action": "ban", "device_ref": "missing", "reason": "test", "confirm": false},
	} {
		if w := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/action", token, body); w.Code != 400 {
			t.Fatal("unknown/spoofed actor accepted")
		}
	}
	for _, suffix := range []string{"?limit=201", "?offset=-1", "?filter=arbitrary", "?limit=1&limit=2", "?auth_token=ignored"} {
		if w := adminRequest(t, s, http.MethodGet, Prefix+"/_admin/v1/state"+suffix, token, nil); w.Code != 400 {
			t.Fatal("unsafe state query accepted")
		}
	}
	if w := adminRequest(t, s, http.MethodGet, Prefix+"/_admin/v1/state?limit=10&offset=0&filter=paired", token, nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"update_configured":false`) {
		t.Fatal("valid paged state failed")
	}
}

func publicHTTP(t *testing.T, client *http.Client, base, method, path, id, secret, token string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	r, _ := http.NewRequest(method, base+Prefix+"/v1"+path, bytes.NewReader(b))
	r.Header.Set("X-Salcara-Device-Id", id)
	r.Header.Set("X-Salcara-Device-Secret", secret)
	r.Header.Set("X-Salcara-Pair-Token", token)
	r.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		t.Fatal("invalid public JSON")
	}
	return resp.StatusCode, out
}

func TestActualHTTPQRAdminBanPersistenceAndRestart(t *testing.T) {
	s, cfg, token := serverFixture(t)
	ts := httptest.NewServer(s)
	defer ts.Close()
	client := ts.Client()
	id, secret := "standalone-fixture-pc", strings.Repeat("ab", 32)
	if code, _ := publicHTTP(t, client, ts.URL, http.MethodPost, "/device/register", id, secret, "", hub.Device{DeviceID: id, Name: "Fixture desktop"}); code != 200 {
		t.Fatal("independent device registration failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+Prefix+"/v1/bridge/stream?deviceId="+id, nil)
	r.Header.Set("X-Salcara-Device-Id", id)
	r.Header.Set("X-Salcara-Device-Secret", secret)
	stream, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if line, err := bufio.NewReader(stream.Body).ReadString('\n'); err != nil || line != ": connected\n" {
		t.Fatal("desktop bridge did not establish SSE")
	}
	code, qr := publicHTTP(t, client, ts.URL, http.MethodPost, "/bridge/pair/start", id, secret, "", map[string]string{"deviceId": id})
	if code != 200 {
		t.Fatal("QR ticket failed")
	}
	code, phone := publicHTTP(t, client, ts.URL, http.MethodPost, "/app/pair/qr", "", "", "", map[string]any{"deviceId": id, "ticket": qr["ticket"]})
	if code != 200 {
		t.Fatal("phone QR pairing failed")
	}
	phoneToken, _ := phone["pair_token"].(string)
	if len(phoneToken) != 64 {
		t.Fatal("phone token not issued")
	}
	if code, _ := publicHTTP(t, client, ts.URL, http.MethodGet, "/app/devices", "", "", phoneToken, nil); code != 200 {
		t.Fatal("phone cannot list its paired computer")
	}
	state := adminRequest(t, s, http.MethodGet, Prefix+"/_admin/v1/state", token, nil)
	var snapshot struct {
		Devices []hub.AdminDevice `json:"devices"`
	}
	if state.Code != 200 || json.Unmarshal(state.Body.Bytes(), &snapshot) != nil || len(snapshot.Devices) != 1 || !snapshot.Devices[0].Paired || !snapshot.Devices[0].Online {
		t.Fatal("admin independent pairing status inaccurate")
	}
	ref := snapshot.Devices[0].Ref
	ban := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/action", token, map[string]any{"action": "ban", "device_ref": ref, "reason": "Fixture abuse report", "confirm": true})
	if ban.Code != 200 {
		t.Fatal("admin ban failed")
	}
	if code, _ := publicHTTP(t, client, ts.URL, http.MethodGet, "/app/devices", "", "", phoneToken, nil); code != 403 {
		t.Fatal("ban did not revoke phone authority")
	}
	cancel()
	stream.Body.Close()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if w := adminRequest(t, s, http.MethodGet, "/healthz", "", nil); w.Code != 503 {
		t.Fatal("closed Hub claimed healthy")
	}
	restarted, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal("restarting with the same token and volume failed")
	}
	defer restarted.Close()
	state = adminRequest(t, restarted, http.MethodGet, Prefix+"/_admin/v1/state", token, nil)
	var persisted struct {
		Devices []hub.AdminDevice `json:"devices"`
		Audit   []hub.AdminAudit  `json:"audit"`
	}
	if state.Code != 200 || json.Unmarshal(state.Body.Bytes(), &persisted) != nil || len(persisted.Devices) != 1 || !persisted.Devices[0].Banned || persisted.Devices[0].Online || len(persisted.Audit) != 1 || persisted.Audit[0].Actor != "standalone-admin" {
		t.Fatal("ban/audit persistence or restart status failed")
	}
	unban := adminRequest(t, restarted, http.MethodPost, Prefix+"/_admin/v1/action", token, map[string]any{"action": "unban", "device_ref": ref, "reason": "Fixture release", "confirm": true})
	if unban.Code != 200 {
		t.Fatal("unban after restart failed")
	}
}
