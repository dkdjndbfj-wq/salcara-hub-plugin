package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestStandaloneRejectsModelKeysWithoutContactingSub2API(t *testing.T) {
	var contacted atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacted.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	h, err := New(Config{DisableLegacyAPIKey: true, Sub2APIURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	request := httptest.NewRequest(http.MethodGet, "/salcara-hub/v1/me", nil)
	request.Header.Set("Authorization", "Bearer example-model-key-not-a-secret")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || contacted.Load() != 0 {
		t.Fatalf("status=%d contacted=%d", response.Code, contacted.Load())
	}
	ping := httptest.NewRecorder()
	h.ServeHTTP(ping, httptest.NewRequest(http.MethodGet, "/salcara-hub/v1/ping", nil))
	var body struct {
		AuthModes []string `json:"authModes"`
	}
	if err := json.Unmarshal(ping.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.AuthModes) != 1 || body.AuthModes[0] != "device-pairing" {
		t.Fatalf("unexpected modes: %#v", body.AuthModes)
	}
}
