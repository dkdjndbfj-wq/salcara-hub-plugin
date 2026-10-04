package standalone

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"salcara/hubplugin/internal/hub"
)

//go:embed web/*
var webFiles embed.FS

// Server separates public device pairing from a standalone admin credential.
// The Hub itself still has no public admin routes. No relay cookies, model
// keys, URL credentials or iframe-injected login parameters are trusted here.
type Server struct {
	cfg                 Config
	hub                 *hub.Hub
	lock                *dataLock
	auth                *adminAuth
	closeOnce           sync.Once
	closeErr            error
	stopped             atomic.Bool
	control             *controlClient
	resourceMu          sync.Mutex
	previousMemoryLimit int64
}

func New(cfg Config, logger *slog.Logger) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	lock, err := acquireDataLock(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	auth, err := newAdminAuth(cfg)
	if err != nil {
		lock.release()
		return nil, err
	}
	complete := false
	defer func() {
		if !complete {
			auth.close()
		}
	}()
	control, err := newControlClient(cfg)
	if err != nil {
		lock.release()
		return nil, err
	}
	mode, err := loadResourceMode(cfg.DataDir, cfg.ResourceMode)
	if err != nil {
		lock.release()
		return nil, err
	}
	cleanup, err := loadDeviceCleanup(cfg.DataDir, cleanupSetting{UnpairedDays: cfg.CleanupUnpairedDays, PairedDays: cfg.CleanupPairedDays})
	if err != nil {
		lock.release()
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	fcm, err := readFCMCredentials(cfg.FCMCredentialsFile)
	if err != nil {
		lock.release()
		return nil, err
	}
	h, err := hub.New(hub.Config{DataDir: cfg.DataDir, Prefix: Prefix, PublicURL: cfg.PublicURL, TrustProxy: cfg.TrustProxy,
		FCMCredentials:      fcm,
		ResourceMode:        mode,
		CleanupUnpairedDays: cleanup.UnpairedDays, CleanupPairedDays: cleanup.PairedDays,
		CommandTimeout: cfg.CommandTimeout, PingInterval: cfg.PingInterval,
		DisableLegacyAPIKey: true, Logger: logger})
	if err != nil {
		lock.release()
		if len(fcm) > 0 && strings.Contains(err.Error(), "FCM") {
			return nil, err
		}
		return nil, errors.New("cannot load Hub pairing data; inspect the persistent volume before restarting")
	}
	preset, _ := hub.ResourceMode(mode)
	previous := debug.SetMemoryLimit(preset.MemoryLimitMiB << 20)
	complete = true
	return &Server{cfg: cfg, hub: h, lock: lock, auth: auth, control: control, previousMemoryLimit: previous}, nil
}

func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.stopped.Store(true)
		s.auth.close()
		s.resourceMu.Lock()
		defer s.resourceMu.Unlock()
		s.hub.Close()
		s.closeErr = s.lock.release()
		if s.control != nil {
			s.control.client.CloseIdleConnections()
		}
		debug.SetMemoryLimit(s.previousMemoryLimit)
	})
	return s.closeErr
}

func headers(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'self'")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
}

func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func failure(w http.ResponseWriter, status int, message string) {
	respond(w, status, map[string]string{"error": message})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	headers(w)
	if s.stopped.Load() {
		failure(w, http.StatusServiceUnavailable, "Hub 正在重启，请稍后重试")
		return
	}
	if r.URL.Path == "/healthz" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			failure(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		// PID is an operational identity, never an authentication credential.
		// The same-container launcher checks it against the exact child PID so
		// another Hub already occupying the port cannot satisfy this probe.
		respond(w, http.StatusOK, map[string]any{"ok": true, "service": "salcara-hub", "product": Product, "version": Version, "pid": os.Getpid()})
		return
	}
	if strings.HasPrefix(r.URL.Path, Prefix+"/v1/") {
		if r.URL.Path == Prefix+"/v1/ping" && r.Method == http.MethodGet {
			buf := &bufferResponse{header: make(http.Header)}
			s.hub.ServeHTTP(buf, r)
			var ping map[string]any
			if buf.status == http.StatusOK && json.Unmarshal(buf.body.Bytes(), &ping) == nil {
				for key, values := range buf.header {
					w.Header()[key] = values
				}
				ping["standalone_version"] = Version
				respond(w, http.StatusOK, ping)
				return
			}
			failure(w, http.StatusServiceUnavailable, "Hub discovery unavailable")
			return
		}
		s.hub.ServeHTTP(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, Prefix+"/_admin/") {
		s.serveAdmin(w, r)
		return
	}
	s.serveWeb(w, r)
}

func (s *Server) serveWeb(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	var file, kind string
	switch path {
	case Prefix, Prefix + "/admin":
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			http.Redirect(w, r, path+"/", http.StatusSeeOther)
			return
		}
	case Prefix + "/":
		file, kind = "help.html", "text/html; charset=utf-8"
	case Prefix + "/admin/":
		file, kind = "index.html", "text/html; charset=utf-8"
	case Prefix + "/admin/app.js":
		file, kind = "app.js", "text/javascript; charset=utf-8"
	case Prefix + "/help.js":
		file, kind = "help.js", "text/javascript; charset=utf-8"
	case Prefix + "/app.css", Prefix + "/admin/app.css":
		file, kind = "app.css", "text/css; charset=utf-8"
	case Prefix + "/icon.svg", Prefix + "/admin/icon.svg":
		file, kind = "icon.svg", "image/svg+xml"
	}
	if file == "" {
		failure(w, http.StatusNotFound, "page not found")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		failure(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		// Sub2API custom iframes can append its own auth_token/user_id. Never
		// evaluate them or retain them in history, response content or logs.
		http.Redirect(w, r, path, http.StatusSeeOther)
		return
	}
	b, err := webFiles.ReadFile("web/" + file)
	if err != nil {
		failure(w, http.StatusNotFound, "page not found")
		return
	}
	w.Header().Set("Content-Type", kind)
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(b)
	}
}

func origin(u *url.URL) (string, error) {
	if u.Scheme != "http" && u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return "", errors.New("invalid origin")
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if port != "" {
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return "", errors.New("invalid port")
		}
	}
	if u.Scheme == "http" && port == "80" || u.Scheme == "https" && port == "443" {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return u.Scheme + "://" + host, nil
}

func (s *Server) sameOrigin(r *http.Request) bool {
	value := r.Header.Get("Origin")
	if value == "" {
		return false
	}
	if len(r.Header.Values("Origin")) != 1 {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return false
	}
	got, err := origin(u)
	if err != nil {
		return false
	}
	var expected string
	if s.cfg.PublicURL != "" {
		u, _ = url.Parse(s.cfg.PublicURL) // validated at construction
		expected, _ = origin(u)
	} else {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		u, err = url.Parse(scheme + "://" + r.Host)
		if err != nil {
			return false
		}
		expected, _ = origin(u)
	}
	return expected != "" && got == expected
}

func (s *Server) serveAdmin(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, Prefix+"/_admin/v1/auth/") {
		s.serveAuth(w, r)
		return
	}
	if _, ok := s.authorizeAdmin(w, r); !ok {
		return
	}
	switch r.URL.Path {
	case Prefix + "/_admin/v1/state":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			failure(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		q, err := adminQuery(r.URL.Query())
		if err != nil {
			failure(w, http.StatusBadRequest, "管理查询参数无效")
			return
		}
		raw, _ := json.Marshal(q)
		forward := forwarded(r, "/salcara-hub/_admin/snapshot", raw)
		buf := &bufferResponse{header: make(http.Header)}
		s.hub.ServeAdminHTTP(buf, forward)
		if buf.status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(buf.status)
			_, _ = w.Write(buf.body.Bytes())
			return
		}
		var state map[string]any
		if json.Unmarshal(buf.body.Bytes(), &state) != nil {
			failure(w, http.StatusInternalServerError, "Hub state unavailable")
			return
		}
		state["service"], state["version"] = "salcara-hub", Version
		state["standalone_version"] = Version
		state["update_configured"] = s.control != nil
		state["resource_mode"] = s.hub.CurrentResourceMode().ID
		state["resource_modes"] = hub.ResourceModes()
		state["resource_scope"] = resourceScope
		state["device_cleanup"] = s.hub.CurrentDeviceCleanup()
		state["memory_limit_kind"] = "Go soft target; not RSS or hard guarantee"
		respond(w, http.StatusOK, state)
	case Prefix + "/_admin/v1/action":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			failure(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if r.URL.RawQuery != "" || r.URL.ForceQuery {
			failure(w, http.StatusBadRequest, "管理接口不接受 URL 参数")
			return
		}
		var in struct {
			Action    string `json:"action"`
			DeviceRef string `json:"device_ref"`
			Reason    string `json:"reason"`
			Confirm   bool   `json:"confirm"`
		}
		if !strictJSON(w, r, &in) {
			return
		}
		if !in.Confirm {
			failure(w, http.StatusBadRequest, "请明确确认管理操作")
			return
		}
		raw, _ := json.Marshal(map[string]string{"action": in.Action, "ref": in.DeviceRef, "reason": in.Reason, "actor": "standalone-admin"})
		s.hub.ServeAdminHTTP(w, forwarded(r, "/salcara-hub/_admin/action", raw))
	case Prefix + "/_admin/v1/update/status", Prefix + "/_admin/v1/update/check", Prefix + "/_admin/v1/update/apply":
		s.serveUpdate(w, r)
	case Prefix + "/_admin/v1/resource-mode":
		s.serveResourceMode(w, r)
	case Prefix + "/_admin/v1/device-cleanup":
		s.serveDeviceCleanup(w, r)
	default:
		failure(w, http.StatusNotFound, "管理接口不存在")
	}
}

func strictJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(15 * time.Second))
	defer http.NewResponseController(w).SetReadDeadline(time.Time{}) //nolint:errcheck
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		failure(w, http.StatusBadRequest, "JSON 请求过大或读取失败")
		return false
	}
	// A security-sensitive action is exactly one object with unique keys, not
	// a permissive last-key-wins object. Typed decoding below rejects unknown
	// keys and mismatched values as well.
	keys := json.NewDecoder(bytes.NewReader(raw))
	first, err := keys.Token()
	if err != nil || first != json.Delim('{') {
		failure(w, http.StatusBadRequest, "需要一个 JSON 对象")
		return false
	}
	seen := map[string]bool{}
	for keys.More() {
		key, err := keys.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] {
			failure(w, http.StatusBadRequest, "JSON 字段无效或重复")
			return false
		}
		seen[name] = true
		if keys.Decode(new(json.RawMessage)) != nil {
			failure(w, http.StatusBadRequest, "JSON 字段格式无效")
			return false
		}
	}
	if _, err := keys.Token(); err != nil {
		failure(w, http.StatusBadRequest, "JSON 对象不完整")
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		failure(w, http.StatusBadRequest, "需要完整 JSON 对象，且不能包含未知字段")
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		failure(w, http.StatusBadRequest, "只能提交一个 JSON 对象")
		return false
	}
	return true
}

func adminQuery(values url.Values) (hub.AdminQuery, error) {
	var q hub.AdminQuery
	for key, values := range values {
		if len(values) != 1 || key != "query" && key != "filter" && key != "offset" && key != "limit" {
			return q, errors.New("unknown query")
		}
	}
	q.Query, q.Filter = values.Get("query"), values.Get("filter")
	if len(q.Query) > 128 || q.Filter != "" && q.Filter != "online" && q.Filter != "offline" && q.Filter != "paired" && q.Filter != "banned" {
		return q, errors.New("invalid filter")
	}
	var err error
	if v := values.Get("offset"); v != "" {
		q.Offset, err = strconv.Atoi(v)
		if err != nil || q.Offset < 0 || q.Offset > 100000 {
			return q, errors.New("invalid offset")
		}
	}
	if v := values.Get("limit"); v != "" {
		q.Limit, err = strconv.Atoi(v)
		if err != nil || q.Limit < 1 || q.Limit > 200 {
			return q, errors.New("invalid limit")
		}
	}
	return q, nil
}

func forwarded(r *http.Request, path string, raw []byte) *http.Request {
	clone := r.Clone(r.Context())
	u := *r.URL
	u.Path, u.RawPath, u.RawQuery, u.ForceQuery = path, "", "", false
	clone.URL = &u
	clone.Method = http.MethodPost
	clone.Body = io.NopCloser(bytes.NewReader(raw))
	clone.ContentLength = int64(len(raw))
	return clone
}

type bufferResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *bufferResponse) Header() http.Header  { return w.header }
func (w *bufferResponse) WriteHeader(code int) { w.status = code }
func (w *bufferResponse) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(b)
}

// Run closes Hub streams before draining the HTTP server, persists accepted
// mutations, and releases the data lock only after the old writer has stopped.
func Run(ctx context.Context, cfg Config, logger *slog.Logger) error {
	s, err := New(cfg, logger)
	if err != nil {
		return err
	}
	server := &http.Server{Addr: cfg.Listen, Handler: s, ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	served := make(chan error, 1)
	go func() { served <- server.ListenAndServe() }()
	select {
	case err := <-served:
		s.Close()
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}
	grace, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	drained := make(chan error, 1)
	go func() { drained <- s.Close() }()
	if err := server.Shutdown(grace); err != nil {
		server.Close()
		select {
		case <-drained:
		case <-grace.Done():
		}
		return errors.New("Hub shutdown exceeded its 20-second grace period")
	}
	select {
	case err := <-drained:
		return err
	case <-grace.Done():
		server.Close()
		return errors.New("Hub state flush exceeded its 20-second grace period")
	}
}

func Healthcheck(ctx context.Context, listen string) error {
	u, err := healthURL(listen)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("health check redirect forbidden") }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("Hub health check did not connect")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("Hub health check did not pass")
	}
	var state struct {
		OK      bool   `json:"ok"`
		Service string `json:"service"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&state) != nil || !state.OK || state.Service != "salcara-hub" {
		return errors.New("Hub health response was invalid")
	}
	return nil
}
