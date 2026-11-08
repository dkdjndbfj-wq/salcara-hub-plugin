package hub

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	deviceEnrollmentClaimTTL    = 24 * time.Hour
	maxDeviceEnrollmentClaims   = 256
	maxEnrollmentClaimsFileSize = 64 << 10
	enrollmentClaimsFile        = "device-enrollment-claims.json"
)

type persistedEnrollmentClaim struct {
	Hash      string `json:"hash"`
	ExpiresAt int64  `json:"expiresAt"`
	UserID    string `json:"userId,omitempty"`
}

type persistedEnrollmentState struct {
	Version int                        `json:"version"`
	Claims  []persistedEnrollmentClaim `json:"claims"`
}

func enrollmentClaimHash(value string) ([32]byte, bool) {
	if len(value) != 64 {
		return [32]byte{}, false
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return [32]byte{}, false
	}
	return sha256.Sum256(decoded), true
}

func (h *Hub) enrollmentClaimValidLocked(hash [32]byte) bool {
	expires, ok := h.enrollmentClaims[hash]
	userID := h.enrollmentOwners[hash]
	user := h.managedUserLocked(userID)
	return ok && time.Now().Before(expires) && (userID == "" || user != nil && !user.Disabled)
}

func decodeEnrollmentClaims(raw []byte) (persistedEnrollmentState, error) {
	state := persistedEnrollmentState{}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	expect := func(want json.Delim) error {
		got, err := d.Token()
		if err != nil || got != want {
			return errors.New("invalid enrollment claim state structure")
		}
		return nil
	}
	if err := expect('{'); err != nil {
		return state, err
	}
	seenFields, seenClaims := map[string]bool{}, map[[32]byte]bool{}
	for d.More() {
		key, err := d.Token()
		name, ok := key.(string)
		if err != nil || !ok || seenFields[name] {
			return state, errors.New("duplicate or invalid enrollment claim field")
		}
		seenFields[name] = true
		switch name {
		case "version":
			err = d.Decode(&state.Version)
		case "claims":
			if err = expect('['); err != nil {
				break
			}
			for d.More() && err == nil {
				if len(state.Claims) >= maxDeviceEnrollmentClaims {
					err = errors.New("enrollment claim capacity exceeded")
					break
				}
				var entry json.RawMessage
				if err = d.Decode(&entry); err != nil || len(entry) > 256 {
					err = errors.New("enrollment claim record capacity exceeded")
					break
				}
				var claim persistedEnrollmentClaim
				if err = decodeStoredRecord(entry, &claim); err != nil {
					break
				}
				decoded, hashErr := hex.DecodeString(claim.Hash)
				var hash [32]byte
				if hashErr == nil && len(decoded) == len(hash) {
					copy(hash[:], decoded)
				}
				if !hexHash(claim.Hash) || claim.UserID != "" && !policyID(claim.UserID) || claim.ExpiresAt <= 0 || claim.ExpiresAt > time.Now().Add(deviceEnrollmentClaimTTL+time.Minute).UnixMilli() || seenClaims[hash] {
					err = errors.New("invalid or duplicate enrollment claim record")
					break
				}
				seenClaims[hash] = true
				state.Claims = append(state.Claims, claim)
			}
			if err == nil {
				err = expect(']')
			}
		default:
			err = errors.New("unknown enrollment claim state field")
		}
		if err != nil {
			return state, err
		}
	}
	if err := expect('}'); err != nil {
		return state, err
	}
	if _, err := d.Token(); err != io.EOF || state.Version != 1 || !seenFields["claims"] {
		return state, errors.New("invalid enrollment claim state version or trailing data")
	}
	return state, nil
}

func (h *Hub) loadEnrollmentClaims() error {
	if h.cfg.DataDir == "" {
		return nil
	}
	path := filepath.Join(h.cfg.DataDir, enrollmentClaimsFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxEnrollmentClaimsFileSize {
		return errors.New("device enrollment claim file must be a bounded regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return errors.New("device enrollment claim file changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxEnrollmentClaimsFileSize+1))
	if err != nil || len(raw) > maxEnrollmentClaimsFileSize {
		return errors.New("device enrollment claim file exceeds its limit")
	}
	state, err := decodeEnrollmentClaims(raw)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, entry := range state.Claims {
		expires := time.UnixMilli(entry.ExpiresAt)
		if time.Now().Before(expires) {
			decoded, _ := hex.DecodeString(entry.Hash)
			var hash [32]byte
			copy(hash[:], decoded)
			h.enrollmentClaims[hash] = expires
			if entry.UserID != "" && h.managedUserLocked(entry.UserID) == nil {
				return errors.New("enrollment user is missing")
			}
			h.enrollmentOwners[hash] = entry.UserID
		}
	}
	return nil
}

func (h *Hub) saveEnrollmentClaims() error {
	h.mu.Lock()
	state := persistedEnrollmentState{Version: 1, Claims: []persistedEnrollmentClaim{}}
	for hash, expires := range h.enrollmentClaims {
		if time.Now().Before(expires) {
			state.Claims = append(state.Claims, persistedEnrollmentClaim{Hash: hex.EncodeToString(hash[:]), ExpiresAt: expires.UnixMilli(), UserID: h.enrollmentOwners[hash]})
		}
	}
	h.mu.Unlock()
	sort.Slice(state.Claims, func(i, j int) bool { return state.Claims[i].Hash < state.Claims[j].Hash })
	raw, err := json.Marshal(state)
	if err != nil || len(raw) > maxEnrollmentClaimsFileSize {
		return errors.New("device enrollment claim state exceeds its limit")
	}
	return writeFileAtomic(filepath.Join(h.cfg.DataDir, enrollmentClaimsFile), raw, 0o600)
}

// CreateDeviceEnrollmentClaim returns a high-entropy, single-use claim. Only
// its hash is retained, and the issued claim is committed before it is shown.
func (h *Hub) CreateDeviceEnrollmentClaim() (string, time.Time, error) {
	return h.CreateDeviceEnrollmentClaimForUser("")
}

func (h *Hub) CreateDeviceEnrollmentClaimForUser(userID string) (string, time.Time, error) {
	if !h.IsRunning() {
		return "", time.Time{}, errors.New("Hub is not running")
	}
	value, ok := randomPairToken()
	if !ok {
		return "", time.Time{}, errors.New("cannot generate enrollment claim")
	}
	hash, _ := enrollmentClaimHash(value)
	expires := time.Now().Add(deviceEnrollmentClaimTTL)
	if h.cfg.DataDir != "" {
		h.saveMu.Lock()
		defer h.saveMu.Unlock()
	}
	h.mu.Lock()
	if userID != "" {
		if u := h.managedUserLocked(userID); u == nil || u.Disabled {
			h.mu.Unlock()
			return "", time.Time{}, errors.New("用户不存在或已停用")
		}
	}
	for existing, deadline := range h.enrollmentClaims {
		if !time.Now().Before(deadline) {
			delete(h.enrollmentClaims, existing)
			delete(h.enrollmentOwners, existing)
		}
	}
	if len(h.enrollmentClaims) >= maxDeviceEnrollmentClaims {
		h.mu.Unlock()
		return "", time.Time{}, errors.New("too many active enrollment claims")
	}
	h.enrollmentClaims[hash] = expires
	h.enrollmentOwners[hash] = userID
	if h.cfg.DataDir == "" {
		h.mu.Unlock()
		return value, expires, nil
	}
	h.mu.Unlock()
	if err := h.saveEnrollmentClaims(); err != nil {
		h.mu.Lock()
		delete(h.enrollmentClaims, hash)
		delete(h.enrollmentOwners, hash)
		h.mu.Unlock()
		return "", time.Time{}, errors.New("cannot persist enrollment claim")
	}
	return value, expires, nil
}

// consumeEnrollmentClaim makes claim use durable before a new device can be
// allocated. A crash between claim consumption and registration fails closed.
func (h *Hub) consumeEnrollmentClaim(hash [32]byte) error {
	if h.cfg.DataDir != "" {
		h.saveMu.Lock()
		defer h.saveMu.Unlock()
	}
	h.mu.Lock()
	if !h.enrollmentClaimValidLocked(hash) {
		delete(h.enrollmentClaims, hash)
		delete(h.enrollmentOwners, hash)
		h.mu.Unlock()
		return errors.New("enrollment claim is missing or expired")
	}
	expires := h.enrollmentClaims[hash]
	owner := h.enrollmentOwners[hash]
	delete(h.enrollmentClaims, hash)
	delete(h.enrollmentOwners, hash)
	if h.cfg.DataDir == "" {
		h.mu.Unlock()
		return nil
	}
	h.mu.Unlock()
	if err := h.saveEnrollmentClaims(); err != nil {
		h.mu.Lock()
		h.enrollmentClaims[hash] = expires
		h.enrollmentOwners[hash] = owner
		h.mu.Unlock()
		return err
	}
	return nil
}

func (h *Hub) AdminEnrollmentClaims() any {
	h.mu.Lock()
	defer h.mu.Unlock()
	rows := []map[string]any{}
	for hash, expires := range h.enrollmentClaims {
		userID, name := h.enrollmentOwners[hash], "未分配用户"
		if u := h.managedUserLocked(userID); u != nil {
			name = u.Name
		}
		rows = append(rows, map[string]any{"ref": hex.EncodeToString(hash[:]), "expires_at": expires.UnixMilli(), "user_id": userID, "user_name": name, "usable": h.enrollmentClaimValidLocked(hash)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i]["expires_at"].(int64) > rows[j]["expires_at"].(int64) })
	return map[string]any{"claims": rows}
}

func (h *Hub) RevokeEnrollmentClaim(ref string) error {
	decoded, err := hex.DecodeString(ref)
	if err != nil || len(decoded) != 32 {
		return errors.New("登记码记录无效")
	}
	var hash [32]byte
	copy(hash[:], decoded)
	h.saveMu.Lock()
	defer h.saveMu.Unlock()
	h.mu.Lock()
	expires, ok := h.enrollmentClaims[hash]
	owner := h.enrollmentOwners[hash]
	if !ok {
		h.mu.Unlock()
		return errors.New("登记码已使用或已撤销，请刷新列表")
	}
	delete(h.enrollmentClaims, hash)
	delete(h.enrollmentOwners, hash)
	h.mu.Unlock()
	if h.cfg.DataDir != "" {
		if err := h.saveEnrollmentClaims(); err != nil {
			h.mu.Lock()
			h.enrollmentClaims[hash] = expires
			h.enrollmentOwners[hash] = owner
			h.mu.Unlock()
			return errors.New("登记码撤销未保存")
		}
	}
	return nil
}

// preflightNewDeviceLocked checks the same bounded account and metadata
// budgets as registration, without consuming an invitation on a full Hub.
func (h *Hub) preflightNewDeviceLocked(accountID string, info Device) int {
	if !accountAllowsDevice(accountID, info.DeviceID) || h.bannedDeviceIDs[info.DeviceID] || h.userDisabledLocked(info.DeviceID) {
		return http.StatusForbidden
	}
	a := h.accounts[accountID]
	if a == nil && len(h.accounts) >= maxDeviceAccounts || a != nil && len(a.devices) >= maxDevices {
		return http.StatusTooManyRequests
	}
	_, charge, err := prepareDeviceMetadata(info)
	if err != nil {
		return http.StatusBadRequest
	}
	projected := h.deviceMetadataBytes + charge
	if a == nil || !a.metadataCharged {
		projected += accountMetadataCost(accountID)
	}
	if projected > h.deviceMetadataLimit {
		return http.StatusTooManyRequests
	}
	return 0
}
