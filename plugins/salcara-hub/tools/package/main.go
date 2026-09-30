package main

import (
	"archive/zip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	goBinary := flag.String("go", "go", "Go executable")
	output := flag.String("output", "dist/salcara-hub-0.3.1.s2plugin", "output .s2plugin")
	privatePath := flag.String("signing-key", "", "Base64 Ed25519 private key file")
	keyID := flag.String("key-id", "", "publisher key ID")
	flag.Parse()
	if err := run(*goBinary, *output, *privatePath, *keyID); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(goBinary, output, privatePath, keyID string) error {
	if (privatePath == "") != (keyID == "") {
		return errors.New("-signing-key 和 -key-id 必须同时提供")
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	manifestRaw, err := os.ReadFile(filepath.Join(root, "manifest.source.json"))
	if err != nil {
		return err
	}
	var manifest map[string]any
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		return err
	}
	staging, err := os.MkdirTemp("", "salcara-plugin-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	files := map[string][]byte{}
	runtimes := map[string]any{}
	for _, target := range []struct{ os, arch string }{{"linux", "amd64"}, {"linux", "arm64"}, {"windows", "amd64"}} {
		name := target.os + "-" + target.arch
		binary := filepath.Join(staging, "salcara-hub-"+name)
		if target.os == "windows" {
			binary += ".exe"
		}
		command := exec.Command(goBinary, "build", "-trimpath", "-ldflags=-s -w", "-o", binary, ".")
		command.Dir = root
		command.Env = append(os.Environ(), "GOOS="+target.os, "GOARCH="+target.arch, "CGO_ENABLED=0")
		result, buildErr := command.CombinedOutput()
		if buildErr != nil {
			return fmt.Errorf("build %s: %w: %s", name, buildErr, result)
		}
		contents, readErr := os.ReadFile(binary)
		if readErr != nil {
			return readErr
		}
		path := "runtimes/" + name + "/salcara-hub"
		if target.os == "windows" {
			path += ".exe"
		}
		files[path] = contents
		runtimes[name] = map[string]string{"path": path}
	}
	manifest["runtimes"] = runtimes
	for _, path := range []string{"ui/index.html", "ui/assets/app.css", "ui/assets/app.js"} {
		contents, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if readErr != nil {
			return readErr
		}
		files[path] = contents
	}
	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		return err
	}
	files["README.md"] = readme
	license, err := os.ReadFile(filepath.Join(root, "..", "..", "LICENSE"))
	if err != nil {
		return err
	}
	files["LICENSE"] = license
	// Redistributed runtime dependencies retain their complete notices. These
	// legal resources are declared and hashed like all other package files.
	legalRoot := filepath.Join(root, "licenses")
	if err := filepath.WalkDir(legalRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("license resources must not be symlinks")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > 1024*1024 {
			return errors.New("invalid license resource")
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(relative)] = contents
		return nil
	}); err != nil && !os.IsNotExist(err) {
		return err
	}
	hashes := map[string]string{}
	for path, contents := range files {
		hash := sha256.Sum256(contents)
		hashes[path] = hex.EncodeToString(hash[:])
	}
	manifest["files"] = hashes
	manifestRaw, err = json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	manifestRaw = append(manifestRaw, '\n')
	files["manifest.json"] = manifestRaw
	if privatePath != "" {
		encoded, readErr := os.ReadFile(privatePath)
		if readErr != nil {
			return readErr
		}
		key, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
		if decodeErr != nil || len(key) != ed25519.PrivateKeySize {
			return errors.New("签名私钥格式无效")
		}
		signature, marshalErr := json.Marshal(map[string]string{
			"algorithm": "ed25519", "key_id": keyID,
			"signature": base64.StdEncoding.EncodeToString(ed25519.Sign(ed25519.PrivateKey(key), manifestRaw)),
		})
		if marshalErr != nil {
			return marshalErr
		}
		files["signature.json"] = signature
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
		return err
	}
	target, err := os.Create(output)
	if err != nil {
		return err
	}
	writer := zip.NewWriter(target)
	packagePaths := []string{"manifest.json", "signature.json", "runtimes/linux-amd64/salcara-hub", "runtimes/linux-arm64/salcara-hub", "runtimes/windows-amd64/salcara-hub.exe", "ui/index.html", "ui/assets/app.css", "ui/assets/app.js", "README.md", "LICENSE"}
	var legalPaths []string
	for path := range files {
		if strings.HasPrefix(path, "licenses/") {
			legalPaths = append(legalPaths, path)
		}
	}
	sort.Strings(legalPaths)
	packagePaths = append(packagePaths, legalPaths...)
	for _, path := range packagePaths {
		contents, ok := files[path]
		if !ok {
			continue
		}
		entry, createErr := writer.Create(path)
		if createErr != nil {
			_ = writer.Close()
			_ = target.Close()
			return createErr
		}
		if _, writeErr := entry.Write(contents); writeErr != nil {
			_ = writer.Close()
			_ = target.Close()
			return writeErr
		}
	}
	if err := writer.Close(); err != nil {
		_ = target.Close()
		return err
	}
	if err := target.Close(); err != nil {
		return err
	}
	stat, err := os.Stat(output)
	if err != nil {
		return err
	}
	fmt.Printf("built %s (%d bytes)\n", output, stat.Size())
	return nil
}
