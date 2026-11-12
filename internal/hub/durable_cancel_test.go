package hub

// State-machine regressions use actual handlers and isolated persistent stores.
import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

func cancelContractHTTP(h *Hub, token, id, ticket string) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(map[string]string{"deviceId": "pc", "operationId": id, "ticket": ticket})
	r := httptest.NewRequest(http.MethodPost, "/app/operations/cancel", bytes.NewReader(raw))
	r.Header.Set("X-Salcara-Pair-Token", token)
	w := httptest.NewRecorder()
	h.handleOperationCancel(w, r, "receipt-owner")
	return w
}

func cancelContractFixture(t *testing.T) (*Hub, *bridgeConn, string, string, string, string) {
	t.Helper()
	h, conn, token, credential := receiptLookupFixture(t)
	p := h.DurableStorageStatus().Policy
	p.OfflineQueueEnabled = true
	if err := h.SetStoragePolicy(p); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.accounts["receipt-owner"].devices["pc"].conn = nil
	h.mu.Unlock()
	id, ticket := queueRecoveryOperation(t, h, "pc", token, receiptRequestID)
	key := shaHex(shaHex("receipt-owner\x00pc\x00"+credential) + "\x00" + id)
	return h, conn, token, id, ticket, key
}

func requireCancelContract(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var out struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || w.Code != status || out.Code != code {
		t.Fatalf("cancel HTTP=%d body=%s, want %d %s", w.Code, w.Body, status, code)
	}
}

func cancelContractState(t *testing.T, h *Hub, key string) (string, int64) {
	t.Helper()
	var state string
	var size int64
	if err := h.durable.db.QueryRow(`SELECT state,bytes FROM offline_queue WHERE id=?`, key).Scan(&state, &size); err != nil {
		t.Fatal(err)
	}
	return state, size
}

func TestCancelContractCancelWinsRestartAndReplay(t *testing.T) {
	h, conn, token, id, ticket, key := cancelContractFixture(t)
	h.receipts.mu.Lock()
	done := h.receipts.entries[key].done
	h.receipts.mu.Unlock()
	requireCancelContract(t, cancelContractHTTP(h, token, id, ticket), 200, "operation_cancelled")
	select {
	case <-done:
	default:
		t.Fatal("original receipt waiter was not completed")
	}
	state, size := cancelContractState(t, h, key)
	if state != "cancelled" || size != 0 {
		t.Fatalf("cancelled queue retained payload: %s %d", state, size)
	}
	// Model the actual dispatcher's stale SELECT result at its SQL CAS edge.
	// The paired test below also drives the real dispatcher through the other order.
	result, err := h.durable.db.Exec(`UPDATE offline_queue SET state='claimed' WHERE id=? AND state='queued'`, key)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 0 {
		t.Fatalf("stale claim changed cancelled row: %d %v", changed, err)
	}
	h.mu.Lock()
	h.accounts["receipt-owner"].devices["pc"].conn = conn
	h.mu.Unlock()
	h.dispatchOfflineQueue()
	if len(conn.cmds) != 0 {
		t.Fatal("cancelled task dispatched")
	}
	p := h.DurableStorageStatus().Policy
	p.OfflineQueueEnabled = false
	if err := h.SetStoragePolicy(p); err != nil {
		t.Fatal(err)
	}
	requireCancelContract(t, cancelContractHTTP(h, token, id, ticket), 200, "operation_cancelled")
	h.Close()
	restored, err := New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restored.Close)
	assertCancelled := func() {
		t.Helper()
		out := readRecoveryOperation(t, restored, token, id, ticket)
		if out["state"] != "cancelled" || out["code"] != "operation_cancelled" || out["ok"] != false {
			t.Fatalf("lost cancellation fact: %v", out)
		}
	}
	assertCancelled()
	if _, err := restored.durable.db.Exec(`UPDATE replies SET expires=? WHERE id=?`, nowMs()-1, key); err != nil {
		t.Fatal(err)
	}
	if err := restored.RunStorageCleanup(); err != nil {
		t.Fatal(err)
	}
	assertCancelled()
	requireCancelContract(t, cancelContractHTTP(restored, token, id, ticket), 200, "operation_cancelled")
	prepared := prepareHTTP(t, restored, token, receiptRequestID, json.RawMessage(`{"type":"session.send","tool":"codex","text":"fixture"}`))
	if prepared["operationId"] != id || prepared["ticket"] != ticket {
		t.Fatal("cancel retry allocated a new identity")
	}
	for _, receiptOnly := range []bool{false, true} {
		body := map[string]any{"deviceId": "pc", "requestId": id, "executionTicket": ticket, "receiptOnly": receiptOnly,
			"command": map[string]any{"type": "session.send", "tool": "codex", "text": "fixture", "operationId": id}}
		requireCancelContract(t, lookupReceiptHTTP(restored, token, "receipt-owner", body), 409, "operation_cancelled")
	}
}

func TestCancelContractClaimWinsWithoutAgentStop(t *testing.T) {
	h, conn, token, id, ticket, key := cancelContractFixture(t)
	h.remoteAccessGate.Lock()
	locked := true
	defer func() {
		if locked {
			h.remoteAccessGate.Unlock()
		}
	}()
	h.mu.Lock()
	h.accounts["receipt-owner"].devices["pc"].conn = conn
	h.mu.Unlock()
	h.dispatchOfflineQueue() // The CAS is synchronous; worker waits at source gate.
	if state, _ := cancelContractState(t, h, key); state != "claimed" {
		t.Fatalf("not claimed: %s", state)
	}
	finished := make(chan *httptest.ResponseRecorder, 1)
	go func() { finished <- cancelContractHTTP(h, token, id, ticket) }()
	select {
	case w := <-finished:
		requireCancelContract(t, w, 409, "operation_not_cancellable")
	case <-time.After(3 * time.Second):
		t.Fatal("cancel blocked behind execution gate")
	}
	if len(conn.cmds) != 0 {
		t.Fatal("command passed held gate")
	}
	h.remoteAccessGate.Unlock()
	locked = false
	replyReceipt(t, h, conn, `{"delivered":true}`)
	deadline := time.Now().Add(3 * time.Second)
	for readRecoveryOperation(t, h, token, id, ticket)["state"] != "completed" {
		if time.Now().After(deadline) {
			t.Fatal("claimed command did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	if len(conn.cmds) != 0 {
		t.Fatal("cancellation generated another command or Agent interrupt")
	}
}

func TestCancelContractReplyWriteFailureRollsBack(t *testing.T) {
	h, conn, token, id, ticket, key := cancelContractFixture(t)
	h.receipts.mu.Lock()
	entry := h.receipts.entries[key]
	done := entry.done
	h.receipts.mu.Unlock()
	// One isolated queued row; fail the second statement, after the queue CAS.
	if _, err := h.durable.db.Exec(`CREATE TRIGGER audit_cancel_reply_abort BEFORE INSERT ON replies BEGIN SELECT RAISE(ABORT,'synthetic cancellation failure'); END`); err != nil {
		t.Fatal(err)
	}
	requireCancelContract(t, cancelContractHTTP(h, token, id, ticket), 503, "operation_cancel_unavailable")
	state, size := cancelContractState(t, h, key)
	if state != "queued" || size == 0 {
		t.Fatalf("partial cancellation committed: %s %d", state, size)
	}
	var count int
	if err := h.durable.db.QueryRow(`SELECT COUNT(*) FROM replies WHERE id=?`, key).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("reply survived aborted transaction")
	}
	h.receipts.mu.Lock()
	completed := entry.completed
	h.receipts.mu.Unlock()
	if completed {
		t.Fatal("in-memory receipt completed before durable commit")
	}
	select {
	case <-done:
		t.Fatal("waiter released for rolled-back cancellation")
	default:
	}
	if len(conn.cmds) != 0 {
		t.Fatal("failed cancellation dispatched work")
	}
	if _, err := h.durable.db.Exec(`DROP TRIGGER audit_cancel_reply_abort`); err != nil {
		t.Fatal(err)
	}
	requireCancelContract(t, cancelContractHTTP(h, token, id, ticket), 200, "operation_cancelled")
}

func waitCancelContractReceiptLock(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		buf := make([]byte, 256<<10)
		stacks := string(buf[:runtime.Stack(buf, true)])
		for _, stack := range strings.Split(stacks, "\n\n") {
			if strings.Contains(stack, "(*Hub).handleOperationCancel") && strings.Contains(stack, "sync.(*Mutex)") {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("cancel did not wait at receipt lock")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCancelContractCommitFailureRollsBack(t *testing.T) {
	h, _, token, id, ticket, key := cancelContractFixture(t)
	for _, statement := range []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE cancel_parent (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE cancel_child (id INTEGER REFERENCES cancel_parent(id) DEFERRABLE INITIALLY DEFERRED)`,
		`CREATE TRIGGER cancel_commit_failure AFTER INSERT ON replies BEGIN INSERT INTO cancel_child VALUES(1); END`,
	} {
		if _, err := h.durable.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	requireCancelContract(t, cancelContractHTTP(h, token, id, ticket), 503, "operation_cancel_unavailable")
	if state, size := cancelContractState(t, h, key); state != "queued" || size == 0 {
		t.Fatalf("failed commit changed queue: %s %d", state, size)
	}
	h.receipts.mu.Lock()
	completed := h.receipts.entries[key].completed
	h.receipts.mu.Unlock()
	if completed {
		t.Fatal("failed commit acknowledged in memory")
	}
	if _, err := h.durable.db.Exec(`DROP TRIGGER cancel_commit_failure`); err != nil {
		t.Fatal(err)
	}
	requireCancelContract(t, cancelContractHTTP(h, token, id, ticket), 200, "operation_cancelled")
}

func TestCancelContractExpiredPreparedAndMalformedRequests(t *testing.T) {
	h, conn, token, id, ticket, key := cancelContractFixture(t)
	for _, raw := range []string{
		`{"deviceId":"pc","operationId":"` + id + `","ticket":"` + ticket + `","ticket":"` + ticket + `"}`,
		`{"deviceId":"pc","operationId":"` + id + `","ticket":"` + ticket + `","command":{}}`,
		`{"deviceId":"pc","operationId":"` + id + `","ticket":null}`,
	} {
		r := httptest.NewRequest("POST", "/app/operations/cancel", strings.NewReader(raw))
		r.Header.Set("X-Salcara-Pair-Token", token)
		w := httptest.NewRecorder()
		h.handleOperationCancel(w, r, "receipt-owner")
		if w.Code != 400 {
			t.Fatalf("malformed cancellation accepted: %d", w.Code)
		}
	}
	if w := cancelContractHTTP(h, strings.Repeat("ff", 32), id, ticket); w.Code != 403 {
		t.Fatalf("wrong phone: %d", w.Code)
	}
	if w := cancelContractHTTP(h, token, id, ticket+"bad"); w.Code != 403 {
		t.Fatalf("wrong ticket: %d", w.Code)
	}
	if _, err := h.durable.db.Exec(`UPDATE offline_queue SET deadline=? WHERE id=?`, nowMs()-1, key); err != nil {
		t.Fatal(err)
	}
	requireCancelContract(t, cancelContractHTTP(h, token, id, ticket), 409, "offline_dispatch_expired")
	if out := readRecoveryOperation(t, h, token, id, ticket); out["state"] != "expired" || out["code"] != "offline_dispatch_expired" || out["ok"] != false {
		t.Fatalf("expiry misreported: %v", out)
	}
	prepared := prepareHTTP(t, h, token, "12345678-0000-4000-8000-000000000001", json.RawMessage(`{"type":"session.send","text":"prepared"}`))
	requireCancelContract(t, cancelContractHTTP(h, token, prepared["operationId"].(string), prepared["ticket"].(string)), 409, "operation_not_queued")
	if len(conn.cmds) != 0 {
		t.Fatal("cancel validation dispatched work")
	}
}

func TestCancelContractRechecksRevokedPhoneAfterReceiptWait(t *testing.T) {
	for _, pending := range []bool{false, true} {
		name := "retired"
		if pending {
			name = "revoke-pending"
		}
		t.Run(name, func(t *testing.T) {
			f := newBotStorageFixture(t)
			p := f.h.DurableStorageStatus().Policy
			p.OfflineQueueEnabled = true
			if err := f.h.SetStoragePolicy(p); err != nil {
				t.Fatal(err)
			}
			headers := map[string]string{"X-Salcara-Bot-Token": f.token}
			command := map[string]any{"type": "session.send", "tool": "codex", "text": "fixture"}
			w := botRequest(t, f.h, "POST", "/v1/app/commands/prepare", map[string]any{"deviceId": f.deviceID, "requestId": receiptRequestID, "command": command}, headers)
			if w.Code != 200 {
				t.Fatalf("prepare: %d %s", w.Code, w.Body)
			}
			var prepared struct {
				ID     string `json:"operationId"`
				Ticket string `json:"ticket"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &prepared); err != nil {
				t.Fatal(err)
			}
			command["operationId"] = prepared.ID
			w = botRequest(t, f.h, "POST", "/v1/app/commands", map[string]any{"deviceId": f.deviceID, "requestId": prepared.ID, "executionTicket": prepared.Ticket, "command": command}, headers)
			if w.Code != 202 {
				t.Fatalf("queue: %d %s", w.Code, w.Body)
			}
			body := map[string]string{"deviceId": f.deviceID, "operationId": prepared.ID, "ticket": prepared.Ticket}
			f.h.receipts.mu.Lock()
			locked := true
			defer func() {
				if locked {
					f.h.receipts.mu.Unlock()
				}
			}()
			finished := make(chan *httptest.ResponseRecorder, 1)
			go func() { finished <- botRequest(t, f.h, "POST", "/v1/app/operations/cancel", body, headers) }()
			waitCancelContractReceiptLock(t)
			f.revoke(t, pending)
			f.h.receipts.mu.Unlock()
			locked = false
			select {
			case w := <-finished:
				requireBotStorageDenied(t, w, pending)
			case <-time.After(3 * time.Second):
				t.Fatal("cancel stalled after revoke")
			}
			var queueCancelled, replyCancelled int
			if err := f.h.durable.db.QueryRow(`SELECT COUNT(*) FROM offline_queue WHERE state='cancelled'`).Scan(&queueCancelled); err != nil {
				t.Fatal(err)
			}
			if err := f.h.durable.db.QueryRow(`SELECT COUNT(*) FROM replies WHERE state='cancelled'`).Scan(&replyCancelled); err != nil {
				t.Fatal(err)
			}
			if queueCancelled != 0 || replyCancelled != 0 {
				t.Fatal("revoked phone persisted cancellation")
			}
		})
	}
}
