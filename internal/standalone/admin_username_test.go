package standalone

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
)

func TestAdminUsernameMigrationCannotBeBypassedAndPersists(t *testing.T) {
	s, cfg, password := httpsFixture(t)
	cookie, csrf := freshLogin(t, s, password)
	next := "fixture-new-administrator-password-1234"
	raw, _ := json.Marshal(map[string]string{"current_password": password, "new_password": next, "username": "hub-owner"})
	w := authRequest(t, s, "POST", "auth/password", string(raw), "https://example.test", csrf, cookie)
	if w.Code != 200 {
		t.Fatalf("account migration: %d %s", w.Code, w.Body.String())
	}
	if w := authRequest(t, s, "GET", "state", "", "", "", cookie); w.Code != 401 {
		t.Fatal("old session survived account change")
	}
	s.Close()
	restarted, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	for _, name := range []string{"", "admin", "wrong-owner", "hub-owner"} {
		raw, _ := json.Marshal(map[string]string{"username": name, "password": next})
		w := authRequest(t, restarted, "POST", "auth/login", string(raw), "https://example.test", "")
		want := 401
		if name == "hub-owner" {
			want = 200
		}
		if w.Code != want {
			t.Fatalf("username %q got %d want %d", name, w.Code, want)
		}
	}
}
