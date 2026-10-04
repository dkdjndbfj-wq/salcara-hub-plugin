// standalone-feed writes public signed metadata without uploading anything.
// Only an explicitly supplied, repository-external private key is read. The
// key and raw error contents are never printed or included in the output.
package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxFeedBytes = 64 << 10
const maxBinaryBytes = 64 << 20

type binary struct {
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

type payload struct {
	Product          string            `json:"product"`
	Version          string            `json:"version"`
	LauncherProtocol int               `json:"launcher_protocol"`
	DataSchema       int               `json:"data_schema"`
	ReleaseNotes     string            `json:"release_notes"`
	Binaries         map[string]binary `json:"binaries"`
}

type publicIdentity struct {
	Algorithm string `json:"algorithm,omitempty"`
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
}

type envelope struct {
	SchemaVersion  int    `json:"schema_version"`
	PublisherKeyID string `json:"publisher_key_id"`
	Payload        string `json:"payload"`
	Signature      string `json:"signature"`
}

var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([A-Za-z0-9]+(?:[.-][A-Za-z0-9]+)*))?$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var identityPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
var filenamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,255}$`)
var hostnameLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func main() {
	payloadFile := flag.String("payload", "", "trusted public payload JSON file")
	privateFile := flag.String("private-key", "", "repository-external absolute path to 64-byte Ed25519 private key encoded as base64")
	publicFile := flag.String("public-identity", "", "public publisher identity JSON")
	outputFile := flag.String("output", "", "new public signed feed file (never overwritten)")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		os.Exit(1)
	}
	if err := run(*payloadFile, *privateFile, *publicFile, *outputFile); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("Verified publisher identity and wrote new public signed standalone feed.")
}

func run(payloadFile, privateFile, publicFile, outputFile string) error {
	if payloadFile == "" || publicFile == "" || outputFile == "" || !filepath.IsAbs(privateFile) {
		return errors.New("payload, public identity, new output, and absolute repository-external private-key path are required")
	}
	if err := outsideRepository(privateFile); err != nil {
		return err
	}
	raw, err := readRegular(payloadFile, maxFeedBytes, false)
	if err != nil {
		return errors.New("public payload file is unavailable or oversized")
	}
	var p payload
	if err := strictJSON(raw, &p); err != nil {
		return errors.New("public payload must be a strict JSON object with no unknown, repeated, or trailing fields")
	}
	if err := validatePayload(raw, p); err != nil {
		return err
	}
	identityRaw, err := readRegular(publicFile, 4096, false)
	if err != nil {
		return errors.New("public publisher identity file is unavailable or oversized")
	}
	var identity publicIdentity
	if strictJSON(identityRaw, &identity) != nil || !identityPattern.MatchString(identity.KeyID) || identity.Algorithm != "" && identity.Algorithm != "ed25519" {
		return errors.New("public publisher identity is invalid")
	}
	public, err := base64.StdEncoding.DecodeString(identity.PublicKey)
	if err != nil || len(public) != ed25519.PublicKeySize {
		return errors.New("public publisher key must be 32-byte Ed25519 base64")
	}
	privateRaw, err := readRegular(privateFile, 512, true)
	if err != nil {
		return errors.New("private signing file is unavailable or unsafe")
	}
	defer clear(privateRaw)
	private, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(privateRaw)))
	if err != nil || len(private) != ed25519.PrivateKeySize {
		return errors.New("private signing key must be a 64-byte Ed25519 base64 key")
	}
	defer clear(private)
	derived := ed25519.NewKeyFromSeed(private[:ed25519.SeedSize])
	defer clear(derived)
	if !bytes.Equal(derived, private) || !bytes.Equal(derived.Public().(ed25519.PublicKey), public) {
		return errors.New("private signing key does not match the supplied public identity")
	}
	proof := ed25519.Sign(ed25519.PrivateKey(private), raw)
	if !ed25519.Verify(ed25519.PublicKey(public), raw, proof) {
		return errors.New("generated signature did not verify")
	}
	feed := envelope{SchemaVersion: 1, PublisherKeyID: identity.KeyID, Payload: base64.StdEncoding.EncodeToString(raw), Signature: base64.StdEncoding.EncodeToString(proof)}
	encoded, err := json.MarshalIndent(feed, "", "  ")
	if err != nil || len(encoded)+1 > maxFeedBytes {
		return errors.New("signed feed exceeds the 64 KiB envelope limit")
	}
	f, err := os.OpenFile(outputFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return errors.New("cannot create a new public feed file; existing outputs are never overwritten")
	}
	_, writeErr := f.Write(append(encoded, '\n'))
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		return errors.New("public feed could not be completely saved; inspect output before retrying")
	}
	return nil
}

func readRegular(path string, limit int64, private bool) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > limit {
		return nil, errors.New("invalid input file")
	}
	if private && runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("unsafe signing key permissions")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("input unavailable")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("input changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errors.New("input exceeds limit")
	}
	return raw, nil
}

func outsideRepository(privateFile string) error {
	resolved, err := filepath.EvalSymlinks(privateFile)
	if err != nil {
		return errors.New("private signing file must already exist outside the source repository")
	}
	dir, err := os.Getwd()
	if err != nil {
		return errors.New("cannot determine source repository boundary")
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			rel, err := filepath.Rel(dir, resolved)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
				return errors.New("private signing keys must remain outside the source repository")
			}
			return nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

func strictJSON(raw []byte, out any) error {
	if !utf8.Valid(raw) {
		return errors.New("JSON must be valid UTF-8")
	}
	keys := json.NewDecoder(bytes.NewReader(raw))
	if err := uniqueJSON(keys, 0); err != nil {
		return err
	}
	if err := keys.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON")
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return errors.New("object required")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return errors.New("invalid fields")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func uniqueJSON(d *json.Decoder, depth int) error {
	if depth > 8 {
		return errors.New("JSON nesting limit")
	}
	token, err := d.Token()
	if err != nil {
		return errors.New("invalid JSON")
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			name, ok := key.(string)
			if err != nil || !ok || seen[name] {
				return errors.New("repeated JSON key")
			}
			seen[name] = true
			if err := uniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
		last, err := d.Token()
		if err != nil || last != json.Delim('}') {
			return errors.New("incomplete object")
		}
	case '[':
		for d.More() {
			if err := uniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
		last, err := d.Token()
		if err != nil || last != json.Delim(']') {
			return errors.New("incomplete array")
		}
	default:
		return errors.New("invalid delimiter")
	}
	return nil
}

func validatePayload(raw []byte, p payload) error {
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil || len(root) != 6 || root["release_notes"] == nil {
		return errors.New("all six public payload fields are required")
	}
	if p.Product != "salcara-hub-standalone" || len(p.Version) > 96 || !versionPattern.MatchString(p.Version) || p.LauncherProtocol != 1 || p.DataSchema != 1 {
		return errors.New("version, launcher protocol, or data schema is unsupported")
	}
	// Match the launcher's bounded bare-version subset: uint32 components,
	// no prefix/build metadata, and numeric prereleases bounded by uint64.
	match := versionPattern.FindStringSubmatch(p.Version)
	for i := 1; i <= 3; i++ {
		if _, err := strconv.ParseUint(match[i], 10, 32); err != nil {
			return errors.New("version component is too large")
		}
	}
	pre := strings.SplitN(p.Version, "+", 2)[0]
	if parts := strings.SplitN(pre, "-", 2); len(parts) == 2 {
		for _, part := range strings.Split(parts[1], ".") {
			if len(part) > 1 && part[0] == '0' && strings.Trim(part, "0123456789") == "" {
				return errors.New("invalid semantic prerelease version")
			}
			if strings.Trim(part, "0123456789") == "" {
				if _, err := strconv.ParseUint(part, 10, 64); err != nil {
					return errors.New("numeric prerelease component is too large")
				}
			}
		}
	}
	if len(p.ReleaseNotes) > 4096 {
		return errors.New("release notes exceed 4096 bytes")
	}
	if len(p.Binaries) != 2 {
		return errors.New("exactly linux/amd64 and linux/arm64 binaries are required")
	}
	for _, platform := range []string{"linux/amd64", "linux/arm64"} {
		b, ok := p.Binaries[platform]
		if !ok || !digestPattern.MatchString(b.SHA256) || b.SizeBytes < 1 || b.SizeBytes > maxBinaryBytes {
			return errors.New("binary platform, SHA-256, or size is invalid")
		}
		if !publicBinaryURL(b.URL) {
			return errors.New("binary URLs must be credential-free public HTTPS port 443 asset URLs")
		}
	}
	return nil
}

// This is an offline syntactic filter, not a DNS safety assertion. The launcher
// additionally validates every resolved address and pins each connection.
func publicBinaryURL(value string) bool {
	if len(value) > 2048 {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || u.Opaque != "" || u.Port() != "" && u.Port() != "443" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if address, err := netip.ParseAddr(host); err == nil {
		address = address.Unmap()
		if address.Zone() != "" || !publicIP(address) {
			return false
		}
	} else {
		if len(host) > 253 || !strings.Contains(host, ".") {
			return false
		}
		labels := strings.Split(host, ".")
		if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
			return false // never accept shortened or noncanonical numeric IPs
		}
		for _, label := range labels {
			if !hostnameLabelPattern.MatchString(label) {
				return false
			}
		}
		for _, suffix := range []string{".localhost", ".local", ".internal", ".test", ".example", ".invalid", ".onion", ".home.arpa", ".lan", ".home"} {
			if strings.HasSuffix(host, suffix) {
				return false
			}
		}
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) == 0 || !filenamePattern.MatchString(parts[len(parts)-1]) {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\r\n") {
			return false
		}
	}
	return true
}

func publicIP(address netip.Addr) bool {
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() {
		return false
	}
	for _, network := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "64:ff9b::/96", "100::/64", "2001::/32", "2001:db8::/32", "2002::/16"} {
		if netip.MustParsePrefix(network).Contains(address) {
			return false
		}
	}
	return true
}
