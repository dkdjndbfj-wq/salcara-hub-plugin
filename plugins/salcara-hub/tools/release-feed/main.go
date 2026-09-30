// release-feed derives public update metadata from a locally verified signed
// package. No private key is read and no network or publishing is performed.
package main

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
)

func main() {
	packagePath := flag.String("package", "", "signed .s2plugin")
	publicPath := flag.String("public-identity", "", "public publisher JSON, never a private key")
	packageURL := flag.String("package-url", "", "public HTTPS release asset URL")
	output := flag.String("output", "", "new update.json file")
	notes := flag.String("notes", "", "release notes (maximum 4096 bytes)")
	flag.Parse()
	if err := run(*packagePath, *publicPath, *packageURL, *output, *notes); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(packagePath, publicPath, packageURL, output, notes string) error {
	u, err := url.Parse(packageURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("a credential-free public HTTPS package URL is required")
	}
	if output == "" || len(notes) > 4096 {
		return errors.New("new output path required; notes must be at most 4096 bytes")
	}
	publicRaw, err := os.ReadFile(publicPath)
	if err != nil {
		return err
	}
	var publisher struct {
		KeyID     string `json:"key_id"`
		PublicKey string `json:"public_key"`
	}
	if err := json.Unmarshal(publicRaw, &publisher); err != nil {
		return err
	}
	publicKey, err := base64.StdEncoding.DecodeString(publisher.PublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize || publisher.KeyID == "" {
		return errors.New("invalid public publisher identity")
	}
	data, err := os.ReadFile(packagePath)
	if err != nil {
		return err
	}
	if len(data) > 128*1024*1024 {
		return errors.New("package exceeds release limit")
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	entries := map[string]*zip.File{}
	for _, file := range archive.File {
		if entries[file.Name] != nil {
			return errors.New("duplicate package entry")
		}
		entries[file.Name] = file
	}
	read := func(name string, limit int64) ([]byte, error) {
		file := entries[name]
		if file == nil || file.UncompressedSize64 > uint64(limit) {
			return nil, fmt.Errorf("missing or oversized entry: %s", name)
		}
		r, err := file.Open()
		if err != nil {
			return nil, err
		}
		defer r.Close()
		content, err := io.ReadAll(io.LimitReader(r, limit+1))
		if err != nil {
			return nil, err
		}
		if int64(len(content)) > limit {
			return nil, errors.New("entry exceeds limit")
		}
		return content, nil
	}
	manifestRaw, err := read("manifest.json", 1024*1024)
	if err != nil {
		return err
	}
	var manifest struct {
		ID       string            `json:"id"`
		Version  string            `json:"version"`
		Requires json.RawMessage   `json:"requires"`
		Files    map[string]string `json:"files"`
	}
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		return err
	}
	if manifest.ID == "" || manifest.Version == "" || len(manifest.Requires) == 0 || len(manifest.Files) == 0 {
		return errors.New("incomplete manifest")
	}
	signatureRaw, err := read("signature.json", 64*1024)
	if err != nil {
		return err
	}
	var signature struct {
		Algorithm string `json:"algorithm"`
		KeyID     string `json:"key_id"`
		Signature string `json:"signature"`
	}
	if err := json.Unmarshal(signatureRaw, &signature); err != nil {
		return err
	}
	proof, err := base64.StdEncoding.DecodeString(signature.Signature)
	if err != nil || signature.Algorithm != "ed25519" || signature.KeyID != publisher.KeyID || !ed25519.Verify(ed25519.PublicKey(publicKey), manifestRaw, proof) {
		return errors.New("package signature does not match the supplied public publisher identity")
	}
	for name, expected := range manifest.Files {
		content, err := read(name, 64*1024*1024)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(content)
		if hex.EncodeToString(hash[:]) != expected {
			return fmt.Errorf("package file hash mismatch: %s", name)
		}
	}
	for name := range entries {
		if name != "manifest.json" && name != "signature.json" && manifest.Files[name] == "" {
			return errors.New("undeclared package entry")
		}
	}
	hash := sha256.Sum256(data)
	feed := struct {
		SchemaVersion  int             `json:"schema_version"`
		PluginID       string          `json:"plugin_id"`
		Version        string          `json:"version"`
		PackageURL     string          `json:"package_url"`
		SHA256         string          `json:"sha256"`
		SizeBytes      int             `json:"size_bytes"`
		PublisherKeyID string          `json:"publisher_key_id"`
		Requires       json.RawMessage `json:"requires"`
		ReleaseNotes   string          `json:"release_notes,omitempty"`
	}{1, manifest.ID, manifest.Version, packageURL, hex.EncodeToString(hash[:]), len(data), publisher.KeyID, manifest.Requires, notes}
	encoded, err := json.MarshalIndent(feed, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(append(encoded, '\n'))
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	fmt.Printf("Verified signed package; wrote public update metadata: %s\n", output)
	return nil
}
