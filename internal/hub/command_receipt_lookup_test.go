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

func receiptLookupFixture(t *testing.T) (*Hub, *bridgeConn, string, string) {
	t.Helper()
	h, conn := receiptFixture(t, time.Second)
	token := strings.Repeat("ab", 32)
	h.mu.Lock()
	h.setPairLocked("receipt-owner", h.accounts["receipt-owner"].devices["pc"], sha256.Sum256([]byte(token)), true)
	h.mu.Unlock()
	return h, conn, token, shaHex("\x00" + token)
}

func lookupReceiptHTTP(h *Hub, token, account string, body any) *httptest.ResponseRecorder {
	encoded, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/app/commands", bytes.NewReader(encoded))
	r.Header.Set("X-Salcara-Pair-Token", token)
	w := httptest.NewRecorder()
	h.handleAppCommands(w, r, account)
	return w
}

func TestReceiptOnlyRecoversExactCompletedCommandWithoutSourceGrant(t *testing.T) {
	h, conn, token, credential := receiptLookupFixture(t)
	command := json.RawMessage(`{"type":"session.send","text":"review old patch","operationId":"` + receiptRequestID + `"}`)
	done := make(chan struct{})
	go func() {
		_, _, _ = h.executeReceipt(context.Background(), "receipt-owner", "pc", credential, receiptRequestID, command, nil)
		close(done)
	}()
	replyReceipt(t, h, conn, `{"accepted":true,"revision":"original"}`)
	<-done
	// A now-expired/revoked execution source is immaterial to retrieving this
	// exact owner's already completed response. No source challenge is created.
	h.mu.Lock()
	h.cfg.RemoteAccess.Mode, h.cfg.RemoteAccess.Revision, h.cfg.RemoteAccess.Audience = "restricted", "new-policy", "hub"
	h.mu.Unlock()
	body := map[string]any{"deviceId": "pc", "requestId": receiptRequestID, "receiptOnly": true,
		"command": json.RawMessage(`{"operationId":"` + receiptRequestID + `","text":"review old patch","type":"session.send"}`)}
	w := lookupReceiptHTTP(h, token, "receipt-owner", body)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"revision":"original"`) || !strings.Contains(w.Body.String(), `"requestId":"`+receiptRequestID+`"`) {
		t.Fatalf("exact recovery failed: %d %s", w.Code, w.Body)
	}
	body["command"] = json.RawMessage(`{"type":"session.send","text":"changed"}`)
	w = lookupReceiptHTTP(h, token, "receipt-owner", body)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "request_id_conflict") {
		t.Fatalf("payload identity changed: %d %s", w.Code, w.Body)
	}
	if len(conn.cmds) != 0 || len(h.receipts.entries) != 1 {
		t.Fatal("receipt lookup dispatched or created work")
	}
}

func TestReceiptOnlyMissingPendingExpiredAndUncachedNeverDispatch(t *testing.T) {
	for _, state := range []string{"missing", "pending", "expired", "uncached"} {
		t.Run(state, func(t *testing.T) {
			h, conn, token, credential := receiptLookupFixture(t)
			command := json.RawMessage(`{"type":"session.send","text":"immutable"}`)
			owner := shaHex("receipt-owner\x00pc\x00" + credential)
			key := shaHex(owner + "\x00" + receiptRequestID)
			if state != "missing" {
				canonical, _ := canonicalCommand(command)
				entry := &commandReceipt{durableCommandReceipt: durableCommandReceipt{Version: 1, Owner: owner, Hash: shaHex(canonical), ExpiresAt: nowMs() + 60_000}, done: make(chan struct{})}
				if state != "pending" {
					entry.completed = true
					close(entry.done)
				}
				if state == "expired" {
					entry.ExpiresAt = nowMs() - 1
					entry.cached, entry.reply, entry.status = true, replyBody{OK: true}, 200
				}
				h.receipts.entries[key] = entry
			}
			before := len(h.receipts.entries)
			body := map[string]any{"deviceId": "pc", "requestId": receiptRequestID, "receiptOnly": true, "command": command}
			w := lookupReceiptHTTP(h, token, "receipt-owner", body)
			code := map[string]string{"missing": "command_receipt_missing", "pending": "command_receipt_pending", "expired": "command_receipt_missing", "uncached": "command_delivery_uncertain"}[state]
			if w.Code != 409 || !strings.Contains(w.Body.String(), code) || !strings.Contains(w.Body.String(), `"retryable":false`) {
				t.Fatalf("%s: %d %s", state, w.Code, w.Body)
			}
			if len(conn.cmds) != 0 || len(h.receipts.entries) != before {
				t.Fatal("lookup mutated ledger or dispatched work")
			}
		})
	}
}

func TestReceiptOnlyAuthorizationAndCredentialScope(t *testing.T) {
	h, conn, token, credential := receiptLookupFixture(t)
	command := json.RawMessage(`{"type":"session.send","text":"one"}`)
	done := make(chan struct{})
	go func() {
		_, _, _ = h.executeReceipt(context.Background(), "receipt-owner", "pc", credential, receiptRequestID, command, nil)
		close(done)
	}()
	replyReceipt(t, h, conn, `{"private":"original reply"}`)
	<-done
	body := map[string]any{"deviceId": "pc", "requestId": receiptRequestID, "receiptOnly": true, "command": command}
	for _, other := range []struct{ token, account string }{{strings.Repeat("cd", 32), "receipt-owner"}, {token, "other-owner"}} {
		w := lookupReceiptHTTP(h, other.token, other.account, body)
		if w.Code != 403 || strings.Contains(w.Body.String(), "original reply") {
			t.Fatalf("wrong owner read receipt: %d %s", w.Code, w.Body)
		}
	}
	// Authorization is ignored when the valid pair token authenticated the
	// request, so it cannot split receipt ownership or hide the original reply.
	encoded, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/app/commands", bytes.NewReader(encoded))
	r.Header.Set("X-Salcara-Pair-Token", token)
	r.Header.Set("Authorization", "Bearer another-credential")
	w := httptest.NewRecorder()
	h.handleAppCommands(w, r, "receipt-owner")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "original reply") {
		t.Fatalf("irrelevant Authorization changed the effective receipt owner: %d %s", w.Code, w.Body)
	}
	h.mu.Lock()
	h.revokePairLocked("receipt-owner", h.accounts["receipt-owner"].devices["pc"])
	h.mu.Unlock()
	w = lookupReceiptHTTP(h, token, "receipt-owner", body)
	if w.Code != 403 || strings.Contains(w.Body.String(), "original reply") || len(conn.cmds) != 0 {
		t.Fatalf("revoked pairing recovered result: %d %s", w.Code, w.Body)
	}
}

func TestReceiptCredentialCanonicalizesEffectiveAuthentication(t *testing.T) {
	h, _, token, _ := receiptLookupFixture(t)
	pairedA := httptest.NewRequest(http.MethodPost, "/app/commands", nil)
	pairedA.Header.Set("X-Salcara-Pair-Token", token)
	pairedA.Header.Set("Authorization", "Bearer unrelated-a")
	pairedB := httptest.NewRequest(http.MethodPost, "/app/commands", nil)
	pairedB.Header.Set("X-Salcara-Pair-Token", token)
	pairedB.Header.Set("Authorization", "arbitrary unrelated value")
	if h.receiptCredential("receipt-owner", pairedA) != h.receiptCredential("receipt-owner", pairedB) {
		t.Fatal("ignored Authorization header split paired receipt identity")
	}

	bearerA := httptest.NewRequest(http.MethodPost, "/app/commands", nil)
	bearerA.Header.Set("Authorization", "Bearer account-key")
	bearerB := httptest.NewRequest(http.MethodPost, "/app/commands", nil)
	bearerB.Header.Set("Authorization", "bEaReR   account-key ")
	if h.receiptCredential("legacy-owner", bearerA) != h.receiptCredential("legacy-owner", bearerB) {
		t.Fatal("equivalent bearer credentials produced different receipt identities")
	}
	bearerB.Header.Set("Authorization", "Bearer another-key")
	if h.receiptCredential("legacy-owner", bearerA) == h.receiptCredential("legacy-owner", bearerB) {
		t.Fatal("distinct bearer credentials shared a receipt identity")
	}
}

func TestReceiptOnlyRequestValidationAndCapability(t *testing.T) {
	h, conn, token, _ := receiptLookupFixture(t)
	for _, invalid := range []any{nil, "true", 1} {
		w := lookupReceiptHTTP(h, token, "receipt-owner", map[string]any{"deviceId": "pc", "requestId": receiptRequestID, "receiptOnly": invalid, "command": map[string]string{"type": "session.send"}})
		if w.Code != 400 {
			t.Fatalf("invalid receiptOnly admitted: %v %d", invalid, w.Code)
		}
	}
	for _, typ := range []string{"features.list", "workspace.files.read", "source.grant.acquire", "session.send"} {
		requestID := receiptRequestID
		if typ == "session.send" {
			requestID = ""
		}
		w := lookupReceiptHTTP(h, token, "receipt-owner", map[string]any{"deviceId": "pc", "requestId": requestID, "receiptOnly": true, "command": map[string]string{"type": typ}})
		if w.Code != 400 && w.Code != 403 {
			t.Fatalf("non-receipt path admitted: %s %d", typ, w.Code)
		}
	}
	if len(conn.cmds) != 0 || len(h.receipts.entries) != 0 {
		t.Fatal("invalid request dispatched work")
	}
	caps, _ := h.commandCapabilities()
	if !strings.Contains(strings.Join(caps, ","), "commands.receipt-only.v1") {
		t.Fatal("persistent Hub omitted receipt lookup capability")
	}
	memory, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer memory.Close()
	caps, _ = memory.commandCapabilities()
	if strings.Contains(strings.Join(caps, ","), "commands.receipt-only.v1") {
		t.Fatal("memory-only Hub advertised durable receipt lookup")
	}
}
