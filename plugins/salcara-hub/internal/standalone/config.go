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
)

const Prefix = "/salcara-hub"

// Version is set by the standalone release build via -ldflags -X. The
// launcher verifies this exact value in /healthz before completing an update.
var Version = "0.4.0-dev"

const defaultListen = ":8787"
const defaultTokenFile = "/data/admin-token"

type Config struct {
	DataDir          string
	AdminTokenFile   string
	Listen           string
	PublicURL        string
	CommandTimeout   time.Duration
	PingInterval     time.Duration
	ControlSocket    string
	ControlTokenFile string
	ResourceMode     string
}

// ConfigFromEnv intentionally has no Sub2API URL, API key, proxy trust, or
// legacy-auth switch. An independent deployment never calls the model relay.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	c := Config{DataDir: getenv("SALCARA_HUB_DATA_DIR"), AdminTokenFile: getenv("SALCARA_HUB_ADMIN_TOKEN_FILE"), Listen: getenv("SALCARA_HUB_LISTEN"), PublicURL: getenv("SALCARA_HUB_PUBLIC_URL")}
	c.ControlSocket, c.ControlTokenFile = getenv("SALCARA_HUB_CONTROL_SOCKET"), getenv("SALCARA_HUB_CONTROL_TOKEN_FILE")
	c.ResourceMode = getenv("SALCARA_HUB_RESOURCE_MODE")
	if c.AdminTokenFile == "" {
		c.AdminTokenFile = defaultTokenFile
	}
	if c.Listen == "" {
		c.Listen = defaultListen
	}
	var err error
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
	if c.ResourceMode != "" && c.ResourceMode != "economy" && c.ResourceMode != "balanced" && c.ResourceMode != "performance" {
		return errors.New("SALCARA_HUB_RESOURCE_MODE must be economy, balanced, or performance")
	}
	if c.DataDir == "" || !filepath.IsAbs(c.DataDir) || filepath.Clean(c.DataDir) == filepath.VolumeName(c.DataDir)+string(os.PathSeparator) {
		return errors.New("SALCARA_HUB_DATA_DIR must be an absolute persistent non-root directory")
	}
	if c.AdminTokenFile == "" || !filepath.IsAbs(c.AdminTokenFile) {
		return errors.New("SALCARA_HUB_ADMIN_TOKEN_FILE must be an absolute path")
	}
	if (c.ControlSocket == "") != (c.ControlTokenFile == "") {
		return errors.New("Hub update control socket and token file must both be configured, or both be absent")
	}
	if c.ControlSocket != "" && (!filepath.IsAbs(c.ControlSocket) || !filepath.IsAbs(c.ControlTokenFile) || filepath.Clean(c.ControlSocket) != c.ControlSocket || filepath.Clean(c.ControlTokenFile) != c.ControlTokenFile || c.ControlSocket == c.ControlTokenFile || c.ControlTokenFile == c.AdminTokenFile) {
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

// InitAdminToken only creates an absent secret. It never outputs its value or
// replaces an existing token; existing pairing state is not read or modified.
func InitAdminToken(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("admin token path must be absolute")
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return errors.New("cannot generate admin token")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("cannot create new admin token file: %w", err)
	}
	_, writeErr := io.WriteString(f, hex.EncodeToString(random[:])+"\n")
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		// Keep even an incomplete file rather than silently replacing a secret.
		return errors.New("admin token file could not be saved; inspect the file before retrying initialization")
	}
	return nil
}

func readAdminToken(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4098 || info.Size() < 32 {
		return nil, errors.New("admin token file must be a regular file containing a random token of at least 32 bytes; run -init first")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("admin token file must not be readable or writable by group or other users (use mode 0600 or 0400)")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("admin token file is not readable")
	}
	defer f.Close()
	opened, statErr := f.Stat()
	if statErr != nil || !os.SameFile(info, opened) {
		return nil, errors.New("admin token file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, 4099))
	if err != nil || len(data) > 4098 {
		return nil, errors.New("invalid admin token file")
	}
	token := strings.TrimSpace(string(data))
	if len(token) < 32 || len(token) > 4096 || strings.HasPrefix(strings.ToLower(token), "sk-") || strings.Contains(strings.ToLower(token), "bearer") {
		return nil, errors.New("admin token must be an independent random secret, not a model API key")
	}
	for _, ch := range token {
		if ch < 0x21 || ch > 0x7e {
			return nil, errors.New("admin token must contain printable ASCII without spaces")
		}
	}
	return []byte(token), nil
}
