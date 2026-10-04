// Package hub authenticates independently enrolled desktop devices and paired
// phones and relays commands, replies and timeline events. Legacy relay-key
// authentication remains optional; device pairing never touches model traffic.
package hub

import (
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Version is reported by GET /v1/ping.
const Version = "1.5.0"

// Protocol limits.
const (
	accountRingSize    = 2000 // events kept per account for /app/stream replay
	sessionRingSize    = 500  // events kept per session for /app/events
	maxSessions        = 300  // session buffers per account (least recently updated evicted)
	maxDevices         = 50   // devices per account
	maxAppStreams      = 32   // concurrent /app/stream connections per account
	commandQueueSize   = 64   // buffered commands per bridge stream
	appQueueSize       = 512  // buffered messages per app stream before it is dropped
	maxEventsBody      = 2 << 20
	maxDefaultBody     = 256 << 10
	maxDeviceBody      = 64 << 10
	maxDeviceAccounts  = 10000
	maxPendingCommands = 4096
	maxIDLen           = 128
	sseWriteTimeout    = 30 * time.Second
	defaultPrefix      = "/salcara-hub"
	defaultCmdTimeout  = 45 * time.Second
)

// Config configures a Hub. Zero values are replaced by defaults in New.
type Config struct {
	// ResourceMode is optional; an empty value preserves legacy plugin count
	// defaults. Standalone explicitly chooses economy or a saved mode.
	ResourceMode string
	// DisableLegacyAPIKey makes the standalone device-pairing service independent
	// of Sub2API and rejects bearer-only model keys before any upstream request.
	DisableLegacyAPIKey bool
	// Sub2APIURL is the base URL of the relay, e.g. http://127.0.0.1:8080.
	Sub2APIURL      string
	AllowedGroupIDs []int64
	// DataDir holds devices.json. Empty disables persistence.
	DataDir string
	// Prefix is the public path prefix (default /salcara-hub). Un-prefixed
	// /v1/... paths are always accepted as well.
	Prefix string
	// TrustProxy makes the hub use the right-most X-Forwarded-For entry as
	// the client IP (for the failed-auth rate limit and logs).
	TrustProxy bool
	// DownloadsDir holds the installers served at {prefix}/download/<file> without auth. Empty disables
	// the endpoint.
	DownloadsDir string
	// PublicURL, when set (e.g. https://relay.example.com/salcara-hub), is used for the install-script
	// templates instead of guessing from Host / X-Forwarded-* headers.
	PublicURL string

	// CleanupUnpairedDays / CleanupPairedDays remove offline, unbanned computers
	// not seen for that many days (0 = never). Standalone loads saved settings.
	CleanupUnpairedDays int
	CleanupPairedDays   int

	CommandTimeout time.Duration // how long /app/commands waits for /bridge/reply (45s)
	AuthTimeout    time.Duration // sub2api /v1/usage request timeout (5s)
	ValidTTL       time.Duration // cache for valid keys (10m)
	InvalidTTL     time.Duration // cache for invalid keys (1m)
	PingInterval   time.Duration // SSE keep-alive comment interval (20s)
	AuthFailLimit  int           // failed auth attempts per IP per AuthFailWindow (20)
	AuthFailWindow time.Duration // (1m)

	// FCMCredentials is a Firebase service-account key (JSON). Empty disables push notifications.
	FCMCredentials []byte

	HTTPClient *http.Client
	Logger     *slog.Logger
}

func (c *Config) setDefaults() {
	if !validCleanupDays(c.CleanupUnpairedDays) {
		c.CleanupUnpairedDays = 0
	}
	if !validCleanupDays(c.CleanupPairedDays) {
		c.CleanupPairedDays = 0
	}
	c.Sub2APIURL = strings.TrimRight(c.Sub2APIURL, "/")
	c.PublicURL = strings.TrimRight(strings.TrimSpace(c.PublicURL), "/")
	if c.Prefix == "" {
		c.Prefix = defaultPrefix
	}
	c.Prefix = "/" + strings.Trim(c.Prefix, "/")
	if c.Prefix == "/" {
		c.Prefix = ""
	}
	if c.CommandTimeout <= 0 {
		c.CommandTimeout = defaultCmdTimeout
	}
	if c.AuthTimeout <= 0 {
		c.AuthTimeout = 5 * time.Second
	}
	if c.ValidTTL <= 0 {
		c.ValidTTL = 10 * time.Minute
	}
	if c.InvalidTTL <= 0 {
		c.InvalidTTL = time.Minute
	}
	if c.PingInterval <= 0 {
		c.PingInterval = 20 * time.Second
	}
	if c.AuthFailLimit <= 0 {
		c.AuthFailLimit = 20
	}
	if c.AuthFailWindow <= 0 {
		c.AuthFailWindow = time.Minute
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{}
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}
