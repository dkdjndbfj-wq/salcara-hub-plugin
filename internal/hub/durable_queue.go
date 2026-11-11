package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

type offlineIntent struct {
	Account  string           `json:"account"`
	DeviceID string           `json:"deviceId"`
	Scope    string           `json:"scope"`
	Command  json.RawMessage  `json:"command"`
	Source   *ExecutionSource `json:"source"`
}

func (h *Hub) maybeQueueOffline(w http.ResponseWriter, r *http.Request, acct string, req commandReq, sourceCheck func() (string, *ExecutionSource)) bool {
	s := h.durable
	deadline, hasTicket := r.Context().Value(ticketDeadlineKey{}).(int64)
	if s == nil || !hasTicket {
		return false
	}
	s.mu.Lock()
	enabled := s.policy.OfflineQueueEnabled
	s.mu.Unlock()
	if !enabled {
		return false
	}
	var metadata struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(req.Command, &metadata)
	if metadata.Type != "session.start" && metadata.Type != "session.send" {
		return false
	}
	actual, scope, allowed := h.appStorageIdentity(acct, r, req.DeviceID)
	if !allowed {
		return false
	}
	callerAcct := acct
	acct = actual
	h.mu.Lock()
	a := h.accounts[acct]
	online := a != nil && a.devices[req.DeviceID] != nil && a.devices[req.DeviceID].conn != nil
	h.mu.Unlock()
	if online {
		return false
	}
	if deadline <= nowMs() {
		writeJSON(w, 409, map[string]any{"code": "execution_ticket_expired", "error": "离线发送期限已过，未发送"})
		return true
	}
	credential := h.receiptCredential(acct, r)
	owner := shaHex(acct + "\x00" + req.DeviceID + "\x00" + credential)
	key := shaHex(owner + "\x00" + req.RequestID)
	canonical, err := canonicalCommand(req.Command)
	if err != nil {
		writeError(w, 400, "指令格式无效")
		return true
	}
	if err = h.withAppStorageScope(callerAcct, r, req.DeviceID, scope, func() error {
		return s.retainCommandBlobs(scope, req.Command, "operation:"+req.RequestID)
	}); err != nil {
		if err == errStorageAuthorization {
			h.writeStorageAuthorizationError(w, r, "设备授权已失效")
			return true
		}
		writeError(w, 503, "离线图片引用尚未保存")
		return true
	}
	h.receipts.mu.Lock()
	defer h.receipts.mu.Unlock()
	h.remoteAccessGate.RLock()
	defer h.remoteAccessGate.RUnlock()
	code, source := sourceCheck()
	if code != "" || source == nil || source.Mode != "open" {
		return false
	}
	status := http.StatusAccepted
	var out map[string]any
	err = h.withAppStorageScope(callerAcct, r, req.DeviceID, scope, func() error {
		if h.receipts.closed {
			status, out = 503, map[string]any{"error": "服务正在重启"}
			return nil
		}
		if entry := h.receipts.entries[key]; entry != nil {
			if entry.Hash != shaHex(canonical) {
				status, out = 409, map[string]any{"code": "request_id_conflict", "error": "原操作内容不一致"}
				return nil
			}
			var state string
			_ = s.db.QueryRow(`SELECT state FROM offline_queue WHERE id=?`, key).Scan(&state)
			if state != "queued" {
				return nil
			}
		} else {
			if len(h.receipts.entries) >= maxCommandReceipts {
				status, out = 429, map[string]any{"error": "安全操作记录已满"}
				return nil
			}
			entry := &commandReceipt{durableCommandReceipt: durableCommandReceipt{Version: 2, Owner: owner, Hash: shaHex(canonical), ExpiresAt: nowMs() + 30*durableDay}, done: make(chan struct{})}
			if err := h.receipts.persistLocked(key, entry.durableCommandReceipt); err != nil {
				return err
			}
			h.receipts.entries[key] = entry
			intent := offlineIntent{Account: acct, DeviceID: req.DeviceID, Scope: scope, Command: req.Command, Source: source}
			raw, _ := json.Marshal(intent)
			encrypted, e := s.seal(raw, "queue:"+key)
			if e == nil {
				s.mu.Lock()
				var count, owned int
				e = s.db.QueryRow(`SELECT COUNT(*),COUNT(CASE WHEN owner=? THEN 1 END) FROM offline_queue WHERE state='queued'`, owner).Scan(&count, &owned)
				if e == nil && (count >= 64 || owned >= 4) {
					e = errStorageCapacity
				}
				if e == nil {
					e = s.capacityLocked(scope, int64(len(encrypted)))
				}
				if e == nil {
					_, e = s.db.Exec(`INSERT INTO offline_queue(id,owner,scope,deadline,state,body,bytes) VALUES(?,?,?,?,'queued',?,?)`, key, owner, scope, deadline, encrypted, len(encrypted))
				}
				s.mu.Unlock()
			}
			if e != nil {
				entry.completed = true
				close(entry.done)
				status, out = 503, map[string]any{"code": "command_delivery_uncertain", "error": "原操作未能完成登记，请查询原回执；不会重新分配身份"}
				return nil
			}
		}
		out = map[string]any{"ok": true, "state": "queued", "queued": true, "requestId": req.RequestID, "operationId": req.RequestID, "dispatchNotAfter": deadline, "storageEpoch": s.epoch}
		return nil
	})
	if err == errStorageAuthorization {
		h.writeStorageAuthorizationError(w, r, "设备授权已失效")
		return true
	}
	if err != nil {
		writeError(w, 503, "离线操作尚未保存")
		return true
	}
	if out == nil {
		return false
	}
	writeJSON(w, status, out)
	return true
}

func (h *Hub) runOfflineQueue() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-h.done:
			return
		case <-ticker.C:
			h.dispatchOfflineQueue()
		}
	}
}
func (h *Hub) dispatchOfflineQueue() {
	s := h.durable
	if s == nil {
		return
	}
	s.mu.Lock()
	queueEnabled := s.policy.OfflineQueueEnabled
	s.mu.Unlock()
	type row struct {
		id, owner string
		deadline  int64
		body      []byte
	}
	var pending []row
	rows, err := s.db.Query(`SELECT id,owner,deadline,body FROM offline_queue WHERE state='queued' ORDER BY deadline LIMIT 8`)
	if err != nil {
		return
	}
	for rows.Next() {
		var item row
		if rows.Scan(&item.id, &item.owner, &item.deadline, &item.body) == nil {
			pending = append(pending, item)
		}
	}
	rows.Close()
	for _, item := range pending {
		raw, err := s.open(item.body, "queue:"+item.id)
		if err != nil {
			continue
		}
		var intent offlineIntent
		if json.Unmarshal(raw, &intent) != nil {
			continue
		}
		h.mu.Lock()
		a := h.accounts[intent.Account]
		var d *device
		if a != nil {
			d = a.devices[intent.DeviceID]
		}
		authorized := d != nil && !d.banned && !h.userDisabledLocked(intent.DeviceID) && h.hasMailboxPhoneLocked(d) && h.durableScopeLocked(intent.Account, d) == intent.Scope
		online := authorized && d.conn != nil
		policy := h.userPolicyLocked(intent.DeviceID)
		h.mu.Unlock()
		validPolicy := queueEnabled && intent.Source != nil && intent.Source.Mode == "open" && policy.Mode == "open" && policy.Revision == intent.Source.PolicyRevision
		expired := item.deadline <= nowMs()
		if !expired && authorized && validPolicy && !online {
			continue
		}
		h.receipts.mu.Lock()
		if h.receipts.closed {
			h.receipts.mu.Unlock()
			return
		}
		e := h.receipts.entries[item.id]
		if e == nil {
			h.receipts.mu.Unlock()
			continue
		}
		result, err := s.db.Exec(`UPDATE offline_queue SET state='claimed' WHERE id=? AND state='queued'`, item.id)
		if err != nil {
			h.receipts.mu.Unlock()
			continue
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			h.receipts.mu.Unlock()
			continue
		}
		if e.completed {
			e.completed = false
			e.done = make(chan struct{})
		}
		h.receipts.workers.Add(1)
		h.receipts.mu.Unlock()
		go func(item row, intent offlineIntent, entry *commandReceipt, expired, authorized, validPolicy bool) {
			defer h.receipts.workers.Done()
			var rep replyBody
			status := 409
			switch {
			case expired:
				rep = replyBody{Error: "离线发送期限已过，指令未发送", Code: "offline_dispatch_expired"}
			case !authorized:
				status = 403
				rep = replyBody{Error: "原手机绑定已失效，指令未发送", Code: "device_authorization_expired"}
			case !validPolicy:
				status = 403
				rep = replyBody{Error: "远程来源策略已更改，指令未发送", Code: "source_policy_changed"}
			default:
				ctx := context.WithValue(context.Background(), acceptedReceiptContextKey{}, true)
				rep, status = h.executeCommandGuarded(ctx, intent.Account, intent.DeviceID, intent.Command, func(dev *device) bool {
					return dev != nil && h.hasMailboxPhoneLocked(dev) && !dev.banned && h.durableScopeLocked(intent.Account, dev) == intent.Scope
				}, "", &h.remoteAccessGate, func() (string, *ExecutionSource) {
					p := h.deviceRemoteAccessPolicy(intent.DeviceID)
					if nowMs() >= item.deadline {
						return "execution_ticket_expired", nil
					}
					if p.Mode != "open" || p.Revision != intent.Source.PolicyRevision {
						return "source_policy_changed", nil
					}
					return "", intent.Source
				})
			}
			known := expired || !authorized || !validPolicy || status == 200 || status == 403 || status == 429
			if s.saveReply(item.id, item.owner, rep, status, known) != nil {
				known = false
			}
			h.receipts.mu.Lock()
			entry.completed = true
			if known {
				h.receipts.cacheReplyLocked(entry, rep, status)
			}
			close(entry.done)
			h.receipts.mu.Unlock()
			state := "completed"
			if !known {
				state = "uncertain"
			}
			_, _ = s.db.Exec(`UPDATE offline_queue SET state=?,body=NULL,bytes=0 WHERE id=?`, state, item.id)
		}(item, intent, e, expired, authorized, validPolicy)
	}
}
