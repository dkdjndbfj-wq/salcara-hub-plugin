package hub

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

const pairTTL = 5 * time.Minute

type pairAttempt struct {
	codeHash [32]byte
	qrHash   [32]byte
	expires  time.Time
	attempts int
}

type pairIdentity struct{ accountID, deviceID string }

func (h *Hub) deletePairAttemptLocked(key string) {
	if attempt := h.pairCodes[key]; attempt != nil {
		delete(h.pairTickets, attempt.qrHash)
		delete(h.pairCodes, key)
	}
}

func (h *Hub) setPairLocked(accountID string, dev *device, hash [32]byte, paired bool) {
	if dev.hasPair {
		delete(h.pairTokens, dev.pairHash)
	}
	dev.pairHash, dev.hasPair = hash, paired
	if paired {
		h.pairTokens[hash] = pairIdentity{accountID, dev.info.DeviceID}
	}
}

func pairTokenHash(r *http.Request) ([32]byte, bool) {
	value := strings.TrimSpace(r.Header.Get("X-Salcara-Pair-Token"))
	if len(value) != 64 {
		return [32]byte{}, false
	}
	if decoded, err := hex.DecodeString(value); err != nil || len(decoded) != 32 {
		return [32]byte{}, false
	}
	return sha256.Sum256([]byte(value)), true
}

func randomPairCode() (string, bool) {
	var raw [5]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", false
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:]), true
}

func randomPairToken() (string, bool) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", false
	}
	return hex.EncodeToString(raw[:]), true
}

func deviceSecret(r *http.Request) (hash [32]byte, ok bool) {
	value := strings.TrimSpace(r.Header.Get("X-Salcara-Device-Secret"))
	if len(value) != 64 {
		return hash, false
	}
	if _, err := hex.DecodeString(value); err != nil {
		return hash, false
	}
	return sha256.Sum256([]byte(value)), true
}

func matchesDeviceSecret(dev *device, r *http.Request) bool {
	if dev == nil || !dev.hasSecret {
		return false
	}
	hash, ok := deviceSecret(r)
	return ok && subtle.ConstantTimeCompare(hash[:], dev.secretHash[:]) == 1
}

func pairKey(accountID, deviceID string) string { return accountID + "\x00" + deviceID }

func (h *Hub) pairedDeviceLocked(accountID string, r *http.Request) *device {
	hash, ok := pairTokenHash(r)
	if !ok {
		return nil
	}
	identity, ok := h.pairTokens[hash]
	if !ok || identity.accountID != accountID {
		return nil
	}
	a := h.accounts[accountID]
	if a == nil {
		return nil
	}
	dev := a.devices[identity.deviceID]
	if dev != nil && !dev.banned && dev.hasPair && subtle.ConstantTimeCompare(hash[:], dev.pairHash[:]) == 1 {
		return dev
	}
	return nil
}

func (h *Hub) pairedDevice(w http.ResponseWriter, r *http.Request, accountID string) (*device, bool) {
	h.mu.Lock()
	dev := h.pairedDeviceLocked(accountID, r)
	h.mu.Unlock()
	if dev == nil {
		writeError(w, http.StatusForbidden, "请先在电脑端生成配对码，并在手机上完成配对")
		return nil, false
	}
	return dev, true
}

func (h *Hub) handlePairStart(w http.ResponseWriter, r *http.Request, accountID string) {
	var input struct {
		DeviceID string `json:"deviceId"`
	}
	if !decodeBody(w, r, maxDefaultBody, &input) {
		return
	}
	if !validID(input.DeviceID) {
		writeError(w, http.StatusBadRequest, "deviceId 无效")
		return
	}
	code, ok := randomPairCode()
	if !ok {
		writeError(w, http.StatusInternalServerError, "无法生成配对码")
		return
	}
	ticket, ok := randomPairToken()
	if !ok {
		writeError(w, 500, "无法生成扫码凭证")
		return
	}
	h.mu.Lock()
	a := h.accounts[accountID]
	var dev *device
	if a != nil {
		dev = a.devices[input.DeviceID]
	}
	if !matchesDeviceSecret(dev, r) || dev.banned {
		h.mu.Unlock()
		writeError(w, http.StatusForbidden, "电脑身份验证失败")
		return
	}
	if dev.conn == nil {
		h.mu.Unlock()
		writeError(w, http.StatusConflict, "电脑尚未连接")
		return
	}
	expires := time.Now().Add(pairTTL)
	key := pairKey(accountID, input.DeviceID)
	h.deletePairAttemptLocked(key)
	attempt := &pairAttempt{codeHash: sha256.Sum256([]byte(code)), qrHash: sha256.Sum256([]byte(ticket)), expires: expires}
	h.pairCodes[key], h.pairTickets[attempt.qrHash] = attempt, key
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"code": code, "ticket": ticket, "expires_at": expires.UnixMilli()})
}

func (h *Hub) handlePairConfirm(w http.ResponseWriter, r *http.Request, accountID string) {
	var input struct {
		Code string `json:"code"`
	}
	if !decodeBody(w, r, maxDefaultBody, &input) {
		return
	}
	code := strings.ToUpper(strings.TrimSpace(input.Code))
	if len(code) != 8 {
		writeError(w, http.StatusBadRequest, "请输入电脑显示的 8 位配对码")
		return
	}
	token, ok := randomPairToken()
	if !ok {
		writeError(w, http.StatusInternalServerError, "无法完成配对")
		return
	}
	hash := sha256.Sum256([]byte(code))
	h.mu.Lock()
	var matched *device
	for key, attempt := range h.pairCodes {
		if !strings.HasPrefix(key, accountID+"\x00") {
			continue
		}
		if time.Now().After(attempt.expires) || attempt.attempts >= 8 {
			h.deletePairAttemptLocked(key)
			continue
		}
		attempt.attempts++
		if subtle.ConstantTimeCompare(hash[:], attempt.codeHash[:]) == 1 {
			deviceID := strings.TrimPrefix(key, accountID+"\x00")
			if a := h.accounts[accountID]; a != nil {
				matched = a.devices[deviceID]
			}
			h.deletePairAttemptLocked(key)
			break
		}
	}
	if matched == nil || matched.conn == nil {
		h.mu.Unlock()
		writeError(w, http.StatusForbidden, "配对码无效或已过期")
		return
	}
	h.setPairLocked(accountID, matched, sha256.Sum256([]byte(token)), true)
	for sub := range h.accountLocked(accountID).apps {
		if sub.deviceID == matched.info.DeviceID {
			delete(h.accountLocked(accountID).apps, sub)
			sub.close()
		}
	}
	status := matched.status()
	h.mu.Unlock()
	h.markDirty()
	writeJSON(w, http.StatusOK, map[string]any{"pair_token": token, "device": status})
}

func (h *Hub) handlePairRevoke(w http.ResponseWriter, r *http.Request, accountID string) {
	var input struct {
		DeviceID string `json:"deviceId"`
	}
	if !decodeBody(w, r, maxDefaultBody, &input) {
		return
	}
	h.mu.Lock()
	a := h.accounts[accountID]
	var dev *device
	if a != nil {
		dev = a.devices[input.DeviceID]
	}
	if !matchesDeviceSecret(dev, r) {
		h.mu.Unlock()
		writeError(w, http.StatusForbidden, "电脑身份验证失败")
		return
	}
	h.revokePairLocked(accountID, dev)
	h.mu.Unlock()
	h.markDirty()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Hub) revokePairLocked(accountID string, dev *device) {
	h.setPairLocked(accountID, dev, [32]byte{}, false)
	h.deletePairAttemptLocked(pairKey(accountID, dev.info.DeviceID))
	for sub := range h.accounts[accountID].apps {
		if sub.deviceID == dev.info.DeviceID {
			delete(h.accounts[accountID].apps, sub)
			sub.close()
		}
	}
}

// A phone may revoke only the exact device authenticated by its own token.
// Deliberately ignore supplied device IDs; never accept an arbitrary target.
func (h *Hub) handleAppPairRevoke(w http.ResponseWriter, r *http.Request, accountID string) {
	h.mu.Lock()
	dev := h.pairedDeviceLocked(accountID, r)
	if dev == nil {
		h.mu.Unlock()
		writeError(w, http.StatusForbidden, "手机绑定已失效")
		return
	}
	h.revokePairLocked(accountID, dev)
	h.mu.Unlock()
	h.markDirty()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
