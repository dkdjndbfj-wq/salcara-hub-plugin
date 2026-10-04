// Package standalone exposes the pairing Hub independently of any relay host.
// No model API key, relay login cookie, or host-plugin RPC is accepted here.
package standalone

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"salcara/hubplugin/internal/hub"
)

const Prefix = "/salcara-hub"

// Version is set by the standalone release build via -ldflags -X. The
// launcher verifies this exact value in /healthz before completing an update.
var Version = "0.5.0-dev"

const Product = "salcara-hub-standalone"

const defaultListen = ":8787"
const defaultAccountFile = "admin-account.json"

type Config struct {
	DataDir          string
	AdminAccountFile string
	Listen           string
	PublicURL        string
	CommandTimeout   time.Duration
	PingInterval     time.Duration
	ControlSocket    string
	ControlTokenFile string
	ResourceMode     string
	// Initial inactive-computer cleanup in days (0 = off) until the admin page
	// saves its own setting. Defaults: unpaired 7 days, paired off.
	CleanupUnpairedDays int
	CleanupPairedDays   int
	// TrustProxy uses the address the reverse proxy puts in X-Forwarded-For.
	// Only enable it when the port is reachable by that proxy alone (the
	// Compose file publishes it on 127.0.0.1); otherwise every client shares
	// the proxy's address and one attacker's failures lock out everyone.
	TrustProxy bool
	// FCMCredentialsFile is a Firebase service-account key (JSON) for push
	// notifications to paired phones. Empty = no push (phones fall back to
	// their own background connection).
	FCMCredentialsFile string
}

// ConfigFromEnv intentionally has no Sub2API URL, API key or legacy-auth
// switch. An independent deployment never calls the model relay.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	c := Config{DataDir: getenv("SALCARA_HUB_DATA_DIR"), AdminAccountFile: getenv("SALCARA_HUB_ADMIN_ACCOUNT_FILE"), Listen: getenv("SALCARA_HUB_LISTEN"), PublicURL: getenv("SALCARA_HUB_PUBLIC_URL")}
	c.ControlSocket, c.ControlTokenFile = getenv("SALCARA_HUB_CONTROL_SOCKET"), getenv("SALCARA_HUB_CONTROL_TOKEN_FILE")
	c.ResourceMode = getenv("SALCARA_HUB_RESOURCE_MODE")
	c.FCMCredentialsFile = strings.TrimSpace(getenv("SALCARA_HUB_FCM_CREDENTIALS_FILE"))
	switch strings.ToLower(strings.TrimSpace(getenv("SALCARA_HUB_TRUST_PROXY"))) {
	case "", "0", "false", "no":
	case "1", "true", "yes":
		c.TrustProxy = true
	default:
		return Config{}, errors.New("SALCARA_HUB_TRUST_PROXY must be true or false")
	}
	if c.AdminAccountFile == "" {
		c.AdminAccountFile = filepath.Join(c.DataDir, defaultAccountFile)
	}
	if c.Listen == "" {
		c.Listen = defaultListen
	}
	var err error
	if c.CleanupUnpairedDays, err = envDays(getenv("SALCARA_HUB_CLEANUP_UNPAIRED_DAYS"), 7); err != nil {
		return Config{}, fmt.Errorf("SALCARA_HUB_CLEANUP_UNPAIRED_DAYS: %w", err)
	}
	if c.CleanupPairedDays, err = envDays(getenv("SALCARA_HUB_CLEANUP_PAIRED_DAYS"), 0); err != nil {
		return Config{}, fmt.Errorf("SALCARA_HUB_CLEANUP_PAIRED_DAYS: %w", err)
	}
	c.CommandTimeout, err = envDuration(getenv("SALCARA_HUB_COMMAND_TIMEOUT"), 45*time.Second, time.Second, 120*time.Second)
	if err != nil {
		return Config{}, fmt.Errorf("SALCARA_HUB_COMMAND_TIMEOUT: %w", err)
	}
	c.PingInterval, err = envDuration(getenv("SALCARA_HUB_PING_INTERVAL"), 20*time.Second, 5*time.Second, time.Minute)
	if err != nil {
		return Config{}, fmt.Errorf("SALCARA_HUB_PING_INTERVAL: %w", err)
	}
	return c, c.Validate()
}

func envDays(raw string, fallback int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > hub.MaxDeviceCleanupDays {
		return 0, fmt.Errorf("must be whole days between 0 (off) and %d", hub.MaxDeviceCleanupDays)
	}
	return n, nil
}

func envDuration(raw string, fallback, minimum, maximum time.Duration) (time.Duration, error) {
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < minimum || d > maximum {
		return 0, fmt.Errorf("must be a duration between %s and %s", minimum, maximum)
	}
	return d, nil
}

func (c Config) Validate() error {
	if c.FCMCredentialsFile != "" && !filepath.IsAbs(c.FCMCredentialsFile) {
		return errors.New("SALCARA_HUB_FCM_CREDENTIALS_FILE must be an absolute path")
	}
	if c.ResourceMode != "" && c.ResourceMode != "economy" && c.ResourceMode != "balanced" && c.ResourceMode != "performance" {
		return errors.New("SALCARA_HUB_RESOURCE_MODE must be economy, balanced, or performance")
	}
	if c.DataDir == "" || !filepath.IsAbs(c.DataDir) || filepath.Clean(c.DataDir) == filepath.VolumeName(c.DataDir)+string(os.PathSeparator) {
		return errors.New("SALCARA_HUB_DATA_DIR must be an absolute persistent non-root directory")
	}
	if c.AdminAccountFile == "" || !filepath.IsAbs(c.AdminAccountFile) || filepath.Clean(c.AdminAccountFile) != c.AdminAccountFile || filepath.Dir(c.AdminAccountFile) != filepath.Clean(c.DataDir) || filepath.Base(c.AdminAccountFile) == initialLoginFile || filepath.Base(c.AdminAccountFile) == ".salcara-hub.lock" {
		return errors.New("SALCARA_HUB_ADMIN_ACCOUNT_FILE must be a clean absolute file path directly inside SALCARA_HUB_DATA_DIR")
	}
	if (c.ControlSocket == "") != (c.ControlTokenFile == "") {
		return errors.New("Hub update control socket and token file must both be configured, or both be absent")
	}
	if c.ControlSocket != "" && (!filepath.IsAbs(c.ControlSocket) || !filepath.IsAbs(c.ControlTokenFile) || filepath.Clean(c.ControlSocket) != c.ControlSocket || filepath.Clean(c.ControlTokenFile) != c.ControlTokenFile || c.ControlSocket == c.ControlTokenFile || c.ControlTokenFile == c.AdminAccountFile || c.ControlTokenFile == filepath.Join(c.DataDir, initialLoginFile)) {
		return errors.New("Hub update control paths must be distinct absolute clean paths, with a separate control credential")
	}
	if _, err := healthURL(c.Listen); err != nil {
		return err
	}
	if c.PublicURL != "" {
		u, err := url.Parse(c.PublicURL)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || strings.TrimSuffix(u.Path, "/") != Prefix || !allowedPublicScheme(u) {
			return errors.New("SALCARA_HUB_PUBLIC_URL must be https://your-relay/salcara-hub (HTTP allowed only for loopback testing)")
		}
		if _, err = origin(u); err != nil {
			return errors.New("SALCARA_HUB_PUBLIC_URL has an invalid origin")
		}
	}
	if c.CommandTimeout < time.Second || c.CommandTimeout > 120*time.Second || c.PingInterval < 5*time.Second || c.PingInterval > time.Minute {
		return errors.New("invalid Hub timeout configuration")
	}
	return nil
}

func allowedPublicScheme(u *url.URL) bool {
	if u.Scheme == "https" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return u.Scheme == "http" && (strings.EqualFold(u.Hostname(), "localhost") || (ip != nil && ip.IsLoopback()))
}

func healthURL(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", errors.New("SALCARA_HUB_LISTEN must be host:port")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return "", errors.New("SALCARA_HUB_LISTEN port must be between 1 and 65535")
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	} else if host == "::" {
		host = "::1"
	} else if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil {
			return "", errors.New("SALCARA_HUB_LISTEN must use an IP address or localhost")
		}
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}

// initControlToken is solely a test helper for the private launcher control
// credential. This secret is never a browser administrator credential.
func initControlToken(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("private launcher control token path must be absolute")
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return errors.New("cannot generate private launcher control token")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("cannot create new private launcher control token file: %w", err)
	}
	_, writeErr := io.WriteString(f, hex.EncodeToString(random[:])+"\n")
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		// Keep even an incomplete file rather than silently replacing a secret.
		return errors.New("private launcher control token file could not be saved; inspect it before retrying")
	}
	return nil
}

func readControlToken(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4098 || info.Size() < 32 {
		return nil, errors.New("private launcher control token must be stored in a small regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("private launcher control token file must not be readable or writable by group or other users (use mode 0600 or 0400)")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("private launcher control token file is not readable")
	}
	defer f.Close()
	opened, statErr := f.Stat()
	if statErr != nil || !os.SameFile(info, opened) {
		return nil, errors.New("private launcher control token file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, 4099))
	if err != nil || len(data) > 4098 {
		return nil, errors.New("invalid private launcher control token file")
	}
	token := strings.TrimSpace(string(data))
	decoded, decodeErr := hex.DecodeString(token)
	if len(token) != 64 || decodeErr != nil || len(decoded) != 32 {
		return nil, errors.New("private launcher control token must be an independent 256-bit hexadecimal secret")
	}
	return []byte(token), nil
}

// readFCMCredentials loads the Firebase service-account key; it is a secret,
// so a file readable by other users is refused (except on Windows).
func readFCMCredentials(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return nil, errors.New("SALCARA_HUB_FCM_CREDENTIALS_FILE must be a regular file (a Firebase service account key)")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("FCM service account file must not be readable by group or other users (use mode 0600 or 0400)")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("FCM service account file is not readable")
	}
	return data, nil
}
