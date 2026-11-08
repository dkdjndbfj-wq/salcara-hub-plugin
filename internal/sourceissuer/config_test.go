package sourceissuer

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestConfigRejectsAmbiguousOrUnsafeAuthority(t *testing.T) {
	base := fixtureConfig("http://127.0.0.1:8123")
	raw, _ := json.Marshal(base)
	if _, err := ParseConfig(raw); err != nil {
		t.Fatal(err)
	}
	changes := []func(*Config){
		func(c *Config) { c.ID = "station-official" },
		func(c *Config) { c.AuthorizationURL = "http://127.0.0.1/source-grants" },
		func(c *Config) { c.AuthorizationURL = "https://station.example:443/source-grants" },
		func(c *Config) { c.Routes[0].APIBase = "https://station.example/tenant-b/v1" },
		func(c *Config) { c.Routes[0].APIBase = "https://station.example/tenant-a/" },
		func(c *Config) { c.Routes[0].APIBase = "https://station.example/tenant-a/../tenant-b" },
		func(c *Config) { c.Routes[0].APIBase = "https://other.example/tenant-a" },
		func(c *Config) { c.Routes[0].VerifierURL = "http://192.0.2.1/v1/salcara-hub/key" },
		func(c *Config) { c.Routes[0].VerifierURL = "https://other.example/v1/salcara-hub/key" },
		func(c *Config) { c.Routes[0].VerifierURL = "http://127.0.0.1:99999/v1/salcara-hub/key" },
		func(c *Config) { c.Routes[0].VerifierURL = "http://127.0.0.1:8123/v1/models" },
		func(c *Config) { c.Routes[0].Groups[0].SourceType = "official_account_proxy" },
		func(c *Config) { c.Routes[0].Groups[0].Agents = nil },
		func(c *Config) { c.Routes[0].Groups[0].Audiences = []string{"hub-a", "hub-a"} },
		func(c *Config) { c.Routes[0].Groups[0].ID = 0 },
		func(c *Config) { c.Routes[0].Groups = append(c.Routes[0].Groups, c.Routes[0].Groups[0]) },
	}
	for index, change := range changes {
		var next Config
		_ = json.Unmarshal(raw, &next)
		change(&next)
		b, _ := json.Marshal(next)
		if _, err := ParseConfig(b); err == nil {
			t.Errorf("unsafe configuration %d accepted", index)
		}
	}
	for _, bad := range []string{
		strings.Replace(string(raw), `"id":"station-a"`, `"id":"station-a","id":"station-b"`, 1),
		strings.Replace(string(raw), `"id":7`, `"id":7,"ID":8`, 1),
		strings.Replace(string(raw), `"sourceType":"site_api"`, `"sourceType":"site_api","sourceType":"verified_proxy"`, 1),
		string(raw) + " {}",
	} {
		if _, err := ParseConfig([]byte(bad)); err == nil {
			t.Fatal("ambiguous configuration accepted")
		}
	}
}

func TestConfigurationAndPublicPolicyAreIsolated(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cfg := fixtureConfig("http://127.0.0.1:8123")
	s, err := New(cfg, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Routes[0].Groups[0].Audiences[0] = "evil"
	cfg.Routes[0].Groups[0].Agents[0] = "evil"
	key[0] ^= 1
	pub := s.PublicIssuer()
	pub.AgentIDs[0] = "evil"
	pub.APIBases[0] = "https://evil.example"
	if s.cfg.Routes[0].Groups[0].Audiences[0] != "hub-a" || s.PublicIssuer().AgentIDs[0] != "codex" || s.PublicIssuer().APIBases[0] != "https://station.example/tenant-a" {
		t.Fatal("mutable input escaped isolation")
	}
	if _, err := New(cfg, key, nil); err == nil {
		t.Fatal("malformed seed/public key pair accepted")
	}
}

func TestPrivateKeyReaderBoundedAndSilent(t *testing.T) {
	seed := bytes.Repeat([]byte{7}, 32)
	encoded := base64.RawURLEncoding.EncodeToString(seed)
	key, err := ReadPrivateKey(strings.NewReader(encoded + "\n"))
	if err != nil || !bytes.Equal(key.Seed(), seed) {
		t.Fatal("key did not roundtrip")
	}
	for _, bad := range []string{encoded + "=", strings.Repeat("x", 129), "synthetic-private-content", base64.RawURLEncoding.EncodeToString(seed[:31])} {
		if _, err := ReadPrivateKey(strings.NewReader(bad)); err == nil || strings.Contains(err.Error(), bad) {
			t.Fatal("invalid key or private error")
		}
	}
}
