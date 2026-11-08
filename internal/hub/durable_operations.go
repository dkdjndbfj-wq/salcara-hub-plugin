package hub

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

type executionTicket struct {
	Version     int    `json:"v"`
	OperationID string `json:"id"`
	Owner       string `json:"owner"`
	Hash        string `json:"hash"`
	Deadline    int64  `json:"deadline"`
	Epoch       string `json:"epoch"`
}
type ticketDeadlineKey struct{}

func durableCommandType(t string) bool {
	switch t {
	case "session.start", "session.send", "session.steer", "session.continue", "session.interrupt", "approval.respond", "desktop.session.send", "desktop.approval.respond", "session.compact":
		return true
	}
	return false
}
func normalizedOperation(raw json.RawMessage) (string, string, error) {
	if _, err := strictObjectFields(raw, []string{"operationId"}); err != nil {
		return "", "", err
	}
	if _, err := commandMeta(raw); err != nil {
		return "", "", err
	}
	// canonicalCommand rejects trailing input and preserves JSON number values.
	canonical, err := canonicalCommand(raw)
	if err != nil {
		return "", "", err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(canonical), &fields) != nil || fields == nil {
		return "", "", errors.New("invalid command")
	}
	var typ string
	_ = json.Unmarshal(fields["type"], &typ)
	if !durableCommandType(typ) {
		return "", "", errors.New("unsupported durable command")
	}
	delete(fields, "operationId")
	b, _ := json.Marshal(fields)
	canonical, err = canonicalCommand(b)
	return shaHex(canonical), typ, err
}
func (s *durableStore) signTicket(t executionTicket) string {
	b, _ := json.Marshal(t)
	payload := base64.RawURLEncoding.EncodeToString(b)
	m := hmac.New(sha256.New, s.key)
	_, _ = m.Write([]byte("salcara-operation-v2\x00" + payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
func (s *durableStore) verifyTicket(raw string) (executionTicket, error) {
	var t executionTicket
	parts := strings.Split(raw, ".")
	if len(parts) != 2 || len(raw) > 2048 {
		return t, errors.New("invalid ticket")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return t, err
	}
	m := hmac.New(sha256.New, s.key)
	_, _ = m.Write([]byte("salcara-operation-v2\x00" + parts[0]))
	if !hmac.Equal(sig, m.Sum(nil)) {
		return t, errors.New("invalid ticket")
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return t, err
	}
	if json.Unmarshal(b, &t) != nil || t.Version != 2 || t.Epoch != s.epoch || !requestUUID.MatchString(t.OperationID) || !hexHash(t.Owner) || !hexHash(t.Hash) || t.Deadline <= 0 {
		return t, errors.New("invalid ticket")
	}
	return t, nil
}

func (h *Hub) handleOperationPrepare(w http.ResponseWriter, r *http.Request, acct string) {
	var req commandReq
	if !decodeCommandRequest(w, r, &req) {
		return
	}
	s := h.durable
	if s == nil {
		writeError(w, 503, "持久存储未启用")
		return
	}
	actual, scope, ok := h.appStorageIdentity(acct, r, req.DeviceID)
	if !ok {
		writeError(w, 403, "设备授权已失效")
		return
	}
	if !requestUUID.MatchString(req.RequestID) || req.ReceiptOnly || req.SourceGrant != nil || req.ExecutionTicket != "" {
		writeError(w, 400, "准备请求格式不正确")
		return
	}
	hash, _, err := normalizedOperation(req.Command)
	if err != nil {
		writeJSON(w, 400, map[string]any{"code": "durable_command_unsupported", "error": "此操作不支持持久准备"})
		return
	}
	owner := shaHex(actual + "\x00" + req.DeviceID + "\x00" + h.receiptCredential(actual, r))
	h.receipts.mu.Lock()
	defer h.receipts.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err = s.db.Exec(`INSERT INTO quota_owners(owner,scope) VALUES(?,?) ON CONFLICT(owner) DO NOTHING`, owner, scope); err != nil {
		writeError(w, 503, "存储归属尚未保存")
		return
	}
	var id, oldHash, ticket string
	var deadline int64
	err = s.db.QueryRow(`SELECT id,hash,deadline,ticket FROM operations WHERE owner=? AND intent=?`, owner, req.RequestID).Scan(&id, &oldHash, &deadline, &ticket)
	if errors.Is(err, sql.ErrNoRows) {
		if h.receipts.entries[shaHex(owner+"\x00"+req.RequestID)] != nil {
			writeJSON(w, 409, map[string]any{"code": "legacy_operation_requires_recovery", "error": "原消息已有旧版发送记录，请先恢复原回执；不会分配新执行身份"})
			return
		}
		var count, owned int
		if err = s.db.QueryRow(`SELECT COUNT(*),COUNT(CASE WHEN owner=? THEN 1 END) FROM operations`, owner).Scan(&count, &owned); err != nil {
			writeError(w, 503, "准备记录暂时不可用")
			return
		}
		if count >= 65536 || owned >= 4096 {
			writeJSON(w, 429, map[string]any{"code": "operation_capacity", "error": "安全操作记录已达容量"})
			return
		}
		id = uuid.NewString()
		deadline = nowMs() + int64(10*time.Minute/time.Millisecond)
		ticket = s.signTicket(executionTicket{Version: 2, OperationID: id, Owner: owner, Hash: hash, Deadline: deadline, Epoch: s.epoch})
		_, err = s.db.Exec(`INSERT INTO operations(id,owner,intent,hash,deadline,ticket) VALUES(?,?,?,?,?,?)`, id, owner, req.RequestID, hash, deadline, ticket)
		oldHash = hash
	}
	if err != nil {
		writeError(w, 503, "准备记录尚未保存，请重试同一意图")
		return
	}
	if oldHash != hash {
		writeJSON(w, 409, map[string]any{"code": "request_id_conflict", "error": "同一意图不能更改内容"})
		return
	}
	state := "prepared"
	if deadline <= nowMs() {
		state = "expired"
	}
	writeJSON(w, 200, map[string]any{"ok": true, "operationId": id, "ticket": ticket, "dispatchNotAfter": deadline, "storageEpoch": s.epoch, "state": state})
}

func (h *Hub) validateOperationCommit(w http.ResponseWriter, r *http.Request, acct string, req commandReq) (*http.Request, bool) {
	if req.ExecutionTicket == "" {
		// A prepared server identity can never bypass its ticket by choosing the
		// v1 request shape. Legacy UUIDs remain compatible and independently fenced.
		if h.durable != nil && req.RequestID != "" {
			var id string
			owner := shaHex(acct + "\x00" + req.DeviceID + "\x00" + h.receiptCredential(acct, r))
			err := h.durable.db.QueryRow(`SELECT id FROM operations WHERE id=? OR (owner=? AND intent=?) LIMIT 1`, req.RequestID, owner, req.RequestID).Scan(&id)
			if err == nil {
				writeJSON(w, 409, map[string]any{"code": "execution_ticket_required", "error": "此操作需要原执行票据"})
				return r, false
			}
		}
		return r, true
	}
	s := h.durable
	if s == nil {
		writeError(w, 503, "持久存储未启用")
		return r, false
	}
	t, err := s.verifyTicket(req.ExecutionTicket)
	owner := shaHex(acct + "\x00" + req.DeviceID + "\x00" + h.receiptCredential(acct, r))
	hash, _, hashErr := normalizedOperation(req.Command)
	var command struct {
		OperationID string `json:"operationId"`
	}
	_ = json.Unmarshal(req.Command, &command)
	if err != nil || hashErr != nil || t.Owner != owner || t.Hash != hash || t.OperationID != req.RequestID || command.OperationID != req.RequestID {
		writeJSON(w, 409, map[string]any{"code": "execution_ticket_invalid", "error": "执行票据与原操作不一致"})
		return r, false
	}
	var persisted string
	if err = s.db.QueryRow(`SELECT ticket FROM operations WHERE id=? AND owner=?`, t.OperationID, owner).Scan(&persisted); err != nil || persisted != req.ExecutionTicket {
		writeJSON(w, 409, map[string]any{"code": "operation_missing", "error": "原操作身份不存在，未重新发送"})
		return r, false
	}
	return r.WithContext(context.WithValue(r.Context(), ticketDeadlineKey{}, t.Deadline)), true
}

func (h *Hub) operationAccess(acct string, r *http.Request, deviceID, id, ticket string) (executionTicket, string, bool) {
	var empty executionTicket
	s := h.durable
	if s == nil {
		return empty, "", false
	}
	actual, _, ok := h.appStorageIdentity(acct, r, deviceID)
	if !ok {
		return empty, "", false
	}
	t, err := s.verifyTicket(ticket)
	owner := shaHex(actual + "\x00" + deviceID + "\x00" + h.receiptCredential(actual, r))
	if err != nil || t.OperationID != id || t.Owner != owner {
		return empty, "", false
	}
	return t, shaHex(owner + "\x00" + id), true
}
func (h *Hub) handleOperationRead(w http.ResponseWriter, r *http.Request, acct string) {
	q := r.URL.Query()
	t, key, ok := h.operationAccess(acct, r, q.Get("deviceId"), q.Get("operationId"), q.Get("ticket"))
	if !ok {
		writeError(w, 403, "操作授权已失效")
		return
	}
	state := "prepared"
	if nowMs() >= t.Deadline {
		state = "expired"
	}
	h.receipts.mu.Lock()
	e := h.receipts.entries[key]
	if e != nil {
		state = "pending"
		if e.completed {
			state = "uncertain"
		}
	}
	h.receipts.mu.Unlock()
	out := map[string]any{"ok": true, "operationId": t.OperationID, "dispatchNotAfter": t.Deadline, "storageEpoch": h.durable.epoch, "state": state}
	var queueState string
	if h.durable.db.QueryRow(`SELECT state FROM offline_queue WHERE id=?`, key).Scan(&queueState) == nil {
		if queueState == "queued" {
			out["state"] = "queued"
		}
		if queueState == "uncertain" {
			out["state"] = "uncertain"
		}
	}
	rep, status, persisted, err := h.durable.reply(key)
	if err == nil {
		out["state"] = persisted
		out["httpStatus"] = status
		if persisted == "completed" {
			out["ok"] = rep.OK
			if len(rep.Result) > 0 {
				out["result"] = rep.Result
			}
			if rep.Error != "" {
				out["error"] = rep.Error
			}
			if rep.Code != "" {
				out["code"] = rep.Code
			}
		}
		if persisted == "truncated" {
			out["state"] = "uncertain"
			out["code"] = "result_body_truncated"
		}
		var expires int64
		_ = h.durable.db.QueryRow(`SELECT expires FROM replies WHERE id=?`, key).Scan(&expires)
		out["retainedUntil"] = expires
	} else if !errors.Is(err, sql.ErrNoRows) {
		writeError(w, 503, "回执暂时不可用")
		return
	}
	writeJSON(w, 200, out)
}
func (h *Hub) handleOperationAck(w http.ResponseWriter, r *http.Request, acct string) {
	var in struct {
		DeviceID    string `json:"deviceId"`
		OperationID string `json:"operationId"`
		Ticket      string `json:"ticket"`
	}
	if !decodeBody(w, r, 4096, &in) {
		return
	}
	_, key, ok := h.operationAccess(acct, r, in.DeviceID, in.OperationID, in.Ticket)
	if !ok {
		writeError(w, 403, "操作授权已失效")
		return
	}
	s := h.durable
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`UPDATE replies SET expires=MIN(expires,?) WHERE id=? AND state='completed'`, nowMs()+durableDay, key); err != nil {
		writeError(w, 503, "确认尚未保存")
		return
	}
	var expires int64
	var scope string
	if s.db.QueryRow(`SELECT scope FROM quota_owners WHERE owner=(SELECT owner FROM replies WHERE id=?)`, key).Scan(&scope) == nil {
		if _, err := s.db.Exec(`UPDATE blob_refs SET expires=MIN(expires,?) WHERE scope=? AND ref=?`, nowMs()+durableDay, scope, "operation:"+in.OperationID); err != nil {
			writeError(w, 503, "图片引用确认尚未保存")
			return
		}
	}
	if s.db.QueryRow(`SELECT expires FROM replies WHERE id=?`, key).Scan(&expires) != nil {
		writeJSON(w, 409, map[string]any{"code": "command_receipt_pending", "error": "尚无完整回执可确认"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "operationId": in.OperationID, "retainedUntil": expires})
}
