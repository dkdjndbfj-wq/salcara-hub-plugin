package hub

import (
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strings"
)

const stationOfficialIssuer = "station-official"
const officialAccountOrigin = "https://chatgpt.com"
const officialAccountAPIBase = officialAccountOrigin + "/backend-api/codex"

// StationPolicy belongs to the Hub operator. API keys and issuer grants cannot
// enlarge it. Empty lists disable their category; v2 keeps ordinary API and
// reverse-proxy scopes independent.
type StationPolicy struct {
	Version               int      `json:"version"`
	APIBases              []string `json:"apiBases"`
	ExternalAPIBases      []string `json:"externalApiBases,omitempty"`
	ReverseProxyAPIBases  []string `json:"reverseProxyApiBases,omitempty"`
	AllowOfficialAccounts bool     `json:"allowOfficialAccounts"`
}

func cloneStationPolicy(p *StationPolicy) *StationPolicy {
	if p == nil {
		return nil
	}
	out := *p
	out.APIBases = append([]string{}, p.APIBases...)
	out.ExternalAPIBases = append([]string(nil), p.ExternalAPIBases...)
	out.ReverseProxyAPIBases = append([]string(nil), p.ReverseProxyAPIBases...)
	return &out
}

// ResolveRemoteAccessPolicy resolves deployment defaults before persistence or
// publication. Only the trusted configured PublicURL supplies a default scope.
func ResolveRemoteAccessPolicy(p RemoteAccessPolicy, publicURL, prefix string) (RemoteAccessPolicy, error) {
	p = cloneRemoteAccessPolicy(p)
	if p.Station == nil {
		p.Station = defaultStationPolicy(publicURL, prefix)
	}
	err := p.normalize()
	return p, err
}

// ParseStationPolicy rejects ambiguous field spellings, duplicate keys and null
// values before normalizing complete API bases.
func ParseStationPolicy(raw []byte) (*StationPolicy, error) {
	var p StationPolicy
	if len(raw) > MaxStationPolicyJSONBytes || decodeExactObject(raw, []string{"version", "apiBases", "externalApiBases", "reverseProxyApiBases", "allowOfficialAccounts"}, &p) != nil {
		return nil, errors.New("invalid station policy JSON")
	}
	if err := p.normalize(); err != nil {
		return nil, err
	}
	return cloneStationPolicy(&p), nil
}

// NormalizeStationAPIBase preserves complete path prefixes. It accepts only
// unambiguous root or /v1 forms; URL aliases never grant a sibling path.
func NormalizeStationAPIBase(value string) (string, error) {
	value = strings.TrimSpace(value)
	u, err := url.Parse(value)
	if err != nil || len(value) > 4096 || strings.ContainsAny(value, "%#\\\r\n\x00 \t") || u.Host == "" || strings.HasSuffix(u.Host, ":") || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.RawPath != "" || strings.Contains(u.Path, "//") {
		return "", errors.New("invalid station API base")
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return "", errors.New("invalid station API base")
		}
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	if u.Scheme == "https" && port == "443" || u.Scheme == "http" && port == "80" {
		port = ""
	}
	u.Host = host
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	if strings.HasSuffix(strings.ToLower(u.Path), "/v1") {
		u.Path = u.Path[:len(u.Path)-3]
	}
	base := u.String()
	if _, valid := canonicalAPIBase(base); !valid {
		return "", errors.New("invalid station API base")
	}
	return base, nil
}

func (p *StationPolicy) normalize() error {
	if !p.supportedVersion() || len(p.APIBases) > 32 || len(p.ExternalAPIBases) > 32 || len(p.ReverseProxyAPIBases) > 32 || p.Version == 1 && (p.ExternalAPIBases != nil || p.ReverseProxyAPIBases != nil) {
		return errors.New("invalid station policy")
	}
	seen := map[string]bool{}
	all := map[string]bool{}
	for listIndex, list := range [][]string{p.APIBases, p.ExternalAPIBases, p.ReverseProxyAPIBases} {
		// A provider can expose both API-key and verified-proxy credentials,
		// but the two ordinary API scopes must not overlap.
		if listIndex == 2 {
			seen = map[string]bool{}
		}
		for i, base := range list {
			normalized, err := NormalizeStationAPIBase(base)
			if err != nil || seen[normalized] {
				return errors.New("invalid or duplicate station API base")
			}
			seen[normalized] = true
			all[normalized] = true
			if len(all) > 32 {
				return errors.New("too many station API bases")
			}
			list[i] = normalized
		}
	}
	return nil
}

func (p *StationPolicy) supportedVersion() bool {
	return p != nil && (p.Version == 1 || p.Version == 2)
}

// HasSources includes the v2 independent source categories. An empty list
// disables that category; no hostname wildcard or default-open fallback exists.
func (p *StationPolicy) HasSources() bool {
	return p.supportedVersion() && (len(p.APIBases)+len(p.ExternalAPIBases)+len(p.ReverseProxyAPIBases) > 0 || p.AllowOfficialAccounts)
}

func (p *StationPolicy) requiredCapability() string {
	if p != nil && p.Version == 2 {
		return "station.guard.v2"
	}
	return "station.guard.v1"
}

// UnmarshalJSON applies the exact schema to nested administrative/deployment
// input as well as to standalone station-policy parsing.
func (p *StationPolicy) UnmarshalJSON(raw []byte) error {
	type plain StationPolicy
	var decoded plain
	if err := decodeExactObject(raw, []string{"version", "apiBases", "externalApiBases", "reverseProxyApiBases", "allowOfficialAccounts"}, &decoded); err != nil {
		return err
	}
	// Presence matters: even empty v2 fields must not be silently downgraded.
	if decoded.Version == 1 {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			return errors.New("invalid station policy")
		}
		if _, exists := fields["externalApiBases"]; exists {
			return errors.New("v2 station field in v1 policy")
		}
		if _, exists := fields["reverseProxyApiBases"]; exists {
			return errors.New("v2 station field in v1 policy")
		}
	}
	*p = StationPolicy(decoded)
	return nil
}

func defaultStationPolicy(publicURL, prefix string) *StationPolicy {
	p := &StationPolicy{Version: 1, APIBases: []string{}}
	// PublicURL is trusted deployment configuration. Never infer authority from
	// Host, forwarded headers, discovery labels or a phone-selected account.
	u, err := url.Parse(publicURL)
	if err != nil || publicURL == "" || u.RawPath != "" || prefix != "" && !strings.HasSuffix(u.Path, prefix) {
		return p
	}
	u.Path = strings.TrimSuffix(u.Path, prefix)
	if base, err := NormalizeStationAPIBase(u.String()); err == nil {
		p.APIBases = []string{base}
	}
	return p
}

func (p *StationPolicy) allowsSnapshot(s SourceSnapshot) bool {
	if !p.supportedVersion() {
		return false
	}
	if s.SourceType == "official_account_proxy" {
		return p.AllowOfficialAccounts && s.Origin == officialAccountOrigin && s.APIBase == officialAccountAPIBase
	}
	// A grant without the complete path cannot establish station membership.
	if _, ok := canonicalAPIBase(s.APIBase); !ok {
		return false
	}
	var bases []string
	switch s.SourceType {
	case "site_api":
		bases = append(append([]string(nil), p.APIBases...), p.ExternalAPIBases...)
	case "verified_proxy":
		bases = p.ReverseProxyAPIBases
		if p.Version == 1 {
			bases = p.APIBases
		}
	default:
		return false
	}
	for _, base := range bases {
		if s.APIBase == base {
			return true
		}
	}
	return false
}
