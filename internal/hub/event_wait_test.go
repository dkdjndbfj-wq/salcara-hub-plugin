package hub

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAppEventsLongPollWakesOnNewEvent(t *testing.T) {
	h := newDeviceHub(t, Config{DataDir: t.TempDir()})
	id, secret := "pc-wait", strings.Repeat("a", 64)
	enrollDevice(t, h, id, secret)
	onlineDevice(t, h, id)
	token := claimQR(t, h, id, issueQR(t, h, id, secret))

	start := time.Now()
	done := make(chan []byte, 1)
	go func() {
		w := deviceRequest(t, h, "/app/events?deviceId="+id+"&sessionKey=codex:s&after=0&wait=5", "", "", token, nil, "")
		if w.Code != 200 {
			t.Errorf("long poll: %d %s", w.Code, w.Body)
		}
		done <- w.Body.Bytes()
	}()
	time.Sleep(150 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("empty long poll returned before any event")
	default:
	}
	w := deviceRequest(t, h, "/bridge/events", id, secret, "", map[string]any{"deviceId": id, "events": []any{
		map[string]any{"type": "message", "sessionKey": "codex:s", "id": "m1", "role": "assistant", "text": "hi", "final": false},
	}}, "")
	if w.Code != 200 {
		t.Fatalf("bridge events: %d %s", w.Code, w.Body)
	}
	select {
	case body := <-done:
		var page eventPage
		if err := json.Unmarshal(body, &page); err != nil || len(page.Events) != 1 {
			t.Fatalf("page: %s %v", body, err)
		}
		if time.Since(start) > 3*time.Second {
			t.Fatal("long poll did not wake promptly")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("long poll never woke")
	}
}

func TestAppEventsLongPollTimesOutEmpty(t *testing.T) {
	h := newDeviceHub(t, Config{DataDir: t.TempDir()})
	id, secret := "pc-idle", strings.Repeat("b", 64)
	enrollDevice(t, h, id, secret)
	onlineDevice(t, h, id)
	token := claimQR(t, h, id, issueQR(t, h, id, secret))
	start := time.Now()
	w := deviceRequest(t, h, "/app/events?deviceId="+id+"&sessionKey=codex:s&after=0&wait=1", "", "", token, nil, "")
	if w.Code != 200 || time.Since(start) < 900*time.Millisecond {
		t.Fatalf("timeout: %d after %v", w.Code, time.Since(start))
	}
}

func TestAppEventsLongPollWakesWhenPhoneIsRevoked(t *testing.T) {
	h := newDeviceHub(t, Config{DataDir: t.TempDir()})
	id, secret := "pc-revoke-wait", strings.Repeat("c", 64)
	enrollDevice(t, h, id, secret)
	onlineDevice(t, h, id)
	token := claimQR(t, h, id, issueQR(t, h, id, secret))
	done := make(chan int, 1)
	go func() {
		w := deviceRequest(t, h, "/app/events?deviceId="+id+"&sessionKey=codex:s&after=0&wait=5", "", "", token, nil, "")
		done <- w.Code
	}()
	time.Sleep(150 * time.Millisecond)
	revoke := deviceRequest(t, h, "/app/pair/revoke", "", "", token, map[string]string{"deviceId": id}, "")
	if revoke.Code != 200 {
		t.Fatalf("revoke: %d %s", revoke.Code, revoke.Body)
	}
	select {
	case code := <-done:
		if code != http.StatusForbidden {
			t.Fatalf("revoked long poll returned %d instead of 403", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("revoked long poll did not wake promptly")
	}
}

func TestBridgeEventBatchRetryIsIdempotent(t *testing.T) {
	h := newDeviceHub(t, Config{DataDir: t.TempDir()})
	id, secret := "pc-batch", strings.Repeat("d", 64)
	enrollDevice(t, h, id, secret)
	onlineDevice(t, h, id)
	batch := map[string]any{"deviceId": id, "batchId": "10000000-0000-4000-8000-000000000001", "events": []any{
		map[string]any{"type": "message", "sessionKey": "codex:s", "id": "m1", "role": "assistant", "text": "one", "final": true},
	}}
	first := deviceRequest(t, h, "/bridge/events", id, secret, "", batch, "")
	second := deviceRequest(t, h, "/bridge/events", id, secret, "", batch, "")
	if first.Code != http.StatusOK || second.Code != http.StatusOK || !strings.Contains(second.Body.String(), `"duplicate":true`) {
		t.Fatalf("batch retry: first=%d second=%d body=%s", first.Code, second.Code, second.Body)
	}
	h.mu.Lock()
	count := len(h.accounts[deviceAccount(id)].events.after(0))
	h.mu.Unlock()
	if count != 1 {
		t.Fatalf("duplicate batch appended %d events", count)
	}
}
