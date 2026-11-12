package hub

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stationEnrollmentErrorCode(t *testing.T, raw []byte) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	return body.Code
}

// Observation test: null advertises no API enrollment, not free registration.
func TestStationEnrollmentPingAndUnpairedStatesAreSeparate(t *testing.T) {
	h := newDeviceHub(t, Config{DataDir: t.TempDir(), DeploymentMode: "station"})
	p := botRequest(t, h, http.MethodGet, "/v1/ping", nil, nil)
	var ping struct {
		DeviceEnrollment json.RawMessage `json:"deviceEnrollment"`
	}
	if p.Code != 200 || json.Unmarshal(p.Body.Bytes(), &ping) != nil || string(ping.DeviceEnrollment) != "null" {
		t.Fatalf("unexpected fixture discovery status=%d enrollment=%s", p.Code, ping.DeviceEnrollment)
	}
	d, secret := Device{DeviceID: "enrollment-audit-first"}, strings.Repeat("a", 64)
	w := deviceRequestWithoutEnrollmentClaim(t, h, "/device/register", d.DeviceID, secret, d)
	if w.Code != 403 || stationEnrollmentErrorCode(t, w.Body.Bytes()) != "device_enrollment_required" {
		t.Fatalf("no claim: %d %s", w.Code, w.Body)
	}
	claim, _, err := h.CreateDeviceEnrollmentClaim()
	if err != nil {
		t.Fatal(err)
	}
	w = deviceRequestWithEnrollmentClaim(t, h, "/device/register", d.DeviceID, secret, "", d, "", claim, false)
	if w.Code != 200 {
		t.Fatalf("first register: %d %s", w.Code, w.Body)
	}
	w = deviceRequestWithoutEnrollmentClaim(t, h, "/discovery/policy", d.DeviceID, secret, nil)
	if w.Code != 200 {
		t.Fatalf("registered policy: %d %s", w.Code, w.Body)
	}
	w = deviceRequestWithoutEnrollmentClaim(t, h, "/bridge/pair/status", d.DeviceID, secret, nil)
	var pair struct {
		Paired bool `json:"paired"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &pair) != nil || pair.Paired {
		t.Fatalf("registered, unpaired: %d %s", w.Code, w.Body)
	}
	t.Log("ping=200/deviceEnrollment:null; no claim=403/device_enrollment_required; valid claim=200; policy=200; pair-status=200/paired:false")
}

func TestStationEnrollmentStationPersistenceErrorsHaveMachineCode(t *testing.T) {
	for _, filename := range []string{enrollmentClaimsFile, managedUsersFile, devicesFile} {
		t.Run(filename, func(t *testing.T) {
			h := newDeviceHub(t, Config{DataDir: t.TempDir(), DeploymentMode: "station"})
			userID := ""
			if filename == managedUsersFile {
				managedChange(t, h, ManagedUserChange{Action: "save", Name: "Synthetic persistence owner"})
				userID = h.managedUsers.Users[0].ID
			}
			claim, _, err := h.CreateDeviceEnrollmentClaimForUser(userID)
			if err != nil {
				t.Fatal(err)
			}
			unblock := blockBotRegistrationSave(t, h, filename)
			d, secret := Device{DeviceID: "enrollment-audit-persist"}, strings.Repeat("b", 64)
			w := deviceRequestWithEnrollmentClaim(t, h, "/device/register", d.DeviceID, secret, "", d, "", claim, false)
			if w.Code != 503 {
				t.Fatalf("expected storage failure: %d %s", w.Code, w.Body)
			}
			code := stationEnrollmentErrorCode(t, w.Body.Bytes())
			unblock()
			if code != "remote_unavailable" {
				t.Fatalf("station first-register 503 lacks documented code: code=%q body=%s", code, w.Body)
			}
		})
	}
}

func TestStationEnrollmentOwnerWriteFailureDoesNotBurnUnregisteredClaim(t *testing.T) {
	h := newDeviceHub(t, Config{DataDir: t.TempDir(), DeploymentMode: "station"})
	managedChange(t, h, ManagedUserChange{Action: "save", Name: "Synthetic enrollment owner"})
	user := h.managedUsers.Users[0]
	claim, _, err := h.CreateDeviceEnrollmentClaimForUser(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	unblock := blockBotRegistrationSave(t, h, managedUsersFile)
	d, secret := Device{DeviceID: "enrollment-audit-owner"}, strings.Repeat("c", 64)
	w := deviceRequestWithEnrollmentClaim(t, h, "/device/register", d.DeviceID, secret, "", d, "", claim, false)
	if w.Code != 503 {
		t.Fatalf("expected owner storage failure: %d %s", w.Code, w.Body)
	}
	unblock()
	hash, _ := enrollmentClaimHash(claim)
	h.mu.Lock()
	a := h.accounts[deviceAccount(d.DeviceID)]
	exists := a != nil && a.devices[d.DeviceID] != nil
	valid := h.enrollmentClaimValidLocked(hash)
	h.mu.Unlock()
	w = deviceRequestWithEnrollmentClaim(t, h, "/device/register", d.DeviceID, secret, "", d, "", claim, false)
	if !valid || exists || w.Code != 200 {
		t.Fatalf("owner write failure burned claim before identity existed: claimUsable=%v deviceExists=%v same-attempt retry=%d code=%q", valid, exists, w.Code, stationEnrollmentErrorCode(t, w.Body.Bytes()))
	}
}

func TestStationEnrollmentFailedWritesKeepOneClaimWithoutOrphanAssignments(t *testing.T) {
	for _, filename := range []string{enrollmentClaimsFile, managedUsersFile} {
		t.Run(filename, func(t *testing.T) {
			h := newDeviceHub(t, Config{DataDir: t.TempDir(), DeploymentMode: "station"})
			managedChange(t, h, ManagedUserChange{Action: "save", Name: "Retry owner"})
			userID := h.managedUsers.Users[0].ID
			claim, expires, err := h.CreateDeviceEnrollmentClaimForUser(userID)
			if err != nil {
				t.Fatal(err)
			}
			unblock := blockBotRegistrationSave(t, h, filename)
			secret := strings.Repeat("d", 64)
			for _, id := range []string{"failed-claim-first", "failed-claim-second"} {
				w := deviceRequestWithEnrollmentClaim(t, h, "/device/register", id, secret, "", Device{DeviceID: id}, "", claim, false)
				assertEnrollmentRetryCode(t, w, http.StatusServiceUnavailable, "remote_unavailable")
				h.mu.Lock()
				assignments, accounts := len(h.managedUsers.Devices), len(h.accounts)
				h.mu.Unlock()
				if assignments != 0 || accounts != 0 {
					t.Fatal("failed enrollment allocated an owner or device", assignments, accounts)
				}
			}
			unblock()
			raw, err := os.ReadFile(filepath.Join(h.cfg.DataDir, enrollmentClaimsFile))
			if err != nil {
				t.Fatal(err)
			}
			state, err := decodeEnrollmentClaims(raw)
			if err != nil || len(state.Claims) != 1 || state.Claims[0].UserID != userID || state.Claims[0].ExpiresAt != expires.UnixMilli() {
				t.Fatal("failed enrollment changed the original durable claim")
			}
			h.Close()
			restored := newDeviceHub(t, h.cfg)
			id := "failed-claim-first"
			w := deviceRequestWithEnrollmentClaim(t, restored, "/device/register", id, secret, "", Device{DeviceID: id}, "", claim, false)
			if w.Code != http.StatusOK {
				t.Fatal("same claim could not recover after restart", w.Code, w.Body)
			}
			w = deviceRequestWithEnrollmentClaim(t, restored, "/device/register", "claim-replay", secret, "", Device{DeviceID: "claim-replay"}, "", claim, false)
			assertEnrollmentRetryCode(t, w, http.StatusForbidden, "enrollment_claim_invalid")
			restored.mu.Lock()
			defer restored.mu.Unlock()
			if len(restored.managedUsers.Devices) != 1 || restored.managedUsers.Devices[id].UserID != userID || len(restored.accounts) != 1 {
				t.Fatal("recovery changed ownership or allowed claim replay")
			}
		})
	}
}
