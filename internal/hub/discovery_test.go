package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func discoveryRequest(h *Hub, path string, headers http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header = headers.Clone()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func discoveryRemotePolicy(t *testing.T, w *httptest.ResponseRecorder, scope string) RemoteAccessPolicy {
	t.Helper()
	var body struct {
		OK           bool
		Scope        string
		RemoteAccess RemoteAccessPolicy
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || !body.OK || body.Scope != scope {
		t.Fatalf("policy response: %d %s", w.Code, w.Body)
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Set-Cookie") != "" {
		t.Fatal("policy created a browser session or was cacheable")
	}
	return body.RemoteAccess
}

func TestDiscoveryIgnoresAllCredentialsWithoutModelAuthentication(t *testing.T) {
	transport := &rejectingRoundTripper{}
	h := newDeviceHub(t, Config{DataDir: t.TempDir(), PublicURL: "https://hub.example/salcara-hub", HTTPClient: &http.Client{Transport: transport}, Sub2APIURL: "https://unused.invalid", AuthFailLimit: 1})
	managedChange(t, h, ManagedUserChange{Action: "save", Name: "private-policy", Station: &StationPolicy{Version: 2, ExternalAPIBases: []string{"https://private.example/team"}}})
	claim, _, err := h.CreateDeviceEnrollmentClaimForUser(h.managedUsers.Users[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	id, secret := "discovery-pc", strings.Repeat("a", 64)
	enrollDevice(t, h, id, secret)
	baseline := discoveryRequest(h, "/salcara-hub/v1/ping", nil)
	if baseline.Code != 200 {
		t.Fatal(baseline.Code, baseline.Body)
	}
	for _, headers := range []http.Header{
		{"Authorization": {"Bearer synthetic-model-key"}, "Cookie": {"ignored=synthetic"}},
		{"X-Salcara-Device-Id": {"stale-pc"}, "X-Salcara-Device-Secret": {strings.Repeat("b", 64)}},
		{"X-Salcara-Device-Id": {id}, "X-Salcara-Device-Secret": {secret}},
		{"X-Salcara-Enrollment-Claim": {"expired"}},
		{"X-Salcara-Enrollment-Claim": {claim}},
		{"X-Salcara-Device-Id": {"stale", "another"}, "X-Salcara-Device-Secret": {"invalid"}, "X-Salcara-Enrollment-Claim": {"invalid"}, "X-Salcara-Pair-Token": {"old-phone"}, "X-Salcara-Bot-Token": {"old-bot"}},
	} {
		w := discoveryRequest(h, "/salcara-hub/v1/ping", headers)
		if w.Code != 200 || w.Body.String() != baseline.Body.String() || w.Header().Get("Set-Cookie") != "" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("credentials contaminated public discovery: %d %s", w.Code, w.Body)
		}
	}
	if strings.Contains(baseline.Body.String(), "private.example") || !strings.Contains(baseline.Body.String(), "discovery.policy.v1") || !strings.Contains(baseline.Body.String(), "ingress.signed-manifest.v1") {
		t.Fatal("anonymous policy leaked a user exception or omitted discovery capability")
	}
	if transport.calls.Load() != 0 || h.limiter.blocked("192.0.2.1") {
		t.Fatal("anonymous discovery attempted authentication")
	}
	hash, _ := enrollmentClaimHash(claim)
	h.mu.Lock()
	valid, accounts, pending, challenges := h.enrollmentClaimValidLocked(hash), len(h.accounts), len(h.pending), len(h.sourceChallenges)
	h.mu.Unlock()
	if !valid || accounts != 1 || pending != 0 || challenges != 0 {
		t.Fatal("discovery mutated credential or execution state")
	}
}

func TestDiscoveryPolicyKeepsUserExceptionsScopedAndInvitationsUnconsumed(t *testing.T) {
	dir := t.TempDir()
	h := newDeviceHub(t, Config{DataDir: dir, PublicURL: "https://hub.example/salcara-hub", DisableLegacyAPIKey: true})
	station := &StationPolicy{Version: 2, ExternalAPIBases: []string{"https://external.example/team"}}
	managedChange(t, h, ManagedUserChange{Action: "save", Name: "scoped", Station: station})
	user := h.managedUsers.Users[0]
	station = cloneStationPolicy(user.Station)
	claim, _, err := h.CreateDeviceEnrollmentClaimForUser(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimHeaders := http.Header{"X-Salcara-Enrollment-Claim": {claim}}
	first := discoveryRemotePolicy(t, discoveryRequest(h, "/salcara-hub/v1/discovery/policy", claimHeaders), "enrollment")
	second := discoveryRemotePolicy(t, discoveryRequest(h, "/salcara-hub/v1/discovery/policy", claimHeaders), "enrollment")
	if !reflect.DeepEqual(first, second) || !reflect.DeepEqual(first.Station, station) || !strings.HasPrefix(first.Revision, "invite-") {
		t.Fatal("invitation preview lost its scoped policy or consumed the claim")
	}
	id, secret := "scoped-pc", strings.Repeat("c", 64)
	w := deviceRequestWithEnrollmentClaim(t, h, "/device/register", id, secret, "", Device{DeviceID: id}, "", claim, false)
	if w.Code != 200 {
		t.Fatalf("preview consumed enrollment: %d %s", w.Code, w.Body)
	}
	headers := http.Header{"X-Salcara-Device-Id": {id}, "X-Salcara-Device-Secret": {secret}}
	assigned := discoveryRemotePolicy(t, discoveryRequest(h, "/salcara-hub/v1/discovery/policy", headers), "device")
	if !reflect.DeepEqual(assigned.Station, station) || !strings.HasPrefix(assigned.Revision, "user-") {
		t.Fatal("device policy is not the assigned user policy")
	}
	w = discoveryRequest(h, "/salcara-hub/v1/discovery/policy", claimHeaders)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "enrollment_claim_invalid") {
		t.Fatal("consumed claim remained usable")
	}
	global := discoveryRequest(h, "/salcara-hub/v1/ping", headers)
	if strings.Contains(global.Body.String(), "external.example") {
		t.Fatal("private exception leaked into ping")
	}
	enrollDevice(t, h, "another-pc", strings.Repeat("d", 64))
	other := discoveryRemotePolicy(t, discoveryRequest(h, "/salcara-hub/v1/discovery/policy", http.Header{"X-Salcara-Device-Id": {"another-pc"}, "X-Salcara-Device-Secret": {strings.Repeat("d", 64)}}), "device")
	if reflect.DeepEqual(other.Station, assigned.Station) {
		t.Fatal("another device inherited the private exception")
	}
	h.Close()
	h = newDeviceHub(t, Config{DataDir: dir, PublicURL: "https://hub.example/salcara-hub", DisableLegacyAPIKey: true})
	restored := discoveryRemotePolicy(t, discoveryRequest(h, "/salcara-hub/v1/discovery/policy", headers), "device")
	if !reflect.DeepEqual(assigned, restored) {
		t.Fatal("restart changed the effective policy or binding")
	}
	managedChange(t, h, ManagedUserChange{Action: "save", ID: user.ID, Name: user.Name, Station: station, Disabled: true})
	w = discoveryRequest(h, "/salcara-hub/v1/discovery/policy", headers)
	if w.Code != 423 || !strings.Contains(w.Body.String(), "user_disabled") {
		t.Fatal("disabled user received active policy")
	}
	if discoveryRequest(h, "/salcara-hub/v1/ping", headers).Code != 200 {
		t.Fatal("disabled user contaminated anonymous discovery")
	}
}

func TestDiscoveryPolicyRejectsMissingMixedAndInvalidCredentials(t *testing.T) {
	transport := &rejectingRoundTripper{}
	h := newDeviceHub(t, Config{HTTPClient: &http.Client{Transport: transport}, Sub2APIURL: "https://unused.invalid", AuthFailLimit: 100})
	id, secret := "policy-auth-pc", strings.Repeat("a", 64)
	enrollDevice(t, h, id, secret)
	claim := testEnrollmentClaim(t, h)
	for _, tc := range []struct {
		name, query, code string
		status            int
		headers           http.Header
	}{
		{name: "anonymous", status: 401, code: "device_credentials_required"},
		{name: "model-key", status: 400, code: "ambiguous_credentials", headers: http.Header{"Authorization": {"Bearer synthetic"}}},
		{name: "old-device", status: 403, code: "device_credentials_invalid", headers: http.Header{"X-Salcara-Device-Id": {"old"}, "X-Salcara-Device-Secret": {secret}}},
		{name: "secret-only", status: 403, code: "device_credentials_invalid", headers: http.Header{"X-Salcara-Device-Secret": {secret}}},
		{name: "id-only", status: 403, code: "device_credentials_invalid", headers: http.Header{"X-Salcara-Device-Id": {id}}},
		{name: "wrong-secret", status: 403, code: "device_credentials_invalid", headers: http.Header{"X-Salcara-Device-Id": {id}, "X-Salcara-Device-Secret": {"bad"}}},
		{name: "old-claim", status: 403, code: "enrollment_claim_invalid", headers: http.Header{"X-Salcara-Enrollment-Claim": {"bad"}}},
		{name: "mixed", status: 400, code: "ambiguous_credentials", headers: http.Header{"X-Salcara-Enrollment-Claim": {claim}, "X-Salcara-Device-Id": {id}, "X-Salcara-Device-Secret": {secret}}},
		{name: "duplicate", status: 400, code: "ambiguous_credentials", headers: http.Header{"X-Salcara-Enrollment-Claim": {claim, claim}}},
		{name: "hidden-model-key", status: 400, code: "ambiguous_credentials", headers: http.Header{"X-Salcara-Enrollment-Claim": {claim}, "Authorization": {"", "Bearer synthetic"}}},
		{name: "hidden-phone", status: 400, code: "ambiguous_credentials", headers: http.Header{"X-Salcara-Enrollment-Claim": {claim}, "X-Salcara-Pair-Token": {"", "synthetic"}}},
		{name: "hidden-bot", status: 400, code: "ambiguous_credentials", headers: http.Header{"X-Salcara-Enrollment-Claim": {claim}, "X-Salcara-Bot-Token": {"", "synthetic"}}},
		{name: "phone", status: 400, code: "ambiguous_credentials", headers: http.Header{"X-Salcara-Pair-Token": {"synthetic"}}},
		{name: "query", query: "?user_id=someone", status: 400, code: "invalid_request"},
		{name: "empty-query", query: "?", status: 400, code: "invalid_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := discoveryRequest(h, "/salcara-hub/v1/discovery/policy"+tc.query, tc.headers)
			var body map[string]any
			if w.Code != tc.status || json.Unmarshal(w.Body.Bytes(), &body) != nil || body["code"] != tc.code || body["remoteAccess"] != nil {
				t.Fatalf("unexpected policy failure: %d %s", w.Code, w.Body)
			}
		})
	}
	if transport.calls.Load() != 0 {
		t.Fatal("policy request attempted model authentication")
	}
	headers := http.Header{"X-Salcara-Device-Id": {id}, "X-Salcara-Device-Secret": {secret}}
	for i := 0; i < 101; i++ {
		h.limiter.fail("192.0.2.1")
	}
	if discoveryRequest(h, "/salcara-hub/v1/discovery/policy", headers).Code != 200 {
		t.Fatal("valid device blocked by another caller")
	}
	w := discoveryRequest(h, "/salcara-hub/v1/discovery/policy", http.Header{"X-Salcara-Device-Id": {"bad"}})
	if w.Code != 429 || !strings.Contains(w.Body.String(), "remote_rate_limited") {
		t.Fatal("invalid policy attempts were not limited")
	}
	if discoveryRequest(h, "/salcara-hub/v1/ping", headers).Code != 200 {
		t.Fatal("policy rate limit broke public discovery")
	}
}

func TestDiscoveryPluginCanonicalPreservesConfiguredPrefix(t *testing.T) {
	for _, prefix := range []string{"/salcara-hub", "/tenant/salcara-hub", "/custom-plugin"} {
		t.Run(prefix, func(t *testing.T) {
			h := newDeviceHub(t, Config{DataDir: t.TempDir(), PublicURL: "https://hub.example" + prefix, Prefix: prefix})
			if got := h.IngressState().CanonicalURL; got != "https://hub.example"+prefix+"/v1" {
				t.Fatal("prefix lost:", got)
			}
			if w := discoveryRequest(h, prefix+"/v1/ping", nil); w.Code != 200 {
				t.Fatal(w.Code, w.Body)
			}
			if w := discoveryRequest(h, prefix+".evil/v1/ping", nil); w.Code != 404 {
				t.Fatal("prefix lookalike accepted")
			}
		})
	}
}
