package sourceissuer_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/hub/internal/enrollment"
	"salcara/hub/internal/hub"
	"salcara/hub/internal/sourceissuer"
)

const enrollmentFixtureOrigin = "https://station.integration.example"
const enrollmentFixtureKey = "synthetic-valid-enrollment-key"

type enrollmentFixtureTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (tr enrollmentFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" || r.URL.Host != "station.integration.example" {
		return nil, fmt.Errorf("fixture refused unexpected origin")
	}
	copy := r.Clone(r.Context())
	copy.URL.Scheme, copy.URL.Host = tr.target.Scheme, tr.target.Host
	copy.Host = r.URL.Host
	return tr.base.RoundTrip(copy)
}

type enrollmentFixtureCounts struct {
	Challenge int64 `json:"challenge"`
	Issuer    int64 `json:"issuer"`
	Register  int64 `json:"register"`
	Verifier  int64 `json:"verifier"`
}

type hubEnrollmentFixture struct {
	hub             *hub.Hub
	server          *httptest.Server
	client          *http.Client
	issuerPublic    hub.SourceIssuer
	challengeCount  atomic.Int64
	issuerCount     atomic.Int64
	registerCount   atomic.Int64
	verifierCount   atomic.Int64
	credentialLeaks atomic.Bool
}

func (f *hubEnrollmentFixture) counts() enrollmentFixtureCounts {
	return enrollmentFixtureCounts{f.challengeCount.Load(), f.issuerCount.Load(), f.registerCount.Load(), f.verifierCount.Load()}
}

type hubEnrollmentFixtureOptions struct {
	wrongHubPin      bool
	verifierRedirect string
}

// Only the URL transport is substituted: both public handlers and the TLS
// verifier use their production parsers, signatures and authentication paths.
func newHubEnrollmentFixture(t *testing.T, options hubEnrollmentFixtureOptions) *hubEnrollmentFixture {
	t.Helper()
	f := &hubEnrollmentFixture{}
	issuerKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{37}, ed25519.SeedSize))
	f.issuerPublic = hub.SourceIssuer{
		ID: "station-a", PublicKey: base64.RawURLEncoding.EncodeToString(issuerKey.Public().(ed25519.PublicKey)),
		SourceTypes: []string{"site_api"}, AgentIDs: []string{"codex"}, Origins: []string{enrollmentFixtureOrigin},
		APIBases: []string{enrollmentFixtureOrigin}, AuthorizationURL: enrollmentFixtureOrigin + "/source-grants",
		DeviceEnrollment: true, EnrollmentURL: enrollmentFixtureOrigin + "/device-enrollment",
	}
	var err error
	f.hub, err = hub.New(hub.Config{
		DataDir: t.TempDir(), PublicURL: enrollmentFixtureOrigin + "/salcara-hub", Prefix: "/salcara-hub",
		DeploymentMode: "station", DisableLegacyAPIKey: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		RemoteAccess: hub.RemoteAccessPolicy{Mode: "restricted", Revision: "policy-a", Audience: "hub-a",
			Station: &hub.StationPolicy{Version: 2, APIBases: []string{enrollmentFixtureOrigin}},
			Issuers: []hub.SourceIssuer{f.issuerPublic}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.hub.Close)
	mux := http.NewServeMux()
	mux.Handle("/salcara-hub/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/salcara-hub/v1/device/enrollment/challenge" {
			f.challengeCount.Add(1)
		}
		if r.URL.Path == "/salcara-hub/v1/device/register" {
			f.registerCount.Add(1)
		}
		raw, readErr := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if readErr != nil {
			t.Error(readErr)
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		if bytes.Contains(raw, []byte("synthetic-")) || strings.Contains(fmt.Sprint(r.Header), "synthetic-") || r.Header.Get("Authorization") != "" {
			f.credentialLeaks.Store(true)
		}
		f.hub.ServeHTTP(w, r)
	}))
	mux.HandleFunc("/v1/salcara-hub/enrollment-identity", func(w http.ResponseWriter, r *http.Request) {
		f.verifierCount.Add(1)
		if r.Method != http.MethodGet || r.Header.Get("Accept") != "application/json" || r.Header.Get("X-Salcara-Device-Id") != "" {
			t.Error("incorrect authoritative identity request")
		}
		if r.Header.Get("Authorization") != "Bearer "+enrollmentFixtureKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if options.verifierRedirect != "" {
			w.Header().Set("Location", options.verifierRedirect)
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"version":1,"subject":"sub2api-user:42","groupId":7}`)
	})
	mux.HandleFunc("/fixture-stats", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.counts())
	})
	f.server = httptest.NewTLSServer(mux)
	t.Cleanup(f.server.Close)
	target, err := url.Parse(f.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := enrollmentFixtureTransport{target: target, base: f.server.Client().Transport}
	f.client = &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	identity := f.hub.IngressState()
	if !identity.Configured || len(identity.HubInstanceID) != 32 {
		t.Fatal("Hub did not publish its persisted ingress identity")
	}
	publicKey, err := base64.StdEncoding.Strict().DecodeString(identity.IdentityPublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize || base64.StdEncoding.EncodeToString(publicKey) != identity.IdentityPublicKey {
		t.Fatal("Hub identity public key is not canonical standard base64")
	}
	if options.wrongHubPin {
		publicKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{41}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	}
	s, err := sourceissuer.New(sourceissuer.Config{
		ID: f.issuerPublic.ID, AuthorizationURL: f.issuerPublic.AuthorizationURL, EnrollmentURL: f.issuerPublic.EnrollmentURL,
		EnrollmentHubs: []sourceissuer.EnrollmentHub{{Audience: "hub-a", HubInstanceID: identity.HubInstanceID, IdentityPublicKey: base64.StdEncoding.EncodeToString(publicKey)}},
		Routes: []sourceissuer.Route{{APIBase: enrollmentFixtureOrigin, VerifierURL: enrollmentFixtureOrigin + "/v1/salcara-hub/key",
			EnrollmentVerifierURL: enrollmentFixtureOrigin + "/v1/salcara-hub/enrollment-identity",
			Groups:                []sourceissuer.Group{{ID: 7, SourceType: "site_api", Audiences: []string{"hub-a"}, Agents: []string{"codex"}, AllowDeviceEnrollment: true}}}},
	}, issuerKey, transport)
	if err != nil {
		t.Fatal(err)
	}
	if s.PublicIssuer().PublicKey != f.issuerPublic.PublicKey || !s.PublicIssuer().DeviceEnrollment {
		t.Fatal("issuer public document does not match the Hub trust anchor")
	}
	mux.Handle("/device-enrollment", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.issuerCount.Add(1)
		s.ServeHTTP(w, r)
	}))
	mux.Handle("/source-grants", s)
	t.Cleanup(func() {
		if f.credentialLeaks.Load() {
			t.Error("API credential reached the Hub")
		}
	})
	return f
}

type enrollmentFixtureResponse struct {
	status int
	header http.Header
	body   []byte
}

func (f *hubEnrollmentFixture) call(t *testing.T, method, path string, body any, headers http.Header) enrollmentFixtureResponse {
	t.Helper()
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	r, err := http.NewRequest(method, enrollmentFixtureOrigin+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	r.Header = headers.Clone()
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err = io.ReadAll(io.LimitReader(resp.Body, 128<<10))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(enrollmentFixtureKey)) {
		t.Fatal("API credential appeared in a public response")
	}
	return enrollmentFixtureResponse{resp.StatusCode, resp.Header.Clone(), raw}
}

func (f *hubEnrollmentFixture) challenge(t *testing.T, id, secret, requestID string) (enrollment.Envelope, enrollment.Challenge) {
	t.Helper()
	hash := sha256.Sum256([]byte(secret))
	resp := f.call(t, http.MethodPost, "/salcara-hub/v1/device/enrollment/challenge", map[string]any{
		"version": 1, "issuer": "station-a", "apiBase": enrollmentFixtureOrigin, "deviceId": id,
		"deviceSecretHash": hex.EncodeToString(hash[:]), "requestId": requestID,
	}, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("challenge: %d %s", resp.status, resp.body)
	}
	env, err := enrollment.ParseEnvelope(resp.body)
	if err != nil {
		t.Fatal(err)
	}
	identity := f.hub.IngressState()
	key, _ := base64.StdEncoding.Strict().DecodeString(identity.IdentityPublicKey)
	raw, err := enrollment.VerifyEnvelope(env, enrollment.ChallengePurpose, ed25519.PublicKey(key))
	if err != nil {
		t.Fatal(err)
	}
	c, err := enrollment.ParseChallenge(raw)
	if err != nil {
		t.Fatal(err)
	}
	if c.HubInstanceID != identity.HubInstanceID || c.Audience != "hub-a" || c.APIBase != enrollmentFixtureOrigin || c.PolicyRevision != "policy-a" || c.DeviceID != id || c.RequestID != requestID || c.DeviceSecretHash != hex.EncodeToString(hash[:]) || !enrollment.ValidTime(c.IssuedAt, c.ExpiresAt, time.Now().Unix()) {
		t.Fatal("signed challenge did not bind the real Hub pin and pending identity")
	}
	return env, c
}

func (f *hubEnrollmentFixture) issue(t *testing.T, challenge enrollment.Envelope, key string) enrollmentFixtureResponse {
	t.Helper()
	return f.call(t, http.MethodPost, "/device-enrollment", map[string]any{"version": 1, "challenge": challenge,
		"credential": map[string]string{"type": "api_key", "value": key}}, nil)
}

func assertFixtureFailure(t *testing.T, response enrollmentFixtureResponse, status int, code string) {
	t.Helper()
	var result struct {
		Code string `json:"code"`
	}
	if response.status != status || json.Unmarshal(response.body, &result) != nil || result.Code != code {
		t.Fatalf("got %d %s; want %d/%s", response.status, response.body, status, code)
	}
}

func TestHubIssuerEnrollmentIntegration(t *testing.T) {
	f := newHubEnrollmentFixture(t, hubEnrollmentFixtureOptions{})
	id, secret := "integration-pc", strings.Repeat("a", 64)
	requestID := "10000000-0000-4000-8000-000000000001"
	env, challenge := f.challenge(t, id, secret, requestID)
	issued := f.issue(t, env, enrollmentFixtureKey)
	if issued.status != http.StatusOK || issued.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("issuer: %d %s", issued.status, issued.body)
	}
	grantEnvelope, err := enrollment.ParseEnvelope(issued.body)
	if err != nil {
		t.Fatal(err)
	}
	issuerPublic, err := base64.RawURLEncoding.Strict().DecodeString(f.issuerPublic.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	rawGrant, err := enrollment.VerifyEnvelope(grantEnvelope, enrollment.GrantPurpose, ed25519.PublicKey(issuerPublic))
	if err != nil {
		t.Fatal(err)
	}
	grant, err := enrollment.ParseGrant(rawGrant)
	if err != nil {
		t.Fatal(err)
	}
	if grant.Subject != "sub2api-user:42" || grant.GroupID != 7 || grant.DeviceID != id || grant.HubInstanceID != challenge.HubInstanceID || grant.ExpiresAt > challenge.ExpiresAt {
		t.Fatal("issuer did not preserve the authenticated subject and Hub challenge")
	}
	headers := http.Header{"X-Salcara-Device-Id": {id}, "X-Salcara-Device-Secret": {secret}}
	encodedGrant := base64.RawURLEncoding.EncodeToString(issued.body)
	headers.Set("X-Salcara-Enrollment-Grant", encodedGrant)
	registered := f.call(t, http.MethodPost, "/salcara-hub/v1/device/register", hub.Device{DeviceID: id, ComputerID: "physical-integration-pc", Name: "Integration PC"}, headers)
	if registered.status != http.StatusOK {
		t.Fatalf("register: %d %s", registered.status, registered.body)
	}
	headers.Del("X-Salcara-Enrollment-Grant")
	policy := f.call(t, http.MethodGet, "/salcara-hub/v1/discovery/policy", nil, headers)
	var view struct {
		Scope        string `json:"scope"`
		RemoteAccess struct {
			Revision string `json:"revision"`
		} `json:"remoteAccess"`
	}
	if policy.status != http.StatusOK || json.Unmarshal(policy.body, &view) != nil || view.Scope != "device" || view.RemoteAccess.Revision != "policy-a" {
		t.Fatalf("device policy: %d %s", policy.status, policy.body)
	}
	recovered := f.call(t, http.MethodPost, "/salcara-hub/v1/device/register", hub.Device{DeviceID: id, ComputerID: "physical-integration-pc", Name: "Recovered PC"}, headers)
	if recovered.status != http.StatusOK {
		t.Fatalf("same-secret recovery: %d %s", recovered.status, recovered.body)
	}
	for _, nextRequestID := range []string{requestID, "10000000-0000-4000-8000-000000000002"} {
		hash := sha256.Sum256([]byte(secret))
		duplicate := f.call(t, http.MethodPost, "/salcara-hub/v1/device/enrollment/challenge", map[string]any{
			"version": 1, "issuer": "station-a", "apiBase": enrollmentFixtureOrigin, "deviceId": id,
			"deviceSecretHash": hex.EncodeToString(hash[:]), "requestId": nextRequestID}, nil)
		assertFixtureFailure(t, duplicate, http.StatusConflict, "enrollment_request_conflict")
	}
	if counts := f.counts(); counts.Verifier != 1 || counts.Issuer != 1 || counts.Register != 2 {
		t.Fatalf("recovery repeated API authorization: %+v", counts)
	}
}

func TestHubIssuerEnrollmentIntegrationRejectsInvalidCredentialAndPin(t *testing.T) {
	for _, tc := range []struct {
		name          string
		wrongPin      bool
		key           string
		status        int
		code          string
		verifications int64
	}{
		{"invalid-api", false, "synthetic-invalid-enrollment-key", http.StatusUnauthorized, "api_credentials_invalid", 1},
		{"wrong-hub-pin", true, enrollmentFixtureKey, http.StatusForbidden, "enrollment_grant_invalid", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHubEnrollmentFixture(t, hubEnrollmentFixtureOptions{wrongHubPin: tc.wrongPin})
			secret := strings.Repeat("b", 64)
			env, _ := f.challenge(t, "rejected-pc", secret, "20000000-0000-4000-8000-000000000001")
			assertFixtureFailure(t, f.issue(t, env, tc.key), tc.status, tc.code)
			if counts := f.counts(); counts.Verifier != tc.verifications || counts.Register != 0 {
				t.Fatalf("rejected enrollment contacted unexpected endpoint: %+v", counts)
			}
			policy := f.call(t, http.MethodGet, "/salcara-hub/v1/discovery/policy", nil,
				http.Header{"X-Salcara-Device-Id": {"rejected-pc"}, "X-Salcara-Device-Secret": {secret}})
			assertFixtureFailure(t, policy, http.StatusForbidden, "device_credentials_invalid")
		})
	}
}

func TestHubIssuerEnrollmentIntegrationRefusesVerifierRedirect(t *testing.T) {
	var redirected atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	f := newHubEnrollmentFixture(t, hubEnrollmentFixtureOptions{verifierRedirect: target.URL})
	env, _ := f.challenge(t, "redirect-pc", strings.Repeat("c", 64), "30000000-0000-4000-8000-000000000001")
	assertFixtureFailure(t, f.issue(t, env, enrollmentFixtureKey), http.StatusServiceUnavailable, "api_verifier_unavailable")
	if redirected.Load() != 0 || f.counts().Register != 0 || f.counts().Verifier != 1 {
		t.Fatal("redirect target or registration was reached after verifier redirect")
	}
}

// The optional executable is built from Desktop's external overlay test. No
// real key, user configuration or production trust exception enters this run.
func TestHubEnrollmentDesktopConsumerIntegration(t *testing.T) {
	binary := os.Getenv("SALCARA_TEST_DESKTOP_BINARY")
	if binary == "" {
		t.Skip("SALCARA_TEST_DESKTOP_BINARY is not set; real Desktop consumer fixture was not requested")
	}
	f := newHubEnrollmentFixture(t, hubEnrollmentFixtureOptions{})
	work := t.TempDir()
	certPath := filepath.Join(work, "fixture-ca.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run", "^TestRealHubIssuerEnrollmentOverlay$", "-test.v")
	cmd.Dir = work
	cmd.Env = []string{
		"SALCARA_ENROLLMENT_FIXTURE_URL=" + f.server.URL,
		"SALCARA_ENROLLMENT_FIXTURE_ORIGIN=" + enrollmentFixtureOrigin,
		"SALCARA_ENROLLMENT_FIXTURE_CA=" + certPath,
		"TEMP=" + work, "TMP=" + work,
	}
	if systemRoot := os.Getenv("SystemRoot"); systemRoot != "" {
		cmd.Env = append(cmd.Env, "SystemRoot="+systemRoot)
	}
	output, err := cmd.CombinedOutput()
	t.Logf("Desktop consumer output:\n%s", output)
	if err != nil {
		t.Fatalf("real Desktop consumer failed: %v", err)
	}
	if counts := f.counts(); counts.Challenge < 1 || counts.Issuer < 1 || counts.Register < 1 || counts.Verifier < 1 {
		t.Fatalf("Desktop test did not exercise the real enrollment chain: %+v", counts)
	} else {
		t.Logf("Actual Hub/issuer fixture counts: %+v", counts)
	}
}
