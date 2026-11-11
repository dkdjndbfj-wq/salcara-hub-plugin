// Package sourceissuer authenticates station credentials and issues short-lived,
// command-bound and opt-in device-enrollment grants. Hub never receives API keys.
package sourceissuer

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"

	"salcara/hub/internal/enrollment"
	"salcara/hub/internal/hub"
)

var errConfig = errors.New("invalid source issuer configuration")

// Group grants authority from the station's authenticated group membership.
// SourceType is chosen by the station operator, never by the caller's label.
type Group struct {
	ID                    int64    `json:"id"`
	SourceType            string   `json:"sourceType"`
	Audiences             []string `json:"audiences"`
	Agents                []string `json:"agents"`
	Models                []string `json:"models,omitempty"`
	AllowDeviceEnrollment bool     `json:"allowDeviceEnrollment,omitempty"`
}

// Route binds an exact public API base to its authoritative account verifier.
// VerifierURL must implement the existing Sub2API Salcara key-group endpoint.
type Route struct {
	APIBase               string  `json:"apiBase"`
	VerifierURL           string  `json:"verifierUrl"`
	EnrollmentVerifierURL string  `json:"enrollmentVerifierUrl,omitempty"`
	Groups                []Group `json:"groups"`
}

// EnrollmentHub pins an operator-approved Hub identity, never a request's key.
type EnrollmentHub struct {
	Audience          string `json:"audience"`
	HubInstanceID     string `json:"hubInstanceId"`
	IdentityPublicKey string `json:"identityPublicKey"`
}

type Config struct {
	ID               string          `json:"id"`
	AuthorizationURL string          `json:"authorizationUrl"`
	EnrollmentURL    string          `json:"enrollmentUrl,omitempty"`
	EnrollmentHubs   []EnrollmentHub `json:"enrollmentHubs,omitempty"`
	Routes           []Route         `json:"routes"`
}

// exact rejects duplicate, unknown, case-confusable, null and trailing JSON.
func exact(raw []byte, names []string, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return errConfig
	}
	allowed, seen := map[string]bool{}, map[string]bool{}
	for _, name := range names {
		allowed[name] = true
	}
	for d.More() {
		token, err = d.Token()
		name, ok := token.(string)
		if err != nil || !ok || !allowed[name] || seen[name] {
			return errConfig
		}
		seen[name] = true
		var value json.RawMessage
		if d.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errConfig
		}
	}
	if token, err = d.Token(); err != nil || token != json.Delim('}') || d.Decode(new(any)) != io.EOF {
		return errConfig
	}
	if json.Unmarshal(raw, target) != nil {
		return errConfig
	}
	return nil
}

func ParseConfig(raw []byte) (Config, error) {
	var cfg Config
	var envelope struct {
		Routes         []json.RawMessage `json:"routes"`
		EnrollmentHubs []json.RawMessage `json:"enrollmentHubs"`
	}
	if len(raw) > 64<<10 || exact(raw, []string{"id", "authorizationUrl", "enrollmentUrl", "enrollmentHubs", "routes"}, &cfg) != nil || json.Unmarshal(raw, &envelope) != nil {
		return cfg, errConfig
	}
	for _, trust := range envelope.EnrollmentHubs {
		if exact(trust, []string{"audience", "hubInstanceId", "identityPublicKey"}, new(EnrollmentHub)) != nil {
			return Config{}, errConfig
		}
	}
	for _, route := range envelope.Routes {
		var nested struct {
			Groups []json.RawMessage `json:"groups"`
		}
		if exact(route, []string{"apiBase", "verifierUrl", "enrollmentVerifierUrl", "groups"}, &nested) != nil {
			return Config{}, errConfig
		}
		for _, group := range nested.Groups {
			if exact(group, []string{"id", "sourceType", "audiences", "agents", "models", "allowDeviceEnrollment"}, new(Group)) != nil {
				return Config{}, errConfig
			}
		}
	}
	if cfg.validate() != nil {
		return Config{}, errConfig
	}
	return cfg, nil
}

func identifier(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, ch := range s {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.') {
			return false
		}
	}
	return true
}

func unique(values []string, max int, validate func(string) bool) bool {
	if len(values) == 0 || len(values) > max {
		return false
	}
	seen := map[string]bool{}
	for _, v := range values {
		if seen[v] || !validate(v) {
			return false
		}
		seen[v] = true
	}
	return true
}

func modelName(v string) bool {
	return v != "" && len(v) <= 256 && strings.TrimSpace(v) == v && !strings.ContainsAny(v, "\r\n\x00")
}

func endpoint(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.RawPath != "" || len(raw) > 4096 || strings.ContainsAny(raw, "%\\\r\n\x00") || strings.Contains(u.Path, "//") || u.String() != raw || strings.ToLower(u.Host) != u.Host || strings.HasSuffix(u.Hostname(), ".") {
		return nil, false
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "." || part == ".." {
			return nil, false
		}
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return nil, false
		}
	}
	if u.Scheme == "https" && u.Port() != "443" {
		return u, true
	}
	ip := net.ParseIP(u.Hostname())
	return u, u.Scheme == "http" && u.Port() != "80" && (u.Hostname() == "localhost" || ip != nil && ip.IsLoopback())
}

func (c Config) validate() error {
	if !identifier(c.ID) || c.ID == "station-official" || len(c.Routes) == 0 || len(c.Routes) > 32 {
		return errConfig
	}
	public, ok := endpoint(c.AuthorizationURL)
	if !ok || public.Scheme != "https" || public.Path == "" || public.Path == "/" {
		return errConfig
	}
	if len(c.EnrollmentHubs) > 32 {
		return errConfig
	}
	if c.EnrollmentURL != "" || len(c.EnrollmentHubs) > 0 {
		enrollmentEndpoint, valid := endpoint(c.EnrollmentURL)
		if !valid || enrollmentEndpoint.Scheme != "https" || enrollmentEndpoint.Host != public.Host || enrollmentEndpoint.Path == "" || enrollmentEndpoint.Path == "/" || enrollmentEndpoint.Path == public.Path || len(c.EnrollmentHubs) == 0 {
			return errConfig
		}
		seenHubs := map[string]bool{}
		instanceKeys := map[string]string{}
		for _, trust := range c.EnrollmentHubs {
			key, err := base64.StdEncoding.Strict().DecodeString(trust.IdentityPublicKey)
			pair := trust.Audience + "\x00" + trust.HubInstanceID
			if !identifier(trust.Audience) || !enrollment.Hex(trust.HubInstanceID, 16) || err != nil || len(key) != ed25519.PublicKeySize || seenHubs[pair] || instanceKeys[trust.HubInstanceID] != "" && instanceKeys[trust.HubInstanceID] != trust.IdentityPublicKey {
				return errConfig
			}
			seenHubs[pair] = true
			instanceKeys[trust.HubInstanceID] = trust.IdentityPublicKey
		}
	}
	seen := map[string]bool{}
	for _, route := range c.Routes {
		base, valid := endpoint(route.APIBase)
		verifier, verifierOK := endpoint(route.VerifierURL)
		if !valid || base.Scheme != "https" || base.Host != public.Host || strings.HasSuffix(route.APIBase, "/") || strings.HasSuffix(strings.ToLower(base.Path), "/v1") || seen[route.APIBase] || !verifierOK || !strings.HasSuffix(verifier.Path, "/v1/salcara-hub/key") || len(route.Groups) == 0 || len(route.Groups) > 128 {
			return errConfig
		}
		// The identity endpoint belongs to this station, either on its public
		// origin or a literal loopback backend. Never delegate the key to an
		// unrelated HTTPS host, including one chosen through a config typo.
		loopback := net.ParseIP(verifier.Hostname())
		if verifier.Host != public.Host && verifier.Hostname() != "localhost" && (loopback == nil || !loopback.IsLoopback()) {
			return errConfig
		}
		if route.EnrollmentVerifierURL != "" {
			identity, valid := endpoint(route.EnrollmentVerifierURL)
			if !valid || !strings.HasSuffix(identity.Path, "/v1/salcara-hub/enrollment-identity") {
				return errConfig
			}
			loopback := net.ParseIP(identity.Hostname())
			if identity.Host != public.Host && identity.Hostname() != "localhost" && (loopback == nil || !loopback.IsLoopback()) {
				return errConfig
			}
		}
		seen[route.APIBase] = true
		groups := map[int64]bool{}
		for _, group := range route.Groups {
			if group.ID <= 0 || groups[group.ID] || (group.SourceType != "site_api" && group.SourceType != "verified_proxy") || !unique(group.Audiences, 32, identifier) || !unique(group.Agents, 32, identifier) || len(group.Models) > 0 && !unique(group.Models, 256, modelName) {
				return errConfig
			}
			if group.AllowDeviceEnrollment {
				covered := false
				for _, trust := range c.EnrollmentHubs {
					covered = covered || contains(group.Audiences, trust.Audience)
				}
				if c.EnrollmentURL == "" || route.EnrollmentVerifierURL == "" || !covered {
					return errConfig
				}
			}
			groups[group.ID] = true
		}
	}
	return nil
}

func (c Config) public(key ed25519.PublicKey) hub.SourceIssuer {
	public, _ := url.Parse(c.AuthorizationURL)
	out := hub.SourceIssuer{ID: c.ID, PublicKey: base64.RawURLEncoding.EncodeToString(key), AuthorizationURL: c.AuthorizationURL, Origins: []string{public.Scheme + "://" + public.Host}}
	if c.enrollmentEnabled() {
		out.DeviceEnrollment, out.EnrollmentURL = true, c.EnrollmentURL
	}
	for _, route := range c.Routes {
		out.APIBases = append(out.APIBases, route.APIBase)
		for _, group := range route.Groups {
			if !contains(out.SourceTypes, group.SourceType) {
				out.SourceTypes = append(out.SourceTypes, group.SourceType)
			}
			for _, agent := range group.Agents {
				if !contains(out.AgentIDs, agent) {
					out.AgentIDs = append(out.AgentIDs, agent)
				}
			}
		}
	}
	return out
}

func (c Config) enrollmentEnabled() bool {
	for _, route := range c.Routes {
		for _, group := range route.Groups {
			if group.AllowDeviceEnrollment {
				return c.EnrollmentURL != "" && len(c.EnrollmentHubs) > 0 && route.EnrollmentVerifierURL != ""
			}
		}
	}
	return false
}

func contains(values []string, v string) bool {
	for _, value := range values {
		if value == v {
			return true
		}
	}
	return false
}
