package standalone

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestStandaloneDiscoveryIgnoresStaleCredentials(t *testing.T) {
	s, _, _ := serverFixture(t)
	var baseline map[string]any
	for i, headers := range []map[string]string{
		{},
		{"Authorization": "Bearer synthetic-api-key", "Cookie": "old-session=synthetic"},
		{"X-Salcara-Device-Id": "old-computer", "X-Salcara-Device-Secret": strings.Repeat("a", 64)},
		{"X-Salcara-Enrollment-Claim": "expired-claim", "X-Salcara-Pair-Token": "old-phone"},
	} {
		r := httptest.NewRequest("GET", Prefix+"/v1/ping", nil)
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		var body map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body["service"] != "salcara-hub" || body["standalone_version"] != Version || body["source_status"] == nil {
			t.Fatalf("stale credentials broke discovery: %d %s", w.Code, w.Body)
		}
		if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Set-Cookie") != "" {
			t.Fatal("discovery session/cache leak")
		}
		if i == 0 {
			baseline = body
		} else if !reflect.DeepEqual(body, baseline) {
			t.Fatal("credentials changed public response")
		}
	}
}

func TestStandaloneDiscoveryPreservesFailureClassification(t *testing.T) {
	s, _, _ := serverFixture(t)
	for _, tc := range []struct {
		method, path string
		status       int
		code         string
	}{
		{"POST", Prefix + "/v1/ping", 405, ""},
		{"GET", Prefix + "/v1/not-a-discovery-endpoint", 404, ""},
		{"GET", Prefix + "/v1/discovery/policy", 401, "device_credentials_required"},
		{"GET", Prefix + "/v1/bridge/pair/status", 403, "device_credentials_invalid"},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		if strings.Contains(tc.path, "/bridge/") {
			r.Header.Set("X-Salcara-Device-Id", "old-pc")
			r.Header.Set("X-Salcara-Device-Secret", strings.Repeat("b", 64))
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		var body map[string]any
		if w.Code != tc.status || json.Unmarshal(w.Body.Bytes(), &body) != nil || tc.code != "" && body["code"] != tc.code {
			t.Fatalf("classification lost for %s: %d %s", tc.path, w.Code, w.Body)
		}
	}
	s.hub.Close()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", Prefix+"/v1/ping", nil))
	var body map[string]any
	if w.Code != http.StatusServiceUnavailable || json.Unmarshal(w.Body.Bytes(), &body) != nil || body["code"] != "remote_unavailable" || body["error"] != "服务正在重启，请稍后重试" {
		t.Fatalf("underlying unavailable response was replaced: %d %s", w.Code, w.Body)
	}
}
