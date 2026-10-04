package hub

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const receiptRequestID = "10000000-0000-4000-8000-000000000001"

func receiptFixture(t *testing.T, timeout time.Duration) (*Hub, *bridgeConn) {
	t.Helper()
	h, err := New(Config{DataDir: t.TempDir(), CommandTimeout: timeout, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	conn := &bridgeConn{cmds: make(chan []byte, 10), closed: make(chan struct{})}
	h.mu.Lock()
	h.accountLocked("receipt-owner").devices["pc"] = &device{info: Device{DeviceID: "pc"}, conn: conn}
	h.mu.Unlock()
	return h, conn
}
func replyReceipt(t *testing.T, h *Hub, conn *bridgeConn, result string) string {
	t.Helper()
	var raw []byte
	select {
	case raw = <-conn.cmds:
	case <-time.After(time.Second):
		t.Fatal("missing dispatch")
	}
	var env commandEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	pending := h.pending[env.CommandID]
	h.mu.Unlock()
	if pending == nil {
		t.Fatal("dispatch has no pending receiver")
	}
	pending.ch <- replyBody{OK: true, Result: json.RawMessage(result)}
	return env.CommandID
}
func TestReceiptConcurrentRetryOneDispatch(t *testing.T) {
	h, conn := receiptFixture(t, time.Second)
	var wg sync.WaitGroup
	errors := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rep, status, code := h.executeReceipt(context.Background(), "receipt-owner", "pc", "pair-a", receiptRequestID, json.RawMessage(`{"type":"session.send","text":"one"}`), nil)
			if status != 200 || !rep.OK || code != "" {
				errors <- "retry did not receive original reply"
			}
		}()
	}
	replyReceipt(t, h, conn, `{"accepted":true}`)
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	if len(conn.cmds) != 0 {
		t.Fatal("retried operation was dispatched again")
	}
	_, status, code := h.executeReceipt(context.Background(), "receipt-owner", "pc", "pair-a", receiptRequestID, json.RawMessage(`{"text":"different","type":"session.send"}`), nil)
	if status != 409 || code != "request_id_conflict" {
		t.Fatalf("payload reuse accepted %d %s", status, code)
	}
}
func TestReceiptPhoneDisconnectDoesNotCancelDelivery(t *testing.T) {
	h, conn := receiptFixture(t, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan int, 1)
	go func() {
		_, status, _ := h.executeReceipt(ctx, "receipt-owner", "pc", "pair-a", receiptRequestID, json.RawMessage(`{"type":"session.send","text":"one"}`), nil)
		first <- status
	}()
	var raw []byte
	select {
	case raw = <-conn.cmds:
	case <-time.After(time.Second):
		t.Fatal("missing dispatch")
	}
	cancel()
	if status := <-first; status != 499 {
		t.Fatalf("cancel: %d", status)
	}
	var env commandEnvelope
	_ = json.Unmarshal(raw, &env)
	h.mu.Lock()
	pending := h.pending[env.CommandID]
	h.mu.Unlock()
	if pending == nil {
		t.Fatal("phone disconnect discarded pending command")
	}
	pending.ch <- replyBody{OK: true, Result: json.RawMessage(`{"accepted":true}`)}
	rep, status, code := h.executeReceipt(context.Background(), "receipt-owner", "pc", "pair-a", receiptRequestID, json.RawMessage(`{"text":"one","type":"session.send"}`), nil)
	if status != 200 || !rep.OK || code != "" || len(conn.cmds) != 0 {
		t.Fatalf("recover: %d %s", status, code)
	}
}
func TestReceiptRestartNeverRedispatches(t *testing.T) {
	h, conn := receiptFixture(t, time.Second)
	finished := make(chan struct{})
	go func() {
		h.executeReceipt(context.Background(), "receipt-owner", "pc", "pair-a", receiptRequestID, json.RawMessage(`{"type":"session.send","text":"private prompt"}`), nil)
		close(finished)
	}()
	replyReceipt(t, h, conn, `{"privateReply":"content"}`)
	<-finished
	store, err := newCommandReceipts(h.cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	h.receipts.close()
	h.receipts = store
	_, status, code := h.executeReceipt(context.Background(), "receipt-owner", "pc", "pair-a", receiptRequestID, json.RawMessage(`{"type":"session.send","text":"private prompt"}`), nil)
	if status != 409 || code != "command_delivery_uncertain" || len(conn.cmds) != 0 {
		t.Fatalf("restart replay: %d %s", status, code)
	}
	files, _ := os.ReadDir(filepath.Join(h.cfg.DataDir, "command-receipts"))
	if len(files) != 1 {
		t.Fatalf("receipts: %d", len(files))
	}
	bytes, _ := os.ReadFile(filepath.Join(h.cfg.DataDir, "command-receipts", files[0].Name()))
	for _, private := range []string{"private prompt", "privateReply", "pair-a", "receipt-owner", "content"} {
		if strings.Contains(string(bytes), private) {
			t.Fatalf("private data persisted: %s", private)
		}
	}
}
func TestReceiptTimeoutAndScopeNeverRepeat(t *testing.T) {
	h, conn := receiptFixture(t, 30*time.Millisecond)
	_, status, code := h.executeReceipt(context.Background(), "receipt-owner", "pc", "pair-a", receiptRequestID, json.RawMessage(`{"type":"session.send"}`), nil)
	if status != 409 || code != "command_delivery_uncertain" {
		t.Fatalf("timeout: %d %s", status, code)
	}
	if len(conn.cmds) != 1 {
		t.Fatal("dispatch missing")
	}
	_, status, code = h.executeReceipt(context.Background(), "receipt-owner", "pc", "pair-a", receiptRequestID, json.RawMessage(`{"type":"session.send"}`), nil)
	if status != 409 || code != "command_delivery_uncertain" || len(conn.cmds) != 1 {
		t.Fatal("uncertain receipt redispatched")
	}
	_, status, code = h.executeReceipt(context.Background(), "receipt-owner", "pc", "pair-a", receiptRequestID, json.RawMessage(`{"type":"session.send"}`), func(*device) bool { return false })
	if status != 403 || code != "device_authorization_expired" {
		t.Fatal("expired authorization read receipt")
	}
}
func TestReceiptOfflineHasNoTombstone(t *testing.T) {
	h, _ := receiptFixture(t, time.Second)
	h.mu.Lock()
	h.accounts["receipt-owner"].devices["pc"].conn = nil
	h.mu.Unlock()
	_, status, code := h.executeReceipt(context.Background(), "receipt-owner", "pc", "pair-a", receiptRequestID, json.RawMessage(`{"type":"session.send"}`), nil)
	if status != 409 || code != "computer_offline" || len(h.receipts.entries) != 0 {
		t.Fatalf("offline: %d %s", status, code)
	}
}
func TestCursorPageGapAndDeviceIsolation(t *testing.T) {
	h, _ := receiptFixture(t, time.Second)
	h.mu.Lock()
	account := h.accounts["receipt-owner"]
	for i := 0; i < sessionRingSize+3; i++ {
		fields := map[string]json.RawMessage{"type": json.RawMessage(`"message"`), "sessionKey": json.RawMessage(`"codex:s"`)}
		account.appendEventLocked("pc", fields)
	}
	account.appendEventLocked("other", map[string]json.RawMessage{"type": json.RawMessage(`"message"`), "sessionKey": json.RawMessage(`"codex:other"`)})
	page := h.readEventPageLocked("receipt-owner", "pc", "codex:s", h.seqBase+1, 2)
	if !page.ResetRequired || !page.HasMore || len(page.Events) != 2 {
		t.Fatalf("gap page: %+v", page)
	}
	second := h.readEventPageLocked("receipt-owner", "pc", "codex:s", page.NextSeq, 2)
	if second.ResetRequired || len(second.Events) != 2 || second.NextSeq <= page.NextSeq {
		t.Fatalf("next page: %+v", second)
	}
	devicePage := h.readEventPageLocked("receipt-owner", "pc", "", 0, 500)
	for _, event := range devicePage.Events {
		if strings.Contains(string(event), `"deviceId":"other"`) {
			t.Fatal("other device leaked")
		}
	}
	h.mu.Unlock()
}
