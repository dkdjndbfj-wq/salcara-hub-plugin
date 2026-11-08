package hub

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDurableBotGenerationRejectsOldOrUnscopedEventsAndBlobs(t *testing.T) {
	h, dir, publicURL := botFixture(t)
	t.Cleanup(h.Close)
	h.durable.volumeProbe = healthyMailboxVolume
	pkg, err := h.CreateBotBindingPackage()
	if err != nil {
		t.Fatal(err)
	}
	const deviceID = "bot-generation-pc"
	secret := strings.Repeat("a", 64)
	acct := deviceAccount(deviceID)
	conn := &bridgeConn{cmds: make(chan []byte, 1), closed: make(chan struct{})}
	h.mu.Lock()
	g := h.botGroups[pkg.GroupID]
	g.Computers[deviceID] = true
	g.PhoneTokenHash = shaHex("phone-one")
	g.PhoneIDHash = shaHex("phone-id-one")
	d := &device{info: Device{DeviceID: deviceID}, secretHash: sha256.Sum256([]byte(secret)), hasSecret: true, conn: conn}
	h.accountLocked(acct).devices[deviceID] = d
	old := h.durableScopeLocked(acct, d)
	h.mu.Unlock()
	post := func(scope, batch, text string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"deviceId": deviceID, "eventScope": scope, "batchId": batch, "events": []any{map[string]any{"type": "message", "tool": "codex", "sessionKey": "codex:s", "id": text, "text": text, "role": "assistant", "final": true}}})
		r := httptest.NewRequest("POST", "/bridge/events", bytes.NewReader(body))
		r.Header.Set("X-Salcara-Device-Secret", secret)
		w := httptest.NewRecorder()
		h.handleBridgeEvents(w, r, acct)
		return w
	}
	if w := post("", receiptRequestID, "old"); w.Code != 409 || !strings.Contains(w.Body.String(), "event_scope_changed") {
		t.Fatalf("unscoped Bot upload accepted: %d %s", w.Code, w.Body.String())
	}
	if w := post(old, receiptRequestID, "old"); w.Code != 200 {
		t.Fatalf("valid old upload %d %s", w.Code, w.Body.String())
	}
	h.mu.Lock()
	g.PhoneTokenHash = shaHex("phone-two")
	g.PhoneIDHash = shaHex("phone-id-two")
	h.notifyBotScopeChangedLocked(g)
	fresh := h.durableScopeLocked(acct, d)
	h.mu.Unlock()
	if old == fresh {
		t.Fatal("phone replacement did not change mailbox scope")
	}
	select {
	case <-conn.closed:
	default:
		t.Fatal("old stream not retired")
	}
	if w := post(old, receiptRequestID, "old"); w.Code != 409 {
		t.Fatal("old frozen batch retargeted to replacement phone")
	}
	if w := post(fresh, "10000000-0000-4000-8000-000000000002", "fresh"); w.Code != 200 {
		t.Fatalf("new generation rejected: %s", w.Body.String())
	}
	h.mu.Lock()
	page := h.readEventPageLocked(acct, deviceID, "codex:s", 0, 10)
	h.mu.Unlock()
	if len(page.Events) != 1 || !bytes.Contains(page.Events[0], []byte("fresh")) {
		t.Fatal("old generation contents visible")
	}
	for _, scope := range []string{"", old} {
		r := httptest.NewRequest("GET", "/bridge/blobs?deviceId="+deviceID+"&id=att_"+strings.Repeat("1", 32), nil)
		r.Header.Set("X-Salcara-Device-Secret", secret)
		r.Header.Set("X-Salcara-Event-Scope", scope)
		w := httptest.NewRecorder()
		h.handleBlobs(w, r, acct)
		if w.Code != 409 {
			t.Fatalf("old/unscoped blob read admitted: %d", w.Code)
		}
	}
	if err = h.saveNowError(); err != nil {
		t.Fatal(err)
	}
	if err = h.saveBotStateError(); err != nil {
		t.Fatal(err)
	}
	h.Close()
	restarted, err := New(Config{DataDir: dir, DeploymentMode: "bot", PublicURL: publicURL})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if restarted.bridgeEventScope(acct, deviceID) != fresh {
		t.Fatal("ordinary restart changed the persisted generation")
	}
}
