// Package launcher supervises exactly one unprivileged Hub process. It can
// update a signed Hub executable, never a Docker image or the host daemon.
package launcher

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	ControlSocket        = "/tmp/salcara-hub-control.sock"
	ControlTokenFile     = "/tmp/salcara-hub-control-token"
	PublisherID          = "salcara-local-20260930"
	Product              = "salcara-hub-standalone"
	publicKeyBase64      = "+JTC35ZD2PEJ5TO5ZOQtQTm5BoKDKMH59D6beZbn+gc="
	defaultFeed          = "https://raw.githubusercontent.com/dkdjndbfj-wq/salcara-hub-plugin/main/updates/standalone-account.json"
	defaultCheckInterval = 6 * time.Hour
	maxFeedBytes         = 64 << 10
	maxBinaryBytes       = 64 << 20
)

type Config struct {
	DataDir          string
	Listen           string
	FeedURL          string
	Disabled         bool
	CheckInterval    time.Duration
	BootstrapPath    string
	BootstrapVersion string
}

func ConfigFromEnv(getenv func(string) string, bootstrapVersion string) (Config, error) {
	c := Config{DataDir: getenv("SALCARA_HUB_DATA_DIR"), Listen: getenv("SALCARA_HUB_LISTEN"), FeedURL: getenv("SALCARA_HUB_UPDATE_FEED_URL"), BootstrapPath: "/salcara-hub", BootstrapVersion: bootstrapVersion}
	if c.DataDir == "" {
		c.DataDir = "/data"
	}
	if c.Listen == "" {
		c.Listen = ":8787"
	}
	if c.FeedURL == "" {
		c.FeedURL = defaultFeed
	}
	var err error
	c.CheckInterval, err = updateCheckInterval(getenv("SALCARA_HUB_UPDATE_CHECK_INTERVAL"))
	if err != nil {
		return Config{}, err
	}
	switch strings.ToLower(getenv("SALCARA_HUB_DISABLE_UPDATES")) {
	case "", "0", "false":
	case "1", "true":
		c.Disabled = true
	default:
		return Config{}, errors.New("SALCARA_HUB_DISABLE_UPDATES must be true or false")
	}
	if !filepath.IsAbs(c.DataDir) || filepath.Clean(c.DataDir) == filepath.VolumeName(c.DataDir)+string(os.PathSeparator) || !filepath.IsAbs(c.BootstrapPath) {
		return Config{}, errors.New("launcher paths must be absolute non-root paths")
	}
	if _, err := parseVersion(c.BootstrapVersion); err != nil {
		return Config{}, errors.New("invalid compiled Hub version")
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return Config{}, errors.New("invalid Hub listen address")
	}
	if _, err := validateURL(c.FeedURL); err != nil {
		return Config{}, err
	}
	return c, nil
}

func updateCheckInterval(value string) (time.Duration, error) {
	if value == "" {
		return defaultCheckInterval, nil
	}
	interval, err := time.ParseDuration(value)
	if err != nil || interval < 5*time.Minute || interval > 7*24*time.Hour {
		return 0, errors.New("SALCARA_HUB_UPDATE_CHECK_INTERVAL must be a duration between 5m and 168h")
	}
	return interval, nil
}

func fixedPublicKey() ed25519.PublicKey {
	data, err := base64.StdEncoding.DecodeString(publicKeyBase64)
	if err != nil || len(data) != ed25519.PublicKeySize {
		panic("invalid built-in update publisher")
	}
	return ed25519.PublicKey(data)
}
