package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	maxCommandReceipts      = 4096
	maxOwnerCommandReceipts = 256
	maxCachedCommandBytes   = 16 << 20
	maxCachedReplyBytes     = 2 << 20
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
type commandReceipts struct {
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
		if json.Unmarshal(data, &disk) != nil || disk.Version != 1 || !hexHash(disk.Owner) || !hexHash(disk.Hash) || disk.ExpiresAt > now+commandReceiptTTL.Milliseconds()+60000 {
			return nil, errors.New("invalid command receipt record")
		}
		if disk.ExpiresAt <= now {
			_ = os.Remove(path)
			continue
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
	data, err := json.Marshal(value)
	return string(data), err
}
func (s *commandReceipts) sweepLocked(now int64) {
	for key, receipt := range s.entries {
		if receipt.completed && receipt.ExpiresAt <= now {
			if receipt.cached {
				s.bytes -= len(receipt.reply.Result) + len(receipt.reply.Error)
			}
			delete(s.entries, key)
			_ = os.Remove(filepath.Join(s.dir, key+".json"))
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
	if len(rep.Result)+len(rep.Error) > maxCachedReplyBytes {
		return
	}
	needed := len(rep.Result) + len(rep.Error)
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
		s.bytes -= len(victim.reply.Result) + len(victim.reply.Error)
		victim.cached = false
		victim.reply = replyBody{}
	}
	rep.Result = append(json.RawMessage(nil), rep.Result...)
	entry.reply, entry.status, entry.cached = rep, status, true
	s.bytes += needed
}
func (s *commandReceipts) close() { s.mu.Lock(); s.closed = true; s.mu.Unlock(); s.workers.Wait() }

func (h *Hub) handleReceiptCommand(w http.ResponseWriter, r *http.Request, acct string, req commandReq) bool {
	if req.RequestID == "" {
		return false
	}
	if !requestUUID.MatchString(req.RequestID) {
		writeJSON(w, 400, map[string]any{"error": "requestId 必须是标准小写 UUID", "code": "invalid_request_id", "retryable": false})
		return true
	}
	// Retain only headers required for authorization, not the cancelled body/context.
	authReq := r.Clone(context.Background())
	authReq.Body = nil
	authorize := func(dev *device) bool { return h.authorizeAppCommand(acct, authReq, dev) }
	credential := shaHex(r.Header.Get("Authorization") + "\x00" + r.Header.Get("X-Salcara-Pair-Token"))
	rep, status, code := h.executeReceipt(r.Context(), acct, req.DeviceID, credential, req.RequestID, req.Command, authorize)
	if status == 499 {
		return true
	}
	if code == "" && strings.HasPrefix(rep.Error, "command_delivery_uncertain") {
		code = "command_delivery_uncertain"
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
func (h *Hub) commandCapabilities() ([]string, int64) {
	capabilities := []string{"events.cursor.v1", "events.wait.v1"}
	if h.receipts != nil && h.receipts.dir != "" {
		capabilities = append(capabilities, "commands.idempotency.v1")
	}
	return capabilities, int64(commandReceiptTTL / time.Second)
}

// Once dispatched, the operation outlives an individual phone HTTP connection.
// A retry joins it. Authorization is checked again before releasing any result.
func (h *Hub) executeReceipt(ctx context.Context, accountID, deviceID, credential, requestID string, command json.RawMessage, authorize func(*device) bool) (replyBody, int, string) {
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
		h.mu.Lock()
		a := h.accounts[accountID]
		online := a != nil && a.devices[deviceID] != nil && a.devices[deviceID].conn != nil
		h.mu.Unlock()
		if !online {
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
			s.mu.Unlock()
			return replyBody{Error: "安全重试记录已达本站容量，请稍后重试"}, 429, "command_receipt_capacity"
		}
		entry = &commandReceipt{durableCommandReceipt: durableCommandReceipt{Version: 1, Owner: owner, Hash: payloadHash, ExpiresAt: nowMs() + commandReceiptTTL.Milliseconds()}, done: make(chan struct{})}
		if err := s.persistLocked(key, entry.durableCommandReceipt); err != nil {
			s.mu.Unlock()
			h.log.Error("command receipt write failed")
			return replyBody{Error: "安全重试记录无法保存，指令未发送"}, 503, "command_receipt_unavailable"
		}
		s.entries[key] = entry
		s.workers.Add(1)
		go func() {
			defer s.workers.Done()
			rep, status := h.executeCommand(context.Background(), accountID, deviceID, command, authorize, "")
			s.mu.Lock()
			entry.completed = true
			// A timeout or interrupted relay may have delivered the operation already.
			if status == 200 || status == 403 || status == 429 || (status == 409 && rep.Error == "电脑不在线") || (status == 503 && rep.Error == "电脑正忙") {
				s.cacheReplyLocked(entry, rep, status)
			}
			close(entry.done)
			s.mu.Unlock()
		}()
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
	cached, rep, status := entry.cached, entry.reply, entry.status
	s.mu.Unlock()
	if !cached {
		return replyBody{Error: "指令可能已被电脑接收，请刷新原会话确认；不会重复发送"}, 409, "command_delivery_uncertain"
	}
	return rep, status, ""
}
