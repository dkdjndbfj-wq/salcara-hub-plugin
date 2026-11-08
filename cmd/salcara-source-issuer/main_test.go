package main

import (
	"bytes"
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
