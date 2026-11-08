//go:build linux && recoveryintegration

package launcher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Run only in a disposable Linux container. The two inputs must be actual Hub
// builds from this checkout, with Version set to 0.5.2 and 0.5.3 respectively.
// The feed transport and publisher are synthetic; no released private key,
// Internet update endpoint, existing data volume or operator account is used.
func TestInstalledHubUpgradeRecovery(t *testing.T) {
	oldPath, nextPath := os.Getenv("SALCARA_RECOVERY_OLD_HUB"), os.Getenv("SALCARA_RECOVERY_NEXT_HUB")
	if oldPath == "" || nextPath == "" {
		t.Fatal("recoveryintegration requires two explicit isolated Hub binary paths")
	}
	f := newFixture(t)
	oldBytes, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	nextBytes, err := os.ReadFile(nextPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(f.store.bootstrapPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(f.store.bootstrapPath, oldBytes, 0500); err != nil {
		t.Fatal(err)
	}
	f.store.bootstrapVersion, f.manager.cfg.BootstrapVersion = "0.5.2", "0.5.2"
	f.manager.cfg.Listen = freeListen(t)
	base := "http://" + f.manager.cfg.Listen
	t.Setenv("SALCARA_HUB_PUBLIC_URL", base+"/salcara-hub")
	f.manager.currentVersion("0.5.2")
	lock, err := launcherLock(f.store.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
	runner := &managedRunner{cfg: f.manager.cfg, volumeLock: lock}
	f.manager.runner = runner
	t.Cleanup(func() { runner.Stop() })

	// Create exclusively: accidentally running this outside an isolated fixture
	// must never overwrite a live launcher's control token or socket.
	controlToken := strings.Repeat("b", 64)
	tokenFile, err := os.OpenFile(ControlTokenFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := tokenFile.WriteString(controlToken + "\n")
	closeErr := tokenFile.Close()
	t.Cleanup(func() { os.Remove(ControlTokenFile) })
	if writeErr != nil || closeErr != nil {
		t.Fatal("fixture control credential write failed")
	}
	listener, err := net.Listen("unix", ControlSocket)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(ControlSocket, 0600); err != nil {
		listener.Close()
		t.Fatal(err)
	}
	controlServer := &http.Server{Handler: &control{manager: f.manager, tokenDigest: sha256.Sum256([]byte(controlToken))}}
	go controlServer.Serve(listener)
	t.Cleanup(func() { controlServer.Close() })
	init := exec.Command(f.store.bootstrapPath, "-init")
	init.Env = childEnvironment(os.Environ(), f.manager.cfg)
	if output, err := init.CombinedOutput(); err != nil {
		t.Fatalf("actual Hub installation failed: %v (%s)", err, output)
	}
	password, err := os.ReadFile(filepath.Join(f.manager.cfg.DataDir, "admin-initial-login.txt"))
	if err != nil {
		t.Fatal(err)
	}
	accountBefore, err := os.ReadFile(filepath.Join(f.manager.cfg.DataDir, "admin-account.json"))
	if err != nil {
		t.Fatal(err)
	}
	phoneToken := strings.Repeat("c", 64)
	phoneHash := sha256.Sum256([]byte(phoneToken))
	secretHash := sha256.Sum256([]byte(strings.Repeat("d", 64)))
	deviceID := "recovery-computer"
	deviceHash := sha256.Sum256([]byte(deviceID))
	device := map[string]any{"deviceId": deviceID, "name": "Recovery fixture", "os": "linux", "version": "fixture", "tools": []any{}, "projects": []any{map[string]any{"id": "retained-project", "path": "/synthetic/project"}}}
	paired := map[string]any{"device": device, "lastSeen": time.Now().UnixMilli(), "secretHash": hex.EncodeToString(secretHash[:]), "pairHash": hex.EncodeToString(phoneHash[:])}
	banned := map[string]any{"device": map[string]any{"deviceId": "banned-fixture", "name": "Banned fixture", "os": "linux", "tools": []any{}, "projects": []any{}}, "lastSeen": time.Now().UnixMilli(), "secretHash": hex.EncodeToString(secretHash[:]), "banned": true, "banReason": "fixture ban", "bannedAt": time.Now().UnixMilli()}
	seed, _ := json.Marshal(map[string]any{"version": 2, "accounts": map[string]any{"device:" + hex.EncodeToString(deviceHash[:]): []any{paired}, "fixture-banned-account": []any{banned}}})
	if err = os.WriteFile(filepath.Join(f.manager.cfg.DataDir, "devices.json"), seed, 0600); err != nil {
		t.Fatal(err)
	}
	old, err := f.store.resolve(f.manager.state.Current)
	if err != nil {
		t.Fatal(err)
	}
	if err = runner.Start(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}
	t.Cleanup(client.CloseIdleConnections)
	var cookie, csrf string
	request := func(method, path string, body any, headers map[string]string, want int) ([]byte, http.Header) {
		t.Helper()
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		req, err := http.NewRequest(method, base+path, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", base)
		req.Header.Set("Content-Type", "application/json")
		if strings.Contains(path, "/_admin/") {
			req.Header.Set("Cookie", cookie)
			req.Header.Set("X-Salcara-CSRF", csrf)
		}
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("actual Hub request %s failed: %v", path, err)
		}
		defer res.Body.Close()
		raw, err = io.ReadAll(io.LimitReader(res.Body, 1<<20))
		if err != nil || res.StatusCode != want {
			t.Fatalf("actual Hub request %s: got %d, want %d (body omitted to keep fixture secrets private)", path, res.StatusCode, want)
		}
		return raw, res.Header
	}
	login := func() {
		t.Helper()
		cookie, csrf = "", ""
		raw, headers := request("POST", "/salcara-hub/_admin/v1/auth/login", map[string]any{"password": strings.TrimSpace(string(password))}, nil, 200)
		cookie = strings.SplitN(headers.Get("Set-Cookie"), ";", 2)[0]
		var result struct {
			CSRF string `json:"csrf_token"`
		}
		if json.Unmarshal(raw, &result) != nil || cookie == "" || result.CSRF == "" {
			t.Fatal("actual Hub login response was incomplete")
		}
		csrf = result.CSRF
	}
	login()
	request("POST", "/salcara-hub/_admin/v1/resource-mode", map[string]any{"mode": "balanced", "confirm": true}, nil, 200)
	request("POST", "/salcara-hub/_admin/v1/device-cleanup", map[string]any{"unpaired_days": 14, "paired_days": 90, "confirm": true}, nil, 200)
	checkRetained := func(version string) {
		t.Helper()
		raw, _ := request("GET", "/salcara-hub/_admin/v1/state", nil, nil, 200)
		var state struct {
			Version string                          `json:"standalone_version"`
			Mode    string                          `json:"resource_mode"`
			Devices []struct{ Paired, Banned bool } `json:"devices"`
			Cleanup struct {
				Unpaired int `json:"unpaired_days"`
				Paired   int `json:"paired_days"`
			} `json:"device_cleanup"`
		}
		if json.Unmarshal(raw, &state) != nil || state.Version != version || state.Mode != "balanced" || state.Cleanup.Unpaired != 14 || state.Cleanup.Paired != 90 || len(state.Devices) != 2 {
			t.Fatal("installed Hub did not retain version, settings or both devices")
		}
		var pairedCount, bannedCount int
		for _, device := range state.Devices {
			if device.Paired {
				pairedCount++
			}
			if device.Banned {
				bannedCount++
			}
		}
		if pairedCount != 1 || bannedCount != 1 {
			t.Fatal("pairing or ban disappeared")
		}
		raw, _ = request("GET", "/salcara-hub/v1/app/devices", nil, map[string]string{"X-Salcara-Pair-Token": phoneToken}, 200)
		if !bytes.Contains(raw, []byte("retained-project")) {
			t.Fatal("paired phone lost its project metadata")
		}
		account, err := os.ReadFile(filepath.Join(f.manager.cfg.DataDir, "admin-account.json"))
		if err != nil || !bytes.Equal(account, accountBefore) {
			t.Fatal("application update changed management credentials")
		}
	}
	checkRetained("0.5.2")
	setFeed := func(version string, binary []byte) {
		f.binary = binary
		feed := testFeed(binary)
		feed.Version = version
		f.feed = fixtureFeed(t, f.private, feed)
	}
	apply := func() {
		t.Helper()
		raw, _ := request("POST", "/salcara-hub/_admin/v1/update/check", map[string]any{}, nil, 200)
		var check Status
		if json.Unmarshal(raw, &check) != nil || check.Status != "available" {
			t.Fatal("installed Hub did not discover signed fixture update")
		}
		request("POST", "/salcara-hub/_admin/v1/update/apply", map[string]any{"version": check.LatestVersion, "sha256": check.SHA256, "confirm": true}, nil, 202)
		f.manager.job.Wait()
	}
	setFeed("0.5.3", nextBytes)
	apply()
	if s := f.manager.snapshot(); s.Status != "current" || s.CurrentVersion != "0.5.3" {
		t.Fatal("actual Hub upgrade did not commit", s.Status, s.CurrentVersion)
	}
	request("GET", "/salcara-hub/_admin/v1/state", nil, nil, 401)
	login()
	checkRetained("0.5.3")
	t.Log("actual Hub 0.5.2 install -> signed 0.5.3 update: health/PID accepted, old login revoked, credentials/pair/project/ban/settings retained")
	if err = runner.Stop(); err != nil {
		t.Fatal(err)
	}
	restored, err := f.store.load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.startConfirmed(context.Background(), restored, runner); err != nil {
		t.Fatal(err)
	}
	request("GET", "/salcara-hub/_admin/v1/state", nil, nil, 401)
	login()
	checkRetained("0.5.3")
	t.Log("actual Hub restart selected persisted signed cache instead of older bootstrap; data and credentials retained")

	// Truncation is injected in the synthetic transport; the running Hub remains
	// the real binary and its existing admin session must remain valid.
	setFeed("0.5.4", nextBytes)
	f.binary = nextBytes[:len(nextBytes)/2]
	originalTransport := f.manager.client.Transport
	f.manager.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		res, err := originalTransport.RoundTrip(req)
		if err == nil && req.URL.Path != "/feed.json" {
			// The header promises the signed size, but EOF arrives halfway
			// through the body, exercising the interrupted copy path.
			res.ContentLength = int64(len(nextBytes))
		}
		return res, err
	})
	apply()
	f.manager.client.Transport = originalTransport
	if s := f.manager.snapshot(); s.Status != "failed" || s.CurrentVersion != "0.5.3" {
		t.Fatal("interrupted download changed confirmed version")
	}
	checkRetained("0.5.3")
	t.Log("truncated download failed before stopping the actual Hub; existing authenticated session remained valid")

	// This is a valid signed executable with deliberately mismatched health
	// version, so acceptance must fail and the confirmed real Hub must return.
	setFeed("0.5.4", oldBytes)
	apply()
	if s := f.manager.snapshot(); s.Status != "failed" || s.CurrentVersion != "0.5.3" {
		t.Fatal("unhealthy target did not restore confirmed real Hub")
	}
	request("GET", "/salcara-hub/_admin/v1/state", nil, nil, 401)
	login()
	checkRetained("0.5.3")
	t.Log("signed target with wrong actual health version was stopped; real 0.5.3 restored with all persisted fixture data")
}
