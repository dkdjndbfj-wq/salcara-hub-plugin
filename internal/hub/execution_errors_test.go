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

func TestCapacityCodeSurvivesHTTPAndReceiptReplay(t *testing.T) {
	for _, receipt := range []bool{false, true} {
		h, conn := receiptFixture(t, time.Second)
		token := strings.Repeat("ab", 32)
		h.mu.Lock()
		h.setPairLocked("receipt-owner", h.accounts["receipt-owner"].devices["pc"], sha256.Sum256([]byte(token)), true)
		h.mu.Unlock()
		body := map[string]any{"deviceId": "pc", "command": map[string]any{"type": "session.send", "controlSurface": "cli", "sessionKey": "codex:" + receiptRequestID, "text": "continue"}}
		if receipt {
			body["requestId"] = receiptRequestID
		}
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- requestHub(t, h, http.MethodPost, "/app/commands", "", "", token, body) }()
		var envelope commandEnvelope
		select {
		case raw := <-conn.cmds:
			_ = json.Unmarshal(raw, &envelope)
		case <-time.After(time.Second):
			t.Fatal("command not dispatched")
		}
		h.mu.Lock()
		pending := h.pending[envelope.CommandID]
		h.mu.Unlock()
		pending.ch <- replyBody{OK: false, Error: "localized provider failure", Code: "model_capacity"}
		first := <-done
		var reply map[string]any
		_ = json.Unmarshal(first.Body.Bytes(), &reply)
		if first.Code != http.StatusOK || reply["ok"] != false || reply["code"] != "model_capacity" {
			t.Fatalf("typed failure lost: %d %s", first.Code, first.Body.String())
		}
		if receipt {
			replay := requestHub(t, h, http.MethodPost, "/app/commands", "", "", token, body)
			if !strings.Contains(replay.Body.String(), `"code":"model_capacity"`) || len(conn.cmds) != 0 {
				t.Fatal("failure retry lost code or sent message again")
			}
		}
	}
}
