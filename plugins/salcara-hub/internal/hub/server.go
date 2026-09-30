package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Hub is the HTTP handler and in-memory state of the service.
type Hub struct {
	cfg           Config
	log           *slog.Logger
	auth          *authenticator
	limiter       *failLimiter
	enrollLimiter *failLimiter
	routes        map[string]route

	mu              sync.Mutex
	accounts        map[string]*account
	pending         map[string]*pendingCmd
	receipts        *commandReceipts
	pairCodes       map[string]*pairAttempt
	pairTickets     map[[32]byte]string
	pairTokens      map[[32]byte]pairIdentity
	bannedDeviceIDs map[string]bool
	adminAudit      []AdminAudit
	saveMu          sync.Mutex
	requestErrors   uint64
	commandSuccess  uint64
	commandFailures uint64
	commandTimeouts uint64
	// seqBase makes seq monotonic across restarts without persisting events:
	// every account starts at (process start in ms) * 1000, which stays below
	// 2^53 (safe for JavaScript) and above any seq of a previous run.
	seqBase int64

	done           chan struct{}
	closeOnce      sync.Once
	lifecycleMu    sync.Mutex
	activeRequests sync.WaitGroup
	stopped        bool
	dirty          chan struct{}
	saverDone      chan struct{}
}

type pendingCmd struct {
	account  string
	deviceID string
	ch       chan replyBody
}

type route struct {
	method  string
	auth    bool
	handler func(w http.ResponseWriter, r *http.Request, acct string)
}

// New creates a Hub, loading persisted devices from cfg.DataDir.
func New(cfg Config) (*Hub, error) {
	cfg.setDefaults()
	h := &Hub{
		cfg:             cfg,
		log:             cfg.Logger,
		auth:            newAuthenticator(&cfg),
		limiter:         newFailLimiter(cfg.AuthFailLimit, cfg.AuthFailWindow),
		enrollLimiter:   newFailLimiter(10, time.Minute),
		accounts:        map[string]*account{},
		pending:         map[string]*pendingCmd{},
		pairCodes:       map[string]*pairAttempt{},
		pairTickets:     map[[32]byte]string{},
		pairTokens:      map[[32]byte]pairIdentity{},
		bannedDeviceIDs: map[string]bool{},
		seqBase:         time.Now().UnixMilli() * 1000,
		done:            make(chan struct{}),
		dirty:           make(chan struct{}, 1),
		saverDone:       make(chan struct{}),
	}
	h.auth.cfg = &h.cfg
	h.routes = map[string]route{
		"/ping":               {http.MethodGet, false, h.handlePing},
		"/device/register":    {http.MethodPost, false, h.handleDeviceRegister},
		"/app/pair/qr":        {http.MethodPost, false, h.handleQRPair},
		"/app/pair/revoke":    {http.MethodPost, true, h.handleAppPairRevoke},
		"/me":                 {http.MethodGet, true, h.handleMe},
		"/bridge/register":    {http.MethodPost, true, h.handleRegister},
		"/bridge/stream":      {http.MethodGet, true, h.handleBridgeStream},
		"/bridge/events":      {http.MethodPost, true, h.handleBridgeEvents},
		"/bridge/reply":       {http.MethodPost, true, h.handleBridgeReply},
		"/bridge/pair/start":  {http.MethodPost, true, h.handlePairStart},
		"/bridge/pair/revoke": {http.MethodPost, true, h.handlePairRevoke},
		"/app/pair/confirm":   {http.MethodPost, true, h.handlePairConfirm},
		"/app/devices":        {http.MethodGet, true, h.handleAppDevices},
		"/app/stream":         {http.MethodGet, true, h.handleAppStream},
		"/app/commands":       {http.MethodPost, true, h.handleAppCommands},
		"/app/events":         {http.MethodGet, true, h.handleAppEvents},
	}
	if err := h.loadDevices(); err != nil {
		return nil, err
	}
	var err error
	h.receipts, err = newCommandReceipts(h.cfg.DataDir)
	if err != nil {
		return nil, err
	}
	go h.saver()
	go h.janitor()
	return h, nil
}

// Close ends all streams and pending commands and flushes devices.json. Call
// it before http.Server.Shutdown so long-lived SSE handlers return.
func (h *Hub) Close() {
	h.closeOnce.Do(func() {
		h.lifecycleMu.Lock()
		h.stopped = true
		close(h.done)
		h.lifecycleMu.Unlock()
		h.receipts.close()
		h.activeRequests.Wait()
		<-h.saverDone
		// Requests accepted before shutdown can finish after the saver observes
		// done. Flush their last mutations only after all handlers have drained.
		if h.cfg.DataDir != "" {
			h.saveNow()
		}
	})
}

func (h *Hub) janitor() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case now := <-t.C:
			h.auth.sweep(now)
			h.limiter.sweep(now)
			h.enrollLimiter.sweep(now)
			h.mu.Lock()
			for id, attempt := range h.pairCodes {
				if now.After(attempt.expires) {
					h.deletePairAttemptLocked(id)
				}
			}
			h.mu.Unlock()
		case <-h.done:
			return
		}
	}
}

// ---------------------------------------------------------------------------
// HTTP plumbing

type ctxKey struct{}

type reqInfo struct{ account string }

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() { _ = w.FlushError() }

// FlushError lets http.ResponseController report flush failures.
func (w *statusWriter) FlushError() error {
	return http.NewResponseController(w.ResponseWriter).Flush()
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (h *Hub) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	h.lifecycleMu.Lock()
	if h.stopped {
		h.lifecycleMu.Unlock()
		writeError(rw, http.StatusServiceUnavailable, "服务正在重启，请稍后重试")
		return
	}
	h.activeRequests.Add(1)
	h.lifecycleMu.Unlock()
	defer h.activeRequests.Done()
	start := time.Now()
	w := &statusWriter{ResponseWriter: rw}
	info := &reqInfo{}
	r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, info))
	ip := clientIP(r, h.cfg.TrustProxy)
	defer func() {
		if w.status >= 400 {
			h.mu.Lock()
			h.requestErrors++
			h.mu.Unlock()
		}
		h.log.Info("request", "method", r.Method, "path", r.URL.Path, "status", w.status,
			"ms", time.Since(start).Milliseconds(), "ip", ip, "account", info.account)
	}()

	hd := w.Header()
	hd.Set("Access-Control-Allow-Origin", "*")
	hd.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	hd.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Last-Event-ID, X-Salcara-Device-Secret, X-Salcara-Device-Id, X-Salcara-Pair-Token")
	hd.Set("Access-Control-Max-Age", "86400")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	path := r.URL.Path
	if name, prefix, ok := h.downloadName(path); ok {
		h.handleDownload(w, r, name, prefix)
		return
	}
	if h.cfg.Prefix != "" && strings.HasPrefix(path, h.cfg.Prefix+"/") {
		path = path[len(h.cfg.Prefix):]
	}
	if !strings.HasPrefix(path, "/v1/") {
		writeError(w, http.StatusNotFound, "接口不存在")
		return
	}
	localPath := strings.TrimSuffix(path[len("/v1"):], "/")
	rt, ok := h.routes[localPath]
	if !ok {
		writeError(w, http.StatusNotFound, "接口不存在")
		return
	}
	if r.Method != rt.method {
		w.Header().Set("Allow", rt.method)
		writeError(w, http.StatusMethodNotAllowed, "不支持的请求方法")
		return
	}
	var acct string
	if rt.auth {
		if r.Header.Get("X-Salcara-Device-Id") != "" || r.Header.Get("X-Salcara-Pair-Token") != "" {
			acct, ok = h.deviceOnlyAuth(w, r, localPath, ip)
		} else {
			acct, ok = h.authenticate(w, r, ip)
		}
		if !ok {
			return
		}
		info.account = acct
	}
	rt.handler(w, r, acct)
}

func (h *Hub) authenticate(w http.ResponseWriter, r *http.Request, ip string) (string, bool) {
	key := bearerKey(r)
	if key != "" && h.auth.cachedValid(key) {
		return AccountID(key), true
	}
	if h.limiter.blocked(ip) {
		w.Header().Set("Retry-After", strconv.Itoa(int(h.cfg.AuthFailWindow.Seconds())))
		writeError(w, http.StatusTooManyRequests, "验证失败次数太多，请稍后再试")
		return "", false
	}
	if key == "" {
		h.limiter.fail(ip)
		writeError(w, http.StatusUnauthorized, "缺少 API Key，请在 Authorization 头里带上中转站 Key")
		return "", false
	}
	if len(key) > 1024 {
		h.limiter.fail(ip)
		writeError(w, http.StatusUnauthorized, "API Key 无效")
		return "", false
	}
	verdict, err := h.auth.check(r.Context(), key)
	if err != nil {
		if errors.Is(err, errUpstream) {
			h.log.Warn("sub2api key check failed", "ip", ip)
		}
		writeError(w, http.StatusBadGateway, "暂时无法验证 API Key，请稍后再试")
		return "", false
	}
	if verdict == authGroupDenied {
		writeError(w, http.StatusForbidden, "该 API Key 所在分组未开通远程功能")
		return "", false
	}
	if verdict != authAllowed {
		h.limiter.fail(ip)
		writeError(w, http.StatusUnauthorized, "API Key 无效或不是本中转站的用户")
		return "", false
	}
	return AccountID(key), true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// decodeBody reads a JSON body limited to max bytes. On failure it writes the
// error response and returns false.
func decodeBody(w http.ResponseWriter, r *http.Request, max int64, v any) bool {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(60 * time.Second))
	defer rc.SetReadDeadline(time.Time{}) //nolint:errcheck // unsupported writers are fine
	r.Body = http.MaxBytesReader(w, r.Body, max)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "请求内容太大")
		} else {
			writeError(w, http.StatusBadRequest, "请求格式不正确，需要 JSON")
		}
		return false
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		writeError(w, http.StatusBadRequest, "请求必须是一个完整 JSON 对象")
		return false
	}
	return true
}

func validID(s string) bool {
	if s == "" || len(s) > maxIDLen || strings.TrimSpace(s) != s {
		return false
	}
	for _, c := range s {
		if c < 0x21 || c == 0x7f {
			return false
		}
	}
	return true
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// SSE

type sseWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func startSSE(w http.ResponseWriter) *sseWriter {
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream; charset=utf-8")
	hd.Set("Cache-Control", "no-cache, no-store")
	hd.Set("Connection", "keep-alive")
	hd.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	s := &sseWriter{w: w, rc: http.NewResponseController(w)}
	return s
}

func (s *sseWriter) write(p []byte) error {
	_ = s.rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
	if _, err := s.w.Write(p); err != nil {
		return err
	}
	return s.rc.Flush()
}

func (s *sseWriter) comment(text string) error { return s.write([]byte(": " + text + "\n\n")) }

func (s *sseWriter) send(event string, data []byte) error {
	buf := make([]byte, 0, len(event)+len(data)+16)
	buf = append(buf, "event: "...)
	buf = append(buf, event...)
	buf = append(buf, "\ndata: "...)
	buf = append(buf, data...)
	buf = append(buf, "\n\n"...)
	return s.write(buf)
}

// ---------------------------------------------------------------------------
// Handlers

func (h *Hub) handlePing(w http.ResponseWriter, _ *http.Request, _ string) {
	caps, ttl := h.commandCapabilities()
	caps = append([]string{"device.identity.v1", "pair.qr.v1", "session.remote.v1", "pair.revoke.v1"}, caps...)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "salcara-hub", "version": Version, "protocol": "salcara-remote", "protocolVersion": 1, "authModes": []string{"device-pairing", "legacy-api-key"}, "capabilities": caps, "commandIdempotencyTTLSeconds": ttl})
}

func (h *Hub) handleMe(w http.ResponseWriter, _ *http.Request, acct string) {
	writeJSON(w, http.StatusOK, map[string]string{"account": acct})
}

func (h *Hub) handleRegister(w http.ResponseWriter, r *http.Request, acct string) {
	var d Device
	if !decodeBody(w, r, maxDeviceBody, &d) {
		return
	}
	d.DeviceID = strings.TrimSpace(d.DeviceID)
	if !validDeviceInfo(d) {
		writeError(w, http.StatusBadRequest, "deviceId 不能为空")
		return
	}
	if d.Name == "" {
		d.Name = "我的电脑"
	}
	secretHash, secretOK := deviceSecret(r)
	if !secretOK {
		writeError(w, http.StatusForbidden, "电脑缺少身份密钥，请更新电脑端软件")
		return
	}
	h.mu.Lock()
	st, status := h.registerDeviceLocked(acct, d, secretHash)
	h.mu.Unlock()
	if status != 0 {
		writeError(w, status, "电脑身份验证失败或本站设备容量已满")
		return
	}
	h.markDirty()
	h.log.Info("device registered", "account", acct, "deviceId", d.DeviceID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": acct, "device": st})
}

func (h *Hub) handleBridgeStream(w http.ResponseWriter, r *http.Request, acct string) {
	deviceID := r.URL.Query().Get("deviceId")
	if !validID(deviceID) {
		writeError(w, http.StatusBadRequest, "deviceId 不能为空")
		return
	}
	conn := &bridgeConn{cmds: make(chan []byte, commandQueueSize), closed: make(chan struct{})}
	h.mu.Lock()
	a := h.accounts[acct]
	var dev *device
	if a != nil {
		dev = a.devices[deviceID]
	}
	if dev == nil {
		h.mu.Unlock()
		writeError(w, http.StatusNotFound, "这台电脑还没有登记，请先调用 /bridge/register")
		return
	}
	if !matchesDeviceSecret(dev, r) {
		h.mu.Unlock()
		writeError(w, http.StatusForbidden, "电脑身份验证失败")
		return
	}
	if dev.banned {
		h.mu.Unlock()
		writeError(w, 403, "这台电脑已被本站封禁")
		return
	}
	old := dev.conn
	dev.conn = conn
	dev.lastSeen = nowMs()
	a.broadcastDeviceLocked(dev)
	h.mu.Unlock()
	if old != nil {
		old.close() // only one live stream per device: the newest wins
	}
	h.log.Info("bridge connected", "account", acct, "deviceId", deviceID, "replaced", old != nil)

	defer func() {
		h.mu.Lock()
		if dev.conn == conn {
			dev.conn = nil
			dev.lastSeen = nowMs()
			a.broadcastDeviceLocked(dev)
		}
		h.mu.Unlock()
		conn.close()
		h.markDirty()
		h.log.Info("bridge disconnected", "account", acct, "deviceId", deviceID)
	}()

	sse := startSSE(w)
	if sse.comment("connected") != nil {
		return
	}
	ping := time.NewTicker(h.cfg.PingInterval)
	defer ping.Stop()
	for {
		select {
		case env := <-conn.cmds:
			if sse.send("command", env) != nil {
				return
			}
		case <-ping.C:
			// API keys may be moved out of an allowed group while a stream is
			// open. Recheck on heartbeat instead of keeping access indefinitely.
			if !strings.HasPrefix(acct, "device:") {
				if verdict, err := h.auth.check(r.Context(), bearerKey(r)); err != nil || verdict != authAllowed {
					return
				}
			}
			if sse.comment("ping") != nil {
				return
			}
		case <-conn.closed:
			return
		case <-r.Context().Done():
			return
		case <-h.done:
			return
		}
	}
}

type eventsBody struct {
	DeviceID string            `json:"deviceId"`
	Events   []json.RawMessage `json:"events"`
}

func (h *Hub) handleBridgeEvents(w http.ResponseWriter, r *http.Request, acct string) {
	var body eventsBody
	if !decodeBody(w, r, maxEventsBody, &body) {
		return
	}
	if !validID(body.DeviceID) {
		writeError(w, http.StatusBadRequest, "deviceId 不能为空")
		return
	}
	parsed := make([]map[string]json.RawMessage, 0, len(body.Events))
	for _, raw := range body.Events {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil || m == nil {
			writeError(w, http.StatusBadRequest, "事件格式不正确")
			return
		}
		var typ string
		if err := json.Unmarshal(m["type"], &typ); err != nil || typ == "" {
			writeError(w, http.StatusBadRequest, "事件缺少 type")
			return
		}
		if sk, ok := m["sessionKey"]; ok {
			var s string
			if json.Unmarshal(sk, &s) != nil || len(s) > 512 {
				writeError(w, http.StatusBadRequest, "sessionKey 格式不正确")
				return
			}
		}
		parsed = append(parsed, m)
	}
	h.mu.Lock()
	a := h.accounts[acct]
	var dev *device
	if a != nil {
		dev = a.devices[body.DeviceID]
	}
	if dev == nil {
		h.mu.Unlock()
		writeError(w, http.StatusNotFound, "这台电脑还没有登记，请先调用 /bridge/register")
		return
	}
	if !matchesDeviceSecret(dev, r) {
		h.mu.Unlock()
		writeError(w, http.StatusForbidden, "电脑身份验证失败")
		return
	}
	if dev.banned {
		h.mu.Unlock()
		writeError(w, 403, "这台电脑已被本站封禁")
		return
	}
	dev.lastSeen = nowMs()
	var last int64
	for _, m := range parsed {
		e, err := a.appendEventLocked(body.DeviceID, m)
		if err != nil {
			continue
		}
		last = e.seq
	}
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accepted": len(parsed), "lastSeq": last})
}

type replyBody struct {
	DeviceID  string          `json:"deviceId"`
	CommandID string          `json:"commandId"`
	OK        bool            `json:"ok"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
}

func (h *Hub) handleBridgeReply(w http.ResponseWriter, r *http.Request, acct string) {
	var rep replyBody
	if !decodeBody(w, r, maxEventsBody, &rep) {
		return
	}
	if rep.CommandID == "" {
		writeError(w, http.StatusBadRequest, "commandId 不能为空")
		return
	}
	h.mu.Lock()
	p := h.pending[rep.CommandID]
	if p != nil && p.account == acct && (rep.DeviceID == "" || rep.DeviceID == p.deviceID) &&
		matchesDeviceSecret(h.accountLocked(acct).devices[p.deviceID], r) {
		delete(h.pending, rep.CommandID)
		if dev := h.accountLocked(acct).devices[p.deviceID]; dev != nil {
			dev.lastSeen = nowMs()
		}
	} else {
		p = nil
	}
	h.mu.Unlock()
	if p == nil {
		writeError(w, http.StatusNotFound, "命令不存在或已经超时")
		return
	}
	p.ch <- rep // buffered(1), and removed from pending so sent at most once
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Hub) handleAppDevices(w http.ResponseWriter, r *http.Request, acct string) {
	h.mu.Lock()
	dev := h.pairedDeviceLocked(acct, r)
	var list []DeviceStatus
	if dev != nil {
		list = []DeviceStatus{dev.status()}
	}
	h.mu.Unlock()
	if dev == nil {
		writeError(w, http.StatusForbidden, "请先输入电脑端显示的配对码")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": list})
}

func parseAfter(r *http.Request) (int64, bool) {
	s := r.URL.Query().Get("after")
	if s == "" {
		s = r.Header.Get("Last-Event-ID")
	}
	if s == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n >= 0
}

func (h *Hub) handleAppStream(w http.ResponseWriter, r *http.Request, acct string) {
	after, ok := parseAfter(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "after 必须是数字")
		return
	}
	h.mu.Lock()
	dev := h.pairedDeviceLocked(acct, r)
	if dev == nil {
		h.mu.Unlock()
		writeError(w, http.StatusForbidden, "请先输入电脑端显示的配对码")
		return
	}
	deviceID := dev.info.DeviceID
	sub := &appSub{ch: make(chan sseMsg, appQueueSize), closed: make(chan struct{}), deviceID: deviceID}
	a := h.accountLocked(acct)
	if len(a.apps) >= maxAppStreams {
		h.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "同时打开的连接太多了")
		return
	}
	a.apps[sub] = struct{}{}
	devices := []DeviceStatus{dev.status()}
	if after > a.seq {
		after = 0 // a seq from the future (another hub): replay everything we have
	}
	var replayed []*storedEvent
	if a.events != nil {
		replayed = a.events.after(after)
	}
	replay := make([]*storedEvent, 0, len(replayed))
	for _, event := range replayed {
		if event.deviceID == deviceID {
			replay = append(replay, event)
		}
	}
	lastReplayed := a.seq
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(a.apps, sub)
		h.mu.Unlock()
		sub.close()
	}()

	sse := startSSE(w)
	streamStopped := func() bool {
		select {
		case <-sub.closed:
			return true
		case <-r.Context().Done():
			return true
		case <-h.done:
			return true
		default:
			return false
		}
	}
	if sse.comment("connected") != nil {
		return
	}
	for _, d := range devices {
		if streamStopped() {
			return
		}
		b, _ := json.Marshal(d)
		if sse.send("device", b) != nil {
			return
		}
	}
	for _, e := range replay {
		if streamStopped() {
			return
		}
		if sse.send("event", e.data) != nil {
			return
		}
	}
	ping := time.NewTicker(h.cfg.PingInterval)
	defer ping.Stop()
	for {
		select {
		case m := <-sub.ch:
			if streamStopped() {
				return
			}
			if m.seq > 0 && m.seq <= lastReplayed {
				continue
			}
			if sse.send(m.event, m.data) != nil {
				return
			}
		case <-ping.C:
			if bearerKey(r) != "" {
				if verdict, err := h.auth.check(r.Context(), bearerKey(r)); err != nil || verdict != authAllowed {
					return
				}
			}
			h.mu.Lock()
			stillPaired := h.pairedDeviceLocked(acct, r) == dev
			h.mu.Unlock()
			if !stillPaired {
				return
			}
			if sse.comment("ping") != nil {
				return
			}
		case <-sub.closed:
			return // too slow; the app reconnects with ?after=
		case <-r.Context().Done():
			return
		case <-h.done:
			return
		}
	}
}

type commandReq struct {
	DeviceID  string          `json:"deviceId"`
	Command   json.RawMessage `json:"command"`
	RequestID string          `json:"requestId,omitempty"`
}

type commandEnvelope struct {
	CommandID string          `json:"commandId"`
	DeviceID  string          `json:"deviceId"`
	Command   json.RawMessage `json:"command"`
	TS        int64           `json:"ts"`
}

func (h *Hub) handleAppCommands(w http.ResponseWriter, r *http.Request, acct string) {
	var req commandReq
	if !decodeBody(w, r, maxDefaultBody, &req) {
		return
	}
	if !validID(req.DeviceID) {
		writeError(w, http.StatusBadRequest, "deviceId 不能为空")
		return
	}
	var cmd struct {
		Type string `json:"type"`
	}
	if len(req.Command) == 0 || json.Unmarshal(req.Command, &cmd) != nil || cmd.Type == "" {
		writeError(w, http.StatusBadRequest, "command 格式不正确，需要 type")
		return
	}
	if h.handleReceiptCommand(w, r, acct, req) {
		return
	}

	rep, status := h.executeCommand(r.Context(), acct, req.DeviceID, req.Command, func(dev *device) bool { return h.pairedDeviceLocked(acct, r) == dev }, "")
	if status == 499 {
		return
	}
	if status != 200 {
		writeError(w, status, rep.Error)
		return
	}
	out := map[string]any{"ok": rep.OK}
	if len(rep.Result) > 0 && string(rep.Result) != "null" {
		out["result"] = rep.Result
	}
	if rep.Error != "" {
		out["error"] = rep.Error
	} else if !rep.OK {
		out["error"] = "电脑执行失败"
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Hub) handleAppEvents(w http.ResponseWriter, r *http.Request, acct string) {
	q := r.URL.Query()
	deviceID, sessionKey := q.Get("deviceId"), q.Get("sessionKey")
	if !validID(deviceID) || len(sessionKey) > 512 || (sessionKey == "" && q.Get("scope") != "device") {
		writeError(w, http.StatusBadRequest, "deviceId 或 sessionKey 格式不正确")
		return
	}
	after, ok := parseAfter(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "after 必须是数字")
		return
	}
	limit, valid := eventPageLimit(r)
	if !valid {
		writeError(w, 400, "limit 必须为 1 至 500")
		return
	}
	h.mu.Lock()
	a := h.accounts[acct]
	if a == nil || a.devices[deviceID] == nil || h.pairedDeviceLocked(acct, r) != a.devices[deviceID] {
		h.mu.Unlock()
		writeError(w, http.StatusForbidden, "这台电脑尚未与手机配对")
		return
	}
	page := h.readEventPageLocked(acct, deviceID, sessionKey, after, limit)
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, page)
}
