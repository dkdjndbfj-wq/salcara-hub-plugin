package hostfleet

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBundleContainsFixedFilesAndVerifiesArtifact(t *testing.T) {
	binary := bytes.Repeat([]byte("hostfleet-fixture-binary"), 128)
	path := filepath.Join(t.TempDir(), "edge")
	if err := os.WriteFile(path, binary, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(binary)
	h := record{Host: Host{Name: "../../must-not-be-a-member"}, Secret: Credentials{AuthType: "password", Password: "ssh-secret-not-for-the-bundle"}, Token: strings.Repeat("ad", 32)}
	p := Plan{Arch: "amd64", Origin: "https://origin.example", ArtifactSHA256: hex.EncodeToString(sum[:])}
	var output bytes.Buffer
	if err := writeBundle(&output, path, h, p); err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(bytes.NewReader(output.Bytes()))
	files := map[string][]byte{}
	allowed := map[string]bool{"salcara-edge": true, "install.sh": true, "service": true, "environment": true, "control.token": true, "binary.sha256": true, "machine": true, "curl-control": true, "owner": true}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !allowed[header.Name] || files[header.Name] != nil || header.Typeflag != tar.TypeReg {
			t.Fatalf("unexpected archive member: %+v", header)
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = content
		if header.Mode&0077 != 0 {
			t.Fatalf("bundle member is not private: %s", header.Name)
		}
		if bytes.Contains(content, []byte(h.Secret.Password)) {
			t.Fatalf("SSH credential copied to %s", header.Name)
		}
		if bytes.Contains(content, []byte(h.Token)) && header.Name != "control.token" && header.Name != "curl-control" {
			t.Fatalf("controller secret copied to unexpected member %s", header.Name)
		}
	}
	if len(files) != len(allowed) || !bytes.Equal(files["salcara-edge"], binary) {
		t.Fatal("incomplete or corrupt bundle")
	}
	if string(files["control.token"]) != h.Token || string(files["machine"]) != "x86_64" || string(files["binary.sha256"]) != p.ArtifactSHA256+"  salcara-edge\n" {
		t.Fatal("bundle metadata does not match preview")
	}
	if !bytes.Contains(files["environment"], []byte("SALCARA_EDGE_ORIGIN="+p.Origin+"\n")) {
		t.Fatal("fixed origin missing from environment")
	}
	ownerHash := sha256.Sum256([]byte(h.Token))
	if string(files["owner"]) != "salcara-hostfleet-v1:"+hex.EncodeToString(ownerHash[:]) {
		t.Fatal("deployment owner is not bound to controller credential")
	}
	binary[0] ^= 1
	if err := os.WriteFile(path, binary, 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeBundle(io.Discard, path, h, p); err == nil {
		t.Fatal("artifact changed after preview was accepted")
	}
}
