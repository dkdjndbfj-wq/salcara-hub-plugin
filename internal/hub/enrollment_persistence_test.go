package hub

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func enrollmentStoredRegister(t *testing.T, h *Hub, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAPIEnrollmentFailedWriteHasNoUsableIdentityThenSameAttemptRecovers(t *testing.T) {
	h, key := enrollmentRetryFixture(t)
	id, secret := "independent-write-pc", strings.Repeat("a", 64)
	c, w := enrollmentRetryChallenge(t, h, id, secret, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	target := filepath.Join(h.cfg.DataDir, devicesFile)
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(target)
	w = enrollmentStoredRegister(t, h, enrollmentRetryGrantRequest(t, c, key, secret, "sub2api-user:771"))
	if w.Code != 503 {
		t.Fatalf("storage failure got %d %s", w.Code, w.Body)
	}
	h.mu.Lock()
	existing := h.accounts[deviceAccount(id)]
	_, pending := h.apiEnrollmentChallenges[c.ChallengeID]
	h.mu.Unlock()
	if existing != nil || !pending {
		t.Fatal("failed durable write exposed identity or consumed challenge")
	}
	w = deviceRequestWithoutEnrollmentClaim(t, h, "/device/register", id, secret, Device{DeviceID: id})
	if w.Code == 200 {
		t.Fatal("failed enrollment became credential-only recoverable")
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	w = enrollmentStoredRegister(t, h, enrollmentRetryGrantRequest(t, c, key, secret, "sub2api-user:771"))
	if w.Code != 200 {
		t.Fatalf("same pending attempt did not recover: %d %s", w.Code, w.Body)
	}
	restored := &Hub{cfg: h.cfg, accounts: map[string]*account{}, pairTokens: map[[32]byte]pairIdentity{}, bannedDeviceIDs: map[string]bool{}, deviceMetadataLimit: h.deviceMetadataLimit}
	if err := restored.loadDevices(); err != nil {
		t.Fatal(err)
	}
	if restored.accounts[deviceAccount(id)].devices[id].enrollment.Subject != "sub2api-user:771" {
		t.Fatal("success preceded durable subject")
	}
	h.Close()
	again := newDeviceHub(t, h.cfg)
	w = deviceRequestWithoutEnrollmentClaim(t, again, "/device/register", id, secret, Device{DeviceID: id})
	if w.Code != 200 {
		t.Fatalf("unknown-result recovery after restart needs grant: %d %s", w.Code, w.Body)
	}
}

func TestDisabledSubjectCannotReenrollNewIDAfterLastDeviceRemovalAndRestart(t *testing.T) {
	h, key := enrollmentRetryFixture(t)
	oldID, secret := "independent-old-pc", strings.Repeat("a", 64)
	c, w := enrollmentRetryChallenge(t, h, oldID, secret, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	w = enrollmentStoredRegister(t, h, enrollmentRetryGrantRequest(t, c, key, secret, "sub2api-user:772"))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	managedChange(t, h, ManagedUserChange{Action: "save", Name: "Independent owner"})
	u := h.managedUsers.Users[0]
	managedChange(t, h, ManagedUserChange{Action: "assign", ID: u.ID, DeviceRef: adminRef(deviceAccount(oldID), oldID)})
	managedChange(t, h, ManagedUserChange{Action: "save", ID: u.ID, Name: u.Name, Disabled: true})
	h.mu.Lock()
	delete(h.accounts, deviceAccount(oldID))
	delete(h.managedUsers.Devices, oldID)
	if err := h.commitManagedUsersLocked(cloneManagedState(h.managedUsers)); err != nil {
		h.mu.Unlock()
		t.Fatal(err)
	}
	h.mu.Unlock()
	if err := h.saveNowError(); err != nil {
		t.Fatal(err)
	}
	h.Close()
	again := newDeviceHub(t, h.cfg)
	c, w = enrollmentRetryChallenge(t, again, "independent-new-pc", secret, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	if w.Code != 200 {
		t.Fatalf("anonymous challenge should not guess subject: %d %s", w.Code, w.Body)
	}
	w = enrollmentStoredRegister(t, again, enrollmentRetryGrantRequest(t, c, key, secret, "sub2api-user:772"))
	assertEnrollmentRetryCode(t, w, 423, "user_disabled")
	again.mu.Lock()
	defer again.mu.Unlock()
	if len(again.accounts) != 0 {
		t.Fatal("disabled external subject created a fresh device")
	}
}
