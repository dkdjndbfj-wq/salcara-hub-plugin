package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

type latencyState struct {
	Samples    uint64  `json:"samples"`
	LastMS     float64 `json:"last_ms"`
	MeanMS     float64 `json:"mean_ms"`
	MeasuredAt int64   `json:"measured_at"`
	Kind       string  `json:"kind"`
	Outcome    string  `json:"outcome"`
}

type AdminAudit struct {
	At        int64  `json:"at"`
	Action    string `json:"action"`
	DeviceRef string `json:"device_ref"`
	Reason    string `json:"reason"`
	Actor     string `json:"actor"`
}
type AdminQuery struct {
	Query  string `json:"query"`
	Filter string `json:"filter"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

var credentialLike = regexp.MustCompile(`(?i)(sk-[a-z0-9_-]{8,}|bearer\s|[a-f0-9]{64})`)

type AdminTool struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Version   string `json:"version"`
}
type AdminDevice struct {
	Ref              string       `json:"ref"`
	Name             string       `json:"name"`
	OS               string       `json:"os"`
	Version          string       `json:"version"`
	Online           bool         `json:"online"`
	LastSeen         int64        `json:"last_seen"`
	Paired           bool         `json:"paired"`
	Banned           bool         `json:"banned"`
	BanReason        string       `json:"ban_reason,omitempty"`
	BannedAt         int64        `json:"banned_at,omitempty"`
	RunningSessions  int          `json:"running_sessions"`
	WaitingApprovals int          `json:"waiting_approvals"`
	Tools            []AdminTool  `json:"tools"`
	Latency          latencyState `json:"latency"`
}

func adminRef(accountID, deviceID string) string {
	hash := sha256.Sum256([]byte(accountID + "\x00" + deviceID))
	return hex.EncodeToString(hash[:])
}
func (h *Hub) adminDeviceLocked(ref string) (string, *device) {
	if len(ref) != 64 {
		return "", nil
	}
	for accountID, a := range h.accounts {
		for id, dev := range a.devices {
			if adminRef(accountID, id) == ref {
				return accountID, dev
			}
		}
	}
	return "", nil
}
func sessionCounts(a *account, dev *device) (running, waiting int) {
	if dev.conn == nil || dev.banned {
		return
	}
	for id, s := range a.sessions {
		if strings.HasPrefix(id, dev.info.DeviceID+"\x00") {
			switch s.status {
			case "running":
				running++
			case "waiting_approval":
				waiting++
			}
		}
	}
	return
}

// AdminSnapshot contains explicitly allowlisted metadata only. It is not a
// public handler and never includes prompt/title/project/credential fields.
func (h *Hub) AdminSnapshot() any {
	return h.AdminSnapshotPage(AdminQuery{})
}
func (h *Hub) AdminSnapshotPage(q AdminQuery) any {
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 100
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	q.Query = strings.ToLower(safeLabel(strings.TrimSpace(q.Query), 128))
	h.mu.Lock()
	defer h.mu.Unlock()
	devices := []AdminDevice{}
	for accountID, a := range h.accounts {
		for id, d := range a.devices {
			if q.Query != "" && !strings.Contains(strings.ToLower(d.info.Name+" "+d.info.OS), q.Query) {
				continue
			}
			switch q.Filter {
			case "online":
				if d.conn == nil || d.banned {
					continue
				}
			case "offline":
				if d.conn != nil && !d.banned {
					continue
				}
			case "paired":
				if !d.hasPair || d.banned {
					continue
				}
			case "banned":
				if !d.banned {
					continue
				}
			}
			running, waiting := sessionCounts(a, d)
			tools := []AdminTool{}
			_ = json.Unmarshal(d.info.Tools, &tools)
			if len(tools) > 16 {
				tools = tools[:16]
			}
			for i := range tools {
				tools[i].ID = safeLabel(tools[i].ID, 64)
				tools[i].Name = safeLabel(tools[i].Name, 128)
				tools[i].Version = safeLabel(tools[i].Version, 64)
			}
			devices = append(devices, AdminDevice{Ref: adminRef(accountID, id), Name: safeLabel(d.info.Name, 256), OS: safeLabel(d.info.OS, 128), Version: safeLabel(d.info.Version, 128), Online: d.conn != nil && !d.banned, LastSeen: d.lastSeen, Paired: d.hasPair && !d.banned, Banned: d.banned, BanReason: d.banReason, BannedAt: d.bannedAt, RunningSessions: running, WaitingApprovals: waiting, Tools: tools, Latency: d.latency})
		}
	}
	sort.Slice(devices, func(i, j int) bool {
		if devices[i].Online != devices[j].Online {
			return devices[i].Online
		}
		return devices[i].Ref < devices[j].Ref
	})
	total := len(devices)
	start := min(q.Offset, total)
	end := min(start+q.Limit, total)
	devices = devices[start:end]
	audit := append([]AdminAudit(nil), h.adminAudit...)
	if len(audit) > 50 {
		audit = audit[len(audit)-50:]
	}
	return map[string]any{"devices": devices, "total": total, "offset": start, "limit": q.Limit, "audit": audit, "stats": h.statsLocked(), "metrics_since": h.seqBase / 1000, "auth_mode": "device-pairing", "latency_scope": "Hub ↔ Bridge command request/reply; not phone RTT", "session_scope": "online computer last-reported status; not process inspection"}
}

func safeLabel(value string, max int) string {
	value = strings.Map(func(c rune) rune {
		if c < 0x20 || c == 0x7f {
			return -1
		}
		return c
	}, value)
	if len(value) > max {
		value = value[:max]
	}
	return value
}

// AdminAction is invoked only through the trusted host's admin RPC forwarding
// channel. Ban applies to the device ID across namespaces, not proxy IPs.
func (h *Hub) AdminAction(ctx context.Context, action, ref, reason string) (any, error) {
	return h.AdminActionAs(ctx, action, ref, reason, "host-admin")
}
func (h *Hub) AdminActionAs(ctx context.Context, action, ref, reason, actor string) (any, error) {
	switch action {
	case "ban", "unban", "disconnect", "revoke", "ping":
	default:
		return nil, errors.New("管理操作无效")
	}
	if action != "ping" {
		reason = strings.TrimSpace(reason)
		if reason == "" || len(reason) > 256 || safeLabel(reason, 256) != reason || credentialLike.MatchString(reason) {
			return nil, errors.New("请输入1–256字节的操作原因（不含控制字符）")
		}
	}
	h.mu.Lock()
	if len(actor) > 64 || !regexp.MustCompile(`^(host-admin(:[0-9]+)?|standalone-admin)$`).MatchString(actor) {
		actor = "host-admin"
	}
	accountID, dev := h.adminDeviceLocked(ref)
	if dev == nil {
		h.mu.Unlock()
		return nil, errors.New("设备不存在")
	}
	if action == "ping" {
		id := dev.info.DeviceID
		h.mu.Unlock()
		nonce := newID()
		command, _ := json.Marshal(map[string]string{"type": "device.ping", "nonce": nonce})
		pingCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		rep, status := h.executeCommand(pingCtx, accountID, id, command, nil, nonce)
		h.mu.Lock()
		h.auditLocked(AdminAudit{At: nowMs(), Action: action, DeviceRef: ref, Reason: "连接往返测量", Actor: actor})
		h.mu.Unlock()
		h.markDirty()
		if status != 200 || !rep.OK {
			return nil, errors.New("电脑未在线、已封禁、版本不支持或未正确回应测量")
		}
		h.mu.Lock()
		latency := dev.latency
		h.mu.Unlock()
		return map[string]any{"ok": true, "latency": latency}, nil
	}
	id := dev.info.DeviceID
	for acct, a := range h.accounts {
		for deviceID, d := range a.devices {
			if deviceID != id {
				continue
			}
			switch action {
			case "ban":
				d.banned, d.banReason, d.bannedAt = true, reason, nowMs()
				h.bannedDeviceIDs[id] = true
				h.revokePairLocked(acct, d)
				h.disconnectLocked(acct, d)
			case "unban":
				d.banned, d.banReason, d.bannedAt = false, "", 0
				delete(h.bannedDeviceIDs, id)
			case "disconnect":
				h.disconnectLocked(acct, d)
			case "revoke":
				h.revokePairLocked(acct, d)
			}
		}
	}
	h.auditLocked(AdminAudit{At: nowMs(), Action: action, DeviceRef: ref, Reason: reason, Actor: actor})
	h.mu.Unlock()
	h.markDirty()
	if h.cfg.DataDir != "" {
		if err := h.saveNowError(); err != nil {
			return nil, errors.New("操作已生效，但持久化失败；请检查站点数据目录")
		}
	}
	h.log.Info("admin device action", "action", action, "deviceRef", ref) // Do not log operator-entered reason.
	return map[string]any{"ok": true, "persistent": h.cfg.DataDir != ""}, nil
}

func (h *Hub) auditLocked(entry AdminAudit) {
	h.adminAudit = append(h.adminAudit, entry)
	if len(h.adminAudit) > 200 {
		h.adminAudit = h.adminAudit[len(h.adminAudit)-200:]
	}
}

func (h *Hub) disconnectLocked(accountID string, dev *device) {
	if dev.conn != nil {
		dev.conn.close()
		dev.conn = nil
		dev.lastSeen = nowMs()
	}
	for sub := range h.accounts[accountID].apps {
		if sub.deviceID == dev.info.DeviceID {
			delete(h.accounts[accountID].apps, sub)
			sub.close()
		}
	}
	for id, p := range h.pending {
		if p.account == accountID && p.deviceID == dev.info.DeviceID {
			delete(h.pending, id)
			select {
			case p.ch <- replyBody{DeviceID: dev.info.DeviceID, OK: false, Error: "电脑连接已由管理员断开"}:
			default:
			}
		}
	}
}

func (h *Hub) recordCommandLocked(dev *device, elapsed time.Duration, success bool, kind string) {
	if dev == nil {
		return
	}
	ms := float64(elapsed.Microseconds()) / 1000
	l := &dev.latency
	l.Samples++
	l.LastMS = ms
	l.MeanMS += (ms - l.MeanMS) / float64(l.Samples)
	l.MeasuredAt = nowMs()
	l.Kind = kind
	l.Outcome = "ok"
	if success {
		h.commandSuccess++
	} else {
		l.Outcome = "failed"
		h.commandFailures++
	}
}

func (h *Hub) executeCommand(ctx context.Context, accountID, deviceID string, command json.RawMessage, authorize func(*device) bool, expectedNonce string) (replyBody, int) {
	var typ struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(command, &typ)
	id := newID()
	p := &pendingCmd{account: accountID, deviceID: deviceID, ch: make(chan replyBody, 1)}
	h.mu.Lock()
	a := h.accounts[accountID]
	var dev *device
	if a != nil {
		dev = a.devices[deviceID]
	}
	if dev == nil || dev.banned || (authorize != nil && !authorize(dev)) {
		h.mu.Unlock()
		return replyBody{Error: "这台电脑尚未配对或已被封禁"}, 403
	}
	conn := dev.conn
	standby := false
	if conn == nil && authorize != nil {
		conn = h.standbyCommandLocked(dev, command)
		standby = conn != nil
	}
	if conn == nil {
		h.mu.Unlock()
		return replyBody{Error: "电脑不在线"}, 409
	}
	if len(h.pending) >= h.resource.MaxPendingCommands || h.resource.MaxAccountPendingCommands > 0 && h.accountPendingCountLocked(accountID) >= h.resource.MaxAccountPendingCommands {
		h.mu.Unlock()
		return replyBody{Error: "本站待处理指令过多"}, 429
	}
	if ctx.Err() != nil {
		h.mu.Unlock()
		return replyBody{Error: "请求已取消"}, 499
	}
	h.pending[id] = p
	// Binding metadata comes from the authenticated station record, never the
	// phone's command JSON. Administrator diagnostics do not grant phone access.
	envelope := commandEnvelope{CommandID: id, DeviceID: deviceID, Command: command, TS: nowMs(), Phone: authorize != nil}
	if authorize != nil {
		envelope.BindingID, envelope.PhoneHash = dev.bindingID, dev.phoneHash
	}
	env, _ := json.Marshal(envelope)
	h.mu.Unlock()
	defer func() { h.mu.Lock(); delete(h.pending, id); h.mu.Unlock() }()
	started := time.Now()
	select {
	case conn.cmds <- env:
	case <-conn.closed:
		return replyBody{Error: "电脑不在线"}, 409
	default:
		return replyBody{Error: "电脑正忙"}, 503
	}
	h.log.Info("command sent", "account", accountID, "deviceId", deviceID, "commandId", id, "type", typ.Type)
	timer := time.NewTimer(h.cfg.CommandTimeout)
	defer timer.Stop()
	select {
	case rep := <-p.ch:
		if expectedNonce != "" {
			var result struct {
				Nonce string `json:"nonce"`
			}
			if json.Unmarshal(rep.Result, &result) != nil || result.Nonce != expectedNonce {
				rep.OK = false
				rep.Error = "电脑测量回显无效"
			}
		}
		kind := "command"
		if typ.Type == "device.ping" {
			kind = "device.ping"
		}
		h.mu.Lock()
		// A reply may already have left pending when an administrator bans,
		// revokes or disconnects the device. Revalidate before releasing any
		// response content; a queued success must not outlive authorization.
		current := h.accounts[accountID]
		if current == nil || current.devices[deviceID] != dev || dev.banned || (authorize != nil && !authorize(dev)) {
			h.mu.Unlock()
			return replyBody{Error: "设备授权已失效"}, 403
		}
		if !standby && dev.conn != conn || standby && dev.standby != conn {
			h.mu.Unlock()
			return replyBody{Error: "电脑连接已断开或更换"}, 409
		}
		h.recordCommandLocked(dev, time.Since(started), rep.OK, kind)
		h.mu.Unlock()
		return rep, 200
	case <-timer.C:
		h.mu.Lock()
		h.commandTimeouts++
		h.mu.Unlock()
		return replyBody{Error: "电脑没有响应"}, 504
	case <-ctx.Done():
		return replyBody{Error: "请求已取消"}, 499
	case <-conn.closed:
		return replyBody{Error: "电脑连接已断开"}, 409
	case <-h.done:
		return replyBody{Error: "服务正在重启"}, 503
	}
}

// ServeAdminHTTP has no public registration in Hub.ServeHTTP. The plugin calls
// it only after verifying private host transport provenance.
func (h *Hub) ServeAdminHTTP(w http.ResponseWriter, r *http.Request) {
	// Private requests participate in the same drain barrier as public ones.
	// ApplyConfig must not load a replacement before a prior admin mutation
	// has finished and flushed, nor allow a stale Hub pointer after Close.
	h.lifecycleMu.Lock()
	if h.stopped {
		h.lifecycleMu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "服务正在重启，请稍后重试")
		return
	}
	h.activeRequests.Add(1)
	h.lifecycleMu.Unlock()
	defer h.activeRequests.Done()
	if r.URL.Path == "/salcara-hub/_admin/snapshot" && (r.Method == http.MethodGet || r.Method == http.MethodPost) {
		var q AdminQuery
		if r.Method == http.MethodPost && !decodeBody(w, r, 4096, &q) {
			return
		}
		writeJSON(w, 200, h.AdminSnapshotPage(q))
		return
	}
	if r.URL.Path != "/salcara-hub/_admin/action" || r.Method != http.MethodPost {
		writeError(w, 404, "管理接口不存在")
		return
	}
	var in struct {
		Action string `json:"action"`
		Ref    string `json:"ref"`
		Reason string `json:"reason"`
		Actor  string `json:"actor"`
	}
	if !decodeBody(w, r, 4096, &in) {
		return
	}
	result, err := h.AdminActionAs(r.Context(), in.Action, in.Ref, in.Reason, in.Actor)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, result)
}
