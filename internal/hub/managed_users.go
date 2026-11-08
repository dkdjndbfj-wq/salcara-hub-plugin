package hub

import (
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const managedUsersFile = "managed-users.json"
const maxManagedUsersBytes = 8 << 20

// Managed users are administrator-owned profiles, not client-supplied identities.
// Device credentials and phone pairing remain the authentication boundary.
type ManagedUser struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Note      string         `json:"note"`
	Disabled  bool           `json:"disabled"`
	Revision  string         `json:"revision"`
	CreatedAt int64          `json:"created_at"`
	UpdatedAt int64          `json:"updated_at"`
	Station   *StationPolicy `json:"station,omitempty"`
}

type managedAssignment struct {
	UserID      string `json:"user_id"`
	Revision    string `json:"revision"`
	RevokedPair string `json:"revoked_pair,omitempty"`
}

type managedUserState struct {
	Version  int                          `json:"version"`
	Revision string                       `json:"revision"`
	Users    []ManagedUser                `json:"users"`
	Devices  map[string]managedAssignment `json:"devices"`
}

type ManagedUserChange struct {
	Action           string         `json:"action"`
	ExpectedRevision string         `json:"expected_revision"`
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	Note             string         `json:"note"`
	Disabled         bool           `json:"disabled"`
	Station          *StationPolicy `json:"station"`
	DeviceRef        string         `json:"device_ref"`
	Reason           string         `json:"reason"`
	Confirm          bool           `json:"confirm"`
}

var ErrManagedUserConflict = errors.New("用户资料已更新，请刷新后再操作")
var ErrManagedUserUnavailable = errors.New("用户变更未保存，请检查数据目录或稍后重试")

func validManagedText(s string, max int, required bool) bool {
	return utf8.ValidString(s) && len(s) <= max && s == strings.TrimSpace(s) && (!required || s != "") && strings.IndexFunc(s, unicode.IsControl) < 0
}

func validateManagedState(s *managedUserState) error {
	if s.Version != 1 || !policyID(s.Revision) || len(s.Users) > 128 || s.Devices == nil || len(s.Devices) > 10000 {
		return errors.New("用户资料数量或版本无效")
	}
	ids, names := map[string]bool{}, map[string]bool{}
	for i := range s.Users {
		u := &s.Users[i]
		name := strings.ToLower(u.Name)
		if !policyID(u.ID) || ids[u.ID] || names[name] || !policyID(u.Revision) || !validManagedText(u.Name, 128, true) || !validManagedText(u.Note, 512, false) || u.CreatedAt <= 0 || u.UpdatedAt < u.CreatedAt {
			return errors.New("用户名称、备注或记录无效（名称不可重复）")
		}
		if u.Station != nil {
			if u.Station.Version != 2 || u.Station.normalize() != nil {
				return errors.New("单独授权需要有效的 v2 来源名单")
			}
			raw, err := json.Marshal(u.Station)
			if err != nil || len(raw) > 32<<10 {
				return errors.New("单个用户的来源配置不能超过 32 KiB")
			}
		}
		ids[u.ID], names[name] = true, true
	}
	for deviceID, assignment := range s.Devices {
		if !validID(deviceID) || !policyID(assignment.Revision) || assignment.UserID != "" && !ids[assignment.UserID] || assignment.RevokedPair != "" && !hexHash(assignment.RevokedPair) {
			return errors.New("用户设备归属无效")
		}
	}
	return nil
}

func (h *Hub) loadManagedUsers() error {
	h.managedUsers = managedUserState{Version: 1, Revision: "users-initial", Users: []ManagedUser{}, Devices: map[string]managedAssignment{}}
	if h.cfg.DataDir == "" {
		return nil
	}
	path := filepath.Join(h.cfg.DataDir, managedUsersFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxManagedUsersBytes {
		return errors.New("用户资料文件不可用")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return errors.New("用户资料文件在读取时发生变化")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxManagedUsersBytes+1))
	var state managedUserState
	if err != nil || len(raw) > maxManagedUsersBytes || decodeStoredRecord(raw, &state) != nil {
		return errors.New("用户资料文件格式无效")
	}
	if err := validateManagedState(&state); err != nil {
		return err
	}
	h.managedUsers = state
	for accountID, a := range h.accounts {
		for id, d := range a.devices {
			retired := state.Devices[id].RevokedPair
			if d.hasPair && retired != "" && retired == hex.EncodeToString(d.pairHash[:]) {
				h.revokePairLocked(accountID, d)
				h.markDirty()
			}
		}
	}
	return nil
}

func (h *Hub) managedUserLocked(id string) *ManagedUser {
	for i := range h.managedUsers.Users {
		if h.managedUsers.Users[i].ID == id {
			return &h.managedUsers.Users[i]
		}
	}
	return nil
}

func (h *Hub) userDisabledLocked(deviceID string) bool {
	assignment := h.managedUsers.Devices[deviceID]
	if assignment.UserID == "" {
		return false
	}
	u := h.managedUserLocked(assignment.UserID)
	return u == nil || u.Disabled
}

func (h *Hub) managedPairAllowedLocked(d *device) bool {
	if d == nil || h.userDisabledLocked(d.info.DeviceID) {
		return false
	}
	retired := h.managedUsers.Devices[d.info.DeviceID].RevokedPair
	return retired == "" || retired != hex.EncodeToString(d.pairHash[:])
}

// Classify only a proven, retained phone credential as temporarily disabled.
// A revoked/reassigned/unknown credential still receives the usual 403. Older
// phones retire saved pairing on 401/403, so suspension uses a distinct 423.
func (h *Hub) managedPairUserDisabledLocked(accountID string, r *http.Request) bool {
	hash, ok := pairTokenHash(r)
	identity, found := h.pairTokens[hash]
	if !ok || !found || identity.accountID != accountID || !accountAllowsDevice(accountID, identity.deviceID) {
		return false
	}
	a := h.accounts[accountID]
	if a == nil {
		return false
	}
	d := a.devices[identity.deviceID]
	return d != nil && !d.banned && d.hasPair && !d.pairPending && h.userDisabledLocked(identity.deviceID) &&
		h.managedUsers.Devices[identity.deviceID].RevokedPair != hex.EncodeToString(hash[:]) && subtle.ConstantTimeCompare(d.pairHash[:], hash[:]) == 1
}

func writeManagedUserDisabled(w http.ResponseWriter) {
	writeJSON(w, http.StatusLocked, map[string]any{"code": "user_disabled", "error": "管理员已停用此用户的远程访问，请联系管理员；原配对保留", "retryable": false})
}

func (h *Hub) userPolicyLocked(deviceID string) RemoteAccessPolicy {
	p := cloneRemoteAccessPolicy(h.cfg.RemoteAccess)
	a, assigned := h.managedUsers.Devices[deviceID]
	if !assigned {
		return p
	}
	u := h.managedUserLocked(a.UserID)
	revision := a.Revision
	if u != nil {
		revision += "\x00" + u.Revision
		if u.Station != nil {
			p.Station = cloneStationPolicy(u.Station)
		}
		if p.Audience == "" && p.Station != nil && p.Station.AllowOfficialAccounts {
			p.Audience = "station"
		}
	}
	if h.userDisabledLocked(deviceID) {
		p.Mode = "disabled"
	}
	p.Revision = "user-" + shaHex(p.Revision+"\x00"+deviceID+"\x00"+revision)
	return p
}

func (h *Hub) deviceRemoteAccessPolicy(deviceID string) RemoteAccessPolicy {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.userPolicyLocked(deviceID)
}

func cloneManagedState(s managedUserState) managedUserState {
	out := s
	out.Users = append([]ManagedUser{}, s.Users...)
	for i := range out.Users {
		out.Users[i].Station = cloneStationPolicy(out.Users[i].Station)
	}
	out.Devices = make(map[string]managedAssignment, len(s.Devices))
	for k, v := range s.Devices {
		out.Devices[k] = v
	}
	return out
}

// The separate bounded file is committed before changing live authorization.
// Callers hold mu; policy changes additionally hold remoteAccessGate.
func (h *Hub) commitManagedUsersLocked(next managedUserState) error {
	if err := validateManagedState(&next); err != nil {
		return err
	}
	raw, err := json.Marshal(next)
	if err != nil || len(raw) > maxManagedUsersBytes {
		return errors.New("用户资料超过存储上限")
	}
	if h.cfg.DataDir != "" {
		if err := writeFileAtomic(filepath.Join(h.cfg.DataDir, managedUsersFile), raw, 0600); err != nil {
			return ErrManagedUserUnavailable
		}
	}
	h.managedUsers = next
	return nil
}

func (h *Hub) ManagedUsersSnapshot() any {
	h.mu.Lock()
	defer h.mu.Unlock()
	rows := []map[string]any{}
	for _, u := range h.managedUsers.Users {
		devices, online, paired := 0, 0, 0
		seen := map[string]bool{}
		for _, a := range h.accounts {
			for id, d := range a.devices {
				if seen[id] || h.managedUsers.Devices[id].UserID != u.ID {
					continue
				}
				seen[id] = true
				devices++
				if d.conn != nil && !d.banned && !u.Disabled {
					online++
				}
				if d.hasPair && !d.pairPending && !d.banned {
					paired++
				}
			}
		}
		rows = append(rows, map[string]any{"id": u.ID, "name": u.Name, "note": u.Note, "disabled": u.Disabled, "station": cloneStationPolicy(u.Station), "created_at": u.CreatedAt, "updated_at": u.UpdatedAt, "devices": devices, "online": online, "paired": paired})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i]["name"].(string) < rows[j]["name"].(string) })
	return map[string]any{"revision": h.managedUsers.Revision, "users": rows, "limit": 128}
}

func ValidateManagedUserChange(in ManagedUserChange) error {
	if !in.Confirm || !validManagedText(in.Reason, 256, true) || credentialLike.MatchString(in.Reason) {
		return errors.New("请填写不含密钥的操作原因并确认")
	}
	if in.Action != "save" && in.Action != "assign" && in.Action != "delete" {
		return errors.New("用户操作无效")
	}
	return nil
}

func (h *Hub) ApplyManagedUserChange(in ManagedUserChange) error {
	if err := ValidateManagedUserChange(in); err != nil {
		return err
	}
	h.lifecycleMu.Lock()
	if h.stopped {
		h.lifecycleMu.Unlock()
		return ErrManagedUserUnavailable
	}
	h.activeRequests.Add(1)
	h.lifecycleMu.Unlock()
	defer h.activeRequests.Done()
	h.remoteAccessGate.Lock()
	defer h.remoteAccessGate.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if in.ExpectedRevision != h.managedUsers.Revision {
		return ErrManagedUserConflict
	}
	next := cloneManagedState(h.managedUsers)
	next.Revision = "users-" + newID()
	affected := map[string]bool{}
	switch in.Action {
	case "save":
		policy := cloneStationPolicy(in.Station)
		if policy != nil && (policy.Version != 2 || policy.normalize() != nil) {
			return errors.New("单独授权需要有效的 v2 来源名单")
		}
		if in.ID == "" {
			in.ID = "usr-" + newID()
			next.Users = append(next.Users, ManagedUser{ID: in.ID, CreatedAt: nowMs()})
		}
		found, accessChanged := false, false
		for i := range next.Users {
			if next.Users[i].ID == in.ID {
				found = true
				u := &next.Users[i]
				accessChanged = u.Revision == "" || u.Disabled != in.Disabled || !reflect.DeepEqual(u.Station, policy)
				u.Name, u.Note, u.Disabled, u.Station = in.Name, in.Note, in.Disabled, policy
				if accessChanged {
					u.Revision = "profile-" + newID()
				}
				u.UpdatedAt = nowMs()
			}
		}
		if !found {
			return errors.New("用户不存在")
		}
		for id, a := range next.Devices {
			if accessChanged && a.UserID == in.ID {
				affected[id] = true
			}
		}
	case "assign":
		_, d := h.adminDeviceLocked(in.DeviceRef)
		if d == nil || in.ID != "" && h.managedUserLocked(in.ID) == nil {
			return errors.New("用户或设备不存在")
		}
		if h.cfg.DeploymentMode == "bot" {
			return errors.New("Bot 设备使用整组凭据，暂不支持单台关联用户；可在设备页封禁或解除封禁")
		}
		if h.managedUsers.Devices[d.info.DeviceID].UserID == in.ID {
			return errors.New("设备已属于所选用户")
		}
		assignment := managedAssignment{UserID: in.ID, Revision: "assignment-" + newID(), RevokedPair: h.managedUsers.Devices[d.info.DeviceID].RevokedPair}
		if d.hasPair {
			assignment.RevokedPair = hex.EncodeToString(d.pairHash[:])
		}
		next.Devices[d.info.DeviceID] = assignment
		affected[d.info.DeviceID] = true
	case "delete":
		for _, a := range next.Devices {
			if a.UserID == in.ID {
				return errors.New("请先移出该用户的设备")
			}
		}
		for hash, id := range h.enrollmentOwners {
			if id == in.ID {
				if _, exists := h.enrollmentClaims[hash]; exists {
					return errors.New("该用户仍有登记码，请先撤销")
				}
			}
		}
		found := false
		for i, u := range next.Users {
			if u.ID == in.ID {
				next.Users = append(next.Users[:i], next.Users[i+1:]...)
				found = true
				break
			}
		}
		if !found {
			return errors.New("用户不存在")
		}
	default:
		return errors.New("用户操作无效")
	}
	if err := h.commitManagedUsersLocked(next); err != nil {
		return err
	}
	for acct, a := range h.accounts {
		for id, d := range a.devices {
			if affected[id] {
				if in.Action == "assign" {
					h.revokePairLocked(acct, d)
				}
				h.disconnectLocked(acct, d)
				if d.standby != nil {
					d.standby.close()
					d.standby = nil
				}
				a.wakeEventWaitersLocked()
			}
		}
	}
	for id, challenge := range h.sourceChallenges {
		if affected[challenge.DeviceID] {
			delete(h.sourceChallenges, id)
		}
	}
	h.auditLocked(AdminAudit{At: nowMs(), Action: "user-" + in.Action, DeviceRef: in.DeviceRef, Reason: in.Reason, Actor: "standalone-admin"})
	h.markDirty()
	return nil
}

// Invited membership is persisted before registering the first device secret.
func (h *Hub) assignEnrolledUserLocked(deviceID, userID string) error {
	if userID == "" {
		return nil
	}
	if existing, ok := h.managedUsers.Devices[deviceID]; ok {
		if existing.UserID != userID {
			return errors.New("设备已有其他用户归属")
		}
		return nil
	}
	u := h.managedUserLocked(userID)
	if u == nil || u.Disabled {
		return errors.New("用户不存在或已停用")
	}
	next := cloneManagedState(h.managedUsers)
	next.Devices[deviceID] = managedAssignment{UserID: userID, Revision: "assignment-" + newID()}
	next.Revision = "users-" + newID()
	return h.commitManagedUsersLocked(next)
}

// Public discovery is global. Only proven devices or unused invitations may
// observe a user's exception; never trust a user ID supplied by the client.
func (h *Hub) discoveryUserPolicy(r *http.Request) (RemoteAccessPolicy, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if claim := r.Header.Get("X-Salcara-Enrollment-Claim"); claim != "" {
		hash, ok := enrollmentClaimHash(claim)
		if !ok || !h.enrollmentClaimValidLocked(hash) {
			return RemoteAccessPolicy{}, false
		}
		p := cloneRemoteAccessPolicy(h.cfg.RemoteAccess)
		if u := h.managedUserLocked(h.enrollmentOwners[hash]); u != nil && u.Station != nil {
			p.Station = cloneStationPolicy(u.Station)
			p.Revision = "invite-" + shaHex(p.Revision+u.Revision+hex.EncodeToString(hash[:]))
		}
		return p, true
	}
	if id := r.Header.Get("X-Salcara-Device-Id"); id != "" {
		a := h.accounts[deviceAccount(id)]
		if a == nil || a.devices[id] == nil || !matchesDeviceSecret(a.devices[id], r) {
			return RemoteAccessPolicy{}, false
		}
		return h.userPolicyLocked(id), true
	}
	return cloneRemoteAccessPolicy(h.cfg.RemoteAccess), true
}
