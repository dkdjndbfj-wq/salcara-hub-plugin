package hub

import (
	"crypto/sha256"
	"strings"
	"testing"
	"time"
)

const dayMs = int64(24 * time.Hour / time.Millisecond)

func ageDevice(t *testing.T, h *Hub, id string, days int64) *device {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	dev := h.accounts[deviceAccount(id)].devices[id]
	dev.lastSeen = nowMs() - days*dayMs - 1000
	return dev
}

func TestDeviceCleanupRemovesOnlyInactiveMatchingComputersAndReleasesBudget(t *testing.T) {
	h := newDeviceHub(t, Config{ResourceMode: "economy"})
	secret := strings.Repeat("d", 64)
	for _, id := range []string{"stale-unpaired", "fresh-unpaired", "stale-paired", "online-stale", "banned-stale"} {
		enrollDevice(t, h, id, secret)
	}
	h.mu.Lock()
	start := h.deviceMetadataBytes
	h.mu.Unlock()
	ageDevice(t, h, "stale-unpaired", 10)
	ageDevice(t, h, "fresh-unpaired", 2)
	paired := ageDevice(t, h, "stale-paired", 10)
	ageDevice(t, h, "online-stale", 10)
	onlineDevice(t, h, "online-stale")
	banned := ageDevice(t, h, "banned-stale", 10)
	h.mu.Lock()
	h.setPairLocked(deviceAccount("stale-paired"), paired, sha256.Sum256([]byte("pair")), true)
	banned.banned = true
	h.mu.Unlock()

	// Unpaired after 7 days; paired never.
	removed, err := h.ApplyDeviceCleanup(7, 0, nil)
	if err != nil || removed != 1 {
		t.Fatalf("want exactly the stale unpaired computer removed, got %d %v", removed, err)
	}
	h.mu.Lock()
	if h.accounts[deviceAccount("stale-unpaired")] != nil {
		t.Fatal("stale unpaired computer and its account should be gone")
	}
	for _, id := range []string{"fresh-unpaired", "stale-paired", "online-stale", "banned-stale"} {
		if h.accounts[deviceAccount(id)] == nil {
			t.Fatalf("%s must be kept", id)
		}
	}
	if h.deviceMetadataBytes >= start {
		t.Fatal("cleanup did not release the metadata budget")
	}
	h.mu.Unlock()

	// Paired after 5 days: the paired computer and its phone binding go too.
	if removed, _ := h.ApplyDeviceCleanup(7, 5, nil); removed != 1 {
		t.Fatalf("paired cleanup removed %d", removed)
	}
	h.mu.Lock()
	if h.accounts[deviceAccount("stale-paired")] != nil || len(h.pairTokens) != 0 {
		t.Fatal("paired computer or its phone token survived cleanup")
	}
	cur := h.cleanup
	h.mu.Unlock()
	if cur.UnpairedDays != 7 || cur.PairedDays != 5 || cur.LastRunAt == 0 || cur.LastRemoved != 1 {
		t.Fatalf("policy state not reported: %+v", cur)
	}

	// A cleaned computer simply registers again with its own key.
	enrollDevice(t, h, "stale-unpaired", secret)
}

func TestDeviceCleanupOffRemovesNothingAndRejectsInvalidDays(t *testing.T) {
	h := newDeviceHub(t, Config{ResourceMode: "economy"})
	enrollDevice(t, h, "old-pc", strings.Repeat("e", 64))
	ageDevice(t, h, "old-pc", 4000)
	if removed := h.RunDeviceCleanup(); removed != 0 {
		t.Fatal("cleanup is off by default for the embedded Hub")
	}
	for _, bad := range [][2]int{{-1, 0}, {0, MaxDeviceCleanupDays + 1}} {
		if _, err := h.ApplyDeviceCleanup(bad[0], bad[1], nil); err == nil {
			t.Fatalf("invalid days accepted: %v", bad)
		}
	}
	if _, err := h.ApplyDeviceCleanup(30, 0, func() error { return errTestPersist }); err == nil || h.CurrentDeviceCleanup().UnpairedDays != 0 {
		t.Fatal("a failed save must leave the running policy unchanged")
	}
}

func TestDeviceCleanupStartsCountingForComputersWithoutTimestamp(t *testing.T) {
	h := newDeviceHub(t, Config{ResourceMode: "economy", CleanupUnpairedDays: 1})
	enrollDevice(t, h, "legacy-pc", strings.Repeat("f", 64))
	h.mu.Lock()
	h.accounts[deviceAccount("legacy-pc")].devices["legacy-pc"].lastSeen = 0
	h.mu.Unlock()
	if removed := h.RunDeviceCleanup(); removed != 0 {
		t.Fatal("a computer without a timestamp must not be removed immediately")
	}
	h.mu.Lock()
	if h.accounts[deviceAccount("legacy-pc")].devices["legacy-pc"].lastSeen == 0 {
		t.Fatal("missing timestamp should start counting from now")
	}
	h.mu.Unlock()
}

var errTestPersist = &persistErr{}

type persistErr struct{}

func (*persistErr) Error() string { return "fixture save failure" }
