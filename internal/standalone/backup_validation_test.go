package standalone

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func backupValidationTarget(cfg Config, dir string) Config {
	cfg.DataDir = dir
	cfg.AdminAccountFile = filepath.Join(dir, defaultAccountFile)
	return cfg
}

func assertBackupNoPublication(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatal("failed restore published a target", err)
	}
	stages, err := filepath.Glob(filepath.Join(filepath.Dir(dir), ".salcara-restore-*"))
	if err != nil || len(stages) != 0 {
		t.Fatal("failed restore left a plaintext staging directory", stages, err)
	}
}

func TestBackupValidationRejectsMismatchedMailboxKey(t *testing.T) {
	s, cfg, _ := serverFixture(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(cfg.DataDir, "mailbox", "key-v1")
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	key[0] ^= 1
	if err = os.WriteFile(keyPath, key, 0600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	archive, wrappingKey := filepath.Join(outside, "mismatched.scbackup"), filepath.Join(outside, "wrapping.key")
	if _, err = CreateBackup(cfg, archive, wrappingKey); err != nil {
		t.Fatal(err)
	}
	// The archive's authentication is valid; the mailbox's separate key is not.
	if _, err = VerifyBackup(archive, wrappingKey); err != nil {
		t.Fatal(err)
	}
	target := backupValidationTarget(cfg, filepath.Join(outside, "restored"))
	if err = RestoreBackup(target, archive, wrappingKey); err == nil {
		t.Fatal("restored an unreadable mailbox with a different 32-byte key")
	}
	assertBackupNoPublication(t, target.DataDir)
}

func TestBackupValidationPreservesExecutableOwnerPermission(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable permission is validated in Linux")
	}
	s, cfg, _ := serverFixture(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join("updates", "hub-fixture")
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "updates"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, name), []byte("#!/bin/sh\nprintf restore-executable"), 0500); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	archive, key := filepath.Join(outside, "executable.scbackup"), filepath.Join(outside, "backup.key")
	if _, err := CreateBackup(cfg, archive, key); err != nil {
		t.Fatal(err)
	}
	target := backupValidationTarget(cfg, filepath.Join(outside, "restored"))
	if err := RestoreBackup(target, archive, key); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(target.DataDir, name)
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("owner execute bit lost or non-owner permissions granted", info, err)
	}
	out, err := exec.Command(file).Output()
	if err != nil || string(out) != "restore-executable" {
		t.Fatal("restored executable cannot run", string(out), err)
	}
}

func TestBackupValidationRejectsAlteredManifestAndMode(t *testing.T) {
	cfg := testConfig(t)
	outside := t.TempDir()
	keyFile := filepath.Join(outside, "backup.key")
	key, err := backupKey(keyFile, true)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	data := []byte("private payload")
	sum := sha256.Sum256(data)
	for _, kind := range []string{"digest", "mode", "size", "duplicate-manifest"} {
		t.Run(kind, func(t *testing.T) {
			var buffer bytes.Buffer
			sealed, err := newSealedBackupWriter(&buffer, key)
			if err != nil {
				t.Fatal(err)
			}
			gz := gzip.NewWriter(sealed)
			tarWriter := tar.NewWriter(gz)
			if err = tarWriter.WriteHeader(&tar.Header{Name: "data/payload", Mode: 0600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
				t.Fatal(err)
			}
			if _, err = tarWriter.Write(data); err != nil {
				t.Fatal(err)
			}
			entry := backupEntry{Path: "payload", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Mode: 0600}
			switch kind {
			case "digest":
				entry.SHA256 = "wrong"
			case "mode":
				entry.Mode = 0700
			case "size":
				entry.Size++
			}
			manifest, err := json.Marshal(backupManifest{Schema: 1, Entries: []backupEntry{entry}})
			if err != nil {
				t.Fatal(err)
			}
			count := 1
			if kind == "duplicate-manifest" {
				count = 2
			}
			for i := 0; i < count; i++ {
				if err = tarWriter.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: int64(len(manifest)), Typeflag: tar.TypeReg}); err != nil {
					t.Fatal(err)
				}
				if _, err = tarWriter.Write(manifest); err != nil {
					t.Fatal(err)
				}
			}
			for _, close := range []func() error{tarWriter.Close, gz.Close, sealed.Close} {
				if err = close(); err != nil {
					t.Fatal(err)
				}
			}
			archive := filepath.Join(outside, kind+".scbackup")
			if err = os.WriteFile(archive, buffer.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = VerifyBackup(archive, keyFile); err == nil {
				t.Fatal("verified altered manifest", kind)
			}
			target := backupValidationTarget(cfg, filepath.Join(outside, "restored-"+kind))
			if err = RestoreBackup(target, archive, keyFile); err == nil {
				t.Fatal("restored altered manifest", kind)
			}
			assertBackupNoPublication(t, target.DataDir)
		})
	}
}

func TestBackupValidationFullArchiveRejectsTruncationAndEmptyTarget(t *testing.T) {
	s, cfg, _ := serverFixture(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	archive, key := filepath.Join(outside, "complete.scbackup"), filepath.Join(outside, "backup.key")
	if _, err := CreateBackup(cfg, archive, key); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	for i, invalid := range [][]byte{raw[:len(raw)-1], raw[:len(raw)-20], raw[:len(raw)/2], append(append([]byte(nil), raw...), 1)} {
		damaged := filepath.Join(outside, fmt.Sprintf("damaged-%d.scbackup", i))
		if err = os.WriteFile(damaged, invalid, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = VerifyBackup(damaged, key); err == nil {
			t.Fatal("verified truncated/appended archive", i)
		}
		target := backupValidationTarget(cfg, filepath.Join(outside, fmt.Sprintf("restored-%d", i)))
		if err = RestoreBackup(target, damaged, key); err == nil {
			t.Fatal("restored truncated/appended archive", i)
		}
		assertBackupNoPublication(t, target.DataDir)
	}
	target := backupValidationTarget(cfg, filepath.Join(outside, "existing-empty"))
	if err = os.Mkdir(target.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(target.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err = RestoreBackup(target, archive, key); err == nil {
		t.Fatal("overwrote an existing empty directory")
	}
	after, err := os.Stat(target.DataDir)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("existing directory changed", err)
	}
	stage := filepath.Join(outside, "publication-test")
	if err = os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err = publishRestoredDirectory(stage, target.DataDir); err == nil {
		t.Fatal("atomic publication replaced an existing empty directory")
	}
}
