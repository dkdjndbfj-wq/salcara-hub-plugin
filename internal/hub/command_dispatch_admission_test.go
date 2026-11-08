package hub

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCommandDispatchRechecksAdmissionAfterSourceCheck(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		status int
	}{
		{"cancelled-request", 499},
		{"revoked-pair", 403},
		{"replaced-connection", 409},
		{"closed-connection", 409},
		{"hub-shutdown", 503},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			h, conn := receiptFixture(t, 100*time.Millisecond)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			paired := true // all reads and writes are under h.mu
			result := make(chan int, 1)
			go func() {
				_, status := h.executeCommandGuarded(ctx, "receipt-owner", "pc", json.RawMessage(`{"type":"session.send","text":"admission fixture"}`), func(*device) bool { return paired }, "", &h.remoteAccessGate, func() (string, *ExecutionSource) {
					close(entered)
					<-release
					return "", nil
				})
				result <- status
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("command did not reach source check")
			}
			switch scenario.name {
			case "cancelled-request":
				cancel()
			case "revoked-pair":
				h.mu.Lock()
				paired = false
				h.mu.Unlock()
			case "replaced-connection":
				h.mu.Lock()
				h.accounts["receipt-owner"].devices["pc"].conn = &bridgeConn{cmds: make(chan []byte, 1), closed: make(chan struct{})}
				h.mu.Unlock()
			case "closed-connection":
				conn.close()
			case "hub-shutdown":
				h.Close()
			}
			unblock()
			select {
			case status := <-result:
				if status != scenario.status {
					t.Errorf("status = %d, want %d", status, scenario.status)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("command did not finish")
			}
			if len(conn.cmds) != 0 {
				t.Fatal("stale command reached the old connection after admission changed")
			}
			h.mu.Lock()
			pending := len(h.pending)
			h.mu.Unlock()
			if pending != 0 {
				t.Fatal("rejected command retained pending capacity")
			}
		})
	}
}

func TestAcceptedReceiptSurvivesPhoneCancellationBeforeEnqueue(t *testing.T) {
	h, conn := receiptFixture(t, 2*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var checks atomic.Int32
	command := json.RawMessage(`{"type":"session.send","text":"durably accepted fixture"}`)
	first := make(chan int, 1)
	go func() {
		_, status, _ := h.executeReceiptWithPolicy(ctx, "receipt-owner", "pc", "pair-a", receiptRequestID, command, nil, func() (string, *ExecutionSource) {
			if checks.Add(1) == 2 {
				close(entered)
				<-release
			}
			return "", nil
		})
		first <- status
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("receipt worker did not reach final source check")
	}
	cancel()
	select {
	case status := <-first:
		if status != 499 {
			t.Fatalf("disconnected phone status = %d", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("disconnected phone remained blocked")
	}
	unblock()
	replyReceipt(t, h, conn, `{"accepted":true}`)
	rep, status, code := h.executeReceipt(context.Background(), "receipt-owner", "pc", "pair-a", receiptRequestID, command, nil)
	if status != 200 || !rep.OK || code != "" || len(conn.cmds) != 0 {
		t.Fatalf("accepted receipt lost or repeated: status=%d code=%s", status, code)
	}
}

func TestStandbyDispatchRechecksAvailabilityAfterSourceCheck(t *testing.T) {
	for _, change := range []string{"expired", "replaced"} {
		t.Run(change, func(t *testing.T) {
			h, id, secret, _ := standbyFixture(t)
			if w := standbyPoll(t, h, id, secret); w.Code != 200 {
				t.Fatal("standby setup failed")
			}
			h.mu.Lock()
			dev := h.accounts[deviceAccount(id)].devices[id]
			conn := dev.standby
			h.mu.Unlock()
			command := json.RawMessage(`{"type":"remote.station.switch","targetHubUrl":"http://example.test/salcara-hub","targetDeviceId":"standby-pc","targetComputerId":"physical-pc","operationId":"10000000-0000-4000-8000-000000000001"}`)
			_, status := h.executeCommandGuarded(context.Background(), deviceAccount(id), id, command, func(d *device) bool { return d.hasPair }, "", &h.remoteAccessGate, func() (string, *ExecutionSource) {
				h.mu.Lock()
				defer h.mu.Unlock()
				if change == "expired" {
					dev.standbyAt = nowMs() - standbyTTL.Milliseconds() - 1
				} else {
					dev.standby = &bridgeConn{cmds: make(chan []byte, 1), closed: make(chan struct{})}
				}
				return "", nil
			})
			if status != 409 || len(conn.cmds) != 0 {
				t.Fatalf("stale standby received handover: status=%d commands=%d", status, len(conn.cmds))
			}
		})
	}
}
