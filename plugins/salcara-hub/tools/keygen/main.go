// keygen creates a new publisher identity. It never prints private key material.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

func main() {
	privateDir := flag.String("private-dir", "", "new private directory outside any repository")
	publicOutput := flag.String("public-output", "", "new public publisher JSON file")
	keyID := flag.String("key-id", "", "new publisher identity; not a built-in publisher ID")
	flag.Parse()
	if err := run(*privateDir, *publicOutput, *keyID); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(privateDir, publicOutput, keyID string) error {
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{2,99}$`).MatchString(keyID) || keyID == "salcara-hub-v1" || keyID == "sub2api-openai-transport-v1" {
		return errors.New("supply a new non-reserved publisher key ID")
	}
	if privateDir == "" || publicOutput == "" {
		return errors.New("-private-dir and -public-output are required")
	}
	privateDir, err := filepath.Abs(privateDir)
	if err != nil {
		return err
	}
	publicOutput, err = filepath.Abs(publicOutput)
	if err != nil {
		return err
	}
	// Refuse reuse/overwrites: a new identity must not destroy an existing key.
	if _, err := os.Stat(publicOutput); !os.IsNotExist(err) {
		return errors.New("public output already exists or is inaccessible")
	}
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		return err
	}
	// Windows mode bits are insufficient. Protect the directory before writing secrets.
	if err := protectPrivateDirectory(privateDir); err != nil {
		return fmt.Errorf("protect private directory: %w", err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	privateFile, err := os.OpenFile(filepath.Join(privateDir, "publisher.private.b64"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := privateFile.WriteString(base64.StdEncoding.EncodeToString(privateKey) + "\n")
	syncErr := privateFile.Sync()
	closeErr := privateFile.Close()
	if writeErr != nil {
		return writeErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	encoded, err := json.MarshalIndent(map[string]string{"algorithm": "ed25519", "key_id": keyID, "public_key": base64.StdEncoding.EncodeToString(publicKey)}, "", "  ")
	if err != nil {
		return err
	}
	publicFile, err := os.OpenFile(publicOutput, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, writeErr = publicFile.Write(append(encoded, '\n'))
	closeErr = publicFile.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	fmt.Printf("Created publisher %s; public identity: %s. Keep the private directory offline and outside repositories.\n", keyID, publicOutput)
	return nil
}
