// Package edgepool routes a stable HTTP ingress to one fixed Hub origin through
// a server-owned pool of HTTP or SOCKS proxies. An edge on the same VPS is useful
// for verification, but does not by itself improve users' Internet routes.
package edgepool

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const maxNodes = 32
const stateFile = "edge-pool.json"
const maxStateBytes = 4 << 20

// Config is deployment configuration. Origin is immutable through the API.
// Proxy credentials in the data directory must be treated as secrets.
type Config struct {
	Origin               string
	DataDir              string
	ControlToken         string
	ProbeInterval        time.Duration
	ProbeTimeout         time.Duration
	DisablePeriodicProbe bool
	// TrustedProxyCIDRs is an explicit allowlist of HTTPS frontend addresses.
	// Empty means incoming Forwarded/X-Forwarded-* values are never trusted.
	TrustedProxyCIDRs []string
}

type nodeConfig struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	ProxyURL          string `json:"proxy_url"`
	Enabled           bool   `json:"enabled"`
	Imported          bool   `json:"imported,omitempty"`
	Weight            int    `json:"weight"`
	MaxRequests       int    `json:"max_requests"`
	MaxStreams        int    `json:"max_streams"`
	Draining          bool   `json:"draining"`
	Region            string `json:"region"`
	SourceID          string `json:"source_id,omitempty"`
	SourceKey         string `json:"source_key,omitempty"`
	subscriptionIndex int
}

type savedConfig struct {
	Revision        int64            `json:"revision"`
	Mode            string           `json:"mode"`
	SelectedID      string           `json:"selected_id"`
	Nodes           []nodeConfig     `json:"nodes"`
	Limits          Limits           `json:"limits"`
	HealthPolicy    HealthPolicy     `json:"health_policy"`
	SelectionPolicy SelectionPolicy  `json:"selection_policy"`
	SourceLimits    []SourceLimit    `json:"source_limits,omitempty"`
	History         []releaseVersion `json:"release_history,omitempty"`
	Trial           *pendingRelease  `json:"pending_release,omitempty"`
	ReleaseError    string           `json:"release_error,omitempty"`
}

type Limits struct {
	MaxRequests    int `json:"max_requests"`
	MaxStreams     int `json:"max_streams"`
	QueueLimit     int `json:"queue_limit"`
	QueueTimeoutMS int `json:"queue_timeout_ms"`
}

func normalizeConfig(s *savedConfig) {
	if s.SourceLimits == nil {
		s.SourceLimits = []SourceLimit{}
	}
	if s.HealthPolicy == (HealthPolicy{}) {
		s.HealthPolicy = defaultHealthPolicy()
	}
	if s.SelectionPolicy.RegionMode == "" {
		s.SelectionPolicy = defaultSelectionPolicy()
	}
	if s.SelectionPolicy.PreferredRegions == nil {
		s.SelectionPolicy.PreferredRegions = []string{}
	}
	if s.Limits == (Limits{}) {
		s.Limits = Limits{MaxRequests: 256, MaxStreams: 128, QueueLimit: 64, QueueTimeoutMS: 2000}
	}
	for i := range s.Nodes {
		n := &s.Nodes[i]
		if n.Weight == 0 {
			n.Weight = 1
		}
		if n.MaxRequests == 0 {
			n.MaxRequests = 64
		}
		if n.MaxStreams == 0 {
			n.MaxStreams = 16
		}
	}
}

var nodeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func parseOrigin(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 4096 || strings.TrimSpace(raw) != raw || u == nil ||
		(u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil ||
		u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/" && u.Path != "/salcara-hub" && u.Path != "/salcara-hub/") || u.RawPath != "" {
		return nil, errors.New("invalid fixed Hub origin")
	}
	if strings.ContainsAny(u.Host, "\\\r\n\t") {
		return nil, errors.New("invalid fixed Hub origin")
	}
	u.Path = ""
	return u, nil
}

func parseProxy(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 8192 || strings.TrimSpace(raw) != raw || u == nil ||
		u.Hostname() == "" || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return nil, errors.New("invalid proxy URL")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, errors.New("unsupported proxy scheme")
	}
	if strings.ContainsAny(u.Host, "\\\r\n\t") {
		return nil, errors.New("invalid proxy URL")
	}
	if port := u.Port(); port != "" {
		if _, err := net.LookupPort("tcp", port); err != nil {
			return nil, errors.New("invalid proxy port")
		}
	}
	if u.User != nil {
		password, _ := u.User.Password()
		if strings.ContainsAny(u.User.Username()+password, "\r\n\x00") {
			return nil, errors.New("invalid proxy credentials")
		}
	}
	return u, nil
}

func validateSaved(s savedConfig) error {
	normalizeConfig(&s)
	if err := validatePolicies(s.HealthPolicy, s.SelectionPolicy); err != nil {
		return err
	}
	if err := validateSourceLimits(s.SourceLimits); err != nil {
		return err
	}
	if s.Revision < 1 || len(s.Nodes) > maxNodes {
		return errors.New("invalid stored pool")
	}
	if s.Mode != "manual" && s.Mode != "failover" && s.Mode != "latency" && s.Mode != "balance" {
		return errors.New("invalid pool policy")
	}
	if s.Limits.MaxRequests < 1 || s.Limits.MaxRequests > 4096 || s.Limits.MaxStreams < 1 || s.Limits.MaxStreams > 4096 || s.Limits.QueueLimit < 0 || s.Limits.QueueLimit > 4096 || s.Limits.QueueTimeoutMS < 1 || s.Limits.QueueTimeoutMS > 30000 {
		return errors.New("invalid pool limits")
	}
	seen := make(map[string]bool)
	selected := s.SelectedID == ""
	for _, n := range s.Nodes {
		if !validRegion(n.Region) {
			return errors.New("invalid node region")
		}
		if n.Weight < 1 || n.Weight > 100 || n.MaxRequests < 1 || n.MaxRequests > 4096 || n.MaxStreams < 1 || n.MaxStreams > 4096 {
			return errors.New("invalid node limits")
		}
		if !nodeID.MatchString(n.ID) || seen[n.ID] || strings.TrimSpace(n.Name) == "" || len(n.Name) > 160 || strings.ContainsAny(n.Name, "\r\n\x00") {
			return errors.New("invalid node")
		}
		if _, err := parseProxy(n.ProxyURL); err != nil {
			return err
		}
		if n.Imported {
			if _, err := subscriptionProxy(n.ProxyURL); err != nil {
				return errors.New("invalid imported proxy address")
			}
		}
		seen[n.ID] = true
		if n.ID == s.SelectedID && n.Enabled {
			selected = true
		}
	}
	if !selected {
		return errors.New("selected node must exist and be enabled")
	}
	if s.Mode == "manual" && s.SelectedID == "" {
		for _, n := range s.Nodes {
			if n.Enabled {
				return errors.New("manual policy requires an enabled selected node")
			}
		}
	}
	return nil
}

func validateOriginNodes(origin *url.URL, s savedConfig) error {
	if origin.Scheme != "https" {
		for _, node := range s.Nodes {
			if node.ProxyURL != "" {
				return errors.New("https_origin_required")
			}
		}
	}
	return nil
}

func readSaved(dir string) (savedConfig, error) {
	path := filepath.Join(dir, stateFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return savedConfig{Revision: 1, Mode: "manual", SelectedID: "direct", Nodes: []nodeConfig{{ID: "direct", Name: "直连", Enabled: true}}}, nil
	}
	if err != nil {
		return savedConfig{}, errors.New("cannot read pool configuration")
	}
	if !info.Mode().IsRegular() || !privateMode(info) || info.Size() > maxStateBytes {
		return savedConfig{}, errors.New("pool configuration must be a private regular file of bounded size")
	}
	f, err := os.Open(path)
	if err != nil {
		return savedConfig{}, errors.New("cannot read pool configuration")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return savedConfig{}, errors.New("pool configuration changed while opening")
	}
	dec := json.NewDecoder(io.LimitReader(f, maxStateBytes))
	dec.DisallowUnknownFields()
	var s savedConfig
	if dec.Decode(&s) != nil {
		return savedConfig{}, errors.New("invalid stored pool")
	}
	var trailing any
	if dec.Decode(&trailing) != io.EOF {
		return savedConfig{}, errors.New("invalid stored pool")
	}
	if err := validateSaved(s); err != nil {
		return savedConfig{}, err
	}
	if err := validateReleaseJournal(s); err != nil {
		return savedConfig{}, err
	}
	return s, nil
}

func writeSaved(dir string, s savedConfig) (string, error) {
	if info, err := os.Lstat(filepath.Join(dir, stateFile)); err == nil && (!info.Mode().IsRegular() || !privateMode(info)) {
		return "", errors.New("invalid pool configuration file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("cannot inspect pool configuration")
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil || len(b)+1 > maxStateBytes {
		return "", errors.New("cannot encode pool configuration")
	}
	f, err := os.CreateTemp(dir, ".edge-pool-*")
	if err != nil {
		return "", errors.New("cannot persist pool configuration")
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return "", errors.New("cannot persist pool configuration")
	}
	if os.Rename(name, filepath.Join(dir, stateFile)) != nil {
		return "", errors.New("cannot replace pool configuration")
	}
	// The rename already committed. Never report a rejected mutation after this
	// point; surface uncertain directory durability explicitly in API state.
	if runtime.GOOS == "windows" {
		return "platform_atomic", nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return "uncertain", nil
	}
	err = d.Sync()
	closeErr = d.Close()
	if err != nil || closeErr != nil {
		return "uncertain", nil
	}
	return "confirmed", nil
}
