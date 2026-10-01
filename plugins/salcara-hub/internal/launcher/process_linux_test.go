//go:build linux

package launcher

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The child is the newly-built test executable, never an operator's program.
// This fixture path exists only in test binaries, not the shipped launcher.
func TestMain(m *testing.M) {
	if os.Getenv("SALCARA_LAUNCHER_TEST_CHILD") == "1" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
		defer stop()
		server := &http.Server{Addr: os.Getenv("SALCARA_HUB_LISTEN"), Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			pid := os.Getpid()
			if os.Getenv("SALCARA_LAUNCHER_TEST_BAD_PID") == "1" {
				pid = -1
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "service": "salcara-hub", "version": "0.4.0", "pid": pid})
		})}
		go func() {
			<-ctx.Done()
			grace, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			server.Shutdown(grace)
		}()
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
func testProcessRelease(t *testing.T) release {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f, err := openRegular(path, maxBinaryBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	hash, size, err := hashFile(f, maxBinaryBytes)
	if err != nil {
		t.Fatal(err)
	}
	return release{Path: path, Version: "0.4.0", SHA256: hash, Size: size}
}
func freeListen(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	l.Close()
	return address
}
func TestRunnerStartsVerifiedInodeAndWaitsBeforeReplacement(t *testing.T) {
	t.Setenv("SALCARA_LAUNCHER_TEST_CHILD", "1")
	r := testProcessRelease(t)
	runner := &managedRunner{cfg: Config{DataDir: t.TempDir(), Listen: freeListen(t)}}
	t.Cleanup(func() { runner.Stop() })
	if err := runner.Start(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if err := runner.Start(context.Background(), r); err == nil {
		t.Fatal("started a second live writer")
	}
	if err := runner.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := runner.Start(context.Background(), r); err != nil {
		t.Fatal("could not start after confirmed child exit", err)
	}
}
func TestRunnerRejectsWrongPIDAndStopsFailedChildBeforeFallback(t *testing.T) {
	t.Setenv("SALCARA_LAUNCHER_TEST_CHILD", "1")
	t.Setenv("SALCARA_LAUNCHER_TEST_BAD_PID", "1")
	r := testProcessRelease(t)
	runner := &managedRunner{cfg: Config{DataDir: t.TempDir(), Listen: freeListen(t)}}
	t.Cleanup(func() { runner.Stop() })
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if err := runner.Start(ctx, r); err == nil {
		t.Fatal("accepted same-version health from wrong PID")
	}
	if runner.child != nil {
		t.Fatal("failed child remained live before fallback")
	}
	t.Setenv("SALCARA_LAUNCHER_TEST_BAD_PID", "0")
	if err := runner.Start(context.Background(), r); err != nil {
		t.Fatal("fallback could not start after unhealthy child stopped", err)
	}
}
func TestRunnerDoesNotMistakeOccupiedPortForNewChild(t *testing.T) {
	t.Setenv("SALCARA_LAUNCHER_TEST_CHILD", "1")
	r := testProcessRelease(t)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "service": "salcara-hub", "version": "0.4.0", "pid": os.Getpid()})
	}))
	defer other.Close()
	runner := &managedRunner{cfg: Config{DataDir: t.TempDir(), Listen: strings.TrimPrefix(other.URL, "http://")}}
	t.Cleanup(func() { runner.Stop() })
	if err := runner.Start(context.Background(), r); err == nil {
		t.Fatal("accepted preexisting same-version Hub instead of new child")
	}
	if runner.child != nil {
		t.Fatal("occupied-port failure left an unconfirmed writer")
	}
}
