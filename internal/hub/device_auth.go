package hub

import (
	"crypto/hmac"
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

// Independently enrolled devices own exactly one device ID. Legacy API-key
// accounts may own multiple computers, subject to their existing quota.
func accountAllowsDevice(accountID, deviceID string) bool {
	return !strings.HasPrefix(accountID, "device:") || accountID == deviceAccount(deviceID)
}

func (h *Hub) handleDeviceRegister(w http.ResponseWriter, r *http.Request, _ string) {
	ip := clientIP(r, h.cfg.TrustProxy)
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
	info, _, metadataErr := prepareDeviceMetadata(info)
	if metadataErr != nil {
		h.limiter.fail(ip)
		writeError(w, http.StatusBadRequest, "设备元数据无效或超出限制")
		return
	}
	secretHash, secretOK := deviceSecret(r)
	if !secretOK {
		h.limiter.fail(ip)
		writeError(w, 403, "缺少独立设备凭证")
		return
	}
	accountID := deviceAccount(info.DeviceID)
	if invite := strings.TrimSpace(r.Header.Get("X-Salcara-Bot-Invite")); invite != "" {
		p := BotBindingPackage{Type: botBindingType, Version: botBindingVersion, HubURL: h.cfg.PublicURL, GroupID: strings.TrimSpace(r.Header.Get("X-Salcara-Bot-Group")), Invite: invite, Signature: strings.TrimSpace(r.Header.Get("X-Salcara-Bot-Signature"))}
		if h.cfg.DeploymentMode != "bot" || !validBotPackage(p) {
			h.limiter.fail(ip)
			writeError(w, 403, "Bot 绑定包无效")
			return
		}
		h.mu.Lock()
		g := h.botGroupForPackageLocked(p)
		if g == nil || !g.Computers[info.DeviceID] && len(g.Computers) >= maxBotComputers {
			h.mu.Unlock()
			writeError(w, 403, "Bot 绑定包无效或设备上限已满")
			return
		}
		st, status, deferred := h.registerDeviceLocked(accountID, info, secretHash)
		if status == 0 {
			g.Computers[info.DeviceID] = true
		}
		h.mu.Unlock()
		if status != 0 {
			writeError(w, status, "电脑登记失败")
			return
		}
		if err := h.saveNowError(); err != nil {
			writeError(w, 503, "电脑登记尚未安全保存，请稍后重试")
			return
		}
		if err := h.saveBotStateError(); err != nil {
			writeError(w, 503, "Bot 绑定尚未安全保存，请稍后重试")
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "account": accountID, "device": st, "metadataDeferred": deferred, "groupId": g.ID, "eventScopeRequired": h.cfg.DeploymentMode == "bot", "eventScope": h.bridgeEventScope(accountID, info.DeviceID)})
		return
	}
	if h.cfg.DeploymentMode == "bot" {
		h.mu.Lock()
		a := h.accounts[accountID]
		var existing *device
		if a != nil {
			existing = a.devices[info.DeviceID]
		}
		if existing == nil || !matchesDeviceSecret(existing, r) || existing.banned {
			h.mu.Unlock()
			h.limiter.fail(ip)
			writeError(w, http.StatusForbidden, "Bot 首次登记需要 Salcara Hub 签发的绑定包")
			return
		}
		st, status, deferred := h.registerDeviceLocked(accountID, info, secretHash)
		h.mu.Unlock()
		if status != 0 {
			writeError(w, status, "电脑身份验证失败或本站设备容量已满")
			return
		}
		if deferred {
			writeJSON(w, 200, map[string]any{"ok": true, "account": accountID, "device": st, "metadataDeferred": true, "eventScopeRequired": h.cfg.DeploymentMode == "bot", "eventScope": h.bridgeEventScope(accountID, info.DeviceID)})
			return
		}
		h.markDirty()
		writeJSON(w, 200, map[string]any{"ok": true, "account": accountID, "device": st, "eventScopeRequired": h.cfg.DeploymentMode == "bot", "eventScope": h.bridgeEventScope(accountID, info.DeviceID)})
		return
	}
	h.enrollmentMu.Lock()
	defer h.enrollmentMu.Unlock()
	h.mu.Lock()
	a := h.accounts[accountID]
	exists := a != nil && a.devices[info.DeviceID] != nil
	// A computer that proves its existing secret is never locked out by other
	// clients' failures behind the same address; everyone else is rate limited.
	if !(exists && matchesDeviceSecret(a.devices[info.DeviceID], r)) && h.limiter.blocked(ip) {
		h.mu.Unlock()
		writeError(w, 429, "身份验证失败次数过多，请稍后重试")
		return
	}
	if exists && !matchesDeviceSecret(a.devices[info.DeviceID], r) {
		h.mu.Unlock()
		h.limiter.fail(ip)
		writeError(w, 403, "设备身份验证失败")
		return
	}
	if !exists && h.enrollLimiter.blocked(ip) {
		h.mu.Unlock()
		writeError(w, 429, "设备登记过于频繁或达到本站容量限制，请稍后重试")
		return
	}
	if !exists {
		claimHash, claimOK := enrollmentClaimHash(r.Header.Get("X-Salcara-Enrollment-Claim"))
		if !claimOK || !h.enrollmentClaimValidLocked(claimHash) {
			h.mu.Unlock()
			h.limiter.fail(ip)
			writeError(w, http.StatusForbidden, "首次登记需要有效的一次性设备登记码")
			return
		}
		if status := h.preflightNewDeviceLocked(accountID, info); status != 0 {
			h.mu.Unlock()
			writeError(w, status, "本站设备容量已满或设备身份不可用")
			return
		}
		userID := h.enrollmentOwners[claimHash]
		h.enrollLimiter.fail(ip)
		h.mu.Unlock()
		if err := h.consumeEnrollmentClaim(claimHash); err != nil {
			h.log.Error("device enrollment claim persistence failed", "err", err)
			writeError(w, http.StatusServiceUnavailable, "登记码无法安全提交，请稍后重试")
			return
		}
		h.mu.Lock()
		if err := h.assignEnrolledUserLocked(info.DeviceID, userID); err != nil {
			h.mu.Unlock()
			writeError(w, 503, "用户归属无法保存或用户已停用，请联系管理员")
			return
		}
	}
	// Commit the first secret under the same lock as the existence check. An
	// empty reservation followed by a second handler permits enrollment races.
	st, status, deferred := h.registerDeviceLocked(accountID, info, secretHash)
	h.mu.Unlock()
	if status != 0 {
		writeError(w, status, "电脑身份验证失败或本站设备容量已满")
		return
	}
	if !exists && h.cfg.DataDir != "" {
		if err := h.saveNowError(); err != nil {
			h.log.Error("new device registration persistence failed", "err", err)
			writeError(w, http.StatusServiceUnavailable, "设备登记尚未安全保存，请稍后重试")
			return
		}
	}
	if deferred {
		h.log.Warn("device metadata update deferred: site metadata budget full", "account", accountID, "deviceId", info.DeviceID)
		writeJSON(w, 200, map[string]any{"ok": true, "account": accountID, "device": st, "metadataDeferred": true, "eventScopeRequired": h.cfg.DeploymentMode == "bot", "eventScope": h.bridgeEventScope(accountID, info.DeviceID)})
		return
	}
	h.markDirty()
	h.log.Info("device registered", "account", accountID, "deviceId", info.DeviceID)
	writeJSON(w, 200, map[string]any{"ok": true, "account": accountID, "device": st, "eventScopeRequired": h.cfg.DeploymentMode == "bot", "eventScope": h.bridgeEventScope(accountID, info.DeviceID)})
}

func validDeviceInfo(info Device) bool {
	return validID(info.DeviceID) && len(info.Name) <= 256 && len(info.OS) <= 128 && len(info.Version) <= 128
}

// registerDeviceLocked is shared by legacy and device-only enrollment. Caller
// holds h.mu. Authentication and capacity checks never create orphan accounts.
func (h *Hub) registerDeviceLocked(acct string, info Device, secretHash [32]byte) (DeviceStatus, int, bool) {
	// /bridge/register also accepts device credentials. They must not create
	// siblings in that namespace and bypass /device/register admission limits.
	if !accountAllowsDevice(acct, info.DeviceID) {
		return DeviceStatus{}, http.StatusForbidden, false
	}
	if h.bannedDeviceIDs[info.DeviceID] || h.userDisabledLocked(info.DeviceID) {
		return DeviceStatus{}, 403, false
	}
	a := h.accounts[acct]
	if a == nil && len(h.accounts) >= maxDeviceAccounts {
		return DeviceStatus{}, 429, false
	}
	if a != nil {
		if dev := a.devices[info.DeviceID]; dev != nil && dev.banned {
			return DeviceStatus{}, 403, false
		}
		if dev := a.devices[info.DeviceID]; dev != nil && dev.hasSecret &&
			subtle.ConstantTimeCompare(dev.secretHash[:], secretHash[:]) != 1 {
			return DeviceStatus{}, 403, false
		}
		if a.devices[info.DeviceID] == nil && len(a.devices) >= maxDevices {
			return DeviceStatus{}, 429, false
		}
	}
	if info.Name == "" {
		info.Name = "我的电脑"
	}
	info, charge, err := prepareDeviceMetadata(info)
	if err != nil {
		return DeviceStatus{}, http.StatusBadRequest, false
	}
	projected := h.deviceMetadataBytes + charge
	if a == nil || !a.metadataCharged {
		projected += accountMetadataCost(acct)
	}
	if a != nil {
		if old := a.devices[info.DeviceID]; old != nil {
			projected -= old.metadataBytes
		}
	}
	if projected > h.deviceMetadataLimit {
		// An already registered, authenticated computer must never be locked out
		// of its own Hub because its metadata grew (a new project folder, a tool
		// version string) while the site budget is full: the Bridge re-registers
		// before every stream connection. Keep its previous metadata and charge
		// unchanged and let it connect; only brand-new admissions are refused.
		if a != nil {
			if old := a.devices[info.DeviceID]; old != nil && old.hasSecret {
				return old.status(), 0, true
			}
		}
		return DeviceStatus{}, http.StatusTooManyRequests, false
	}
	// Capacity is checked before allocating the account or replacing any
	// identity, metadata, last-seen timestamp, or stream publication.
	a = h.accountLocked(acct)
	a.metadataCharged = true
	dev := a.devices[info.DeviceID]
	if dev == nil {
		dev = &device{}
		a.devices[info.DeviceID] = dev
	}
	h.deviceMetadataBytes = projected
	dev.metadataBytes = charge
	dev.info, dev.secretHash, dev.hasSecret, dev.lastSeen = info, secretHash, true, nowMs()
	a.broadcastDeviceLocked(dev)
	return dev.status(), 0, false
}

func (h *Hub) deviceOnlyAuth(w http.ResponseWriter, r *http.Request, path, ip string) (string, bool) {
	if strings.HasPrefix(path, "/bot/") || strings.HasPrefix(path, "/app/") {
		h.mu.Lock()
		if g := h.botGroupForTokenLocked(r); g != nil {
			h.mu.Unlock()
			return "bot:" + g.ID, true
		}
		h.mu.Unlock()
	}
	// Valid credentials are checked first: failures by someone else behind the
	// same proxy address must never lock out paired phones and computers.
	if strings.HasPrefix(path, "/bridge/") {
		id := r.Header.Get("X-Salcara-Device-Id")
		accountID := deviceAccount(id)
		h.mu.Lock()
		a := h.accounts[accountID]
		valid := validID(id) && a != nil && a.devices[id] != nil && !a.devices[id].banned && !h.userDisabledLocked(id) && matchesDeviceSecret(a.devices[id], r)
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
			if found && h.managedPairUserDisabledLocked(identity.accountID, r) {
				h.mu.Unlock()
				writeManagedUserDisabled(w)
				return "", false
			}
			if found && h.pairedDeviceLocked(identity.accountID, r) != nil {
				h.mu.Unlock()
				return identity.accountID, true
			}
		}
		h.mu.Unlock()
		if h.rejectUnpersistedPairRevocation(w, path == "/app/pair/revoke") {
			return "", false
		}
	}
	if h.limiter.blocked(ip) {
		writeError(w, 429, "身份验证失败次数过多")
		return "", false
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
		PhoneID  string `json:"phoneId"`
	}
	if !decodeBody(w, r, maxDefaultBody, &in) {
		return
	}
	decoded, err := hex.DecodeString(in.Ticket)
	phoneHash := ""
	if in.PhoneID != "" {
		if !validPairHash(in.PhoneID) {
			writeError(w, 400, "手机配对身份无效")
			return
		}
		hash := sha256.Sum256([]byte(in.PhoneID))
		phoneHash = hex.EncodeToString(hash[:])
	}
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
	var bindingID string
	var computerID string
	var attemptID string
	if key, ok := h.pairTickets[hash]; ok {
		attempt := h.pairCodes[key]
		parts := strings.SplitN(key, "\x00", 2)
		if attempt != nil && len(parts) == 2 && parts[1] == in.DeviceID {
			// An attempt carrying a phone hash is restricted to that
			// installation. After a local revoke the desktop starts a fresh
			// binding generation with an empty hash; that is a legitimate rebind
			// window, but it must not be usable while the device is still paired.
			activePair := false
			if a := h.accounts[parts[0]]; a != nil {
				if current := a.devices[in.DeviceID]; current != nil {
					activePair = current.hasPair
				}
			}
			phoneMismatch := attempt.phoneHash != "" && (phoneHash == "" || subtle.ConstantTimeCompare([]byte(phoneHash), []byte(attempt.phoneHash)) != 1)
			phoneMissing := attempt.bindingID != "" && phoneHash == ""
			emptyHashActivePair := attempt.phoneHash == "" && activePair
			if attempt.bindingID != "" && (phoneMissing || phoneMismatch || emptyHashActivePair) {
				h.mu.Unlock()
				h.limiter.fail(ip)
				writeError(w, 403, "这台电脑已绑定另一台手机，请先在电脑解除绑定")
				return
			}
			if time.Now().After(attempt.expires) {
				h.deletePairAttemptLocked(key)
			} else if a := h.accounts[parts[0]]; a != nil {
				matched = a.devices[in.DeviceID]
				if matched != nil && matched.conn != nil && !matched.banned && !h.userDisabledLocked(matched.info.DeviceID) && accountAllowsDevice(parts[0], matched.info.DeviceID) {
					bindingID = attempt.bindingID
					computerID = attempt.computerID
					attemptID = attempt.attemptID
					if attempt.pairSeed != "" {
						mac := hmac.New(sha256.New, []byte(attempt.pairSeed))
						_, _ = mac.Write([]byte(phoneHash))
						token = hex.EncodeToString(mac.Sum(nil))
					}
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
	tokenHash := sha256.Sum256([]byte(token))
	h.setPairLocked(accountID, matched, tokenHash, true)
	matched.pairPending = h.cfg.DataDir != ""
	matched.bindingID, matched.phoneHash = bindingID, phoneHash
	matched.pairAttemptID = attemptID
	revision := matched.pairRevision
	for sub := range h.accounts[accountID].apps {
		if sub.deviceID == in.DeviceID {
			delete(h.accounts[accountID].apps, sub)
			sub.close()
		}
	}
	h.mu.Unlock()
	status, persisted := h.finishPairIssuance(w, accountID, matched, tokenHash, revision)
	if !persisted {
		return
	}
	writeJSON(w, 200, map[string]any{"pair_token": token, "device": status, "computerId": computerID})
}
