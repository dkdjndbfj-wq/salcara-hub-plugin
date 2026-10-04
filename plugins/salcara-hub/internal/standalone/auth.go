package standalone

import (
	"bytes"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	initialLoginFile   = "admin-initial-login.txt"
	adminCookie        = "salcara_hub_admin"
	passwordIterations = 600000
	sessionTTL         = 12 * time.Hour
	maxAdminSessions   = 128
	maxLoginIPs        = 1024
	maxLoginAttempts   = 5
)

// There is no unbounded queue of expensive password work, even if requests
// arrive through many different IPs or several test/application instances.
var passwordSlots = make(chan struct{}, 2)

type adminAccount struct {
	Schema     int    `json:"schema"`
	Algorithm  string `json:"algorithm"`
	Iterations int    `json:"iterations"`
	Salt       string `json:"salt"`
	Hash       string `json:"hash"`
}

type adminSession struct {
	CSRF    string
	Expires time.Time
}

type loginWindow struct {
	Start    time.Time
	Attempts int
}

type adminAuth struct {
	mu         sync.Mutex
	root       *os.Root
	file       string
	account    adminAccount
	generation uint64
	sessions   map[[32]byte]adminSession
	loginIPs   map[string]loginWindow
	closed     bool
}

func randomSecret() (string, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", errors.New("secure randomness unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(random[:]), nil
}

func validPassword(password string) bool {
	return len(password) >= 16 && len(password) <= 1024 && utf8.ValidString(password) && !strings.ContainsRune(password, 0)
}

func makeAccount(password string) (adminAccount, error) {
	var account adminAccount
	var salt [32]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return account, errors.New("secure randomness unavailable")
	}
	key, err := pbkdf2.Key(sha256.New, password, salt[:], passwordIterations, 32)
	if err != nil {
		return account, errors.New("password derivation failed")
	}
	defer clear(key)
	return adminAccount{Schema: 1, Algorithm: "pbkdf2-sha256", Iterations: passwordIterations, Salt: base64.RawStdEncoding.EncodeToString(salt[:]), Hash: base64.RawStdEncoding.EncodeToString(key)}, nil
}

func verifyPassword(account adminAccount, password string) bool {
	salt, _ := base64.RawStdEncoding.DecodeString(account.Salt)
	want, _ := base64.RawStdEncoding.DecodeString(account.Hash)
	got, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, 32)
	defer clear(got)
	defer clear(want)
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

func openAuthRoot(dir string) (*os.Root, error) {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || runtime.GOOS != "windows" && info.Mode().Perm()&0022 != 0 {
		return nil, errors.New("Hub data directory must be a real directory, not writable by other users")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, errors.New("Hub data directory is unavailable")
	}
	f, err := root.Open(".")
	if err != nil {
		root.Close()
		return nil, errors.New("cannot inspect opened Hub data directory")
	}
	opened, statErr := f.Stat()
	f.Close()
	if statErr != nil || !os.SameFile(info, opened) {
		root.Close()
		return nil, errors.New("Hub data directory changed while opening")
	}
	return root, nil
}

// Only regular mode-0600 files are read. Root confines all resolution to the
// held data directory; inode checks are performed before reading any bytes.
func readPrivateFile(root *os.Root, name string, limit int64) ([]byte, error) {
	before, err := root.Lstat(name)
	if err != nil || !before.Mode().IsRegular() || before.Size() < 1 || before.Size() > limit || runtime.GOOS != "windows" && before.Mode().Perm() != 0600 {
		return nil, errors.New("administrator file must be a regular mode-0600 file")
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, errors.New("administrator file is unavailable")
	}
	defer f.Close()
	opened, statErr := f.Stat()
	current, pathErr := root.Lstat(name)
	if statErr != nil || pathErr != nil || !opened.Mode().IsRegular() || !current.Mode().IsRegular() || !os.SameFile(before, opened) || !os.SameFile(opened, current) || runtime.GOOS != "windows" && opened.Mode().Perm() != 0600 {
		return nil, errors.New("administrator file changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		clear(raw)
		return nil, errors.New("administrator file is invalid")
	}
	return raw, nil
}

func loadAdminAccount(root *os.Root, name string) (adminAccount, error) {
	var account adminAccount
	raw, err := readPrivateFile(root, name, 4096)
	if err != nil {
		return account, errors.New("administrator credential record missing or unsafe; stop Hub and run -init once to initialize a management key (legacy admin tokens are not accepted)")
	}
	defer clear(raw)
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&account) != nil || d.Decode(new(any)) != io.EOF {
		return account, errors.New("administrator account file is invalid")
	}
	salt, saltErr := base64.RawStdEncoding.DecodeString(account.Salt)
	hash, hashErr := base64.RawStdEncoding.DecodeString(account.Hash)
	if account.Schema != 1 || account.Algorithm != "pbkdf2-sha256" || account.Iterations != passwordIterations || saltErr != nil || len(salt) != 32 || hashErr != nil || len(hash) != 32 {
		return account, errors.New("administrator account file has unsupported password parameters")
	}
	return account, nil
}

func syncAuthDir(root *os.Root) bool {
	if runtime.GOOS == "windows" {
		return false
	}
	f, err := root.Open(".")
	if err != nil {
		return true
	}
	defer f.Close()
	return f.Sync() != nil
}

func writePrivateNew(root *os.Root, name string, raw []byte) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("administrator initialization target already exists or is unavailable; no existing credentials were replaced")
	}
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.New("administrator initialization could not be saved; inspect the private files before retrying")
	}
	return nil
}

// InitAdminAccount never reads/replaces old tokens, accounts or pairing data.
// It holds the same exclusive volume lock as a running Hub. The random initial
// password is saved privately before publishing its derived account record.
func InitAdminAccount(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	lock, err := acquireDataLock(cfg.DataDir)
	if err != nil {
		return err
	}
	defer lock.release()
	root, err := openAuthRoot(cfg.DataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	name := filepath.Base(cfg.AdminAccountFile)
	for _, target := range []string{name, initialLoginFile} {
		if _, err := root.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			return errors.New("administrator account or initial-login file already exists or cannot be inspected; no existing credentials were replaced")
		}
	}
	password, err := randomSecret()
	if err != nil {
		return err
	}
	account, err := makeAccount(password)
	if err != nil {
		return err
	}
	initial := []byte(password + "\n")
	defer clear(initial)
	if err := writePrivateNew(root, initialLoginFile, initial); err != nil {
		return err
	}
	raw, _ := json.Marshal(account)
	if err := writePrivateNew(root, name, append(raw, '\n')); err != nil {
		return err
	}
	if syncAuthDir(root) {
		return errors.New("administrator files were created but directory sync failed; inspect durability before starting, do not reinitialize")
	}
	return nil
}

func newAdminAuth(cfg Config) (*adminAuth, error) {
	root, err := openAuthRoot(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	account, err := loadAdminAccount(root, filepath.Base(cfg.AdminAccountFile))
	if err != nil {
		root.Close()
		return nil, err
	}
	return &adminAuth{root: root, file: filepath.Base(cfg.AdminAccountFile), account: account, generation: 1, sessions: make(map[[32]byte]adminSession), loginIPs: make(map[string]loginWindow)}, nil
}

func (a *adminAuth) close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.closed {
		a.closed = true
		clear(a.sessions)
		a.root.Close()
	}
}

func (a *adminAuth) session(r *http.Request) ([32]byte, adminSession, bool) {
	var key [32]byte
	var found *http.Cookie
	for _, cookie := range r.Cookies() {
		if cookie.Name != adminCookie {
			continue
		}
		if found != nil {
			return key, adminSession{}, false
		}
		found = cookie
	}
	if found == nil || len(found.Value) != 43 {
		return key, adminSession{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(found.Value)
	if err != nil || len(raw) != 32 {
		return key, adminSession{}, false
	}
	key = sha256.Sum256([]byte(found.Value))
	a.mu.Lock()
	defer a.mu.Unlock()
	session, ok := a.sessions[key]
	if a.closed || !ok || !time.Now().Before(session.Expires) {
		delete(a.sessions, key)
		return key, adminSession{}, false
	}
	return key, session, true
}

func (s *Server) setAdminCookie(w http.ResponseWriter, r *http.Request, value string) {
	secure := r.TLS != nil
	if s.cfg.PublicURL != "" {
		u, _ := url.Parse(s.cfg.PublicURL)
		secure = u.Scheme == "https"
	}
	cookie := &http.Cookie{Name: adminCookie, Value: value, Path: Prefix + "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds()), Expires: time.Now().Add(sessionTTL)}
	if value == "" {
		cookie.MaxAge = -1
		cookie.Expires = time.Unix(1, 0)
	}
	http.SetCookie(w, cookie)
}

func (s *Server) authorizeAdmin(w http.ResponseWriter, r *http.Request) ([32]byte, bool) {
	key, session, ok := s.auth.session(r)
	if !ok {
		failure(w, http.StatusUnauthorized, "请输入 Hub 管理密钥")
		return key, false
	}
	mutation := r.Method != http.MethodGet && r.Method != http.MethodHead
	if mutation || r.Header.Get("Origin") != "" {
		if !s.sameOrigin(r) {
			failure(w, http.StatusForbidden, "不允许跨站管理请求")
			return key, false
		}
	}
	if mutation {
		if len(r.Header.Values("X-Salcara-CSRF")) != 1 || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Salcara-CSRF")), []byte(session.CSRF)) != 1 {
			failure(w, http.StatusForbidden, "管理会话校验失败，请重新登录")
			return key, false
		}
		if !jsonContentType(r) {
			failure(w, http.StatusUnsupportedMediaType, "请提交 JSON 请求")
			return key, false
		}
	}
	return key, true
}

func jsonContentType(r *http.Request) bool {
	if len(r.Header.Values("Content-Type")) != 1 {
		return false
	}
	kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && kind == "application/json"
}

func (s *Server) loginIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		// Deployment must overwrite this header, not append attacker-controlled
		// input. Refuse lists rather than letting clients pick their rate key.
		if len(r.Header.Values("X-Forwarded-For")) == 1 {
			if ip := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Forwarded-For"))); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip.String()
		}
	}
	return "unknown"
}

func (a *adminAuth) allowPasswordAttempt(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return false
	}
	now := time.Now()
	for candidate, window := range a.loginIPs {
		if now.Sub(window.Start) >= time.Minute {
			delete(a.loginIPs, candidate)
		}
	}
	window, exists := a.loginIPs[ip]
	if !exists && len(a.loginIPs) >= maxLoginIPs || window.Attempts >= maxLoginAttempts {
		return false
	}
	if !exists {
		window.Start = now
	}
	window.Attempts++
	a.loginIPs[ip] = window
	return true
}

func (s *Server) serveAuth(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, Prefix+"/_admin/v1/auth/")
	method := http.MethodPost
	if path == "session" {
		method = http.MethodGet
	}
	if path != "login" && path != "session" && path != "logout" && path != "password" {
		failure(w, 404, "管理接口不存在")
		return
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		failure(w, 405, "method not allowed")
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		failure(w, 400, "登录接口不接受 URL 参数")
		return
	}
	if path == "login" {
		if !s.sameOrigin(r) {
			failure(w, 403, "不允许跨站登录请求")
			return
		}
		if !jsonContentType(r) {
			failure(w, 415, "请提交 JSON 请求")
			return
		}
		s.login(w, r)
		return
	}
	key, ok := s.authorizeAdmin(w, r)
	if !ok {
		return
	}
	switch path {
	case "session":
		_, session, ok := s.auth.session(r)
		if !ok {
			failure(w, 401, "请重新登录")
			return
		}
		respond(w, 200, map[string]string{"csrf_token": session.CSRF})
	case "logout":
		if !strictJSON(w, r, &struct{}{}) {
			return
		}
		s.auth.mu.Lock()
		delete(s.auth.sessions, key)
		s.auth.mu.Unlock()
		s.setAdminCookie(w, r, "")
		respond(w, 200, map[string]bool{"ok": true})
	case "password":
		s.changePassword(w, r, key)
	}
}

func acquirePasswordWork(w http.ResponseWriter) bool {
	select {
	case passwordSlots <- struct{}{}:
		return true
	default:
		w.Header().Set("Retry-After", "5")
		failure(w, 429, "登录请求较多，请稍后重试")
		return false
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
	}
	if !strictJSON(w, r, &in) {
		return
	}
	if !validPassword(in.Password) {
		failure(w, 401, "管理密钥错误")
		return
	}
	ip := s.loginIP(r)
	if !s.auth.allowPasswordAttempt(ip) {
		w.Header().Set("Retry-After", "60")
		failure(w, 429, "登录尝试较多，请稍后重试")
		return
	}
	if !acquirePasswordWork(w) {
		return
	}
	defer func() { <-passwordSlots }()
	s.auth.mu.Lock()
	account, generation := s.auth.account, s.auth.generation
	s.auth.mu.Unlock()
	if !verifyPassword(account, in.Password) {
		failure(w, 401, "管理密钥错误")
		return
	}
	token, err := randomSecret()
	if err != nil {
		failure(w, 503, "登录暂不可用")
		return
	}
	csrf, err := randomSecret()
	if err != nil {
		failure(w, 503, "登录暂不可用")
		return
	}
	s.auth.mu.Lock()
	defer s.auth.mu.Unlock()
	if s.auth.closed || generation != s.auth.generation {
		failure(w, 401, "管理密钥错误")
		return
	}
	now := time.Now()
	for key, session := range s.auth.sessions {
		if !now.Before(session.Expires) {
			delete(s.auth.sessions, key)
		}
	}
	if len(s.auth.sessions) >= maxAdminSessions {
		var oldest [32]byte
		var expiry time.Time
		for key, session := range s.auth.sessions {
			if expiry.IsZero() || session.Expires.Before(expiry) {
				oldest, expiry = key, session.Expires
			}
		}
		delete(s.auth.sessions, oldest)
	}
	s.auth.sessions[sha256.Sum256([]byte(token))] = adminSession{CSRF: csrf, Expires: now.Add(sessionTTL)}
	delete(s.auth.loginIPs, ip)
	s.setAdminCookie(w, r, token)
	respond(w, 200, map[string]string{"csrf_token": csrf})
}

// Rename is the credential commit point. Errors before it leave the previous
// on-disk and live hashes unchanged; after it memory is updated even if the
// directory fsync fails (reported separately as a durability warning).
func (a *adminAuth) savePassword(account adminAccount, removeInitial bool) (bool, error) {
	if _, err := readPrivateFile(a.root, a.file, 4096); err != nil {
		return false, errors.New("current administrator file is unsafe or unavailable")
	}
	id, err := randomSecret()
	if err != nil {
		return false, err
	}
	temp := ".admin-account-" + id + ".tmp"
	defer a.root.Remove(temp)
	raw, _ := json.Marshal(account)
	if err := writePrivateNew(a.root, temp, append(raw, '\n')); err != nil {
		return false, err
	}
	if err := a.root.Rename(temp, a.file); err != nil {
		return false, errors.New("administrator password commit failed")
	}
	a.account = account
	a.generation++
	clear(a.sessions)
	warning := false
	if removeInitial {
		if err := a.root.Remove(initialLoginFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			warning = true
		}
	}
	return syncAuthDir(a.root) || warning, nil
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, sessionKey [32]byte) {
	var in struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if !strictJSON(w, r, &in) {
		return
	}
	if !validPassword(in.New) {
		failure(w, 400, "新管理密钥需要 16–1024 字节")
		return
	}
	if !validPassword(in.Current) {
		failure(w, 401, "管理密钥错误")
		return
	}
	ip := s.loginIP(r)
	if !s.auth.allowPasswordAttempt(ip) {
		w.Header().Set("Retry-After", "60")
		failure(w, 429, "验证尝试较多，请稍后重试")
		return
	}
	if !acquirePasswordWork(w) {
		return
	}
	defer func() { <-passwordSlots }()
	s.auth.mu.Lock()
	account, generation := s.auth.account, s.auth.generation
	s.auth.mu.Unlock()
	if !verifyPassword(account, in.Current) {
		failure(w, 401, "管理密钥错误")
		return
	}
	if subtle.ConstantTimeCompare([]byte(in.Current), []byte(in.New)) == 1 {
		failure(w, 400, "新管理密钥不能与当前密钥相同")
		return
	}
	updated, err := makeAccount(in.New)
	if err != nil {
		failure(w, 503, "更换管理密钥暂不可用")
		return
	}
	s.auth.mu.Lock()
	defer s.auth.mu.Unlock()
	session, exists := s.auth.sessions[sessionKey]
	if s.auth.closed || generation != s.auth.generation || !exists || !time.Now().Before(session.Expires) {
		failure(w, 401, "请重新登录")
		return
	}
	warning, err := s.auth.savePassword(updated, true)
	if err != nil {
		failure(w, 500, "管理密钥未更换，请检查数据卷文件权限与可写状态")
		return
	}
	delete(s.auth.loginIPs, ip)
	s.setAdminCookie(w, r, "")
	respond(w, 200, map[string]bool{"ok": true, "durability_warning": warning})
}

func replacePrivateFile(root *os.Root, name string, raw []byte) (bool, error) {
	if _, err := root.Lstat(name); err == nil {
		old, err := readPrivateFile(root, name, 4096)
		clear(old)
		if err != nil {
			return false, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, errors.New("cannot inspect private administrator recovery file")
	}
	id, err := randomSecret()
	if err != nil {
		return false, err
	}
	temp := ".admin-recovery-" + id + ".tmp"
	defer root.Remove(temp)
	if err := writePrivateNew(root, temp, raw); err != nil {
		return false, err
	}
	if err := root.Rename(temp, name); err != nil {
		return false, errors.New("administrator recovery file commit failed")
	}
	return syncAuthDir(root), nil
}

// resetPassword commits a private recovery copy before the new hash. Thus a
// committed replacement key is never left without a privately retrievable
// plaintext copy. If the hash write fails, restore the prior initial file.
// Interruption between the two commits is recoverable by rerunning the CLI;
// it never removes/changes device data or opens unauthenticated management.
func (a *adminAuth) resetPassword(password string) (bool, error) {
	var previous []byte
	if _, err := a.root.Lstat(initialLoginFile); err == nil {
		previous, err = readPrivateFile(a.root, initialLoginFile, 4096)
		if err != nil {
			return false, err
		}
		defer clear(previous)
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, errors.New("cannot inspect prior administrator recovery file")
	}
	account, err := makeAccount(password)
	if err != nil {
		return false, err
	}
	raw := []byte(password + "\n")
	defer clear(raw)
	warning, err := replacePrivateFile(a.root, initialLoginFile, raw)
	if err != nil {
		return false, err
	}
	// Do not hash-commit if the new recovery copy could not be durably synced.
	if warning {
		err = errors.New("administrator recovery directory sync failed; new prepared recovery key is not active, rerun reset after inspecting storage")
	} else {
		var committedWarning bool
		committedWarning, err = a.savePassword(account, false)
		if err == nil {
			return committedWarning, nil
		}
	}
	if previous != nil {
		if _, restoreErr := replacePrivateFile(a.root, initialLoginFile, previous); restoreErr != nil {
			return false, errors.New("management key was not changed; recovery-file rollback failed and its prepared key is not active, inspect storage before retrying")
		}
	} else if removeErr := a.root.Remove(initialLoginFile); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return false, errors.New("management key was not changed; prepared recovery file could not be removed and its key is not active, inspect storage before retrying")
	}
	syncAuthDir(a.root)
	return false, err
}

// ResetAdminKey is an explicit offline recovery operation, separate from -init.
// No input password is accepted and the random key is never returned/printed.
// A true warning means the new hash committed but its directory fsync failed;
// the recovery key already exists and the caller must report the warning.
func ResetAdminKey(cfg Config) (warning bool, err error) {
	if err := cfg.Validate(); err != nil {
		return false, err
	}
	lock, err := acquireDataLock(cfg.DataDir)
	if err != nil {
		return false, err
	}
	defer lock.release()
	auth, err := newAdminAuth(cfg)
	if err != nil {
		return false, err
	}
	defer auth.close()
	password, err := randomSecret()
	if err != nil {
		return false, err
	}
	return auth.resetPassword(password)
}
