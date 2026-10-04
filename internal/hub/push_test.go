package hub

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGoogle answers the OAuth token exchange and FCM sends, recording messages.
type fakeGoogle struct {
	t        *testing.T
	key      *rsa.PublicKey
	mu       sync.Mutex
	sent     []map[string]any
	tokens   int
	response func(token string) (int, string)
}

func (f *fakeGoogle) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	reply := func(code int, text string) (*http.Response, error) {
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(text)), Header: http.Header{}}, nil
	}
	switch r.URL.Host {
	case "oauth.test":
		form, _ := url.ParseQuery(string(body))
		parts := strings.Split(form.Get("assertion"), ".")
		if len(parts) != 3 {
			f.t.Error("assertion is not a JWT")
			return reply(400, `{}`)
		}
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if err := rsa.VerifyPKCS1v15(f.key, crypto.SHA256, digest[:], sig); err != nil {
			f.t.Error("JWT signature does not verify with the service account key")
		}
		claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
		if !strings.Contains(string(claims), `"scope":"https://www.googleapis.com/auth/firebase.messaging"`) {
			f.t.Errorf("JWT scope missing: %s", claims)
		}
		f.mu.Lock()
		f.tokens++
		f.mu.Unlock()
		return reply(200, `{"access_token":"ya29.fixture","expires_in":3600}`)
	case "fcm.test":
		if r.Header.Get("Authorization") != "Bearer ya29.fixture" || r.URL.Path != "/v1/projects/salcara-test/messages:send" {
			f.t.Errorf("unexpected FCM request %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		var msg map[string]any
		_ = json.Unmarshal(body, &msg)
		f.mu.Lock()
		f.sent = append(f.sent, msg)
		f.mu.Unlock()
		token := msg["message"].(map[string]any)["token"].(string)
		if f.response != nil {
			return reply(f.response(token))
		}
		return reply(200, `{"name":"projects/salcara-test/messages/1"}`)
	}
	f.t.Errorf("unexpected outbound request to %s", r.URL)
	return reply(500, `{}`)
}

func (f *fakeGoogle) messages() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.sent...)
}

func serviceAccountFixture(t *testing.T) ([]byte, *rsa.PublicKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	raw, _ := json.Marshal(map[string]string{"type": "service_account", "project_id": "salcara-test", "private_key": pemKey, "client_email": "push@salcara-test.iam.gserviceaccount.com"})
	return raw, &key.PublicKey
}

func pushHub(t *testing.T, dir string) (*Hub, *fakeGoogle) {
	t.Helper()
	creds, pub := serviceAccountFixture(t)
	google := &fakeGoogle{t: t, key: pub}
	h := newDeviceHub(t, Config{ResourceMode: "economy", DataDir: dir, FCMCredentials: creds, HTTPClient: &http.Client{Transport: google}})
	h.push.sender.tokenURL = "https://oauth.test/token"
	h.push.sender.endpoint = "https://fcm.test"
	return h, google
}

func pairPhone(t *testing.T, h *Hub, id, secret string) string {
	t.Helper()
	enrollDevice(t, h, id, secret)
	onlineDevice(t, h, id)
	return claimQR(t, h, id, issueQR(t, h, id, secret))
}

const fcmToken = "fcm-token-fixture:APA91bExampleTokenValue_0123456789"

func registerPush(t *testing.T, h *Hub, id, pair, token string) map[string]any {
	t.Helper()
	w := deviceRequest(t, h, "/app/push/register", "", "", pair, map[string]string{"deviceId": id, "token": token, "platform": "android"}, "")
	if w.Code != 200 {
		t.Fatalf("register push: %d %s", w.Code, w.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

func postTurn(t *testing.T, h *Hub, id, secret string, events ...map[string]any) {
	t.Helper()
	raw := make([]json.RawMessage, 0, len(events))
	for _, e := range events {
		b, _ := json.Marshal(e)
		raw = append(raw, b)
	}
	w := deviceRequest(t, h, "/bridge/events", id, secret, "", map[string]any{"deviceId": id, "events": raw}, "")
	if w.Code != 200 {
		t.Fatalf("bridge events: %d %s", w.Code, w.Body)
	}
}

// The phone counts as "using the app" for a short while after any request; let that pass.
func phoneIdle(h *Hub, id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.accounts[deviceAccount(id)].devices[id].lastAppSeen = 0
}

func waitMessages(t *testing.T, g *fakeGoogle, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := g.messages(); len(got) >= n {
			time.Sleep(50 * time.Millisecond)
			return g.messages()
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("want %d push messages, got %d", n, len(g.messages()))
	return nil
}

func TestPushOffWithoutCredentials(t *testing.T) {
	h := newDeviceHub(t, Config{ResourceMode: "economy"})
	secret := strings.Repeat("a", 64)
	pair := pairPhone(t, h, "pc-off", secret)
	if out := registerPush(t, h, "pc-off", pair, fcmToken); out["push"] != "off" {
		t.Fatalf("push must report off without a service account: %v", out)
	}
	w := deviceRequest(t, h, "/ping", "", "", "", nil, "")
	if strings.Contains(w.Body.String(), "push.fcm.v1") {
		t.Fatal("capability advertised without credentials")
	}
}

func TestPushSendsOneGenericMessagePerNews(t *testing.T) {
	dir := t.TempDir()
	h, google := pushHub(t, dir)
	if w := deviceRequest(t, h, "/ping", "", "", "", nil, ""); !strings.Contains(w.Body.String(), "push.fcm.v1") {
		t.Fatal("push capability missing")
	}
	secret := strings.Repeat("b", 64)
	pair := pairPhone(t, h, "pc-push", secret)
	// Only the paired phone may register.
	if w := deviceRequest(t, h, "/app/push/register", "", "", strings.Repeat("c", 64), map[string]string{"deviceId": "pc-push", "token": fcmToken}, ""); w.Code == 200 {
		t.Fatal("an unpaired token registered for push")
	}
	if out := registerPush(t, h, "pc-push", pair, fcmToken); out["push"] != "fcm" {
		t.Fatalf("register: %v", out)
	}
	if info, err := os.Stat(filepath.Join(dir, pushTokensFile)); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("push tokens not saved privately: %v %v", info, err)
	}
	phoneIdle(h, "pc-push")
	done := map[string]any{"type": "turn", "sessionKey": "codex:s1", "tool": "codex", "status": "completed", "turnId": "t1", "text": "secret task output", "ts": time.Now().UnixMilli()}
	postTurn(t, h, "pc-push", secret, done)
	postTurn(t, h, "pc-push", secret, done) // the computer retried the upload
	ask := map[string]any{"type": "approval.request", "sessionKey": "codex:s1", "tool": "codex", "approvalId": "ap-7", "kind": "command", "title": "rm -rf build", "detail": "private detail", "ts": time.Now().UnixMilli()}
	postTurn(t, h, "pc-push", secret, ask)
	msgs := waitMessages(t, google, 2)
	if len(msgs) != 2 {
		t.Fatalf("want exactly 2 messages (retry deduplicated), got %d", len(msgs))
	}
	first := msgs[0]["message"].(map[string]any)
	data := first["data"].(map[string]any)
	if first["token"] != fcmToken || data["kind"] != "done" || data["agent"] != "Codex" || data["deviceId"] != "pc-push" || data["sessionKey"] != "codex:s1" {
		t.Fatalf("unexpected message: %v", first)
	}
	if !strings.HasPrefix(data["id"].(string), "done:pc-push:codex:s1:") {
		t.Fatalf("notification id must match the phone's own scheme: %v", data["id"])
	}
	blob, _ := json.Marshal(msgs)
	for _, private := range []string{"secret task output", "rm -rf build", "private detail"} {
		if strings.Contains(string(blob), private) {
			t.Fatalf("task text %q leaked into a push message", private)
		}
	}
	second := msgs[1]["message"].(map[string]any)["data"].(map[string]any)
	if second["kind"] != "ask" || second["id"] != "ask:pc-push:codex:s1:ap-7" {
		t.Fatalf("approval message: %v", second)
	}
	if android := first["android"].(map[string]any); android["priority"] != "HIGH" {
		t.Fatal("task news must be high priority")
	}
	if google.tokens != 1 {
		t.Fatalf("OAuth token should be cached, exchanged %d times", google.tokens)
	}
}

func TestPushWaitsWhileThePhoneIsActive(t *testing.T) {
	h, google := pushHub(t, t.TempDir())
	secret := strings.Repeat("d", 64)
	pair := pairPhone(t, h, "pc-active", secret)
	registerPush(t, h, "pc-active", pair, fcmToken) // this request marks the phone active
	postTurn(t, h, "pc-active", secret, map[string]any{"type": "turn", "sessionKey": "s", "status": "failed", "ts": time.Now().UnixMilli()})
	time.Sleep(200 * time.Millisecond)
	if n := len(google.messages()); n != 0 {
		t.Fatalf("pushed %d messages while the app was open", n)
	}
}

func TestPushFollowsThePairingAndDropsDeadTokens(t *testing.T) {
	dir := t.TempDir()
	h, google := pushHub(t, dir)
	secret := strings.Repeat("e", 64)
	pair := pairPhone(t, h, "pc-life", secret)
	registerPush(t, h, "pc-life", pair, fcmToken)

	// A new pairing (another phone scanned) makes the old registration stale.
	claimQR(t, h, "pc-life", issueQR(t, h, "pc-life", secret))
	phoneIdle(h, "pc-life")
	postTurn(t, h, "pc-life", secret, map[string]any{"type": "turn", "sessionKey": "s", "status": "completed", "turnId": "a", "ts": time.Now().UnixMilli()})
	time.Sleep(200 * time.Millisecond)
	if n := len(google.messages()); n != 0 {
		t.Fatal("pushed to the phone of an old pairing")
	}

	// Restart keeps a current registration; FCM reporting it gone removes it.
	pair2 := claimQR(t, h, "pc-life", issueQR(t, h, "pc-life", secret))
	registerPush(t, h, "pc-life", pair2, fcmToken)
	h.Close()
	h2, google2 := pushHub(t, dir)
	google2.response = func(string) (int, string) {
		return 404, `{"error":{"status":"NOT_FOUND","details":[{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"UNREGISTERED"}]}}`
	}
	postTurn(t, h2, "pc-life", secret, map[string]any{"type": "turn", "sessionKey": "s", "status": "completed", "turnId": "b", "ts": time.Now().UnixMilli()})
	waitMessages(t, google2, 1)
	time.Sleep(100 * time.Millisecond)
	h2.push.mu.Lock()
	_, kept := h2.push.entries[pairKey(deviceAccount("pc-life"), "pc-life")]
	h2.push.mu.Unlock()
	if kept {
		t.Fatal("an unregistered token was kept")
	}

	// Unpairing forgets the phone's token too.
	registerPush(t, h2, "pc-life", pair2, fcmToken)
	if w := deviceRequest(t, h2, "/app/pair/revoke", "", "", pair2, map[string]string{"deviceId": "pc-life"}, ""); w.Code != 200 {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	h2.push.mu.Lock()
	n := len(h2.push.entries)
	h2.push.mu.Unlock()
	if n != 0 {
		t.Fatal("revoked pairing kept its push token")
	}
}

func TestPushRejectsBadTokensAndCredentials(t *testing.T) {
	h, _ := pushHub(t, t.TempDir())
	secret := strings.Repeat("f", 64)
	pair := pairPhone(t, h, "pc-bad", secret)
	for _, token := range []string{"short", strings.Repeat("x", maxPushToken+1), "has space in it but long enough"} {
		if w := deviceRequest(t, h, "/app/push/register", "", "", pair, map[string]string{"deviceId": "pc-bad", "token": token}, ""); w.Code != 400 {
			t.Fatalf("token %q accepted: %d", token[:5], w.Code)
		}
	}
	if _, err := New(Config{FCMCredentials: []byte(`{"type":"authorized_user"}`)}); err == nil {
		t.Fatal("a non service-account key was accepted")
	}
}
