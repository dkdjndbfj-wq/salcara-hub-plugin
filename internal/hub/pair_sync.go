package hub

import (
	"encoding/hex"
	"net/http"
)

// A computer may extend its already established physical-phone binding to
// another saved relay. Only the relay's computer credential can do this; the
// phone never receives that credential. The Bridge supplies a deterministic,
// generation-scoped phone-token hash so retries do not invalidate the phone.
func (h *Hub) handlePairSync(w http.ResponseWriter, r *http.Request, accountID string) {
	var in struct {
		DeviceID   string `json:"deviceId"`
		ComputerID string `json:"computerId"`
		BindingID  string `json:"bindingId"`
		PhoneHash  string `json:"phoneHash"`
		PairHash   string `json:"pairHash"`
	}
	if !decodeBody(w, r, maxDefaultBody, &in) {
		return
	}
	if !validID(in.DeviceID) || !validID(in.ComputerID) || !validPairHash(in.BindingID) || !validPairHash(in.PhoneHash) || !validPairHash(in.PairHash) {
		writeError(w, 400, "电脑或手机绑定身份无效")
		return
	}
	decoded, _ := hex.DecodeString(in.PairHash)
	var hash [32]byte
	copy(hash[:], decoded)
	h.mu.Lock()
	var dev *device
	if a := h.accounts[accountID]; a != nil {
		dev = a.devices[in.DeviceID]
	}
	if dev == nil || dev.banned || !matchesDeviceSecret(dev, r) {
		h.mu.Unlock()
		writeError(w, 403, "电脑身份验证失败")
		return
	}
	if dev.pairPending {
		h.mu.Unlock()
		writePairPersistencePending(w)
		return
	}
	sameBinding := dev.bindingID == in.BindingID && dev.phoneHash == in.PhoneHash
	// A revoked generation is a tombstone, not an unclaimed computer. A new
	// local QR generates a new binding ID; delayed syncs cannot revive the old.
	if (!dev.hasPair && sameBinding) || (dev.hasPair && !sameBinding) {
		h.mu.Unlock()
		writeError(w, 409, "本站手机绑定已更换或解除，请刷新电脑连接")
		return
	}
	if dev.hasPair {
		status := dev.status()
		retained := dev.pairHash != hash
		h.mu.Unlock()
		// Legacy QR credentials cannot be reproduced from their hash. Keep
		// them valid instead of silently replacing a working saved connection.
		writeJSON(w, 200, map[string]any{"device": status, "computerId": in.ComputerID, "bindingId": in.BindingID, "retained": retained})
		return
	}
	h.deletePairAttemptLocked(pairKey(accountID, in.DeviceID))
	h.setPairLocked(accountID, dev, hash, true)
	dev.pairPending = h.cfg.DataDir != ""
	dev.bindingID, dev.phoneHash, dev.pairAttemptID = in.BindingID, in.PhoneHash, ""
	revision := dev.pairRevision
	for sub := range h.accounts[accountID].apps {
		if sub.deviceID == in.DeviceID {
			delete(h.accounts[accountID].apps, sub)
			sub.close()
		}
	}
	h.mu.Unlock()
	status, persisted := h.finishPairIssuance(w, accountID, dev, hash, revision)
	if !persisted {
		return
	}
	writeJSON(w, 200, map[string]any{"device": status, "computerId": in.ComputerID, "bindingId": in.BindingID})
}
