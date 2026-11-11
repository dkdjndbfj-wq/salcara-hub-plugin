package hub

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// A credential rotation creates a different scope even if deviceId is reused.
func durableScope(acct string, d *device) string {
	if d == nil {
		return ""
	}
	return shaHex(acct + "\x00" + d.info.DeviceID + "\x00" + d.bindingID + "\x00" + hex.EncodeToString(d.pairHash[:]) + "\x00" + hex.EncodeToString(d.secretHash[:]))
}

// Bot credentials are group scoped rather than QR pair scoped. Include every
// active group generation for this computer so revoking/replacing a Bot phone
// never exposes the previous generation's retained mailbox to its successor.
// Callers hold h.mu, like all pairing/group access.
func (h *Hub) durableScopeLocked(acct string, d *device) string {
	base := durableScope(acct, d)
	if d == nil || h.cfg.DeploymentMode != "bot" {
		return base
	}
	var generations []string
	for _, g := range h.botGroups {
		if g.Computers[d.info.DeviceID] {
			generations = append(generations, g.ID+":"+g.PhoneTokenHash)
		}
	}
	sort.Strings(generations)
	return shaHex(base + "\x00" + strings.Join(generations, "\x00"))
}

func prepareStoredEvent(deviceID string, seq int64, m map[string]json.RawMessage) (*storedEvent, error) {
	m["seq"], _ = json.Marshal(seq)
	m["deviceId"], _ = json.Marshal(deviceID)
	if _, ok := m["ts"]; !ok {
		m["ts"], _ = json.Marshal(nowMs())
	}
	var session string
	_ = json.Unmarshal(m["sessionKey"], &session)
	data, err := json.Marshal(m)
	return &storedEvent{seq: seq, deviceID: deviceID, sessionKey: session, data: data}, err
}

func durableNotice(fields map[string]json.RawMessage, code, text string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{"type": json.RawMessage(`"notice"`), "seq": fields["seq"], "deviceId": fields["deviceId"], "sessionKey": fields["sessionKey"], "ts": fields["ts"], "tool": fields["tool"], "level": json.RawMessage(`"warn"`)}
	if len(out["tool"]) == 0 {
		out["tool"] = json.RawMessage(`"remote"`)
	}
	out["errorCode"], _ = json.Marshal(code)
	out["text"], _ = json.Marshal(text)
	return out
}

func (s *durableStore) findBatch(scope, id string) (eventBatchReceipt, bool, error) {
	if id == "" {
		return eventBatchReceipt{}, false, nil
	}
	var out eventBatchReceipt
	err := s.db.QueryRow(`SELECT digest,accepted,last_seq FROM batches WHERE scope=? AND id=?`, scope, id).Scan(&out.digest, &out.accepted, &out.lastSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return out, false, nil
	}
	return out, err == nil, err
}

func (s *durableStore) storeEvents(scope, batch, digest string, events []*storedEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var bytes int64
	for _, e := range events {
		bytes += int64(len(e.data)) + 64
	}
	if err := s.capacityLocked(scope, bytes); err != nil {
		return err
	}
	// Never forget an accepted batch and later append it again. Bound metadata
	// admission instead of turning expiration into duplicate conversation text.
	if batch != "" {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM batches`).Scan(&count); err != nil {
			return err
		}
		if count >= 200000 {
			return errStorageCapacity
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	last := int64(0)
	for _, e := range events {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(e.data, &fields)
		var typ string
		_ = json.Unmarshal(fields["type"], &typ)
		// Runtime terminal/browser frames and embedded images are not mailbox
		// data. Their original surfaces remain the only authority for recovery.
		if strings.HasPrefix(typ, "terminal.") || strings.HasPrefix(typ, "browser.") || strings.HasPrefix(typ, "workspace.file") {
			fields = durableNotice(fields, "transient_content_not_retained", "此实时内容未存入消息缓存，请回到原功能查看。")
		}
		delete(fields, "imageData")
		data, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		expires := nowMs() + int64(s.policy.RetentionDays)*durableDay
		// Intermediate fragments never outlive a day; a deleted fragment is
		// recorded in event_heads so a subsequent page explicitly reports a gap.
		if typ == "reasoning" || typ == "message.delta" {
			expires = nowMs() + durableDay
		}
		// A turn's accumulated bodies have a separate 2 MiB budget. Preserve
		// terminal state and an explicit recovery gap when the body is truncated.
		turn := eventString(fields, "turnId")
		if turn == "" {
			turn = eventString(fields, "id")
		}
		budgetID := shaHex(e.sessionKey + "\x00" + turn)
		var used, truncated int
		err = tx.QueryRow(`SELECT bytes,truncated FROM event_budgets WHERE scope=? AND id=?`, scope, budgetID).Scan(&used, &truncated)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		limited := (typ == "message" || typ == "message.delta" || typ == "reasoning" || typ == "tool") && used+len(data) > maxCachedReplyBytes
		skip := false
		if limited {
			skip = truncated != 0
			fields = durableNotice(fields, "event_body_truncated", "此轮内容超过缓存上限，已保留任务状态；完整内容请从原会话恢复。")
			data, _ = json.Marshal(fields)
			truncated = 1
		} else {
			used += len(data)
		}
		if _, err = tx.Exec(`INSERT INTO event_budgets(scope,id,bytes,truncated,expires) VALUES(?,?,?,?,?) ON CONFLICT(scope,id) DO UPDATE SET bytes=excluded.bytes,truncated=excluded.truncated,expires=MAX(expires,excluded.expires)`, scope, budgetID, used, truncated, expires); err != nil {
			return err
		}
		data, err = s.seal(data, "event:"+scope+":"+strconv.FormatInt(e.seq, 10))
		if err != nil {
			return err
		}
		if !skip {
			if _, err = tx.Exec(`INSERT INTO events(scope,seq,session,body,expires,bytes) VALUES(?,?,?,?,?,?)`, scope, e.seq, shaHex(e.sessionKey), data, expires, len(data)); err != nil {
				return err
			}
		}
		for _, session := range []string{"", e.sessionKey} {
			if _, err = tx.Exec(`INSERT INTO event_heads(scope,session,last_seq) VALUES(?,?,?) ON CONFLICT(scope,session) DO UPDATE SET last_seq=MAX(last_seq,excluded.last_seq)`, scope, shaHex(session), e.seq); err != nil {
				return err
			}
			if limited {
				if _, err = tx.Exec(`UPDATE event_heads SET evicted=MAX(evicted,?) WHERE scope=? AND session=?`, e.seq, scope, shaHex(session)); err != nil {
					return err
				}
			}
		}
		if err = s.addBlobRefs(tx, scope, e.data, "event:"+strconv.FormatInt(e.seq, 10), expires); err != nil {
			return err
		}
		last = e.seq
	}
	if batch != "" {
		if _, err = tx.Exec(`INSERT INTO batches(scope,id,digest,accepted,last_seq) VALUES(?,?,?,?,?)`, scope, batch, digest, len(events), last); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *durableStore) eventPage(scope, session string, after int64, limit int) (eventPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := eventPage{Events: []json.RawMessage{}, NextSeq: after, StorageEpoch: s.epoch}
	var evicted int64
	err := s.db.QueryRow(`SELECT last_seq,evicted FROM event_heads WHERE scope=? AND session=?`, scope, shaHex(session)).Scan(&out.LastSeq, &evicted)
	if errors.Is(err, sql.ErrNoRows) {
		out.ResetRequired = after > 0
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.ResetRequired = (evicted > 0 && after < evicted) || (after > 0 && after > out.LastSeq)
	if after > out.LastSeq {
		after = 0
		out.NextSeq = 0
	}
	query := `SELECT seq,body FROM events WHERE scope=? AND expires>?`
	args := []any{scope, nowMs()}
	if session != "" {
		query += ` AND session=?`
		args = append(args, shaHex(session))
	}
	var oldest sql.NullInt64
	qold := strings.Replace(query, "seq,body", "MIN(seq)", 1)
	if err = s.db.QueryRow(qold, args...).Scan(&oldest); err != nil {
		return out, err
	}
	out.OldestSeq = oldest.Int64
	// Expired rows have not necessarily been swept yet. Report their gap now.
	gapQuery := `SELECT MAX(seq) FROM events WHERE scope=? AND expires<=?`
	gapArgs := []any{scope, nowMs()}
	if session != "" {
		gapQuery += ` AND session=?`
		gapArgs = append(gapArgs, shaHex(session))
	}
	var expired sql.NullInt64
	if err = s.db.QueryRow(gapQuery, gapArgs...).Scan(&expired); err != nil {
		return out, err
	}
	if expired.Valid && after < expired.Int64 {
		out.ResetRequired = true
	}
	query += ` AND seq>? ORDER BY seq LIMIT ?`
	args = append(args, after, limit+1)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	used := 0
	for rows.Next() {
		var seq int64
		var data []byte
		if err = rows.Scan(&seq, &data); err != nil {
			return out, err
		}
		if len(out.Events) >= limit {
			out.HasMore = true
			break
		}
		data, err = s.open(data, "event:"+scope+":"+strconv.FormatInt(seq, 10))
		if err != nil {
			return out, err
		}
		if len(out.Events) > 0 && used+len(data) > maxEventPageBytes {
			out.HasMore = true
			break
		}
		out.Events = append(out.Events, json.RawMessage(data))
		out.NextSeq = seq
		used += len(data)
	}
	if !out.HasMore && out.ResetRequired && out.LastSeq > out.NextSeq {
		out.NextSeq = out.LastSeq
	}
	return out, rows.Err()
}

func (h *Hub) appStorageIdentity(acct string, r *http.Request, deviceID string) (string, string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.appStorageIdentityLocked(acct, r, deviceID)
}

func (h *Hub) appStorageIdentityLocked(acct string, r *http.Request, deviceID string) (string, string, bool) {
	actual := acct
	var d *device
	if strings.HasPrefix(acct, "bot:") {
		actual, d = h.botAccountAndDeviceLocked(acct, r, deviceID)
	} else {
		if a := h.accounts[acct]; a != nil {
			d = a.devices[deviceID]
		}
		if d != nil && h.pairedDeviceLocked(acct, r) != d {
			d = nil
		}
	}
	if d == nil || d.banned {
		return "", "", false
	}
	return actual, h.durableScopeLocked(actual, d), true
}

var errStorageAuthorization = errors.New("storage authorization changed")

func (h *Hub) appStorageScopeMatchesLocked(acct string, r *http.Request, deviceID, scope string) bool {
	_, current, ok := h.appStorageIdentityLocked(acct, r, deviceID)
	return ok && current == scope
}

// Use only for bounded local mutations. action may acquire the durable mutex,
// but must not acquire Hub.mu again or read/write a network stream.
func (h *Hub) withAppStorageScope(acct string, r *http.Request, deviceID, scope string, action func() error) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.appStorageScopeMatchesLocked(acct, r, deviceID, scope) {
		return errStorageAuthorization
	}
	return action()
}

func (h *Hub) writeStorageAuthorizationError(w http.ResponseWriter, r *http.Request, message string) {
	if !h.writeBotPhoneUnavailable(w, r) {
		writeError(w, http.StatusForbidden, message)
	}
}

func (h *Hub) handleEventAck(w http.ResponseWriter, r *http.Request, acct string) {
	var in struct {
		DeviceID     string `json:"deviceId"`
		SessionKey   string `json:"sessionKey"`
		Through      int64  `json:"through"`
		StorageEpoch string `json:"storageEpoch"`
	}
	if !decodeBody(w, r, 4096, &in) {
		return
	}
	s := h.durable
	if s == nil {
		writeError(w, 503, "持久存储未启用")
		return
	}
	_, scope, ok := h.appStorageIdentity(acct, r, in.DeviceID)
	if !ok {
		h.writeStorageAuthorizationError(w, r, "设备授权已失效")
		return
	}
	if in.StorageEpoch != s.epoch {
		writeJSON(w, 409, map[string]any{"code": "storage_epoch_mismatch", "storageEpoch": s.epoch})
		return
	}
	if in.Through < 0 || len(in.SessionKey) > 512 {
		writeError(w, 400, "回执格式不正确")
		return
	}
	var head int64
	if err := s.db.QueryRow(`SELECT last_seq FROM event_heads WHERE scope=? AND session=?`, scope, shaHex(in.SessionKey)).Scan(&head); err != nil || in.Through > head {
		h.mu.Lock()
		current := h.appStorageScopeMatchesLocked(acct, r, in.DeviceID, scope)
		h.mu.Unlock()
		if !current {
			h.writeStorageAuthorizationError(w, r, "设备授权已失效")
			return
		}
		writeError(w, 409, "确认游标超出已接收范围")
		return
	}
	err := h.withAppStorageScope(acct, r, in.DeviceID, scope, func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		_, err = tx.Exec(`INSERT INTO acks(scope,session,seq) VALUES(?,?,?) ON CONFLICT(scope,session) DO UPDATE SET seq=MAX(seq,excluded.seq)`, scope, shaHex(in.SessionKey), in.Through)
		if err == nil {
			q := `UPDATE blob_refs SET expires=MIN(expires,?) WHERE scope=? AND ref IN(SELECT 'event:' || seq FROM events WHERE scope=? AND seq<=?`
			args := []any{nowMs() + durableDay, scope, scope, in.Through}
			if in.SessionKey != "" {
				q += ` AND session=?`
				args = append(args, shaHex(in.SessionKey))
			}
			q += `)`
			_, err = tx.Exec(q, args...)
		}
		if err == nil {
			q := `UPDATE events SET expires=MIN(expires,?) WHERE scope=? AND seq<=?`
			args := []any{nowMs() + durableDay, scope, in.Through}
			if in.SessionKey != "" {
				q += ` AND session=?`
				args = append(args, shaHex(in.SessionKey))
			}
			_, err = tx.Exec(q, args...)
		}
		if err == nil {
			err = tx.Commit()
		}
		return err
	})
	if errors.Is(err, errStorageAuthorization) {
		h.writeStorageAuthorizationError(w, r, "设备授权已失效")
		return
	}
	if err != nil {
		writeError(w, 503, "回执暂时无法保存")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "through": in.Through, "storageEpoch": s.epoch})
}
