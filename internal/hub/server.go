package hub

import (
	"container/list"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Hub is the HTTP handler and in-memory state of the service.
type Hub struct {
	push                 *pushState     // FCM push to paired phones (push.go); nil-safe
	deviceMetadataBytes  int64          // guarded by mu; includes raw fields and map/identity reserves
	deviceMetadataLimit  int64          // fixed across resource modes; small test fixtures may lower it
	cleanup              DeviceCleanup  // inactive-computer cleanup policy, guarded by mu
	eventCacheBytes      int64          // guarded by mu
	cacheOrder           *list.List     // least recently updated payload cache first, under mu
	resource             ResourcePreset // guarded by mu, never mutate shared cfg live
	cfg                  Config
	log                  *slog.Logger
	auth                 *authenticator
	limiter              *failLimiter
	enrollLimiter        *failLimiter
	apiEnrollmentLimiter *failLimiter
	routes               map[string]route
	diagnostics          diagnosticAdmission // ephemeral; guarded by mu
	ingress              *ingressRegistry

	mu sync.Mutex
	// enrollmentMu serializes first-time device admissions while their one-time
	// enrollment claims are durably consumed.
	enrollmentMu sync.Mutex
	// remoteAccessGate serializes source-policy changes with the final
	// receipt/dispatch boundary. It is deliberately separate from mu: policy
	// evaluation may take mu, while the guarded dispatch must never hold mu
	// while waiting for an administrator's mode change.
	remoteAccessGate sync.RWMutex
	sourceChallenges map[string]*sourceChallengeState // ephemeral, bounded, guarded by mu
	// Only authenticated Bridge acquisition replies can use this ephemeral signer.
	officialPublicKey  ed25519.PublicKey
	officialPrivateKey ed25519.PrivateKey
	accounts           map[string]*account
	bridgeStreams      int // admitted Bridge SSE handlers, including replaced connections until drain; guarded by mu
	appStreams         int // admitted SSE handlers, including removed subscribers/owners until drain; guarded by mu
	eventWaiters       int // active long-poll handlers, including accounts removed during drain; guarded by mu
	pending            map[string]*pendingCmd
	receipts           *commandReceipts
	durable            *durableStore
	pairCodes          map[string]*pairAttempt
	pairTickets        map[[32]byte]string
	pairTokens         map[[32]byte]pairIdentity
	botGroups          map[string]*botGroup
	// botSigningKey is persisted only in the Bot data volume and is used to
	// sign binding package metadata. It is never included in a package.
	botSigningKey           []byte
	enrollmentClaims        map[[32]byte]time.Time
	enrollmentOwners        map[[32]byte]string
	apiEnrollmentChallenges map[string]apiEnrollmentChallenge
	managedUsers            managedUserState
	bannedDeviceIDs         map[string]bool
	adminAudit              []AdminAudit
	saveMu                  sync.Mutex
	// Process-local revocation epochs, guarded by mu. A failed persistence must
	// not turn a repeated phone revoke into an apparently final 401/403.
	pairRevocationEpoch      uint64
	savedPairRevocationEpoch uint64
	requestErrors            uint64
	commandSuccess           uint64
	commandFailures          uint64
	commandTimeouts          uint64
	// Persistent installations reserve sequence blocks before publishing any
	// event. The clock alone cannot fence crashes or clock rollback.
	seqBase    int64
	seqCeiling int64 // guarded by mu; durable exclusive process reservation

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
	if err := ValidateDesktopCompatibility(cfg.DesktopMinimumVersion, cfg.DesktopUpdateURL); err != nil {
		return nil, err
	}
	if !validDeploymentMode(cfg.DeploymentMode) {
		return nil, errors.New("invalid Hub deployment mode")
	}
	if cfg.DeploymentMode == "bot" && !validateBotHubURL(cfg.PublicURL, "") {
		return nil, errors.New("bot deployment requires a public HTTPS Hub URL")
	}
	policy, err := ResolveRemoteAccessPolicy(cfg.RemoteAccess, cfg.PublicURL, cfg.Prefix)
	if err != nil {
		return nil, err
	}
	cfg.RemoteAccess = policy
	officialPublic, officialPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, errors.New("cannot initialize official source signer")
	}
	resource := legacyResourceMode()
	if cfg.ResourceMode != "" {
		var ok bool
		resource, ok = ResourceMode(cfg.ResourceMode)
		if !ok {
			return nil, errors.New("invalid Hub resource mode")
		}
	}
	h := &Hub{
		deviceMetadataLimit:     maxDeviceMetadataBytes,
		cleanup:                 DeviceCleanup{UnpairedDays: cfg.CleanupUnpairedDays, PairedDays: cfg.CleanupPairedDays},
		cacheOrder:              list.New(),
		resource:                resource,
		cfg:                     cfg,
		officialPublicKey:       officialPublic,
		officialPrivateKey:      officialPrivate,
		log:                     cfg.Logger,
		auth:                    newAuthenticator(&cfg),
		limiter:                 newFailLimiter(cfg.AuthFailLimit, cfg.AuthFailWindow),
		enrollLimiter:           newFailLimiter(10, time.Minute),
		apiEnrollmentLimiter:    newFailLimiter(30, time.Minute),
		accounts:                map[string]*account{},
		pending:                 map[string]*pendingCmd{},
		pairCodes:               map[string]*pairAttempt{},
		pairTickets:             map[[32]byte]string{},
		pairTokens:              map[[32]byte]pairIdentity{},
		botGroups:               map[string]*botGroup{},
		enrollmentClaims:        map[[32]byte]time.Time{},
		enrollmentOwners:        map[[32]byte]string{},
		apiEnrollmentChallenges: map[string]apiEnrollmentChallenge{},
		bannedDeviceIDs:         map[string]bool{},
		seqBase:                 time.Now().UnixMilli() * 1000,
		done:                    make(chan struct{}),
		dirty:                   make(chan struct{}, 1),
		saverDone:               make(chan struct{}),
	}
	h.auth.cfg = &h.cfg
	h.routes = map[string]route{
		"/ping":                        {http.MethodGet, false, h.handlePing},
		"/discovery/policy":            {http.MethodGet, false, h.handleDiscoveryPolicy},
		"/ingress/manifest":            {http.MethodGet, false, h.handleIngressManifest},
		"/ingress/identity":            {http.MethodGet, false, h.handleIngressIdentity},
		"/device/register":             {http.MethodPost, false, h.handleDeviceRegister},
		"/device/enrollment/challenge": {http.MethodPost, false, h.handleAPIEnrollmentChallenge},
		"/app/pair/qr":                 {http.MethodPost, false, h.handleQRPair},
		"/app/pair/revoke":             {http.MethodPost, true, h.handleAppPairRevoke},
		"/me":                          {http.MethodGet, true, h.handleMe},
		"/bridge/register":             {http.MethodPost, true, h.handleRegister},
		"/bridge/stream":               {http.MethodGet, true, h.handleBridgeStream},
		"/bridge/standby":              {http.MethodGet, true, h.handleBridgeStandby},
		"/bridge/pair/status":          {http.MethodGet, true, h.handlePairStatus},
		"/bridge/events":               {http.MethodPost, true, h.handleBridgeEvents},
		"/bridge/blobs":                {"GET, POST", true, h.handleBlobs},
		"/app/blobs":                   {"GET, POST", true, h.handleBlobs},
		"/bridge/reply":                {http.MethodPost, true, h.handleBridgeReply},
		"/bridge/pair/start":           {http.MethodPost, true, h.handlePairStart},
		"/bridge/pair/sync":            {http.MethodPost, true, h.handlePairSync},
		"/bridge/pair/revoke":          {http.MethodPost, true, h.handlePairRevoke},
		"/app/pair/confirm":            {http.MethodPost, true, h.handlePairConfirm},
		"/app/devices":                 {http.MethodGet, true, h.handleAppDevices},
		"/app/diagnostics":             {http.MethodPost, true, h.handleAppDiagnostics},
		"/app/stream":                  {http.MethodGet, true, h.handleAppStream},
		"/app/commands":                {http.MethodPost, true, h.handleAppCommands},
		"/app/commands/prepare":        {http.MethodPost, true, h.handleOperationPrepare},
		"/app/operations":              {http.MethodGet, true, h.handleOperationRead},
		"/app/operations/ack":          {http.MethodPost, true, h.handleOperationAck},
		"/app/source/challenge":        {http.MethodPost, true, h.handleSourceChallenge},
		"/app/source/grant":            {http.MethodPost, true, h.handleSourceGrant},
		"/bridge/source/challenge":     {http.MethodGet, true, h.handleBridgeSourceChallenge},
		"/app/events":                  {http.MethodGet, true, h.handleAppEvents},
		"/app/events/ack":              {http.MethodPost, true, h.handleEventAck},
		"/app/push/register":           {http.MethodPost, true, h.handlePushRegister},
		"/bot/computer/bind":           {http.MethodPost, false, h.handleBotComputerBind},
		"/bot/phone/bind":              {http.MethodPost, false, h.handleBotPhoneBind},
		"/bot/phone/revoke":            {http.MethodPost, true, h.handleBotPhoneRevoke},
	}
	if err := h.loadDevices(); err != nil {
		return nil, err
	}
	if err := h.loadManagedUsers(); err != nil {
		return nil, err
	}
	if err := h.loadEnrollmentClaims(); err != nil {
		return nil, err
	}
	if err := h.loadBotState(); err != nil {
		return nil, err
	}
	if err := h.ensureBotSigningKey(); err != nil {
		return nil, err
	}
	if err := h.initializeEventSequence(); err != nil {
		return nil, err
	}
	h.receipts, err = newCommandReceipts(h.cfg.DataDir)
	if err != nil {
		return nil, err
	}
	h.durable, err = newDurableStore(h.cfg.DataDir)
	if err != nil {
		return nil, err
	}
	h.receipts.store = h.durable
	if h.push, err = newPushState(&h.cfg); err != nil {
		h.durable.close()
		return nil, err
	}
	if h.push.enabled() {
		go h.runPush(h.done)
	}
	ingressURL := h.cfg.PublicURL
	ingressRoot := false
	if public, err := url.Parse(ingressURL); err == nil {
		ingressRoot = h.cfg.Prefix == "" && public.Path == ""
	}
	if ingressURL != "" && (ingressRoot || h.cfg.Prefix != "" && strings.HasSuffix(ingressURL, h.cfg.Prefix)) {
		ingressURL += "/v1"
	}
	h.ingress = newIngressRegistry(h.cfg.DataDir, ingressURL)
	go h.saver()
	go h.janitor()
	if h.durable != nil {
		go h.runOfflineQueue()
	}
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
		h.ingress.close()
		h.receipts.close()
		h.activeRequests.Wait()
		h.durable.close()
		<-h.saverDone
		// Requests accepted before shutdown can finish after the saver observes
		// done. Flush their last mutations only after all handlers have drained.
		if h.cfg.DataDir != "" {
			h.saveNow()
			_ = h.saveBotStateError()
		}
	})
}

// IsRunning reports whether the Hub still accepts requests.
func (h *Hub) IsRunning() bool {
	h.lifecycleMu.Lock()
	defer h.lifecycleMu.Unlock()
	return !h.stopped
}

func (h *Hub) janitor() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	nextCleanup := time.Now().Add(5 * time.Minute) // let computers reconnect after a restart first
	nextStorageCleanup := time.Now().Add(5 * time.Minute)
	for {
		select {
		case now := <-t.C:
			if !now.Before(nextStorageCleanup) {
				nextStorageCleanup = now.Add(5 * time.Minute)
				_ = h.RunStorageCleanup()
			}
			if !now.Before(nextCleanup) {
				nextCleanup = now.Add(cleanupSweepEvery)
				h.RunDeviceCleanup()
			}
			h.auth.sweep(now)
			h.limiter.sweep(now)
			h.enrollLimiter.sweep(now)
			h.apiEnrollmentLimiter.sweep(now)
			h.mu.Lock()
			h.sweepSourceChallengesLocked(now.Unix())
			h.sweepAPIEnrollmentChallengesLocked(now.Unix())
			for _, account := range h.accounts {
				for _, dev := range account.devices {
					if dev.standby != nil && now.UnixMilli()-dev.standbyAt > standbyTTL.Milliseconds() {
						dev.standby.close()
						dev.standby = nil
					}
				}
			}
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
		writeCodedError(rw, http.StatusServiceUnavailable, "remote_unavailable", "服务正在重启，请稍后重试")
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
	hd.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Last-Event-ID, X-Salcara-Device-Secret, X-Salcara-Device-Id, X-Salcara-Pair-Token, X-Salcara-Bot-Token, X-Salcara-Bot-Group, X-Salcara-Bot-Invite, X-Salcara-Bot-Signature, X-Salcara-Enrollment-Claim, X-Salcara-Enrollment-Grant, X-Salcara-Event-Scope")
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
	if r.Method != rt.method && !(rt.method == "GET, POST" && (r.Method == http.MethodGet || r.Method == http.MethodPost)) {
		w.Header().Set("Allow", rt.method)
		writeError(w, http.StatusMethodNotAllowed, "不支持的请求方法")
		return
	}
	var acct string
	if rt.auth {
		deviceAuth := false
		for _, name := range []string{"X-Salcara-Device-Id", "X-Salcara-Pair-Token", "X-Salcara-Bot-Token"} {
			if len(r.Header.Values(name)) > 1 || r.Header.Get(name) != "" {
				deviceAuth = true
			}
		}
		// Legacy clients send a bearer key and a device secret, without a
		// device ID. Preserve that explicit mode only where legacy is enabled;
		// a secret alone must never trigger model API authentication.
		secrets := r.Header.Values("X-Salcara-Device-Secret")
		if len(secrets) > 1 || len(secrets) == 1 && secrets[0] != "" && (h.cfg.DisableLegacyAPIKey || bearerKey(r) == "") {
			deviceAuth = true
		}
		if deviceAuth {
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
	if h.cfg.DisableLegacyAPIKey {
		writeCodedError(w, http.StatusUnauthorized, "device_credentials_required", "请使用已配对的设备凭据；本站不接受模型 API Key 登录")
		return "", false
	}
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

// decodeCommandRequest applies the stricter duplicate/case-confusion checks
// required at the remote execution boundary. Other Hub request bodies retain
// their backwards-compatible decoder; only commands can cause work on a
// paired computer and therefore cannot use last-key-wins JSON semantics.
func decodeCommandRequest(w http.ResponseWriter, r *http.Request, req *commandReq) bool {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(60 * time.Second))
	defer rc.SetReadDeadline(time.Time{}) //nolint:errcheck
	r.Body = http.MaxBytesReader(w, r.Body, maxDefaultBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "请求内容太大")
		} else {
			writeError(w, http.StatusBadRequest, "请求格式不正确，需要 JSON")
		}
		return false
	}
	fields, err := strictObjectFields(raw, []string{"deviceId", "command", "requestId", "sourceGrant", "receiptOnly", "executionTicket"})
	if err != nil {
		writeError(w, http.StatusBadRequest, "请求必须是一个完整且无重复字段的 JSON 对象")
		return false
	}
	if err := json.Unmarshal(raw, req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式不正确，需要 JSON")
		return false
	}
	if receiptOnly, ok := fields["receiptOnly"]; ok && string(receiptOnly) != "true" && string(receiptOnly) != "false" {
		writeError(w, http.StatusBadRequest, "receiptOnly 必须是布尔值")
		return false
	}
	if source, ok := fields["sourceGrant"]; ok {
		req.sourceGrantRaw = append(json.RawMessage(nil), source...)
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

func (h *Hub) handlePing(w http.ResponseWriter, r *http.Request, _ string) {
	caps, ttl := h.commandCapabilities()
	caps = append([]string{"device.identity.v1", "pair.qr.v1", "session.remote.v1", "pair.revoke.v1", "pair.status.v1", "pair.single-phone.v1", "pair.connections.v1"}, caps...)
	caps = append(caps, "source.grant.acquire.v1", "station.guard.v1", "station.guard.v2", "station.handover.v2", "session.steer.v1", "session.context.v1", "session.actions.v1", "session.workflows.v1", "session.side-question.v1", "session.cooperation.v1")
	caps = append(caps, "session.side-question.persistent.v1", "session.side-question.tools.v1")
	caps = append(caps, "context.references.diff.v1")
	if h.push.enabled() {
		caps = append(caps, "push.fcm.v1")
	}
	authModes := []string{"device-pairing"}
	if !h.cfg.DisableLegacyAPIKey {
		authModes = append(authModes, "legacy-api-key")
	}
	caps = append(caps, "discovery.policy.v1")
	if h.IngressState().Configured {
		caps = append(caps, "ingress.signed-manifest.v1")
	}
	remoteAccess := h.CurrentRemoteAccessPolicy()
	enrollmentDiscovery := h.apiEnrollmentDiscovery()
	if enrollmentDiscovery != nil {
		caps = append(caps, "device.enrollment.api.v1")
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "salcara-hub", "version": Version, "protocol": "salcara-remote", "protocolVersion": 1, "authModes": authModes, "capabilities": caps, "commandIdempotencyTTLSeconds": ttl,
		"desktopCompatibility": h.desktopCompatibility(),
		"remoteAccess":         discoveryPolicyView(remoteAccess), "deviceEnrollment": enrollmentDiscovery})
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
	h.enrollmentMu.Lock()
	defer h.enrollmentMu.Unlock()
	h.mu.Lock()
	st, status, deferred := h.registerDeviceLocked(acct, d, secretHash)
	h.mu.Unlock()
	if status != 0 {
		writeError(w, status, "电脑身份验证失败或本站设备容量已满")
		return
	}
	if err := h.confirmDeviceRegistration(acct, d.DeviceID); err != nil {
		writeCodedError(w, 503, "remote_unavailable", "电脑登记尚未安全保存，请稍后重试")
		return
	}
	if deferred {
		h.log.Warn("device metadata update deferred: site metadata budget full", "account", acct, "deviceId", d.DeviceID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": acct, "device": st, "metadataDeferred": true, "eventScopeRequired": h.cfg.DeploymentMode == "bot", "eventScope": h.bridgeEventScope(acct, d.DeviceID)})
		return
	}
	h.markDirty()
	h.log.Info("device registered", "account", acct, "deviceId", d.DeviceID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": acct, "device": st, "eventScopeRequired": h.cfg.DeploymentMode == "bot", "eventScope": h.bridgeEventScope(acct, d.DeviceID)})
}

func (h *Hub) handleBridgeStream(w http.ResponseWriter, r *http.Request, acct string) {
	deviceID := r.URL.Query().Get("deviceId")
	if !validID(deviceID) {
		writeError(w, http.StatusBadRequest, "deviceId 不能为空")
		return
	}
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
	if !h.matchesReadyDeviceSecret(dev, r) {
		h.mu.Unlock()
		writeError(w, http.StatusForbidden, "电脑身份验证失败")
		return
	}
	if dev.banned || h.userDisabledLocked(deviceID) {
		h.mu.Unlock()
		writeError(w, 403, "这台电脑已被本站封禁")
		return
	}
	if h.resource.MaxBridgeStreams > 0 && h.bridgeStreamCountLocked() >= h.resource.MaxBridgeStreams {
		// A reconnect cannot borrow a slot still occupied by a blocked handler.
		// Ask only this device's prior connection to drain, so a retry can take
		// its released slot even when the global budget is completely full.
		if dev.conn != nil {
			dev.conn.close()
		}
		h.mu.Unlock()
		w.Header().Set("Retry-After", "2")
		writeError(w, http.StatusTooManyRequests, "本站电脑连接已达当前资源模式上限，请稍后重试")
		return
	}
	conn := &bridgeConn{cmds: make(chan []byte, h.resource.BridgeQueueMessages), closed: make(chan struct{}), pairChanged: make(chan struct{}, 1)}
	old := dev.conn
	dev.conn = conn
	h.bridgeStreams++
	dev.lastSeen = nowMs()
	a.broadcastDeviceLocked(dev)
	h.mu.Unlock()
	if old != nil {
		old.close() // only one live stream per device: the newest wins
	}
	h.log.Info("bridge connected", "account", acct, "deviceId", deviceID, "replaced", old != nil)

	defer func() {
		h.mu.Lock()
		h.bridgeStreams--
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
	// Initial snapshot restores state after reconnect; changes ride the already
	// open Bridge stream, with no extra polling connections on the server.
	sendPair := func() error {
		h.mu.Lock()
		state := h.pairStateLocked(acct, dev)
		scope := h.durableScopeLocked(acct, dev)
		h.mu.Unlock()
		hello, _ := json.Marshal(map[string]any{"eventScope": scope, "eventScopeRequired": h.cfg.DeploymentMode == "bot"})
		if err := sse.send("hello", hello); err != nil {
			return err
		}
		if state == nil {
			return nil // Pending issuance must not install or revoke a Bridge identity.
		}
		data, _ := json.Marshal(state)
		return sse.send("pair.status", data)
	}
	if sendPair() != nil {
		return
	}
	ping := time.NewTicker(h.cfg.PingInterval)
	defer ping.Stop()
	for {
		select {
		case env := <-conn.cmds:
			// A phone command can arrive immediately after claiming the QR. Put
			// the authenticated pair snapshot on the same ordered SSE stream
			// before that command so the bridge installs its phone identity before
			// attempting the read/write.
			var peek struct {
				Phone bool `json:"phone"`
			}
			if json.Unmarshal(env, &peek) == nil && peek.Phone {
				if sendPair() != nil {
					return
				}
			}
			if sse.send("command", env) != nil {
				return
			}
		case <-conn.pairChanged:
			if sendPair() != nil {
				return
			}
		case <-ping.C:
			if sendPair() != nil {
				return
			}
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
	EventScope string            `json:"eventScope,omitempty"`
	DeviceID   string            `json:"deviceId"`
	BatchID    string            `json:"batchId,omitempty"`
	Events     []json.RawMessage `json:"events"`
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
	if body.BatchID != "" && !requestUUID.MatchString(body.BatchID) {
		writeError(w, http.StatusBadRequest, "batchId 必须是标准小写 UUID")
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
	if !h.matchesReadyDeviceSecret(dev, r) {
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
	if !h.eventScopeMatchesLocked(acct, dev, body.EventScope) {
		h.mu.Unlock()
		writeEventScopeChanged(w)
		return
	}
	var last int64
	// A revoked station must not receive fresh conversation events. This also
	// closes the race between a local authorization check and the HTTP upload.
	if dev.bindingID != "" && (!dev.hasPair || r.Header.Get("X-Salcara-Binding-Id") != dev.bindingID || r.Header.Get("X-Salcara-Phone-Hash") != dev.phoneHash) {
		h.mu.Unlock()
		writeError(w, 403, "手机绑定已更换或撤销")
		return
	}
	digestInput, _ := json.Marshal(body.Events)
	digestBytes := sha256.Sum256(append([]byte(body.DeviceID+"\x00"), digestInput...))
	digest := hex.EncodeToString(digestBytes[:])
	if h.durable != nil && body.BatchID != "" {
		receipt, found, err := h.durable.findBatch(h.durableScopeLocked(acct, dev), body.BatchID)
		if err != nil {
			h.mu.Unlock()
			writeError(w, 503, "事件回执暂时不可用")
			return
		}
		if found {
			h.mu.Unlock()
			if receipt.digest != digest {
				writeError(w, 409, "事件批次编号已用于其他内容")
				return
			}
			writeJSON(w, 200, map[string]any{"ok": true, "accepted": receipt.accepted, "lastSeq": receipt.lastSeq, "duplicate": true})
			return
		}
	}
	if body.BatchID != "" {
		if receipt, ok := a.eventBatches[body.BatchID]; ok {
			if receipt.digest != digest {
				h.mu.Unlock()
				writeError(w, http.StatusConflict, "事件批次编号已用于其他内容")
				return
			}
			h.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accepted": receipt.accepted, "lastSeq": receipt.lastSeq, "duplicate": true})
			return
		}
	}
	// Reserve the complete batch before publishing any event. A storage error
	// is retryable and must never acknowledge a partially accepted batch.
	if err := h.reserveEventSequenceLocked(a.seq + int64(len(parsed))); err != nil {
		h.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "事件游标暂时无法保存，请稍后重试")
		return
	}
	if h.durable != nil {
		events := make([]*storedEvent, 0, len(parsed))
		for i, m := range parsed {
			e, err := prepareStoredEvent(body.DeviceID, a.seq+int64(i)+1, m)
			if err != nil {
				h.mu.Unlock()
				writeError(w, 400, "事件格式不正确")
				return
			}
			events = append(events, e)
		}
		if err := h.durable.storeEvents(h.durableScopeLocked(acct, dev), body.BatchID, digest, events); err != nil {
			h.mu.Unlock()
			writeJSON(w, 503, map[string]any{"code": "event_storage_unavailable", "error": "事件尚未保存，请保留本地批次后重试", "retryable": true})
			return
		}
		for i, e := range events {
			a.publishEventLocked(e, parsed[i])
			last = e.seq
			h.pushEventLocked(acct, a, dev, parsed[i], e.seq)
		}
	} else {
		for _, m := range parsed {
			e, err := a.appendEventLocked(body.DeviceID, m)
			if err != nil {
				continue
			}
			last = e.seq
			h.pushEventLocked(acct, a, dev, m, e.seq)
		}
	}
	if body.BatchID != "" {
		if a.eventBatches == nil {
			a.eventBatches = map[string]eventBatchReceipt{}
		}
		a.eventBatches[body.BatchID] = eventBatchReceipt{digest: digest, accepted: len(parsed), lastSeq: last}
		a.eventBatchOrder = append(a.eventBatchOrder, body.BatchID)
		if len(a.eventBatchOrder) > 256 {
			old := a.eventBatchOrder[0]
			a.eventBatchOrder = a.eventBatchOrder[1:]
			delete(a.eventBatches, old)
		}
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
	Code      string          `json:"code,omitempty"`
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
	if len(rep.Code) > 128 || strings.ContainsAny(rep.Code, "\r\n\x00") {
		writeError(w, http.StatusBadRequest, "错误代码格式不正确")
		return
	}
	h.mu.Lock()
	p := h.pending[rep.CommandID]
	if p != nil && p.account == acct && (rep.DeviceID == "" || rep.DeviceID == p.deviceID) &&
		h.matchesReadyDeviceSecret(h.accountLocked(acct).devices[p.deviceID], r) {
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
	if strings.HasPrefix(acct, "bot:") {
		g := h.botGroupForTokenLocked(r)
		if g == nil {
			h.mu.Unlock()
			writeError(w, http.StatusForbidden, "Bot 手机凭据已失效")
			return
		}
		list := h.botDevicesLocked(g)
		h.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"devices": list, "groupId": g.ID})
		return
	}
	dev := h.pairedDeviceLocked(acct, r)
	userDisabled := h.managedPairUserDisabledLocked(acct, r)
	var list []DeviceStatus
	if dev != nil {
		list = []DeviceStatus{dev.status()}
	}
	h.mu.Unlock()
	if dev == nil {
		if userDisabled {
			writeManagedUserDisabled(w)
			return
		}
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
	actualAcct := acct
	var dev *device
	if strings.HasPrefix(acct, "bot:") {
		actualAcct, dev = h.botAccountAndDeviceLocked(acct, r, r.URL.Query().Get("deviceId"))
	} else {
		dev = h.pairedDeviceLocked(acct, r)
	}
	if dev == nil {
		h.mu.Unlock()
		writeError(w, http.StatusForbidden, "请先输入电脑端显示的配对码")
		return
	}
	deviceID := dev.info.DeviceID
	a := h.accountLocked(actualAcct)
	if h.appReadLimitReachedLocked(a) {
		h.mu.Unlock()
		writeAppReadCapacity(w)
		return
	}
	sub := &appSub{ch: make(chan sseMsg, h.resource.AppQueueMessages), closed: make(chan struct{}), deviceID: deviceID, maxQueuedBytes: h.resource.AppQueueBytes}
	a.apps[sub] = struct{}{}
	a.appStreams++
	h.appStreams++
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
		// Unsubscribe/revoke may remove sub or even a before Write/Flush returns.
		// Only the original handler releases its original owner's admission.
		a.appStreams--
		h.appStreams--
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
			h.mu.Lock()
			sub.queuedBytes -= int64(len(m.data))
			if sub.queuedBytes < 0 {
				sub.queuedBytes = 0
			}
			h.mu.Unlock()
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
			stillPaired := false
			if strings.HasPrefix(acct, "bot:") {
				_, current := h.botAccountAndDeviceLocked(acct, r, deviceID)
				stillPaired = current == dev
			} else {
				stillPaired = h.pairedDeviceLocked(acct, r) == dev
			}
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
	ExecutionTicket string             `json:"executionTicket,omitempty"`
	DeviceID        string             `json:"deviceId"`
	Command         json.RawMessage    `json:"command"`
	RequestID       string             `json:"requestId,omitempty"`
	ReceiptOnly     bool               `json:"receiptOnly,omitempty"`
	SourceGrant     *SignedSourceGrant `json:"sourceGrant,omitempty"`
	// sourceGrantRaw is retained only for strict duplicate-key validation. It
	// is never forwarded to the Bridge or persisted in a receipt.
	sourceGrantRaw json.RawMessage
}

type commandEnvelope struct {
	EventScope string          `json:"eventScope,omitempty"`
	CommandID  string          `json:"commandId"`
	DeviceID   string          `json:"deviceId"`
	Command    json.RawMessage `json:"command"`
	TS         int64           `json:"ts"`
	BindingID  string          `json:"bindingId,omitempty"`
	PhoneHash  string          `json:"phoneHash,omitempty"`
	Phone      bool            `json:"phone,omitempty"`
	// Source is generated by the Hub after source-policy validation. It is not
	// read from command JSON and contains no API key, grant payload or signature.
	Source *ExecutionSource `json:"source,omitempty"`
}

func (h *Hub) handleAppCommands(w http.ResponseWriter, r *http.Request, acct string) {
	var req commandReq
	if !decodeCommandRequest(w, r, &req) {
		return
	}
	if !validID(req.DeviceID) {
		writeError(w, http.StatusBadRequest, "deviceId 不能为空")
		return
	}
	execAcct := acct
	if strings.HasPrefix(acct, "bot:") {
		h.mu.Lock()
		resolved, dev := h.botAccountAndDeviceLocked(acct, r, req.DeviceID)
		h.mu.Unlock()
		if dev == nil {
			writeError(w, http.StatusForbidden, "目标电脑不属于该 Bot 组")
			return
		}
		execAcct = resolved
	}
	var cmd struct {
		Type string `json:"type"`
	}
	if len(req.Command) == 0 || json.Unmarshal(req.Command, &cmd) != nil || cmd.Type == "" {
		writeError(w, http.StatusBadRequest, "command 格式不正确，需要 type")
		return
	}
	if cmd.Type == "source.grant.acquire" {
		sourceFailure(w, http.StatusForbidden, "source_command_unsupported")
		return
	}
	if cmd.Type == "session.revert" && !requestUUID.MatchString(req.RequestID) {
		// Replacing a sent turn must never take the untracked command path.
		// A cancelled wait or an uncertain reply remains the same operation.
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "修改已发送消息需要持久化操作编号", "code": "invalid_request_id", "retryable": false})
		return
	}
	if req.ReceiptOnly {
		// Receipt recovery has no execution authority. Do not acquire or retire
		// a source grant, create a receipt, or enter a command dispatch path.
		h.handleReceiptCommand(w, r, execAcct, req, nil)
		return
	}
	var validTicket bool
	r, validTicket = h.validateOperationCommit(w, r, execAcct, req)
	if !validTicket {
		return
	}
	defer h.retireCompletedSourceChallenge(execAcct, r, req)
	sourceCheck := func() (string, *ExecutionSource) {
		if deadline, ok := r.Context().Value(ticketDeadlineKey{}).(int64); ok && deadline <= nowMs() {
			return "execution_ticket_expired", nil
		}
		return h.executionSourceFor(acct, r, req)
	}
	if code, _ := sourceCheck(); code != "" && !h.cachedReceiptReplay(execAcct, r, req) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "远程来源策略拒绝此操作", "code": code, "retryable": false})
		return
	}
	if h.handleStoredAttachment(w, r, acct, req) {
		return
	}
	if h.maybeQueueOffline(w, r, acct, req, sourceCheck) {
		return
	}
	if h.durable != nil && req.RequestID != "" {
		_, scope, ok := h.appStorageIdentity(acct, r, req.DeviceID)
		if ok {
			if err := h.durable.retainCommandBlobs(scope, req.Command, "operation:"+req.RequestID); err != nil {
				writeError(w, 503, "图片引用尚未保存，指令未发送")
				return
			}
		}
	}
	if h.handleReceiptCommand(w, r, execAcct, req, sourceCheck) {
		return
	}

	rep, status := h.executeCommandGuarded(r.Context(), execAcct, req.DeviceID, req.Command, func(dev *device) bool {
		if strings.HasPrefix(acct, "bot:") {
			_, current := h.botAccountAndDeviceLocked(acct, r, req.DeviceID)
			return current == dev
		}
		return h.pairedDeviceLocked(execAcct, r) == dev
	}, "", &h.remoteAccessGate, sourceCheck)
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
	if !rep.OK && rep.Code != "" {
		out["code"] = rep.Code
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
	actualAcct := acct
	var target *device
	if strings.HasPrefix(acct, "bot:") {
		actualAcct, target = h.botAccountAndDeviceLocked(acct, r, deviceID)
	} else {
		a := h.accounts[acct]
		if a != nil {
			target = a.devices[deviceID]
		}
		if target != nil && h.pairedDeviceLocked(acct, r) != target {
			target = nil
		}
	}
	a := h.accounts[actualAcct]
	if a == nil || target == nil {
		userDisabled := h.managedPairUserDisabledLocked(acct, r)
		h.mu.Unlock()
		if userDisabled {
			writeManagedUserDisabled(w)
			return
		}
		writeError(w, http.StatusForbidden, "这台电脑尚未与手机配对")
		return
	}
	page := h.readEventPageLocked(actualAcct, deviceID, sessionKey, after, limit)
	// Existing feature snapshots are hints returned with this read, not unseen
	// session events. Only a new signal during this wait may wake an empty page.
	initialFeatureSeq := a.seq
	wait := eventWait(r)
	if len(page.Events) > 0 || page.ResetRequired || wait <= 0 {
		h.mu.Unlock()
		writeJSON(w, http.StatusOK, page)
		return
	}
	if r.Context().Err() != nil {
		h.mu.Unlock()
		return
	}
	// Long polling consumes the same phone-read budget as SSE. Only an empty
	// read that will actually wait reserves a slot; immediate pages stay usable
	// under saturation. Keep the original owner even if pairing/account state
	// is replaced while waiting, and release once on every exit path.
	if h.appReadLimitReachedLocked(a) {
		h.mu.Unlock()
		writeAppReadCapacity(w)
		return
	}
	waitOwner := a
	waitOwner.eventWaiters++
	h.eventWaiters++
	defer func() {
		h.mu.Lock()
		waitOwner.eventWaiters--
		h.eventWaiters--
		h.mu.Unlock()
	}()
	// Long poll: hold an empty read open until this account receives a new
	// event, then answer once. One request per watching phone, no SSE.
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		wake := a.wakeChanLocked()
		h.mu.Unlock()
		select {
		case <-wake:
		case <-deadline.C:
			// The pair may have been revoked at the same time as the timer. Do
			// one final authorization check before returning an empty page; a
			// timeout must never turn a revoked token into a successful read.
			h.mu.Lock()
			actualAcct = acct
			if strings.HasPrefix(acct, "bot:") {
				actualAcct, _ = h.botAccountAndDeviceLocked(acct, r, deviceID)
			}
			a = h.accounts[actualAcct]
			authorized := a != nil && a.devices[deviceID] != nil
			if authorized && !strings.HasPrefix(acct, "bot:") {
				authorized = h.pairedDeviceLocked(acct, r) == a.devices[deviceID]
			}
			if authorized {
				page.FeatureChanges = h.featureChangesLocked(a, deviceID)
			}
			userDisabled := h.managedPairUserDisabledLocked(acct, r)
			h.mu.Unlock()
			if !authorized {
				if userDisabled {
					writeManagedUserDisabled(w)
					return
				}
				writeError(w, http.StatusForbidden, "这台电脑尚未与手机配对")
				return
			}
			writeJSON(w, http.StatusOK, page)
			return
		case <-r.Context().Done():
			return
		case <-h.done:
			writeJSON(w, http.StatusOK, page)
			return
		}
		h.mu.Lock()
		actualAcct = acct
		if strings.HasPrefix(acct, "bot:") {
			actualAcct, _ = h.botAccountAndDeviceLocked(acct, r, deviceID)
		}
		a = h.accounts[actualAcct]
		authorized := a != nil && a.devices[deviceID] != nil
		if authorized && !strings.HasPrefix(acct, "bot:") {
			authorized = h.pairedDeviceLocked(acct, r) == a.devices[deviceID]
		}
		if !authorized {
			userDisabled := h.managedPairUserDisabledLocked(acct, r)
			h.mu.Unlock()
			if userDisabled {
				writeManagedUserDisabled(w)
				return
			}
			writeError(w, http.StatusForbidden, "这台电脑尚未与手机配对")
			return
		}
		page = h.readEventPageLocked(actualAcct, deviceID, sessionKey, after, limit)
		if len(page.Events) > 0 || page.ResetRequired || featureChangesAfter(page.FeatureChanges, initialFeatureSeq) {
			h.mu.Unlock()
			writeJSON(w, http.StatusOK, page)
			return
		}
	}
}

func writeAppReadCapacity(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "2")
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"error": "同时打开的连接太多了，请稍后重试", "code": "app_read_capacity", "retryable": true,
	})
}

// eventWait parses the optional long-poll wait in seconds (0–25).
func eventWait(r *http.Request) time.Duration {
	n, err := strconv.Atoi(r.URL.Query().Get("wait"))
	if err != nil || n <= 0 {
		return 0
	}
	if n > 25 {
		n = 25
	}
	return time.Duration(n) * time.Second
}
