package hub

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// A real ResponseController-capable writer, with only the network replaced by
// a gate. Tests can expire the configured deadline without waiting 30 seconds.
// Recorder state is inspected only after ServeHTTP has returned.
type blockedSSEWriter struct {
	*httptest.ResponseRecorder
	flush          bool
	blocked        chan struct{}
	entered        sync.Once
	release        chan struct{}
	released       sync.Once
	mu             sync.Mutex
	deadline       time.Time
	deadlineChange chan struct{}
}

func (w *blockedSSEWriter) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	w.deadline = deadline
	close(w.deadlineChange)
	w.deadlineChange = make(chan struct{})
	w.mu.Unlock()
	return nil
}

func (w *blockedSSEWriter) wait() error {
	w.entered.Do(func() { close(w.blocked) })
	for {
		w.mu.Lock()
		deadline, changed := w.deadline, w.deadlineChange
		w.mu.Unlock()
		if deadline.IsZero() {
			select {
			case <-w.release:
				return nil
			case <-changed:
				continue
			}
		}
		left := time.Until(deadline)
		if left <= 0 {
			return os.ErrDeadlineExceeded
		}
		timer := time.NewTimer(left)
		select {
		case <-w.release:
			timer.Stop()
			return nil
		case <-changed:
			timer.Stop()
		case <-timer.C:
			return os.ErrDeadlineExceeded
		}
	}
}

func (w *blockedSSEWriter) Write(data []byte) (int, error) {
	if !w.flush && w.Code == http.StatusOK {
		if err := w.wait(); err != nil {
			return 0, err
		}
	}
	return w.ResponseRecorder.Write(data)
}

func (w *blockedSSEWriter) FlushError() error {
	if w.flush {
		if err := w.wait(); err != nil {
			return err
		}
	}
	w.ResponseRecorder.Flush()
	return nil
}

func holdBlockedSSE(t *testing.T, h *Hub, p concurrentPhone, flush bool) (*blockedSSEWriter, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	return holdBlockedSSERequest(t, h, phoneReadRequest(p, "/app/stream"), flush)
}

func holdBlockedSSERequest(t *testing.T, h *Hub, request *http.Request, flush bool) (*blockedSSEWriter, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	w, cancel, done := startBlockedSSERequest(t, h, request, flush)
	awaitSSESignal(t, w.blocked, "SSE never reached the synthetic network gate")
	w.mu.Lock()
	deadline := w.deadline
	w.mu.Unlock()
	if left := time.Until(deadline); sseWriteTimeout != 30*time.Second || left <= sseWriteTimeout/2 || left > sseWriteTimeout {
		t.Fatalf("SSE failed to configure its 30-second write deadline: %v", left)
	}
	return w, cancel, done
}

func startBlockedSSERequest(t *testing.T, h *Hub, request *http.Request, flush bool) (*blockedSSEWriter, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	w := &blockedSSEWriter{ResponseRecorder: httptest.NewRecorder(), flush: flush, blocked: make(chan struct{}), release: make(chan struct{}), deadlineChange: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(w, request.WithContext(ctx))
	}()
	t.Cleanup(func() {
		cancel()
		w.released.Do(func() { close(w.release) })
		awaitSSESignal(t, done, "blocked SSE did not drain during cleanup")
	})
	return w, cancel, done
}

func awaitSSESignal(t *testing.T, done <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal(message)
	}
}

func expireSSEWrite(t *testing.T, w *blockedSSEWriter, done <-chan struct{}) {
	t.Helper()
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	awaitSSESignal(t, done, "SSE ignored its expired write deadline")
	if w.Code != http.StatusOK {
		t.Fatalf("accepted stream status = %d", w.Code)
	}
}

func TestSSEAdmissionDroppedStreamReservesOwnerUntilDrain(t *testing.T) {
	for _, stage := range []string{"write", "flush"} {
		for _, drop := range []string{"queue-bytes", "queue-messages", "revoke"} {
			t.Run(stage+"/"+drop, func(t *testing.T) {
				h := newDeviceHub(t, Config{ResourceMode: "economy"})
				phones := concurrentPhones(t, h, 2)
				first, second := phones[0], phones[1]
				owner := h.accounts[deviceAccount(first.id)]
				h.mu.Lock()
				h.resource.MaxAppStreams = 1
				h.resource.MaxGlobalAppStreams = 3
				if drop == "queue-bytes" {
					h.resource.AppQueueBytes = 1
				} else {
					h.resource.AppQueueMessages = 1
				}
				h.mu.Unlock()
				w, cancel, done := holdBlockedSSE(t, h, first, stage == "flush")
				h.mu.Lock()
				if drop == "revoke" {
					dev := owner.devices[first.id]
					h.revokePairLocked(owner.id, dev)
					first.token = strings.Repeat("e", 64)
					h.setPairLocked(owner.id, dev, sha256.Sum256([]byte(first.token)), true)
				} else {
					owner.sendLocked(sseMsg{deviceID: first.id, event: "event", data: []byte("{}")})
					if drop == "queue-messages" {
						owner.sendLocked(sseMsg{deviceID: first.id, event: "event", data: []byte("{}")})
					}
				}
				registered := len(owner.apps)
				h.mu.Unlock()
				if registered != 0 {
					t.Fatal("test did not remove the dropped subscriber from the registry")
				}
				awaitReadState(t, h, func() bool { return owner.appStreams == 1 && h.appStreamCountLocked() == 1 })
				// Cancel cannot complete a handler already blocked in Write/Flush.
				cancel()
				requireReadCapacity(t, h, first, "/app/stream")
				requireReadCapacity(t, h, first, emptyWaitPath(first, 1))
				other := holdPhoneRead(t, h, second, emptyWaitPath(second, 25))
				awaitReadState(t, h, func() bool { return h.eventWaiters == 1 && h.appStreamCountLocked() == 1 })
				if drop == "queue-messages" {
					w.released.Do(func() { close(w.release) })
					awaitSSESignal(t, done, "cancelled dropped stream did not drain after write completion")
				} else {
					expireSSEWrite(t, w, done)
				}
				awaitReadState(t, h, func() bool { return h.eventWaiters == 1 && owner.appStreams == 0 && h.appStreamCountLocked() == 0 })
				replacement := holdPhoneRead(t, h, first, "/app/stream")
				awaitReadState(t, h, func() bool { return h.appStreamCountLocked() == 1 })
				cancel() // Repeated cancellation of the old handler cannot release the new one.
				requireReadCapacity(t, h, first, emptyWaitPath(first, 1))
				replacement.cancel()
				awaitPhoneRead(t, replacement)
				other.cancel()
				awaitPhoneRead(t, other)
				awaitReadState(t, h, func() bool { return h.eventWaiters == 0 && h.appStreamCountLocked() == 0 })
			})
		}
	}
}

func TestSSEAdmissionRemovedOwnerRetainsGlobalBudgetUntilDeadline(t *testing.T) {
	for _, stage := range []string{"write", "flush"} {
		t.Run(stage, func(t *testing.T) {
			h := newDeviceHub(t, Config{ResourceMode: "economy"})
			phones := concurrentPhones(t, h, 2)
			first, second := phones[0], phones[1]
			owner := h.accounts[deviceAccount(first.id)]
			h.mu.Lock()
			h.resource.MaxGlobalAppStreams = 2
			h.mu.Unlock()
			w, cancel, done := holdBlockedSSE(t, h, first, stage == "flush")
			other := holdPhoneRead(t, h, second, "/app/stream")
			awaitReadState(t, h, func() bool { return h.appStreamCountLocked() == 2 })
			h.mu.Lock()
			h.removeDeviceLocked(owner.id, owner, first.id, owner.devices[first.id])
			h.removeAccountLocked(owner.id, owner)
			h.mu.Unlock()
			first = concurrentPhones(t, h, 1)[0]
			recreated := h.accounts[owner.id]
			if recreated == owner {
				t.Fatal("test did not recreate the original account")
			}
			awaitReadState(t, h, func() bool {
				return owner.appStreams == 1 && recreated.appStreams == 0 && h.appStreamCountLocked() == 2
			})
			requireReadCapacity(t, h, first, "/app/stream")
			requireReadCapacity(t, h, first, emptyWaitPath(first, 1))
			requireReadCapacity(t, h, second, emptyWaitPath(second, 1))
			expireSSEWrite(t, w, done)
			awaitReadState(t, h, func() bool {
				return owner.appStreams == 0 && recreated.appStreams == 0 && h.appStreamCountLocked() == 1
			})
			replacement := holdPhoneRead(t, h, first, "/app/stream")
			awaitReadState(t, h, func() bool { return recreated.appStreams == 1 && h.appStreamCountLocked() == 2 })
			cancel()
			requireReadCapacity(t, h, first, "/app/stream")
			replacement.cancel()
			awaitPhoneRead(t, replacement)
			awaitReadState(t, h, func() bool { return h.appStreamCountLocked() == 1 })
			other.cancel()
			awaitPhoneRead(t, other)
			awaitReadState(t, h, func() bool { return h.eventWaiters == 0 && h.appStreamCountLocked() == 0 })
		})
	}
}

func TestSSEAdmissionShutdownWaitsForBlockedHandlerDeadline(t *testing.T) {
	for _, stage := range []string{"write", "flush"} {
		t.Run(stage, func(t *testing.T) {
			h := newDeviceHub(t, Config{ResourceMode: "economy"})
			p := concurrentPhones(t, h, 1)[0]
			owner := h.accounts[deviceAccount(p.id)]
			w, _, done := holdBlockedSSE(t, h, p, stage == "flush")
			closed := make(chan struct{})
			go func() {
				h.Close()
				close(closed)
			}()
			awaitSSESignal(t, h.done, "Hub did not begin shutdown")
			select {
			case <-closed:
				t.Fatal("Hub shutdown skipped an admitted blocked handler")
			default:
			}
			if result := concurrentRequest(t, h, p, "/app/stream", false, nil); result.Code != http.StatusServiceUnavailable {
				t.Fatalf("shutdown accepted a new stream: %d", result.Code)
			}
			awaitReadState(t, h, func() bool { return h.appStreamCountLocked() == 1 })
			expireSSEWrite(t, w, done)
			awaitSSESignal(t, closed, "Hub did not finish shutdown after deadline")
			h.Close()
			awaitReadState(t, h, func() bool { return h.eventWaiters == 0 && owner.appStreams == 0 && h.appStreamCountLocked() == 0 })
		})
	}
}

func TestSSEAdmissionConcurrentMixedReadsNeverOverbook(t *testing.T) {
	h := newDeviceHub(t, Config{ResourceMode: "economy"})
	phones := concurrentPhones(t, h, 12)
	var reads []heldPhoneRead
	// Admit both transports before the concurrent burst so the assertion tests
	// a genuinely mixed budget regardless of goroutine scheduling order.
	reads = append(reads, holdPhoneRead(t, h, phones[0], "/app/stream"))
	reads = append(reads, holdPhoneRead(t, h, phones[1], emptyWaitPath(phones[1], 25)))
	awaitReadState(t, h, func() bool { return h.appStreams == 1 && h.eventWaiters == 1 })
	for _, p := range phones {
		for i := range 8 {
			path := "/app/stream"
			if i%2 == 1 {
				path = emptyWaitPath(p, 25)
			}
			reads = append(reads, holdPhoneRead(t, h, p, path))
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
	if got := h.appStreams + h.eventWaiters; got != h.resource.MaxGlobalAppStreams {
		t.Errorf("mixed global admission = %d, want %d", got, h.resource.MaxGlobalAppStreams)
	}
	for _, a := range h.accounts {
		if got := a.appStreams + a.eventWaiters; got > h.resource.MaxAppStreams {
			t.Errorf("mixed owner overbooked: %d > %d", got, h.resource.MaxAppStreams)
		}
	}
	h.mu.Unlock()
	rejected := 0
	for _, read := range reads {
		select {
		case result := <-read.done:
			if result.Code != http.StatusTooManyRequests {
				t.Errorf("unadmitted mixed request status = %d", result.Code)
			}
			rejected++
		default:
			read.cancel()
			awaitPhoneRead(t, read)
		}
	}
	if rejected != wantRejected {
		t.Errorf("mixed rejected = %d, want %d", rejected, wantRejected)
	}
	awaitReadState(t, h, func() bool { return h.appStreams == 0 && h.eventWaiters == 0 })
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range h.accounts {
		if a.appStreams != 0 || a.eventWaiters != 0 {
			t.Errorf("mixed cancellation leaked owner counters: SSE=%d wait=%d", a.appStreams, a.eventWaiters)
		}
	}
}

func TestSSEAdmissionModeChangesPreserveDroppedHandlers(t *testing.T) {
	h := newDeviceHub(t, Config{ResourceMode: "performance"})
	p := concurrentPhones(t, h, 1)[0]
	owner := h.accounts[deviceAccount(p.id)]
	h.mu.Lock()
	h.resource.AppQueueBytes = 1
	h.mu.Unlock()
	var writers []*blockedSSEWriter
	var finished []<-chan struct{}
	for i := range 5 {
		w, _, done := holdBlockedSSE(t, h, p, i%2 == 1)
		writers = append(writers, w)
		finished = append(finished, done)
	}
	h.mu.Lock()
	owner.sendLocked(sseMsg{deviceID: p.id, event: "event", data: []byte("{}")})
	h.mu.Unlock()
	awaitReadState(t, h, func() bool { return len(owner.apps) == 0 && owner.appStreams == 5 && h.appStreams == 5 })
	if err := h.ApplyResourceMode("economy", nil); err != nil {
		t.Fatal(err)
	}
	requireReadCapacity(t, h, p, "/app/stream")
	requireReadCapacity(t, h, p, emptyWaitPath(p, 1))
	for i := range 2 {
		expireSSEWrite(t, writers[i], finished[i])
	}
	awaitReadState(t, h, func() bool { return owner.appStreams == 3 && h.appStreams == 3 })
	replacement := holdPhoneRead(t, h, p, emptyWaitPath(p, 25))
	awaitReadState(t, h, func() bool { return owner.eventWaiters == 1 && h.eventWaiters == 1 })
	requireReadCapacity(t, h, p, "/app/stream")
	if err := h.ApplyResourceMode("balanced", nil); err != nil {
		t.Fatal(err)
	}
	extra := holdPhoneRead(t, h, p, "/app/stream")
	awaitReadState(t, h, func() bool { return owner.appStreams == 4 && h.appStreams == 4 })
	for i := 2; i < len(writers); i++ {
		expireSSEWrite(t, writers[i], finished[i])
	}
	for _, read := range []heldPhoneRead{replacement, extra} {
		read.cancel()
		awaitPhoneRead(t, read)
	}
	awaitReadState(t, h, func() bool {
		return owner.appStreams == 0 && h.appStreams == 0 && owner.eventWaiters == 0 && h.eventWaiters == 0
	})
}
