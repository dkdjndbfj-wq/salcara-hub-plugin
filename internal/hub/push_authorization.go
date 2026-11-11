package hub

import (
	"encoding/hex"
	"sort"
	"strings"
)

// All helpers in this file require h.mu. Notification routing uses the same
// live device/group identity at registration, admission, and actual send time.
// A Bot group account is not a device account and must never borrow QR state.
func (h *Hub) pushDeviceLocked(acct, deviceID string) *device {
	actual := acct
	if strings.HasPrefix(acct, "bot:") {
		if h.cfg.DeploymentMode != "bot" {
			return nil
		}
		group := h.botGroups[strings.TrimPrefix(acct, "bot:")]
		if group == nil || acct != "bot:"+group.ID || !group.Computers[deviceID] {
			return nil
		}
		actual = deviceAccount(deviceID)
	}
	if a := h.accounts[actual]; a != nil {
		if dev := a.devices[deviceID]; dev != nil && dev.info.DeviceID == deviceID {
			return dev
		}
	}
	return nil
}

func (h *Hub) pushBindingLocked(acct string, dev *device) string {
	if dev == nil || h.pushDeviceLocked(acct, dev.info.DeviceID) != dev || dev.banned || h.bannedDeviceIDs[dev.info.DeviceID] || h.deviceRegistrationPendingLocked(dev) || h.userDisabledLocked(dev.info.DeviceID) {
		return ""
	}
	if strings.HasPrefix(acct, "bot:") {
		group := h.botGroups[strings.TrimPrefix(acct, "bot:")]
		if group.PhonePending || !dev.hasSecret || !validPairHash(group.PhoneTokenHash) || !validPairHash(group.PhoneIDHash) {
			return ""
		}
		// Revoked refers only to the reusable invitation; an already bound
		// phone remains authorized until its own token or membership is removed.
		return shaHex("salcara-push-bot-v1\x00" + acct + "\x00" + dev.info.DeviceID + "\x00" + group.PhoneIDHash + "\x00" + group.PhoneTokenHash + "\x00" + hex.EncodeToString(dev.secretHash[:]))
	}
	if !dev.hasPair || dev.pairPending || !h.managedPairAllowedLocked(dev) {
		return ""
	}
	// Preserve the stable identity of existing relay registrations on upgrade.
	return hex.EncodeToString(dev.pairHash[:])
}

func (h *Hub) pushAccountsLocked(acct string, dev *device) []string {
	if dev == nil || h.pushDeviceLocked(acct, dev.info.DeviceID) != dev {
		return nil
	}
	actual := acct
	if strings.HasPrefix(acct, "bot:") {
		actual = deviceAccount(dev.info.DeviceID)
	}
	var accounts []string
	if h.pushBindingLocked(actual, dev) != "" {
		accounts = append(accounts, actual)
	}
	if h.cfg.DeploymentMode == "bot" {
		for id := range h.botGroups {
			target := "bot:" + id
			if h.pushBindingLocked(target, dev) != "" {
				accounts = append(accounts, target)
			}
		}
	}
	sort.Strings(accounts)
	return accounts
}
