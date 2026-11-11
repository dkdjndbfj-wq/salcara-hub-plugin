package sourceissuer

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
)

func TestEnrollmentConfigurationRequiresExplicitBoundedTrust(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{23}, ed25519.SeedSize))
	cfg := enrollmentConfig("http://127.0.0.1:8123", key.Public().(ed25519.PublicKey))
	raw, _ := json.Marshal(cfg)
	if _, err := ParseConfig(raw); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change func(*Config)
	}{
		{"foreign-endpoint", func(c *Config) { c.EnrollmentURL = "https://foreign.example/enroll" }},
		{"plaintext-endpoint", func(c *Config) { c.EnrollmentURL = "http://127.0.0.1/enroll" }},
		{"query-endpoint", func(c *Config) { c.EnrollmentURL += "?target=x" }},
		{"same-command-endpoint", func(c *Config) { c.EnrollmentURL = c.AuthorizationURL }},
		{"no-endpoint", func(c *Config) { c.EnrollmentURL = "" }},
		{"no-hubs", func(c *Config) { c.EnrollmentHubs = nil }},
		{"too-many-hubs", func(c *Config) { c.EnrollmentHubs = make([]EnrollmentHub, 33) }},
		{"duplicate-hub", func(c *Config) { c.EnrollmentHubs = append(c.EnrollmentHubs, c.EnrollmentHubs[0]) }},
		{"invalid-instance", func(c *Config) { c.EnrollmentHubs[0].HubInstanceID = "client-chosen-instance" }},
		{"invalid-public-key", func(c *Config) { c.EnrollmentHubs[0].IdentityPublicKey = "private-looking-synthetic-value" }},
		{"wrong-audience", func(c *Config) { c.EnrollmentHubs[0].Audience = "hub-b" }},
		{"no-verifier", func(c *Config) { c.Routes[0].EnrollmentVerifierURL = "" }},
		{"models-as-verifier", func(c *Config) { c.Routes[0].EnrollmentVerifierURL = "https://station.example/v1/models" }},
		{"old-verifier", func(c *Config) { c.Routes[0].EnrollmentVerifierURL = c.Routes[0].VerifierURL }},
		{"foreign-verifier", func(c *Config) {
			c.Routes[0].EnrollmentVerifierURL = "https://foreign.example/v1/salcara-hub/enrollment-identity"
		}},
		{"private-network-verifier", func(c *Config) {
			c.Routes[0].EnrollmentVerifierURL = "http://192.0.2.1/v1/salcara-hub/enrollment-identity"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var next Config
			_ = json.Unmarshal(raw, &next)
			tc.change(&next)
			changed, _ := json.Marshal(next)
			if _, err := ParseConfig(changed); err == nil {
				t.Fatal("unsafe enrollment configuration accepted")
			}
		})
	}
	for _, bad := range []string{
		strings.Replace(string(raw), `"enrollmentUrl":`, `"EnrollmentUrl":`, 1),
		strings.Replace(string(raw), `"allowDeviceEnrollment":true`, `"allowDeviceEnrollment":true,"allowDeviceEnrollment":false`, 1),
		strings.Replace(string(raw), `"allowDeviceEnrollment":true`, `"allowDeviceEnrollment":null`, 1),
		strings.Replace(string(raw), `"hubInstanceId":`, `"unknown":1,"hubInstanceId":`, 1),
		strings.Replace(string(raw), `"audience":"hub-a"`, `"audience":"hub-a","audience":"hub-b"`, 1),
	} {
		if _, err := ParseConfig([]byte(bad)); err == nil {
			t.Fatal("ambiguous enrollment configuration accepted")
		}
	}
}

func TestEnrollmentDisabledConfigurationDoesNotChangeLegacyService(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{23}, ed25519.SeedSize))
	legacy, err := New(fixtureConfig("http://127.0.0.1:8123"), key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.PublicIssuer().DeviceEnrollment || legacy.PublicIssuer().EnrollmentURL != "" {
		t.Fatal("legacy config gained enrollment permission")
	}
	if w := callEnrollment(legacy, []byte(`{}`)); w.Code != 404 {
		t.Fatal("legacy service exposed enrollment endpoint")
	}
	cfg := enrollmentConfig("http://127.0.0.1:8123", key.Public().(ed25519.PublicKey))
	cfg.Routes[0].Groups[0].AllowDeviceEnrollment = false
	s, err := New(cfg, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.PublicIssuer().DeviceEnrollment || s.PublicIssuer().EnrollmentURL != "" {
		t.Fatal("configured URL silently opted in groups")
	}
	cfg.EnrollmentHubs[0].Audience = "changed"
	if s.cfg.EnrollmentHubs[0].Audience != "hub-a" {
		t.Fatal("mutable input changed pinned trust")
	}
}
