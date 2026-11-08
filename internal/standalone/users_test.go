package standalone

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestManagedUsersAdminOnlyWritesAndConflict(t *testing.T) {
	s, _, password := httpsFixture(t)
	owner, csrf := freshLogin(t, s, password)
	key := createMember(t, s, owner, csrf, "用户只读", "viewer")
	viewer, vcsrf := memberLogin(t, s, key, "viewer")
	for _, tc := range []struct {
		method, body string
		status       int
	}{{"GET", "", 200}, {"POST", `{}`, 403}} {
		if w := authRequest(t, s, tc.method, "users", tc.body, "https://example.test", vcsrf, viewer); w.Code != tc.status {
			t.Fatalf("viewer %s %d", tc.method, w.Code)
		}
	}
	w := authRequest(t, s, "GET", "users", "", "", "", owner)
	var state struct {
		Revision string `json:"revision"`
		Users    []struct {
			ID       string `json:"id"`
			Disabled bool   `json:"disabled"`
		} `json:"users"`
	}
	if json.Unmarshal(w.Body.Bytes(), &state) != nil || state.Revision == "" {
		t.Fatal("no users revision")
	}
	raw, _ := json.Marshal(map[string]any{"action": "save", "expected_revision": state.Revision, "name": "小林", "reason": "首次开通", "confirm": true})
	if w := authRequest(t, s, "POST", "users", string(raw), "https://example.test", "", owner); w.Code != 403 {
		t.Fatal("write without CSRF")
	}
	w = authRequest(t, s, "POST", "users", string(raw), "https://example.test", csrf, owner)
	if w.Code != 200 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if w := authRequest(t, s, "POST", "users", string(raw), "https://example.test", csrf, owner); w.Code != 409 {
		t.Fatal("stale editor accepted")
	}
	if json.Unmarshal(w.Body.Bytes(), &state) != nil || len(state.Users) != 1 {
		t.Fatal("created user missing")
	}
	invite, _ := json.Marshal(map[string]any{"user_id": state.Users[0].ID, "confirm": true})
	if w := authRequest(t, s, "POST", "enrollment-claims", string(invite), "https://example.test", csrf, owner); w.Code != 201 {
		t.Fatal("user invitation failed")
	}
	if w := authRequest(t, s, "GET", "enrollment-claims", "", "", "", viewer); w.Code != 403 {
		t.Fatal("viewer can read enrollment records")
	}
	for _, path := range []string{"users.js", "users.css"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", Prefix+"/admin/"+path, nil))
		if w.Code != 200 {
			t.Fatalf("asset %s %d", path, w.Code)
		}
	}
}
