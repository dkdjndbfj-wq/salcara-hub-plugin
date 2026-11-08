package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func healthyMailboxVolume(string) (int64, int64, error) { return 100 << 30, 50 << 30, nil }
func mailboxFixture(t *testing.T) (*durableStore, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := newDurableStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.volumeProbe = healthyMailboxVolume
	t.Cleanup(s.close)
	return s, dir
}

func TestDurableReplyRestartEncryptedAndExpiration(t *testing.T) {
	s, dir := mailboxFixture(t)
	key, owner := shaHex("receipt"), shaHex("owner")
	if err := s.saveReply(key, owner, replyBody{OK: true, Result: json.RawMessage(`{"text":"private-result-fixture"}`)}, 200, true); err != nil {
		t.Fatal(err)
	}
	s.close()
	next, err := newDurableStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer next.close()
	if next.epoch != s.epoch {
		t.Fatal("restart changed storage epoch")
	}
	rep, status, state, err := next.reply(key)
	if err != nil || status != 200 || state != "completed" || !bytes.Contains(rep.Result, []byte("private-result-fixture")) {
		t.Fatalf("reply not restored: %s %d %v", state, status, err)
	}
	for _, name := range []string{"mailbox.sqlite", "mailbox.sqlite-wal"} {
		raw, _ := os.ReadFile(filepath.Join(next.dir, name))
		if bytes.Contains(raw, []byte("private-result-fixture")) {
			t.Fatal("plaintext body persisted")
		}
	}
	_, _ = next.db.Exec(`UPDATE replies SET expires=? WHERE id=?`, nowMs()-1, key)
	_, _, state, err = next.reply(key)
	if err != nil || state != "expired" {
		t.Fatal("expired body leaked")
	}
	if err = next.cleanupLocked(nowMs()); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = next.db.QueryRow(`SELECT COUNT(*) FROM replies WHERE id=? AND body IS NULL`, key).Scan(&n)
	if n != 1 {
		t.Fatal("cleanup lost tombstone or kept body")
	}
}

func TestDurableStoreMissingKeyFailsClosed(t *testing.T) {
	s, dir := mailboxFixture(t)
	s.close()
	if err := os.Remove(filepath.Join(s.dir, "key-v1")); err != nil {
		t.Fatal(err)
	}
	if next, err := newDurableStore(dir); err == nil {
		next.close()
		t.Fatal("missing key silently regenerated")
	}
}

func durableEvent(t *testing.T, seq int64, session, text string) *storedEvent {
	t.Helper()
	m := map[string]json.RawMessage{"type": json.RawMessage(`"message"`)}
	m["sessionKey"], _ = json.Marshal(session)
	m["text"], _ = json.Marshal(text)
	e, err := prepareStoredEvent("pc", seq, m)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func TestDurableEventsRestartBatchAndScope(t *testing.T) {
	s, dir := mailboxFixture(t)
	scope := shaHex("original-binding")
	events := []*storedEvent{durableEvent(t, 101, "s", "one"), durableEvent(t, 102, "s", "two")}
	if err := s.storeEvents(scope, receiptRequestID, shaHex("batch"), events); err != nil {
		t.Fatal(err)
	}
	s.close()
	s2, err := newDurableStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.close()
	b, found, err := s2.findBatch(scope, receiptRequestID)
	if err != nil || !found || b.lastSeq != 102 || b.accepted != 2 {
		t.Fatalf("batch lost %v %v", found, err)
	}
	p, err := s2.eventPage(scope, "s", 0, 1)
	if err != nil || p.ResetRequired || len(p.Events) != 1 || !p.HasMore || p.NextSeq != 101 {
		t.Fatalf("first page %+v %v", p, err)
	}
	p, err = s2.eventPage(scope, "s", p.NextSeq, 10)
	if err != nil || len(p.Events) != 1 || p.NextSeq != 102 {
		t.Fatal("continuation lost")
	}
	p, _ = s2.eventPage(shaHex("rebound"), "s", 0, 10)
	if len(p.Events) != 0 {
		t.Fatal("old binding leaked")
	}
}
func TestDurableEventExpiryPreservesGapAndReadDoesNotRenew(t *testing.T) {
	s, _ := mailboxFixture(t)
	scope := shaHex("binding")
	if err := s.storeEvents(scope, "", "", []*storedEvent{durableEvent(t, 1, "s", "expired"), durableEvent(t, 2, "s", "kept")}); err != nil {
		t.Fatal(err)
	}
	_, _ = s.db.Exec(`UPDATE events SET expires=? WHERE seq=1`, nowMs()-1)
	p, err := s.eventPage(scope, "s", 0, 10)
	if err != nil || !p.ResetRequired || len(p.Events) != 1 || p.NextSeq != 2 {
		t.Fatalf("missing gap %+v %v", p, err)
	}
	if err = s.cleanupLocked(nowMs()); err != nil {
		t.Fatal(err)
	}
	p, _ = s.eventPage(scope, "s", 0, 10)
	if !p.ResetRequired || len(p.Events) != 1 {
		t.Fatal("cleanup erased recovery gap")
	}
}
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	m := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	m.Set(0, 0, color.NRGBA{R: 255, A: 255})
	if err := png.Encode(&b, m); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestDurableBlobInterruptedUploadRestartReferencesAndLease(t *testing.T) {
	s, dir := mailboxFixture(t)
	scope, id := shaHex("binding"), "att_"+strings.Repeat("1", 32)
	data := tinyPNG(t)
	half := len(data) / 2
	if done, err := s.putBlobPart(scope, id, "image/png", 0, 2, data[:half]); err != nil || done {
		t.Fatalf("first part %v %v", done, err)
	}
	s.close()
	s2, err := newDurableStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.close()
	s2.volumeProbe = healthyMailboxVolume
	if done, err := s2.putBlobPart(scope, id, "image/png", 1, 2, data[half:]); err != nil || !done {
		t.Fatalf("restart upload %v %v", done, err)
	}
	if _, err = s2.putBlobPart(scope, id, "image/png", 0, 2, []byte("changed")); !errors.Is(err, errBlobConflict) {
		t.Fatal("same id changed payload accepted")
	}
	reader, err := s2.blobReader(scope, id)
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(out, data) {
		t.Fatal("decrypted image changed")
	}
	if _, _, err = s2.blobInfo(shaHex("other"), id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("blob scope leaked")
	}
	raw := []byte(`{"sessionKey":"s","attachments":["` + id + `"]}`)
	if err = s2.retainCommandBlobs(scope, raw, "operation:one"); err != nil {
		t.Fatal(err)
	}
	if err = s2.retainCommandBlobs(scope, raw, "operation:two"); err != nil {
		t.Fatal(err)
	}
	_, _ = s2.db.Exec(`UPDATE blobs SET expires=? WHERE id=?`, nowMs()-1, id)
	if err = s2.cleanupLocked(nowMs()); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s2.blobInfo(scope, id); err != nil {
		t.Fatal("referenced image deleted")
	}
	_, _ = s2.db.Exec(`DELETE FROM blob_refs WHERE ref='operation:one'`)
	if err = s2.cleanupLocked(nowMs()); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s2.blobInfo(scope, id); err != nil {
		t.Fatal("shared image deleted")
	}
	_, _ = s2.db.Exec(`UPDATE blob_refs SET expires=?`, nowMs()-1)
	if err = s2.cleanupLocked(nowMs()); err != nil {
		t.Fatal(err)
	}
	info, _, err := s2.blobInfo(scope, id)
	if err != nil || info.ExpiresAt <= nowMs() {
		t.Fatal("last reference has no grace")
	}
	_, _ = s2.db.Exec(`UPDATE blobs SET expires=?,lease=? WHERE id=?`, nowMs()-1, nowMs()+60000, id)
	if err = s2.cleanupLocked(nowMs()); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s2.blobInfo(scope, id); err != nil {
		t.Fatal("active stream deleted")
	}
	_, _ = s2.db.Exec(`UPDATE blobs SET lease=0 WHERE id=?`, id)
	if err = s2.cleanupLocked(nowMs()); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s2.blobInfo(scope, id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("unreferenced expired image retained")
	}
}
func TestDurableLowDiskRejectsImagesKeepsTerminal(t *testing.T) {
	s, _ := mailboxFixture(t)
	s.volumeProbe = func(string) (int64, int64, error) { return 100 << 30, 1 << 30, nil }
	if _, err := s.putBlobPart(shaHex("binding"), "att_"+strings.Repeat("2", 32), "image/png", 0, 1, tinyPNG(t)); !errors.Is(err, errStorageCapacity) {
		t.Fatalf("disk reserve ignored: %v", err)
	}
	if err := s.saveReply("receipt", shaHex("owner"), replyBody{OK: true, Result: json.RawMessage(`{"text":"full"}`)}, 200, true); err != nil {
		t.Fatal(err)
	}
	_, _, state, err := s.reply("receipt")
	if err != nil || state != "truncated" {
		t.Fatal("disk pressure must keep terminal without claiming full body")
	}
}

func TestDurableStreamBudgetKeepsTerminalAndVisibleGap(t *testing.T) {
	s, _ := mailboxFixture(t)
	scope := shaHex("binding")
	for i := int64(1); i <= 5; i++ {
		e := durableEvent(t, i, "s", strings.Repeat("x", 700000))
		var m map[string]json.RawMessage
		_ = json.Unmarshal(e.data, &m)
		m["turnId"] = json.RawMessage(`"turn-one"`)
		m["tool"] = json.RawMessage(`"codex"`)
		e.data, _ = json.Marshal(m)
		if err := s.storeEvents(scope, "", "", []*storedEvent{e}); err != nil {
			t.Fatal(err)
		}
	}
	m := map[string]json.RawMessage{"type": json.RawMessage(`"turn"`), "sessionKey": json.RawMessage(`"s"`), "tool": json.RawMessage(`"codex"`), "turnId": json.RawMessage(`"turn-one"`), "status": json.RawMessage(`"completed"`)}
	terminal, err := prepareStoredEvent("pc", 6, m)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.storeEvents(scope, "", "", []*storedEvent{terminal}); err != nil {
		t.Fatal(err)
	}
	p, err := s.eventPage(scope, "s", 0, 500)
	if err != nil || !p.ResetRequired || p.NextSeq != 6 {
		t.Fatalf("truncated page %+v %v", p, err)
	}
	var notice, done bool
	for _, raw := range p.Events {
		var row map[string]any
		_ = json.Unmarshal(raw, &row)
		if row["type"] == "notice" {
			notice = true
			for _, field := range []string{"tool", "ts", "level", "text", "errorCode"} {
				if row[field] == nil {
					t.Fatalf("notice missing mobile contract %s", field)
				}
			}
		}
		if row["type"] == "turn" && row["status"] == "completed" {
			done = true
		}
	}
	if !notice || !done {
		t.Fatal("truncation lost marker or terminal")
	}
}
func prepareHTTP(t *testing.T, h *Hub, token, intent string, command json.RawMessage) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"deviceId": "pc", "requestId": intent, "command": command})
	r := httptest.NewRequest("POST", "/app/commands/prepare", bytes.NewReader(raw))
	r.Header.Set("X-Salcara-Pair-Token", token)
	w := httptest.NewRecorder()
	h.handleOperationPrepare(w, r, "receipt-owner")
	if w.Code != 200 {
		t.Fatalf("prepare %d %s", w.Code, w.Body.String())
	}
	var value map[string]any
	if json.Unmarshal(w.Body.Bytes(), &value) != nil {
		t.Fatal("invalid prepare")
	}
	return value
}
func TestDurablePrepareIdempotentCommitAndReceipt(t *testing.T) {
	h, conn, token, _ := receiptLookupFixture(t)
	cmd := json.RawMessage(`{"type":"session.send","text":"durable","operationId":"` + receiptRequestID + `"}`)
	first := prepareHTTP(t, h, token, receiptRequestID, cmd)
	second := prepareHTTP(t, h, token, receiptRequestID, cmd)
	if first["operationId"] != second["operationId"] || first["ticket"] != second["ticket"] || first["dispatchNotAfter"] != second["dispatchNotAfter"] {
		t.Fatal("prepare renewed identity")
	}
	if len(conn.cmds) > 0 {
		t.Fatal("prepare dispatched")
	}
	oldShape := lookupReceiptHTTP(h, token, "receipt-owner", map[string]any{"deviceId": "pc", "requestId": receiptRequestID, "command": cmd})
	if oldShape.Code != 409 || len(conn.cmds) > 0 {
		t.Fatal("prepared client intent downgraded to legacy dispatch")
	}
	id, ticket := first["operationId"].(string), first["ticket"].(string)
	final := json.RawMessage(`{"type":"session.send","text":"durable","operationId":"` + id + `"}`)
	// Omitting a v2 ticket cannot opt the server UUID into legacy dispatch.
	w := lookupReceiptHTTP(h, token, "receipt-owner", map[string]any{"deviceId": "pc", "requestId": id, "command": final})
	if w.Code != 409 || len(conn.cmds) > 0 {
		t.Fatal("v2 identity bypassed ticket")
	}
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		result <- lookupReceiptHTTP(h, token, "receipt-owner", map[string]any{"deviceId": "pc", "requestId": id, "executionTicket": ticket, "command": final})
	}()
	replyReceipt(t, h, conn, `{"confirmed":true}`)
	if w = <-result; w.Code != 200 {
		t.Fatalf("commit %d %s", w.Code, w.Body.String())
	}
	r := httptest.NewRequest(http.MethodGet, "/app/operations?deviceId=pc&operationId="+id+"&ticket="+url.QueryEscape(ticket), nil)
	r.Header.Set("X-Salcara-Pair-Token", token)
	w = httptest.NewRecorder()
	h.handleOperationRead(w, r, "receipt-owner")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"completed"`) || !strings.Contains(w.Body.String(), `"confirmed":true`) {
		t.Fatalf("read %s", w.Body.String())
	}
	if len(conn.cmds) > 0 {
		t.Fatal("lookup dispatched")
	}
}
func TestDurableSignedExpiryCannotRenewOrDispatch(t *testing.T) {
	h, conn, token, _ := receiptLookupFixture(t)
	cmd := json.RawMessage(`{"type":"session.send","text":"expires"}`)
	prepared := prepareHTTP(t, h, token, receiptRequestID, cmd)
	ticket, _ := h.durable.verifyTicket(prepared["ticket"].(string))
	ticket.Deadline = nowMs() - 1
	expired := h.durable.signTicket(ticket)
	_, err := h.durable.db.Exec(`UPDATE operations SET deadline=?,ticket=? WHERE id=?`, ticket.Deadline, expired, ticket.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	again := prepareHTTP(t, h, token, receiptRequestID, cmd)
	if again["ticket"] != expired || again["state"] != "expired" {
		t.Fatal("expired intent renewed")
	}
	final := json.RawMessage(`{"type":"session.send","text":"expires","operationId":"` + ticket.OperationID + `"}`)
	w := lookupReceiptHTTP(h, token, "receipt-owner", map[string]any{"deviceId": "pc", "requestId": ticket.OperationID, "executionTicket": expired, "command": final})
	if w.Code < 400 || len(conn.cmds) != 0 {
		t.Fatal("expired ticket dispatched")
	}
}
func TestDurableQueuedClaimAfterRestartUncertain(t *testing.T) {
	s, dir := mailboxFixture(t)
	payload, err := s.seal([]byte(`{"command":{"type":"session.send","text":"private-offline"}}`), "queue:q")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`INSERT INTO offline_queue(id,owner,scope,deadline,state,body,bytes) VALUES('q','o','s',?,'claimed',?,?)`, nowMs()+60000, payload, len(payload))
	if err != nil {
		t.Fatal(err)
	}
	s.close()
	next, err := newDurableStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer next.close()
	var state string
	var body []byte
	if err = next.db.QueryRow(`SELECT state,body FROM offline_queue WHERE id='q'`).Scan(&state, &body); err != nil || state != "uncertain" || body != nil {
		t.Fatal("claimed work recovered as runnable")
	}
}
func TestDurableReceiptRestartReturnsFullReply(t *testing.T) {
	h, conn := receiptFixture(t, time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = h.executeReceipt(context.Background(), "receipt-owner", "pc", "pair", receiptRequestID, json.RawMessage(`{"type":"session.send","text":"x"}`), nil)
	}()
	replyReceipt(t, h, conn, `{"complete":"yes"}`)
	<-done
	h.receipts.close()
	receipts, err := newCommandReceipts(h.cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	receipts.store = h.durable
	h.receipts = receipts
	rep, status, code := h.executeReceipt(context.Background(), "receipt-owner", "pc", "pair", receiptRequestID, json.RawMessage(`{"type":"session.send","text":"x"}`), nil)
	if status != 200 || code != "" || !bytes.Contains(rep.Result, []byte("yes")) || len(conn.cmds) > 0 {
		t.Fatal("restart lost full reply or redispatched")
	}
}

func TestDurablePrepareCannotMigrateLegacyReceipt(t *testing.T) {
	h, _, token, credential := receiptLookupFixture(t)
	owner := shaHex("receipt-owner\x00pc\x00" + credential)
	key := shaHex(owner + "\x00" + receiptRequestID)
	h.receipts.mu.Lock()
	entry := &commandReceipt{durableCommandReceipt: durableCommandReceipt{Version: 1, Owner: owner, Hash: shaHex("old"), ExpiresAt: maxCommandReceiptExpiry}, done: make(chan struct{}), completed: true}
	close(entry.done)
	h.receipts.entries[key] = entry
	h.receipts.mu.Unlock()
	raw := []byte(`{"deviceId":"pc","requestId":"` + receiptRequestID + `","command":{"type":"session.send","text":"old"}}`)
	r := httptest.NewRequest("POST", "/app/commands/prepare", bytes.NewReader(raw))
	r.Header.Set("X-Salcara-Pair-Token", token)
	w := httptest.NewRecorder()
	h.handleOperationPrepare(w, r, "receipt-owner")
	if w.Code != 409 || !strings.Contains(w.Body.String(), "legacy_operation_requires_recovery") {
		t.Fatalf("legacy operation migrated: %d %s", w.Code, w.Body.String())
	}
}

func TestDurableOfflineQueueDispatchesOnceAfterReconnect(t *testing.T) {
	h, conn, token, _ := receiptLookupFixture(t)
	policy := h.DurableStorageStatus().Policy
	policy.OfflineQueueEnabled = true
	if err := h.SetStoragePolicy(policy); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.accounts["receipt-owner"].devices["pc"].conn = nil
	h.mu.Unlock()
	prepared := prepareHTTP(t, h, token, receiptRequestID, json.RawMessage(`{"type":"session.send","text":"offline"}`))
	id, ticket := prepared["operationId"].(string), prepared["ticket"].(string)
	command := json.RawMessage(`{"type":"session.send","text":"offline","operationId":"` + id + `"}`)
	body := map[string]any{"deviceId": "pc", "requestId": id, "executionTicket": ticket, "command": command}
	for i := 0; i < 2; i++ {
		w := lookupReceiptHTTP(h, token, "receipt-owner", body)
		if w.Code != 202 || !strings.Contains(w.Body.String(), `"state":"queued"`) {
			t.Fatalf("queue %d %s", w.Code, w.Body.String())
		}
	}
	if len(conn.cmds) > 0 {
		t.Fatal("offline queue dispatched early")
	}
	h.mu.Lock()
	h.accounts["receipt-owner"].devices["pc"].conn = conn
	h.mu.Unlock()
	h.dispatchOfflineQueue()
	replyReceipt(t, h, conn, `{"delivered":true}`)
	deadline := time.After(3 * time.Second)
	for {
		var state string
		_ = h.durable.db.QueryRow(`SELECT state FROM offline_queue LIMIT 1`).Scan(&state)
		if state == "completed" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("queue did not complete")
		case <-time.After(time.Millisecond):
		}
	}
	h.dispatchOfflineQueue()
	if len(conn.cmds) > 0 {
		t.Fatal("completed queue replayed")
	}
}

func TestDurableThumbnailBoundsAndExpiry(t *testing.T) {
	s, _ := mailboxFixture(t)
	scope, id := shaHex("binding"), "att_"+strings.Repeat("7", 32)
	if _, err := s.putBlobPart(scope, id, "image/png", 0, 1, tinyPNG(t)); err != nil {
		t.Fatal(err)
	}
	thumb, err := s.thumbnail(scope, id)
	if err != nil {
		t.Fatal(err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(thumb))
	if err != nil || format != "jpeg" || cfg.Width > 512 || cfg.Height > 512 {
		t.Fatal("invalid preview")
	}
	var stored []byte
	var expires int64
	if err = s.db.QueryRow(`SELECT body,expires FROM thumbnails WHERE id=?`, id).Scan(&stored, &expires); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(stored, thumb) || expires < nowMs()+6*durableDay {
		t.Fatal("thumbnail is not encrypted or has wrong retention")
	}
	_, _ = s.db.Exec(`UPDATE thumbnails SET expires=? WHERE id=?`, nowMs()-1, id)
	if err = s.cleanupLocked(nowMs()); err != nil {
		t.Fatal(err)
	}
	var count int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM thumbnails`).Scan(&count)
	if count != 0 {
		t.Fatal("expired thumbnail retained")
	}
	if _, _, err = s.blobInfo(scope, id); err != nil {
		t.Fatal("thumbnail cleanup deleted original")
	}
}

func TestDurableBlobRestartAfterFinalPartBeforeReady(t *testing.T) {
	s, dir := mailboxFixture(t)
	scope, id := shaHex("binding"), "att_"+strings.Repeat("b", 32)
	data := tinyPNG(t)
	if _, err := s.putBlobPart(scope, id, "image/png", 0, 1, data); err != nil {
		t.Fatal(err)
	}
	// Recreate the durable state at the crash boundary: the last part is
	// committed, but image verification and the ready marker are absent.
	if _, err := s.db.Exec(`UPDATE blobs SET state='uploading',hash='',width=0,height=0 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	s.close()
	restarted, err := newDurableStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.close()
	if done, err := restarted.putBlobPart(scope, id, "image/png", 0, 1, data); err != nil || !done {
		t.Fatalf("last-part retry did not complete interrupted finalization: %v %v", done, err)
	}
	info, state, err := restarted.blobInfo(scope, id)
	digest := sha256.Sum256(data)
	if err != nil || state != "ready" || info.Hash != hex.EncodeToString(digest[:]) || info.Width != 2 || info.Height != 2 {
		t.Fatalf("recovered blob not verified: %+v %s %v", info, state, err)
	}
}

func TestDurableThumbnailCommandPaginatesPreviewAndEnforcesSession(t *testing.T) {
	h, conn, token, _ := receiptLookupFixture(t)
	r := httptest.NewRequest(http.MethodPost, "/app/commands", nil)
	r.Header.Set("X-Salcara-Pair-Token", token)
	_, scope, ok := h.appStorageIdentity("receipt-owner", r, "pc")
	if !ok {
		t.Fatal("fixture has no mailbox scope")
	}
	id := "att_" + strings.Repeat("8", 32)
	original := tinyPNG(t)
	if _, err := h.durable.putBlobPart(scope, id, "image/png", 0, 1, original); err != nil {
		t.Fatal(err)
	}
	if err := h.durable.retainCommandBlobs(scope, json.RawMessage(`{"sessionKey":"codex:s","attachments":["`+id+`"]}`), "operation:preview"); err != nil {
		t.Fatal(err)
	}
	var joined []byte
	for offset := 0; ; {
		w := lookupReceiptHTTP(h, token, "receipt-owner", map[string]any{"deviceId": "pc", "command": map[string]any{"type": "attachment.read", "id": id, "sessionKey": "codex:s", "variant": "thumbnail", "offset": offset, "length": 127}})
		var out struct {
			OK     bool `json:"ok"`
			Result struct {
				ID, Variant, Mime, SHA256, Data         string
				Size, Width, Height, Offset, NextOffset int
				Complete                                bool
			} `json:"result"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || !out.OK {
			t.Fatalf("preview read failed: %d %s", w.Code, w.Body)
		}
		part, err := base64.StdEncoding.DecodeString(out.Result.Data)
		if err != nil || out.Result.ID != id || out.Result.Variant != "thumbnail" || out.Result.Mime != "image/jpeg" || out.Result.Width > 512 || out.Result.Height > 512 || out.Result.Offset != offset || out.Result.NextOffset != offset+len(part) || len(part) > 127 {
			t.Fatalf("preview contract changed: %+v, %v", out.Result, err)
		}
		joined = append(joined, part...)
		if out.Result.Complete {
			digest := sha256.Sum256(joined)
			if out.Result.Size != len(joined) || out.Result.SHA256 != hex.EncodeToString(digest[:]) || bytes.Equal(joined, original) {
				t.Fatal("preview returned original metadata or data")
			}
			break
		}
		if len(part) == 0 || len(joined) > 1<<20 {
			t.Fatal("preview pagination did not advance")
		}
		offset = out.Result.NextOffset
	}
	for _, check := range []struct {
		id, session string
		status      int
	}{{id, "codex:other", 403}, {"att_" + strings.Repeat("9", 32), "codex:s", 410}} {
		w := lookupReceiptHTTP(h, token, "receipt-owner", map[string]any{"deviceId": "pc", "command": map[string]any{"type": "attachment.read", "id": check.id, "sessionKey": check.session, "variant": "thumbnail"}})
		if w.Code != check.status {
			t.Fatalf("preview authorization/fallback: %d %s", w.Code, w.Body)
		}
	}
	if len(conn.cmds) != 0 {
		t.Fatal("thumbnail request fell back to Bridge original-image transfer")
	}
}

func TestDurableRestrictedChunkUploadReleasesVerifiedChallengeSlots(t *testing.T) {
	h, pair, req, snapshot, private := sourceContractFixture(t)
	data := tinyPNG(t)
	id := "att_" + strings.Repeat("a", 32)
	const parts = 40 // More than the 32 outstanding source acquisition slots.
	for index := 0; index < parts; index++ {
		req.RequestID = fmt.Sprintf("10000000-0000-4000-8000-%012d", index+1)
		req.Command, _ = json.Marshal(map[string]any{"type": "attachment.put", "id": id, "mime": "image/png", "index": index, "total": parts, "data": base64.StdEncoding.EncodeToString(data[len(data)*index/parts : len(data)*(index+1)/parts]), "accountId": "credential-1", "agent": "codex", "controlSurface": "cli", "model": "gpt"})
		canonical, err := canonicalCommand(req.Command)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.RequestID, snapshot.CommandHash = req.RequestID, shaHex(canonical)
		challenge := issueSourceChallengeFixture(t, h, pair, req)
		snapshot.ChallengeID = challenge.ChallengeID
		signSourceFixture(t, &req, snapshot, private)
		body, _ := json.Marshal(req)
		r := httptest.NewRequest(http.MethodPost, "/app/commands", bytes.NewReader(body))
		r.Header = pair.Header.Clone()
		w := httptest.NewRecorder()
		h.handleAppCommands(w, r, "receipt-owner")
		if w.Code != 200 {
			t.Fatalf("chunk %d rejected: %d %s", index, w.Code, w.Body)
		}
		h.mu.Lock()
		retained := len(h.sourceChallenges)
		h.mu.Unlock()
		if retained != 0 {
			t.Fatalf("chunk %d still consumes %d source slots", index, retained)
		}
	}
	_, scope, _ := h.appStorageIdentity("receipt-owner", pair, "pc")
	if info, state, err := h.durable.blobInfo(scope, id); err != nil || state != "ready" || info.Size != int64(len(data)) {
		t.Fatalf("full upload not retained: %+v %s %v", info, state, err)
	}
}

func TestDurableCleanupEnforcesSharedPassBudget(t *testing.T) {
	s, _ := mailboxFixture(t)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		if _, err = tx.Exec(`INSERT INTO replies(id,owner,body,status,expires,state,bytes) VALUES(?, 'owner', ?, 200, 1, 'completed', 100)`, i, []byte("ciphertext")); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = s.cleanupLocked(nowMs()); err != nil {
		t.Fatal(err)
	}
	var left int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM replies WHERE body IS NOT NULL`).Scan(&left)
	if left < 100 {
		t.Fatalf("cleanup exceeded 500 logical items: %d left", left)
	}
	_, _ = s.db.Exec(`UPDATE replies SET bytes=? WHERE body IS NOT NULL`, 3<<20)
	if err = s.cleanupLocked(nowMs()); err != nil {
		t.Fatal(err)
	}
	var after int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM replies WHERE body IS NOT NULL`).Scan(&after)
	if int64(left-after)*(3<<20) > 50<<20 {
		t.Fatal("cleanup exceeded byte budget")
	}
}
