package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestBotStorageOfflineQueueRechecksAfterSourceWait(t *testing.T) {
	for _, mode := range []string{"unchanged", "revoke", "pending", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			h, _, _ := botFixture(t)
			defer h.Close()
			h.durable.volumeProbe = healthyMailboxVolume
			p, token := botScopePhone(t, h, "offline-storage-computer")
			policy := h.DurableStorageStatus().Policy
			policy.OfflineQueueEnabled = true
			if err := h.SetStoragePolicy(policy); err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "/v1/app/commands", nil)
			r.Header.Set("X-Salcara-Bot-Token", token)
			r = r.WithContext(context.WithValue(r.Context(), ticketDeadlineKey{}, nowMs()+60000))
			req := commandReq{DeviceID: "offline-storage-computer", RequestID: receiptRequestID, Command: json.RawMessage(`{"type":"session.send","text":"original generation"}`)}
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			w := httptest.NewRecorder()
			go func() {
				defer close(done)
				h.maybeQueueOffline(w, r, "bot:"+p.GroupID, req, func() (string, *ExecutionSource) {
					close(entered)
					<-release
					return "", &ExecutionSource{Mode: "open"}
				})
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("queue did not reach final source check")
			}
			var unblock func()
			if mode == "pending" {
				unblock = blockBotRegistrationSave(t, h, botGroupFile)
			}
			if mode == "revoke" || mode == "pending" {
				want := 200
				if mode == "pending" {
					want = 503
				}
				requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/revoke", nil, map[string]string{"X-Salcara-Bot-Token": token}), want, "")
			} else if mode == "replacement" {
				fresh := resetBotPhonePackage(t, h, p.GroupID)
				requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/bind", botV2Body(fresh, strings.Repeat("c", 64)), nil), 200, "")
			}
			close(release)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("queue request stalled")
			}
			want := 403
			if mode == "pending" {
				want = 503
			}
			if mode == "unchanged" {
				want = 202
			}
			requireBotCode(t, w, want, "")
			var count int
			if err := h.durable.db.QueryRow(`SELECT COUNT(*) FROM offline_queue`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			expected := 0
			if mode == "unchanged" {
				expected = 1
			}
			if count != expected {
				t.Fatalf("queue rows %d want %d", count, expected)
			}
			if unblock != nil {
				unblock()
			}
		})
	}
}

// Observe a real mutex wait in this single, non-parallel fixture; no production
// hook or scheduling sleep stands in for reaching the post-authentication sink.
func waitBotPrepareReceiptLock(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		buf := make([]byte, 256<<10)
		stacks := string(buf[:runtime.Stack(buf, true)])
		for _, stack := range strings.Split(stacks, "\n\n") {
			if strings.Contains(stack, "(*Hub).handleOperationPrepare") && strings.Contains(stack, "sync.(*Mutex)") {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("prepare did not wait at receipts lock")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBotStoragePrepareRechecksAfterReceiptWait(t *testing.T) {
	h, _, _ := botFixture(t)
	defer h.Close()
	h.durable.volumeProbe = healthyMailboxVolume
	_, token := botScopePhone(t, h, "prepare-storage-computer")
	body := map[string]any{"deviceId": "prepare-storage-computer", "requestId": receiptRequestID, "command": map[string]any{"type": "session.send", "text": "do not prepare after revoke"}}
	h.receipts.mu.Lock()
	locked := true
	defer func() {
		if locked {
			h.receipts.mu.Unlock()
		}
	}()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- botRequest(t, h, "POST", "/v1/app/commands/prepare", body, map[string]string{"X-Salcara-Bot-Token": token})
	}()
	waitBotPrepareReceiptLock(t)
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/revoke", nil, map[string]string{"X-Salcara-Bot-Token": token}), 200, "")
	h.receipts.mu.Unlock()
	locked = false
	select {
	case w := <-done:
		requireBotCode(t, w, 403, "bot_credentials_invalid")
	case <-time.After(3 * time.Second):
		t.Fatal("prepare stalled")
	}
	for _, table := range []string{"operations", "quota_owners"} {
		var count int
		if err := h.durable.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("revoked prepare wrote %s", table)
		}
	}
}
