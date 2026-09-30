package main

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T, change string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("test runtime")
	hash := sha256.Sum256(content)
	manifest, _ := json.Marshal(map[string]any{"id": "top.salcara.hub", "version": "0.3.0", "requires": map[string]any{"sub2api": ">=0.2.11 <0.3.0", "plugin_protocol": 1, "transport_api": 1, "ui_bridge": 1}, "files": map[string]string{"bin/test": hex.EncodeToString(hash[:])}})
	sig, _ := json.Marshal(map[string]string{"algorithm": "ed25519", "key_id": "test-publisher", "signature": base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, manifest))})
	if change == "manifest" {
		manifest = bytes.ReplaceAll(manifest, []byte("0.3.0"), []byte("0.3.1"))
	}
	if change == "content" {
		content = []byte("changed runtime")
	}
	if change == "publisher" {
		publicKey, _, err = ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
	}
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for name, data := range map[string][]byte{"manifest.json": manifest, "signature.json": sig, "bin/test": content} {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	identity, _ := json.Marshal(map[string]string{"key_id": "test-publisher", "public_key": base64.StdEncoding.EncodeToString(publicKey)})
	packagePath, publicPath := filepath.Join(dir, "package.s2plugin"), filepath.Join(dir, "publisher.public.json")
	if err := os.WriteFile(packagePath, archive.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publicPath, identity, 0o600); err != nil {
		t.Fatal(err)
	}
	return packagePath, publicPath
}

func TestReleaseFeedValidatesBeforeWriting(t *testing.T) {
	for _, change := range []string{"", "manifest", "content", "publisher"} {
		t.Run(change, func(t *testing.T) {
			packagePath, publicPath := fixture(t, change)
			output := filepath.Join(t.TempDir(), "update.json")
			err := run(packagePath, publicPath, "https://github.com/example/plugin/releases/download/v0.3.0/package.s2plugin", output, "offline test")
			if change != "" {
				if err == nil {
					t.Fatal("invalid package accepted")
				}
				if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
					t.Fatal("wrote metadata for invalid package")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			var feed map[string]any
			if err := json.Unmarshal(data, &feed); err != nil {
				t.Fatal(err)
			}
			if feed["plugin_id"] != "top.salcara.hub" || feed["publisher_key_id"] != "test-publisher" || feed["schema_version"] != float64(1) {
				t.Fatal("incorrect update metadata")
			}
			if err := run(packagePath, publicPath, "https://github.com/example/pkg", output, ""); err == nil {
				t.Fatal("existing metadata overwritten")
			}
		})
	}
}

func TestReleaseFeedRefusesCredentialURL(t *testing.T) {
	for _, address := range []string{"http://example.com/pkg", "https://secret@example.com/pkg", "https://example.com/pkg#fragment", ""} {
		if err := run("unused", "unused", address, "unused", ""); err == nil {
			t.Fatal("unsafe URL accepted")
		}
	}
}
