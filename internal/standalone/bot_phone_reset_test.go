package standalone

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"salcara/hub/internal/hub"
)

func botAdminFixture(t *testing.T) (*Server, Config, *http.Cookie, string) {
	t.Helper()
	cfg := testConfig(t)
	cfg.DeploymentMode = "bot"
	cfg.PublicURL = "https://example.test/salcara-hub"
	if err := InitAdminAccount(cfg); err != nil {
		t.Fatal(err)
	}
	password := initialPassword(t, cfg)
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	cookie, csrf := freshLogin(t, s, password)
	return s, cfg, cookie, csrf
}

func botAdminResetInput(t *testing.T, s *Server, owner *http.Cookie, csrf string) (hub.BotBindingPackage, hub.BotPhoneReset) {
	t.Helper()
	w := authRequest(t, s, "POST", "bot/invites", `{"confirm":true}`, "https://example.test", csrf, owner)
	var pkg hub.BotBindingPackage
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &pkg) != nil {
		t.Fatalf("create Bot fixture: %d", w.Code)
	}
	w = authRequest(t, s, "GET", "bot/invites", "", "", "", owner)
	var out struct {
		Groups []struct {
			GroupID       string `json:"groupId"`
			PhoneRevision string `json:"phoneRevision"`
		} `json:"groups"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Groups) != 1 || len(out.Groups[0].PhoneRevision) != 32 || out.Groups[0].GroupID != pkg.GroupID {
		t.Fatal("missing public group revision")
	}
	return pkg, hub.BotPhoneReset{GroupID: pkg.GroupID, ExpectedRevision: out.Groups[0].PhoneRevision, RequestID: "12345678-1234-4234-8234-123456789abc", Confirm: true, Reason: "replace fixture phone"}
}

func botResetJSON(t *testing.T, in hub.BotPhoneReset) string {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestBotPhoneResetAdminAuthorizationAndStrictInput(t *testing.T) {
	s, _, owner, csrf := botAdminFixture(t)
	_, in := botAdminResetInput(t, s, owner, csrf)
	raw := botResetJSON(t, in)
	before := s.hub.BotGroupSummaries()
	for _, tc := range []struct {
		name, origin, csrf string
		cookie             *http.Cookie
		code               int
	}{
		{"anonymous", "https://example.test", csrf, nil, 401},
		{"missing csrf", "https://example.test", "", owner, 403},
		{"wrong csrf", "https://example.test", strings.Repeat("x", 43), owner, 403},
		{"cross origin", "https://attacker.example", csrf, owner, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cookies []*http.Cookie
			if tc.cookie != nil {
				cookies = append(cookies, tc.cookie)
			}
			if w := authRequest(t, s, "POST", "bot/phone/reset", raw, tc.origin, tc.csrf, cookies...); w.Code != tc.code {
				t.Fatalf("status %d want %d", w.Code, tc.code)
			}
		})
	}
	for _, role := range []string{"viewer", "operator"} {
		key := createMember(t, s, owner, csrf, "bot-"+role, role)
		cookie, token := memberLogin(t, s, key, role)
		for _, path := range []string{"bot/phone/reset", "bot/invites/revoke"} {
			if w := authRequest(t, s, "POST", path, raw, "https://example.test", token, cookie); w.Code != 403 {
				t.Fatalf("%s admitted to %s: %d", role, path, w.Code)
			}
		}
	}
	for _, mutate := range []func(*hub.BotPhoneReset){
		func(v *hub.BotPhoneReset) { v.Confirm = false },
		func(v *hub.BotPhoneReset) { v.GroupID = " " + v.GroupID },
		func(v *hub.BotPhoneReset) { v.ExpectedRevision = "1" },
		func(v *hub.BotPhoneReset) { v.RequestID = strings.ToUpper(v.RequestID) },
		func(v *hub.BotPhoneReset) { v.Reason = " \n\t" },
		func(v *hub.BotPhoneReset) { v.Reason = strings.Repeat("更", 86) },
	} {
		invalid := in
		mutate(&invalid)
		if w := authRequest(t, s, "POST", "bot/phone/reset", botResetJSON(t, invalid), "https://example.test", csrf, owner); w.Code != 400 {
			t.Fatalf("invalid reset accepted: %d", w.Code)
		}
	}
	for _, invalid := range []string{raw[:len(raw)-1] + `,"unknown":true}`, raw[:len(raw)-1] + `,"confirm":true}`, raw + `{}`} {
		if w := authRequest(t, s, "POST", "bot/phone/reset", invalid, "https://example.test", csrf, owner); w.Code != 400 {
			t.Fatalf("non-strict JSON accepted: %d", w.Code)
		}
	}
	if w := authRequest(t, s, "POST", "bot/phone/reset?groupId=other", raw, "https://example.test", csrf, owner); w.Code != 400 {
		t.Fatalf("query accepted: %d", w.Code)
	}
	if w := authRequest(t, s, "GET", "bot/phone/reset", "", "", "", owner); w.Code != 405 {
		t.Fatalf("wrong method accepted: %d", w.Code)
	}
	if !reflect.DeepEqual(before, s.hub.BotGroupSummaries()) {
		t.Fatal("denied requests changed a group")
	}
}

func TestBotPhoneResetAdminIdempotencyConflictAndPrivateAudit(t *testing.T) {
	s, cfg, owner, csrf := botAdminFixture(t)
	original, in := botAdminResetInput(t, s, owner, csrf)
	raw := botResetJSON(t, in)
	w := authRequest(t, s, "POST", "bot/phone/reset", raw, "https://example.test", csrf, owner)
	var result struct {
		OK            bool                  `json:"ok"`
		GroupID       string                `json:"groupId"`
		PhoneRevision string                `json:"phoneRevision"`
		Binding       hub.BotBindingPackage `json:"binding"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || !result.OK || result.GroupID != in.GroupID || result.Binding.GroupID != in.GroupID || len(result.PhoneRevision) != 32 || result.PhoneRevision == in.ExpectedRevision || result.Binding.Invite == original.Invite {
		t.Fatalf("invalid reset result: %d", w.Code)
	}
	second := authRequest(t, s, "POST", "bot/phone/reset", raw, "https://example.test", csrf, owner)
	if second.Code != 200 || second.Body.String() != w.Body.String() {
		t.Fatal("exact retry changed the replacement package/revision")
	}
	for _, changed := range []hub.BotPhoneReset{
		{GroupID: in.GroupID, ExpectedRevision: in.ExpectedRevision, RequestID: in.RequestID, Confirm: true, Reason: "changed reason"},
		{GroupID: in.GroupID, ExpectedRevision: in.ExpectedRevision, RequestID: "12345678-1234-4234-8234-123456789abd", Confirm: true, Reason: in.Reason},
	} {
		w := authRequest(t, s, "POST", "bot/phone/reset", botResetJSON(t, changed), "https://example.test", csrf, owner)
		if w.Code != 409 || !strings.Contains(w.Body.String(), "bot_phone_revision_conflict") {
			t.Fatalf("conflicting retry admitted: %d", w.Code)
		}
	}
	missing := in
	missing.GroupID = strings.Repeat("f", 32)
	w = authRequest(t, s, "POST", "bot/phone/reset", botResetJSON(t, missing), "https://example.test", csrf, owner)
	if w.Code != 404 || !strings.Contains(w.Body.String(), "bot_group_not_found") {
		t.Fatalf("missing exact group: %d", w.Code)
	}
	journal, err := os.ReadFile(filepath.Join(cfg.DataDir, operationsFile))
	if err != nil {
		t.Fatal(err)
	}
	var saved operationJournal
	if json.Unmarshal(journal, &saved) != nil {
		t.Fatal("invalid audit file")
	}
	found := false
	for _, record := range saved.Records {
		if record.Action == "bot/phone/reset" && record.Status == 200 && record.Target == in.GroupID && record.Reason == in.Reason && record.Detail == "bot-phone-reset" {
			found = true
		}
	}
	if !found {
		t.Fatal("reset target/reason missing from durable audit")
	}
	for _, secret := range []string{original.Invite, original.Signature, result.Binding.Invite, result.Binding.Signature, in.RequestID} {
		if strings.Contains(string(journal), secret) {
			t.Fatal("audit persisted reset request/package secrets")
		}
	}
}

func TestBotPhoneResetAdminAuditFailureBeforeMutation(t *testing.T) {
	for _, annotationOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial-audit", true: "target-reason-audit"}[annotationOnly], func(t *testing.T) {
			s, cfg, owner, csrf := botAdminFixture(t)
			_, in := botAdminResetInput(t, s, owner, csrf)
			before := s.hub.BotGroupSummaries()
			var id uint64
			if annotationOnly {
				var err error
				id, err = s.operations.begin(adminSession{}, Prefix+"/_admin/v1/bot/phone/reset")
				if err != nil {
					t.Fatal(err)
				}
			}
			// Only the exact private fixture file is replaced, never user data.
			file := filepath.Join(cfg.DataDir, operationsFile)
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(file, 0700); err != nil {
				t.Fatal(err)
			}
			var w *httptest.ResponseRecorder
			if annotationOnly {
				w = httptest.NewRecorder()
				r := httptest.NewRequest("POST", Prefix+"/_admin/v1/bot/phone/reset", strings.NewReader(botResetJSON(t, in)))
				r.Header.Set("Content-Type", "application/json")
				s.serveBotAdmin(&auditWriter{ResponseWriter: w, id: id}, r)
			} else {
				w = authRequest(t, s, "POST", "bot/phone/reset", botResetJSON(t, in), "https://example.test", csrf, owner)
			}
			if w.Code != 503 || !reflect.DeepEqual(before, s.hub.BotGroupSummaries()) {
				t.Fatalf("mutation ran without durable audit: %d", w.Code)
			}
		})
	}
}

func TestBotPhoneResetAdminNotAvailableOutsideBotMode(t *testing.T) {
	s, _, password := httpsFixture(t)
	owner, csrf := freshLogin(t, s, password)
	if w := authRequest(t, s, "POST", "bot/phone/reset", `{}`, "https://example.test", csrf, owner); w.Code != 404 {
		t.Fatalf("non-Bot reset route admitted: %d", w.Code)
	}
}

func TestBotResetRecoveryAssetIsServed(t *testing.T) {
	s, _, _ := serverFixture(t)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", Prefix+"/admin/bot-reset-store.js", nil))
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "javascript") || !strings.Contains(w.Body.String(), "SalcaraBotResetStore") {
		t.Fatalf("Bot recovery helper was not embedded/served: %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", Prefix+"/admin/", nil))
	helper, app := strings.Index(w.Body.String(), `src="./bot-reset-store.js"`), strings.Index(w.Body.String(), `src="./app.js"`)
	if w.Code != 200 || helper < 0 || helper >= app {
		t.Fatal("Bot recovery helper must load before the admin app")
	}
}

func TestBotInviteRevokeAdminUnavailableDoesNotReportMissing(t *testing.T) {
	for _, pendingReset := range []bool{false, true} {
		t.Run(map[bool]string{false: "invite-save-failure", true: "phone-reset-pending"}[pendingReset], func(t *testing.T) {
			s, cfg, owner, csrf := botAdminFixture(t)
			_, in := botAdminResetInput(t, s, owner, csrf)
			// This is an isolated test data directory. An exact, empty directory
			// in place of the snapshot makes atomic replacement fail on every OS.
			file := filepath.Join(cfg.DataDir, "bot-groups.json")
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Remove(file); err != nil {
				t.Fatal(err)
			}
			if err = os.Mkdir(file, 0700); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.Remove(file); err != nil {
					t.Error(err)
					return
				}
				if err := os.WriteFile(file, raw, 0600); err != nil {
					t.Error(err)
				}
			})
			if pendingReset {
				w := authRequest(t, s, "POST", "bot/phone/reset", botResetJSON(t, in), "https://example.test", csrf, owner)
				if w.Code != 503 || !strings.Contains(w.Body.String(), "bot_phone_reset_pending") {
					t.Fatalf("failed snapshot did not report pending reset: %d", w.Code)
				}
				if !s.hub.BotGroupSummaries()[0]["phonePending"].(bool) {
					t.Fatal("fixture reset did not remain pending")
				}
			}
			body, _ := json.Marshal(map[string]any{"groupId": in.GroupID, "confirm": true})
			w := authRequest(t, s, "POST", "bot/invites/revoke", string(body), "https://example.test", csrf, owner)
			if w.Code != 503 || strings.Contains(w.Body.String(), "不存在") {
				t.Fatalf("temporary revoke failure misreported as missing group: %d", w.Code)
			}
			body, _ = json.Marshal(map[string]any{"groupId": strings.Repeat("e", 32), "confirm": true})
			if w := authRequest(t, s, "POST", "bot/invites/revoke", string(body), "https://example.test", csrf, owner); w.Code != 404 {
				t.Fatalf("missing invitation group status %d", w.Code)
			}
		})
	}
}
