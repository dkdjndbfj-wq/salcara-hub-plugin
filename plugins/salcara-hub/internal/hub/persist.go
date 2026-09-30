package hub

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const devicesFile = "devices.json"

type persistedDevice struct {
	Device     Device `json:"device"`
	LastSeen   int64  `json:"lastSeen"`
	SecretHash string `json:"secretHash,omitempty"`
	PairHash   string `json:"pairHash,omitempty"`
	Banned     bool   `json:"banned,omitempty"`
	BanReason  string `json:"banReason,omitempty"`
	BannedAt   int64  `json:"bannedAt,omitempty"`
}

type persistedState struct {
	Version  int                          `json:"version"`
	Accounts map[string][]persistedDevice `json:"accounts"`
	Audit    []AdminAudit                 `json:"adminAudit,omitempty"`
}

// loadDevices reads devices.json into h.accounts. A missing file is fine.
func (h *Hub) loadDevices() error {
	if h.cfg.DataDir == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(h.cfg.DataDir, devicesFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var st persistedState
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("parse %s: %w", devicesFile, err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.adminAudit = st.Audit
	if len(h.adminAudit) > 200 {
		h.adminAudit = h.adminAudit[len(h.adminAudit)-200:]
	}
	for id, devs := range st.Accounts {
		if len(h.accounts) >= maxDeviceAccounts || len(devs) > maxDevices {
			return errors.New("persisted device capacity exceeded")
		}
		a := h.accountLocked(id)
		for _, pd := range devs {
			if !validDeviceInfo(pd.Device) {
				continue
			}
			dev := &device{info: pd.Device, lastSeen: pd.LastSeen, banned: pd.Banned, banReason: pd.BanReason, bannedAt: pd.BannedAt}
			if decoded, err := hex.DecodeString(pd.SecretHash); err == nil && len(decoded) == 32 {
				copy(dev.secretHash[:], decoded)
				dev.hasSecret = true
			}
			if decoded, err := hex.DecodeString(pd.PairHash); err == nil && len(decoded) == 32 && !dev.banned {
				copy(dev.pairHash[:], decoded)
				dev.hasPair = true
			}
			a.devices[pd.Device.DeviceID] = dev
			if dev.banned {
				h.bannedDeviceIDs[pd.Device.DeviceID] = true
			}
			if dev.hasPair {
				if _, exists := h.pairTokens[dev.pairHash]; exists {
					return errors.New("ambiguous persisted pair token")
				}
				h.pairTokens[dev.pairHash] = pairIdentity{id, pd.Device.DeviceID}
			}
		}
	}
	// A device ID is globally banned even if an older/corrupt snapshot has a
	// duplicate in another legacy namespace with an inconsistent ban flag.
	for accountID, a := range h.accounts {
		for id, dev := range a.devices {
			if h.bannedDeviceIDs[id] {
				dev.banned = true
				h.revokePairLocked(accountID, dev)
			}
		}
	}
	return nil
}

// markDirty asks the saver goroutine to write devices.json soon.
func (h *Hub) markDirty() {
	if h.cfg.DataDir == "" {
		return
	}
	select {
	case h.dirty <- struct{}{}:
	default:
	}
}

func (h *Hub) saver() {
	defer close(h.saverDone)
	for {
		select {
		case <-h.dirty:
			h.saveNow()
		case <-h.done:
			select {
			case <-h.dirty:
				h.saveNow()
			default:
			}
			return
		}
	}
}

func (h *Hub) snapshot() persistedState {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := persistedState{Version: 2, Accounts: map[string][]persistedDevice{}, Audit: append([]AdminAudit(nil), h.adminAudit...)}
	for id, a := range h.accounts {
		if len(a.devices) == 0 {
			continue
		}
		list := make([]persistedDevice, 0, len(a.devices))
		for _, d := range a.devices {
			ls := d.lastSeen
			if d.conn != nil {
				ls = nowMs()
			}
			entry := persistedDevice{Device: d.info, LastSeen: ls, Banned: d.banned, BanReason: d.banReason, BannedAt: d.bannedAt}
			if d.hasSecret {
				entry.SecretHash = hex.EncodeToString(d.secretHash[:])
			}
			if d.hasPair {
				entry.PairHash = hex.EncodeToString(d.pairHash[:])
			}
			list = append(list, entry)
		}
		st.Accounts[id] = list
	}
	return st
}

func (h *Hub) saveNow() {
	err := h.saveNowError()
	if err != nil {
		h.log.Error("save devices failed", "err", err)
	}
}
func (h *Hub) saveNowError() error {
	h.saveMu.Lock()
	defer h.saveMu.Unlock()
	b, err := json.MarshalIndent(h.snapshot(), "", "  ")
	if err == nil {
		err = writeFileAtomic(filepath.Join(h.cfg.DataDir, devicesFile), b, 0o600)
	}
	return err
}

// writeFileAtomic writes to a temp file in the same directory, fsyncs it and
// renames it over path.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".devices-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
