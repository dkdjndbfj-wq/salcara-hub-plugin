package standalone

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func authRequest(t *testing.T, s *Server, method, path, raw, origin, csrf string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "https://example.test"+Prefix+"/_admin/v1/"+path, strings.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if csrf != "" {
		r.Header.Set("X-Salcara-CSRF", csrf)
	}
	for _, cookie := range cookies {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func freshLogin(t *testing.T, s *Server, password string) (*http.Cookie, string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"password": password})
	w := authRequest(t, s, "POST", "auth/login", string(raw), "https://example.test", "")
	var out map[string]string
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out) != 1 || len(out["csrf_token"]) != 43 || len(w.Result().Cookies()) != 1 {
		t.Fatalf("password-only login failed: %d", w.Code)
	}
	return w.Result().Cookies()[0], out["csrf_token"]
}

func httpsFixture(t *testing.T) (*Server, Config, string) {
	t.Helper()
	s, cfg, password := serverFixture(t)
	s.cfg.PublicURL = "https://example.test/salcara-hub"
	cfg.PublicURL = s.cfg.PublicURL
	return s, cfg, password
}

func TestPasswordLoginCookieAndStrictOriginCSRFContract(t *testing.T) {
	s, _, password := httpsFixture(t)
	raw, _ := json.Marshal(map[string]string{"password": password})
	for _, tc := range []struct {
		path, raw, origin string
		code              int
	}{
		{"auth/login", string(raw), "", 403},
		{"auth/login", string(raw), "https://attacker.example", 403},
		{"auth/login", string(raw), "https://example.test/", 403},
		{"auth/login", `{"password":"fixture-password-long","password":"fixture-password-long"}`, "https://example.test", 400},
		{"auth/login", `{"username":"admin","password":"fixture-password-long"}`, "https://example.test", 400},
		{"auth/login?password=fixture", string(raw), "https://example.test", 400},
	} {
		if w := authRequest(t, s, "POST", tc.path, tc.raw, tc.origin, ""); w.Code != tc.code {
			t.Fatalf("login restriction: expected %d got %d", tc.code, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "https://example.test"+Prefix+"/_admin/v1/auth/login", bytes.NewReader(raw))
	r.Header.Set("Origin", "https://example.test")
	r.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 415 {
		t.Fatal("non-JSON password login accepted")
	}
	cookie, csrf := freshLogin(t, s, password)
	if cookie.Name != adminCookie || cookie.Path != Prefix+"/" || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.Domain != "" || cookie.MaxAge != int(sessionTTL.Seconds()) || len(cookie.Value) != 43 || cookie.Value == csrf {
		t.Fatal("administrator cookie was not scoped, secure, HTTP-only and independent of CSRF")
	}
	w = authRequest(t, s, "GET", "auth/session", "", "", "", cookie)
	if w.Code != 200 || strings.Contains(w.Body.String(), password) || strings.Contains(w.Body.String(), cookie.Value) || strings.Contains(w.Body.String(), "username") {
		t.Fatal("session did not restore safely")
	}
	if w := authRequest(t, s, "GET", "state", "", "https://attacker.example", "", cookie); w.Code != 403 {
		t.Fatal("cross-site session read accepted")
	}
	if w := authRequest(t, s, "GET", "state", "", "", "", cookie, cookie); w.Code != 401 {
		t.Fatal("ambiguous session cookies accepted")
	}
	for _, tc := range []struct{ origin, csrf string }{{"", csrf}, {"https://attacker.example", csrf}, {"https://example.test", ""}, {"https://example.test", "wrong-csrf"}} {
		if w := authRequest(t, s, "POST", "auth/logout", `{}`, tc.origin, tc.csrf, cookie); w.Code != 403 {
			t.Fatal("mutation without same-origin and exact CSRF accepted")
		}
	}
	if w := authRequest(t, s, "POST", "auth/logout", `{}`, "https://example.test", csrf, cookie); w.Code != 200 || w.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal("logout did not clear administrator cookie")
	}
	if w := authRequest(t, s, "GET", "state", "", "", "", cookie); w.Code != 401 {
		t.Fatal("logged-out session retained authority")
	}
	if w := authRequest(t, s, "GET", "auth/session", "", "", "", &http.Cookie{Name: "auth_token", Value: password}); w.Code != 401 {
		t.Fatal("relay cookie became an administrator")
	}
}

func TestPasswordChangeAtomicallyPersistsRevokesAllSessionsAndRemovesInitialFile(t *testing.T) {
	s, cfg, password := httpsFixture(t)
	cookie1, csrf := freshLogin(t, s, password)
	cookie2, _ := freshLogin(t, s, password)
	if cookie1.Value == cookie2.Value {
		t.Fatal("login did not generate a fresh independent session")
	}
	before := s.auth.account
	newPassword := "fixture-new-password-long-and-safe"
	for _, tc := range []struct {
		current, next string
		code          int
	}{{"fixture-wrong-password", newPassword, 401}, {password, "short", 400}, {password, password, 400}} {
		raw, _ := json.Marshal(map[string]string{"current_password": tc.current, "new_password": tc.next})
		if w := authRequest(t, s, "POST", "auth/password", string(raw), "https://example.test", csrf, cookie1); w.Code != tc.code {
			t.Fatalf("password change validation: %d", w.Code)
		}
	}
	raw, _ := json.Marshal(map[string]string{"current_password": password, "new_password": newPassword})
	w := authRequest(t, s, "POST", "auth/password", string(raw), "https://example.test", csrf, cookie1)
	if w.Code != 200 || strings.Contains(w.Body.String(), password) || strings.Contains(w.Body.String(), newPassword) || w.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal("password change did not commit and clear session")
	}
	if _, err := os.Lstat(filepath.Join(cfg.DataDir, initialLoginFile)); !os.IsNotExist(err) {
		t.Fatal("initial plaintext password was retained after changing it")
	}
	if before == s.auth.account || before.Salt == s.auth.account.Salt || before.Hash == s.auth.account.Hash {
		t.Fatal("password change did not use a new salt/hash")
	}
	for _, cookie := range []*http.Cookie{cookie1, cookie2} {
		if w := authRequest(t, s, "GET", "state", "", "", "", cookie); w.Code != 401 {
			t.Fatal("password change did not revoke every session")
		}
	}
	oldLogin, _ := json.Marshal(map[string]string{"password": password})
	if w := authRequest(t, s, "POST", "auth/login", string(oldLogin), "https://example.test", ""); w.Code != 401 {
		t.Fatal("previous password still accepted")
	}
	newCookie, _ := freshLogin(t, s, newPassword)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if w := authRequest(t, restarted, "GET", "state", "", "", "", newCookie); w.Code != 401 {
		t.Fatal("process restart retained a prior session")
	}
	freshLogin(t, restarted, newPassword)
	if verifyPassword(restarted.auth.account, password) || !verifyPassword(restarted.auth.account, newPassword) {
		t.Fatal("disk/live password hash diverged after restart")
	}
	if raw, err := os.ReadFile(cfg.AdminAccountFile); err != nil || strings.Contains(string(raw), newPassword) || strings.Contains(string(raw), password) {
		t.Fatal("plaintext password stored in account record")
	}
}

func TestPasswordCommitFailurePreservesOldLiveHashAndSession(t *testing.T) {
	s, cfg, password := httpsFixture(t)
	cookie, csrf := freshLogin(t, s, password)
	before := s.auth.account
	backup := cfg.AdminAccountFile + ".test-backup"
	if err := os.Rename(cfg.AdminAccountFile, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cfg.AdminAccountFile, 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"current_password": password, "new_password": "fixture-new-password-long-and-safe"})
	if w := authRequest(t, s, "POST", "auth/password", string(raw), "https://example.test", csrf, cookie); w.Code != 500 {
		t.Fatal("unsafe/unwritable account path changed password")
	}
	if s.auth.account != before || !verifyPassword(s.auth.account, password) {
		t.Fatal("failed persistent write changed running password")
	}
	if w := authRequest(t, s, "GET", "state", "", "", "", cookie); w.Code != 200 {
		t.Fatal("failed password commit revoked an existing session")
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, initialLoginFile)); err != nil {
		t.Fatal("failed password commit removed initial login file")
	}
	if err := os.Remove(cfg.AdminAccountFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, cfg.AdminAccountFile); err != nil {
		t.Fatal(err)
	}
	freshLogin(t, s, password)
}

func TestPasswordAttemptsBoundedSessionExpiryAndDerivationAdmission(t *testing.T) {
	s, _, password := httpsFixture(t)
	cookie, _ := freshLogin(t, s, password)
	key, _, ok := s.auth.session(&http.Request{Header: http.Header{"Cookie": []string{cookie.String()}}})
	if !ok {
		t.Fatal("test cookie invalid")
	}
	s.auth.mu.Lock()
	session := s.auth.sessions[key]
	session.Expires = time.Now().Add(-time.Second)
	s.auth.sessions[key] = session
	s.auth.mu.Unlock()
	if w := authRequest(t, s, "GET", "state", "", "", "", cookie); w.Code != 401 {
		t.Fatal("expired session was retained")
	}
	wrong, _ := json.Marshal(map[string]string{"password": "fixture-wrong-password"})
	for i := 0; i < maxLoginAttempts; i++ {
		if w := authRequest(t, s, "POST", "auth/login", string(wrong), "https://example.test", ""); w.Code != 401 {
			t.Fatal("early login attempt unexpectedly accepted/limited")
		}
	}
	if w := authRequest(t, s, "POST", "auth/login", string(wrong), "https://example.test", ""); w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal("unbounded password attempts")
	}
	s.auth.mu.Lock()
	clear(s.auth.loginIPs)
	s.auth.mu.Unlock()
	passwordSlots <- struct{}{}
	passwordSlots <- struct{}{}
	w := authRequest(t, s, "POST", "auth/login", string(wrong), "https://example.test", "")
	<-passwordSlots
	<-passwordSlots
	if w.Code != 429 {
		t.Fatal("expensive password derivations queued without a limit")
	}
	s.auth.mu.Lock()
	clear(s.auth.loginIPs)
	s.auth.mu.Unlock()
	for i := 0; i < maxLoginIPs; i++ {
		if !s.auth.allowPasswordAttempt(string(rune(i + 1))) {
			t.Fatal("bounded IP table rejected early")
		}
	}
	if s.auth.allowPasswordAttempt("overflow-key") || len(s.auth.loginIPs) != maxLoginIPs {
		t.Fatal("IP rate table grew without bound")
	}
}

func TestAccountInitializationLocksExistingVolumeAndLegacyTokenCannotLogin(t *testing.T) {
	s, cfg, _ := serverFixture(t)
	if err := InitAdminAccount(cfg); err == nil {
		t.Fatal("initialization ignored running Hub volume lock")
	}
	s.Close()
	legacy := testConfig(t)
	if err := initControlToken(filepath.Join(legacy.DataDir, "admin-token")); err != nil {
		t.Fatal(err)
	}
	if server, err := New(legacy, nil); err == nil {
		server.Close()
		t.Fatal("legacy-only admin token started new password Hub")
	}
	if err := InitAdminAccount(legacy); err != nil {
		t.Fatal("legacy pairing volume could not be explicitly initialized")
	}
	if _, err := os.Stat(filepath.Join(legacy.DataDir, "admin-token")); err != nil {
		t.Fatal("initialization modified legacy token file")
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(legacy.AdminAccountFile, 0640); err != nil {
			t.Fatal(err)
		}
		if server, err := New(legacy, nil); err == nil {
			server.Close()
			t.Fatal("unsafe account permissions accepted")
		}
	}
}

func TestPasswordConfigDefaultAndTraversalRefusal(t *testing.T) {
	cfg := testConfig(t)
	defaults, err := ConfigFromEnv(func(k string) string {
		if k == "SALCARA_HUB_DATA_DIR" {
			return cfg.DataDir
		}
		return ""
	})
	if err != nil || defaults.AdminAccountFile != filepath.Join(cfg.DataDir, defaultAccountFile) {
		t.Fatal("password configuration default incorrect")
	}
	for _, unsafe := range []string{filepath.Join(cfg.DataDir, "nested", defaultAccountFile), filepath.Join(cfg.DataDir, "..", defaultAccountFile), filepath.Join(cfg.DataDir, initialLoginFile), filepath.Join(cfg.DataDir, ".salcara-hub.lock")} {
		invalid := cfg
		invalid.AdminAccountFile = unsafe
		if invalid.Validate() == nil {
			t.Fatal("account storage escaped reserved data location")
		}
	}
}

func TestOfflineManagementKeyRecoveryPreservesDeviceDataAndRefusesRunningHub(t *testing.T) {
	s, cfg, oldPassword := httpsFixture(t)
	if _, err := ResetAdminKey(cfg); err == nil {
		t.Fatal("offline recovery acquired a running Hub volume")
	}
	oldCookie, _ := freshLogin(t, s, oldPassword)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(cfg.DataDir, "pairing-data-test-marker")
	if err := os.WriteFile(marker, []byte("existing pairings retained"), 0600); err != nil {
		t.Fatal(err)
	}
	if warning, err := ResetAdminKey(cfg); err != nil || warning {
		t.Fatal("offline management key recovery failed")
	}
	newPassword := initialPassword(t, cfg)
	if newPassword == oldPassword {
		t.Fatal("offline recovery reused the prior key")
	}
	if raw, err := os.ReadFile(marker); err != nil || string(raw) != "existing pairings retained" {
		t.Fatal("offline recovery modified device data")
	}
	restarted, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if verifyPassword(restarted.auth.account, oldPassword) || !verifyPassword(restarted.auth.account, newPassword) {
		t.Fatal("recovered key/file and active hash diverged")
	}
	if w := authRequest(t, restarted, "GET", "state", "", "", "", oldCookie); w.Code != 401 {
		t.Fatal("offline recovery retained an old session")
	}
	newCookie, csrf := freshLogin(t, restarted, newPassword)
	raw, _ := json.Marshal(map[string]string{"current_password": newPassword, "new_password": "fixture-after-recovery-key-long"})
	if w := authRequest(t, restarted, "POST", "auth/password", string(raw), "https://example.test", csrf, newCookie); w.Code != 200 {
		t.Fatal("recovered key could not be rotated in admin console")
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
	// Recovery also works after the initial plaintext file was deleted by a
	// successful web rotation; no existing account is reinitialized.
	if _, err := ResetAdminKey(cfg); err != nil {
		t.Fatal("recovery after web rotation failed")
	}
	newest := initialPassword(t, cfg)
	if newest == newPassword {
		t.Fatal("second recovery reused an old key")
	}
	missing := testConfig(t)
	if _, err := ResetAdminKey(missing); err == nil {
		t.Fatal("recovery silently initialized a missing account")
	}
	if _, err := os.Stat(filepath.Join(missing.DataDir, initialLoginFile)); !os.IsNotExist(err) {
		t.Fatal("missing-account recovery created an unpaired plaintext key")
	}
}

func TestOfflineRecoveryHashFailureRestoresPriorInitialFile(t *testing.T) {
	for _, keepInitial := range []bool{true, false} {
		t.Run(map[bool]string{true: "existing-initial", false: "no-initial"}[keepInitial], func(t *testing.T) {
			s, cfg, password := serverFixture(t)
			s.Close()
			auth, err := newAdminAuth(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer auth.close()
			old := auth.account
			initial := filepath.Join(cfg.DataDir, initialLoginFile)
			if !keepInitial {
				if err := os.Remove(initial); err != nil {
					t.Fatal(err)
				}
			}
			backup := cfg.AdminAccountFile + ".test-backup"
			if err := os.Rename(cfg.AdminAccountFile, backup); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(cfg.AdminAccountFile, 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := auth.resetPassword("fixture-new-recovery-key-long"); err == nil {
				t.Fatal("recovery claimed success despite failed hash persistence")
			}
			if auth.account != old || !verifyPassword(auth.account, password) {
				t.Fatal("failed recovery changed current credential")
			}
			if keepInitial {
				if initialPassword(t, cfg) != password {
					t.Fatal("failed recovery left an inactive key in the prior initial file")
				}
			} else if _, err := os.Stat(initial); !os.IsNotExist(err) {
				t.Fatal("failed recovery left an inactive initial key when none existed before")
			}
			if err := os.Remove(cfg.AdminAccountFile); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(backup, cfg.AdminAccountFile); err != nil {
				t.Fatal(err)
			}
		})
	}
}
