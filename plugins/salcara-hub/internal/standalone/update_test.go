package standalone

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fixtureTransport func(*http.Request) (*http.Response, error)

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func controlFixture(t *testing.T) (*Server, string, string) {
	t.Helper()
	cfg := testConfig(t)
	cfg.ControlSocket = filepath.Join(cfg.DataDir, "control.sock")
	cfg.ControlTokenFile = filepath.Join(cfg.DataDir, "control-token")
	for _, path := range []string{cfg.AdminTokenFile, cfg.ControlTokenFile} {
		if err := InitAdminToken(path); err != nil {
			t.Fatal(err)
		}
	}
	admin, err := readAdminToken(cfg.AdminTokenFile)
	if err != nil {
		t.Fatal(err)
	}
	control, err := readAdminToken(cfg.ControlTokenFile)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, string(admin), string(control)
}

func controlResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestUpdateControlConfigFailClosed(t *testing.T) {
	cfg := testConfig(t)
	for _, update := range []func(*Config){
		func(c *Config) { c.ControlSocket = filepath.Join(c.DataDir, "control.sock") },
		func(c *Config) { c.ControlTokenFile = filepath.Join(c.DataDir, "control-token") },
		func(c *Config) {
			c.ControlSocket = "http://remote.test"
			c.ControlTokenFile = filepath.Join(c.DataDir, "control-token")
		},
		func(c *Config) {
			c.ControlSocket = filepath.Join(c.DataDir, "control.sock")
			c.ControlTokenFile = c.AdminTokenFile
		},
	} {
		invalid := cfg
		update(&invalid)
		if invalid.Validate() == nil {
			t.Fatal("unsafe control configuration accepted")
		}
	}
	if err := InitAdminToken(cfg.AdminTokenFile); err != nil {
		t.Fatal(err)
	}
	cfg.ControlSocket, cfg.ControlTokenFile = filepath.Join(cfg.DataDir, "control.sock"), filepath.Join(cfg.DataDir, "control-token")
	if _, err := New(cfg, nil); err == nil {
		t.Fatal("missing control secret silently accepted")
	}
	// Same secret in a distinct file must not make Hub admin credentials into
	// launcher credentials. Both files belong solely to this test directory.
	raw, err := os.ReadFile(cfg.AdminTokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.ControlTokenFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg, nil); err == nil {
		t.Fatal("admin credential reused for the control plane")
	}
}

func TestUpdateAbsentDoesNotUseNetworkAndDiscoveryReportsVersion(t *testing.T) {
	s, _, admin := serverFixture(t)
	for _, tc := range []struct {
		method, path string
		body         any
		code         int
	}{
		{http.MethodGet, "/status", nil, 200},
		{http.MethodPost, "/check", map[string]any{}, 503},
		{http.MethodPost, "/apply", map[string]any{"version": "0.4.1", "sha256": strings.Repeat("a", 64), "confirm": true}, 503},
	} {
		w := adminRequest(t, s, tc.method, Prefix+"/_admin/v1/update"+tc.path, admin, tc.body)
		if w.Code != tc.code || !strings.Contains(w.Body.String(), `"status":"unconfigured"`) || strings.Contains(w.Body.String(), `"configured":true`) {
			t.Fatal("missing updater misrepresented")
		}
	}
	w := adminRequest(t, s, http.MethodGet, Prefix+"/v1/ping", "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"standalone_version":"`+Version+`"`) || strings.Contains(w.Body.String(), "legacy-api-key") {
		t.Fatal("standalone discovery inaccurate")
	}
}

func TestUpdateProxyIndependentTokenStrictRequestAndAcceptedNotComplete(t *testing.T) {
	s, admin, control := controlFixture(t)
	calls := 0
	s.control.client.Transport = fixtureTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "launcher" || r.Header.Get("Authorization") != "Bearer "+control || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") == "Bearer "+admin {
			t.Fatal("admin/model credential or external target forwarded")
		}
		body, _ := io.ReadAll(r.Body)
		status, state := 200, "available"
		switch r.URL.Path {
		case "/status":
			if r.Method != http.MethodGet {
				t.Fatal("wrong status method")
			}
		case "/check":
			if r.Method != http.MethodPost || string(body) != `{}` {
				t.Fatal("unsafe manual check body")
			}
		case "/apply":
			var value struct {
				Version string `json:"version"`
				SHA     string `json:"sha256"`
				Confirm bool   `json:"confirm"`
			}
			if r.Method != http.MethodPost || json.Unmarshal(body, &value) != nil || value.Version != "0.4.1" || value.SHA != strings.Repeat("a", 64) || !value.Confirm {
				t.Fatal("apply lost checked version/digest/confirmation")
			}
			status, state = 202, "updating"
		default:
			t.Fatal("free-form control path selected")
		}
		return controlResponse(status, `{"configured":true,"current_version":"0.4.0-dev","latest_version":"0.4.1","status":"`+state+`","sha256":"`+strings.Repeat("a", 64)+`","release_notes":"Fixture notes","message":"Fixture","job_id":"fixture-job","private_unrecognized":"do-not-forward"}`), nil
	})
	state := adminRequest(t, s, http.MethodGet, Prefix+"/_admin/v1/state", admin, nil)
	if state.Code != 200 || !strings.Contains(state.Body.String(), `"update_configured":true`) || calls != 0 {
		t.Fatal("state polled updater without an explicit request")
	}
	for _, tc := range []struct {
		method, path string
		body         any
		code         int
	}{
		{http.MethodGet, "/status", nil, 200},
		{http.MethodPost, "/check", map[string]any{}, 200},
		{http.MethodPost, "/apply", map[string]any{"version": "0.4.1", "sha256": strings.Repeat("a", 64), "confirm": true}, 202},
	} {
		w := adminRequest(t, s, tc.method, Prefix+"/_admin/v1/update"+tc.path, admin, tc.body)
		if w.Code != tc.code || strings.Contains(w.Body.String(), control) || strings.Contains(w.Body.String(), admin) || strings.Contains(w.Body.String(), "private_unrecognized") || strings.Contains(w.Body.String(), "do-not-forward") {
			t.Fatal("safe control response failed")
		}
		if tc.code == 202 && !strings.Contains(w.Body.String(), `"status":"updating"`) {
			t.Fatal("accepted job falsely reported complete")
		}
	}
	for _, bad := range []struct {
		path string
		body any
	}{
		{"/check", map[string]any{"url": "https://attacker.test"}},
		{"/apply", map[string]any{"version": "0.4.1", "sha256": strings.Repeat("a", 64), "confirm": false}},
		{"/apply", map[string]any{"version": "v0.4.1", "sha256": strings.Repeat("a", 64), "confirm": true}},
		{"/apply", map[string]any{"version": "0.4.1", "sha256": strings.Repeat("A", 64), "confirm": true}},
		{"/apply?url=ignored", map[string]any{"version": "0.4.1", "sha256": strings.Repeat("a", 64), "confirm": true}},
	} {
		if w := adminRequest(t, s, http.MethodPost, Prefix+"/_admin/v1/update"+bad.path, admin, bad.body); w.Code != 400 {
			t.Fatal("unsafe update body accepted")
		}
	}
	if calls != 3 {
		t.Fatal("rejected request contacted updater or request was retried")
	}
}

func TestUpdateProxyRejectsRedirectOversizeInvalidAndSecretResponses(t *testing.T) {
	s, admin, control := controlFixture(t)
	for _, raw := range []string{
		strings.Repeat("x", (64<<10)+1),
		`{"configured":true,"status":"available","message":"no current version"}`,
		`{"configured":true,"current_version":"0.4.0-dev","status":"available","message":"` + control + `"}`,
	} {
		calls := 0
		s.control.client.Transport = fixtureTransport(func(*http.Request) (*http.Response, error) { calls++; return controlResponse(200, raw), nil })
		w := adminRequest(t, s, http.MethodGet, Prefix+"/_admin/v1/update/status", admin, nil)
		if w.Code != 502 || strings.Contains(w.Body.String(), control) || calls != 1 {
			t.Fatal("unsafe control response leaked or retried")
		}
	}
	calls := 0
	s.control.client.Transport = fixtureTransport(func(*http.Request) (*http.Response, error) {
		calls++
		resp := controlResponse(302, `{}`)
		resp.Header.Set("Location", "https://attacker.test/private")
		return resp, nil
	})
	if w := adminRequest(t, s, http.MethodGet, Prefix+"/_admin/v1/update/status", admin, nil); w.Code != 502 || calls != 1 || strings.Contains(w.Body.String(), "attacker") {
		t.Fatal("control redirect followed or leaked")
	}
	s.control.client.Transport = fixtureTransport(func(r *http.Request) (*http.Response, error) { return nil, context.Canceled })
	if w := adminRequest(t, s, http.MethodGet, Prefix+"/_admin/v1/update/status", admin, nil); w.Code != 502 || strings.Contains(w.Body.String(), "launcher") {
		t.Fatal("internal connection failure leaked")
	}
}
