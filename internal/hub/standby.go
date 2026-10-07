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
	if dev == nil || dev.banned || !matchesDeviceSecret(dev, r) {
		h.mu.Unlock()
		writeError(w, 403, "电脑身份验证失败")
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

// The caller holds h.mu. The standby path cannot send messages, read history,
// run diagnostics, or target a different computer/station. Desktop verifies
// the exact saved station credential and global phone generation again.
func (h *Hub) standbyCommandLocked(dev *device, raw json.RawMessage) *bridgeConn {
	if dev.standby == nil || !dev.hasPair || dev.banned || nowMs()-dev.standbyAt > standbyTTL.Milliseconds() || len(raw) > 16<<10 {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	allowed := map[string]bool{"type": true, "targetHubUrl": true, "targetDeviceId": true, "targetComputerId": true, "agent": true, "accountId": true, "model": true, "sessionKey": true, "operationId": true}
	for key := range fields {
		if !allowed[key] {
			return nil
		}
	}
	var cmd struct{ Type, TargetHubURL, TargetDeviceID, TargetComputerID, OperationID string }
	if json.Unmarshal(raw, &cmd) != nil || cmd.Type != "remote.station.switch" || cmd.TargetDeviceID != dev.info.DeviceID || cmd.TargetComputerID != dev.standbyComputerID || cmd.TargetComputerID == "" || !requestUUID.MatchString(cmd.OperationID) {
		return nil
	}
	canonical := func(s string) string { return strings.TrimSuffix(strings.TrimRight(s, "/"), "/v1") }
	if h.cfg.PublicURL != "" && canonical(cmd.TargetHubURL) != canonical(h.cfg.PublicURL) {
		return nil
	}
	return dev.standby
}
