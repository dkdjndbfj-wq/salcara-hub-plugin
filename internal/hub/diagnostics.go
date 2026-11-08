package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

const diagnosticInterval = 5 * time.Second
const diagnosticTimeout = 8 * time.Second
const diagnosticMaxActive = 16
const diagnosticMaxOwners = 1024

type diagnosticSlot struct {
	nextHub, nextDevice, touched time.Time
	active                       bool
}
type diagnosticAdmission struct {
	owners map[*device]*diagnosticSlot
	active int
}

type diagnosticRequest struct {
	DeviceID string `json:"deviceId"`
	Nonce    string `json:"nonce"`
	Target   string `json:"target"`
}

func diagnosticFailure(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": "连接诊断未完成", "code": code, "retryable": false})
}
func validDiagnosticNonce(value string) bool {
	if len(value) < 16 || len(value) > 64 {
		return false
	}
	for _, c := range []byte(value) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// Both targets require ownership of an already paired computer. The Hub target
// measures only this authenticated HTTP exchange; it never contacts the Bridge.
func (h *Hub) handleAppDiagnostics(w http.ResponseWriter, r *http.Request, acct string) {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		diagnosticFailure(w, 400, "diagnostic_invalid_request")
		return
	}
	var raw json.RawMessage
	if !decodeBody(w, r, 1024, &raw) {
		return
	}
	fields, err := strictObjectFields(raw, []string{"deviceId", "nonce", "target"})
	var input diagnosticRequest
	if err != nil || len(fields) != 3 || fields["deviceId"] == nil || fields["nonce"] == nil || fields["target"] == nil || json.Unmarshal(raw, &input) != nil || !validID(input.DeviceID) || !validDiagnosticNonce(input.Nonce) || input.Target != "hub" && input.Target != "device" {
		diagnosticFailure(w, 400, "diagnostic_invalid_request")
		return
	}
	h.mu.Lock()
	execAcct := acct
	var dev *device
	if strings.HasPrefix(acct, "bot:") {
		execAcct, dev = h.botAccountAndDeviceLocked(acct, r, input.DeviceID)
	} else {
		dev = h.pairedDeviceLocked(acct, r)
	}
	if dev == nil || dev.info.DeviceID != input.DeviceID || dev.banned {
		h.mu.Unlock()
		diagnosticFailure(w, 403, "diagnostic_pairing_invalid")
		return
	}
	now := time.Now()
	if h.diagnostics.owners == nil {
		h.diagnostics.owners = make(map[*device]*diagnosticSlot)
	}
	slot := h.diagnostics.owners[dev]
	if slot == nil {
		for owner, previous := range h.diagnostics.owners {
			if !previous.active && now.Sub(previous.touched) > 10*time.Minute {
				delete(h.diagnostics.owners, owner)
			}
		}
		if len(h.diagnostics.owners) >= diagnosticMaxOwners {
			h.mu.Unlock()
			diagnosticFailure(w, 503, "diagnostic_busy")
			return
		}
		slot = &diagnosticSlot{}
		h.diagnostics.owners[dev] = slot
	}
	next := slot.nextHub
	if input.Target == "device" {
		next = slot.nextDevice
	}
	if now.Before(next) || input.Target == "device" && slot.active {
		h.mu.Unlock()
		w.Header().Set("Retry-After", "5")
		diagnosticFailure(w, 429, "diagnostic_rate_limited")
		return
	}
	online := dev.conn != nil
	if online {
		select {
		case <-dev.conn.closed:
			online = false
		default:
		}
	}
	lastSeen := dev.lastSeen
	slot.touched = now
	if input.Target == "hub" {
		slot.nextHub = now.Add(diagnosticInterval)
		h.mu.Unlock()
		writeJSON(w, 200, map[string]any{"version": 1, "deviceId": input.DeviceID, "nonce": input.Nonce, "target": "hub", "status": "ok", "deviceOnline": online, "lastSeen": lastSeen})
		return
	}
	slot.nextDevice = now.Add(diagnosticInterval)
	if !online {
		h.mu.Unlock()
		diagnosticFailure(w, 409, "diagnostic_device_offline")
		return
	}
	if h.diagnostics.active >= diagnosticMaxActive {
		h.mu.Unlock()
		diagnosticFailure(w, 503, "diagnostic_busy")
		return
	}
	slot.active = true
	h.diagnostics.active++
	h.mu.Unlock()
	defer func() { h.mu.Lock(); slot.active = false; h.diagnostics.active--; h.mu.Unlock() }()
	authorize := func(current *device) bool {
		if current != dev {
			return false
		}
		if strings.HasPrefix(acct, "bot:") {
			_, bound := h.botAccountAndDeviceLocked(acct, r, input.DeviceID)
			return bound == dev
		}
		return h.pairedDeviceLocked(execAcct, r) == dev
	}
	// No generic command payload, execution receipt, model authority or offline
	// queue is accepted on this route. The existing Bridge echo is side-effect free.
	command, _ := json.Marshal(map[string]string{"type": "device.ping", "nonce": input.Nonce})
	ctx, cancel := context.WithTimeout(r.Context(), diagnosticTimeout)
	defer cancel()
	started := time.Now()
	reply, status := h.executeCommand(ctx, execAcct, input.DeviceID, command, authorize, input.Nonce)
	elapsed := float64(time.Since(started).Microseconds()) / 1000
	if r.Context().Err() != nil {
		return
	}
	if ctx.Err() != nil || status == 504 {
		diagnosticFailure(w, 504, "diagnostic_timeout")
		return
	}
	if status == 403 {
		diagnosticFailure(w, 403, "diagnostic_pairing_invalid")
		return
	}
	if status == 409 {
		diagnosticFailure(w, 409, "diagnostic_device_offline")
		return
	}
	if status == 429 || status == 503 {
		diagnosticFailure(w, 503, "diagnostic_busy")
		return
	}
	var echo struct {
		Nonce      string `json:"nonce"`
		ReceivedAt int64  `json:"receivedAt"`
	}
	if status != 200 || !reply.OK || len(reply.Result) > 1024 {
		diagnosticFailure(w, 502, "diagnostic_reply_invalid")
		return
	}
	echoFields, echoErr := strictObjectFields(reply.Result, []string{"nonce", "receivedAt"})
	if echoErr != nil || len(echoFields) != 2 || echoFields["nonce"] == nil || echoFields["receivedAt"] == nil || json.Unmarshal(reply.Result, &echo) != nil || echo.Nonce != input.Nonce || echo.ReceivedAt <= 0 || echo.ReceivedAt > 9007199254740991 {
		diagnosticFailure(w, 502, "diagnostic_reply_invalid")
		return
	}
	// Wall-clock timestamps from the computer are never subtracted from Hub or
	// phone clocks. Return only the Hub's monotonic request/reply duration.
	writeJSON(w, 200, map[string]any{"version": 1, "deviceId": input.DeviceID, "nonce": input.Nonce, "target": "device", "status": "ok", "hubDeviceMs": elapsed})
}
