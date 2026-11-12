package hub

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net/http"
)

// These are execution facts, not retained message content. A body-free marker
// reconstructs the same result after content cleanup or a process restart.
func queueTerminalReply(state string) (replyBody, bool) {
	switch state {
	case "cancelled":
		return replyBody{Code: "operation_cancelled", Error: "该待发任务已撤回，未发送到电脑"}, true
	case "dispatch_expired":
		return replyBody{Code: "offline_dispatch_expired", Error: "待发时间已过，未发送到电脑"}, true
	case "queue_unavailable":
		return replyBody{Code: "offline_queue_unavailable", Error: "待发记录无法恢复，未发送到电脑；请查看原任务状态"}, true
	case "queue_rejected":
		return replyBody{Code: "offline_queue_registration_failed", Error: "待发任务未能保存，未发送到电脑；请检查服务器存储后再处理原任务"}, true
	}
	return replyBody{}, false
}

// Caller holds store.mu and receipts.mu. Queue CAS and reply are atomic. Never
// call saveReply inside this transaction: the DB has a single connection.
func (s *durableStore) finishQueuedLocked(key, owner, scope, state string) (bool, error) {
	if _, ok := queueTerminalReply(state); !ok {
		return false, errors.New("invalid queue terminal state")
	}
	// Keep this connection pinned through error recovery. database/sql marks a
	// Tx done even when COMMIT fails; SQLite can still have an open transaction
	// (for example a deferred constraint). A later Tx.Rollback would be a no-op.
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		return false, err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(context.Background(), nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE offline_queue SET state=?,body=NULL,bytes=0 WHERE id=? AND owner=? AND scope=? AND state='queued'`, state, key, owner, scope)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return false, err
	}
	// Body-free receipts need no payload quota, allowing cancellation when full.
	_, err = tx.Exec(`INSERT INTO replies(id,owner,body,status,expires,state,bytes) VALUES(?,?,NULL,409,?,?,0)`, key, owner, nowMs()+int64(s.policy.RetentionDays)*durableDay, state)
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		if _, rollbackErr := conn.ExecContext(context.Background(), "ROLLBACK"); rollbackErr != nil {
			// Never return a potentially uncommitted connection to the pool.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		return false, err
	}
	return true, nil
}

func (h *Hub) cacheQueueTerminalLocked(key, state string) {
	entry := h.receipts.entries[key]
	if entry == nil {
		return
	}
	rep, ok := queueTerminalReply(state)
	if !ok {
		return
	}
	if !entry.completed {
		close(entry.done)
	}
	entry.completed = true
	h.receipts.cacheReplyLocked(entry, rep, http.StatusConflict)
}

func (h *Hub) handleOperationCancel(w http.ResponseWriter, r *http.Request, acct string) {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeError(w, 400, "撤回请求不接受 URL 参数")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	fields, parseErr := strictObjectFields(raw, []string{"deviceId", "operationId", "ticket"})
	deviceID, id, ticket := commandString(fields, "deviceId"), commandString(fields, "operationId"), commandString(fields, "ticket")
	if err != nil || parseErr != nil || len(fields) != 3 || !validID(deviceID) || !requestUUID.MatchString(id) || ticket == "" {
		writeError(w, 400, "撤回请求格式不正确，请使用原任务编号和票据")
		return
	}
	t, key, scope, ok := h.operationAccess(acct, r, deviceID, id, ticket)
	if !ok {
		h.writeStorageAuthorizationError(w, r, "操作授权已失效")
		return
	}
	s := h.durable
	status, state, code := http.StatusConflict, "", "operation_not_queued"
	message := "没有可撤回的待发任务，请查看原任务状态"
	h.receipts.mu.Lock()
	err = h.withAppStorageScope(acct, r, deviceID, scope, func() error {
		if h.receipts.closed {
			return errors.New("receipts closed")
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		var storedTicket string
		err := s.db.QueryRow(`SELECT ticket FROM operations WHERE id=? AND owner=?`, id, t.Owner).Scan(&storedTicket)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if storedTicket != ticket {
			return nil
		}
		var deadline int64
		err = s.db.QueryRow(`SELECT state,deadline FROM offline_queue WHERE id=? AND owner=? AND scope=?`, key, t.Owner, scope).Scan(&state, &deadline)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if state == "queued" {
			next := "cancelled"
			if deadline <= nowMs() {
				next = "dispatch_expired"
			}
			changed, err := s.finishQueuedLocked(key, t.Owner, scope, next)
			if err != nil {
				return err
			}
			if !changed {
				return errors.New("queue changed")
			}
			state = next
			h.cacheQueueTerminalLocked(key, state)
		}
		if state == "cancelled" {
			status, code = http.StatusOK, "operation_cancelled"
		} else if state == "dispatch_expired" {
			code, message = "offline_dispatch_expired", "待发时间已过，未发送到电脑"
		} else {
			code, message = "operation_not_cancellable", "任务已开始发送或结束，无法撤回。请查看原会话，需要时停止任务。"
		}
		return nil
	})
	h.receipts.mu.Unlock()
	if errors.Is(err, errStorageAuthorization) {
		h.writeStorageAuthorizationError(w, r, "操作授权已失效")
		return
	}
	if err != nil {
		writeJSON(w, 503, map[string]any{"ok": false, "operationId": id, "code": "operation_cancel_unavailable", "error": "撤回结果暂未确认，请重试撤回或查看原任务状态", "retryable": true})
		return
	}
	_, currentKey, currentScope, current := h.operationAccess(acct, r, deviceID, id, ticket)
	if !current || currentKey != key || currentScope != scope {
		h.writeStorageAuthorizationError(w, r, "操作授权已失效")
		return
	}
	out := map[string]any{"ok": status == http.StatusOK, "operationId": id, "state": state, "code": code, "cancelled": state == "cancelled", "retryable": false, "storageEpoch": s.epoch, "dispatchNotAfter": t.Deadline}
	if state == "dispatch_expired" {
		out["state"] = "expired"
	}
	if state == "claimed" {
		out["state"] = "pending"
	}
	if state == "queue_unavailable" {
		out["state"] = "not_dispatched"
	}
	if state == "" {
		delete(out, "state")
	}
	if status != http.StatusOK {
		out["error"] = message
	}
	writeJSON(w, status, out)
}
