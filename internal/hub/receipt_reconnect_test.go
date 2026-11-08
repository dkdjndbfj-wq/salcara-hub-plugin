package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReceiptReplySurvivesBridgeReconnect(t *testing.T) {
	for _, scenario := range []string{"same-binding", "changed-binding", "revoked", "replaced-device"} {
		t.Run(scenario, func(t *testing.T) {
			h, conn := receiptFixture(t, time.Second)
			secret := strings.Repeat("ab", 32)
			h.mu.Lock()
			dev := h.accounts["receipt-owner"].devices["pc"]
			dev.secretHash, dev.hasSecret = sha256.Sum256([]byte(secret)), true
			dev.bindingID, dev.phoneHash, dev.hasPair = "binding-a", "phone-a", true
			h.mu.Unlock()
			authorize := func(d *device) bool { return d.hasPair }
			command := json.RawMessage(`{"type":"session.send","text":"once"}`)
			type result struct {
				rep    replyBody
				status int
				code   string
			}
			finished := make(chan result, 1)
			go func() {
				rep, status, code := h.executeReceipt(context.Background(), "receipt-owner", "pc", "pair-a", receiptRequestID, command, authorize)
				finished <- result{rep, status, code}
			}()
			var env commandEnvelope
			select {
			case raw := <-conn.cmds:
				if err := json.Unmarshal(raw, &env); err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("command was not dispatched")
			}
			replacement := &bridgeConn{cmds: make(chan []byte, 1), closed: make(chan struct{})}
			h.mu.Lock()
			dev.conn = replacement
			h.mu.Unlock()
			conn.close()
			select {
			case value := <-finished:
				t.Fatalf("SSE reconnect discarded accepted operation: %d %s", value.status, value.code)
			case <-time.After(30 * time.Millisecond):
			}
			h.mu.Lock()
			switch scenario {
			case "changed-binding":
				dev.bindingID = "binding-b"
			case "revoked":
				dev.hasPair = false
			case "replaced-device":
				copy := *dev
				h.accounts["receipt-owner"].devices["pc"] = &copy
			}
			h.mu.Unlock()
			body, _ := json.Marshal(replyBody{DeviceID: "pc", CommandID: env.CommandID, OK: true, Result: json.RawMessage(`{"accepted":true}`)})
			req := httptest.NewRequest(http.MethodPost, "/bridge/reply", bytes.NewReader(body))
			req.Header.Set("X-Salcara-Device-Secret", secret)
			req.Header.Set("X-Salcara-Device-Id", "pc")
			ack := httptest.NewRecorder()
			h.handleBridgeReply(ack, req, "receipt-owner")
			if ack.Code != http.StatusOK {
				t.Fatalf("late reply: %d %s", ack.Code, ack.Body.String())
			}
			value := <-finished
			if scenario == "same-binding" {
				if value.status != 200 || !value.rep.OK {
					t.Fatalf("reply was lost: %+v", value)
				}
				rep, status, code := h.lookupCommandReceipt("receipt-owner", "pc", "pair-a", receiptRequestID, command, authorize)
				if status != 200 || !rep.OK || code != "" {
					t.Fatalf("receipt lookup: %d %s", status, code)
				}
			} else if value.status != 403 || value.rep.OK || len(value.rep.Result) != 0 {
				t.Fatalf("stale scope released reply: %+v", value)
			}
			if len(conn.cmds)+len(replacement.cmds) != 0 {
				t.Fatal("accepted operation was dispatched again")
			}
		})
	}
}

func TestDisconnectedReceiptStillExpiresAtOriginalDeadline(t *testing.T) {
	h, conn := receiptFixture(t, 60*time.Millisecond)
	finished := make(chan string, 1)
	go func() {
		_, _, code := h.executeReceipt(context.Background(), "receipt-owner", "pc", "pair-a", receiptRequestID, json.RawMessage(`{"type":"session.send"}`), nil)
		finished <- code
	}()
	<-conn.cmds
	conn.close()
	select {
	case code := <-finished:
		if code != "command_delivery_uncertain" {
			t.Fatal(code)
		}
	case <-time.After(time.Second):
		t.Fatal("disconnected receipt did not expire")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.pending) != 0 {
		t.Fatal("expired receipt retained pending capacity")
	}
}
