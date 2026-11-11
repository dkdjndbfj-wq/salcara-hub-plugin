package hub

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var notificationScopeA = "ns1_" + strings.Repeat("a", 64)
var notificationScopeB = "ns1_" + strings.Repeat("b", 64)

func registerPushScope(t *testing.T, h *Hub, id, pair, token, scope string) {
	t.Helper()
	w := deviceRequest(t, h, "/app/push/register", "", "", pair, map[string]string{
		"deviceId": id, "token": token, "platform": "android", "notificationScope": scope,
	}, "")
	if w.Code != http.StatusOK {
		t.Fatalf("register scoped push: %d %s", w.Code, w.Body)
	}
}

func postScopeTurn(t *testing.T, h *Hub, id, secret, turn string) {
	t.Helper()
	phoneIdle(h, id)
	postTurn(t, h, id, secret, map[string]any{
		"type": "turn", "sessionKey": "codex:shared-id", "tool": "codex", "status": "completed", "turnId": turn, "ts": time.Now().UnixMilli(),
	})
}

func TestPushScopeUpdatesWithSameTokenAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	h, google := pushHub(t, dir)
	id, secret := "scope-pc", strings.Repeat("a", 64)
	pair := pairPhone(t, h, id, secret)
	registerPushScope(t, h, id, pair, fcmToken, notificationScopeA)
	registerPushScope(t, h, id, pair, fcmToken, notificationScopeB)
	postScopeTurn(t, h, id, secret, "first")
	message := waitMessages(t, google, 1)[0]["message"].(map[string]any)
	if message["data"].(map[string]any)["notificationScope"] != notificationScopeB {
		t.Fatal("push retained the old scope after registration updated")
	}
	collapse := notificationScopeB + ":task:" + id + ":codex:shared-id:first"
	if message["android"].(map[string]any)["collapse_key"] != pushCollapseKey(collapse) {
		t.Fatal("FCM collapse key must be short and scoped to this station/turn")
	}
	h.Close()
	restarted, google2 := pushHub(t, dir)
	postScopeTurn(t, restarted, id, secret, "second")
	message = waitMessages(t, google2, 1)[0]["message"].(map[string]any)
	if message["data"].(map[string]any)["notificationScope"] != notificationScopeB {
		t.Fatal("persisted push registration lost its scope")
	}
}

func TestPushRejectsInvalidNotificationScope(t *testing.T) {
	h, _ := pushHub(t, t.TempDir())
	id, secret := "scope-invalid", strings.Repeat("b", 64)
	pair := pairPhone(t, h, id, secret)
	for _, scope := range []any{"", "ns1_" + strings.Repeat("A", 64), "ns1_" + strings.Repeat("a", 63), "ns2_" + strings.Repeat("a", 64), "raw-pair-token", nil, 42} {
		w := deviceRequest(t, h, "/app/push/register", "", "", pair, map[string]any{
			"deviceId": id, "token": fcmToken, "notificationScope": scope,
		}, "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid scope accepted: %v (HTTP %d)", scope, w.Code)
		}
	}
	// An omitted scope remains compatible with older phones, including unregister.
	registerPush(t, h, id, pair, fcmToken)
	registerPush(t, h, id, pair, "")
}

type oauthWaitingPushTransport struct {
	base    http.RoundTripper
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *oauthWaitingPushTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == "oauth.test" {
		p.once.Do(func() { close(p.started) })
		select {
		case <-p.release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	return p.base.RoundTrip(r)
}

func TestPushDropsStaleJobAfterOAuthWait(t *testing.T) {
	for _, change := range []string{"scope", "unregister", "pairing"} {
		t.Run(change, func(t *testing.T) {
			h, google := pushHub(t, t.TempDir())
			transport := &oauthWaitingPushTransport{base: google, started: make(chan struct{}), release: make(chan struct{})}
			h.push.sender.client = &http.Client{Transport: transport}
			unblock := sync.OnceFunc(func() { close(transport.release) })
			defer unblock()
			id, secret := "scope-wait", strings.Repeat("c", 64)
			pair := pairPhone(t, h, id, secret)
			registerPushScope(t, h, id, pair, fcmToken, notificationScopeA)
			postScopeTurn(t, h, id, secret, "old")
			awaitSSESignal(t, transport.started, "old push did not reach the OAuth wait")
			scope := notificationScopeA
			switch change {
			case "scope":
				scope = notificationScopeB
			case "unregister":
				registerPush(t, h, id, pair, "")
			case "pairing":
				pair = claimQR(t, h, id, issueQR(t, h, id, secret))
			}
			registerPushScope(t, h, id, pair, fcmToken, scope)
			postScopeTurn(t, h, id, secret, "new")
			unblock()
			messages := waitMessages(t, google, 1)
			if len(messages) != 1 {
				t.Fatalf("old queued push survived registration change: %d messages", len(messages))
			}
			data := messages[0]["message"].(map[string]any)["data"].(map[string]any)
			if data["notificationScope"] != scope {
				t.Fatal("push borrowed a different registration's scope")
			}
		})
	}
}

func TestOldFCMFailureCannotRemoveReplacementRegistration(t *testing.T) {
	h, google := pushHub(t, t.TempDir())
	started, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	var requests atomic.Int32
	google.response = func(string) (int, string) {
		if requests.Add(1) == 1 {
			close(started)
			<-release
			return 404, `{"error":{"status":"NOT_FOUND"}}`
		}
		return 200, `{"name":"fixture"}`
	}
	id, secret := "scope-late-result", strings.Repeat("d", 64)
	pair := pairPhone(t, h, id, secret)
	registerPushScope(t, h, id, pair, fcmToken, notificationScopeA)
	postScopeTurn(t, h, id, secret, "old")
	awaitSSESignal(t, started, "old push did not reach FCM")
	// Even an identical replacement is a new registration generation.
	registerPush(t, h, id, pair, "")
	registerPushScope(t, h, id, pair, fcmToken, notificationScopeA)
	postScopeTurn(t, h, id, secret, "new")
	unblock()
	if messages := waitMessages(t, google, 2); len(messages) != 2 {
		t.Fatalf("late FCM failure removed the new registration: %d", len(messages))
	}
}
