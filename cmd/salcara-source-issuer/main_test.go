package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"salcara/hub/internal/hub"
)

func TestCheckAndPublicConfigNeverStartNetworkOrRevealPrivateKey(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "issuer.json")
	keyPath := filepath.Join(root, "issuer.key")
	cfg := `{"id":"station-a","authorizationUrl":"https://station.example/source-grants","routes":[{"apiBase":"https://station.example","verifierUrl":"http://127.0.0.1:1/v1/salcara-hub/key","groups":[{"id":7,"sourceType":"site_api","audiences":["hub-a"],"agents":["codex"]}]}]}`
	private := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{17}, 32))
	if err := os.WriteFile(configPath, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(private), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"-check", "-public-config"} {
		var out bytes.Buffer
		if err := run([]string{"-config", configPath, "-key", keyPath, mode}, &out); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), private) || strings.Contains(out.String(), "verifierUrl") {
			t.Fatal("private data disclosed")
		}
		if mode == "-public-config" {
			var public hub.SourceIssuer
			if json.Unmarshal(out.Bytes(), &public) != nil || public.ID != "station-a" || public.PublicKey == "" {
				t.Fatal("invalid public entry")
			}
		}
	}
	var out bytes.Buffer
	if err := run([]string{"-config", configPath, "-key", keyPath, "-listen", "0.0.0.0:9418", "-check"}, &out); err == nil {
		t.Fatal("public plaintext listener permitted")
	}
}

func TestLoopbackListenerAddress(t *testing.T) {
	for _, address := range []string{"0.0.0.0:9418", ":9418", "example.test:9418", "localhost:9418", "[::]:9418", "127.0.0.1"} {
		if loopback(address) {
			t.Fatal("unsafe listener", address)
		}
	}
	for _, address := range []string{"127.0.0.1:9418", "[::1]:9418"} {
		if !loopback(address) {
			t.Fatal("loopback listener rejected", address)
		}
	}
}

func TestEnrollmentPublicConfigRequiresExplicitOptInAndOmitsPrivateVerifier(t *testing.T) {
	root := t.TempDir()
	configPath, keyPath := filepath.Join(root, "issuer.json"), filepath.Join(root, "issuer.key")
	hubKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{23}, ed25519.SeedSize))
	private := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{17}, ed25519.SeedSize))
	cfg := map[string]any{
		"id": "station-a", "authorizationUrl": "https://station.example/source-grants", "enrollmentUrl": "https://station.example/device-enrollment",
		"enrollmentHubs": []any{map[string]any{"audience": "hub-a", "hubInstanceId": strings.Repeat("1", 32), "identityPublicKey": base64.StdEncoding.EncodeToString(hubKey.Public().(ed25519.PublicKey))}},
		"routes":         []any{map[string]any{"apiBase": "https://station.example", "verifierUrl": "http://127.0.0.1:1/v1/salcara-hub/key", "enrollmentVerifierUrl": "http://127.0.0.1:1/v1/salcara-hub/enrollment-identity", "groups": []any{map[string]any{"id": 7, "sourceType": "site_api", "audiences": []string{"hub-a"}, "agents": []string{"codex"}, "allowDeviceEnrollment": true}}}},
	}
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(private), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := run([]string{"-config", configPath, "-key", keyPath, "-public-config"}, &output); err != nil {
		t.Fatal(err)
	}
	var public hub.SourceIssuer
	if json.Unmarshal(output.Bytes(), &public) != nil || !public.DeviceEnrollment || public.EnrollmentURL != "https://station.example/device-enrollment" {
		t.Fatal("enrollment public configuration missing")
	}
	if strings.Contains(output.String(), private) || strings.Contains(output.String(), "127.0.0.1") || strings.Contains(output.String(), "enrollmentVerifierUrl") || strings.Contains(output.String(), "enrollmentHubs") {
		t.Fatal("private enrollment configuration disclosed")
	}
}
