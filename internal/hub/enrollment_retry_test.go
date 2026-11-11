package hub

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"salcara/hub/internal/enrollment"
)

const retryAPIBase = "https://station.example.test"

func enrollmentRetryFixture(t *testing.T) (*Hub, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	policy := RemoteAccessPolicy{
		Mode: "restricted", Revision: "retry-policy-1", Audience: "retry-hub",
		Station: &StationPolicy{Version: 2, APIBases: []string{retryAPIBase}},
		Issuers: []SourceIssuer{{ID: "retry-issuer", PublicKey: base64.RawURLEncoding.EncodeToString(public),
			SourceTypes: []string{"site_api"}, Origins: []string{retryAPIBase}, APIBases: []string{retryAPIBase},
			DeviceEnrollment: true, EnrollmentURL: retryAPIBase + "/device-enrollment"}},
	}
	h := newDeviceHub(t, Config{DataDir: t.TempDir(), PublicURL: retryAPIBase + "/salcara-hub", DeploymentMode: "station", RemoteAccess: policy})
	if h.apiEnrollmentDiscovery() == nil {
		t.Fatal("fixture does not advertise API enrollment")
	}
	return h, private
}

func enrollmentRetryRequest(t *testing.T, id, secret, requestID string) *http.Request {
	t.Helper()
	hash := sha256.Sum256([]byte(secret))
	raw, err := json.Marshal(map[string]any{"version": 1, "issuer": "retry-issuer", "apiBase": retryAPIBase,
		"deviceId": id, "deviceSecretHash": hex.EncodeToString(hash[:]), "requestId": requestID})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/salcara-hub/v1/device/enrollment/challenge", bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func enrollmentRetryChallenge(t *testing.T, h *Hub, id, secret, requestID string) (enrollment.Challenge, *httptest.ResponseRecorder) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, enrollmentRetryRequest(t, id, secret, requestID))
	if w.Code != http.StatusOK {
		return enrollment.Challenge{}, w
	}
	env, err := enrollment.ParseEnvelope(w.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	key, err := base64.StdEncoding.Strict().DecodeString(h.IngressState().IdentityPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := enrollment.VerifyEnvelope(env, enrollment.ChallengePurpose, ed25519.PublicKey(key))
	if err != nil {
		t.Fatal(err)
	}
	c, err := enrollment.ParseChallenge(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !enrollment.ValidTime(c.IssuedAt, c.ExpiresAt, time.Now().Unix()) || c.DeviceID != id || c.RequestID != requestID || c.DeviceSecretHash != shaHex(secret) || c.HubInstanceID != h.IngressState().HubInstanceID || c.APIBase != retryAPIBase {
		t.Fatal("challenge did not bind the pending attempt")
	}
	return c, w
}

func enrollmentRetryGrantRequest(t *testing.T, c enrollment.Challenge, key ed25519.PrivateKey, secret, subject string) *http.Request {
	t.Helper()
	c.Purpose = enrollment.GrantPurpose
	env, err := enrollment.SignEnvelope(enrollment.Grant{Challenge: c, Subject: subject, GroupID: 17}, enrollment.GrantPurpose, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(Device{DeviceID: c.DeviceID, ComputerID: "physical-computer-1", Name: "Retry test"})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/salcara-hub/v1/device/register", bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Salcara-Device-Id", c.DeviceID)
	r.Header.Set("X-Salcara-Device-Secret", secret)
	r.Header.Set("X-Salcara-Enrollment-Grant", enrollmentGrantHeader(env))
	return r
}

func assertEnrollmentRetryCode(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var out struct {
		Code string `json:"code"`
	}
	if w.Code != status || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Code != code {
		t.Fatalf("got %d %s; want %d %s", w.Code, w.Body, status, code)
	}
	if strings.Contains(w.Body.String(), `"payload"`) || strings.Contains(w.Body.String(), `"signature"`) {
		t.Fatal("rejected attempt received a challenge")
	}
}

func TestAPIEnrollmentRetryKeepsUnknownPolicyForbidden(t *testing.T) {
	h, _ := enrollmentRetryFixture(t)
	secret := strings.Repeat("a", 64)
	headers := http.Header{"X-Salcara-Device-Id": {"missing-pc"}, "X-Salcara-Device-Secret": {secret}}
	w := discoveryRequest(h, "/salcara-hub/v1/discovery/policy", headers)
	assertEnrollmentRetryCode(t, w, http.StatusForbidden, "device_credentials_invalid")
	_, w = enrollmentRetryChallenge(t, h, "missing-pc", secret, "11111111-1111-4111-8111-111111111111")
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.accounts) != 0 || len(h.apiEnrollmentChallenges) != 1 {
		t.Fatal("challenge enrolled a device")
	}
}

func TestAPIEnrollmentChallengeRejectsExistingAndBlockedTargets(t *testing.T) {
	for _, kind := range []string{"matching-secret", "wrong-secret", "legacy-namespace", "pending-save", "banned", "ban-tombstone", "disabled", "disabled-without-device"} {
		t.Run(kind, func(t *testing.T) {
			h, _ := enrollmentRetryFixture(t)
			id, secret := "existing-pc", strings.Repeat("a", 64)
			status, code := http.StatusConflict, "enrollment_request_conflict"
			h.mu.Lock()
			if kind != "ban-tombstone" && kind != "disabled-without-device" {
				accountID := deviceAccount(id)
				if kind == "legacy-namespace" {
					accountID = "legacy-api-account"
				}
				_, status, _ := h.registerDeviceLocked(accountID, Device{DeviceID: id}, sha256.Sum256([]byte(secret)))
				if status != 0 {
					h.mu.Unlock()
					t.Fatal(status)
				}
				if kind != "pending-save" {
					h.accounts[accountID].devices[id].savedRegistrationEpoch = h.accounts[accountID].devices[id].registrationEpoch
				}
				if kind == "banned" {
					h.accounts[accountID].devices[id].banned = true
				}
			}
			if kind == "ban-tombstone" {
				h.bannedDeviceIDs[id] = true
			}
			if kind == "disabled" || kind == "disabled-without-device" {
				h.managedUsers.Users = []ManagedUser{{ID: "usr-disabled", Disabled: true}}
				h.managedUsers.Devices[id] = managedAssignment{UserID: "usr-disabled"}
				status, code = http.StatusLocked, "user_disabled"
			}
			if kind == "banned" || kind == "ban-tombstone" {
				status, code = http.StatusForbidden, "device_credentials_invalid"
			}
			h.mu.Unlock()
			if kind == "wrong-secret" {
				secret = strings.Repeat("b", 64)
			}
			_, w := enrollmentRetryChallenge(t, h, id, secret, "11111111-1111-4111-8111-111111111111")
			assertEnrollmentRetryCode(t, w, status, code)
			h.mu.Lock()
			defer h.mu.Unlock()
			if len(h.apiEnrollmentChallenges) != 0 {
				t.Fatal("blocked target consumed a challenge slot")
			}
		})
	}
}

type enrollmentReadSignal struct {
	io.ReadCloser
	once sync.Once
	read chan struct{}
}

func (b *enrollmentReadSignal) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.once.Do(func() { close(b.read) })
	}
	return n, err
}

func TestAPIEnrollmentChallengeWaitsForInFlightRegistration(t *testing.T) {
	h, _ := enrollmentRetryFixture(t)
	id, secret := "in-flight-pc", strings.Repeat("a", 64)
	r := enrollmentRetryRequest(t, id, secret, "11111111-1111-4111-8111-111111111111")
	body := &enrollmentReadSignal{ReadCloser: r.Body, read: make(chan struct{})}
	r.Body = body
	h.enrollmentMu.Lock()
	locked := true
	defer func() {
		if locked {
			h.enrollmentMu.Unlock()
		}
	}()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { w := httptest.NewRecorder(); h.ServeHTTP(w, r); done <- w }()
	select {
	case <-body.read:
	case <-time.After(3 * time.Second):
		t.Fatal("challenge body not read")
	}
	select {
	case w := <-done:
		t.Fatalf("challenge bypassed in-flight transaction: %d %s", w.Code, w.Body)
	case <-time.After(30 * time.Millisecond):
	}
	// The in-flight transaction now publishes the committed record before
	// releasing enrollmentMu. No challenge may observe its earlier RAM gap.
	h.mu.Lock()
	_, status, _ := h.registerDeviceLocked(deviceAccount(id), Device{DeviceID: id}, sha256.Sum256([]byte(secret)))
	h.mu.Unlock()
	h.enrollmentMu.Unlock()
	locked = false
	if status != 0 {
		t.Fatal(status)
	}
	select {
	case w := <-done:
		assertEnrollmentRetryCode(t, w, http.StatusConflict, "enrollment_request_conflict")
	case <-time.After(3 * time.Second):
		t.Fatal("challenge did not finish after registration")
	}
}

func TestAPIEnrollmentConcurrentAttemptsPreserveWinningIdentity(t *testing.T) {
	for _, different := range []bool{false, true} {
		name := "same-secret"
		if different {
			name = "different-secrets"
		}
		t.Run(name, func(t *testing.T) { testAPIEnrollmentConcurrentAttempts(t, different) })
	}
}

func testAPIEnrollmentConcurrentAttempts(t *testing.T, different bool) {
	h, key := enrollmentRetryFixture(t)
	id, secret := "racing-pc", strings.Repeat("a", 64)
	secrets := []string{secret, secret}
	if different {
		secrets[1] = strings.Repeat("b", 64)
	}
	requests := make([]*http.Request, 2)
	for i, requestID := range []string{"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"} {
		c, w := enrollmentRetryChallenge(t, h, id, secrets[i], requestID)
		if w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body)
		}
		requests[i] = enrollmentRetryGrantRequest(t, c, key, secrets[i], []string{"sub2api-user:31", "sub2api-user:32"}[i])
	}
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 2)
	for _, r := range requests {
		go func(r *http.Request) { <-start; w := httptest.NewRecorder(); h.ServeHTTP(w, r); results <- w }(r)
	}
	close(start)
	successes := 0
	for range requests {
		select {
		case w := <-results:
			if w.Code == http.StatusOK {
				successes++
			} else {
				assertEnrollmentRetryCode(t, w, http.StatusConflict, "enrollment_request_conflict")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent enrollment did not finish")
		}
	}
	if successes != 1 {
		t.Fatalf("got %d successful identities", successes)
	}
	h.mu.Lock()
	if len(h.accounts) != 1 || len(h.accounts[deviceAccount(id)].devices) != 1 {
		h.mu.Unlock()
		t.Fatal("duplicate device created")
	}
	stored := *h.accounts[deviceAccount(id)].devices[id].enrollment
	h.mu.Unlock()
	if stored.Subject != "sub2api-user:31" && stored.Subject != "sub2api-user:32" {
		t.Fatal("winner lost trusted subject")
	}
	if stored.Subject == "sub2api-user:32" {
		secret = secrets[1]
	}
	policy := discoveryRequest(h, "/salcara-hub/v1/discovery/policy", http.Header{"X-Salcara-Device-Id": {id}, "X-Salcara-Device-Secret": {secret}})
	if policy.Code != http.StatusOK {
		t.Fatal("successful enrollment cannot be confirmed", policy.Code, policy.Body)
	}
	// A cached pre-registration challenge must not be returned after commit.
	_, w := enrollmentRetryChallenge(t, h, id, secret, "11111111-1111-4111-8111-111111111111")
	assertEnrollmentRetryCode(t, w, http.StatusConflict, "enrollment_request_conflict")
	// Reload the committed devices file without creating another live server.
	restored := &Hub{cfg: h.cfg, accounts: map[string]*account{}, pairTokens: map[[32]byte]pairIdentity{}, bannedDeviceIDs: map[string]bool{}, deviceMetadataLimit: h.deviceMetadataLimit}
	if err := restored.loadDevices(); err != nil {
		t.Fatal(err)
	}
	if got := restored.accounts[deviceAccount(id)].devices[id].enrollment; got == nil || *got != stored {
		t.Fatal("durable winner differs from live identity")
	}
}

func TestAPIEnrollmentRechecksTargetAfterChallenge(t *testing.T) {
	for _, kind := range []string{"device-namespace", "legacy-namespace", "banned", "disabled"} {
		t.Run(kind, func(t *testing.T) {
			h, key := enrollmentRetryFixture(t)
			id, secret := "changed-pc", strings.Repeat("a", 64)
			c, w := enrollmentRetryChallenge(t, h, id, secret, "11111111-1111-4111-8111-111111111111")
			if w.Code != http.StatusOK {
				t.Fatal(w.Code, w.Body)
			}
			h.enrollmentMu.Lock()
			h.mu.Lock()
			status, code := http.StatusConflict, "enrollment_request_conflict"
			switch kind {
			case "banned":
				h.bannedDeviceIDs[id] = true
				status, code = http.StatusForbidden, "device_credentials_invalid"
			case "disabled":
				h.managedUsers.Users = []ManagedUser{{ID: "usr-disabled", Disabled: true}}
				h.managedUsers.Devices[id] = managedAssignment{UserID: "usr-disabled"}
				status, code = http.StatusLocked, "user_disabled"
			default:
				accountID := deviceAccount(id)
				if kind == "legacy-namespace" {
					accountID = "legacy-api-account"
				}
				_, admission, _ := h.registerDeviceLocked(accountID, Device{DeviceID: id}, sha256.Sum256([]byte(strings.Repeat("b", 64))))
				if admission != 0 {
					h.mu.Unlock()
					h.enrollmentMu.Unlock()
					t.Fatal(admission)
				}
			}
			h.mu.Unlock()
			h.enrollmentMu.Unlock()
			w = httptest.NewRecorder()
			h.ServeHTTP(w, enrollmentRetryGrantRequest(t, c, key, secret, "sub2api-user:31"))
			assertEnrollmentRetryCode(t, w, status, code)
			h.mu.Lock()
			defer h.mu.Unlock()
			for _, a := range h.accounts {
				for _, d := range a.devices {
					if d.enrollment != nil {
						t.Fatal("stale challenge replaced identity")
					}
				}
			}
		})
	}
}
