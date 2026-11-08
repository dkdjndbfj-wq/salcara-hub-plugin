package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func recoveryToken(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRecoveryUsesAuthenticatedLocalChannelAndCAS(t *testing.T) {
	token := recoveryToken(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("a", 64) || r.URL.RawQuery != "" {
			t.Error("missing independent header auth")
		}
		if r.Method == "POST" {
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["expected_revision"] != float64(42) || body["trial_id"] != "trial" || r.URL.Path != "/v1/releases/rollback" {
				t.Error("lost explicit rollback ownership")
			}
		}
		_, _ = w.Write([]byte(`{"revision":43,"status":"ready","release":{"trial":null,"previews":[],"history":[],"last_error":""},"private_snapshot":"server-secret"}`))
	}))
	defer server.Close()
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	for _, action := range []string{"state", "rollback"} {
		args := []string{action, "--control-listen", strings.TrimPrefix(server.URL, "http://"), "--control-token-file", token}
		if action == "rollback" {
			args = append(args, "--expected-revision", "42", "--trial-id", "trial")
		}
		var output bytes.Buffer
		if err := runRecovery(args, &output); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(output.String(), "server-secret") || strings.Contains(output.String(), strings.Repeat("a", 64)) {
			t.Fatal("recovery leaked secret")
		}
		if !strings.Contains(output.String(), `"revision": 43`) {
			t.Fatal("missing state")
		}
	}
}

func TestRecoveryRejectsNonLoopbackMissingCASRedirectAndLargeResponse(t *testing.T) {
	token := recoveryToken(t)
	for _, address := range []string{"localhost:8791", "0.0.0.0:8791", "192.0.2.1:8791", "127.0.0.1:0", "127.0.0.1:http"} {
		if err := runRecovery([]string{"state", "--control-listen", address, "--control-token-file", token}, &bytes.Buffer{}); err == nil {
			t.Fatalf("address accepted: %s", address)
		}
	}
	if err := runRecovery([]string{"rollback", "--trial-id", "trial", "--control-token-file", token}, &bytes.Buffer{}); err == nil {
		t.Fatal("missing CAS accepted")
	}
	var reached atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
	defer target.Close()
	for _, mode := range []string{"redirect", "large", "auth"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "redirect":
					http.Redirect(w, r, target.URL, 302)
				case "large":
					_, _ = w.Write([]byte(strings.Repeat("x", (1<<20)+1)))
				default:
					w.WriteHeader(401)
					_, _ = w.Write([]byte("sensitive-upstream-error"))
				}
			}))
			defer server.Close()
			var output bytes.Buffer
			err := runRecovery([]string{"state", "--control-listen", strings.TrimPrefix(server.URL, "http://"), "--control-token-file", token}, &output)
			if err == nil || strings.Contains(err.Error(), "sensitive-") || output.Len() != 0 {
				t.Fatal("invalid or sensitive response exposed")
			}
		})
	}
	if reached.Load() != 0 {
		t.Fatal("recovery followed redirect")
	}
}
