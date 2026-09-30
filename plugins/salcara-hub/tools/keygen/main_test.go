package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeygenIndependentIdentityNoOverwrite(t *testing.T) {
	dir := t.TempDir()
	privateDir, publicPath := filepath.Join(dir, "private"), filepath.Join(dir, "publisher.public.json")
	if err := run(privateDir, publicPath, "test-publisher"); err != nil {
		t.Fatal(err)
	}
	secret, err := os.ReadFile(filepath.Join(privateDir, "publisher.private.b64"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(secret)))
	if err != nil || len(key) != ed25519.PrivateKeySize {
		t.Fatal("invalid generated private identity")
	}
	public, err := os.ReadFile(publicPath)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]string
	if err := json.Unmarshal(public, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["public_key"] != base64.StdEncoding.EncodeToString(ed25519.PrivateKey(key).Public().(ed25519.PublicKey)) {
		t.Fatal("public identity mismatch")
	}
	if bytes := strings.TrimSpace(string(secret)); strings.Contains(string(public), bytes) {
		t.Fatal("private identity in public metadata")
	}
	if err := run(privateDir, publicPath, "test-publisher"); err == nil {
		t.Fatal("existing identity overwritten")
	}
	after, err := os.ReadFile(filepath.Join(privateDir, "publisher.private.b64"))
	if err != nil || string(after) != string(secret) {
		t.Fatal("existing private identity modified")
	}
}

func TestKeygenRefusesReservedIdentities(t *testing.T) {
	for _, id := range []string{"salcara-hub-v1", "sub2api-openai-transport-v1", "../escape", ""} {
		if err := run("unused", "unused", id); err == nil {
			t.Fatal("reserved identity accepted")
		}
	}
}
