package standalone

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func memberLogin(t *testing.T, s *Server, key, role string) (*http.Cookie, string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"password": key})
	w := authRequest(t, s, "POST", "auth/login", string(raw), "https://example.test", "")
	var data map[string]string
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &data) != nil || data["role"] != role {
		t.Fatalf("member login %s: %d %s", role, w.Code, w.Body.String())
	}
	return w.Result().Cookies()[0], data["csrf_token"]
}
func createMember(t *testing.T, s *Server, cookie *http.Cookie, csrf, name, role string) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"action": "create", "name": name, "role": role, "confirm": true})
	w := authRequest(t, s, "POST", "operations/members", string(raw), "https://example.test", csrf, cookie)
	var data struct {
		Key string `json:"key"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &data) != nil || len(data.Key) != 47 {
		t.Fatalf("member create %d %s", w.Code, w.Body.String())
	}
	return data.Key
}
func TestOperationsRolesRevocationAndMonitorIsolation(t *testing.T) {
	s, cfg, password := httpsFixture(t)
	owner, csrf := freshLogin(t, s, password)
	viewerKey := createMember(t, s, owner, csrf, "只读同事", "viewer")
	viewer, viewerCSRF := memberLogin(t, s, viewerKey, "viewer")
	operatorKey := createMember(t, s, owner, csrf, "运维同事", "operator")
	operator, operatorCSRF := memberLogin(t, s, operatorKey, "operator")
	monitorKey := createMember(t, s, owner, csrf, "Prometheus", "monitor")
	for _, tc := range []struct {
		path, method, body string
		code               int
	}{
		{"state", "GET", "", 200}, {"operations/health", "GET", "", 200}, {"operations/audit", "GET", "", 200}, {"operations/members", "GET", "", 403},
		{"storage", "POST", `{"runNow":true}`, 403}, {"resource-mode", "POST", `{}`, 403}, {"servers/state", "GET", "", 403}, {"network/state", "GET", "", 403},
		{"auth/password", "POST", `{}`, 403}, {"action", "POST", `{}`, 403}, {"enrollment-claims", "POST", `{"confirm":true}`, 403}, {"unknown-new-api", "GET", "", 403},
	} {
		w := authRequest(t, s, tc.method, tc.path, tc.body, "https://example.test", viewerCSRF, viewer)
		if w.Code != tc.code {
			t.Errorf("viewer %s got %d want %d", tc.path, w.Code, tc.code)
		}
	}
	w := authRequest(t, s, "POST", "action", `{"action":"ping","device_ref":"unknown","confirm":true}`, "https://example.test", operatorCSRF, operator)
	if w.Code == 403 || w.Code == 401 || w.Code >= 500 {
		t.Fatalf("operator device action not admitted: %d", w.Code)
	}
	w = authRequest(t, s, "POST", "operations/members", `{"action":"create","name":"escalation","role":"operator","confirm":true}`, "https://example.test", operatorCSRF, operator)
	if w.Code != 403 {
		t.Fatal("operator escalated")
	}
	for _, key := range []string{"", viewerKey, password, monitorKey} {
		r := httptest.NewRequest("GET", Prefix+"/_monitor/v1/metrics", nil)
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		want := 401
		if key == monitorKey {
			want = 200
		}
		if w.Code != want {
			t.Errorf("monitor isolation got %d want %d", w.Code, want)
		}
		if strings.Contains(w.Body.String(), monitorKey) {
			t.Fatal("monitor key leaked")
		}
	}
	w = authRequest(t, s, "POST", "auth/login", `{"password":"`+monitorKey+`"}`, "https://example.test", "")
	if w.Code != 401 {
		t.Fatal("monitor can login")
	}
	raw, err := os.ReadFile(filepath.Join(cfg.DataDir, membersFile))
	if err != nil || strings.Contains(string(raw), viewerKey) || strings.Contains(string(raw), monitorKey) {
		t.Fatal("plaintext keys persisted")
	}
	s.auth.mu.Lock()
	id := s.auth.members[0].ID
	s.auth.mu.Unlock()
	w = authRequest(t, s, "POST", "operations/members", `{"action":"revoke","id":"`+id+`","confirm":true}`, "https://example.test", csrf, owner)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = authRequest(t, s, "GET", "state", "", "https://example.test", "", viewer)
	if w.Code != 401 {
		t.Fatal("revoked session survived")
	}
	s.Close()
	restarted, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	memberLogin(t, restarted, operatorKey, "operator")
	journal, err := os.ReadFile(filepath.Join(cfg.DataDir, operationsFile))
	if err != nil || strings.Contains(string(journal), viewerKey) || strings.Contains(string(journal), monitorKey) {
		t.Fatal("audit leaked secret")
	}
	if !strings.Contains(string(journal), `"status":200`) {
		t.Fatal("operation results not persisted")
	}
}

func TestAuditFailClosedBeforeMutationAndFiltering(t *testing.T) {
	s, cfg, password := httpsFixture(t)
	owner, csrf := freshLogin(t, s, password)
	createMember(t, s, owner, csrf, "audit-member", "viewer")
	w := authRequest(t, s, "GET", "operations/audit?query=operations%2Fmembers&result=success", "", "https://example.test", "", owner)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "operations/members") {
		t.Fatal("filtered audit missing")
	}
	w = authRequest(t, s, "GET", "operations/audit/export?result=success", "", "https://example.test", "", owner)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "ndjson") || !strings.Contains(w.Body.String(), "operations/members") {
		t.Fatal("export missing")
	}
	w = authRequest(t, s, "GET", "operations/audit?unexpected=x&before=1", "", "https://example.test", "", owner)
	if w.Code != 400 {
		t.Fatal("unknown audit query accepted")
	}
	file := filepath.Join(cfg.DataDir, operationsFile)
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(file, 0700); err != nil {
		t.Fatal(err)
	}
	w = authRequest(t, s, "POST", "operations/members", `{"action":"create","name":"must-not-exist","role":"viewer","confirm":true}`, "https://example.test", csrf, owner)
	if w.Code != 503 {
		t.Fatalf("audit write failure was admitted: %d", w.Code)
	}
	s.auth.mu.Lock()
	defer s.auth.mu.Unlock()
	if len(s.auth.members) != 1 {
		t.Fatal("mutation executed without durable intent")
	}
}

func TestAuditPendingSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	j, e := openOperations(dir)
	if e != nil {
		t.Fatal(e)
	}
	id, e := j.begin(adminSession{}, Prefix+"/_admin/v1/resource-mode")
	if e != nil || id == 0 {
		t.Fatal(e)
	}
	j.close()
	j, e = openOperations(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer j.close()
	if len(j.Records) != 1 || j.Records[0].Status != 0 {
		t.Fatal("pending operation not retained")
	}
}

func TestOperationsFilesCannotReplaceCustomOwnerCredential(t *testing.T) {
	for _, name := range []string{membersFile, operationsFile, backupReceiptFile, strings.ToUpper(backupReceiptFile)} {
		cfg := testConfig(t)
		cfg.AdminAccountFile = filepath.Join(cfg.DataDir, name)
		if err := cfg.Validate(); err == nil {
			t.Fatalf("reserved account path accepted: %s", name)
		}
		if err := InitAdminAccount(cfg); err == nil {
			t.Fatalf("initialized reserved account path: %s", name)
		}
	}
}

func TestOperatorDeviceMutationKeepsActorAndSpecificAudit(t *testing.T) {
	s, _, password := httpsFixture(t)
	owner, csrf := freshLogin(t, s, password)
	key := createMember(t, s, owner, csrf, "值班运维", "operator")
	operator, operatorCSRF := memberLogin(t, s, key, "operator")
	w := authRequest(t, s, "POST", "enrollment-claims", `{"confirm":true}`, "https://example.test", csrf, owner)
	var enrolled struct {
		Claim string `json:"claim"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &enrolled) != nil {
		t.Fatal("enrollment", w.Code)
	}
	r := httptest.NewRequest("POST", "https://example.test"+Prefix+"/v1/device/register", strings.NewReader(`{"deviceId":"ops-fixture","name":"fixture"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Salcara-Device-Id", "ops-fixture")
	r.Header.Set("X-Salcara-Device-Secret", strings.Repeat("a", 64))
	r.Header.Set("X-Salcara-Enrollment-Claim", enrolled.Claim)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("registration", w.Code, w.Body.String())
	}
	w = authRequest(t, s, "GET", "state", "", "https://example.test", "", operator)
	var state struct {
		Devices []struct {
			Ref string `json:"ref"`
		} `json:"devices"`
		Audit []struct {
			Actor string `json:"actor"`
		} `json:"audit"`
	}
	if json.Unmarshal(w.Body.Bytes(), &state) != nil || len(state.Devices) != 1 {
		t.Fatal("missing device")
	}
	ref := state.Devices[0].Ref
	w = authRequest(t, s, "POST", "action", `{"action":"ban","device_ref":"`+ref+`","reason":"维护测试","confirm":true}`, "https://example.test", operatorCSRF, operator)
	if w.Code != 200 {
		t.Fatal("operator action", w.Code, w.Body.String())
	}
	w = authRequest(t, s, "GET", "state", "", "https://example.test", "", operator)
	if json.Unmarshal(w.Body.Bytes(), &state) != nil || len(state.Audit) == 0 || !strings.HasPrefix(state.Audit[len(state.Audit)-1].Actor, "standalone-member:") {
		t.Fatal("device actor collapsed", w.Body.String())
	}
	w = authRequest(t, s, "GET", "operations/audit?query=ban", "", "https://example.test", "", owner)
	var audit struct {
		Records []operationRecord `json:"records"`
	}
	if json.Unmarshal(w.Body.Bytes(), &audit) != nil || len(audit.Records) != 1 || audit.Records[0].Target != ref || audit.Records[0].Status != 200 || audit.Records[0].Name != "值班运维" {
		t.Fatal("specific device audit missing", w.Body.String())
	}
}
