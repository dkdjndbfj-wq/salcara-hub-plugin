package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func pendingPairClaim(t *testing.T, h *Hub, mode, id, secret string) func() *httptest.ResponseRecorder {
	t.Helper()
	w := deviceRequest(t, h, "/bridge/pair/start", id, secret, "", map[string]string{"deviceId": id}, "")
	if w.Code != http.StatusOK {
		t.Fatalf("start pair: %d %s", w.Code, w.Body)
	}
	var attempt struct {
		Code   string `json:"code"`
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &attempt); err != nil {
		t.Fatal(err)
	}
	return func() *httptest.ResponseRecorder {
		if mode == "qr" {
			return deviceRequest(t, h, "/app/pair/qr", "", "", "", map[string]string{"deviceId": id, "ticket": attempt.Ticket}, "")
		}
		// Enter the legacy handler after account authentication without calling
		// an external station API or depending on a real account credential.
		body, _ := json.Marshal(map[string]string{"code": attempt.Code})
		r := httptest.NewRequest(http.MethodPost, "/app/pair/confirm", bytes.NewReader(body))
		out := httptest.NewRecorder()
		h.handlePairConfirm(out, r, deviceAccount(id))
		return out
	}
}

func pairClaimToken(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var result struct {
		Token string `json:"pair_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result.Token
}

func TestPairIssuanceWaitsForPersistenceBeforeReturningToken(t *testing.T) {
	for _, mode := range []string{"qr", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			h := newDeviceHub(t, Config{DataDir: dir})
			id, secret := "durable-pair", strings.Repeat("a", 64)
			enrollDevice(t, h, id, secret)
			onlineDevice(t, h, id)
			claim := pendingPairClaim(t, h, mode, id, secret)
			if err := h.saveNowError(); err != nil {
				t.Fatal(err)
			}
			h.mu.Lock()
			wake := h.accounts[deviceAccount(id)].wakeChanLocked()
			h.mu.Unlock()
			h.saveMu.Lock()
			unblock := sync.OnceFunc(h.saveMu.Unlock)
			defer unblock()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- claim() }()
			awaitSSESignal(t, wake, "pair claim did not reach persistence")
			status := deviceRequest(t, h, "/bridge/pair/status", id, secret, "", nil, "")
			if status.Code != http.StatusServiceUnavailable || !strings.Contains(status.Body.String(), "pair_persistence_pending") || strings.Contains(status.Body.String(), `"paired":`) {
				t.Fatalf("pending pair was published to the desktop: %d %s", status.Code, status.Body)
			}
			if h.Stats().PairedDevices != 0 {
				t.Fatal("pending pair was counted as completed")
			}
			select {
			case w := <-done:
				t.Fatalf("pair token returned before persistence: status=%d tokenPresent=%t", w.Code, pairClaimToken(t, w) != "")
			case <-time.After(40 * time.Millisecond):
			}
			unblock()
			var response *httptest.ResponseRecorder
			select {
			case response = <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("pair response blocked after storage resumed")
			}
			if response.Code != http.StatusOK {
				t.Fatalf("durable pair failed: %d %s", response.Code, response.Body)
			}
			token := pairClaimToken(t, response)
			if len(token) != 64 {
				t.Fatal("successful pair omitted its credential")
			}
			// Restart from the exact acknowledged snapshot, not from a later
			// orderly shutdown that could conceal an asynchronous save gap.
			raw, err := os.ReadFile(filepath.Join(dir, devicesFile))
			if err != nil {
				t.Fatal(err)
			}
			restartDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(restartDir, devicesFile), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			restarted := newDeviceHub(t, Config{DataDir: restartDir})
			if w := deviceRequest(t, restarted, "/app/devices", "", "", token, nil, ""); w.Code != http.StatusOK {
				t.Fatalf("acknowledged pair disappeared after restart: %d", w.Code)
			}
		})
	}
}

func TestPairIssuancePersistenceFailureDoesNotReturnToken(t *testing.T) {
	for _, mode := range []string{"qr", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			h := newDeviceHub(t, Config{DataDir: dir})
			id, secret := "failed-pair", strings.Repeat("b", 64)
			enrollDevice(t, h, id, secret)
			onlineDevice(t, h, id)
			claim := pendingPairClaim(t, h, mode, id, secret)
			if err := h.saveNowError(); err != nil {
				t.Fatal(err)
			}
			h.saveMu.Lock()
			err := os.Remove(filepath.Join(dir, devicesFile))
			if err == nil {
				err = os.Mkdir(filepath.Join(dir, devicesFile), 0o700)
			}
			h.saveMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			w := claim()
			if w.Code != http.StatusServiceUnavailable || pairClaimToken(t, w) != "" || !strings.Contains(w.Body.String(), "pair_persistence_failed") {
				t.Fatalf("failed save reported a successful pairing: status=%d tokenPresent=%t", w.Code, pairClaimToken(t, w) != "")
			}
			h.mu.Lock()
			paired := h.accounts[deviceAccount(id)].devices[id].hasPair
			h.mu.Unlock()
			if paired {
				t.Fatal("failed issuance left an undisclosed credential as a live pairing")
			}
		})
	}
}

func TestPairIssuanceFailureCannotRevokePhoneAtAnotherStation(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "new-station"
		if existing {
			name = "same-phone-refresh"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			h := newDeviceHub(t, Config{DataDir: dir})
			id, secret := "shared-phone", strings.Repeat("a", 64)
			binding, phone := strings.Repeat("b", 64), strings.Repeat("c", 64)
			phoneHash := shaHex(phone)
			enrollDevice(t, h, id, secret)
			onlineDevice(t, h, id)
			if existing {
				modernPair(t, h, id, secret, binding, phoneHash, phone)
			}
			start := deviceRequest(t, h, "/bridge/pair/start", id, secret, "", map[string]string{
				"deviceId": id, "bindingId": binding, "phoneHash": phoneHash, "computerId": "physical-fixture",
			}, "")
			var attempt struct {
				Ticket string `json:"ticket"`
			}
			if start.Code != http.StatusOK || json.Unmarshal(start.Body.Bytes(), &attempt) != nil {
				t.Fatalf("start modern pairing: %d", start.Code)
			}
			if err := h.saveNowError(); err != nil {
				t.Fatal(err)
			}
			h.saveMu.Lock()
			err := os.Remove(filepath.Join(dir, devicesFile))
			if err == nil {
				err = os.Mkdir(filepath.Join(dir, devicesFile), 0o700)
			}
			h.saveMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			claim := deviceRequest(t, h, "/app/pair/qr", "", "", "", map[string]string{
				"deviceId": id, "ticket": attempt.Ticket, "phoneId": phone,
			}, "")
			if claim.Code != http.StatusServiceUnavailable || pairClaimToken(t, claim) != "" {
				t.Fatalf("failed modern claim returned success: status=%d", claim.Code)
			}
			assertNotGlobalRevoke := func(current *Hub) {
				t.Helper()
				status := deviceRequest(t, current, "/bridge/pair/status", id, secret, "", nil, "")
				var state struct {
					Paired    bool   `json:"paired"`
					BindingID string `json:"bindingId"`
					PhoneHash string `json:"phoneHash"`
				}
				if status.Code != http.StatusOK || json.Unmarshal(status.Body.Bytes(), &state) != nil || state.Paired || state.BindingID != "" || state.PhoneHash != "" {
					t.Fatalf("failed issuance published a global phone revocation: status=%d", status.Code)
				}
			}
			assertNotGlobalRevoke(h)
			conditional := func() *httptest.ResponseRecorder {
				return deviceRequest(t, h, "/bridge/pair/revoke", id, secret, "", map[string]string{
					"deviceId": id, "bindingId": binding, "phoneHash": phoneHash,
				}, "")
			}
			if w := conditional(); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "pair_revoke_persistence_failed") {
				t.Fatalf("conditional cleanup completed before its disk fence: %d", w.Code)
			}
			h.saveMu.Lock()
			err = os.Remove(filepath.Join(dir, devicesFile))
			h.saveMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if err := h.saveNowError(); err != nil {
				t.Fatal(err)
			}
			if w := conditional(); w.Code != http.StatusConflict {
				t.Fatalf("conditional cleanup did not finish after the removal was saved: %d", w.Code)
			}
			restarted := newDeviceHub(t, Config{DataDir: dir})
			assertNotGlobalRevoke(restarted)
		})
	}
}

func awaitPairClaim(t *testing.T, done <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case w := <-done:
		return w
	case <-time.After(3 * time.Second):
		t.Fatal("pair operation did not finish")
		return nil
	}
}

func TestPairIssuanceCannotPublishAfterNewPairOrRevocation(t *testing.T) {
	for _, mode := range []string{"qr", "legacy"} {
		for _, change := range []string{"revoke", "new-qr", "replacement"} {
			t.Run(mode+"/"+change, func(t *testing.T) {
				h := newDeviceHub(t, Config{DataDir: t.TempDir()})
				id, secret := "changed-pair", strings.Repeat("c", 64)
				oldToken := pairPhone(t, h, id, secret)
				claim := pendingPairClaim(t, h, mode, id, secret)
				h.mu.Lock()
				wake := h.accounts[deviceAccount(id)].wakeChanLocked()
				h.mu.Unlock()
				h.saveMu.Lock()
				unblock := sync.OnceFunc(h.saveMu.Unlock)
				defer unblock()
				first := make(chan *httptest.ResponseRecorder, 1)
				go func() { first <- claim() }()
				awaitSSESignal(t, wake, "first claim did not reach the storage boundary")
				second := make(chan *httptest.ResponseRecorder, 1)
				var nextClaim func() *httptest.ResponseRecorder
				if change != "revoke" {
					nextClaim = pendingPairClaim(t, h, mode, id, secret)
				}
				if change != "new-qr" {
					h.mu.Lock()
					wake = h.accounts[deviceAccount(id)].wakeChanLocked()
					h.mu.Unlock()
					go func() {
						if change == "revoke" {
							second <- pairRevocationRequest(t, h, "/bridge/pair/revoke", id, secret, "")
						} else {
							second <- nextClaim()
						}
					}()
					awaitSSESignal(t, wake, "new transition did not replace the old issuance")
				}
				unblock()
				stale := awaitPairClaim(t, first)
				if stale.Code != http.StatusConflict || pairClaimToken(t, stale) != "" || !strings.Contains(stale.Body.String(), "pair_state_changed") {
					t.Fatalf("superseded claim returned a usable credential: status=%d tokenPresent=%t", stale.Code, pairClaimToken(t, stale) != "")
				}
				var latest *httptest.ResponseRecorder
				if change == "new-qr" {
					// Abandoning the stale claim must preserve the newer unused QR.
					latest = nextClaim()
				} else {
					latest = awaitPairClaim(t, second)
				}
				if latest.Code != http.StatusOK {
					t.Fatalf("stale claim damaged the later operation: %d %s", latest.Code, latest.Body)
				}
				if w := deviceRequest(t, h, "/app/devices", "", "", oldToken, nil, ""); w.Code != http.StatusForbidden {
					t.Fatalf("old credential was restored or left pending: %d", w.Code)
				}
				if change != "revoke" {
					if w := deviceRequest(t, h, "/app/devices", "", "", pairClaimToken(t, latest), nil, ""); w.Code != http.StatusOK {
						t.Fatalf("new credential did not remain usable: %d", w.Code)
					}
				}
			})
		}
	}
}

type pairPublicationWriter struct {
	*httptest.ResponseRecorder
	published chan struct{}
	once      sync.Once
}

func (w *pairPublicationWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	if bytes.Contains(p, []byte("event: pair.status")) {
		w.once.Do(func() { close(w.published) })
	}
	return n, err
}

func TestPairIssuancePublishesBridgeStateOnlyAfterStorage(t *testing.T) {
	h := newDeviceHub(t, Config{DataDir: t.TempDir()})
	id, secret := "pair-publication", strings.Repeat("d", 64)
	enrollDevice(t, h, id, secret)
	onlineDevice(t, h, id)
	claim := pendingPairClaim(t, h, "qr", id, secret)
	if err := h.saveNowError(); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	wake := h.accounts[deviceAccount(id)].wakeChanLocked()
	h.mu.Unlock()
	h.saveMu.Lock()
	unblock := sync.OnceFunc(h.saveMu.Unlock)
	defer unblock()
	claimDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { claimDone <- claim() }()
	awaitSSESignal(t, wake, "claim did not start")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &pairPublicationWriter{ResponseRecorder: httptest.NewRecorder(), published: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(w, bridgeReadRequest(concurrentPhone{id: id, secret: secret, ip: "192.0.2.2"}).WithContext(ctx))
	}()
	t.Cleanup(func() { cancel(); awaitSSESignal(t, done, "Bridge stream did not drain") })
	awaitReadState(t, h, func() bool { return h.bridgeStreams == 1 })
	select {
	case <-w.published:
		t.Fatal("pending claim published an authoritative Bridge pairing")
	case <-time.After(40 * time.Millisecond):
	}
	unblock()
	if response := awaitPairClaim(t, claimDone); response.Code != http.StatusOK {
		t.Fatalf("pair failed after disk resumed: %d", response.Code)
	}
	awaitSSESignal(t, w.published, "durable pair was not published")
	cancel()
	awaitSSESignal(t, done, "Bridge stream did not finish")
	if !strings.Contains(w.Body.String(), `"paired":true`) || strings.Contains(w.Body.String(), `"paired":false`) {
		t.Fatal("Bridge received a false revoke while issuance was pending")
	}
}
