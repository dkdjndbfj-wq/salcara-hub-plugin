package hub

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReceiptHTTPNativeUncertaintyAndAuthorization(t *testing.T) {
	h, conn := receiptFixture(t, time.Second)
	token := strings.Repeat("ab", 32)
	h.mu.Lock()
	h.setPairLocked("receipt-owner", h.accounts["receipt-owner"].devices["pc"], sha256.Sum256([]byte(token)), true)
	h.mu.Unlock()
	command := map[string]any{"type": "desktop.session.send", "controlSurface": "desktop", "sessionKey": "codex:" + receiptRequestID, "operationId": receiptRequestID, "text": "one"}
	body := map[string]any{"deviceId": "pc", "requestId": receiptRequestID, "command": command}
	completed := make(chan *httptest.ResponseRecorder, 1)
	go func() { completed <- requestHub(t, h, http.MethodPost, "/app/commands", "", "", token, body) }()
	var raw []byte
	select {
	case raw = <-conn.cmds:
	case <-time.After(time.Second):
		t.Fatal("missing dispatch")
	}
	var envelope commandEnvelope
	_ = json.Unmarshal(raw, &envelope)
	h.mu.Lock()
	pending := h.pending[envelope.CommandID]
	h.mu.Unlock()
	pending.ch <- replyBody{OK: false, Error: "command_delivery_uncertain: refresh original conversation"}
	first := <-completed
	var reply map[string]any
	_ = json.Unmarshal(first.Body.Bytes(), &reply)
	if first.Code != 200 || reply["code"] != "command_delivery_uncertain" || reply["retryable"] != false || reply["requestId"] != receiptRequestID {
		t.Fatalf("wrapped reply: %d %s", first.Code, first.Body.String())
	}
	retry := requestHub(t, h, http.MethodPost, "/app/commands", "", "", token, body)
	if retry.Code != 200 || len(conn.cmds) != 0 {
		t.Fatal("cached uncertainty redispatched")
	}
	command["text"] = "different"
	conflict := requestHub(t, h, http.MethodPost, "/app/commands", "", "", token, body)
	if conflict.Code != 409 || !strings.Contains(conflict.Body.String(), "request_id_conflict") || len(conn.cmds) != 0 {
		t.Fatal("changed payload reused ID")
	}
	h.mu.Lock()
	h.revokePairLocked("receipt-owner", h.accounts["receipt-owner"].devices["pc"])
	h.mu.Unlock()
	forbidden := requestHub(t, h, http.MethodPost, "/app/commands", "", "", token, body)
	if forbidden.Code != 403 || strings.Contains(forbidden.Body.String(), "refresh original conversation") {
		t.Fatal("revoked phone read cached result")
	}
}
func TestReceiptCapabilityOnlyWithDataDirectory(t *testing.T) {
	h, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	caps, _ := h.commandCapabilities()
	for _, cap := range caps {
		if cap == "commands.idempotency.v1" {
			t.Fatal("memory-only ledger advertised durable retry")
		}
	}
	persisted, _ := receiptFixture(t, time.Second)
	found := false
	caps, ttl := persisted.commandCapabilities()
	for _, cap := range caps {
		if cap == "commands.idempotency.v1" {
			found = true
		}
	}
	if !found || ttl != 86400 {
		t.Fatalf("caps=%v ttl=%d", caps, ttl)
	}
}
