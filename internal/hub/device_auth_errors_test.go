package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func assertRemoteCredentialError(t *testing.T, w *httptest.ResponseRecorder, status int, code, message string) {
	t.Helper()
	var body map[string]string
	if w.Code != status || json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body) != 2 || body["code"] != code || body["error"] != message {
		t.Fatalf("remote error: status=%d body=%s; want %d/%s/%s", w.Code, w.Body, status, code, message)
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Set-Cookie") != "" {
		t.Fatal("credential failure was cacheable or created a session")
	}
}

func TestDeviceAuthErrorsStayRemoteAndNeverCheckModelKey(t *testing.T) {
	transport := &rejectingRoundTripper{}
	h := newDeviceHub(t, Config{Sub2APIURL: "http://unused.invalid", HTTPClient: &http.Client{Transport: transport}, AuthFailLimit: 100})
	secret := strings.Repeat("a", 64)
	enrollDevice(t, h, "enrolled-pc", secret)
	for _, tc := range []struct {
		name, path, code string
		headers          map[string]string
	}{
		{"old-device", "/bridge/pair/status", "device_credentials_invalid", map[string]string{"X-Salcara-Device-Id": "old-pc", "X-Salcara-Device-Secret": secret}},
		{"wrong-secret", "/bridge/pair/status", "device_credentials_invalid", map[string]string{"X-Salcara-Device-Id": "enrolled-pc", "X-Salcara-Device-Secret": strings.Repeat("b", 64)}},
		{"missing-secret", "/bridge/pair/status", "device_credentials_invalid", map[string]string{"X-Salcara-Device-Id": "enrolled-pc"}},
		{"old-phone", "/app/devices", "pair_credentials_invalid", map[string]string{"X-Salcara-Pair-Token": strings.Repeat("c", 64)}},
		{"old-bot", "/app/devices", "bot_credentials_invalid", map[string]string{"X-Salcara-Bot-Token": strings.Repeat("d", 64)}},
		{"old-bot-alone", "/app/devices", "bot_credentials_invalid", map[string]string{"X-Salcara-Bot-Token": strings.Repeat("d", 64)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/salcara-hub/v1"+tc.path, nil)
			if tc.name != "old-bot-alone" {
				r.Header.Set("Authorization", "Bearer synthetic-model-key-must-not-be-used")
			}
			for name, value := range tc.headers {
				r.Header.Set(name, value)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			switch tc.name {
			case "old-bot":
				assertRemoteCredentialError(t, w, http.StatusBadRequest, "ambiguous_credentials", "请只使用一组 Bot 手机凭据")
			case "old-bot-alone":
				assertRemoteCredentialError(t, w, http.StatusForbidden, tc.code, "Bot 手机凭据已失效")
			default:
				assertRemoteCredentialError(t, w, http.StatusForbidden, tc.code, "设备凭证或手机绑定已失效，请重新扫码绑定")
			}
		})
	}
	if transport.calls.Load() != 0 {
		t.Fatal("remote credential rejection contacted the model API")
	}
	if len(h.accounts) != 1 || len(h.pending) != 0 || len(h.sourceChallenges) != 0 {
		t.Fatal("credential rejection allocated remote state")
	}
	if w := deviceRequest(t, h, "/bridge/pair/status", "enrolled-pc", secret, "", nil, ""); w.Code != http.StatusOK {
		t.Fatalf("invalid requests damaged valid device credentials: %d %s", w.Code, w.Body)
	}
}

func TestIncompleteDeviceCredentialsNeverFallBackToModelAuthentication(t *testing.T) {
	secret := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name          string
		disableLegacy bool
		headers       http.Header
	}{
		{name: "secret-only", headers: http.Header{"X-Salcara-Device-Secret": {secret}}},
		{name: "secret-without-bearer", headers: http.Header{"X-Salcara-Device-Secret": {secret}, "Authorization": {"Basic synthetic"}}},
		{name: "legacy-disabled", disableLegacy: true, headers: http.Header{"X-Salcara-Device-Secret": {secret}, "Authorization": {"Bearer synthetic"}}},
		{name: "hidden-device-id", headers: http.Header{"X-Salcara-Device-Id": {"", "stale-pc"}, "X-Salcara-Device-Secret": {secret}, "Authorization": {"Bearer synthetic"}}},
		{name: "duplicate-secret", headers: http.Header{"X-Salcara-Device-Secret": {"", secret}, "Authorization": {"Bearer synthetic"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := &rejectingRoundTripper{}
			h := newDeviceHub(t, Config{DisableLegacyAPIKey: tc.disableLegacy, Sub2APIURL: "http://unused.invalid", HTTPClient: &http.Client{Transport: transport}})
			r := httptest.NewRequest(http.MethodGet, "/salcara-hub/v1/bridge/pair/status", nil)
			r.Header = tc.headers.Clone()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			assertRemoteCredentialError(t, w, http.StatusForbidden, "device_credentials_invalid", "设备凭证或手机绑定已失效，请重新扫码绑定")
			if transport.calls.Load() != 0 {
				t.Fatal("incomplete remote credentials contacted the model API")
			}
		})
	}
}

func TestExplicitLegacyBearerKeepsDeviceSecretAsSecondaryProof(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer synthetic-legacy-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"group_id":7}`))
	}))
	defer upstream.Close()
	h := newDeviceHub(t, Config{Sub2APIURL: upstream.URL, AllowedGroupIDs: []int64{7}})
	secret := strings.Repeat("a", 64)
	info := Device{DeviceID: "legacy-proof-pc"}
	for _, suppliedSecret := range []string{secret, secret, strings.Repeat("b", 64)} {
		w := requestHub(t, h, http.MethodPost, "/bridge/register", "synthetic-legacy-key", suppliedSecret, "", info)
		want := http.StatusOK
		if suppliedSecret != secret {
			want = http.StatusForbidden
		}
		if w.Code != want {
			t.Fatalf("legacy secret validation: %d %s; want %d", w.Code, w.Body, want)
		}
	}
	if calls.Load() != 1 || len(h.accounts) != 1 || h.accounts[AccountID("synthetic-legacy-key")] == nil || h.accounts[deviceAccount(info.DeviceID)] != nil {
		t.Fatal("legacy proof changed the account namespace or bypassed cached key authentication")
	}
}

func TestDeviceEnrollmentErrorsDistinguishMissingInvalidAndConsumedClaim(t *testing.T) {
	h := newDeviceHub(t, Config{AuthFailLimit: 100})
	info := Device{DeviceID: "claim-error-pc"}
	secret := strings.Repeat("a", 64)
	for _, malformedSecret := range []string{"", "not-a-device-secret"} {
		w := deviceRequestWithoutEnrollmentClaim(t, h, "/device/register", info.DeviceID, malformedSecret, info)
		assertRemoteCredentialError(t, w, http.StatusForbidden, "device_credentials_invalid", "缺少独立设备凭证")
	}
	w := deviceRequestWithoutEnrollmentClaim(t, h, "/device/register", info.DeviceID, secret, info)
	assertRemoteCredentialError(t, w, http.StatusForbidden, "device_enrollment_required", "首次登记需要有效的一次性设备登记码")
	expired := strings.Repeat("b", 64)
	expiredHash, _ := enrollmentClaimHash(expired)
	h.mu.Lock()
	h.enrollmentClaims[expiredHash] = time.Now().Add(-time.Minute)
	h.mu.Unlock()
	for _, claim := range []string{"not-an-enrollment-claim", strings.Repeat("c", 64), expired} {
		w = deviceRequestWithEnrollmentClaim(t, h, "/device/register", info.DeviceID, secret, "", info, "", claim, false)
		assertRemoteCredentialError(t, w, http.StatusForbidden, "enrollment_claim_invalid", "首次登记需要有效的一次性设备登记码")
	}
	if len(h.accounts) != 0 || h.deviceMetadataBytes != 0 {
		t.Fatal("failed enrollment allocated a device")
	}
	claim := testEnrollmentClaim(t, h)
	w = deviceRequestWithEnrollmentClaim(t, h, "/device/register", info.DeviceID, secret, "", info, "", claim, false)
	if w.Code != http.StatusOK {
		t.Fatalf("valid enrollment failed: %d %s", w.Code, w.Body)
	}
	replayInfo := Device{DeviceID: "claim-replay-error-pc"}
	w = deviceRequestWithEnrollmentClaim(t, h, "/device/register", replayInfo.DeviceID, strings.Repeat("d", 64), "", replayInfo, "", claim, false)
	assertRemoteCredentialError(t, w, http.StatusForbidden, "enrollment_claim_invalid", "首次登记需要有效的一次性设备登记码")
	w = deviceRequestWithoutEnrollmentClaim(t, h, "/device/register", info.DeviceID, strings.Repeat("e", 64), info)
	assertRemoteCredentialError(t, w, http.StatusForbidden, "device_credentials_invalid", "设备身份验证失败")
	if w = deviceRequestWithoutEnrollmentClaim(t, h, "/device/register", info.DeviceID, secret, info); w.Code != http.StatusOK || len(h.accounts) != 1 {
		t.Fatalf("existing reconnect required another claim: %d %s", w.Code, w.Body)
	}
}

func TestDeviceAuthRateLimitCodePreservesValidSharedAddressAccess(t *testing.T) {
	h := newDeviceHub(t, Config{AuthFailLimit: 2})
	secret := strings.Repeat("a", 64)
	enrollDevice(t, h, "shared-error-pc", secret)
	onlineDevice(t, h, "shared-error-pc")
	phone := claimQR(t, h, "shared-error-pc", issueQR(t, h, "shared-error-pc", secret))
	for i := 0; i < 2; i++ {
		w := deviceRequest(t, h, "/app/devices", "", "", strings.Repeat("f", 64), nil, "")
		assertRemoteCredentialError(t, w, http.StatusForbidden, "pair_credentials_invalid", "设备凭证或手机绑定已失效，请重新扫码绑定")
	}
	w := deviceRequest(t, h, "/app/devices", "", "", strings.Repeat("f", 64), nil, "")
	assertRemoteCredentialError(t, w, http.StatusTooManyRequests, "remote_rate_limited", "身份验证失败次数过多")
	w = deviceRequestWithoutEnrollmentClaim(t, h, "/device/register", "new-error-pc", strings.Repeat("b", 64), Device{DeviceID: "new-error-pc"})
	assertRemoteCredentialError(t, w, http.StatusTooManyRequests, "remote_rate_limited", "身份验证失败次数过多，请稍后重试")
	if w = deviceRequest(t, h, "/app/devices", "", "", phone, nil, ""); w.Code != http.StatusOK {
		t.Fatal("rate limit locked out a valid phone")
	}
	if w = deviceRequestWithoutEnrollmentClaim(t, h, "/device/register", "shared-error-pc", secret, Device{DeviceID: "shared-error-pc"}); w.Code != http.StatusOK {
		t.Fatal("rate limit locked out an enrolled computer")
	}
}

func TestDeviceCapacityErrorDoesNotConsumeEnrollmentClaim(t *testing.T) {
	h := newDeviceHub(t, Config{})
	claim := testEnrollmentClaim(t, h)
	claimHash, _ := enrollmentClaimHash(claim)
	h.deviceMetadataLimit = 1
	info := Device{DeviceID: "capacity-error-pc"}
	w := deviceRequestWithEnrollmentClaim(t, h, "/device/register", info.DeviceID, strings.Repeat("a", 64), "", info, "", claim, false)
	assertRemoteCredentialError(t, w, http.StatusTooManyRequests, "device_capacity_exceeded", "本站设备容量已满或设备身份不可用")
	h.mu.Lock()
	validClaim := h.enrollmentClaimValidLocked(claimHash)
	h.mu.Unlock()
	if !validClaim || len(h.accounts) != 0 || h.deviceMetadataBytes != 0 {
		t.Fatal("capacity rejection consumed the claim or allocated a device")
	}
}

func TestDeviceEnrollmentAndQRRateLimitErrorsStaySeparateFromCapacity(t *testing.T) {
	h := newDeviceHub(t, Config{AuthFailLimit: 1})
	h.enrollLimiter = newFailLimiter(1, time.Minute)
	h.enrollLimiter.fail("192.0.2.1")
	info := Device{DeviceID: "limited-enrollment-pc"}
	w := deviceRequestWithoutEnrollmentClaim(t, h, "/device/register", info.DeviceID, strings.Repeat("a", 64), info)
	assertRemoteCredentialError(t, w, http.StatusTooManyRequests, "remote_rate_limited", "设备登记过于频繁或达到本站容量限制，请稍后重试")
	h.limiter.fail("192.0.2.1")
	w = deviceRequest(t, h, "/app/pair/qr", "", "", "", map[string]string{"deviceId": info.DeviceID, "ticket": strings.Repeat("b", 64)}, "")
	assertRemoteCredentialError(t, w, http.StatusTooManyRequests, "remote_rate_limited", "配对失败次数过多，请稍后再试")
	if len(h.accounts) != 0 || len(h.pairCodes) != 0 || len(h.pairTickets) != 0 {
		t.Fatal("rate-limited enrollment or QR allocated state")
	}
}

func TestBotRegistrationErrorsNeverBecomeModelKeyFailures(t *testing.T) {
	h := newDeviceHub(t, Config{DeploymentMode: "bot", PublicURL: "https://bot.example/salcara-hub"})
	info := Device{DeviceID: "bot-error-pc"}
	secret := strings.Repeat("a", 64)
	w := deviceRequestWithoutEnrollmentClaim(t, h, "/device/register", info.DeviceID, secret, info)
	assertRemoteCredentialError(t, w, http.StatusForbidden, "bot_credentials_invalid", "Bot 首次登记需要 Salcara Hub 签发的绑定包")
	w = botRequest(t, h, http.MethodPost, "/v1/device/register", info, map[string]string{
		"X-Salcara-Device-Id": info.DeviceID, "X-Salcara-Device-Secret": secret, "X-Salcara-Bot-Invite": "invalid-invite",
	})
	assertRemoteCredentialError(t, w, http.StatusForbidden, "bot_credentials_invalid", "Bot 绑定包无效")
	if len(h.accounts) != 0 {
		t.Fatal("invalid Bot credentials allocated a device")
	}
}

func TestDeviceAuthErrorsRetainDisabledUserContract(t *testing.T) {
	h := newDeviceHub(t, Config{DataDir: t.TempDir()})
	managedChange(t, h, ManagedUserChange{Action: "save", Name: "停用错误分类测试"})
	user := h.managedUsers.Users[0]
	claim, _, err := h.CreateDeviceEnrollmentClaimForUser(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	info := Device{DeviceID: "disabled-error-pc"}
	secret := strings.Repeat("a", 64)
	if w := deviceRequestWithEnrollmentClaim(t, h, "/device/register", info.DeviceID, secret, "", info, "", claim, false); w.Code != http.StatusOK {
		t.Fatalf("enrollment: %d %s", w.Code, w.Body)
	}
	onlineDevice(t, h, info.DeviceID)
	phone := claimQR(t, h, info.DeviceID, issueQR(t, h, info.DeviceID, secret))
	managedChange(t, h, ManagedUserChange{Action: "save", ID: user.ID, Name: user.Name, Disabled: true})
	w := deviceRequest(t, h, "/app/devices", "", "", phone, nil, "")
	var out struct {
		Code      string `json:"code"`
		Retryable bool   `json:"retryable"`
	}
	if w.Code != http.StatusLocked || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Code != "user_disabled" || out.Retryable {
		t.Fatalf("disabled user lost its recovery contract: %d %s", w.Code, w.Body)
	}
	h.mu.Lock()
	paired := h.accounts[deviceAccount(info.DeviceID)].devices[info.DeviceID].hasPair
	h.mu.Unlock()
	if !paired {
		t.Fatal("disabled user error deleted the phone pairing")
	}
}
