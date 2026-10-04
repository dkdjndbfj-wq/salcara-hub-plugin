//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package standalone

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestUpdateRealUnixSocketIgnoresProxyAndKeepsControlTokenPrivate(t *testing.T) {
	cfg := testConfig(t)
	cfg.ControlSocket = filepath.Join(cfg.DataDir, "control.sock")
	cfg.ControlTokenFile = filepath.Join(cfg.DataDir, "control-token")
	if err := InitAdminAccount(cfg); err != nil {
		t.Fatal(err)
	}
	admin := initialPassword(t, cfg)
	if err := initControlToken(cfg.ControlTokenFile); err != nil {
		t.Fatal(err)
	}
	control, err := readControlToken(cfg.ControlTokenFile)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", cfg.ControlSocket)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	local := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+string(control) || r.Header.Get("Cookie") != "" {
			t.Error("wrong control credential")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"configured":true,"current_version":"0.4.0-dev","status":"current","message":"Unix fixture"}`)
	})}
	go local.Serve(listener)
	defer local.Close()
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w := adminRequest(t, s, http.MethodGet, Prefix+"/_admin/v1/update/status", string(admin), nil)
	if w.Code != 200 || calls.Load() != 1 {
		t.Fatal("Unix-only update control failed")
	}
}
