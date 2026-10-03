package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type feedFixture struct {
	payload, private, public, output string
	raw                              []byte
	publicKey                        ed25519.PublicKey
	privateKey                       ed25519.PrivateKey
}

func validPayload() payload {
	return payload{Product: "salcara-hub-standalone", Version: "0.4.1", LauncherProtocol: 1, DataSchema: 1, ReleaseNotes: "Standalone Hub test release", Binaries: map[string]binary{
		"linux/amd64": {URL: "https://github.com/dkdjndbfj-wq/salcara-hub-plugin/releases/download/v0.4.1/salcara-hub_0.4.1_linux_amd64", SHA256: strings.Repeat("a", 64), SizeBytes: 1024},
		"linux/arm64": {URL: "https://github.com/dkdjndbfj-wq/salcara-hub-plugin/releases/download/v0.4.1/salcara-hub_0.4.1_linux_arm64", SHA256: strings.Repeat("b", 64), SizeBytes: 2048},
	}}
}

func fixture(t *testing.T) feedFixture {
	t.Helper()
	dir := t.TempDir()
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(validPayload(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	f := feedFixture{payload: filepath.Join(dir, "payload.json"), private: filepath.Join(dir, "fixture-private.b64"), public: filepath.Join(dir, "public.json"), output: filepath.Join(dir, "feed.json"), raw: append(raw, '\n'), publicKey: pub, privateKey: private}
	identity, err := json.Marshal(publicIdentity{Algorithm: "ed25519", KeyID: "fixture-publisher", PublicKey: base64.StdEncoding.EncodeToString(pub)})
	if err != nil {
		t.Fatal(err)
	}
	for path, raw := range map[string][]byte{f.payload: f.raw, f.private: []byte(base64.StdEncoding.EncodeToString(private) + "\n"), f.public: identity} {
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func TestSignedFeedExactPayloadSignatureTamperAndNoPrivateOutput(t *testing.T) {
	f := fixture(t)
	if err := run(f.payload, f.private, f.public, f.output); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(f.output)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > maxFeedBytes || bytes.Contains(raw, []byte(base64.StdEncoding.EncodeToString(f.privateKey))) {
		t.Fatal("private key or oversized metadata in public output")
	}
	var e envelope
	if strictJSON(raw, &e) != nil || e.SchemaVersion != 1 || e.PublisherKeyID != "fixture-publisher" {
		t.Fatal("invalid generated envelope")
	}
	payloadRaw, err := base64.StdEncoding.DecodeString(e.Payload)
	if err != nil || !bytes.Equal(payloadRaw, f.raw) {
		t.Fatal("signer rewrote rather than signed the exact payload")
	}
	signature, err := base64.StdEncoding.DecodeString(e.Signature)
	if err != nil || !ed25519.Verify(f.publicKey, payloadRaw, signature) {
		t.Fatal("generated signature did not verify")
	}
	tampered := append([]byte(nil), payloadRaw...)
	tampered[0] = '['
	if ed25519.Verify(f.publicKey, tampered, signature) {
		t.Fatal("tampered payload verified")
	}
	signature[0] ^= 1
	if ed25519.Verify(f.publicKey, payloadRaw, signature) {
		t.Fatal("tampered signature verified")
	}
	before := append([]byte(nil), raw...)
	if err := run(f.payload, f.private, f.public, f.output); err == nil {
		t.Fatal("existing feed was overwritten")
	}
	after, err := os.ReadFile(f.output)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed O_EXCL signing changed existing output")
	}
}

func TestSignerRejectsMismatchedAndMalformedPrivateKeysWithoutDisclosingContents(t *testing.T) {
	for _, kind := range []string{"mismatch", "malformed-length", "malformed-public-half", "invalid-base64"} {
		t.Run(kind, func(t *testing.T) {
			f := fixture(t)
			var raw []byte
			switch kind {
			case "mismatch":
				_, other, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				raw = []byte(base64.StdEncoding.EncodeToString(other))
			case "malformed-length":
				raw = []byte(base64.StdEncoding.EncodeToString(make([]byte, 32)))
			case "malformed-public-half":
				other := append(ed25519.PrivateKey(nil), f.privateKey...)
				other[32] ^= 1
				raw = []byte(base64.StdEncoding.EncodeToString(other))
			default:
				raw = []byte("private-fixture-not-a-key")
			}
			if err := os.WriteFile(f.private, raw, 0600); err != nil {
				t.Fatal(err)
			}
			err := run(f.payload, f.private, f.public, f.output)
			if err == nil || strings.Contains(err.Error(), string(raw)) {
				t.Fatal("bad key accepted or content disclosed")
			}
			if _, err := os.Stat(f.output); !os.IsNotExist(err) {
				t.Fatal("invalid key created public output")
			}
		})
	}
}

func TestPayloadStrictSchemaAndLimits(t *testing.T) {
	for _, mutate := range []func(*payload){
		func(p *payload) { p.Product = "" }, func(p *payload) { p.Product = "salcara-personal-hub" }, func(p *payload) { p.Product = "salcara-hub-plugin" },
		func(p *payload) { p.Version = "v0.4.1" }, func(p *payload) { p.Version = "01.4.1" }, func(p *payload) { p.Version = "0.4.1-01" }, func(p *payload) { p.Version = "0.4.1.." },
		func(p *payload) { p.Version = "0.4.1+unsupported-build" }, func(p *payload) { p.Version = "4294967296.1.1" }, func(p *payload) { p.Version = "0.4.1-18446744073709551616" },
		func(p *payload) { p.LauncherProtocol = 2 }, func(p *payload) { p.DataSchema = 2 }, func(p *payload) { p.ReleaseNotes = strings.Repeat("x", 4097) },
		func(p *payload) { delete(p.Binaries, "linux/arm64") }, func(p *payload) { p.Binaries["windows/amd64"] = p.Binaries["linux/arm64"] },
		func(p *payload) {
			b := p.Binaries["linux/amd64"]
			b.SizeBytes = maxBinaryBytes + 1
			p.Binaries["linux/amd64"] = b
		},
		func(p *payload) { b := p.Binaries["linux/amd64"]; b.SizeBytes = 0; p.Binaries["linux/amd64"] = b },
		func(p *payload) {
			b := p.Binaries["linux/amd64"]
			b.SHA256 = strings.Repeat("A", 64)
			p.Binaries["linux/amd64"] = b
		},
	} {
		f := fixture(t)
		p := validPayload()
		mutate(&p)
		raw, _ := json.Marshal(p)
		if err := os.WriteFile(f.payload, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err := run(f.payload, f.private, f.public, f.output); err == nil {
			t.Fatal("unsupported payload metadata signed")
		}
	}
	for _, makeRaw := range []func([]byte) []byte{
		func(raw []byte) []byte { return append(raw, []byte(` {}`)...) },
		func(raw []byte) []byte {
			return bytes.Replace(raw, []byte(`"version": "0.4.1"`), []byte(`"version": "0.4.1", "version":"0.4.2"`), 1)
		},
		func(raw []byte) []byte {
			return bytes.Replace(raw, []byte(`"size_bytes": 1024`), []byte(`"size_bytes":1024,"size_bytes":2048`), 1)
		},
		func(raw []byte) []byte {
			return bytes.Replace(raw, []byte(`"version": "0.4.1"`), []byte(`"version":"0.4.1", "unknown":"value"`), 1)
		},
		func(raw []byte) []byte {
			return bytes.Replace(raw, []byte(`"size_bytes": 1024`), []byte(`"size_bytes":1024,"unknown":"value"`), 1)
		},
		func(raw []byte) []byte { return []byte(strings.Repeat("x", maxFeedBytes+1)) },
		func(raw []byte) []byte { return []byte(`null`) },
	} {
		f := fixture(t)
		if err := os.WriteFile(f.payload, makeRaw(f.raw), 0600); err != nil {
			t.Fatal(err)
		}
		if err := run(f.payload, f.private, f.public, f.output); err == nil {
			t.Fatal("non-strict payload signed")
		}
	}
	for _, version := range []string{"0.4.0-dev", "0.4.1-beta.1", "1.2.3-beta-build"} {
		p := validPayload()
		p.Version = version
		raw, _ := json.Marshal(p)
		if err := validatePayload(raw, p); err != nil {
			t.Fatal("valid semantic version rejected")
		}
	}
}

func TestOfflineURLValidationRejectsLocalReservedCredentialsAndUnsafeFilenames(t *testing.T) {
	for _, value := range []string{
		"file:///tmp/private-key", "http://github.com/binary", "https://github.com:8443/binary",
		"https://user:password@github.com/binary", "https://github.com/binary?auth_token=fixture", "https://github.com/binary#fragment",
		"https://localhost/binary", "https://api.local/binary", "https://router.home.arpa/binary", "https://api.internal/binary",
		"https://127.0.0.1/binary", "https://127.1/binary", "https://0177.0.0.1/binary", "https://2130706433/binary",
		"https://10.1.2.3/binary", "https://100.64.0.1/binary", "https://192.0.2.1/binary", "https://198.51.100.1/binary", "https://203.0.113.1/binary",
		"https://[::1]/binary", "https://[fc00::1]/binary", "https://[2001:db8::1]/binary", "https://[64:ff9b::7f00:1]/binary",
		"https://github.com/", "https://github.com/../private", "https://github.com/%2e%2e/private", "https://github.com/a//binary", "https://github.com/.hidden", "https://github.com/path/binary%2Fprivate",
	} {
		if publicBinaryURL(value) {
			t.Fatal("unsafe URL accepted: " + value)
		}
	}
	for _, value := range []string{"https://github.com/releases/download/v0.4.1/hub_linux_amd64", "https://downloads.salcara.top/hub_linux_arm64", "https://8.8.8.8:443/hub"} {
		if !publicBinaryURL(value) {
			t.Fatal("valid public asset URL rejected")
		}
	}
}

func TestIdentityStrictness(t *testing.T) {
	for _, raw := range []string{
		`{"algorithm":"rsa","key_id":"fixture","public_key":"AAAA"}`,
		`{"key_id":"fixture","key_id":"duplicate","public_key":"AAAA"}`,
		`{"key_id":"fixture","public_key":"AAAA","private_key":"do-not-use"}`,
	} {
		f := fixture(t)
		if err := os.WriteFile(f.public, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if err := run(f.payload, f.private, f.public, f.output); err == nil {
			t.Fatal("invalid identity accepted")
		}
	}
}

func TestEnvelopeLimitAndRepositoryPrivateBoundary(t *testing.T) {
	f := fixture(t)
	// The unsigned payload remains below its read limit, but base64 expansion
	// would exceed the public envelope limit. Reject rather than emitting an
	// oversized feed that the launcher could not verify.
	padded := append([]byte(strings.Repeat(" ", 50<<10)), f.raw...)
	if err := os.WriteFile(f.payload, padded, 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(f.payload, f.private, f.public, f.output); err == nil {
		t.Fatal("oversized base64 envelope emitted")
	}
	if _, err := os.Stat(f.output); !os.IsNotExist(err) {
		t.Fatal("oversized envelope created output")
	}
	if err := os.WriteFile(f.payload, f.raw, 0600); err != nil {
		t.Fatal(err)
	}
	repository := t.TempDir()
	if err := os.Mkdir(filepath.Join(repository, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(repository, "test-private.b64")
	if err := os.WriteFile(inside, []byte(base64.StdEncoding.EncodeToString(f.privateKey)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repository)
	if err := run(f.payload, inside, f.public, f.output); err == nil {
		t.Fatal("repository-contained private key accepted")
	}
	if _, err := os.Stat(f.output); !os.IsNotExist(err) {
		t.Fatal("repository-contained key created output")
	}
	if err := run(f.payload, f.private, f.public, f.output); err != nil {
		t.Fatal("repository-external temporary key rejected")
	}
}
