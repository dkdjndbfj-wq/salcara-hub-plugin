package standalone

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestServerManagementUsesExistingAdminGuards(t *testing.T) {
	s, _, password := serverFixture(t)
	for _, path := range []string{"state", "discover", "check", "save", "plan", "deploy", "activate", "delete"} {
		method := "POST"
		if path == "state" {
			method = "GET"
		}
		if w := adminRequest(t, s, method, Prefix+"/_admin/v1/servers/"+path, "", map[string]any{}); w.Code != 401 {
			t.Fatalf("anonymous %s: %d", path, w.Code)
		}
	}
	w := adminRequest(t, s, "GET", Prefix+"/_admin/v1/servers/state", password, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"hosts":[]`) || strings.Contains(w.Body.String(), "password") {
		t.Fatalf("state %d %s", w.Code, w.Body.String())
	}
	w = adminRequest(t, s, "POST", Prefix+"/_admin/v1/servers/discover", password, map[string]any{"host": "127.0.0.1", "port": 22})
	if w.Code != 400 {
		t.Fatalf("private discovery: %d", w.Code)
	}
	w = adminRequest(t, s, "POST", Prefix+"/_admin/v1/servers/discover", password, map[string]any{"host": "example.com", "port": 22, "password": "synthetic-must-not-be-sent"})
	if w.Code != 400 || strings.Contains(w.Body.String(), "synthetic") {
		t.Fatalf("unexpected discovery credentials: %d %s", w.Code, w.Body.String())
	}
	w = adminRequest(t, s, "POST", Prefix+"/_admin/v1/servers/deploy", password, map[string]any{"plan_id": "missing", "expected_revision": 1})
	if w.Code != 400 {
		t.Fatalf("deployment without reviewed plan: %d", w.Code)
	}
	w = adminRequest(t, s, "GET", Prefix+"/_admin/v1/servers/state?secret=bad", password, nil)
	if w.Code != 400 {
		t.Fatal("query accepted")
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", Prefix+"/admin/servers.js", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "SalcaraServers") {
		t.Fatal("server UI script missing")
	}
}

func TestNetworkControllerIdentityAndTypedMetrics(t *testing.T) {
	var posts atomic.Int64
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["expected_controller_id"]; ok {
				t.Error("bridge leaked identity into Edge contract")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"revision":3,"mode":"balance","limits":{"max_requests":42,"password":"secret-fixture"},"runtime":{"active_requests":2,"token":"secret-fixture"},"nodes":[{"id":"direct","weight":2,"max_requests":10,"max_streams":4,"draining":true,"recovering":false,"runtime":{"active_streams":1,"password":"secret-fixture"}}]}`))
	}))
	defer controller.Close()
	s, password := networkFixture(t, controller.URL)
	path := Prefix + "/_admin/v1/network/"
	w := adminRequest(t, s, "GET", path+"state", password, nil)
	var state struct {
		ControllerID string `json:"controller_id"`
	}
	if json.Unmarshal(w.Body.Bytes(), &state) != nil || state.ControllerID == "" || w.Code != 200 {
		t.Fatal("missing controller identity")
	}
	if strings.Contains(w.Body.String(), "secret-fixture") || !strings.Contains(w.Body.String(), `"active_requests":2`) {
		t.Fatal("metrics were not typed/redacted")
	}
	w = adminRequest(t, s, "POST", path+"policy", password, map[string]any{"expected_revision": 3, "expected_controller_id": "wrong-controller", "mode": "balance"})
	if w.Code != 409 || posts.Load() != 0 {
		t.Fatal("stale controller reached wrong target")
	}
	w = adminRequest(t, s, "POST", path+"policy", password, map[string]any{"expected_revision": 3, "expected_controller_id": state.ControllerID, "mode": "balance"})
	if w.Code != 200 || posts.Load() != 1 {
		t.Fatalf("bound operation: %d %s", w.Code, w.Body.String())
	}
}
