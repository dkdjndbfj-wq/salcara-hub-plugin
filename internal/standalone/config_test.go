package standalone

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	return Config{DataDir: dir, AdminAccountFile: filepath.Join(dir, defaultAccountFile), Listen: ":8787", CommandTimeout: time.Second, PingInterval: 5 * time.Second}
}

func TestEnvConfigAndOriginValidation(t *testing.T) {
	dir := t.TempDir()
	base := map[string]string{"SALCARA_HUB_DATA_DIR": dir, "SALCARA_HUB_ADMIN_ACCOUNT_FILE": filepath.Join(dir, defaultAccountFile)}
	cfg, err := ConfigFromEnv(func(key string) string { return base[key] })
	if err != nil || cfg.Listen != ":8787" || cfg.AdminAccountFile != base["SALCARA_HUB_ADMIN_ACCOUNT_FILE"] {
		t.Fatalf("unexpected defaults: %v", err)
	}
	for _, tc := range []struct{ key, value string }{
		{"SALCARA_HUB_DATA_DIR", ""}, {"SALCARA_HUB_DATA_DIR", "relative"},
		{"SALCARA_HUB_LISTEN", "example.com:8787"}, {"SALCARA_HUB_LISTEN", ":0"},
		{"SALCARA_HUB_COMMAND_TIMEOUT", "121s"}, {"SALCARA_HUB_PING_INTERVAL", "1s"},
		{"SALCARA_HUB_PUBLIC_URL", "http://example.com/salcara-hub"},
		{"SALCARA_HUB_PUBLIC_URL", "https://user:secret@example.com/salcara-hub"},
		{"SALCARA_HUB_PUBLIC_URL", "https://example.com/salcara-hub?auth_token=example"},
		{"SALCARA_HUB_PUBLIC_URL", "https://example.com/other"},
		{"SALCARA_HUB_PUBLIC_URL", "https://example.com:70000/salcara-hub"},
	} {
		values := map[string]string{"SALCARA_HUB_DATA_DIR": dir, "SALCARA_HUB_ADMIN_ACCOUNT_FILE": filepath.Join(dir, defaultAccountFile)}
		values[tc.key] = tc.value
		if _, err := ConfigFromEnv(func(key string) string { return values[key] }); err == nil {
			t.Fatalf("invalid configuration accepted for %s", tc.key)
		}
	}
	for _, publicURL := range []string{"https://relay.example.com/salcara-hub", "http://localhost:8787/salcara-hub", "http://[::1]:8787/salcara-hub"} {
		base["SALCARA_HUB_PUBLIC_URL"] = publicURL
		if _, err := ConfigFromEnv(func(key string) string { return base[key] }); err != nil {
			t.Fatalf("valid public URL rejected: %v", err)
		}
	}
}

func TestAccountInitializationDoesNotReplaceAndRejectsUnsafeFiles(t *testing.T) {
	cfg := testConfig(t)
	if err := InitAdminAccount(cfg); err != nil {
		t.Fatal(err)
	}
	root, err := openAuthRoot(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	before, err := loadAdminAccount(root, defaultAccountFile)
	if err != nil || !verifyPassword(before, initialPassword(t, cfg)) {
		t.Fatal("initialized account not accepted")
	}
	if err := InitAdminAccount(cfg); err == nil {
		t.Fatal("existing admin account replaced")
	}
	after, err := loadAdminAccount(root, defaultAccountFile)
	if err != nil || before != after {
		t.Fatal("existing account changed")
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(cfg.AdminAccountFile, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadAdminAccount(root, defaultAccountFile); err == nil {
			t.Fatal("world-readable secret accepted")
		}
		if err := os.Chmod(cfg.AdminAccountFile, 0600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(cfg.DataDir, "account-link")
	if err := os.Symlink(cfg.AdminAccountFile, link); err == nil {
		if _, err := loadAdminAccount(root, "account-link"); err == nil {
			t.Fatal("account symlink accepted")
		}
	}
	// Test-only fixtures are independent fake strings, not local model keys.
	for i, fake := range []string{strings.Repeat("a", 31), "sk-" + strings.Repeat("a", 64), strings.Repeat("a", 40) + " bearer", strings.Repeat("a", 40) + "\nsecond-line"} {
		path := filepath.Join(cfg.DataDir, string(rune('a'+i)))
		if err := os.WriteFile(path, []byte(fake), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadAdminAccount(root, filepath.Base(path)); err == nil {
			t.Fatal("unsafe test token accepted")
		}
	}
}

func TestDataLockExclusiveAndRetainedFile(t *testing.T) {
	dir := t.TempDir()
	a, err := acquireDataLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := acquireDataLock(dir); err == nil {
		b.release()
		t.Fatal("second writer acquired the same data volume")
	}
	if err := a.release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".salcara-hub.lock")); err != nil {
		t.Fatal("lock file removed, allowing a different inode race")
	}
	b, err := acquireDataLock(dir)
	if err != nil {
		t.Fatal("retained lock file prevented restart")
	}
	b.release()
}

func TestDataLockReleasesAfterProcessCrash(t *testing.T) {
	if os.Getenv("SALCARA_LOCK_TEST_CHILD") == "1" {
		lock, err := acquireDataLock(os.Getenv("SALCARA_LOCK_TEST_DIR"))
		if err != nil {
			t.Fatal(err)
		}
		defer lock.release()
		fmt.Println("test-lock-ready")
		for {
			time.Sleep(time.Second)
		}
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDataLockReleasesAfterProcessCrash$")
	child.Env = append(os.Environ(), "SALCARA_LOCK_TEST_CHILD=1", "SALCARA_LOCK_TEST_DIR="+dir)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || strings.TrimSpace(line) != "test-lock-ready" {
		child.Process.Kill()
		child.Wait()
		t.Fatal("isolated child did not acquire its test data lock")
	}
	if lock, err := acquireDataLock(dir); err == nil {
		lock.release()
		child.Process.Kill()
		child.Wait()
		t.Fatal("another process could acquire a live lock")
	}
	if err := child.Process.Kill(); err != nil {
		child.Wait()
		t.Fatal(err)
	}
	child.Wait() // intentional abrupt exit, not a graceful unlock
	lock, err := acquireDataLock(dir)
	if err != nil {
		t.Fatal("process crash left a stale data lock")
	}
	lock.release()
}

func TestTrustProxyIsExplicitAndValidated(t *testing.T) {
	dir := t.TempDir()
	base := map[string]string{"SALCARA_HUB_DATA_DIR": dir, "SALCARA_HUB_ADMIN_ACCOUNT_FILE": filepath.Join(dir, defaultAccountFile)}
	get := func(extra map[string]string) func(string) string {
		return func(k string) string {
			if v, ok := extra[k]; ok {
				return v
			}
			return base[k]
		}
	}
	if c, err := ConfigFromEnv(get(nil)); err != nil || c.TrustProxy {
		t.Fatalf("default must not trust forwarding headers: %+v %v", c, err)
	}
	if c, err := ConfigFromEnv(get(map[string]string{"SALCARA_HUB_TRUST_PROXY": "true"})); err != nil || !c.TrustProxy {
		t.Fatalf("explicit trust: %+v %v", c, err)
	}
	if _, err := ConfigFromEnv(get(map[string]string{"SALCARA_HUB_TRUST_PROXY": "maybe"})); err == nil {
		t.Fatal("invalid value accepted")
	}
}
