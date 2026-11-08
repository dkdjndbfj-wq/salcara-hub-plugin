package hub

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func bridgeReadRequest(p concurrentPhone) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://example.test/salcara-hub/v1/bridge/stream?deviceId="+p.id, nil)
	r.RemoteAddr = p.ip + ":1234"
	r.Header.Set("X-Salcara-Device-Id", p.id)
	r.Header.Set("X-Salcara-Device-Secret", p.secret)
	return r
}

func requireBridgeReadCapacity(t *testing.T, h *Hub, p concurrentPhone) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, bridgeReadRequest(p).WithContext(ctx))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "2" {
		t.Fatalf("full active Bridge budget was bypassed: status=%d", w.Code)
	}
}

func TestBridgeStreamAdmissionReplacementCannotExceedActiveBudget(t *testing.T) {
	for _, stage := range []string{"write", "flush"} {
		t.Run(stage, func(t *testing.T) {
			h := newDeviceHub(t, Config{ResourceMode: "economy"})
			p := concurrentPhones(t, h, 1)[0]
			h.mu.Lock()
			h.resource.MaxBridgeStreams = 2
			dev := h.accounts[deviceAccount(p.id)].devices[p.id]
			h.mu.Unlock()
			first, cancelFirst, firstDone := holdBlockedSSERequest(t, h, bridgeReadRequest(p), stage == "flush")
			second, cancelSecond, secondDone := holdBlockedSSERequest(t, h, bridgeReadRequest(p), stage != "flush")
			// Both handlers are still blocked, although the registry keeps only
			// the second. A third replacement must not borrow either live slot.
			requireBridgeReadCapacity(t, h, p)
			awaitReadState(t, h, func() bool { return h.bridgeStreamCountLocked() == 2 })
			cancelFirst()
			cancelSecond()
			requireBridgeReadCapacity(t, h, p)
			expireSSEWrite(t, first, firstDone)
			awaitReadState(t, h, func() bool { return h.bridgeStreamCountLocked() == 1 })
			replacement := holdHTTPRead(t, h, bridgeReadRequest(p))
			awaitReadState(t, h, func() bool { return h.bridgeStreamCountLocked() == 2 })
			h.mu.Lock()
			newest := dev.conn
			h.mu.Unlock()
			expireSSEWrite(t, second, secondDone)
			awaitReadState(t, h, func() bool { return h.bridgeStreamCountLocked() == 1 && dev.conn == newest })
			cancelFirst()
			cancelSecond()
			replacement.cancel()
			awaitPhoneRead(t, replacement)
			awaitReadState(t, h, func() bool { return h.bridgeStreamCountLocked() == 0 && dev.conn == nil })
		})
	}
}

func TestBridgeStreamAdmissionLifecycleRetainsDrainingBudget(t *testing.T) {
	for _, stage := range []string{"write", "flush"} {
		for _, action := range []string{"disconnect", "account-replaced", "rebind"} {
			t.Run(stage+"/"+action, func(t *testing.T) {
				h := newDeviceHub(t, Config{ResourceMode: "economy"})
				phones := concurrentPhones(t, h, 2)
				first, second := phones[0], phones[1]
				h.mu.Lock()
				h.resource.MaxBridgeStreams = 1
				owner := h.accounts[deviceAccount(first.id)]
				dev := owner.devices[first.id]
				h.mu.Unlock()
				w, cancel, done := holdBlockedSSERequest(t, h, bridgeReadRequest(first), stage == "flush")
				h.mu.Lock()
				if action == "rebind" {
					h.revokePairLocked(owner.id, dev)
					h.setPairLocked(owner.id, dev, sha256.Sum256([]byte(strings.Repeat("e", 64))), true)
				} else {
					h.disconnectLocked(owner.id, dev)
					if action == "account-replaced" {
						h.removeDeviceLocked(owner.id, owner, first.id, dev)
						h.removeAccountLocked(owner.id, owner)
					}
				}
				h.mu.Unlock()
				if action == "account-replaced" {
					first = concurrentPhones(t, h, 1)[0]
				}
				cancel() // Request cancellation alone cannot finish a blocked write.
				requireBridgeReadCapacity(t, h, first)
				requireBridgeReadCapacity(t, h, second)
				awaitReadState(t, h, func() bool { return h.bridgeStreamCountLocked() == 1 })
				// Phone reads have a separate capacity budget and remain usable.
				phone := holdPhoneRead(t, h, second, emptyWaitPath(second, 25))
				awaitReadState(t, h, func() bool { return h.eventWaiters == 1 })
				if action == "disconnect" {
					w.released.Do(func() { close(w.release) })
					awaitSSESignal(t, done, "disconnected Bridge did not drain after write completed")
				} else {
					expireSSEWrite(t, w, done)
				}
				awaitReadState(t, h, func() bool { return h.bridgeStreamCountLocked() == 0 && h.eventWaiters == 1 })
				replacement := holdHTTPRead(t, h, bridgeReadRequest(first))
				awaitReadState(t, h, func() bool { return h.bridgeStreamCountLocked() == 1 })
				// Rejection for another owner must never close the admitted owner.
				requireBridgeReadCapacity(t, h, second)
				select {
				case <-replacement.done:
					t.Fatal("another owner's rejected request closed the active Bridge")
				default:
				}
				replacement.cancel()
				awaitPhoneRead(t, replacement)
				phone.cancel()
				awaitPhoneRead(t, phone)
				awaitReadState(t, h, func() bool { return h.bridgeStreamCountLocked() == 0 && h.eventWaiters == 0 })
			})
		}
	}
}

func TestBridgeStreamAdmissionFullReplacementDrainsThenReconnects(t *testing.T) {
	h := newDeviceHub(t, Config{ResourceMode: "economy"})
	p := concurrentPhones(t, h, 1)[0]
	h.mu.Lock()
	h.resource.MaxBridgeStreams = 1
	h.mu.Unlock()
	original := holdHTTPRead(t, h, bridgeReadRequest(p))
	awaitReadState(t, h, func() bool { return h.bridgeStreamCountLocked() == 1 })
	requireBridgeReadCapacity(t, h, p)
	// Full-capacity replacement asks only this device's old connection to
	// drain. The next retry can acquire the released slot, with no deadlock.
	if w := awaitPhoneRead(t, original); w.Code != http.StatusOK {
		t.Fatalf("original Bridge status = %d", w.Code)
	}
	awaitReadState(t, h, func() bool { return h.bridgeStreamCountLocked() == 0 })
	reconnected := holdHTTPRead(t, h, bridgeReadRequest(p))
	awaitReadState(t, h, func() bool { return h.bridgeStreamCountLocked() == 1 })
	reconnected.cancel()
	awaitPhoneRead(t, reconnected)
	awaitReadState(t, h, func() bool { return h.bridgeStreamCountLocked() == 0 })
}

func TestBridgeStreamAdmissionShutdownDrainsWriteDeadline(t *testing.T) {
	for _, stage := range []string{"write", "flush"} {
		t.Run(stage, func(t *testing.T) {
			h := newDeviceHub(t, Config{ResourceMode: "economy"})
			p := concurrentPhones(t, h, 1)[0]
			w, cancel, done := holdBlockedSSERequest(t, h, bridgeReadRequest(p), stage == "flush")
			closed := make(chan struct{})
			go func() {
				h.Close()
				close(closed)
			}()
			awaitSSESignal(t, h.done, "Hub did not begin shutdown")
			cancel()
			select {
			case <-closed:
				t.Fatal("Hub shutdown skipped an admitted blocked Bridge")
			default:
			}
			awaitReadState(t, h, func() bool { return h.bridgeStreamCountLocked() == 1 })
			expireSSEWrite(t, w, done)
			awaitSSESignal(t, closed, "Hub did not finish shutdown after Bridge deadline")
			h.Close()
			awaitReadState(t, h, func() bool { return h.bridgeStreamCountLocked() == 0 })
		})
	}
}

func TestBridgeStreamAdmissionModeChangesPreserveDrainingHandlers(t *testing.T) {
	h := newDeviceHub(t, Config{ResourceMode: "performance"})
	phones := concurrentPhones(t, h, 65)
	var writers []*blockedSSEWriter
	var finished []<-chan struct{}
	for i, p := range phones {
		w, _, done := holdBlockedSSERequest(t, h, bridgeReadRequest(p), i%2 == 1)
		writers = append(writers, w)
		finished = append(finished, done)
	}
	h.mu.Lock()
	for _, p := range phones {
		owner := h.accounts[deviceAccount(p.id)]
		h.disconnectLocked(owner.id, owner.devices[p.id])
	}
	h.mu.Unlock()
	if err := h.ApplyResourceMode("economy", nil); err != nil {
		t.Fatal(err)
	}
	requireBridgeReadCapacity(t, h, phones[0])
	awaitReadState(t, h, func() bool { return h.bridgeStreamCountLocked() == 65 })
	for i := range 2 {
		expireSSEWrite(t, writers[i], finished[i])
	}
	awaitReadState(t, h, func() bool { return h.bridgeStreamCountLocked() == 63 })
	replacement := holdHTTPRead(t, h, bridgeReadRequest(phones[0]))
	awaitReadState(t, h, func() bool { return h.bridgeStreamCountLocked() == 64 })
	requireBridgeReadCapacity(t, h, phones[1])
	if err := h.ApplyResourceMode("balanced", nil); err != nil {
		t.Fatal(err)
	}
	extra := holdHTTPRead(t, h, bridgeReadRequest(phones[1]))
	awaitReadState(t, h, func() bool { return h.bridgeStreamCountLocked() == 65 })
	for i := 2; i < len(writers); i++ {
		expireSSEWrite(t, writers[i], finished[i])
	}
	for _, read := range []heldPhoneRead{replacement, extra} {
		read.cancel()
		awaitPhoneRead(t, read)
	}
	awaitReadState(t, h, func() bool { return h.bridgeStreamCountLocked() == 0 })
}

func TestBridgeStreamAdmissionConcurrentReplacementBurst(t *testing.T) {
	h := newDeviceHub(t, Config{ResourceMode: "economy"})
	p := concurrentPhones(t, h, 1)[0]
	h.mu.Lock()
	h.resource.MaxBridgeStreams = 8
	h.mu.Unlock()
	var writers []*blockedSSEWriter
	var finished []<-chan struct{}
	for i := range 32 {
		w, _, done := startBlockedSSERequest(t, h, bridgeReadRequest(p), i%2 == 1)
		writers = append(writers, w)
		finished = append(finished, done)
	}
	awaitReadState(t, h, func() bool {
		known := 0
		for i, w := range writers {
			select {
			case <-finished[i]:
				known++
			case <-w.blocked:
				known++
			default:
			}
		}
		return known == len(writers)
	})
	awaitReadState(t, h, func() bool { return h.bridgeStreamCountLocked() == 8 })
	admitted, rejected := 0, 0
	for i, w := range writers {
		select {
		case <-finished[i]:
			if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "2" {
				t.Errorf("concurrent unadmitted Bridge status = %d", w.Code)
			}
			rejected++
		default:
			expireSSEWrite(t, w, finished[i])
			admitted++
		}
	}
	if admitted != 8 || rejected != 24 {
		t.Fatalf("concurrent replacements admitted=%d rejected=%d, want 8/24", admitted, rejected)
	}
	awaitReadState(t, h, func() bool { return h.bridgeStreamCountLocked() == 0 })
}
