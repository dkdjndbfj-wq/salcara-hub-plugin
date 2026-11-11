package standalone

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestEnrollmentSourceReleaseKeepsAdminCSRFAndAuditBoundary(t *testing.T) {
	s, _, password := httpsFixture(t)
	owner, csrf := freshLogin(t, s, password)
	memberKey := createMember(t, s, owner, csrf, "Source read only", "viewer")
	viewer, viewerCSRF := memberLogin(t, s, memberKey, "viewer")
	state := s.hub.ManagedUsersSnapshot().(map[string]any)
	base := map[string]any{"action": "release-source", "expected_revision": state["revision"], "id": "usr-missing", "source_ref": strings.Repeat("a", 64), "reason": "Release future enrollment association", "confirm": true}
	raw, _ := json.Marshal(base)
	if w := authRequest(t, s, http.MethodPost, "users", string(raw), "https://example.test", ""); w.Code != http.StatusUnauthorized {
		t.Fatal("anonymous mutation not rejected", w.Code)
	}
	if w := authRequest(t, s, http.MethodPost, "users", string(raw), "https://example.test", viewerCSRF, viewer); w.Code != http.StatusForbidden {
		t.Fatal("viewer mutation not rejected", w.Code)
	}
	if w := authRequest(t, s, http.MethodPost, "users", string(raw), "https://example.test", "", owner); w.Code != http.StatusForbidden {
		t.Fatal("mutation without CSRF not rejected", w.Code)
	}
	if w := authRequest(t, s, http.MethodPost, "users", string(raw), "https://untrusted.example", csrf, owner); w.Code != http.StatusForbidden {
		t.Fatal("cross-origin mutation not rejected", w.Code)
	}
	for _, kind := range []string{"confirm", "reason", "source_ref"} {
		t.Run(kind, func(t *testing.T) {
			in := map[string]any{}
			for k, v := range base {
				in[k] = v
			}
			switch kind {
			case "confirm":
				in[kind] = false
			case "reason":
				in[kind] = ""
			case "source_ref":
				in[kind] = "not-an-association"
			}
			encoded, _ := json.Marshal(in)
			if w := authRequest(t, s, http.MethodPost, "users", string(encoded), "https://example.test", csrf, owner); w.Code != http.StatusBadRequest {
				t.Fatal("invalid scope/confirmation accepted", w.Code)
			}
		})
	}
	w := authRequest(t, s, http.MethodPost, "users", string(raw), "https://example.test", csrf, owner)
	if w.Code != http.StatusBadRequest {
		t.Fatal("nonexistent source association accepted", w.Code, w.Body)
	}
	s.operations.mu.Lock()
	found := false
	for _, record := range s.operations.Records {
		if record.Detail == "user-release-source" && record.Target == "usr-missing" && record.Reason == base["reason"] {
			found = true
		}
	}
	s.operations.mu.Unlock()
	if !found {
		t.Fatal("protected source release attempt lacks operation audit")
	}
	if now := s.hub.ManagedUsersSnapshot().(map[string]any); now["revision"] != state["revision"] {
		t.Fatal("rejected release changed user state")
	}
}
