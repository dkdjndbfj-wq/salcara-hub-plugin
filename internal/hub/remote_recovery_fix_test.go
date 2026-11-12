package hub

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func readRecoveryOperation(t *testing.T, h *Hub, token, id, ticket string) map[string]any {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/app/operations?deviceId=pc&operationId="+id+"&ticket="+url.QueryEscape(ticket), nil)
	r.Header.Set("X-Salcara-Pair-Token", token)
	w := httptest.NewRecorder()
	h.handleOperationRead(w, r, "receipt-owner")
	if w.Code != http.StatusOK {
		t.Fatalf("operation read: %d", w.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRecoveryUncertainAndTruncatedSurviveRetentionCleanupAndRestart(t *testing.T) {
	for _, stored := range []string{"uncertain", "truncated"} {
		t.Run(stored, func(t *testing.T) {
			h, conn, token, credential := receiptLookupFixture(t)
			prepared := prepareHTTP(t, h, token, receiptRequestID, json.RawMessage(`{"type":"session.send","text":"fixture"}`))
			id, ticket := prepared["operationId"].(string), prepared["ticket"].(string)
			owner := shaHex("receipt-owner\x00pc\x00" + credential)
			key := shaHex(owner + "\x00" + id)
			if err := h.durable.saveReply(key, owner, replyBody{Error: "fixture timeout"}, 504, false); err != nil {
				t.Fatal(err)
			}
			if _, err := h.durable.db.Exec(`UPDATE replies SET state=?,expires=? WHERE id=?`, stored, nowMs()-1, key); err != nil {
				t.Fatal(err)
			}
			assertState := func(current *Hub) {
				t.Helper()
				out := readRecoveryOperation(t, current, token, id, ticket)
				if out["state"] != "uncertain" || out["httpStatus"] != float64(504) {
					t.Fatalf("lost execution evidence: state=%v status=%v", out["state"], out["httpStatus"])
				}
				if stored == "truncated" && out["code"] != "result_body_truncated" {
					t.Fatal("lost truncated-result explanation")
				}
			}
			assertState(h)
			if err := h.RunStorageCleanup(); err != nil {
				t.Fatal(err)
			}
			assertState(h)
			if len(conn.cmds) != 0 {
				t.Fatal("recovery dispatched work")
			}
			h.Close()
			restored, err := New(h.cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(restored.Close)
			assertState(restored)
		})
	}
}

func TestRecoveryRequiredUpdateRemainsKnownAcrossRetryAndRestart(t *testing.T) {
	for _, protocol := range []string{"v1", "v2"} {
		t.Run(protocol, func(t *testing.T) {
			h, conn, token, _ := receiptLookupFixture(t)
			h.cfg.DesktopMinimumVersion = "1.6.8"
			h.accounts["receipt-owner"].devices["pc"].info.Version = "1.6.7"
			command := json.RawMessage(`{"type":"session.start","tool":"codex","text":"fixture"}`)
			body := map[string]any{"deviceId": "pc", "requestId": receiptRequestID, "command": command}
			if protocol == "v2" {
				prepared := prepareHTTP(t, h, token, receiptRequestID, command)
				id := prepared["operationId"].(string)
				body["requestId"], body["executionTicket"] = id, prepared["ticket"]
				body["command"] = map[string]any{"type": "session.start", "tool": "codex", "text": "fixture", "operationId": id}
			}
			check := func(current *Hub) {
				t.Helper()
				w := lookupReceiptHTTP(current, token, "receipt-owner", body)
				if w.Code != http.StatusUpgradeRequired || !bytes.Contains(w.Body.Bytes(), []byte(`"code":"update_required"`)) {
					t.Fatalf("upgrade rejection lost: %d %s", w.Code, w.Body)
				}
			}
			check(h)
			check(h)
			if len(conn.cmds) != 0 {
				t.Fatal("outdated computer received new work")
			}
			h.Close()
			restored, err := New(h.cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(restored.Close)
			check(restored)
		})
	}
}

func queueRecoveryOperation(t *testing.T, h *Hub, deviceID, token, intent string) (string, string) {
	t.Helper()
	command := map[string]any{"type": "session.send", "tool": "codex", "text": "fixture"}
	request := func(path string, body any, handler func(http.ResponseWriter, *http.Request, string)) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		r.Header.Set("X-Salcara-Pair-Token", token)
		w := httptest.NewRecorder()
		handler(w, r, "receipt-owner")
		return w
	}
	w := request("/app/commands/prepare", map[string]any{"deviceId": deviceID, "requestId": intent, "command": command}, h.handleOperationPrepare)
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
	w = request("/app/commands", map[string]any{"deviceId": deviceID, "requestId": prepared.ID, "executionTicket": prepared.Ticket, "command": command}, h.handleAppCommands)
	if w.Code != http.StatusAccepted {
		t.Fatalf("queue: %d %s", w.Code, w.Body)
	}
	return prepared.ID, prepared.Ticket
}

func TestRecoveryOfflineQueueSkipsEightOfflineItems(t *testing.T) {
	h, conn, token, _ := receiptLookupFixture(t)
	p := h.DurableStorageStatus().Policy
	p.OfflineQueueEnabled = true
	if err := h.SetStoragePolicy(p); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.accounts["receipt-owner"].devices["pc"].conn = nil
	tokens := map[string]string{"offline-a": strings.Repeat("ac", 32), "offline-b": strings.Repeat("ad", 32)}
	for id, tok := range tokens {
		d := &device{info: Device{DeviceID: id, Capabilities: []string{"source.guard.v1", "station.guard.v1"}}}
		h.accounts["receipt-owner"].devices[id] = d
		h.setPairLocked("receipt-owner", d, sha256.Sum256([]byte(tok)), true)
	}
	h.mu.Unlock()
	for i := 0; i < 8; i++ {
		id := "offline-a"
		if i >= 4 {
			id = "offline-b"
		}
		queueRecoveryOperation(t, h, id, tokens[id], fmt.Sprintf("20000000-0000-4000-8000-%012d", i+1))
	}
	id, ticket := queueRecoveryOperation(t, h, "pc", token, "20000000-0000-4000-8000-000000000009")
	h.mu.Lock()
	h.accounts["receipt-owner"].devices["pc"].conn = conn
	h.mu.Unlock()
	h.dispatchOfflineQueue()
	replyReceipt(t, h, conn, `{"delivered":true}`)
	deadline := time.Now().Add(time.Second)
	for {
		out := readRecoveryOperation(t, h, token, id, ticket)
		if out["state"] == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("online task behind offline owners did not complete")
		}
		time.Sleep(time.Millisecond)
	}
	var queued int
	if err := h.durable.db.QueryRow(`SELECT COUNT(*) FROM offline_queue WHERE state='queued'`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 8 {
		t.Fatalf("changed another owner's queued work: %d", queued)
	}
	h.dispatchOfflineQueue()
	if len(conn.cmds) != 0 {
		t.Fatal("completed operation was sent again")
	}
}

func TestRecoveryApprovalOperationIdentityAndTypedUncertainReceipt(t *testing.T) {
	for _, protocol := range []string{"v1", "v2"} {
		t.Run(protocol, func(t *testing.T) {
			h, conn, token, _ := receiptLookupFixture(t)
			id, ticket := receiptRequestID, ""
			command := map[string]any{"type": "approval.respond", "approvalId": "fixture-approval", "decision": "allow"}
			if protocol == "v2" {
				raw, _ := json.Marshal(command)
				prepared := prepareHTTP(t, h, token, id, raw)
				id, ticket = prepared["operationId"].(string), prepared["ticket"].(string)
			}
			command["operationId"] = id
			body := map[string]any{"deviceId": "pc", "requestId": id, "command": command}
			if ticket != "" {
				body["executionTicket"] = ticket
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- lookupReceiptHTTP(h, token, "receipt-owner", body) }()
			var envelope commandEnvelope
			select {
			case raw := <-conn.cmds:
				if err := json.Unmarshal(raw, &envelope); err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("approval not dispatched")
			}
			var sent map[string]any
			if err := json.Unmarshal(envelope.Command, &sent); err != nil {
				t.Fatal(err)
			}
			if sent["type"] != "approval.respond" || sent["operationId"] != id {
				t.Fatalf("approval identity changed: %v", sent)
			}
			h.mu.Lock()
			pending := h.pending[envelope.CommandID]
			h.mu.Unlock()
			if pending == nil {
				t.Fatal("missing reply receiver")
			}
			pending.ch <- replyBody{OK: false, Code: "command_delivery_uncertain", Error: "审批结果尚未确认"}
			check := func(w *httptest.ResponseRecorder) {
				t.Helper()
				var result map[string]any
				if json.Unmarshal(w.Body.Bytes(), &result) != nil || w.Code != 200 || result["ok"] != false || result["code"] != "command_delivery_uncertain" || result["requestId"] != id {
					t.Fatalf("typed reply lost: %d %s", w.Code, w.Body)
				}
			}
			check(<-done)
			body["receiptOnly"] = true
			check(lookupReceiptHTTP(h, token, "receipt-owner", body))
			if len(conn.cmds) != 0 || len(h.receipts.entries) != 1 {
				t.Fatal("receipt recovery dispatched another approval")
			}
			h.Close()
			restored, err := New(h.cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(restored.Close)
			check(lookupReceiptHTTP(restored, token, "receipt-owner", body))
			if len(restored.receipts.entries) != 1 {
				t.Fatal("restart recovery allocated another identity")
			}
		})
	}
}
