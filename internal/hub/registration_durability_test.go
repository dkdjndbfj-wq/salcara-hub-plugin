package hub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistrationStorageFailureCannotBecomeSuccessOnRetryOrRestart(t *testing.T) {
	for _, mode := range []string{"claim", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			h := newDeviceHub(t, Config{DataDir: t.TempDir(), PublicURL: "https://hub.example.test/salcara-hub"})
			id, secret := "durability-pc", strings.Repeat("c", 64)
			accountID := deviceAccount(id)
			if mode == "legacy" {
				accountID = "legacy-durable-account"
			}
			register := func(h *Hub, first bool) *httptest.ResponseRecorder {
				if mode == "claim" {
					if first {
						return deviceRequest(t, h, "/device/register", id, secret, "", Device{DeviceID: id}, "")
					}
					return deviceRequestWithoutEnrollmentClaim(t, h, "/device/register", id, secret, Device{DeviceID: id})
				}
				raw, _ := json.Marshal(Device{DeviceID: id})
				r := httptest.NewRequest(http.MethodPost, "/bridge/register", bytes.NewReader(raw))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("X-Salcara-Device-Secret", secret)
				w := httptest.NewRecorder()
				h.handleRegister(w, r, accountID)
				return w
			}
			target := filepath.Join(h.cfg.DataDir, devicesFile)
			if err := os.Mkdir(target, 0700); err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				w := register(h, attempt == 0)
				if w.Code != http.StatusServiceUnavailable {
					t.Fatalf("failed save attempt %d became %d: %s", attempt, w.Code, w.Body)
				}
			}
			assertPending := func(current *Hub) {
				current.mu.Lock()
				d := current.accounts[accountID].devices[id]
				pending := current.deviceRegistrationPendingLocked(d)
				current.mu.Unlock()
				if !pending {
					t.Fatal("uncommitted identity lost pending marker")
				}
				if mode == "claim" {
					w := discoveryRequest(current, "/salcara-hub/v1/discovery/policy", http.Header{"X-Salcara-Device-Id": {id}, "X-Salcara-Device-Secret": {secret}})
					assertEnrollmentRetryCode(t, w, http.StatusServiceUnavailable, "remote_unavailable")
				}
				r := httptest.NewRequest(http.MethodGet, "/bridge/pair/status?deviceId="+id, nil)
				r.Header.Set("X-Salcara-Device-Secret", secret)
				w := httptest.NewRecorder()
				current.handlePairStatus(w, r, accountID)
				if w.Code == http.StatusOK {
					t.Fatal("uncommitted identity can query operational pairing")
				}
			}
			assertPending(h)
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			// An unrelated asynchronous saver may now work again. Its snapshot
			// must preserve pending, not acknowledge the failed registration.
			if err := h.saveNowError(); err != nil {
				t.Fatal(err)
			}
			h.Close()
			restored := newDeviceHub(t, h.cfg)
			assertPending(restored)
			if w := register(restored, false); w.Code != http.StatusOK {
				t.Fatal("same identity could not recover", w.Code, w.Body)
			}
			restored.Close()
			again := newDeviceHub(t, h.cfg)
			again.mu.Lock()
			defer again.mu.Unlock()
			if again.deviceRegistrationPendingLocked(again.accounts[accountID].devices[id]) || len(again.accounts) != 1 {
				t.Fatal("confirmed restart lost identity or remained pending")
			}
		})
	}
}

func TestAPIEnrollmentAdmissionIncludesIdentityMetadata(t *testing.T) {
	h, key := enrollmentRetryFixture(t)
	id, secret := "metadata-full-pc", strings.Repeat("a", 64)
	info := Device{DeviceID: id, ComputerID: "physical-computer-1", Name: "Retry test"}
	_, base, _ := prepareDeviceMetadata(info)
	h.deviceMetadataLimit = base + accountMetadataCost(deviceAccount(id))
	c, w := enrollmentRetryChallenge(t, h, id, secret, "11111111-1111-4111-8111-111111111111")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	w = enrollmentStoredRegister(t, h, enrollmentRetryGrantRequest(t, c, key, secret, "sub2api-user:711"))
	assertEnrollmentRetryCode(t, w, http.StatusTooManyRequests, "device_capacity_exceeded")
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.accounts) != 0 || h.deviceMetadataBytes != 0 {
		t.Fatal("rejected metadata admission installed an identity")
	}
}

func TestBotPendingRestartCannotConfirmMissingGroupWithoutInvite(t *testing.T) {
	h, _, _ := botFixture(t)
	t.Cleanup(h.Close)
	binding, err := h.CreateBotBindingPackage()
	if err != nil {
		t.Fatal(err)
	}
	id, secret := "pending-orphan-pc", strings.Repeat("d", 64)
	info := botDevice(id, "Pending Bot computer")
	unblock := blockBotRegistrationSave(t, h, botGroupFile)
	if w := botBodyRegister(t, h, binding, info, secret); w.Code != 503 {
		t.Fatal(w.Code, w.Body)
	}
	if err := h.saveNowError(); err != nil {
		t.Fatal(err)
	}
	h.Close()
	// Keep group persistence blocked during shutdown too. Otherwise Close
	// repairs the failed membership and this fixture no longer models a crash.
	unblock()
	restored := newDeviceHub(t, h.cfg)
	headers := map[string]string{"X-Salcara-Device-Id": id, "X-Salcara-Device-Secret": secret}
	for _, path := range []string{"/v1/device/register", "/v1/bridge/register"} {
		if w := botRequest(t, restored, http.MethodPost, path, info, headers); w.Code != 503 {
			t.Fatalf("missing Bot membership falsely confirmed at %s: %d %s", path, w.Code, w.Body)
		}
	}
	botRegistrationEpoch(t, restored, id, true)
	if w := botBodyRegister(t, restored, binding, info, secret); w.Code != 200 {
		t.Fatal("original invitation could not restore membership", w.Code, w.Body)
	}
	botRegistrationEpoch(t, restored, id, false)
	assertBotRegistrationSaved(t, restored, binding, id, secret)
}
