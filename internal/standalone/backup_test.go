package standalone

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncryptedBackupRoundTripAndNoOverwrite(t *testing.T) {
	s, cfg, _ := serverFixture(t)
	outside := t.TempDir()
	archive := filepath.Join(outside, "hub.scbackup")
	key := filepath.Join(outside, "private.key")
	if _, e := CreateBackup(cfg, archive, key); e == nil {
		t.Fatal("backed up a live writer")
	}
	if _, e := os.Stat(archive); !os.IsNotExist(e) {
		t.Fatal("live backup created output")
	}
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
	data := []byte(strings.Repeat("sensitive-file-", 13000))
	source := filepath.Join(cfg.DataDir, "payload-test.bin")
	if e := os.WriteFile(source, data, 0600); e != nil {
		t.Fatal(e)
	}
	receipt, e := CreateBackup(cfg, archive, key)
	if e != nil {
		t.Fatal(e)
	}
	if receipt.Files < 3 || len(receipt.SHA256) != 64 {
		t.Fatal("missing receipt")
	}
	raw, e := os.ReadFile(archive)
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(raw, []byte("sensitive-file")) || bytes.Contains(raw, []byte("admin-account")) {
		t.Fatal("backup plaintext leaked")
	}
	if count, e := VerifyBackup(archive, key); e != nil || count != receipt.Files {
		t.Fatalf("verify %d %v", count, e)
	}
	if _, e := CreateBackup(cfg, archive, key); e == nil {
		t.Fatal("overwrote backup")
	}
	if e := RestoreBackup(cfg, archive, key); e == nil {
		t.Fatal("overwrote existing volume")
	}
	target := cfg
	target.DataDir = filepath.Join(outside, "restored")
	target.AdminAccountFile = filepath.Join(target.DataDir, defaultAccountFile)
	if e := RestoreBackup(target, archive, key); e != nil {
		t.Fatal(e)
	}
	got, e := os.ReadFile(filepath.Join(target.DataDir, "payload-test.bin"))
	if e != nil || !bytes.Equal(got, data) {
		t.Fatal("restored payload changed")
	}
	restarted, e := New(target, nil)
	if e != nil {
		t.Fatal(e)
	}
	restarted.Close()
	if e := RestoreBackup(target, archive, key); e == nil {
		t.Fatal("second restore overwrote volume")
	}
}

func TestBackupRejectsWrongKeyTruncationTamperAndAppendedData(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	var b bytes.Buffer
	w, e := newSealedBackupWriter(&b, key)
	if e != nil {
		t.Fatal(e)
	}
	data := make([]byte, 150000)
	rand.Read(data)
	if _, e = w.Write(data); e != nil {
		t.Fatal(e)
	}
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
	raw := b.Bytes()
	wrong := append([]byte{}, key...)
	wrong[0] ^= 1
	for _, tc := range []struct {
		name     string
		raw, key []byte
	}{
		{"wrong-key", raw, wrong}, {"truncated", raw[:len(raw)-1], key}, {"appended", append(append([]byte{}, raw...), 1), key},
	} {
		r, e := newSealedBackupReader(bytes.NewReader(tc.raw), tc.key)
		if e == nil {
			var out bytes.Buffer
			_, e = out.ReadFrom(r)
		}
		if e == nil {
			t.Errorf("accepted %s", tc.name)
		}
	}
	tampered := append([]byte{}, raw...)
	tampered[90] ^= 1
	r, e := newSealedBackupReader(bytes.NewReader(tampered), key)
	if e != nil {
		t.Fatal(e)
	}
	var out bytes.Buffer
	if _, e = out.ReadFrom(r); e == nil {
		t.Fatal("accepted tampering")
	}
	r, e = newSealedBackupReader(bytes.NewReader(raw), key)
	if e != nil {
		t.Fatal(e)
	}
	out.Reset()
	if _, e = out.ReadFrom(r); e != nil || !bytes.Equal(out.Bytes(), data) {
		t.Fatal("multi-frame roundtrip failed", e)
	}
}

func TestBackupRejectsTraversalAndNoPartialRestore(t *testing.T) {
	cfg := testConfig(t)
	parent := t.TempDir()
	cfg.DataDir = filepath.Join(parent, "restored")
	cfg.AdminAccountFile = filepath.Join(cfg.DataDir, defaultAccountFile)
	keyPath := filepath.Join(parent, "key")
	key, e := backupKey(keyPath, true)
	if e != nil {
		t.Fatal(e)
	}
	for i, name := range []string{"data/../escaped", "data/a/../../escaped", "data/C:/escaped", "data/a\\b"} {
		var b bytes.Buffer
		sealed, _ := newSealedBackupWriter(&b, key)
		gz := gzip.NewWriter(sealed)
		tw := tar.NewWriter(gz)
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: 4, Typeflag: tar.TypeReg})
		tw.Write([]byte("test"))
		tw.Close()
		gz.Close()
		sealed.Close()
		archive := filepath.Join(parent, string(rune('a'+i))+".scbackup")
		os.WriteFile(archive, b.Bytes(), 0600)
		if e := RestoreBackup(cfg, archive, keyPath); e == nil {
			t.Errorf("accepted %q", name)
		}
		if _, e := os.Stat(cfg.DataDir); !os.IsNotExist(e) {
			t.Fatal("published partial restore")
		}
	}
	if _, e := os.Stat(filepath.Join(parent, "escaped")); !os.IsNotExist(e) {
		t.Fatal("escaped restore root")
	}
}

func TestBackupOutputAndKeyMustStayOutsideVolume(t *testing.T) {
	s, cfg, _ := serverFixture(t)
	s.Close()
	outside := t.TempDir()
	for _, tc := range [][2]string{{filepath.Join(cfg.DataDir, "backup"), filepath.Join(outside, "key")}, {filepath.Join(outside, "backup"), filepath.Join(cfg.DataDir, "key")}} {
		if _, e := CreateBackup(cfg, tc[0], tc[1]); e == nil {
			t.Fatal("accepted archive/key inside volume")
		}
	}
	if e := publishRestoredDirectory(t.TempDir(), cfg.DataDir); e == nil {
		t.Fatal("publication overwrote existing directory")
	}
}
