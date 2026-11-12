package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// Regression fixtures use isolated temporary Hub data and no real Agent.
func recoveryQueuePrepare(t *testing.T, h *Hub, token, intent string) (string, string, map[string]any) {
	t.Helper()
	command := json.RawMessage(`{"type":"session.send","tool":"codex","text":"fixture"}`)
	p := prepareHTTP(t, h, token, intent, command)
	id, ticket := p["operationId"].(string), p["ticket"].(string)
	return id, ticket, map[string]any{"deviceId": "pc", "requestId": id, "executionTicket": ticket,
		"command": map[string]any{"type": "session.send", "tool": "codex", "text": "fixture", "operationId": id}}
}

func recoveryQueueRead(t *testing.T, h *Hub, token, id, ticket string) map[string]any {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/app/operations?deviceId=pc&operationId="+id+"&ticket="+url.QueryEscape(ticket), nil)
	r.Header.Set("X-Salcara-Pair-Token", token)
	w := httptest.NewRecorder()
	h.handleOperationRead(w, r, "receipt-owner")
	if w.Code != 200 {
		t.Fatalf("operation read=%d", w.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func recoveryQueueEnable(t *testing.T, h *Hub) {
	t.Helper()
	p := h.DurableStorageStatus().Policy
	p.OfflineQueueEnabled = true
	if err := h.SetStoragePolicy(p); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.accounts["receipt-owner"].devices["pc"].conn = nil
	h.mu.Unlock()
}

func TestRecoveryQueueCapacityKeepsUnacceptedIntentPrepared(t *testing.T) {
	h, conn, token, credential := receiptLookupFixture(t)
	recoveryQueueEnable(t, h)
	var fifthID, fifthTicket string
	for i := 0; i < 5; i++ {
		id, ticket, body := recoveryQueuePrepare(t, h, token, fmt.Sprintf("20000000-0000-4000-8000-%012d", i+1))
		w := lookupReceiptHTTP(h, token, "receipt-owner", body)
		if i < 4 && w.Code != 202 {
			t.Fatalf("fixture queue %d status=%d", i, w.Code)
		}
		if i == 4 {
			fifthID, fifthTicket = id, ticket
			var out struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if w.Code != 429 || out.Code != "offline_queue_capacity" {
				t.Fatalf("observed capacity behavior changed: HTTP=%d code=%s", w.Code, out.Code)
			}
		}
	}
	key := shaHex(shaHex("receipt-owner\x00pc\x00"+credential) + "\x00" + fifthID)
	var queued, replyCount int
	if err := h.durable.db.QueryRow(`SELECT COUNT(*) FROM offline_queue WHERE state='queued'`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if err := h.durable.db.QueryRow(`SELECT COUNT(*) FROM replies WHERE id=?`, key).Scan(&replyCount); err != nil {
		t.Fatal(err)
	}
	out := recoveryQueueRead(t, h, token, fifthID, fifthTicket)
	if queued != 4 || replyCount != 0 || out["state"] != "prepared" || h.receipts.entries[key] != nil || len(conn.cmds) != 0 {
		t.Fatal("capacity rejection consumed a replay fence or changed the prepared intent")
	}

}

func TestRecoveryQueueExpiredReceiptAfterRestartReleasesBody(t *testing.T) {
	h, conn, token, credential := receiptLookupFixture(t)
	recoveryQueueEnable(t, h)
	id, _, body := recoveryQueuePrepare(t, h, token, "30000000-0000-4000-8000-000000000001")
	if w := lookupReceiptHTTP(h, token, "receipt-owner", body); w.Code != 202 {
		t.Fatalf("fixture queue=%d", w.Code)
	}
	key := shaHex(shaHex("receipt-owner\x00pc\x00"+credential) + "\x00" + id)
	h.Close()
	// Simulate elapsed retention in this fixture's files/database only.
	path := filepath.Join(h.cfg.DataDir, "command-receipts", key+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var disk durableCommandReceipt
	if err = json.Unmarshal(raw, &disk); err != nil {
		t.Fatal(err)
	}
	disk.ExpiresAt = nowMs() - 1
	raw, err = json.Marshal(disk)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := newDurableStore(h.cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`UPDATE offline_queue SET deadline=? WHERE id=?`, nowMs()-1, key)
	s.close()
	if err != nil {
		t.Fatal(err)
	}
	next, err := New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(next.Close)
	if next.receipts.entries[key] != nil {
		t.Fatal("expired v2 receipt should have been removed at restart")
	}
	for i := 0; i < 3; i++ {
		next.dispatchOfflineQueue()
	}
	var state string
	var size int64
	if err = next.durable.db.QueryRow(`SELECT state,bytes FROM offline_queue WHERE id=?`, key).Scan(&state, &size); err != nil {
		t.Fatal(err)
	}
	if state != "dispatch_expired" || size != 0 || len(conn.cmds) != 0 {
		t.Fatalf("observed recovery behavior changed: state=%s bytes=%d", state, size)
	}
	rep, status, stored, err := next.durable.reply(key)
	if err != nil || status != 409 || stored != "dispatch_expired" || rep.Code != "offline_dispatch_expired" {
		t.Fatalf("missing terminal evidence: %s %d %v", stored, status, err)
	}
}

func TestRecoveryQueueInsertFailurePreservesKnownRejection(t *testing.T) {
	h, conn, token, _ := receiptLookupFixture(t)
	recoveryQueueEnable(t, h)
	id, ticket, body := recoveryQueuePrepare(t, h, token, receiptRequestID)
	if _, err := h.durable.db.Exec(`CREATE TRIGGER reject_queue BEFORE INSERT ON offline_queue BEGIN SELECT RAISE(ABORT,'synthetic queue write failure'); END`); err != nil {
		t.Fatal(err)
	}
	requireCancelContract(t, lookupReceiptHTTP(h, token, "receipt-owner", body), 409, "offline_queue_registration_failed")
	if _, err := h.durable.db.Exec(`DROP TRIGGER reject_queue`); err != nil {
		t.Fatal(err)
	}
	h.Close()
	restored, err := New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restored.Close)
	out := readRecoveryOperation(t, restored, token, id, ticket)
	if out["state"] != "not_dispatched" || out["code"] != "offline_queue_registration_failed" {
		t.Fatalf("lost definite rejection: %v", out)
	}
	requireCancelContract(t, lookupReceiptHTTP(restored, token, "receipt-owner", body), 409, "offline_queue_registration_failed")
	if len(conn.cmds) != 0 {
		t.Fatal("rejected queue item dispatched")
	}
}

func TestRecoveryQueueUnreadableBodyNeverDispatches(t *testing.T) {
	h, conn, token, id, ticket, key := cancelContractFixture(t)
	if _, err := h.durable.db.Exec(`UPDATE offline_queue SET body=x'00' WHERE id=?`, key); err != nil {
		t.Fatal(err)
	}
	h.dispatchOfflineQueue()
	state, size := cancelContractState(t, h, key)
	if state != "queue_unavailable" || size != 0 {
		t.Fatalf("unreadable body stranded: %s %d", state, size)
	}
	out := readRecoveryOperation(t, h, token, id, ticket)
	if out["state"] != "not_dispatched" || out["code"] != "offline_queue_unavailable" {
		t.Fatalf("bad recovery state: %v", out)
	}
	if len(conn.cmds) != 0 {
		t.Fatal("corrupt queue item dispatched")
	}
}

func TestRecoveryQueueRequiredUpdateReturnsDefiniteRejection(t *testing.T) {
	h, conn, token, id, ticket, _ := cancelContractFixture(t)
	h.cfg.DesktopMinimumVersion = "1.6.8"
	h.mu.Lock()
	d := h.accounts["receipt-owner"].devices["pc"]
	d.info.Version, d.conn = "1.6.7", conn
	h.mu.Unlock()
	h.dispatchOfflineQueue()
	h.receipts.workers.Wait()
	out := readRecoveryOperation(t, h, token, id, ticket)
	if out["state"] != "completed" || out["httpStatus"] != float64(426) || out["code"] != "update_required" {
		t.Fatalf("queued upgrade rejection lost: %v", out)
	}
	if len(conn.cmds) != 0 {
		t.Fatal("old desktop received queued task")
	}
}
