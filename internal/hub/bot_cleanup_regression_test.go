package hub

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func cleanupBotGroup(t *testing.T, h *Hub) BotBindingPackage {
	t.Helper()
	p, err := h.CreateBotBindingPackage()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func cleanupBotMember(t *testing.T, h *Hub, group, id string) bool {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	g := h.botGroups[group]
	return g != nil && g.Computers[id]
}

func cleanupBotHasDevice(t *testing.T, h *Hub, id string) bool {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.accounts[deviceAccount(id)]
	return a != nil && a.devices[id] != nil
}

// Capture only durable identity files while the original hub is alive. Unlike
// Close followed by New, this does not repair a partial commit before restart.
func cleanupBotCrashImage(t *testing.T, h *Hub) string {
	t.Helper()
	dir := t.TempDir()
	h.saveMu.Lock()
	defer h.saveMu.Unlock()
	for _, name := range []string{devicesFile, botGroupFile, botPhoneIntentFile} {
		b, err := os.ReadFile(filepath.Join(h.cfg.DataDir, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func cleanupBotRestart(t *testing.T, dir, url string) *Hub {
	t.Helper()
	h, err := New(Config{DeploymentMode: "bot", DataDir: dir, PublicURL: url})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

func TestBotCleanupRetainsPhoneIdentityAfterInviteRevocationAndRestart(t *testing.T) {
	h, _, url := botFixture(t)
	t.Cleanup(h.Close)
	p := cleanupBotGroup(t, h)
	d, secret := botDevice("cleanup-paired", "Paired"), strings.Repeat("1", 64)
	requireBotCode(t, botBodyRegister(t, h, p, d, secret), http.StatusOK, "")
	token := bindBotSourcePhone(t, h, p)
	if err := h.RevokeBotGroup(p.GroupID); err != nil {
		t.Fatal(err)
	}
	ageDevice(t, h, d.DeviceID, 30)
	if removed, err := h.ApplyDeviceCleanup(7, 0, nil); err != nil || removed != 0 {
		t.Fatalf("phone identity classified unpaired: removed=%d err=%v", removed, err)
	}
	if err := h.saveNowError(); err != nil {
		t.Fatal(err)
	}
	next := cleanupBotRestart(t, cleanupBotCrashImage(t, h), url)
	headers := map[string]string{"X-Salcara-Device-Id": d.DeviceID, "X-Salcara-Device-Secret": secret}
	requireBotCode(t, botRequest(t, next, http.MethodPost, "/v1/device/register", d, headers), http.StatusOK, "")
	list := botRequest(t, next, http.MethodGet, "/v1/app/devices", nil, map[string]string{"X-Salcara-Bot-Token": token})
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), d.DeviceID) {
		t.Fatalf("preserved identity lost phone authority: %d %s", list.Code, list.Body)
	}
}

func TestBotCleanupReleasesCapacityForBothEnrollmentPathsAfterRestart(t *testing.T) {
	h, _, url := botFixture(t)
	t.Cleanup(h.Close)
	p := cleanupBotGroup(t, h)
	secret := strings.Repeat("2", 64)
	for i := 0; i < maxBotComputers; i++ {
		d := botDevice(fmt.Sprintf("cleanup-cap-%d", i), "Expired")
		requireBotCode(t, botBodyRegister(t, h, p, d, secret), http.StatusOK, "")
		ageDevice(t, h, d.DeviceID, 30)
	}
	if removed, err := h.ApplyDeviceCleanup(7, 0, nil); err != nil || removed != maxBotComputers {
		t.Fatalf("cleanup capacity: removed=%d err=%v", removed, err)
	}
	next := cleanupBotRestart(t, cleanupBotCrashImage(t, h), url)
	requireBotCode(t, botRegisterRequest(t, next, p, botDevice("cleanup-new-header", "New header"), secret), http.StatusOK, "")
	requireBotCode(t, botBodyRegister(t, next, p, botDevice("cleanup-new-body", "New body"), secret), http.StatusOK, "")
	for i := 0; i < maxBotComputers; i++ {
		if cleanupBotMember(t, next, p.GroupID, fmt.Sprintf("cleanup-cap-%d", i)) {
			t.Fatal("expired membership survived restart")
		}
	}
}

func TestBotCleanupDeviceWriteFailureKeepsIdentityAndMembership(t *testing.T) {
	h, _, _ := botFixture(t)
	t.Cleanup(h.Close)
	p := cleanupBotGroup(t, h)
	d, secret := botDevice("cleanup-first-write", "Retained"), strings.Repeat("3", 64)
	requireBotCode(t, botBodyRegister(t, h, p, d, secret), http.StatusOK, "")
	ageDevice(t, h, d.DeviceID, 30)
	h.mu.Lock()
	beforeBudget := h.deviceMetadataBytes
	h.mu.Unlock()
	unblock := blockBotRegistrationSave(t, h, devicesFile)
	removed, err := h.ApplyDeviceCleanup(7, 0, nil)
	if err == nil || removed != 0 {
		t.Fatalf("failed first write acknowledged: %d %v", removed, err)
	}
	if !cleanupBotHasDevice(t, h, d.DeviceID) || !cleanupBotMember(t, h, p.GroupID, d.DeviceID) {
		t.Fatal("first write failure deleted live identity or membership")
	}
	h.mu.Lock()
	afterBudget := h.deviceMetadataBytes
	h.mu.Unlock()
	if beforeBudget != afterBudget {
		t.Fatal("first write failure released metadata")
	}
	unblock()
	requireBotCode(t, botRequest(t, h, http.MethodPost, "/v1/device/register", d,
		map[string]string{"X-Salcara-Device-Id": d.DeviceID, "X-Salcara-Device-Secret": secret}), http.StatusOK, "")
}

func TestBotCleanupBotWriteFailureCommitsDeletionAndRepairsOnRestart(t *testing.T) {
	h, _, url := botFixture(t)
	t.Cleanup(h.Close)
	p, other := cleanupBotGroup(t, h), cleanupBotGroup(t, h)
	d, secret := botDevice("cleanup-second-write", "Expired paired"), strings.Repeat("4", 64)
	requireBotCode(t, botBodyRegister(t, h, p, d, secret), http.StatusOK, "")
	token := bindBotSourcePhone(t, h, p)
	ageDevice(t, h, d.DeviceID, 30)
	unblock := blockBotRegistrationSave(t, h, botGroupFile)
	removed, err := h.ApplyDeviceCleanup(0, 7, nil)
	if err == nil || removed != 1 {
		t.Fatalf("second write result: removed=%d err=%v", removed, err)
	}
	if cleanupBotHasDevice(t, h, d.DeviceID) || cleanupBotMember(t, h, p.GroupID, d.DeviceID) {
		t.Fatal("second write failure rolled back committed deletion")
	}
	// Restores the old valid Bot snapshot, modeling a crash before its replacement.
	unblock()
	crashDir := cleanupBotCrashImage(t, h)
	next := cleanupBotRestart(t, crashDir, url)
	if cleanupBotHasDevice(t, next, d.DeviceID) || cleanupBotMember(t, next, p.GroupID, d.DeviceID) {
		t.Fatal("startup resurrected deleted identity or stale group authority")
	}
	oldHeaders := map[string]string{"X-Salcara-Device-Id": d.DeviceID, "X-Salcara-Device-Secret": secret}
	requireBotCode(t, botRequest(t, next, http.MethodPost, "/v1/device/register", d, oldHeaders), http.StatusForbidden, "bot_credentials_invalid")
	// An explicit invitation for another group cannot restore the old group's grant.
	requireBotCode(t, botBodyRegister(t, next, other, d, strings.Repeat("5", 64)), http.StatusOK, "")
	list := botRequest(t, next, http.MethodGet, "/v1/app/devices", nil, map[string]string{"X-Salcara-Bot-Token": token})
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), d.DeviceID) {
		t.Fatalf("same ID inherited former group: %d %s", list.Code, list.Body)
	}
	// Retry in the original live instance must persist groups even with no candidates.
	if removed, err := h.ApplyDeviceCleanup(0, 7, nil); err != nil || removed != 0 {
		t.Fatalf("partial-commit retry: %d %v", removed, err)
	}
	h.saveMu.Lock()
	raw, readErr := os.ReadFile(filepath.Join(h.cfg.DataDir, botGroupFile))
	h.saveMu.Unlock()
	if readErr != nil {
		t.Fatal(readErr)
	}
	var groups persistedBotState
	if err := json.Unmarshal(raw, &groups); err != nil {
		t.Fatal(err)
	}
	for _, group := range groups.Groups {
		for _, id := range group.Computers {
			if id == d.DeviceID {
				t.Fatal("retry left the stale membership on disk")
			}
		}
	}
}

func TestBotCleanupLegacyAccountCollisionDoesNotRemoveCanonicalMembership(t *testing.T) {
	h, _, _ := botFixture(t)
	t.Cleanup(h.Close)
	p := cleanupBotGroup(t, h)
	d, secret := botDevice("cleanup-legacy-collision", "Canonical"), strings.Repeat("6", 64)
	requireBotCode(t, botBodyRegister(t, h, p, d, secret), http.StatusOK, "")
	bindBotSourcePhone(t, h, p)
	h.mu.Lock()
	_, status, _ := h.registerDeviceLocked("legacy-api-account", d, sha256.Sum256([]byte(secret)))
	if status == 0 {
		legacy := h.accounts["legacy-api-account"].devices[d.DeviceID]
		legacy.savedRegistrationEpoch = legacy.registrationEpoch
		legacy.lastSeen = nowMs() - 30*dayMs
	}
	h.mu.Unlock()
	if status != 0 {
		t.Fatalf("legacy fixture status=%d", status)
	}
	if removed, err := h.ApplyDeviceCleanup(7, 0, nil); err != nil || removed != 1 {
		t.Fatalf("legacy same ID incorrectly borrowed Bot retention: %d %v", removed, err)
	}
	if !cleanupBotHasDevice(t, h, d.DeviceID) || !cleanupBotMember(t, h, p.GroupID, d.DeviceID) {
		t.Fatal("deleting legacy duplicate removed canonical Bot membership")
	}
}

func TestBotCleanupKeepsPendingEnrollmentForOriginalIdentityRetry(t *testing.T) {
	h, _, url := botFixture(t)
	t.Cleanup(h.Close)
	p := cleanupBotGroup(t, h)
	d, secret := botDevice("cleanup-pending-enrollment", "Pending"), strings.Repeat("7", 64)
	unblock := blockBotRegistrationSave(t, h, botGroupFile)
	requireBotCode(t, botBodyRegister(t, h, p, d, secret), http.StatusServiceUnavailable, "remote_unavailable")
	ageDevice(t, h, d.DeviceID, 30)
	if err := h.saveNowError(); err != nil {
		t.Fatal(err)
	}
	unblock()
	if removed, err := h.ApplyDeviceCleanup(7, 7, nil); err != nil || removed != 0 {
		t.Fatalf("pending identity swept: %d %v", removed, err)
	}
	next := cleanupBotRestart(t, cleanupBotCrashImage(t, h), url)
	botRegistrationEpoch(t, next, d.DeviceID, true)
	requireBotCode(t, botBodyRegister(t, next, p, d, secret), http.StatusOK, "")
	botRegistrationEpoch(t, next, d.DeviceID, false)
}

func TestBotCleanupKeepsPendingPhoneIntentAcrossRestartAndRetry(t *testing.T) {
	h, _, url := botFixture(t)
	t.Cleanup(h.Close)
	p := cleanupBotGroup(t, h)
	d, secret := botDevice("cleanup-pending-phone", "Pending phone"), strings.Repeat("8", 64)
	requireBotCode(t, botBodyRegister(t, h, p, d, secret), http.StatusOK, "")
	bindBotSourcePhone(t, h, p)
	reset := botResetInput(t, h, p.GroupID)
	unblock := blockBotRegistrationSave(t, h, botGroupFile)
	if _, err := h.ResetBotPhone(reset); err != ErrBotPhoneUnavailable {
		t.Fatalf("pending reset: %v", err)
	}
	ageDevice(t, h, d.DeviceID, 30)
	unblock()
	if removed, err := h.ApplyDeviceCleanup(7, 7, nil); err != nil || removed != 0 {
		t.Fatalf("pending phone member swept: %d %v", removed, err)
	}
	if err := h.saveNowError(); err != nil {
		t.Fatal(err)
	}
	next := cleanupBotRestart(t, cleanupBotCrashImage(t, h), url)
	if !cleanupBotHasDevice(t, next, d.DeviceID) || !cleanupBotMember(t, next, p.GroupID, d.DeviceID) {
		t.Fatal("phone intent replay lost computer")
	}
	if _, err := next.ResetBotPhone(reset); err != nil {
		t.Fatal(err)
	}
	if !cleanupBotMember(t, next, p.GroupID, d.DeviceID) {
		t.Fatal("phone reset retry lost computer")
	}
}

func TestBotCleanupStartupOrphanIsNotRescuedByLegacyNamespace(t *testing.T) {
	h, _, url := botFixture(t)
	t.Cleanup(h.Close)
	p := cleanupBotGroup(t, h)
	d, secret := botDevice("cleanup-legacy-orphan", "Legacy remains"), strings.Repeat("9", 64)
	requireBotCode(t, botBodyRegister(t, h, p, d, secret), http.StatusOK, "")
	h.mu.Lock()
	_, status, _ := h.registerDeviceLocked("legacy-api-account", d, sha256.Sum256([]byte(secret)))
	if status == 0 {
		legacy := h.accounts["legacy-api-account"].devices[d.DeviceID]
		legacy.savedRegistrationEpoch = legacy.registrationEpoch
	}
	h.mu.Unlock()
	if status != 0 {
		t.Fatalf("legacy fixture status=%d", status)
	}
	if err := h.saveNowError(); err != nil {
		t.Fatal(err)
	}
	dir := cleanupBotCrashImage(t, h)
	path := filepath.Join(dir, devicesFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var devices persistedState
	if err := json.Unmarshal(raw, &devices); err != nil {
		t.Fatal(err)
	}
	delete(devices.Accounts, deviceAccount(d.DeviceID))
	raw, err = json.Marshal(devices)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	next := cleanupBotRestart(t, dir, url)
	if cleanupBotMember(t, next, p.GroupID, d.DeviceID) {
		t.Fatal("legacy duplicate incorrectly rescued orphan Bot authority")
	}
	requireBotCode(t, botRequest(t, next, http.MethodPost, "/v1/device/register", d,
		map[string]string{"X-Salcara-Device-Id": d.DeviceID, "X-Salcara-Device-Secret": secret}), http.StatusForbidden, "bot_credentials_invalid")
}

func TestBotCleanupPreservesBanAcrossRestart(t *testing.T) {
	h, _, url := botFixture(t)
	t.Cleanup(h.Close)
	p := cleanupBotGroup(t, h)
	d, secret := botDevice("cleanup-banned", "Banned"), strings.Repeat("a", 64)
	requireBotCode(t, botBodyRegister(t, h, p, d, secret), http.StatusOK, "")
	if _, err := h.AdminAction(context.Background(), "ban", adminRef(deviceAccount(d.DeviceID), d.DeviceID), "synthetic cleanup test"); err != nil {
		t.Fatal(err)
	}
	ageDevice(t, h, d.DeviceID, 30)
	if removed, err := h.ApplyDeviceCleanup(7, 7, nil); err != nil || removed != 0 {
		t.Fatalf("ban record swept: %d %v", removed, err)
	}
	if err := h.saveNowError(); err != nil {
		t.Fatal(err)
	}
	next := cleanupBotRestart(t, cleanupBotCrashImage(t, h), url)
	if !cleanupBotHasDevice(t, next, d.DeviceID) {
		t.Fatal("ban record missing after restart")
	}
	for _, attempt := range []string{secret, strings.Repeat("b", 64)} {
		if w := botBodyRegister(t, next, p, d, attempt); w.Code != http.StatusForbidden {
			t.Fatalf("ban bypass after cleanup: %d %s", w.Code, w.Body)
		}
	}
}
