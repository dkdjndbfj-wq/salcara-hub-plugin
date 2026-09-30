package hub

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

// Device-only enrollment never authenticates against a model API. Each computer
// owns a separate namespace, and phone tokens can authorize app routes only.
func deviceAccount(id string) string {
	hash := sha256.Sum256([]byte(id))
	return "device:" + hex.EncodeToString(hash[:])
}

func (h *Hub) handleDeviceRegister(w http.ResponseWriter, r *http.Request, _ string) {
	ip := clientIP(r, h.cfg.TrustProxy)
	if h.limiter.blocked(ip) {
		writeError(w, 429, "身份验证失败次数过多，请稍后重试")
		return
	}
	var info Device
	if !decodeBody(w, r, maxDeviceBody, &info) {
		return
	}
	info.DeviceID = strings.TrimSpace(info.DeviceID)
	if !validDeviceInfo(info) || r.Header.Get("X-Salcara-Device-Id") != info.DeviceID {
		h.limiter.fail(ip)
		writeError(w, 400, "设备编号无效")
		return
	}
	secretHash, secretOK := deviceSecret(r)
	if !secretOK {
		h.limiter.fail(ip)
		writeError(w, 403, "缺少独立设备凭证")
		return
	}
	accountID := deviceAccount(info.DeviceID)
	h.mu.Lock()
	a := h.accounts[accountID]
	exists := a != nil && a.devices[info.DeviceID] != nil
	if exists && !matchesDeviceSecret(a.devices[info.DeviceID], r) {
		h.mu.Unlock()
		h.limiter.fail(ip)
		writeError(w, 403, "设备身份验证失败")
		return
	}
	if !exists && (len(h.accounts) >= maxDeviceAccounts || h.enrollLimiter.blocked(ip)) {
		h.mu.Unlock()
		writeError(w, 429, "设备登记过于频繁或达到本站容量限制，请稍后重试")
		return
	}
	if !exists {
		h.enrollLimiter.fail(ip)
	}
	// Commit the first secret under the same lock as the existence check. An
	// empty reservation followed by a second handler permits enrollment races.
	st, status := h.registerDeviceLocked(accountID, info, secretHash)
	h.mu.Unlock()
	if status != 0 {
		writeError(w, status, "电脑身份验证失败或本站设备容量已满")
		return
	}
	h.markDirty()
	h.log.Info("device registered", "account", accountID, "deviceId", info.DeviceID)
	writeJSON(w, 200, map[string]any{"ok": true, "account": accountID, "device": st})
}

func validDeviceInfo(info Device) bool {
	return validID(info.DeviceID) && len(info.Name) <= 256 && len(info.OS) <= 128 && len(info.Version) <= 128
}

// registerDeviceLocked is shared by legacy and device-only enrollment. Caller
// holds h.mu. Authentication and capacity checks never create orphan accounts.
func (h *Hub) registerDeviceLocked(acct string, info Device, secretHash [32]byte) (DeviceStatus, int) {
	if h.bannedDeviceIDs[info.DeviceID] {
		return DeviceStatus{}, 403
	}
	a := h.accounts[acct]
	if a == nil && len(h.accounts) >= maxDeviceAccounts {
		return DeviceStatus{}, 429
	}
	if a != nil {
		if dev := a.devices[info.DeviceID]; dev != nil && dev.banned {
			return DeviceStatus{}, 403
		}
		if dev := a.devices[info.DeviceID]; dev != nil && dev.hasSecret &&
			subtle.ConstantTimeCompare(dev.secretHash[:], secretHash[:]) != 1 {
			return DeviceStatus{}, 403
		}
		if a.devices[info.DeviceID] == nil && len(a.devices) >= maxDevices {
			return DeviceStatus{}, 429
		}
	}
	a = h.accountLocked(acct)
	dev := a.devices[info.DeviceID]
	if dev == nil {
		dev = &device{}
		a.devices[info.DeviceID] = dev
	}
	if info.Name == "" {
		info.Name = "我的电脑"
	}
	dev.info, dev.secretHash, dev.hasSecret, dev.lastSeen = info, secretHash, true, nowMs()
	a.broadcastDeviceLocked(dev)
	return dev.status(), 0
}

func (h *Hub) deviceOnlyAuth(w http.ResponseWriter, r *http.Request, path, ip string) (string, bool) {
	if h.limiter.blocked(ip) {
		writeError(w, 429, "身份验证失败次数过多")
		return "", false
	}
	if strings.HasPrefix(path, "/bridge/") {
		id := r.Header.Get("X-Salcara-Device-Id")
		accountID := deviceAccount(id)
		h.mu.Lock()
		a := h.accounts[accountID]
		valid := validID(id) && a != nil && a.devices[id] != nil && !a.devices[id].banned && matchesDeviceSecret(a.devices[id], r)
		h.mu.Unlock()
		if valid {
			return accountID, true
		}
	}
	if strings.HasPrefix(path, "/app/") {
		// Never let this token authenticate bridge registration, admin or billing.
		h.mu.Lock()
		if hash, ok := pairTokenHash(r); ok {
			identity, found := h.pairTokens[hash]
			if found && h.pairedDeviceLocked(identity.accountID, r) != nil {
				h.mu.Unlock()
				return identity.accountID, true
			}
		}
		h.mu.Unlock()
	}
	h.limiter.fail(ip)
	writeError(w, 403, "设备凭证或手机绑定已失效，请重新扫码绑定")
	return "", false
}

func (h *Hub) handleQRPair(w http.ResponseWriter, r *http.Request, _ string) {
	ip := clientIP(r, h.cfg.TrustProxy)
	if h.limiter.blocked(ip) {
		writeError(w, 429, "配对失败次数过多，请稍后再试")
		return
	}
	var in struct {
		DeviceID string `json:"deviceId"`
		Ticket   string `json:"ticket"`
	}
	if !decodeBody(w, r, maxDefaultBody, &in) {
		return
	}
	decoded, err := hex.DecodeString(in.Ticket)
	if !validID(in.DeviceID) || err != nil || len(decoded) != 32 {
		h.limiter.fail(ip)
		writeError(w, 403, "二维码无效或已过期")
		return
	}
	hash := sha256.Sum256([]byte(in.Ticket))
	token, tokenOK := randomPairToken()
	if !tokenOK {
		writeError(w, 500, "无法生成设备会话")
		return
	}
	h.mu.Lock()
	var accountID string
	var matched *device
	if key, ok := h.pairTickets[hash]; ok {
		attempt := h.pairCodes[key]
		parts := strings.SplitN(key, "\x00", 2)
		if attempt != nil && len(parts) == 2 && parts[1] == in.DeviceID {
			if time.Now().After(attempt.expires) {
				h.deletePairAttemptLocked(key)
			} else if a := h.accounts[parts[0]]; a != nil {
				matched = a.devices[in.DeviceID]
				if matched != nil && matched.conn != nil {
					accountID = parts[0]
					h.deletePairAttemptLocked(key) // atomic, single-use claim
				}
			}
		}
	}
	if matched == nil || accountID == "" {
		h.mu.Unlock()
		h.limiter.fail(ip)
		writeError(w, 403, "二维码无效、已使用、已过期或电脑已离线")
		return
	}
	h.setPairLocked(accountID, matched, sha256.Sum256([]byte(token)), true)
	for sub := range h.accounts[accountID].apps {
		if sub.deviceID == in.DeviceID {
			delete(h.accounts[accountID].apps, sub)
			sub.close()
		}
	}
	status := matched.status()
	h.mu.Unlock()
	h.markDirty()
	writeJSON(w, 200, map[string]any{"pair_token": token, "device": status})
}
