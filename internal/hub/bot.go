package hub

// Bot groups are a constrained relay binding. They deliberately sit beside
// the existing per-device accounts: a phone gets a group credential, while
// every computer keeps its own device secret, event ring, and command scope.

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	botBindingType    = "salcara-hub-bot-binding"
	botBindingVersion = 1
	botGroupFile      = "bot-groups.json"
	maxBotComputers   = 8
)

// BotBindingPackage is the only object users copy between the cloud, desktop,
// and phone. It contains a scoped invitation, never an admin key or device
// secret. The invite remains valid until its group is revoked.
type BotBindingPackage struct {
	Type      string `json:"type"`
	Version   int    `json:"version"`
	HubURL    string `json:"hubUrl"`
	GroupID   string `json:"groupId"`
	Invite    string `json:"invite"`
	Signature string `json:"signature"`
}

type botGroup struct {
	ID               string
	InviteHash       string
	SignatureKeyHash string
	Revoked          bool
	PhoneIDHash      string
	PhoneTokenHash   string
	Computers        map[string]bool
	CreatedAt        int64
}

type persistedBotGroup struct {
	ID         string `json:"id"`
	InviteHash string `json:"inviteHash"`
	// Kept for reading pre-HMAC Bot state. New groups use the state signing key.
	SignatureKeyHash string   `json:"signatureKeyHash,omitempty"`
	Revoked          bool     `json:"revoked,omitempty"`
	PhoneIDHash      string   `json:"phoneIdHash,omitempty"`
	PhoneTokenHash   string   `json:"phoneTokenHash,omitempty"`
	Computers        []string `json:"computers,omitempty"`
	CreatedAt        int64    `json:"createdAt"`
}

type persistedBotState struct {
	Version    int                 `json:"version"`
	SigningKey string              `json:"signingKey,omitempty"`
	Groups     []persistedBotGroup `json:"groups"`
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func hashText(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:])
}

func validHex(value string, bytesLen int) bool {
	if len(value) != bytesLen*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validBotPackage(p BotBindingPackage) bool {
	return p.Type == botBindingType && p.Version == botBindingVersion && validID(p.GroupID) && validHex(p.Invite, 32) && validHex(p.Signature, 32)
}

func validateBotHubURL(raw, expected string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || strings.TrimRight(u.Path, "/") != defaultPrefix {
		return false
	}
	if expected != "" {
		return strings.TrimRight(raw, "/") == strings.TrimRight(expected, "/")
	}
	return true
}

func (h *Hub) loadBotState() error {
	if h.cfg.DeploymentMode != "bot" {
		return nil
	}
	if h.cfg.DataDir == "" {
		return nil
	}
	path := filepath.Join(h.cfg.DataDir, botGroupFile)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return errors.New("invalid bot-groups.json")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var st persistedBotState
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&st); err != nil || st.Version != 1 || d.Decode(new(any)) != io.EOF {
		return errors.New("invalid bot-groups.json")
	}
	var signingKey []byte
	if st.SigningKey != "" {
		var decodeErr error
		signingKey, decodeErr = hex.DecodeString(st.SigningKey)
		if decodeErr != nil || len(signingKey) != 32 {
			return errors.New("invalid bot signing key")
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.botSigningKey = signingKey
	for _, p := range st.Groups {
		if !validID(p.ID) || !validHex(p.InviteHash, 32) || (p.SignatureKeyHash != "" && !validHex(p.SignatureKeyHash, 32)) || p.CreatedAt <= 0 || len(p.Computers) > maxBotComputers {
			return errors.New("invalid bot group record")
		}
		g := &botGroup{ID: p.ID, InviteHash: p.InviteHash, SignatureKeyHash: p.SignatureKeyHash, Revoked: p.Revoked, PhoneIDHash: p.PhoneIDHash, PhoneTokenHash: p.PhoneTokenHash, CreatedAt: p.CreatedAt, Computers: map[string]bool{}}
		if g.PhoneIDHash != "" && !validHex(g.PhoneIDHash, 32) || g.PhoneTokenHash != "" && !validHex(g.PhoneTokenHash, 32) {
			return errors.New("invalid bot phone credential")
		}
		for _, id := range p.Computers {
			if !validID(id) || g.Computers[id] {
				return errors.New("invalid bot computer membership")
			}
			g.Computers[id] = true
		}
		if _, exists := h.botGroups[g.ID]; exists {
			return errors.New("duplicate bot group")
		}
		h.botGroups[g.ID] = g
	}
	return nil
}

func (h *Hub) botSnapshot() persistedBotState {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := persistedBotState{Version: 1, SigningKey: hex.EncodeToString(h.botSigningKey)}
	for _, g := range h.botGroups {
		p := persistedBotGroup{ID: g.ID, InviteHash: g.InviteHash, SignatureKeyHash: g.SignatureKeyHash, Revoked: g.Revoked, PhoneIDHash: g.PhoneIDHash, PhoneTokenHash: g.PhoneTokenHash, CreatedAt: g.CreatedAt}
		for id := range g.Computers {
			p.Computers = append(p.Computers, id)
		}
		st.Groups = append(st.Groups, p)
	}
	return st
}

func (h *Hub) ensureBotSigningKey() error {
	if h.cfg.DeploymentMode != "bot" {
		return nil
	}
	h.mu.Lock()
	if len(h.botSigningKey) == 0 {
		key, err := randomHex(32)
		if err != nil {
			h.mu.Unlock()
			return err
		}
		h.botSigningKey, err = hex.DecodeString(key)
		if err != nil {
			h.mu.Unlock()
			return err
		}
	}
	h.mu.Unlock()
	return h.saveBotStateError()
}

func botSignaturePayload(p BotBindingPackage) []byte {
	// Keep field order and representation stable so packages can be checked
	// across Hub restarts and by future clients without accepting arbitrary JSON.
	return []byte(p.Type + "\x00" + strconv.Itoa(p.Version) + "\x00" + p.HubURL + "\x00" + p.GroupID + "\x00" + p.Invite)
}

func (h *Hub) signBotPackage(p BotBindingPackage) string {
	h.mu.Lock()
	key := append([]byte(nil), h.botSigningKey...)
	h.mu.Unlock()
	m := hmac.New(sha256.New, key)
	_, _ = m.Write(botSignaturePayload(p))
	return hex.EncodeToString(m.Sum(nil))
}

func (h *Hub) saveBotStateError() error {
	if h.cfg.DataDir == "" {
		return nil
	}
	h.saveMu.Lock()
	defer h.saveMu.Unlock()
	b, err := json.Marshal(h.botSnapshot())
	if err != nil {
		return err
	}
	return writeFileAtomic(h.cfg.DataDir+"/"+botGroupFile, b, 0o600)
}

func (h *Hub) CreateBotBindingPackage() (BotBindingPackage, error) {
	if h.cfg.DeploymentMode != "bot" {
		return BotBindingPackage{}, errors.New("bot bindings require bot deployment")
	}
	invite, err := randomHex(32)
	if err != nil {
		return BotBindingPackage{}, err
	}
	groupID, err := randomHex(16)
	if err != nil {
		return BotBindingPackage{}, err
	}
	p := BotBindingPackage{Type: botBindingType, Version: botBindingVersion, HubURL: strings.TrimRight(h.cfg.PublicURL, "/"), GroupID: groupID, Invite: invite}
	p.Signature = h.signBotPackage(p)
	// The invite is the scoped bearer credential. The package signature is a
	// Hub-issued HMAC over the complete package metadata and is verified below.
	g := &botGroup{ID: groupID, InviteHash: hashText(invite), CreatedAt: time.Now().UnixMilli(), Computers: map[string]bool{}}
	h.mu.Lock()
	h.botGroups[groupID] = g
	h.mu.Unlock()
	if err := h.saveBotStateError(); err != nil {
		h.mu.Lock()
		delete(h.botGroups, groupID)
		h.mu.Unlock()
		return BotBindingPackage{}, err
	}
	return p, nil
}

func (h *Hub) RevokeBotGroup(groupID string) error {
	h.mu.Lock()
	g := h.botGroups[groupID]
	if g == nil {
		h.mu.Unlock()
		return errors.New("bot group not found")
	}
	g.Revoked = true
	// Invite revocation stops new bindings only. Existing phone and computer
	// credentials remain valid until their own explicit revocation.
	h.mu.Unlock()
	return h.saveBotStateError()
}

func (h *Hub) BotGroupSummaries() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]map[string]any, 0, len(h.botGroups))
	for _, g := range h.botGroups {
		out = append(out, map[string]any{"groupId": g.ID, "revoked": g.Revoked, "computers": len(g.Computers), "phoneBound": g.PhoneTokenHash != "", "createdAt": g.CreatedAt})
	}
	return out
}

func (h *Hub) botGroupForPackageLocked(p BotBindingPackage) *botGroup {
	g := h.botGroups[p.GroupID]
	if g == nil || g.Revoked || hashText(p.Invite) != g.InviteHash {
		return nil
	}
	if !h.verifyBotPackageSignatureLocked(p) && (g.SignatureKeyHash == "" || hashText(p.Signature) != g.SignatureKeyHash) {
		return nil
	}
	return g
}

func (h *Hub) verifyBotPackageSignatureLocked(p BotBindingPackage) bool {
	if len(h.botSigningKey) == 0 || !validHex(p.Signature, 32) {
		return false
	}
	m := hmac.New(sha256.New, h.botSigningKey)
	_, _ = m.Write(botSignaturePayload(p))
	expected := m.Sum(nil)
	actual, err := hex.DecodeString(p.Signature)
	return err == nil && hmac.Equal(actual, expected)
}

func decodeBotPackage(r *http.Request) (BotBindingPackage, bool) {
	dec := json.NewDecoder(io.LimitReader(r.Body, 16<<10))
	dec.DisallowUnknownFields()
	var p BotBindingPackage
	if dec.Decode(&p) != nil || dec.Decode(new(any)) != io.EOF || !validBotPackage(p) {
		return BotBindingPackage{}, false
	}
	return p, true
}

func decodeStrictBotBody(r *http.Request, out any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxDefaultBody))
	dec.DisallowUnknownFields()
	return dec.Decode(out) == nil && dec.Decode(new(any)) == io.EOF
}

func botTokenHash(r *http.Request) ([32]byte, bool) {
	v := strings.TrimSpace(r.Header.Get("X-Salcara-Bot-Token"))
	if !validHex(v, 32) {
		return [32]byte{}, false
	}
	return sha256.Sum256([]byte(v)), true
}

func (h *Hub) botGroupForTokenLocked(r *http.Request) *botGroup {
	hash, ok := botTokenHash(r)
	if !ok {
		return nil
	}
	for _, g := range h.botGroups {
		if g.PhoneTokenHash != "" && subtleConstantTimeEqual(hashTextBytes(hash), g.PhoneTokenHash) {
			return g
		}
	}
	return nil
}

func hashTextBytes(h [32]byte) []byte                 { return []byte(hex.EncodeToString(h[:])) }
func subtleConstantTimeEqual(a []byte, b string) bool { return hmac.Equal(a, []byte(b)) }

func (h *Hub) botGroupForRequestLocked(r *http.Request) *botGroup {
	return h.botGroupForTokenLocked(r)
}

func (h *Hub) handleBotComputerBind(w http.ResponseWriter, r *http.Request, _ string) {
	var in struct {
		Binding BotBindingPackage `json:"binding"`
		Device  Device            `json:"device"`
	}
	if !decodeStrictBotBody(r, &in) || !validBotPackage(in.Binding) || !validDeviceInfo(in.Device) || !validateBotHubURL(in.Binding.HubURL, h.cfg.PublicURL) {
		writeError(w, 400, "绑定包或设备信息无效")
		return
	}
	id := strings.TrimSpace(r.Header.Get("X-Salcara-Device-Id"))
	secret, ok := deviceSecret(r)
	if !ok || id == "" || id != in.Device.DeviceID {
		writeError(w, 403, "缺少独立设备凭证")
		return
	}
	h.mu.Lock()
	g := h.botGroupForPackageLocked(in.Binding)
	if g == nil {
		h.mu.Unlock()
		writeError(w, 403, "绑定包无效、已撤销或已过期")
		return
	}
	if !g.Computers[id] && len(g.Computers) >= maxBotComputers {
		h.mu.Unlock()
		writeError(w, 409, "该 Bot 组已达到 8 台电脑上限")
		return
	}
	acct := deviceAccount(id)
	status, code, _ := h.registerDeviceLocked(acct, in.Device, secret)
	if code != 0 {
		h.mu.Unlock()
		writeError(w, code, "电脑登记失败")
		return
	}
	g.Computers[id] = true
	h.mu.Unlock()
	if err := h.saveNowError(); err != nil {
		writeError(w, 503, "电脑登记尚未保存，请重试")
		return
	}
	if err := h.saveBotStateError(); err != nil {
		writeError(w, 503, "Bot 绑定尚未保存，请重试")
		return
	}
	h.mu.Lock()
	h.notifyBotScopeChangedLocked(g)
	h.mu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true, "groupId": g.ID, "device": status, "eventScopeRequired": h.cfg.DeploymentMode == "bot", "eventScope": h.bridgeEventScope(acct, id)})
}

func (h *Hub) handleBotPhoneBind(w http.ResponseWriter, r *http.Request, _ string) {
	var in struct {
		Binding BotBindingPackage `json:"binding"`
		PhoneID string            `json:"phoneId"`
	}
	if !decodeStrictBotBody(r, &in) || !validBotPackage(in.Binding) || !validHex(in.PhoneID, 32) || !validateBotHubURL(in.Binding.HubURL, h.cfg.PublicURL) {
		writeError(w, 400, "绑定包或手机身份无效")
		return
	}
	token, err := randomHex(32)
	if err != nil {
		writeError(w, 500, "无法生成手机凭据")
		return
	}
	hash := sha256.Sum256([]byte(token))
	phoneHash := hashText(in.PhoneID)
	h.mu.Lock()
	g := h.botGroupForPackageLocked(in.Binding)
	if g == nil {
		h.mu.Unlock()
		writeError(w, 403, "绑定包无效、已撤销或已过期")
		return
	}
	if g.PhoneTokenHash != "" {
		h.mu.Unlock()
		writeError(w, 409, "该 Bot 组已绑定手机")
		return
	}
	g.PhoneIDHash, g.PhoneTokenHash = phoneHash, hex.EncodeToString(hash[:])
	devices := h.botDevicesLocked(g)
	h.mu.Unlock()
	if err := h.saveBotStateError(); err != nil {
		// Do not consume the group's single phone slot when the durable commit
		// failed. Keep a concurrent revoke or replacement untouched.
		h.mu.Lock()
		if current := h.botGroups[g.ID]; current == g && current.PhoneIDHash == phoneHash && current.PhoneTokenHash == hex.EncodeToString(hash[:]) {
			current.PhoneIDHash, current.PhoneTokenHash = "", ""
		}
		h.mu.Unlock()
		writeError(w, 503, "手机绑定尚未保存，请重试")
		return
	}
	h.mu.Lock()
	h.notifyBotScopeChangedLocked(g)
	h.mu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true, "groupId": g.ID, "botToken": token, "devices": devices})
}

func (h *Hub) botDevicesLocked(g *botGroup) []DeviceStatus {
	out := make([]DeviceStatus, 0, len(g.Computers))
	for id := range g.Computers {
		if a := h.accounts[deviceAccount(id)]; a != nil {
			if d := a.devices[id]; d != nil && !d.banned && !h.userDisabledLocked(id) {
				out = append(out, d.status())
			}
		}
	}
	return out
}

func (h *Hub) handleBotPhoneRevoke(w http.ResponseWriter, r *http.Request, _ string) {
	h.mu.Lock()
	g := h.botGroupForTokenLocked(r)
	if g == nil {
		h.mu.Unlock()
		writeError(w, 403, "Bot 手机凭据已失效")
		return
	}
	g.PhoneIDHash, g.PhoneTokenHash = "", ""
	h.notifyBotScopeChangedLocked(g)
	h.mu.Unlock()
	if err := h.saveBotStateError(); err != nil {
		writeError(w, 503, "撤销状态尚未保存，请重试")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (h *Hub) botAccountAndDeviceLocked(acct string, r *http.Request, deviceID string) (string, *device) {
	if !strings.HasPrefix(acct, "bot:") {
		a := h.accounts[acct]
		if a == nil {
			return "", nil
		}
		return acct, a.devices[deviceID]
	}
	g := h.botGroupForTokenLocked(r)
	if g == nil || acct != "bot:"+g.ID || !g.Computers[deviceID] {
		return "", nil
	}
	a := h.accounts[deviceAccount(deviceID)]
	if a == nil {
		return "", nil
	}
	dev := a.devices[deviceID]
	if dev == nil || dev.banned || h.userDisabledLocked(deviceID) {
		return "", nil
	}
	return deviceAccount(deviceID), dev
}

// The source binding is derived from the authenticated event generation. That
// generation includes device credentials and current Bot phone tokens; adding
// the group prevents two groups sharing a computer from sharing source grants.
// It remains stable across restart, but phone revoke/rebind rotates it.
func (h *Hub) botSourceBindingLocked(acct string, dev *device) string {
	if dev == nil || dev.banned || h.userDisabledLocked(dev.info.DeviceID) || h.cfg.DeploymentMode != "bot" || !strings.HasPrefix(acct, "bot:") {
		return ""
	}
	groupID := strings.TrimPrefix(acct, "bot:")
	g := h.botGroups[groupID]
	if g == nil || !g.Computers[dev.info.DeviceID] || g.PhoneTokenHash == "" || g.PhoneIDHash == "" {
		return ""
	}
	scope := h.durableScopeLocked(deviceAccount(dev.info.DeviceID), dev)
	return shaHex("salcara-bot-source-v1\x00" + groupID + "\x00" + scope)
}

func (h *Hub) appAuthorizedDeviceLocked(acct string, r *http.Request, deviceID string) bool {
	if h.userDisabledLocked(deviceID) {
		return false
	}
	if strings.HasPrefix(acct, "bot:") {
		_, dev := h.botAccountAndDeviceLocked(acct, r, deviceID)
		return dev != nil && !dev.banned
	}
	a := h.accounts[acct]
	// Receipt and transient-read dispatch retain the resolved device account.
	// Recheck the original Bot token against that exact device, not QR state.
	if acct == deviceAccount(deviceID) && a != nil && a.devices[deviceID] != nil {
		if g := h.botGroupForTokenLocked(r); g != nil && g.Computers[deviceID] {
			return !a.devices[deviceID].banned
		}
	}
	return a != nil && a.devices[deviceID] != nil && h.pairedDeviceLocked(acct, r) == a.devices[deviceID]
}
