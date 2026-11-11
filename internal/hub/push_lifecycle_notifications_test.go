package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

const lifecyclePrivateText = "lifecycle-private-task-and-approval-text"
const lifecycleBarrierSession = "codex:lifecycle-barrier"

func lifecycleEvent(kind, session, turn, id string) map[string]any {
	e := map[string]any{"sessionKey": session, "tool": "codex", "ts": nowMs(), "text": lifecyclePrivateText}
	if turn != "" {
		e["turnId"] = turn
	}
	switch kind {
	case "reply":
		e["type"], e["role"], e["final"], e["messagePhase"], e["id"] = "message", "assistant", true, "final_answer", id
	case "done", "failed", "started":
		e["type"], e["status"] = "turn", kind
		if kind == "done" {
			e["status"] = "completed"
		}
	case "approval", "question", "resolved":
		e["type"], e["approvalId"], e["kind"] = "approval.request", id, "command"
		e["title"], e["detail"] = lifecyclePrivateText, lifecyclePrivateText
		if kind == "question" {
			e["kind"] = "question"
		}
		if kind == "resolved" {
			e["type"] = "approval.resolved"
		}
	}
	return e
}

func lifecycleData(message map[string]any) map[string]any {
	return message["message"].(map[string]any)["data"].(map[string]any)
}

// The single sender processes this terminal marker after all preceding jobs.
// This makes negative assertions depend on a queue boundary, not a short sleep.
func lifecycleDrain(t *testing.T, h *Hub, google *fakeGoogle, id, secret, marker string) []map[string]any {
	t.Helper()
	postTurn(t, h, id, secret, lifecycleEvent("failed", lifecycleBarrierSession, marker, ""))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		messages := google.messages()
		found := false
		for _, message := range messages {
			data := lifecycleData(message)
			if data["sessionKey"] == lifecycleBarrierSession && strings.HasSuffix(fmt.Sprint(data["id"]), ":"+marker) {
				found = true
			}
		}
		if found {
			var results []map[string]any
			for _, message := range messages {
				if lifecycleData(message)["sessionKey"] != lifecycleBarrierSession {
					results = append(results, message)
				}
			}
			raw, _ := json.Marshal(messages)
			if strings.Contains(string(raw), lifecyclePrivateText) {
				t.Fatal("task or approval text leaked into notification metadata")
			}
			return results
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("push sender did not reach the lifecycle queue boundary")
	return nil
}

func lifecycleFixture(t *testing.T, id string) (*Hub, *fakeGoogle, string, string) {
	t.Helper()
	h, google := pushHub(t, t.TempDir())
	secret := strings.Repeat("9", 64)
	pair := pairPhone(t, h, id, secret)
	registerPushScope(t, h, id, pair, fcmToken, notificationScopeA)
	return h, google, secret, pair
}

func TestPushLifecycleFailureUpgradesTheSameNoticeAndNeverDowngrades(t *testing.T) {
	for _, first := range []string{"reply", "done", "failed"} {
		t.Run(first, func(t *testing.T) {
			const id, session, turn = "lifecycle-priority", "codex:priority", "one-turn"
			h, google, secret, _ := lifecycleFixture(t, id)
			postTurn(t, h, id, secret, lifecycleEvent(first, session, turn, "first-final"))
			waitMessages(t, google, 1)
			if first != "failed" {
				postTurn(t, h, id, secret, lifecycleEvent("failed", session, turn, ""))
			}
			postTurn(t, h, id, secret, lifecycleEvent("reply", session, turn, "late-final"), lifecycleEvent("done", session, turn, ""), lifecycleEvent("failed", session, turn, ""))
			messages := lifecycleDrain(t, h, google, id, secret, "priority-drain")
			want := 2
			if first == "failed" {
				want = 1
			}
			if len(messages) != want {
				t.Fatalf("%s then failed/ordinary retries: want %d notices, got %d", first, want, len(messages))
			}
			last := lifecycleData(messages[len(messages)-1])
			if last["kind"] != "failed" || last["id"] != lifecycleData(messages[0])["id"] {
				t.Fatalf("failure must upgrade the original identity without downgrade: %v", last)
			}
		})
	}
}

func TestPushLifecycleIndependentFailuresAreNotSessionRateLimited(t *testing.T) {
	const id = "lifecycle-two-failures"
	h, google, secret, _ := lifecycleFixture(t, id)
	postTurn(t, h, id, secret, lifecycleEvent("failed", "codex:one-session", "failure-one", ""), lifecycleEvent("failed", "codex:one-session", "failure-two", ""))
	messages := lifecycleDrain(t, h, google, id, secret, "independent-drain")
	if len(messages) != 2 || lifecycleData(messages[0])["id"] == lifecycleData(messages[1])["id"] {
		t.Fatalf("two immediate independent failures must both survive: %v", messages)
	}
}

func TestPushLifecycleStartedDefinesLegacyTurnBoundaries(t *testing.T) {
	const id, session = "lifecycle-started", "codex:legacy-started"
	h, google, secret, _ := lifecycleFixture(t, id)
	var starts []int64
	for _, replyID := range []string{"first-final", "second-final"} {
		postTurn(t, h, id, secret, lifecycleEvent("started", session, "", ""))
		h.mu.Lock()
		starts = append(starts, h.accounts[deviceAccount(id)].seq)
		h.mu.Unlock()
		postTurn(t, h, id, secret, lifecycleEvent("reply", session, "", replyID), lifecycleEvent("done", session, "", ""))
	}
	messages := lifecycleDrain(t, h, google, id, secret, "started-drain")
	if len(messages) != 2 {
		t.Fatalf("two started boundaries must yield two notices, not one per terminal event: %d", len(messages))
	}
	for i, message := range messages {
		want := fmt.Sprintf("%s:task:%s:%s:legacy:%d", notificationScopeA, id, session, starts[i])
		if lifecycleData(message)["id"] != want {
			t.Fatalf("legacy turn did not retain its started sequence: got %v, want %s", lifecycleData(message)["id"], want)
		}
	}
}

func TestPushLifecycleCompletedStartDoesNotOwnFutureBoundarylessReplies(t *testing.T) {
	const id, session = "lifecycle-ended-start", "codex:ended-start"
	h, google, secret, _ := lifecycleFixture(t, id)
	postTurn(t, h, id, secret, lifecycleEvent("started", session, "", ""), lifecycleEvent("reply", session, "", "started-final"), lifecycleEvent("done", session, "", ""))
	postTurn(t, h, id, secret, lifecycleEvent("reply", session, "", "new-independent-final"), lifecycleEvent("done", session, "", ""))
	messages := lifecycleDrain(t, h, google, id, secret, "ended-start-drain")
	if len(messages) != 2 || lifecycleData(messages[0])["id"] == lifecycleData(messages[1])["id"] {
		t.Fatalf("a completed started turn must not swallow later boundaryless results: %v", messages)
	}
}

func TestPushLifecycleBoundarylessComplementsMergeButNewRepliesDoNot(t *testing.T) {
	for _, order := range [][]string{{"reply", "done", "reply", "done"}, {"done", "reply", "reply", "done"}, {"reply", "reply"}} {
		t.Run(strings.Join(order, "-"), func(t *testing.T) {
			const id, session = "lifecycle-complement", "codex:no-start"
			h, google, secret, _ := lifecycleFixture(t, id)
			for i, kind := range order {
				postTurn(t, h, id, secret, lifecycleEvent(kind, session, "", fmt.Sprintf("final-%d", i)))
			}
			messages := lifecycleDrain(t, h, google, id, secret, "complement-drain")
			if len(messages) != 2 || lifecycleData(messages[0])["id"] == lifecycleData(messages[1])["id"] {
				t.Fatalf("only complementary events in the short window may merge; independent replies need distinct notices: %v", messages)
			}
		})
	}
}

func TestPushLifecycleBoundarylessReplyRetryKeepsItsSourceIdentity(t *testing.T) {
	const id, session = "lifecycle-reply-retry", "codex:no-turn-retry"
	h, google, secret, _ := lifecycleFixture(t, id)
	reply := lifecycleEvent("reply", session, "", "same-source-message")
	postTurn(t, h, id, secret, reply)
	postTurn(t, h, id, secret, reply) // Legacy clients can retry without a batch ID.
	messages := lifecycleDrain(t, h, google, id, secret, "same-source-drain")
	if len(messages) != 1 {
		t.Fatalf("reuploading one source message must not invent a new legacy turn: %d", len(messages))
	}
	postTurn(t, h, id, secret, lifecycleEvent("reply", session, "", "a-new-source-message"))
	messages = lifecycleDrain(t, h, google, id, secret, "new-source-drain")
	if len(messages) != 2 || lifecycleData(messages[0])["id"] == lifecycleData(messages[1])["id"] {
		t.Fatalf("a different source reply must remain a new independent result: %v", messages)
	}
	postTurn(t, h, id, secret, reply) // Replayed older pages can follow a newer result.
	messages = lifecycleDrain(t, h, google, id, secret, "older-source-replay")
	if len(messages) != 2 {
		t.Fatalf("an older replay must remain deduped after a newer reply: %d", len(messages))
	}
}

func TestPushLifecycleRequestsHaveIndependentIdentitiesAndRealExpiry(t *testing.T) {
	const id, session = "lifecycle-requests", "codex:requests"
	h, google, secret, _ := lifecycleFixture(t, id)
	oldLive := lifecycleEvent("approval", session, "same-turn", "approval-one")
	oldLive["ts"], oldLive["expiresAt"] = nowMs()-20*time.Minute.Milliseconds(), nowMs()+time.Hour.Milliseconds()
	question := lifecycleEvent("question", session, "same-turn", "question-two")
	question["expiresAt"] = nowMs() + time.Hour.Milliseconds()
	expired := lifecycleEvent("approval", session, "same-turn", "already-expired")
	expired["expiresAt"] = nowMs() - 1
	postTurn(t, h, id, secret, lifecycleEvent("reply", session, "same-turn", "final"), oldLive, oldLive, question, question, expired)
	messages := lifecycleDrain(t, h, google, id, secret, "request-drain")
	if len(messages) != 3 {
		t.Fatalf("one ordinary result and two independent live requests wanted, got %d", len(messages))
	}
	want := map[string]string{
		notificationScopeA + ":task:" + id + ":" + session + ":same-turn":        "reply",
		notificationScopeA + ":approval:" + id + ":" + session + ":approval-one": "approval",
		notificationScopeA + ":approval:" + id + ":" + session + ":question-two": "question",
	}
	for _, message := range messages {
		data := lifecycleData(message)
		if kind, ok := want[fmt.Sprint(data["id"])]; !ok || data["kind"] != kind {
			t.Fatalf("unexpected request identity/kind: %v", data)
		}
		delete(want, fmt.Sprint(data["id"]))
	}
	if len(want) != 0 {
		t.Fatalf("live requests were lost: %v", want)
	}
}

func TestPushLifecycleRequestResolvedOrExpiredDuringOAuthDoesNotAlert(t *testing.T) {
	for _, outcome := range []string{"resolved", "expired"} {
		t.Run(outcome, func(t *testing.T) {
			const id, session = "lifecycle-request-wait", "codex:pending-request"
			h, google, secret, _ := lifecycleFixture(t, id)
			transport := &oauthWaitingPushTransport{base: google, started: make(chan struct{}), release: make(chan struct{})}
			h.push.sender.client = &http.Client{Transport: transport}
			unblock := sync.OnceFunc(func() { close(transport.release) })
			defer unblock()
			request := lifecycleEvent("approval", session, "request-turn", "request-one")
			request["expiresAt"] = nowMs() + time.Hour.Milliseconds()
			expiry := time.Now().Add(2 * time.Second)
			if outcome == "expired" {
				request["expiresAt"] = expiry.UnixMilli()
			}
			postTurn(t, h, id, secret, request)
			awaitSSESignal(t, transport.started, "approval request did not reach the OAuth gate")
			if outcome == "resolved" {
				postTurn(t, h, id, secret, lifecycleEvent("resolved", session, "request-turn", "request-one"))
			} else {
				time.Sleep(time.Until(expiry) + 10*time.Millisecond)
			}
			unblock()
			messages := lifecycleDrain(t, h, google, id, secret, "request-wait-drain")
			if outcome == "expired" {
				if len(messages) != 0 {
					t.Fatalf("expired queued request reached FCM: %v", messages)
				}
				return
			}
			if len(messages) != 1 || lifecycleData(messages[0])["kind"] != "resolved" {
				t.Fatalf("only the silent withdrawal may survive request resolution: %v", messages)
			}
			if _, hasAlert := messages[0]["message"].(map[string]any)["notification"]; hasAlert {
				t.Fatal("resolved request must not carry a visible FCM notification")
			}
		})
	}
}

func TestPushLifecycleFailurePriorityAndPendingQueueSurviveRestart(t *testing.T) {
	const id, session, turn = "lifecycle-restart", "codex:restart", "restart-turn"
	dir := t.TempDir()
	h, google := pushHub(t, dir)
	secret := strings.Repeat("8", 64)
	pair := pairPhone(t, h, id, secret)
	registerPushScope(t, h, id, pair, fcmToken, notificationScopeA)
	google.response = func(string) (int, string) {
		messages := google.messages()
		if lifecycleData(messages[len(messages)-1])["sessionKey"] == lifecycleBarrierSession {
			return 200, `{"name":"fixture"}`
		}
		return 503, `{"error":{"status":"UNAVAILABLE"}}`
	}
	postTurn(t, h, id, secret, lifecycleEvent("reply", session, turn, "original-final"))
	waitMessages(t, google, 1)
	postTurn(t, h, id, secret, lifecycleEvent("failed", session, turn, ""))
	lifecycleDrain(t, h, google, id, secret, "before-restart")
	h.Close()
	restarted, google2 := pushHub(t, dir)
	if _, err := restarted.durable.db.Exec(`UPDATE push_jobs SET due=0 WHERE state='pending'`); err != nil {
		t.Fatal(err)
	}
	restarted.recoverPushJobs()
	postTurn(t, restarted, id, secret, lifecycleEvent("reply", session, turn, "late-after-restart"), lifecycleEvent("done", session, turn, ""))
	messages := lifecycleDrain(t, restarted, google2, id, secret, "after-restart")
	if len(messages) != 1 || lifecycleData(messages[0])["kind"] != "failed" {
		t.Fatalf("restart must recover the failure, never an obsolete ordinary result: %v", messages)
	}
	if lifecycleData(messages[0])["id"] != notificationScopeA+":task:"+id+":"+session+":"+turn {
		t.Fatalf("restart changed failure notification identity: %v", lifecycleData(messages[0]))
	}
}

func TestPushLifecycleResolvedProjectionFailureRejectsTheWholeBatch(t *testing.T) {
	const id, session, batch = "lifecycle-atomic-request", "codex:atomic-request", "00000000-0000-4000-8000-000000000211"
	dir := t.TempDir()
	h, google := pushHub(t, dir)
	secret := strings.Repeat("6", 64)
	pair := pairPhone(t, h, id, secret)
	registerPushScope(t, h, id, pair, fcmToken, notificationScopeA)
	google.response = func(string) (int, string) {
		messages := google.messages()
		if lifecycleData(messages[len(messages)-1])["kind"] == "approval" {
			return 503, `{"error":{"status":"UNAVAILABLE"}}`
		}
		return 200, `{"name":"fixture"}`
	}
	request := lifecycleEvent("approval", session, "atomic-turn", "atomic-request")
	request["expiresAt"] = nowMs() + time.Hour.Milliseconds()
	postTurn(t, h, id, secret, request)
	messages := lifecycleDrain(t, h, google, id, secret, "before-projection-failure")
	if len(messages) != 1 || lifecycleData(messages[0])["kind"] != "approval" {
		t.Fatalf("fixture request was not retained for a failed delivery: %v", messages)
	}
	h.mu.Lock()
	account := h.accounts[deviceAccount(id)]
	beforeSeq := account.seq
	scope := h.durableScopeLocked(deviceAccount(id), account.devices[id])
	h.mu.Unlock()
	var beforeEvents int
	if err := h.durable.db.QueryRow(`SELECT COUNT(*) FROM events WHERE scope=?`, scope).Scan(&beforeEvents); err != nil {
		t.Fatal(err)
	}
	if _, err := h.durable.db.Exec(`CREATE TRIGGER lifecycle_notice_failure BEFORE UPDATE ON push_notices BEGIN SELECT RAISE(FAIL, 'synthetic notice projection failure'); END`); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"deviceId": id, "batchId": batch, "events": []any{lifecycleEvent("resolved", session, "atomic-turn", "atomic-request")}}
	failed := deviceRequest(t, h, "/bridge/events", id, secret, "", body, "")
	if failed.Code != http.StatusServiceUnavailable || strings.Contains(failed.Body.String(), `"lastSeq"`) {
		t.Fatalf("projection failure must reject, never acknowledge, the resolution: %d %s", failed.Code, failed.Body)
	}
	h.mu.Lock()
	afterSeq := h.accounts[deviceAccount(id)].seq
	h.mu.Unlock()
	if afterSeq != beforeSeq {
		t.Fatalf("failed resolution advanced the published cursor: %d -> %d", beforeSeq, afterSeq)
	}
	var afterEvents int
	if err := h.durable.db.QueryRow(`SELECT COUNT(*) FROM events WHERE scope=?`, scope).Scan(&afterEvents); err != nil || afterEvents != beforeEvents {
		t.Fatalf("failed projection committed part of its event batch: before=%d after=%d err=%v", beforeEvents, afterEvents, err)
	}
	if _, found, err := h.durable.findBatch(scope, batch); err != nil || found {
		t.Fatalf("failed projection left a successful batch receipt: found=%v err=%v", found, err)
	}
	if _, err := h.durable.db.Exec(`DROP TRIGGER lifecycle_notice_failure`); err != nil {
		t.Fatal(err)
	}
	if retry := deviceRequest(t, h, "/bridge/events", id, secret, "", body, ""); retry.Code != http.StatusOK {
		t.Fatalf("same rejected batch must be retryable: %d %s", retry.Code, retry.Body)
	}
	if _, found, err := h.durable.findBatch(scope, batch); err != nil || !found {
		t.Fatalf("successful retry did not commit its receipt: found=%v err=%v", found, err)
	}
	messages = lifecycleDrain(t, h, google, id, secret, "after-projection-retry")
	if len(messages) != 2 || lifecycleData(messages[1])["kind"] != "resolved" {
		t.Fatalf("successful retry should withdraw the pending request once: %v", messages)
	}
	h.Close()
	restarted, google2 := pushHub(t, dir)
	if _, err := restarted.durable.db.Exec(`UPDATE push_jobs SET due=0 WHERE state='pending'`); err != nil {
		t.Fatal(err)
	}
	restarted.recoverPushJobs()
	postTurn(t, restarted, id, secret, request) // An old page must not revive it either.
	if got := lifecycleDrain(t, restarted, google2, id, secret, "resolved-after-restart"); len(got) != 0 {
		t.Fatalf("restart revived a durably resolved approval: %v", got)
	}
}

func TestPushLifecycleProgressDoesNotWriteTheNoticeProjection(t *testing.T) {
	const id, session = "lifecycle-quiet-progress", "codex:quiet-progress"
	h, google, secret, _ := lifecycleFixture(t, id)
	postTurn(t, h, id, secret, lifecycleEvent("done", session, "finished", ""))
	for _, operation := range []string{"INSERT", "UPDATE"} {
		statement := fmt.Sprintf("CREATE TRIGGER lifecycle_no_notice_%s BEFORE %s ON push_notices BEGIN SELECT RAISE(FAIL, 'progress must not write notice state'); END", operation, operation)
		if _, err := h.durable.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	body := map[string]any{"deviceId": id, "events": []any{
		map[string]any{"type": "message.delta", "sessionKey": session, "id": "stream-one", "delta": lifecyclePrivateText},
		map[string]any{"type": "message.delta", "sessionKey": "codex:new-quiet-session", "id": "stream-two", "delta": lifecyclePrivateText},
	}}
	if response := deviceRequest(t, h, "/bridge/events", id, secret, "", body, ""); response.Code != http.StatusOK {
		t.Fatalf("ordinary progress must be admitted without any notice projection write: %d %s", response.Code, response.Body)
	}
	for _, operation := range []string{"INSERT", "UPDATE"} {
		if _, err := h.durable.db.Exec("DROP TRIGGER lifecycle_no_notice_" + operation); err != nil {
			t.Fatal(err)
		}
	}
	if got := lifecycleDrain(t, h, google, id, secret, "progress-drain"); len(got) != 1 || lifecycleData(got[0])["kind"] != "done" {
		t.Fatalf("streaming progress unexpectedly produced notifications: %v", got)
	}
}

func TestPushLifecycleSamePhoneTokenKeepsDeviceRegistrationsIndependent(t *testing.T) {
	h, google := pushHub(t, t.TempDir())
	secret := strings.Repeat("7", 64)
	const first, second, session = "lifecycle-device-a", "lifecycle-device-b", "codex:shared"
	pairA := pairPhone(t, h, first, secret)
	pairB := pairPhone(t, h, second, secret)
	registerPushScope(t, h, first, pairA, fcmToken, notificationScopeA)
	registerPushScope(t, h, second, pairB, fcmToken, notificationScopeB)
	postTurn(t, h, first, secret, lifecycleEvent("failed", session, "same-turn", ""))
	postTurn(t, h, second, secret, lifecycleEvent("failed", session, "same-turn", ""))
	messages := lifecycleDrain(t, h, google, second, secret, "both-devices")
	if len(messages) != 2 {
		t.Fatalf("one phone must keep both device targets registered: %d", len(messages))
	}
	scopes := map[string]string{first: notificationScopeA, second: notificationScopeB}
	for _, message := range messages {
		data := lifecycleData(message)
		if data["notificationScope"] != scopes[fmt.Sprint(data["deviceId"])] || message["message"].(map[string]any)["token"] != fcmToken {
			t.Fatalf("cross-device routing identity lost: %v", message)
		}
	}
	registerPushScope(t, h, first, pairA, "", notificationScopeA)
	postTurn(t, h, first, secret, lifecycleEvent("failed", session, "after-unregister", ""))
	postTurn(t, h, second, secret, lifecycleEvent("failed", session, "after-unregister", ""))
	messages = lifecycleDrain(t, h, google, second, secret, "one-device-left")
	if len(messages) != 3 || lifecycleData(messages[2])["deviceId"] != second {
		t.Fatalf("unregistering A must not unsubscribe B or keep A: %v", messages)
	}
}
