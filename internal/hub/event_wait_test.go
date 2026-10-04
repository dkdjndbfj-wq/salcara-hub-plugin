package hub

import (
	"encoding/json"
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
