package hub

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func standbyFixture(t *testing.T) (*Hub, string, string, string) {
	t.Helper()
	h := newDeviceHub(t, Config{DataDir: t.TempDir(), CommandTimeout: 2 * time.Second})
	id, secret, token := "standby-pc", strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	enrollDevice(t, h, id, secret)
	h.mu.Lock()
	dev := h.accounts[deviceAccount(id)].devices[id]
	h.setPairLocked(deviceAccount(id), dev, sha256.Sum256([]byte(token)), true)
	dev.bindingID, dev.phoneHash = strings.Repeat("a", 64), strings.Repeat("b", 64)
	h.mu.Unlock()
	return h, id, secret, token
}

func standbyPoll(t *testing.T, h *Hub, id, secret string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "http://example.test/salcara-hub/v1/bridge/standby", nil)
	r.RemoteAddr = "192.0.2.1:1234"
	r.Header.Set("X-Salcara-Device-Id", id)
	r.Header.Set("X-Salcara-Device-Secret", secret)
	r.Header.Set("X-Salcara-Computer-Id", "physical-pc")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestStandbyShortPollHandoverIsNotAnOnlineTaskChannel(t *testing.T) {
	h, id, secret, token := standbyFixture(t)
	w := standbyPoll(t, h, id, secret)
	if w.Code != 200 {
		t.Fatalf("poll: %d %s", w.Code, w.Body)
	}
	h.mu.Lock()
	dev := h.accounts[deviceAccount(id)].devices[id]
	if dev.status().Online || dev.conn != nil {
		t.Fatal("standby advertised a live Agent channel")
	}
	h.mu.Unlock()
	command := json.RawMessage(`{"type":"remote.station.switch","targetHubUrl":"http://example.test/salcara-hub","targetDeviceId":"standby-pc","targetComputerId":"physical-pc","operationId":"10000000-0000-4000-8000-000000000001"}`)
	finished := make(chan int, 1)
	go func() {
		_, status, _ := h.executeReceipt(context.Background(), deviceAccount(id), id, "paired-owner", receiptRequestID, command, func(dev *device) bool { return dev.hasPair })
		finished <- status
	}()
	var result struct{ Commands []commandEnvelope }
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		w = standbyPoll(t, h, id, secret)
		if json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatal("bad poll response")
		}
		if len(result.Commands) != 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(result.Commands) != 1 || result.Commands[0].PhoneHash != strings.Repeat("b", 64) {
		t.Fatal("missing authenticated handover")
	}
	w = deviceRequest(t, h, "/bridge/reply", id, secret, "", map[string]any{"deviceId": id, "commandId": result.Commands[0].CommandID, "ok": true, "result": map[string]bool{"switched": true}}, "")
	if w.Code != 200 || <-finished != 200 {
		t.Fatal("handover acknowledgement failed")
	}
	// Pair-token authorization remains mandatory even though standby is available.
	w = deviceRequest(t, h, "/app/commands", "", "", token, map[string]any{"deviceId": id, "requestId": "10000000-0000-4000-8000-000000000002", "command": map[string]any{"type": "session.send", "text": "must not send"}}, "")
	if w.Code != 409 || !strings.Contains(w.Body.String(), "computer_offline") {
		t.Fatal("standby accepted an ordinary task")
	}
}

func TestStandbyRejectsExpiredWrongIdentityAndCredentialSmuggling(t *testing.T) {
	h, id, secret, _ := standbyFixture(t)
	if w := standbyPoll(t, h, id, secret); w.Code != 200 {
		t.Fatal("poll failed")
	}
	base := map[string]any{"type": "remote.station.switch", "targetHubUrl": "http://example.test/salcara-hub", "targetDeviceId": id, "targetComputerId": "physical-pc", "operationId": receiptRequestID}
	for _, change := range []map[string]any{{"type": "session.send"}, {"targetComputerId": "other-pc"}, {"targetDeviceId": "other-pc"}, {"apiKey": "synthetic-secret"}, {"operationId": "bad"}} {
		candidate := map[string]any{}
		for k, v := range base {
			candidate[k] = v
		}
		for k, v := range change {
			candidate[k] = v
		}
		raw, _ := json.Marshal(candidate)
		h.mu.Lock()
		conn := h.standbyCommandLocked(h.accounts[deviceAccount(id)].devices[id], raw)
		h.mu.Unlock()
		if conn != nil {
			t.Fatalf("invalid standby command accepted: %v", change)
		}
	}
	h.mu.Lock()
	dev := h.accounts[deviceAccount(id)].devices[id]
	dev.standbyAt = nowMs() - standbyTTL.Milliseconds() - 1
	raw, _ := json.Marshal(base)
	if h.standbyCommandLocked(dev, raw) != nil {
		t.Fatal("expired standby accepted")
	}
	h.mu.Unlock()
	if w := standbyPoll(t, h, id, strings.Repeat("ef", 32)); w.Code != 403 {
		t.Fatal("wrong device secret accepted")
	}
	h.mu.Lock()
	h.revokePairLocked(deviceAccount(id), dev)
	h.mu.Unlock()
	if w := standbyPoll(t, h, id, secret); w.Code != 403 {
		t.Fatal("revoked phone standby accepted")
	}
}
