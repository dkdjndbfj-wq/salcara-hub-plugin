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
)

const (
	ControlSocket    = "/tmp/salcara-hub-control.sock"
	ControlTokenFile = "/tmp/salcara-hub-control-token"
	PublisherID      = "salcara-local-20260930"
	Product          = "salcara-hub-standalone"
	publicKeyBase64  = "+JTC35ZD2PEJ5TO5ZOQtQTm5BoKDKMH59D6beZbn+gc="
	defaultFeed      = "https://raw.githubusercontent.com/dkdjndbfj-wq/salcara-hub-plugin/main/updates/standalone.json"
	maxFeedBytes     = 64 << 10
	maxBinaryBytes   = 64 << 20
)

type Config struct {
	DataDir          string
	Listen           string
	FeedURL          string
	Disabled         bool
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

func fixedPublicKey() ed25519.PublicKey {
	data, err := base64.StdEncoding.DecodeString(publicKeyBase64)
	if err != nil || len(data) != ed25519.PublicKeySize {
		panic("invalid built-in update publisher")
	}
	return ed25519.PublicKey(data)
}
