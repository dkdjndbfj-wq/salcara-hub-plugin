package hub

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

const standbyTTL = 45 * time.Second

// A short authenticated poll, not a second SSE stream. Only already paired
// devices with durable command receipts can advertise handover availability.
func (h *Hub) handleBridgeStandby(w http.ResponseWriter, r *http.Request, acct string) {
	if h.receipts == nil || h.receipts.dir == "" {
		writeError(w, 503, "备用控制需要启用持久化目录")
		return
	}
	id := r.Header.Get("X-Salcara-Device-Id")
	computerID := r.Header.Get("X-Salcara-Computer-Id")
	h.mu.Lock()
	a := h.accounts[acct]
	var dev *device
	if a != nil {
		dev = a.devices[id]
	}
	if dev == nil || dev.banned || h.userDisabledLocked(dev.info.DeviceID) || !matchesDeviceSecret(dev, r) {
		h.mu.Unlock()
		writeError(w, 403, "电脑身份验证失败")
		return
	}
	if dev.pairPending {
		h.mu.Unlock()
		writePairPersistencePending(w)
		return
	}
	if !dev.hasPair || !validPairHash(dev.bindingID) || !validPairHash(dev.phoneHash) || !validID(computerID) {
		if dev.standby != nil {
			dev.standby.close()
			dev.standby = nil
		}
		h.mu.Unlock()
		writeError(w, 403, "备用控制的手机绑定已失效")
		return
	}
	if dev.standby != nil && dev.standbyComputerID != computerID {
		dev.standby.close()
		dev.standby = nil
	}
	dev.standbyComputerID = computerID
	if dev.standby == nil {
		dev.standby = &bridgeConn{cmds: make(chan []byte, 1), closed: make(chan struct{})}
	}
	dev.standbyAt = nowMs()
	dev.lastSeen = dev.standbyAt
	commands := []json.RawMessage{}
	select {
	case raw := <-dev.standby.cmds:
		var envelope commandEnvelope
		if json.Unmarshal(raw, &envelope) == nil && envelope.Phone && envelope.BindingID == dev.bindingID && envelope.PhoneHash == dev.phoneHash && h.pending[envelope.CommandID] != nil {
			commands = append(commands, json.RawMessage(raw))
		}
	default:
	}
	pair := h.pairStateLocked(acct, dev)
	h.mu.Unlock()
	writeJSON(w, 200, map[string]any{"pairing": pair, "commands": commands})
}

// The caller holds h.mu. Besides a handover, standby may acquire its source
// grant only through an active Hub challenge for that exact handover. Desktop
// verifies the saved station credential and global phone generation again.
func (h *Hub) standbyCommandLocked(dev *device, raw json.RawMessage) *bridgeConn {
	if dev.standby == nil || !dev.hasPair || dev.pairPending || dev.banned || !h.managedPairAllowedLocked(dev) || nowMs()-dev.standbyAt > standbyTTL.Milliseconds() || len(raw) > 16<<10 {
		return nil
	}
	var acquisition struct {
		Type        string `json:"type"`
		ChallengeID string `json:"challengeId"`
	}
	if decodeExactObject(raw, []string{"type", "challengeId"}, &acquisition) == nil && acquisition.Type == "source.grant.acquire" {
		challenge := h.sourceChallenges[acquisition.ChallengeID]
		if challenge == nil || !challenge.acquiring {
			return nil
		}
		owner := h.accounts[challenge.account]
		if owner == nil || owner.devices[dev.info.DeviceID] != dev || h.sourceChallengeCurrentLocked(acquisition.ChallengeID, challenge.account, dev) == nil {
			return nil
		}
		raw = challenge.Command
	}
	if !h.standbySwitchAllowedLocked(dev, raw) {
		return nil
	}
	return dev.standby
}

func (h *Hub) standbySwitchAllowedLocked(dev *device, raw json.RawMessage) bool {
	if len(raw) > 16<<10 {
		return false
	}
	var cmd struct {
		Type, TargetHubURL, TargetDeviceID, TargetComputerID, OperationID string
		SourceOnly                                                        bool `json:"sourceOnly"`
	}
	fields := []string{"type", "targetHubUrl", "targetDeviceId", "targetComputerId", "agent", "accountId", "model", "sessionKey", "operationId", "sourceOnly"}
	if decodeExactObject(raw, fields, &cmd) != nil || cmd.Type != "remote.station.switch" || cmd.TargetDeviceID != dev.info.DeviceID || cmd.TargetComputerID != dev.standbyComputerID || cmd.TargetComputerID == "" || !requestUUID.MatchString(cmd.OperationID) {
		return false
	}
	canonical := func(s string) string { return strings.TrimSuffix(strings.TrimRight(s, "/"), "/v1") }
	if h.cfg.PublicURL != "" && canonical(cmd.TargetHubURL) != canonical(h.cfg.PublicURL) {
		return false
	}
	return true
}
