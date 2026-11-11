package sourceissuer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"salcara/hub/internal/enrollment"
)

func enrollmentConfig(verifier string, hubKey ed25519.PublicKey) Config {
	cfg := fixtureConfig(verifier)
	cfg.EnrollmentURL = "https://station.example/device-enrollment"
	cfg.EnrollmentHubs = []EnrollmentHub{{Audience: "hub-a", HubInstanceID: strings.Repeat("1", 32), IdentityPublicKey: base64.StdEncoding.EncodeToString(hubKey)}}
	cfg.Routes[0].EnrollmentVerifierURL = verifier + "/v1/salcara-hub/enrollment-identity"
	cfg.Routes[0].Groups[0].AllowDeviceEnrollment = true
	return cfg
}

func enrollmentFixture(t *testing.T, handler http.Handler) (*Service, ed25519.PrivateKey) {
	t.Helper()
	verifier := httptest.NewServer(handler)
	t.Cleanup(verifier.Close)
	hubKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{23}, ed25519.SeedSize))
	issuerKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{37}, ed25519.SeedSize))
	s, err := New(enrollmentConfig(verifier.URL, hubKey.Public().(ed25519.PublicKey)), issuerKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, hubKey
}

func enrollmentChallenge() enrollment.Challenge {
	now := time.Now().Unix()
	return enrollment.Challenge{Purpose: enrollment.ChallengePurpose, Version: 1, Issuer: "station-a", HubInstanceID: strings.Repeat("1", 32), Audience: "hub-a", APIBase: "https://station.example/tenant-a", PolicyRevision: "policy-a", DeviceID: "device-a", DeviceSecretHash: strings.Repeat("a", 64), ChallengeID: "challenge-a", RequestID: "10000000-0000-4000-8000-000000000001", IssuedAt: now, ExpiresAt: now + 40}
}

func enrollmentBody(t *testing.T, challenge enrollment.Challenge, hubKey ed25519.PrivateKey) []byte {
	t.Helper()
	proof, err := enrollment.SignEnvelope(challenge, enrollment.ChallengePurpose, hubKey)
	if err != nil {
		t.Fatal(err)
	}
	return enrollmentEnvelopeBody(t, proof)
}

func enrollmentEnvelopeBody(t *testing.T, proof enrollment.Envelope) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"version": 1, "challenge": proof, "credential": map[string]string{"type": "api_key", "value": "synthetic-enrollment-key"}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func callEnrollment(s *Service, body []byte) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "https://station.example/device-enrollment", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(w, r)
	return w
}

func assertEnrollmentFailure(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var result struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(w.Body.Bytes(), &result) != nil || w.Code != status || result.Code != code || strings.Contains(w.Body.String(), "synthetic") {
		t.Fatalf("status=%d body=%s, want=%d/%s", w.Code, w.Body, status, code)
	}
}

func TestEnrollmentGrantVerifiesHubAndAuthoritativeIdentity(t *testing.T) {
	var enrollmentChecks, commandChecks atomic.Int32
	s, hubKey := enrollmentFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Accept") != "application/json" || r.Header.Get("Authorization") != "Bearer synthetic-enrollment-key" || r.Header.Get("X-Salcara-Device-Id") != "" {
			t.Error("incorrect verifier request")
		}
		switch r.URL.Path {
		case "/v1/salcara-hub/key":
			commandChecks.Add(1)
			_, _ = io.WriteString(w, `{"group_id":7}`)
		case "/v1/salcara-hub/enrollment-identity":
			enrollmentChecks.Add(1)
			_, _ = io.WriteString(w, `{"version":1,"subject":"sub2api-user:42","groupId":7}`)
		default:
			t.Error("unexpected verifier endpoint")
		}
	}))
	expected := enrollmentChallenge()
	w := callEnrollment(s, enrollmentBody(t, expected, hubKey))
	if w.Code != http.StatusOK || enrollmentChecks.Load() != 1 || commandChecks.Load() != 0 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("grant status=%d body=%s checks=%d", w.Code, w.Body, enrollmentChecks.Load())
	}
	proof, err := enrollment.ParseEnvelope(w.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := enrollment.VerifyEnvelope(proof, enrollment.GrantPurpose, s.key.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	grant, err := enrollment.ParseGrant(raw)
	if err != nil {
		t.Fatal(err)
	}
	if grant.Subject != "sub2api-user:42" || grant.GroupID != 7 || grant.ExpiresAt > expected.ExpiresAt || !enrollment.ValidTime(grant.IssuedAt, grant.ExpiresAt, time.Now().Unix()) || bytes.Contains(raw, []byte("synthetic-enrollment-key")) {
		t.Fatal("incorrect identity, TTL, or secret disclosure")
	}
	grant.Purpose, grant.IssuedAt, grant.ExpiresAt = expected.Purpose, expected.IssuedAt, expected.ExpiresAt
	if grant.Challenge != expected {
		t.Fatal("changed immutable challenge fields")
	}
	if _, err := enrollment.VerifyEnvelope(proof, enrollment.ChallengePurpose, s.key.Public().(ed25519.PublicKey)); err == nil {
		t.Fatal("grant replayed as Hub challenge")
	}
	if w := call(s, requestBody(fixtureSnapshot(), "synthetic-enrollment-key")); w.Code != 200 || commandChecks.Load() != 1 {
		t.Fatal("old command endpoint changed")
	}
	public := s.PublicIssuer()
	if !public.DeviceEnrollment || public.EnrollmentURL != s.cfg.EnrollmentURL {
		t.Fatal("enabled enrollment missing from public policy")
	}
}

func TestEnrollmentRejectsUntrustedChallengesBeforeCredentialUse(t *testing.T) {
	var checks atomic.Int32
	s, hubKey := enrollmentFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { checks.Add(1) }))
	cases := []struct {
		name   string
		change func(*enrollment.Challenge)
		code   string
	}{
		{"issuer", func(v *enrollment.Challenge) { v.Issuer = "station-b" }, "enrollment_issuer_untrusted"},
		{"instance", func(v *enrollment.Challenge) { v.HubInstanceID = strings.Repeat("2", 32) }, "enrollment_issuer_untrusted"},
		{"audience", func(v *enrollment.Challenge) { v.Audience = "hub-b" }, "enrollment_issuer_untrusted"},
		{"tenant", func(v *enrollment.Challenge) { v.APIBase = "https://station.example/tenant-b" }, "api_enrollment_denied"},
		{"foreign-api", func(v *enrollment.Challenge) { v.APIBase = "https://foreign.example" }, "api_enrollment_denied"},
		{"expired", func(v *enrollment.Challenge) { v.IssuedAt = time.Now().Unix() - 121; v.ExpiresAt = v.IssuedAt + 120 }, "enrollment_challenge_expired"},
		{"future", func(v *enrollment.Challenge) { v.IssuedAt = time.Now().Unix() + 60; v.ExpiresAt = v.IssuedAt + 40 }, "enrollment_challenge_expired"},
		{"purpose", func(v *enrollment.Challenge) { v.Purpose = enrollment.GrantPurpose }, "enrollment_grant_invalid"},
		{"hash", func(v *enrollment.Challenge) { v.DeviceSecretHash = strings.Repeat("A", 64) }, "enrollment_grant_invalid"},
		{"request-id", func(v *enrollment.Challenge) { v.RequestID = "10000000-0000-1000-8000-000000000001" }, "enrollment_grant_invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := enrollmentChallenge()
			tc.change(&v)
			assertEnrollmentFailure(t, callEnrollment(s, enrollmentBody(t, v, hubKey)), 403, tc.code)
		})
	}
	foreignKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{51}, ed25519.SeedSize))
	assertEnrollmentFailure(t, callEnrollment(s, enrollmentBody(t, enrollmentChallenge(), foreignKey)), 403, "enrollment_grant_invalid")
	if checks.Load() != 0 {
		t.Fatal("untrusted challenge disclosed API credential")
	}
}

func TestEnrollmentRequiresIndependentGroupOptIn(t *testing.T) {
	var groupID atomic.Int64
	groupID.Store(8)
	s, key := enrollmentFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"version":1,"subject":"sub2api-user:42","groupId":%d}`, groupID.Load())
	}))
	body := enrollmentBody(t, enrollmentChallenge(), key)
	assertEnrollmentFailure(t, callEnrollment(s, body), 403, "api_enrollment_denied")
	groupID.Store(7)
	if w := callEnrollment(s, body); w.Code != 200 {
		t.Fatal("allowed group denied", w.Code)
	}
	s.cfg.Routes[0].Groups[0].AllowDeviceEnrollment = false
	assertEnrollmentFailure(t, callEnrollment(s, body), 403, "api_enrollment_denied")
	if s.PublicIssuer().DeviceEnrollment || s.PublicIssuer().EnrollmentURL != "" {
		t.Fatal("disabled registration advertised")
	}
}

func TestEnrollmentVerifierStrictResponsesRevocationAndNoRedirects(t *testing.T) {
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetRequests.Add(1) }))
	defer target.Close()
	cases := []struct {
		name, body   string
		status, want int
		code         string
	}{
		{"revoked", "synthetic-private-error", 401, 401, "api_credentials_invalid"},
		{"expired", "synthetic-private-error", 403, 401, "api_credentials_invalid"},
		{"upstream", "synthetic-private-error", 500, 503, "api_verifier_unavailable"},
		{"busy", "synthetic-private-error", 429, 503, "api_verifier_unavailable"},
		{"old-identity", `{"group_id":7}`, 200, 503, "api_verifier_unavailable"},
		{"duplicate", `{"version":1,"subject":"sub2api-user:42","groupId":7,"groupId":8}`, 200, 503, "api_verifier_unavailable"},
		{"case", `{"version":1,"subject":"sub2api-user:42","groupId":7,"GroupId":8}`, 200, 503, "api_verifier_unavailable"},
		{"null", `{"version":1,"subject":null,"groupId":7}`, 200, 503, "api_verifier_unavailable"},
		{"unknown", `{"version":1,"subject":"sub2api-user:42","groupId":7,"token":"synthetic"}`, 200, 503, "api_verifier_unavailable"},
		{"trailing", `{"version":1,"subject":"sub2api-user:42","groupId":7} {}`, 200, 503, "api_verifier_unavailable"},
		{"version", `{"version":2,"subject":"sub2api-user:42","groupId":7}`, 200, 503, "api_verifier_unavailable"},
		{"subject", `{"version":1,"subject":"sub2api-user:042","groupId":7}`, 200, 503, "api_verifier_unavailable"},
		{"oversize", strings.Repeat("x", 4097), 200, 503, "api_verifier_unavailable"},
	}
	for _, status := range []int{301, 302, 303, 307, 308} {
		cases = append(cases, struct {
			name, body   string
			status, want int
			code         string
		}{fmt.Sprint("redirect-", status), "", status, 503, "api_verifier_unavailable"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, key := enrollmentFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", target.URL)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			assertEnrollmentFailure(t, callEnrollment(s, enrollmentBody(t, enrollmentChallenge(), key)), tc.want, tc.code)
		})
	}
	if targetRequests.Load() != 0 {
		t.Fatal("API credential followed redirect")
	}
	var revoked atomic.Bool
	s, key := enrollmentFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if revoked.Load() {
			w.WriteHeader(401)
			return
		}
		_, _ = io.WriteString(w, `{"version":1,"subject":"sub2api-user:42","groupId":7}`)
	}))
	body := enrollmentBody(t, enrollmentChallenge(), key)
	if w := callEnrollment(s, body); w.Code != 200 {
		t.Fatal(w.Code)
	}
	revoked.Store(true)
	assertEnrollmentFailure(t, callEnrollment(s, body), 401, "api_credentials_invalid")
}

func TestEnrollmentMalformedRequestsNeverUseCredential(t *testing.T) {
	var checks atomic.Int32
	s, key := enrollmentFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { checks.Add(1) }))
	valid := string(enrollmentBody(t, enrollmentChallenge(), key))
	for _, raw := range []string{
		strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(valid, `"type":"api_key"`, `"Type":"api_key"`, 1),
		strings.Replace(valid, `"value":"synthetic-enrollment-key"`, `"value":null`, 1),
		strings.Replace(valid, `"challenge":{`, `"challenge":{"Payload":"fake",`, 1),
		strings.Replace(valid, `"credential":{`, `"subject":"sub2api-user:1","credential":{`, 1),
		valid + " {}", strings.Repeat("x", (48<<10)+1),
	} {
		if w := callEnrollment(s, []byte(raw)); w.Code == 200 {
			t.Fatal("malformed request accepted")
		}
	}
	proof, _ := enrollment.SignEnvelope(enrollmentChallenge(), enrollment.ChallengePurpose, key)
	payload, _ := base64.RawURLEncoding.DecodeString(proof.Payload)
	for _, suffix := range []string{`,"subject":"sub2api-user:1"}`, `,"DeviceId":"another"}`, `,"deviceId":"another"}`} {
		ambiguous := strings.TrimSuffix(string(payload), "}") + suffix
		proof.Payload = base64.RawURLEncoding.EncodeToString([]byte(ambiguous))
		proof.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, append([]byte(enrollment.ChallengePurpose+"\n"), []byte(ambiguous)...)))
		assertEnrollmentFailure(t, callEnrollment(s, enrollmentEnvelopeBody(t, proof)), 403, "enrollment_grant_invalid")
	}
	if checks.Load() != 0 {
		t.Fatal("malformed enrollment disclosed credential")
	}
}

func TestEnrollmentConcurrentCancellationAndCapacity(t *testing.T) {
	s, key := enrollmentFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"version":1,"subject":"sub2api-user:42","groupId":7}`)
	}))
	body := enrollmentBody(t, enrollmentChallenge(), key)
	var wg sync.WaitGroup
	for range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := callEnrollment(s, body); w.Code != 200 {
				t.Errorf("concurrent enrollment %d", w.Code)
			}
		}()
	}
	wg.Wait()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodPost, "https://station.example/device-enrollment", bytes.NewReader(body)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	assertEnrollmentFailure(t, w, 503, "api_verifier_unavailable")
	for range cap(s.capacity) {
		s.capacity <- struct{}{}
	}
	if w := callEnrollment(s, body); w.Code != 429 {
		t.Fatal("shared capacity not enforced")
	}
	for len(s.capacity) > 0 {
		<-s.capacity
	}
	for range 120 {
		s.allow("synthetic-enrollment-key", time.Now())
	}
	assertEnrollmentFailure(t, callEnrollment(s, body), 429, "issuer_rate_limited")
}
