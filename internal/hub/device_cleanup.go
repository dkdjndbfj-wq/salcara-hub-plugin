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
	UnpairedDays int    `json:"unpaired_days"`
	PairedDays   int    `json:"paired_days"`
	LastRunAt    int64  `json:"last_run_at"`  // epoch ms of the last sweep, 0 if none yet
	LastRemoved  int    `json:"last_removed"` // computers removed by the last sweep
	LastError    string `json:"last_error,omitempty"`
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
	h.enrollmentMu.Lock()
	defer h.enrollmentMu.Unlock()
	h.saveMu.Lock()
	defer h.saveMu.Unlock()
	h.mu.Lock()
	if before != nil {
		if err := before(); err != nil {
			h.mu.Unlock()
			return 0, err
		}
	}
	h.cleanup.UnpairedDays, h.cleanup.PairedDays = unpairedDays, pairedDays
	h.auditLocked(AdminAudit{At: nowMs(), Action: "device-cleanup", Reason: cleanupReason(unpairedDays, pairedDays), Actor: "standalone-admin"})
	return h.sweepInactiveDevicesLocked(time.Now())
}

// RunDeviceCleanup sweeps now with the current policy.
func (h *Hub) RunDeviceCleanup() int {
	removed, _ := h.RunDeviceCleanupError()
	return removed
}

// RunDeviceCleanupError also reports partial persistence; callers must not
// claim success when membership repair is still pending.
func (h *Hub) RunDeviceCleanupError() (int, error) {
	h.lifecycleMu.Lock()
	if h.stopped {
		h.lifecycleMu.Unlock()
		return 0, errors.New("Hub is stopping")
	}
	h.activeRequests.Add(1)
	h.lifecycleMu.Unlock()
	defer h.activeRequests.Done()
	h.enrollmentMu.Lock()
	defer h.enrollmentMu.Unlock()
	h.saveMu.Lock()
	defer h.saveMu.Unlock()
	h.mu.Lock()
	return h.sweepInactiveDevicesLocked(time.Now())
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

// Caller holds enrollmentMu, saveMu and mu. This method releases mu. Commit
// device removal first, then publish it in memory and persist group membership.
// Startup prunes orphan memberships if the second write fails or we crash.
func (h *Hub) sweepInactiveDevicesLocked(now time.Time) (int, error) {
	h.cleanup.LastRunAt = now.UnixMilli()
	h.cleanup.LastError = ""
	h.cleanup.LastRemoved = 0
	unpaired, paired := h.cleanup.UnpairedDays, h.cleanup.PairedDays
	nowMillis := now.UnixMilli()
	candidates := map[string]map[string]bool{}
	for accountID, a := range h.accounts {
		for deviceID, dev := range a.devices {
			if dev.conn != nil || dev.banned || h.deviceRegistrationPendingLocked(dev) {
				continue // online computers and ban records are never cleaned
			}
			botPaired, pending := h.cleanupBotIdentityLocked(accountID, deviceID)
			if pending {
				continue
			}
			if dev.lastSeen <= 0 {
				// Old state without a timestamp: start counting from now.
				dev.lastSeen = nowMillis
				continue
			}
			days := unpaired
			if dev.hasPair || botPaired {
				days = paired
			}
			if days == 0 || nowMillis-dev.lastSeen < int64(days)*24*int64(time.Hour/time.Millisecond) {
				continue
			}
			if candidates[accountID] == nil {
				candidates[accountID] = map[string]bool{}
			}
			candidates[accountID][deviceID] = true
		}
	}
	removed := 0
	if len(candidates) > 0 {
		snapshot := h.snapshotLocked()
		for accountID, ids := range candidates {
			kept := snapshot.Accounts[accountID][:0]
			for _, d := range snapshot.Accounts[accountID] {
				if !ids[d.Device.DeviceID] {
					kept = append(kept, d)
				}
			}
			if len(kept) == 0 {
				delete(snapshot.Accounts, accountID)
			} else {
				snapshot.Accounts[accountID] = kept
			}
		}
		if err := h.writeDeviceSnapshot(snapshot); err != nil {
			h.cleanup.LastError = "设备清理尚未保存，请检查数据目录空间和写入权限后重试。"
			h.mu.Unlock()
			h.log.Error("device cleanup commit failed", "err", err)
			return 0, err
		}
		for accountID, ids := range candidates {
			a := h.accounts[accountID]
			for id := range ids {
				h.removeDeviceLocked(accountID, a, id, a.devices[id])
				if h.cfg.DeploymentMode == "bot" && accountID == deviceAccount(id) {
					for _, g := range h.botGroups {
						delete(g.Computers, id)
					}
					h.cleanupBotPending = true
				}
				removed++
			}
			if len(a.devices) == 0 && len(a.apps) == 0 {
				h.removeAccountLocked(accountID, a)
			}
		}
		// The committed snapshot already excludes every newly revoked pair.
		h.savedPairRevocationEpoch = h.pairRevocationEpoch
	}
	h.cleanup.LastRemoved = removed
	if removed > 0 {
		h.auditLocked(AdminAudit{At: nowMillis, Action: "device-cleanup-run", Reason: "自动清理 " + itoa(removed) + " 台不活跃电脑", Actor: "hub"})
		h.log.Info("inactive computers removed", "count", removed)
	}
	botSnapshot := h.botSnapshotLocked()
	h.mu.Unlock()
	if h.cleanupBotPending {
		if err := h.writeBotSnapshot(botSnapshot); err != nil {
			h.mu.Lock()
			h.cleanup.LastError = "电脑已清理，绑定组记录尚未同步；请检查数据目录空间和写入权限，再执行一次清理。"
			h.mu.Unlock()
			h.log.Error("device cleanup membership commit failed", "err", err)
			return removed, err
		}
		h.cleanupBotPending = false
	}
	h.markDirty()
	return removed, nil
}

func (h *Hub) cleanupBotIdentityLocked(accountID, id string) (paired, pending bool) {
	if h.cfg.DeploymentMode != "bot" || accountID != deviceAccount(id) {
		return
	}
	for _, g := range h.botGroups {
		if g.Computers[id] {
			paired = paired || g.PhoneTokenHash != "" || g.PhonePending
			pending = pending || g.PhonePending || g.phoneIntent != nil
		}
	}
	return
}

func (h *Hub) pruneBotComputersLocked() {
	for _, g := range h.botGroups {
		for id := range g.Computers {
			a := h.accounts[deviceAccount(id)]
			if a == nil || a.devices[id] == nil {
				delete(g.Computers, id)
			}
		}
	}
}

func (h *Hub) removeDeviceLocked(accountID string, a *account, deviceID string, dev *device) {
	if h.cfg.DeploymentMode == "bot" && accountID == deviceAccount(deviceID) {
		h.notifyBotScopeChangedLocked(&botGroup{Computers: map[string]bool{deviceID: true}})
	}
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
