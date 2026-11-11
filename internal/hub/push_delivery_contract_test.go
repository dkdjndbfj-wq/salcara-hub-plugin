package hub

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPushIOSDigestPreservesTaskIDsAndUrgentPriority(t *testing.T) {
	h, google := pushHub(t, t.TempDir())
	id, secret := "ios-digest-root", strings.Repeat("e", 64)
	pair := pairPhone(t, h, id, secret)
	registerPushPlatform(t, h, id, pair, fcmToken, "ios")
	postTurn(t, h, id, secret, lifecycleEvent("done", "codex:a", "one", ""), lifecycleEvent("reply", "codex:b", "two", "final-b"), lifecycleEvent("failed", "codex:c", "urgent", ""))
	messages := waitMessages(t, google, 2)
	if len(messages) != 2 || lifecycleData(messages[0])["kind"] != "failed" {
		t.Fatalf("urgent result waited behind ordinary digest: %v", messages)
	}
	digest := lifecycleData(messages[1])
	if digest["notificationCount"] != "2" || !strings.HasPrefix(digest["id"].(string), "summary:") {
		t.Fatalf("not a two-task digest: %v", digest)
	}
	var tasks []iosDigestTask
	if json.Unmarshal([]byte(digest["taskIds"].(string)), &tasks) != nil || len(tasks) != 2 || tasks[0].ID == tasks[1].ID {
		t.Fatalf("digest lost identities: %v", digest)
	}
	var pending int
	// A sender queue barrier settles the digest's per-source durable completions.
	lifecycleDrain(t, h, google, id, secret, "digest-done")
	if err := h.durable.db.QueryRow(`SELECT COUNT(*) FROM push_jobs WHERE state='pending'`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("source jobs not completed: %d %v", pending, err)
	}
}

func TestPushIOSDigestFiltersResolvedTargetAfterOAuth(t *testing.T) {
	h, google := pushHub(t, t.TempDir())
	id, secret := "ios-digest-oauth", strings.Repeat("e", 64)
	pair := pairPhone(t, h, id, secret)
	registerPushPlatform(t, h, id, pair, fcmToken, "ios")
	gate := &oauthWaitingPushTransport{base: google, started: make(chan struct{}), release: make(chan struct{})}
	h.push.sender.client = &http.Client{Transport: gate}
	unblock := sync.OnceFunc(func() { close(gate.release) })
	defer unblock()
	postTurn(t, h, id, secret, lifecycleEvent("done", "codex:a", "one", ""), lifecycleEvent("reply", "codex:b", "two", "final-b"))
	awaitSSESignal(t, gate.started, "digest never reached oauth")
	postTurn(t, h, id, secret, lifecycleEvent("failed", "codex:a", "one", ""))
	unblock()
	messages := waitMessages(t, google, 2)
	if len(messages) != 2 {
		t.Fatalf("wrong surviving jobs: %v", messages)
	}
	for _, message := range messages {
		data := lifecycleData(message)
		if data["notificationCount"] != nil || data["sessionKey"] == "codex:a" && data["kind"] != "failed" {
			t.Fatalf("obsolete normal target survived: %v", data)
		}
	}
}

func TestPushDeliveryRequestExpiryAndSilentResolve(t *testing.T) {
	now := time.Now()
	expiry := now.Add(2 * time.Second).UnixMilli()
	data := map[string]string{"kind": "approval", "expiresAt": strconv.FormatInt(expiry, 10)}
	if !pushPayloadUnexpired(data, now) || pushPayloadUnexpired(data, time.UnixMilli(expiry)) || pushPayloadDeadline(data, now).UnixMilli() != expiry {
		t.Fatal("request deadline was extended")
	}
	data["kind"] = "resolved"
	config := apnsTaskConfig(data, "request")
	aps := config["payload"].(map[string]any)["aps"].(map[string]any)
	if aps["alert"] != nil || aps["sound"] != nil || config["headers"].(map[string]string)["apns-push-type"] != "background" {
		t.Fatal("resolved must be silent")
	}
}

func TestPushOutboxInsertFailureDoesNotAcknowledgeEvent(t *testing.T) {
	h, google, secret, _ := lifecycleFixture(t, "atomic-outbox-root")
	id := "atomic-outbox-root"
	if _, err := h.durable.db.Exec(`CREATE TRIGGER outbox_reject BEFORE INSERT ON push_jobs BEGIN SELECT RAISE(FAIL,'synthetic outbox failure'); END`); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"deviceId": id, "batchId": "00000000-0000-4000-8000-000000000221", "events": []any{lifecycleEvent("failed", "codex:outbox", "one", "")}}
	w := deviceRequest(t, h, "/bridge/events", id, secret, "", body, "")
	if w.Code != 503 {
		t.Fatalf("outbox failure was acknowledged: %d %s", w.Code, w.Body)
	}
	var events, notices int
	if err := h.durable.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := h.durable.db.QueryRow(`SELECT COUNT(*) FROM push_notices`).Scan(&notices); err != nil {
		t.Fatal(err)
	}
	if events != 0 || notices != 0 {
		t.Fatalf("partial commit: %d events, %d notices", events, notices)
	}
	if _, err := h.durable.db.Exec(`DROP TRIGGER outbox_reject`); err != nil {
		t.Fatal(err)
	}
	w = deviceRequest(t, h, "/bridge/events", id, secret, "", body, "")
	if w.Code != 200 {
		t.Fatalf("retry failed: %d %s", w.Code, w.Body)
	}
	if len(waitMessages(t, google, 1)) != 1 {
		t.Fatal("retry lost the task notification")
	}
}

func TestPushDisabledSkipsExtraProjectionStorage(t *testing.T) {
	h := newDeviceHub(t, Config{DataDir: t.TempDir()})
	id, secret := "no-push-storage", strings.Repeat("b", 64)
	pairPhone(t, h, id, secret)
	if _, err := h.durable.db.Exec(`CREATE TRIGGER no_notifications BEFORE INSERT ON push_notices BEGIN SELECT RAISE(FAIL,'disabled push must not write projection'); END`); err != nil {
		t.Fatal(err)
	}
	postTurn(t, h, id, secret, lifecycleEvent("failed", "codex:normal-mailbox", "one", ""))
}
