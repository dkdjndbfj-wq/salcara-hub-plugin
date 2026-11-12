package hub

import (
	"encoding/json"
	"testing"
	"time"
)

// Regression probe: an existing terminal receipt takes precedence over the
// offline admission deadline. This uses a valid short-lived synthetic ticket.
func TestCancelContractReplayAfterTicketExpiry(t *testing.T) {
	h, conn, token, id, ticket, key := cancelContractFixture(t)
	short, err := h.durable.verifyTicket(ticket)
	if err != nil {
		t.Fatal(err)
	}
	short.Deadline = nowMs() + 1000
	ticket = h.durable.signTicket(short)
	if _, err := h.durable.db.Exec(`UPDATE operations SET deadline=?,ticket=? WHERE id=?`, short.Deadline, ticket, id); err != nil {
		t.Fatal(err)
	}
	if _, err := h.durable.db.Exec(`UPDATE offline_queue SET deadline=? WHERE id=?`, short.Deadline, key); err != nil {
		t.Fatal(err)
	}
	requireCancelContract(t, cancelContractHTTP(h, token, id, ticket), 200, "operation_cancelled")
	for nowMs() <= short.Deadline {
		time.Sleep(10 * time.Millisecond)
	}
	body := map[string]any{"deviceId": "pc", "requestId": id, "executionTicket": ticket,
		"command": map[string]any{"type": "session.send", "tool": "codex", "text": "fixture", "operationId": id}}
	w := lookupReceiptHTTP(h, token, "receipt-owner", body)
	var out struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if w.Code != 409 || out.Code != "operation_cancelled" {
		t.Fatalf("terminal replay lost after ticket expiry: %d %s", w.Code, w.Body)
	}
	read := readRecoveryOperation(t, h, token, id, ticket)
	if read["state"] != "cancelled" || read["code"] != "operation_cancelled" {
		t.Fatalf("GET did not retain cancelled: %v", read)
	}
	body["receiptOnly"] = true
	requireCancelContract(t, lookupReceiptHTTP(h, token, "receipt-owner", body), 409, "operation_cancelled")
	if len(conn.cmds) != 0 {
		t.Fatal("synthetic cancelled task dispatched")
	}
	t.Log("PASS: expired normal replay + GET + receiptOnly=operation_cancelled; no dispatch")
}
