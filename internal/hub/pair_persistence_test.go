package hub

import (
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

func pairRevocationRequest(t *testing.T, h *Hub, route, id, secret, token string) *httptest.ResponseRecorder {
	t.Helper()
	if route == "/app/pair/revoke" {
		return deviceRequest(t, h, route, "", "", token, map[string]string{}, "")
	}
	return deviceRequest(t, h, route, id, secret, "", map[string]string{"deviceId": id}, "")
}

func TestPairRevocationWaitsForPersistenceBeforeSuccess(t *testing.T) {
	for _, route := range []string{"/app/pair/revoke", "/bridge/pair/revoke"} {
		t.Run(strings.ReplaceAll(route, "/", "_"), func(t *testing.T) {
			dir := t.TempDir()
			h := newDeviceHub(t, Config{DataDir: dir})
			id, secret := "durable-revoke", strings.Repeat("a", 64)
			enrollDevice(t, h, id, secret)
			onlineDevice(t, h, id)
			token := claimQR(t, h, id, issueQR(t, h, id, secret))
			if err := h.saveNowError(); err != nil {
				t.Fatal(err)
			}
			h.mu.Lock()
			wake := h.accounts[deviceAccount(id)].wakeChanLocked()
			h.mu.Unlock()
			// Model a slow disk while keeping the authorization lock available.
			h.saveMu.Lock()
			unblock := sync.OnceFunc(h.saveMu.Unlock)
			defer unblock()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- pairRevocationRequest(t, h, route, id, secret, token) }()
			awaitSSESignal(t, wake, "revocation did not wake the existing phone read")
			if w := deviceRequest(t, h, "/app/devices", "", "", token, nil, ""); w.Code != http.StatusServiceUnavailable {
				t.Fatalf("unpersisted revocation appeared final or remained live: %d", w.Code)
			}
			select {
			case w := <-done:
				t.Fatalf("revocation acknowledged before persistence: %d", w.Code)
			case <-time.After(40 * time.Millisecond):
			}
			unblock()
			select {
			case w := <-done:
				if w.Code != http.StatusOK {
					t.Fatalf("durable revocation failed: %d %s", w.Code, w.Body)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("revocation blocked after storage resumed")
			}
			// Load precisely the state present when success was acknowledged,
			// without allowing the first Hub's orderly shutdown to hide a gap.
			raw, err := os.ReadFile(filepath.Join(dir, devicesFile))
			if err != nil {
				t.Fatal(err)
			}
			restartDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(restartDir, devicesFile), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			restarted := newDeviceHub(t, Config{DataDir: restartDir})
			if w := deviceRequest(t, restarted, "/app/devices", "", "", token, nil, ""); w.Code != http.StatusForbidden {
				t.Fatalf("acknowledged revocation resurrected after restart: %d", w.Code)
			}
		})
	}
}

func TestPairRevocationPersistenceFailureDoesNotRestoreLiveAccess(t *testing.T) {
	for _, route := range []string{"/app/pair/revoke", "/bridge/pair/revoke"} {
		t.Run(strings.ReplaceAll(route, "/", "_"), func(t *testing.T) {
			dir := t.TempDir()
			h := newDeviceHub(t, Config{DataDir: dir})
			id, secret := "failed-revoke", strings.Repeat("b", 64)
			enrollDevice(t, h, id, secret)
			onlineDevice(t, h, id)
			token := claimQR(t, h, id, issueQR(t, h, id, secret))
			if err := h.saveNowError(); err != nil {
				t.Fatal(err)
			}
			sub := &appSub{deviceID: id, closed: make(chan struct{})}
			h.mu.Lock()
			h.accounts[deviceAccount(id)].apps[sub] = struct{}{}
			h.mu.Unlock()
			// A directory at the snapshot target reliably fails the atomic rename
			// on Windows and Unix, without relying on permission-bit behavior.
			h.saveMu.Lock()
			err := os.Remove(filepath.Join(dir, devicesFile))
			if err == nil {
				err = os.Mkdir(filepath.Join(dir, devicesFile), 0o700)
			}
			h.saveMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			w := pairRevocationRequest(t, h, route, id, secret, token)
			var response struct {
				Code string `json:"code"`
			}
			if w.Code != http.StatusServiceUnavailable || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Code != "pair_revoke_persistence_failed" {
				t.Fatalf("storage failure reported success: %d %s", w.Code, w.Body)
			}
			select {
			case <-sub.closed:
			default:
				t.Fatal("old phone stream stayed open after failed persistence")
			}
			if w := deviceRequest(t, h, "/app/devices", "", "", token, nil, ""); w.Code != http.StatusServiceUnavailable {
				t.Fatalf("storage failure restored revoked token or appeared final: %d", w.Code)
			}
		})
	}
}

func TestPairRevocationRetryStaysPendingUntilStorageRecovers(t *testing.T) {
	dir := t.TempDir()
	h := newDeviceHub(t, Config{DataDir: dir})
	id, secret := "retry-revoke", strings.Repeat("c", 64)
	token := pairPhone(t, h, id, secret)
	otherID, otherSecret := "still-paired", strings.Repeat("d", 64)
	otherToken := pairPhone(t, h, otherID, otherSecret)
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
	assertPending := func(w *httptest.ResponseRecorder) {
		t.Helper()
		var response struct {
			Code string `json:"code"`
		}
		if w.Code != http.StatusServiceUnavailable || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Code != "pair_revoke_persistence_failed" {
			t.Fatalf("unfinished revoke became a terminal authentication response: %d %s", w.Code, w.Body)
		}
	}
	assertPending(pairRevocationRequest(t, h, "/app/pair/revoke", id, secret, token))
	for range 3 {
		assertPending(pairRevocationRequest(t, h, "/app/pair/revoke", id, secret, token))
	}
	// Older embedding integrations may enter the paired handler directly after
	// legacy account authentication. They need the same nonterminal outcome.
	legacy := httptest.NewRequest(http.MethodPost, "/app/pair/revoke", nil)
	legacy.Header.Set("X-Salcara-Pair-Token", token)
	w := httptest.NewRecorder()
	h.handleAppPairRevoke(w, legacy, deviceAccount(id))
	assertPending(w)
	if w := deviceRequest(t, h, "/app/devices", "", "", otherToken, nil, ""); w.Code != http.StatusOK {
		t.Fatalf("pending revocation blocked another valid phone: %d", w.Code)
	}
	if w := deviceRequest(t, h, "/bridge/pair/status", otherID, otherSecret, "", nil, ""); w.Code != http.StatusOK {
		t.Fatalf("pending revocation blocked valid device credentials: %d", w.Code)
	}
	h.saveMu.Lock()
	err = os.Remove(filepath.Join(dir, devicesFile)) // remove only the synthetic empty directory
	h.saveMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.saveNowError(); err != nil {
		t.Fatal(err)
	}
	if w := pairRevocationRequest(t, h, "/app/pair/revoke", id, secret, token); w.Code != http.StatusForbidden {
		t.Fatalf("durably revoked token did not receive terminal denial: %d", w.Code)
	}
	w = httptest.NewRecorder()
	h.handleAppPairRevoke(w, legacy, deviceAccount(id))
	if w.Code != http.StatusForbidden {
		t.Fatalf("legacy revoke stayed pending after storage recovered: %d", w.Code)
	}
}

func TestPairRevocationOlderSnapshotCannotAcknowledgeNewRevocation(t *testing.T) {
	h := newDeviceHub(t, Config{DataDir: t.TempDir()})
	idA, idB := "epoch-a", "epoch-b"
	pairPhone(t, h, idA, strings.Repeat("e", 64))
	tokenB := pairPhone(t, h, idB, strings.Repeat("f", 64))
	if err := h.saveNowError(); err != nil {
		t.Fatal(err)
	}
	// Hold the writer lock while modelling an existing save's snapshot and a
	// concurrent revoke that arrives after capture but before disk commit.
	h.saveMu.Lock()
	defer h.saveMu.Unlock()
	h.mu.Lock()
	h.revokePairLocked(deviceAccount(idA), h.accounts[deviceAccount(idA)].devices[idA])
	h.mu.Unlock()
	older := h.snapshot()
	h.mu.Lock()
	h.revokePairLocked(deviceAccount(idB), h.accounts[deviceAccount(idB)].devices[idB])
	h.mu.Unlock()
	if err := h.saveSnapshotError(older); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	pending := h.pairRevocationEpoch > h.savedPairRevocationEpoch
	h.mu.Unlock()
	if !pending {
		t.Fatal("older snapshot cleared a newer unsaved revocation")
	}
	if w := deviceRequest(t, h, "/app/devices", "", "", tokenB, nil, ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("newer revoked token appeared durable after an older save: %d", w.Code)
	}
	if err := h.saveSnapshotError(h.snapshot()); err != nil {
		t.Fatal(err)
	}
	if w := deviceRequest(t, h, "/app/devices", "", "", tokenB, nil, ""); w.Code != http.StatusForbidden {
		t.Fatalf("current snapshot did not acknowledge the revoke: %d", w.Code)
	}
}
