package hub

import (
	"errors"
	"strings"
	"time"
)

// Automatic cleanup of inactive computers.
//
// Device registrations are never removed by anything else, so without this the
// site-wide metadata budget (and the 10,000 account cap) only ever fills up.
// A computer is removed only when it is offline, not banned, and has not been
// seen for the configured time. Unpaired and paired computers have separate
// limits; 0 disables cleanup for that group. Removing a computer drops its
// metadata, cached events and (for paired computers) the phone binding. A
// removed computer that comes back simply registers again with its own key;
// a removed paired computer has to be scanned again from the phone.
const (
	MaxDeviceCleanupDays = 3650
	cleanupSweepEvery    = time.Hour
)

// DeviceCleanup is the active cleanup policy, in whole days (0 = off).
type DeviceCleanup struct {
	UnpairedDays int   `json:"unpaired_days"`
	PairedDays   int   `json:"paired_days"`
	LastRunAt    int64 `json:"last_run_at"`  // epoch ms of the last sweep, 0 if none yet
	LastRemoved  int   `json:"last_removed"` // computers removed by the last sweep
}

func validCleanupDays(days int) bool { return days >= 0 && days <= MaxDeviceCleanupDays }

// CurrentDeviceCleanup reports the policy and the outcome of the last sweep.
func (h *Hub) CurrentDeviceCleanup() DeviceCleanup {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cleanup
}

// ApplyDeviceCleanup changes the policy. before persists it first (standalone
// settings file); an error leaves the running policy unchanged. A sweep with
// the new policy runs immediately. Returns the number of computers removed.
func (h *Hub) ApplyDeviceCleanup(unpairedDays, pairedDays int, before func() error) (int, error) {
	if !validCleanupDays(unpairedDays) || !validCleanupDays(pairedDays) {
		return 0, errors.New("cleanup days must be between 0 and 3650")
	}
	h.lifecycleMu.Lock()
	if h.stopped {
		h.lifecycleMu.Unlock()
		return 0, errors.New("Hub is stopping")
	}
	h.activeRequests.Add(1)
	h.lifecycleMu.Unlock()
	defer h.activeRequests.Done()
	h.mu.Lock()
	if before != nil {
		if err := before(); err != nil {
			h.mu.Unlock()
			return 0, err
		}
	}
	h.cleanup.UnpairedDays, h.cleanup.PairedDays = unpairedDays, pairedDays
	h.auditLocked(AdminAudit{At: nowMs(), Action: "device-cleanup", Reason: cleanupReason(unpairedDays, pairedDays), Actor: "standalone-admin"})
	removed := h.sweepInactiveDevicesLocked(time.Now())
	h.mu.Unlock()
	h.markDirty()
	return removed, nil
}

// RunDeviceCleanup sweeps now with the current policy.
func (h *Hub) RunDeviceCleanup() int {
	h.mu.Lock()
	removed := h.sweepInactiveDevicesLocked(time.Now())
	h.mu.Unlock()
	if removed > 0 {
		h.markDirty()
	}
	return removed
}

func cleanupReason(unpaired, paired int) string {
	part := func(label string, days int) string {
		if days == 0 {
			return label + "不清理"
		}
		return label + itoa(days) + "天"
	}
	return part("未配对", unpaired) + " · " + part("已配对", paired)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// sweepInactiveDevicesLocked removes computers that match the policy and
// releases their metadata budget, cached events and phone streams. Caller holds h.mu.
func (h *Hub) sweepInactiveDevicesLocked(now time.Time) int {
	h.cleanup.LastRunAt = now.UnixMilli()
	unpaired, paired := h.cleanup.UnpairedDays, h.cleanup.PairedDays
	if unpaired == 0 && paired == 0 {
		h.cleanup.LastRemoved = 0
		return 0
	}
	nowMillis := now.UnixMilli()
	removed := 0
	for accountID, a := range h.accounts {
		for deviceID, dev := range a.devices {
			if dev.conn != nil || dev.banned {
				continue // online computers and ban records are never cleaned
			}
			if dev.lastSeen <= 0 {
				// Old state without a timestamp: start counting from now.
				dev.lastSeen = nowMillis
				continue
			}
			days := unpaired
			if dev.hasPair {
				days = paired
			}
			if days == 0 || nowMillis-dev.lastSeen < int64(days)*24*int64(time.Hour/time.Millisecond) {
				continue
			}
			h.removeDeviceLocked(accountID, a, deviceID, dev)
			removed++
		}
		if len(a.devices) == 0 && len(a.apps) == 0 {
			h.removeAccountLocked(accountID, a)
		}
	}
	h.cleanup.LastRemoved = removed
	if removed > 0 {
		h.auditLocked(AdminAudit{At: nowMillis, Action: "device-cleanup-run", Reason: "自动清理 " + itoa(removed) + " 台不活跃电脑", Actor: "hub"})
		h.log.Info("inactive computers removed", "count", removed)
	}
	return removed
}

func (h *Hub) removeDeviceLocked(accountID string, a *account, deviceID string, dev *device) {
	if dev.hasPair {
		h.setPairLocked(accountID, dev, [32]byte{}, false)
	}
	h.forgetPush(accountID, deviceID)
	h.deletePairAttemptLocked(pairKey(accountID, deviceID))
	for sub := range a.apps {
		if sub.deviceID == deviceID {
			delete(a.apps, sub)
			sub.close()
		}
	}
	prefix := deviceID + "\x00"
	for id := range a.sessions {
		if strings.HasPrefix(id, prefix) {
			delete(a.sessions, id)
		}
	}
	a.syncCacheBytesLocked(false)
	h.deviceMetadataBytes -= dev.metadataBytes
	if h.deviceMetadataBytes < 0 {
		h.deviceMetadataBytes = 0
	}
	delete(a.devices, deviceID)
}

func (h *Hub) removeAccountLocked(accountID string, a *account) {
	if a.events != nil {
		a.events.clearCache()
	}
	a.sessions = map[string]*sessionBuf{}
	a.syncCacheBytesLocked(false)
	if a.metadataCharged {
		h.deviceMetadataBytes -= accountMetadataCost(accountID)
		if h.deviceMetadataBytes < 0 {
			h.deviceMetadataBytes = 0
		}
		a.metadataCharged = false
	}
	delete(h.accounts, accountID)
}
