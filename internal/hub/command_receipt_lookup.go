package hub

import "encoding/json"

// lookupCommandReceipt is deliberately separate from executeReceipt: absence
// is never permission to dispatch. Its outer HTTP flag is excluded from the
// original command hash, so recovering an immutable submission cannot mutate it.
func (h *Hub) lookupCommandReceipt(accountID, deviceID, credential, requestID string, command json.RawMessage, authorize func(*device) bool) (replyBody, int, string) {
	authorized := func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		a := h.accounts[accountID]
		return a != nil && a.devices[deviceID] != nil && authorize != nil && authorize(a.devices[deviceID])
	}
	if !authorized() {
		return replyBody{Error: "设备授权已失效"}, 403, "device_authorization_expired"
	}
	s := h.receipts
	if s == nil || s.dir == "" {
		return replyBody{Error: "本站尚未开启持久化回执查询"}, 503, "idempotency_unavailable"
	}
	canonical, err := canonicalCommand(command)
	if err != nil {
		return replyBody{Error: "command 格式不正确"}, 400, "invalid_command"
	}
	owner := shaHex(accountID + "\x00" + deviceID + "\x00" + credential)
	key, payloadHash := shaHex(owner+"\x00"+requestID), shaHex(canonical)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return replyBody{Error: "服务正在重启"}, 503, "service_restarting"
	}
	entry := s.entries[key]
	s.restoreReplyLocked(key, entry)
	var rep replyBody
	status, code := 409, "command_receipt_missing"
	switch {
	case entry == nil || entry.ExpiresAt <= nowMs():
		rep.Error = "未找到原消息的有效回执，未重新发送，请核对原会话"
	case entry.Hash != payloadHash:
		rep.Error, code = "同一个 requestId 不能用于不同指令", "request_id_conflict"
	case !entry.completed:
		rep.Error, code = "原消息仍在处理，请稍后确认；未重新发送", "command_receipt_pending"
	case !entry.cached:
		rep.Error, code = "指令可能已被电脑接收，请刷新原会话确认；不会重复发送", "command_delivery_uncertain"
	default:
		rep, status, code = entry.reply, entry.status, ""
		rep.Result = append(json.RawMessage(nil), rep.Result...)
	}
	s.mu.Unlock()
	// Pair revocation while waiting for the receipt lock must hide the reply.
	if !authorized() {
		return replyBody{Error: "设备授权已失效"}, 403, "device_authorization_expired"
	}
	return rep, status, code
}
