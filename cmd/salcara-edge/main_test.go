package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestControlSecretFiles(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	for _, invalid := range []string{strings.Repeat("a", 32), strings.Repeat("z", 64), strings.Repeat("a", 67)} {
		if err := os.WriteFile(file, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readControlSecret(file); err == nil {
			t.Fatal("invalid token accepted")
		}
	}
	secret := strings.Repeat("a", 64)
	if err := os.WriteFile(file, []byte(secret+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := readControlSecret(file); err != nil || string(got) != secret {
		t.Fatal("valid token rejected")
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(file, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := readControlSecret(file); err == nil {
			t.Fatal("public-readable token accepted")
		}
	}
	link := file + "-link"
	if os.Symlink(file, link) == nil {
		if _, err := readControlSecret(link); err == nil {
			t.Fatal("token symlink accepted")
		}
	}
}
