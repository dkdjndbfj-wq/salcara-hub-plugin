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
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The child is the newly-built test executable, never an operator's program.
// This fixture path exists only in test binaries, not the shipped launcher.
func TestMain(m *testing.M) {
	if os.Getenv("SALCARA_LAUNCHER_TEST_CHILD") == "1" {
		version := "0.4.0"
		if os.Args[0] == os.Getenv("SALCARA_LAUNCHER_TEST_IMAGE_PATH") {
			if os.Getenv("SALCARA_LAUNCHER_TEST_FAIL_IMAGE") == "1" {
				os.Exit(2)
			}
			version = os.Getenv("SALCARA_LAUNCHER_TEST_IMAGE_VERSION")
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
		defer stop()
		server := &http.Server{Addr: os.Getenv("SALCARA_HUB_LISTEN"), Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			pid := os.Getpid()
			if os.Getenv("SALCARA_LAUNCHER_TEST_BAD_PID") == "1" {
				pid = -1
			}
			product := Product
			if os.Getenv("SALCARA_LAUNCHER_TEST_WRONG_PRODUCT") == "1" {
				product = "salcara-personal-hub"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "service": "salcara-hub", "product": product, "version": version, "pid": pid})
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
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "service": "salcara-hub", "product": Product, "version": "0.4.0", "pid": os.Getpid()})
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

func TestRunnerRejectsAnotherProductBeforeFallback(t *testing.T) {
	t.Setenv("SALCARA_LAUNCHER_TEST_CHILD", "1")
	t.Setenv("SALCARA_LAUNCHER_TEST_WRONG_PRODUCT", "1")
	r := testProcessRelease(t)
	runner := &managedRunner{cfg: Config{DataDir: t.TempDir(), Listen: freeListen(t)}}
	t.Cleanup(func() { runner.Stop() })
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if err := runner.Start(ctx, r); err == nil {
		t.Fatal("accepted another product with the same version and PID")
	}
	if runner.child != nil {
		t.Fatal("wrong product remained live before fallback")
	}
	t.Setenv("SALCARA_LAUNCHER_TEST_WRONG_PRODUCT", "0")
	if err := runner.Start(context.Background(), r); err != nil {
		t.Fatal("fallback could not start after wrong product stopped", err)
	}
}

func processCachedFixture(t *testing.T) (*fixture, diskState) {
	t.Helper()
	f := newFixture(t)
	executable := testProcessRelease(t)
	data, err := os.ReadFile(executable.Path)
	if err != nil {
		t.Fatal(err)
	}
	f.binary = data
	f.feed = fixtureFeed(t, f.private, testFeed(data))
	_, start := acceptedFixture(t, f)
	start(true)
	f.manager.job.Wait()
	if err = os.Chmod(f.store.bootstrapPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(f.store.bootstrapPath, data, 0500); err != nil {
		t.Fatal(err)
	}
	f.store.bootstrapVersion = "0.5.0"
	t.Setenv("SALCARA_LAUNCHER_TEST_CHILD", "1")
	t.Setenv("SALCARA_LAUNCHER_TEST_IMAGE_PATH", f.store.bootstrapPath)
	t.Setenv("SALCARA_LAUNCHER_TEST_IMAGE_VERSION", "0.5.0")
	state, err := f.store.load()
	if err != nil {
		t.Fatal(err)
	}
	return f, state
}

func TestImageUpgradeStartsRealVerifiedChildAndConfirmsVersion(t *testing.T) {
	f, state := processCachedFixture(t)
	runner := &managedRunner{cfg: Config{DataDir: f.manager.cfg.DataDir, Listen: freeListen(t)}}
	t.Cleanup(func() { runner.Stop() })
	confirmed, err := f.store.startConfirmed(context.Background(), state, runner)
	if err != nil || !confirmed.Current.Bootstrap || confirmed.Previous == nil || confirmed.Previous.Bootstrap {
		t.Fatal("new image child was not health-confirmed with signed fallback", err)
	}
	if current, err := f.store.resolve(confirmed.Current); err != nil || current.Version != "0.5.0" {
		t.Fatal("real image child version was not committed", err)
	}
}

func TestUnhealthyImageStopsRealChildBeforeSignedCacheFallback(t *testing.T) {
	f, state := processCachedFixture(t)
	t.Setenv("SALCARA_LAUNCHER_TEST_FAIL_IMAGE", "1")
	runner := &managedRunner{cfg: Config{DataDir: f.manager.cfg.DataDir, Listen: freeListen(t)}}
	t.Cleanup(func() { runner.Stop() })
	rolledBack, err := f.store.startConfirmed(context.Background(), state, runner)
	if err != nil || rolledBack.Current.Bootstrap || rolledBack.FailedBootstrap == nil {
		t.Fatal("real unhealthy image could not fall back to verified cache", err)
	}
	if current, err := f.store.resolve(rolledBack.Current); err != nil || current.Version != "0.4.0" || !strings.HasPrefix(filepath.Base(current.Path), "hub-") {
		t.Fatal("fallback did not use signed cache executable", err)
	}
	if err = runner.Stop(); err != nil {
		t.Fatal("fallback process could not be safely stopped", err)
	}
	restored, err := f.store.load()
	if err != nil {
		t.Fatal(err)
	}
	if restarted, err := f.store.startConfirmed(context.Background(), restored, runner); err != nil || restarted.Current.Bootstrap {
		t.Fatal("container restart did not keep verified cache after image failure", err)
	}
}
