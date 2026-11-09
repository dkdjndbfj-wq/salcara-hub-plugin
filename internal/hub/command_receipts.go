package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	commandReceiptTTL       = 24 * time.Hour
	maxCommandReceipts      = 65536
	maxOwnerCommandReceipts = 4096
	maxCachedCommandBytes   = 16 << 20
	maxCachedReplyBytes     = 2 << 20
	// Use a fixed timestamp bound for disk validation. A wall-clock-relative
	// bound rejects valid receipts after the host clock moves backwards.
	maxCommandReceiptExpiry int64 = 253402300799999 // 9999-12-31T23:59:59.999Z
)

var requestUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var receiptName = regexp.MustCompile(`^[0-9a-f]{64}\.json$`)

type durableCommandReceipt struct {
	Version   int    `json:"version"`
	Owner     string `json:"owner"`
	Hash      string `json:"hash"`
	ExpiresAt int64  `json:"expiresAt"`
}
type commandReceipt struct {
	durableCommandReceipt
	done      chan struct{}
	completed bool
	reply     replyBody
	status    int
	cached    bool
}

// Only a durably accepted operation may keep waiting through SSE replacement.
// This marker changes result delivery, never admission or command dispatch.
type acceptedReceiptContextKey struct{}
type commandReceipts struct {
	store   *durableStore
	mu      sync.Mutex
	dir     string
	entries map[string]*commandReceipt
	bytes   int
	closed  bool
	workers sync.WaitGroup
}

// Disk contains hashes only, never keys, pair tokens, prompts, or replies.
// A surviving receipt after restart is uncertain, never permission to resend.
func newCommandReceipts(dataDir string) (*commandReceipts, error) {
	s := &commandReceipts{entries: map[string]*commandReceipt{}}
	if dataDir == "" {
		return s, nil
	}
	s.dir = filepath.Join(dataDir, "command-receipts")
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, err
	}
	probe, err := os.CreateTemp(s.dir, ".write-check-*")
	if err != nil {
		return nil, err
	}
	probeName := probe.Name()
	err = probe.Close()
	_ = os.Remove(probeName)
	if err != nil {
		return nil, err
	}
	files, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	now := nowMs()
	for _, file := range files {
		if !receiptName.MatchString(file.Name()) {
			continue
		}
		path := filepath.Join(s.dir, file.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1024 {
			return nil, errors.New("invalid command receipt file")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var disk durableCommandReceipt
		if json.Unmarshal(data, &disk) != nil || (disk.Version != 1 && disk.Version != 2) || !hexHash(disk.Owner) || !hexHash(disk.Hash) || disk.ExpiresAt <= 0 || disk.ExpiresAt > maxCommandReceiptExpiry {
			return nil, errors.New("invalid command receipt record")
		}
		// Keep future receipts unchanged, including those issued before a clock
		// rollback. Their hashes must continue preventing duplicate dispatch.
		// Legacy identities have no signed dispatch deadline. Never forget their
		// anti-replay fence, even after the body retention period has passed.
		if disk.ExpiresAt <= now && disk.Version == 2 {
			_ = os.Remove(path)
			continue
		}
		if disk.ExpiresAt <= now {
			disk.ExpiresAt = maxCommandReceiptExpiry
		}
		if len(s.entries) >= maxCommandReceipts {
			return nil, errors.New("command receipt capacity exceeded")
		}
		entry := &commandReceipt{durableCommandReceipt: disk, done: make(chan struct{}), completed: true}
		close(entry.done)
		s.entries[strings.TrimSuffix(file.Name(), ".json")] = entry
	}
	return s, nil
}
func hexHash(s string) bool  { b, err := hex.DecodeString(s); return err == nil && len(b) == 32 }
func shaHex(s string) string { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }
func canonicalCommand(raw json.RawMessage) (string, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return "", errors.New("command contains trailing JSON")
	}
	data, err := json.Marshal(value)
	return string(data), err
}
func (s *commandReceipts) sweepLocked(now int64) {
	for key, receipt := range s.entries {
		if receipt.completed && receipt.ExpiresAt <= now {
			if receipt.cached {
				s.bytes -= len(receipt.reply.Result) + len(receipt.reply.Error) + len(receipt.reply.Code)
			}
			receipt.cached = false
			receipt.reply = replyBody{}
			if receipt.Version == 2 {
				delete(s.entries, key)
				_ = os.Remove(filepath.Join(s.dir, key+".json"))
				continue
			}
			receipt.ExpiresAt = maxCommandReceiptExpiry
			_ = key // keep the durable identity; capacity is preferable to replay
		}
	}
}
func (s *commandReceipts) persistLocked(key string, disk durableCommandReceipt) error {
	data, err := json.Marshal(disk)
	if err != nil {
		return err
	}
	// A complete synced temporary inode is linked atomically and exclusively;
	// crashes before dispatch cannot leave half-written JSON at the final name.
	file, err := os.CreateTemp(s.dir, ".receipt-*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err = file.Chmod(0o600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Link(temporary, filepath.Join(s.dir, key+".json")); err != nil {
		return err
	}
	if dir, err := os.Open(s.dir); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
func (s *commandReceipts) cacheReplyLocked(entry *commandReceipt, rep replyBody, status int) {
	if len(rep.Result)+len(rep.Error)+len(rep.Code) > maxCachedReplyBytes {
		return
	}
	needed := len(rep.Result) + len(rep.Error) + len(rep.Code)
	for s.bytes+needed > maxCachedCommandBytes {
		var victim *commandReceipt
		for _, candidate := range s.entries {
			if candidate.cached && candidate != entry && (victim == nil || candidate.ExpiresAt < victim.ExpiresAt) {
				victim = candidate
			}
		}
		if victim == nil {
			return
		}
		s.bytes -= len(victim.reply.Result) + len(victim.reply.Error) + len(victim.reply.Code)
		victim.cached = false
		victim.reply = replyBody{}
	}
	rep.Result = append(json.RawMessage(nil), rep.Result...)
	entry.reply, entry.status, entry.cached = rep, status, true
	s.bytes += needed
}
func (s *commandReceipts) close() { s.mu.Lock(); s.closed = true; s.mu.Unlock(); s.workers.Wait() }

func (s *commandReceipts) restoreReplyLocked(key string, entry *commandReceipt) {
	if entry == nil || !entry.completed || s.store == nil {
		return
	}
	rep, status, state, err := s.store.reply(key)
	if err != nil {
		return
	}
	if state == "completed" && !entry.cached {
		s.cacheReplyLocked(entry, rep, status)
		if entry.Version == 1 {
			entry.ExpiresAt = maxCommandReceiptExpiry
		}
	}
	if state != "completed" && entry.cached {
		s.bytes -= len(entry.reply.Result) + len(entry.reply.Error) + len(entry.reply.Code)
		entry.cached = false
		entry.reply = replyBody{}
	}
}

// receiptCredential follows the authentication branch used for this app
// request. Headers ignored by that branch must not create a new quota owner.
func (h *Hub) receiptCredential(acct string, r *http.Request) string {
	h.mu.Lock()
	bot := h.botGroupForTokenLocked(r) != nil
	paired := bot || h.pairedDeviceLocked(acct, r) != nil
	h.mu.Unlock()
	if paired {
		credential := r.Header.Get("X-Salcara-Pair-Token")
		if bot {
			credential = r.Header.Get("X-Salcara-Bot-Token")
		}
		return shaHex("\x00" + credential)
	}
	return shaHex("Bearer " + bearerKey(r) + "\x00")
}

func transientSourceRead(typ string) bool {
	return typ == "session.cooperation.prepare" || typ == "session.context" || typ == "models.list" || typ == "features.list" || typ == "session.actions" || typ == "sessions.archived" || typ == "session.workflows" || typ == "session.side-question.catalog" || typ == "session.side-question.read"
}

// Temporary tool conversations retain no Hub receipt or response body. All
// actions still require sourceExecutionCommand authority; their owner-bound
// operation deduplication belongs to the live Bridge conversation.
func transientSourceCommand(typ string) bool {
	return transientSourceRead(typ) || typ == "session.side-question.tools"
}

func (h *Hub) handleReceiptCommand(w http.ResponseWriter, r *http.Request, acct string, req commandReq, sourceCheck func() (string, *ExecutionSource)) bool {
	var metadata struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(req.Command, &metadata) // handleAppCommands has validated the command.
	if req.RequestID == "" && !req.ReceiptOnly && !transientSourceCommand(metadata.Type) {
		return false
	}
	if metadata.Type == "remote.connections.sync" {
		// This authenticated response contains scoped phone credentials. It is
		// reproducible by the Bridge and must never be a durable Hub receipt.
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "连接同步不使用持久化回执", "code": "connection_receipt_unsupported", "retryable": false})
		return true
	}
	if sourceWorkspaceCommand(metadata.Type) || metadata.Type == "workspace.files.list" || metadata.Type == "workspace.files.read" || metadata.Type == "workspace.project.files.list" || metadata.Type == "workspace.project.files.read" {
		// File snapshots are transient read responses, never durable operation
		// receipts. Reject an explicit id instead of storing private file text.
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "工作区操作不使用持久化内容回执", "code": "workspace_receipt_unsupported", "retryable": false})
		return true
	}
	if (req.RequestID != "" || req.ReceiptOnly) && !requestUUID.MatchString(req.RequestID) {
		writeJSON(w, 400, map[string]any{"error": "requestId 必须是标准小写 UUID", "code": "invalid_request_id", "retryable": false})
		return true
	}
	if req.ReceiptOnly && transientSourceCommand(metadata.Type) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "此操作没有持久化回执", "code": "command_receipt_unsupported", "retryable": false})
		return true
	}
	var rep replyBody
	var status int
	var code string
	if transientSourceCommand(metadata.Type) {
		// Restricted temporary commands still need a request ID to bind their
		// source grant. HTTP cancellation retires only this response wait; the
		// Bridge reconciles tool mutations using its live operation identities.
		// Neither tool conversation content nor polling consumes durable receipts.
		rep, status = h.executeTransientSourceCommand(r, acct, req, sourceCheck)
	} else {
		// Retain only headers required for authorization, not the cancelled body/context.
		authReq := r.Clone(context.Background())
		authReq.Body = nil
		authorize := func(dev *device) bool { return h.authorizeAppCommand(acct, authReq, dev) }
		credential := h.receiptCredential(acct, r)
		if req.ReceiptOnly {
			rep, status, code = h.lookupCommandReceipt(acct, req.DeviceID, credential, req.RequestID, req.Command, authorize)
		} else {
			rep, status, code = h.executeReceiptWithPolicy(r.Context(), acct, req.DeviceID, credential, req.RequestID, req.Command, authorize, sourceCheck)
		}
	}
	if status == 499 {
		return true
	}
	if code == "" && strings.HasPrefix(rep.Error, "command_delivery_uncertain") {
		code = "command_delivery_uncertain"
	}
	if code == "" && rep.Error == "远程来源策略拒绝此操作" {
		code = "source_policy_changed"
	}
	if code == "" && !rep.OK {
		code = rep.Code
	}
	if status != 200 {
		writeJSON(w, status, map[string]any{"error": rep.Error, "code": code, "requestId": req.RequestID, "retryable": code == "computer_offline"})
		return true
	}
	out := map[string]any{"ok": rep.OK, "requestId": req.RequestID}
	if len(rep.Result) > 0 && string(rep.Result) != "null" {
		out["result"] = rep.Result
	}
	if rep.Error != "" {
		out["error"] = rep.Error
	} else if !rep.OK {
		out["error"] = "电脑执行失败"
	}
	if code != "" {
		out["code"] = code
		out["retryable"] = false
	}
	writeJSON(w, 200, out)
	return true
}

func (h *Hub) executeTransientSourceCommand(r *http.Request, acct string, req commandReq, sourceCheck func() (string, *ExecutionSource)) (replyBody, int) {
	var challengeID string
	var challenge *sourceChallengeState
	if sourceCheck != nil {
		// Remember the exact nonce object across validation. Caller-supplied
		// fields only nominate a candidate; only the complete restricted source
		// check below grants cleanup authority. A replacement is never retired.
		var candidateID string
		var candidate *sourceChallengeState
		h.remoteAccessGate.RLock()
		if req.SourceGrant != nil {
			grant := *req.SourceGrant
			var err error
			if len(req.sourceGrantRaw) > 0 {
				grant, err = ParseSignedSourceGrant(req.sourceGrantRaw)
			}
			if err == nil {
				if snapshot, err := sourceGrantSnapshot(grant); err == nil && snapshot.ChallengeID != "" {
					h.mu.Lock()
					current := h.sourceChallenges[snapshot.ChallengeID]
					if current != nil && current.account == acct && current.DeviceID == req.DeviceID &&
						current.BindingID == snapshot.BindingID && current.RequestID == req.RequestID &&
						current.CommandHash == snapshot.CommandHash && current.PolicyRevision == snapshot.PolicyRevision && current.Audience == snapshot.Audience {
						candidateID, candidate = snapshot.ChallengeID, current
					}
					h.mu.Unlock()
				}
			}
		}
		code, source := sourceCheck()
		if code == "" && source != nil && source.Mode == "restricted" && candidate != nil {
			h.mu.Lock()
			if h.sourceChallenges[candidateID] == candidate {
				challengeID, challenge = candidateID, candidate
			}
			h.mu.Unlock()
		}
		h.remoteAccessGate.RUnlock()
		if code != "" {
			return replyBody{Error: "远程来源策略拒绝此操作"}, http.StatusForbidden
		}
	}
	defer func() {
		if challenge != nil {
			h.mu.Lock()
			if h.sourceChallenges[challengeID] == challenge {
				delete(h.sourceChallenges, challengeID)
			}
			h.mu.Unlock()
		}
	}()
	// Validate before entering execution so even a request cancelled before
	// dispatch releases its verified nonce instead of exhausting acquisition.
	rep, status := h.executeCommandGuarded(r.Context(), acct, req.DeviceID, req.Command,
		func(dev *device) bool {
			return h.appAuthorizedDeviceLocked(acct, r, req.DeviceID) && dev.info.DeviceID == req.DeviceID
		}, "", &h.remoteAccessGate, sourceCheck)
	if r.Context().Err() != nil {
		return replyBody{Error: "请求已取消"}, 499
	}
	if status == http.StatusOK && sourceCheck != nil {
		// No temporary result may be revealed after its source grant or station
		// policy was revoked while the command was pending.
		h.remoteAccessGate.RLock()
		code, _ := sourceCheck()
		h.remoteAccessGate.RUnlock()
		if r.Context().Err() != nil {
			return replyBody{Error: "请求已取消"}, 499
		}
		if code != "" {
			return replyBody{Error: "远程来源策略拒绝此操作"}, http.StatusForbidden
		}
	}
	return rep, status
}

// cachedReceiptReplay reports whether this exact authenticated owner already
// has a completed, cached receipt for the same request and command. It is used
// only when a new source grant is no longer available (for example, a five
// minute grant expired while the phone was offline). It never admits an
// in-flight/uncertain receipt and never creates or dispatches work; the normal
// handleReceiptCommand path still re-checks device pairing before returning
// the cached result.
func (h *Hub) cachedReceiptReplay(acct string, r *http.Request, req commandReq) bool {
	if req.RequestID == "" || !requestUUID.MatchString(req.RequestID) || h.receipts == nil || h.receipts.dir == "" {
		return false
	}
	var metadata struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(req.Command, &metadata) != nil || transientSourceCommand(metadata.Type) {
		// Even a receipt left by an older Hub cannot authorize a fresh source
		// read after its grant expires or the source is revoked.
		return false
	}
	canonical, err := canonicalCommand(req.Command)
	if err != nil {
		return false
	}
	credential := h.receiptCredential(acct, r)
	owner := shaHex(acct + "\x00" + req.DeviceID + "\x00" + credential)
	key := shaHex(owner + "\x00" + req.RequestID)
	payloadHash := shaHex(canonical)
	h.receipts.mu.Lock()
	h.receipts.sweepLocked(nowMs())
	entry := h.receipts.entries[key]
	h.receipts.restoreReplyLocked(key, entry)
	eligible := entry != nil && entry.completed && entry.cached && entry.Hash == payloadHash && entry.ExpiresAt > nowMs()
	h.receipts.mu.Unlock()
	if !eligible {
		return false
	}
	// Keep lock ordering consistent with executeReceipt: never hold the Hub
	// mutex while waiting for receipt storage, avoiding a policy/replay deadlock.
	h.mu.Lock()
	actualAcct := acct
	if strings.HasPrefix(acct, "bot:") {
		actualAcct, _ = h.botAccountAndDeviceLocked(acct, r, req.DeviceID)
	}
	a := h.accounts[actualAcct]
	dev := a != nil && a.devices[req.DeviceID] != nil
	paired := dev && h.appAuthorizedDeviceLocked(acct, r, req.DeviceID)
	h.mu.Unlock()
	return paired
}
func (h *Hub) commandCapabilities() ([]string, int64) {
	capabilities := []string{"events.cursor.v1", "events.wait.v1", "workspace.explorer.v1"}
	if h.receipts != nil && h.receipts.dir != "" {
		capabilities = append(capabilities, "commands.idempotency.v1")
		capabilities = append(capabilities, "session.revert.v1")
		capabilities = append(capabilities, "commands.receipt-only.v1")
		capabilities = append(capabilities, "station.standby.v1")
	}
	if h.durable != nil {
		capabilities = append(capabilities, "commands.durable.v2", "events.durable.v1", "storage.blobs.v1", "events.binding-scope.v1", "session.projectless.v1")
	}
	return capabilities, int64(commandReceiptTTL / time.Second)
}

// Once dispatched, the operation outlives an individual phone HTTP connection.
// A retry joins it. Authorization is checked again before releasing any result.
func (h *Hub) executeReceipt(ctx context.Context, accountID, deviceID, credential, requestID string, command json.RawMessage, authorize func(*device) bool) (replyBody, int, string) {
	return h.executeReceiptWithPolicy(ctx, accountID, deviceID, credential, requestID, command, authorize, nil)
}

// executeReceiptWithPolicy joins the existing receipt operation as before,
// but supplies a final policy callback for a newly-created receipt. The
// callback is evaluated while remoteAccessGate is held at the dispatch edge,
// so mode changes cannot race between the HTTP-level decision and execution.
func (h *Hub) executeReceiptWithPolicy(ctx context.Context, accountID, deviceID, credential, requestID string, command json.RawMessage, authorize func(*device) bool, sourceCheck func() (string, *ExecutionSource)) (replyBody, int, string) {
	s := h.receipts
	if s == nil || s.dir == "" {
		return replyBody{Error: "本站尚未开启持久化安全重试"}, 503, "idempotency_unavailable"
	}
	canonical, err := canonicalCommand(command)
	if err != nil {
		return replyBody{Error: "command 格式不正确"}, 400, "invalid_command"
	}
	owner := shaHex(accountID + "\x00" + deviceID + "\x00" + credential)
	key, payloadHash := shaHex(owner+"\x00"+requestID), shaHex(canonical)
	authorized := func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		a := h.accounts[accountID]
		return a != nil && a.devices[deviceID] != nil && (authorize == nil || authorize(a.devices[deviceID]))
	}
	if !authorized() {
		return replyBody{Error: "设备授权已失效"}, 403, "device_authorization_expired"
	}
	if s.store != nil {
		h.mu.Lock()
		scope := ""
		if a := h.accounts[accountID]; a != nil {
			scope = h.durableScopeLocked(accountID, a.devices[deviceID])
		}
		h.mu.Unlock()
		if _, err := s.store.db.Exec(`INSERT INTO quota_owners(owner,scope) VALUES(?,?) ON CONFLICT(owner) DO NOTHING`, owner, scope); err != nil {
			return replyBody{Error: "存储归属尚未保存，指令未发送"}, 503, "command_receipt_unavailable"
		}
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return replyBody{Error: "服务正在重启"}, 503, "service_restarting"
	}
	s.sweepLocked(nowMs())
	entry := s.entries[key]
	if entry != nil && entry.Hash != payloadHash {
		s.mu.Unlock()
		return replyBody{Error: "同一个 requestId 不能用于不同指令"}, 409, "request_id_conflict"
	}
	if entry == nil {
		if deadline, ok := ctx.Value(ticketDeadlineKey{}).(int64); ok && deadline <= nowMs() {
			s.mu.Unlock()
			return replyBody{Error: "执行票据已过期，指令未发送"}, 409, "execution_ticket_expired"
		}
		policyGateHeld := false
		if sourceCheck != nil {
			// Revalidate before registering a new receipt. The final dispatch
			// check below repeats this under the same gate after the worker starts.
			h.remoteAccessGate.RLock()
			policyGateHeld = true
			if code, _ := sourceCheck(); code != "" {
				h.remoteAccessGate.RUnlock()
				s.mu.Unlock()
				return replyBody{Error: "远程来源策略拒绝此操作"}, 403, ""
			}
		}
		h.mu.Lock()
		a := h.accounts[accountID]
		online := false
		if a != nil && a.devices[deviceID] != nil {
			dev := a.devices[deviceID]
			online = dev.conn != nil || authorize != nil && h.standbyCommandLocked(dev, command) != nil
		}
		h.mu.Unlock()
		if !online {
			if policyGateHeld {
				h.remoteAccessGate.RUnlock()
			}
			s.mu.Unlock()
			return replyBody{Error: "电脑暂时离线，指令未发送"}, 409, "computer_offline"
		}
		ownerCount := 0
		for _, candidate := range s.entries {
			if candidate.Owner == owner {
				ownerCount++
			}
		}
		if len(s.entries) >= maxCommandReceipts || ownerCount >= maxOwnerCommandReceipts {
			if policyGateHeld {
				h.remoteAccessGate.RUnlock()
			}
			s.mu.Unlock()
			return replyBody{Error: "安全重试记录已达本站容量，请稍后重试"}, 429, "command_receipt_capacity"
		}
		entry = &commandReceipt{durableCommandReceipt: durableCommandReceipt{Version: 1, Owner: owner, Hash: payloadHash, ExpiresAt: nowMs() + commandReceiptTTL.Milliseconds()}, done: make(chan struct{})}
		if _, ok := ctx.Value(ticketDeadlineKey{}).(int64); ok {
			entry.Version = 2
			entry.ExpiresAt = nowMs() + 30*durableDay
		}
		if err := s.persistLocked(key, entry.durableCommandReceipt); err != nil {
			if policyGateHeld {
				h.remoteAccessGate.RUnlock()
			}
			s.mu.Unlock()
			h.log.Error("command receipt write failed")
			return replyBody{Error: "安全重试记录无法保存，指令未发送"}, 503, "command_receipt_unavailable"
		}
		s.entries[key] = entry
		s.workers.Add(1)
		go func() {
			defer s.workers.Done()
			workerCtx := context.WithValue(context.Background(), acceptedReceiptContextKey{}, true)
			rep, status := h.executeCommandGuarded(workerCtx, accountID, deviceID, command, authorize, "", &h.remoteAccessGate, sourceCheck)
			s.mu.Lock()
			entry.completed = true
			// A timeout or interrupted relay may have delivered the operation already.
			known := status == 200 || status == 403 || status == 429 || (status == 409 && rep.Error == "电脑不在线") || (status == 503 && rep.Error == "电脑正忙")
			if s.store != nil && s.store.saveReply(key, owner, rep, status, known) != nil {
				known = false
			}
			if known {
				s.cacheReplyLocked(entry, rep, status)
			}
			close(entry.done)
			s.mu.Unlock()
		}()
		if policyGateHeld {
			h.remoteAccessGate.RUnlock()
		}
	}
	done := entry.done
	s.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
		return replyBody{Error: "请求连接已断开"}, 499, ""
	case <-h.done:
		return replyBody{Error: "服务正在重启"}, 503, "service_restarting"
	}
	if !authorized() {
		return replyBody{Error: "设备授权已失效"}, 403, "device_authorization_expired"
	}
	s.mu.Lock()
	s.restoreReplyLocked(key, entry)
	cached, rep, status := entry.cached, entry.reply, entry.status
	s.mu.Unlock()
	if !cached {
		return replyBody{Error: "指令可能已被电脑接收，请刷新原会话确认；不会重复发送"}, 409, "command_delivery_uncertain"
	}
	return rep, status, ""
}
