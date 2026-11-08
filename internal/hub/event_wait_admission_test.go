package hub

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type heldPhoneRead struct {
	cancel context.CancelFunc
	done   chan *httptest.ResponseRecorder
}

func phoneReadRequest(p concurrentPhone, path string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://example.test/salcara-hub/v1"+path, nil)
	r.RemoteAddr = p.ip + ":1234"
	r.Header.Set("X-Salcara-Pair-Token", p.token)
	return r
}

func holdPhoneRead(t *testing.T, h *Hub, p concurrentPhone, path string) heldPhoneRead {
	t.Helper()
	return holdHTTPRead(t, h, phoneReadRequest(p, path))
}

func holdHTTPRead(t *testing.T, h *Hub, request *http.Request) heldPhoneRead {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := request.WithContext(ctx)
	out := heldPhoneRead{cancel: cancel, done: make(chan *httptest.ResponseRecorder, 1)}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		out.done <- w
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Error("synthetic read did not drain after cancellation")
		}
	})
	return out
}

func emptyWaitPath(p concurrentPhone, seconds int) string {
	return fmt.Sprintf("/app/events?deviceId=%s&sessionKey=codex:s&after=0&wait=%d", p.id, seconds)
}

func awaitReadState(t *testing.T, h *Hub, ready func() bool) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		h.mu.Lock()
		ok := ready()
		h.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("synthetic read admission did not reach expected state")
		case <-tick.C:
		}
	}
}

func awaitPhoneRead(t *testing.T, read heldPhoneRead) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case w := <-read.done:
		return w
	case <-time.After(3 * time.Second):
		t.Fatal("synthetic read did not finish")
		return nil
	}
}

func requireReadCapacity(t *testing.T, h *Hub, p concurrentPhone, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, phoneReadRequest(p, path).WithContext(ctx))
	var result struct {
		Code      string `json:"code"`
		Retryable bool   `json:"retryable"`
	}
	if w.Code != http.StatusTooManyRequests || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Code != "app_read_capacity" || !result.Retryable || w.Header().Get("Retry-After") != "2" {
		t.Fatalf("full read budget not rejected safely: status=%d body=%s", w.Code, w.Body)
	}
}

func TestLongPollAdmissionSharesExistingSSEOwnerBudget(t *testing.T) {
	h := newDeviceHub(t, Config{ResourceMode: "economy"})
	p := concurrentPhones(t, h, 1)[0]
	for range h.CurrentResourceMode().MaxAppStreams {
		holdPhoneRead(t, h, p, "/app/stream")
	}
	awaitReadState(t, h, func() bool { return h.appStreamCountLocked() == h.resource.MaxAppStreams })
	w := concurrentRequest(t, h, p, "/app/events?deviceId="+p.id+"&sessionKey=codex:s&after=0&wait=1", false, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("long poll bypassed full SSE owner budget: got %d, want 429", w.Code)
	}
}

func TestLongPollAdmissionOwnerIsolationAndSSECannotBypass(t *testing.T) {
	h := newDeviceHub(t, Config{ResourceMode: "economy"})
	phones := concurrentPhones(t, h, 2)
	first, second := phones[0], phones[1]
	a := h.accounts[deviceAccount(first.id)]
	var reads []heldPhoneRead
	for range h.CurrentResourceMode().MaxAppStreams {
		reads = append(reads, holdPhoneRead(t, h, first, emptyWaitPath(first, 25)))
	}
	awaitReadState(t, h, func() bool { return a.eventWaiters == 4 && h.eventWaiters == 4 })
	requireReadCapacity(t, h, first, emptyWaitPath(first, 1))
	requireReadCapacity(t, h, first, "/app/stream")
	other := holdPhoneRead(t, h, second, emptyWaitPath(second, 25))
	awaitReadState(t, h, func() bool { return h.eventWaiters == 5 })
	reads[0].cancel()
	awaitPhoneRead(t, reads[0])
	awaitReadState(t, h, func() bool { return h.eventWaiters == 4 && a.eventWaiters == 3 })
	stream := holdPhoneRead(t, h, first, "/app/stream")
	awaitReadState(t, h, func() bool { return len(a.apps) == 1 })
	requireReadCapacity(t, h, first, emptyWaitPath(first, 1))
	stream.cancel()
	awaitPhoneRead(t, stream)
	other.cancel()
	awaitPhoneRead(t, other)
	for _, read := range reads[1:] {
		read.cancel()
		awaitPhoneRead(t, read)
	}
	awaitReadState(t, h, func() bool { return h.eventWaiters == 0 && a.eventWaiters == 0 && h.appStreamCountLocked() == 0 })
}

func TestLongPollAdmissionSharesGlobalBudgetWithSSE(t *testing.T) {
	h := newDeviceHub(t, Config{ResourceMode: "economy"})
	phones := concurrentPhones(t, h, 2)
	first, second := phones[0], phones[1]
	h.mu.Lock()
	// A small synthetic global limit makes both owners fit individually while
	// exercising the exact aggregate admission path used by all three modes.
	h.resource.MaxGlobalAppStreams = 3
	h.mu.Unlock()
	one := holdPhoneRead(t, h, first, emptyWaitPath(first, 25))
	two := holdPhoneRead(t, h, first, emptyWaitPath(first, 25))
	awaitReadState(t, h, func() bool { return h.eventWaiters == 2 })
	stream := holdPhoneRead(t, h, second, "/app/stream")
	awaitReadState(t, h, func() bool { return h.appStreamCountLocked() == 1 })
	requireReadCapacity(t, h, second, emptyWaitPath(second, 1))
	requireReadCapacity(t, h, second, "/app/stream")
	two.cancel()
	awaitPhoneRead(t, two)
	replacement := holdPhoneRead(t, h, second, emptyWaitPath(second, 25))
	awaitReadState(t, h, func() bool { return h.eventWaiters == 2 })
	requireReadCapacity(t, h, first, "/app/stream")
	for _, read := range []heldPhoneRead{one, replacement, stream} {
		read.cancel()
		awaitPhoneRead(t, read)
	}
	awaitReadState(t, h, func() bool { return h.eventWaiters == 0 && h.appStreamCountLocked() == 0 })
}

func TestLongPollAdmissionConcurrentRequestsNeverOverbook(t *testing.T) {
	h := newDeviceHub(t, Config{ResourceMode: "economy"})
	phones := concurrentPhones(t, h, 12)
	var reads []heldPhoneRead
	for _, phone := range phones {
		for range 8 {
			reads = append(reads, holdPhoneRead(t, h, phone, emptyWaitPath(phone, 25)))
		}
	}
	wantRejected := len(reads) - h.CurrentResourceMode().MaxGlobalAppStreams
	awaitReadState(t, h, func() bool {
		completed := 0
		for _, read := range reads {
			completed += len(read.done)
		}
		return completed >= wantRejected
	})
	h.mu.Lock()
	if h.eventWaiters != h.resource.MaxGlobalAppStreams {
		t.Errorf("global admission = %d, want %d", h.eventWaiters, h.resource.MaxGlobalAppStreams)
	}
	for _, owner := range h.accounts {
		if owner.eventWaiters > h.resource.MaxAppStreams {
			t.Errorf("owner overbooked: %d > %d", owner.eventWaiters, h.resource.MaxAppStreams)
		}
	}
	h.mu.Unlock()
	rejected := 0
	for _, read := range reads {
		select {
		case result := <-read.done:
			if result.Code != http.StatusTooManyRequests {
				t.Errorf("unadmitted request status = %d", result.Code)
			}
			rejected++
		default:
			read.cancel()
			awaitPhoneRead(t, read)
		}
	}
	if rejected != wantRejected {
		t.Errorf("rejected = %d, want %d", rejected, wantRejected)
	}
	awaitReadState(t, h, func() bool { return h.eventWaiters == 0 })
}

func TestLongPollAdmissionImmediatePagesDoNotReserveSlots(t *testing.T) {
	h := newDeviceHub(t, Config{ResourceMode: "economy"})
	p := concurrentPhones(t, h, 1)[0]
	a := h.accounts[deviceAccount(p.id)]
	for range h.CurrentResourceMode().MaxAppStreams {
		holdPhoneRead(t, h, p, emptyWaitPath(p, 25))
	}
	awaitReadState(t, h, func() bool { return h.eventWaiters == 4 })
	for _, suffix := range []string{
		"&sessionKey=codex:s&after=0&wait=0",        // A manual empty read.
		"&sessionKey=codex:missing&after=1&wait=25", // Authoritative reset response.
	} {
		w := concurrentRequest(t, h, p, "/app/events?deviceId="+p.id+suffix, false, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("immediate event read charged a waiting slot: %d", w.Code)
		}
	}
	h.mu.Lock()
	_, err := a.appendEventLocked(p.id, eventFields(10, "codex:other"))
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	w := concurrentRequest(t, h, p, "/app/events?deviceId="+p.id+"&sessionKey=codex:other&after=0&wait=25", false, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("ready event page charged a waiting slot: %d", w.Code)
	}
	awaitReadState(t, h, func() bool { return h.eventWaiters == 4 && a.eventWaiters == 4 })
}

func TestLongPollAdmissionReleasesOnEveryExit(t *testing.T) {
	for _, action := range []string{"cancel", "timeout", "event", "revoke", "rebind", "shutdown", "account-replaced"} {
		t.Run(action, func(t *testing.T) {
			h := newDeviceHub(t, Config{ResourceMode: "economy"})
			p := concurrentPhones(t, h, 1)[0]
			owner := h.accounts[deviceAccount(p.id)]
			seconds := 25
			if action == "timeout" {
				seconds = 1
			}
			read := holdPhoneRead(t, h, p, emptyWaitPath(p, seconds))
			awaitReadState(t, h, func() bool { return h.eventWaiters == 1 && owner.eventWaiters == 1 })
			wantStatus := http.StatusOK
			switch action {
			case "cancel":
				read.cancel()
			case "event":
				h.mu.Lock()
				_, err := owner.appendEventLocked(p.id, eventFields(10, "codex:s"))
				h.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
			case "revoke":
				w := concurrentRequest(t, h, p, "/app/pair/revoke", false, map[string]string{"deviceId": p.id})
				if w.Code != http.StatusOK {
					t.Fatal("synthetic revoke failed")
				}
				wantStatus = http.StatusForbidden
			case "rebind":
				h.mu.Lock()
				h.setPairLocked(owner.id, owner.devices[p.id], sha256.Sum256([]byte("synthetic-replacement-token")), true)
				h.mu.Unlock()
				wantStatus = http.StatusForbidden
			case "shutdown":
				h.Close()
			case "account-replaced":
				h.mu.Lock()
				h.removeDeviceLocked(owner.id, owner, p.id, owner.devices[p.id])
				h.removeAccountLocked(owner.id, owner)
				h.accountLocked(owner.id)
				h.mu.Unlock()
				wantStatus = http.StatusForbidden
			}
			if w := awaitPhoneRead(t, read); w.Code != wantStatus {
				t.Fatalf("exit status = %d, want %d", w.Code, wantStatus)
			}
			awaitReadState(t, h, func() bool { return h.eventWaiters == 0 && owner.eventWaiters == 0 })
			h.mu.Lock()
			for _, a := range h.accounts {
				if a.eventWaiters != 0 {
					t.Errorf("exit charged a replacement owner: %d", a.eventWaiters)
				}
			}
			h.mu.Unlock()
		})
	}
}

func TestLongPollAdmissionModeChangesPreserveExistingReads(t *testing.T) {
	h := newDeviceHub(t, Config{ResourceMode: "performance"})
	p := concurrentPhones(t, h, 1)[0]
	owner := h.accounts[deviceAccount(p.id)]
	var reads []heldPhoneRead
	for range 5 {
		reads = append(reads, holdPhoneRead(t, h, p, emptyWaitPath(p, 25)))
	}
	awaitReadState(t, h, func() bool { return h.eventWaiters == 5 })
	if err := h.ApplyResourceMode("economy", nil); err != nil {
		t.Fatal(err)
	}
	requireReadCapacity(t, h, p, emptyWaitPath(p, 1))
	requireReadCapacity(t, h, p, "/app/stream")
	for _, read := range reads {
		select {
		case <-read.done:
			t.Fatal("lower mode cancelled an already accepted read")
		default:
		}
	}
	for _, read := range reads[:2] {
		read.cancel()
		awaitPhoneRead(t, read)
	}
	awaitReadState(t, h, func() bool { return h.eventWaiters == 3 })
	replacement := holdPhoneRead(t, h, p, emptyWaitPath(p, 25))
	awaitReadState(t, h, func() bool { return h.eventWaiters == 4 })
	if err := h.ApplyResourceMode("balanced", nil); err != nil {
		t.Fatal(err)
	}
	extra := holdPhoneRead(t, h, p, emptyWaitPath(p, 25))
	awaitReadState(t, h, func() bool { return h.eventWaiters == 5 })
	for _, read := range append(reads[2:], replacement, extra) {
		read.cancel()
		awaitPhoneRead(t, read)
	}
	awaitReadState(t, h, func() bool { return h.eventWaiters == 0 && owner.eventWaiters == 0 })
}
