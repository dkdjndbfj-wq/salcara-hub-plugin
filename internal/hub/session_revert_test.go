package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func revertCommandFixture() json.RawMessage {
	return json.RawMessage(`{"type":"session.revert","sessionKey":"codex:s","expectedTurnId":"turn-1","expectedMessageId":"message-1","operationId":"10000000-0000-4000-8000-000000000001","accountId":"credential-1","controlSurface":"cli"}`)
}

func TestSessionRevertRequiresExactCLIIdentity(t *testing.T) {
	raw := revertCommandFixture()
	if _, err := commandMeta(raw); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"sessionKey", "expectedTurnId", "expectedMessageId", "operationId", "controlSurface"} {
		for _, replacement := range []any{nil, "", 42, " ", "\x00bad", "value\n"} {
			t.Run(field+"/invalid", func(t *testing.T) {
				var fields map[string]any
				_ = json.Unmarshal(raw, &fields)
				fields[field] = replacement
				encoded, _ := json.Marshal(fields)
				if _, err := commandMeta(encoded); err == nil {
					t.Fatalf("invalid %s admitted", field)
				}
			})
		}
		var fields map[string]any
		_ = json.Unmarshal(raw, &fields)
		delete(fields, field)
		encoded, _ := json.Marshal(fields)
		if _, err := commandMeta(encoded); err == nil {
			t.Fatalf("missing %s admitted", field)
		}
	}
	for _, replacement := range [][2]string{{"controlSurface", "desktop"}, {"sessionKey", "claude:s"}, {"sessionKey", "codex:"}, {"operationId", "not-a-uuid"}, {"agent", "claude"}, {"tool", "codex-desktop"}, {"accountId", ""}, {"expectedMessageId", strings.Repeat("x", 513)}} {
		var fields map[string]any
		_ = json.Unmarshal(raw, &fields)
		fields[replacement[0]] = replacement[1]
		encoded, _ := json.Marshal(fields)
		if _, err := commandMeta(encoded); err == nil {
			t.Fatalf("invalid %s admitted", replacement[0])
		}
	}
	for _, field := range []string{"ExpectedTurnId", "expectedMessageID", "operationID", "owner", "rollbackFiles", "numTurns"} {
		injected := append(append([]byte(nil), raw[:len(raw)-1]...), []byte(`,"`+field+`":"injected"}`)...)
		if _, err := commandMeta(injected); err == nil {
			t.Fatalf("unknown or ambiguous %s admitted", field)
		}
	}
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	delete(fields, "accountId")
	fields["tool"], fields["agent"] = "codex", "codex"
	encoded, _ := json.Marshal(fields)
	if _, err := commandMeta(encoded); err != nil {
		t.Fatalf("optional source fields rejected: %v", err)
	}
}

func TestSessionRevertHasMutationSourcePolicyAndDurableCapability(t *testing.T) {
	if !sourceExecutionCommand("session.revert") || sourceReadOrSafetyCommand("session.revert") || transientSourceCommand("session.revert") {
		t.Fatal("revert escaped mutation source policy or durable receipts")
	}
	h, _ := receiptFixture(t, time.Second)
	w := httptest.NewRecorder()
	h.handlePing(w, httptest.NewRequest("GET", "/ping", nil), "")
	if !bytes.Contains(w.Body.Bytes(), []byte(`"session.revert.v1"`)) {
		t.Fatal("persistent Hub does not advertise revert")
	}
	old := h.receipts
	h.receipts = nil
	capabilities, _ := h.commandCapabilities()
	h.receipts = old
	for _, capability := range capabilities {
		if capability == "session.revert.v1" {
			t.Fatal("revert advertised without persistent receipt storage")
		}
	}
}

func TestSessionRevertWithoutRequestIDNeverDispatches(t *testing.T) {
	f := newSourceReadReceiptFixture(t, "session.revert")
	f.req.Command = revertCommandFixture()
	for _, requestID := range []string{"", "not-a-uuid"} {
		req := f.req
		req.RequestID = requestID
		w := awaitSourceReadResponse(t, f.start(t, context.Background(), req))
		if w.Code != http.StatusBadRequest || !bytes.Contains(w.Body.Bytes(), []byte(`"invalid_request_id"`)) || len(f.conn.cmds) != 0 {
			t.Fatalf("untracked revert reached bridge: %d %s", w.Code, w.Body.String())
		}
	}
	assertSourceReadReceiptCount(t, f.h, 0)
}

func TestSessionRevertRequiresCurrentSourceAdmission(t *testing.T) {
	f := newSourceReadReceiptFixture(t, "session.revert")
	f.req.Command = revertCommandFixture()
	f.authorize(t)
	req := f.req
	req.SourceGrant = nil
	w := awaitSourceReadResponse(t, f.start(t, context.Background(), req))
	if w.Code != http.StatusForbidden || !bytes.Contains(w.Body.Bytes(), []byte(`"source_grant_required"`)) || len(f.conn.cmds) != 0 {
		t.Fatalf("revert bypassed source admission: %d %s", w.Code, w.Body.String())
	}
	f.h.remoteAccessGate.Lock()
	f.h.cfg.RemoteAccess.Mode = "disabled"
	f.h.remoteAccessGate.Unlock()
	w = awaitSourceReadResponse(t, f.start(t, context.Background(), f.req))
	if w.Code != http.StatusForbidden || !bytes.Contains(w.Body.Bytes(), []byte(`"remote_execution_disabled"`)) || len(f.conn.cmds) != 0 {
		t.Fatalf("disabled source admitted revert: %d %s", w.Code, w.Body.String())
	}
	assertSourceReadReceiptCount(t, f.h, 0)
}

func TestSessionRevertPhoneCancellationKeepsOneDurableMutation(t *testing.T) {
	f := newSourceReadReceiptFixture(t, "session.revert")
	f.req.Command = revertCommandFixture()
	f.authorize(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := f.start(t, ctx, f.req)
	env, pending := f.pending(t)
	if env.Source == nil || env.Source.Mode != "restricted" || env.Source.CredentialHandle != "credential-1" || !bytes.Equal(env.Command, f.req.Command) {
		t.Fatal("revert lost exact payload or verified source")
	}
	cancel()
	if w := awaitSourceReadResponse(t, first); w.Body.Len() != 0 {
		t.Fatal("cancelled phone wait received a response")
	}
	f.h.mu.Lock()
	retained := f.h.pending[env.CommandID] == pending
	f.h.mu.Unlock()
	if !retained {
		t.Fatal("phone cancellation interrupted revert")
	}
	retry := f.start(t, context.Background(), f.req)
	pending.ch <- replyBody{OK: true, Result: json.RawMessage(`{"status":"completed","sessionKey":"codex:s","turnId":"turn-1","messageId":"message-1","operationId":"10000000-0000-4000-8000-000000000001"}`)}
	w := awaitSourceReadResponse(t, retry)
	if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"status":"completed"`)) || len(f.conn.cmds) != 0 {
		t.Fatalf("revert retry failed to join receipt: %d %s", w.Code, w.Body.String())
	}
	assertSourceReadReleased(t, f.h)
	assertSourceReadReceiptCount(t, f.h, 1)
	f.req.SourceGrant = nil
	w = awaitSourceReadResponse(t, f.start(t, context.Background(), f.req))
	if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"turnId":"turn-1"`)) || len(f.conn.cmds) != 0 {
		t.Fatal("completed revert was dispatched again instead of replaying its exact receipt")
	}
}

func TestSessionRevertUncertainDeliveryNeverRedispatches(t *testing.T) {
	h, conn := receiptFixture(t, 30*time.Millisecond)
	command := revertCommandFixture()
	for i := 0; i < 2; i++ {
		_, status, code := h.executeReceipt(context.Background(), "receipt-owner", "pc", "pair-a", receiptRequestID, command, nil)
		if status != http.StatusConflict || code != "command_delivery_uncertain" || len(conn.cmds) != 1 {
			t.Fatalf("uncertain revert repeated: %d %s dispatches=%d", status, code, len(conn.cmds))
		}
	}
	store, err := newCommandReceipts(h.cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	h.receipts.close()
	h.receipts = store
	_, status, code := h.executeReceipt(context.Background(), "receipt-owner", "pc", "pair-a", receiptRequestID, command, nil)
	if status != http.StatusConflict || code != "command_delivery_uncertain" || len(conn.cmds) != 1 {
		t.Fatal("restart redispatched an uncertain revert")
	}
}

func TestSessionRevertedEventRetainsTargetIdentityAndDeduplicatesBatch(t *testing.T) {
	h := newDeviceHub(t, Config{DataDir: t.TempDir()})
	id, secret := "pc-revert", strings.Repeat("d", 64)
	enrollDevice(t, h, id, secret)
	onlineDevice(t, h, id)
	event := map[string]any{"type": "session.reverted", "sessionKey": "codex:s", "id": "message-1", "turnId": "turn-1", "operationId": receiptRequestID}
	batch := map[string]any{"deviceId": id, "batchId": receiptRequestID, "events": []any{event}}
	for i := 0; i < 2; i++ {
		w := deviceRequest(t, h, "/bridge/events", id, secret, "", batch, "")
		if w.Code != http.StatusOK {
			t.Fatalf("revert event refused: %d %s", w.Code, w.Body.String())
		}
	}
	h.mu.Lock()
	page := h.readEventPageLocked(deviceAccount(id), id, "codex:s", 0, 100)
	other := h.readEventPageLocked(deviceAccount(id), id, "codex:other", 0, 100)
	h.mu.Unlock()
	if len(page.Events) != 1 || len(other.Events) != 0 {
		t.Fatalf("revert event scope or dedupe failed: %d %d", len(page.Events), len(other.Events))
	}
	var received map[string]any
	if err := json.Unmarshal(page.Events[0], &received); err != nil {
		t.Fatal(err)
	}
	for key, value := range event {
		if received[key] != value {
			t.Fatalf("event lost %s: %v", key, received)
		}
	}
}
