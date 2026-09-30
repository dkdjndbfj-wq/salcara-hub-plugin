package hub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func requestHub(t *testing.T, h *Hub, method, path, key, deviceSecret, pairToken string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var input *bytes.Reader
	if body == nil {
		input = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		input = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, "http://example.com/salcara-hub/v1"+path, input)
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	if deviceSecret != "" {
		request.Header.Set("X-Salcara-Device-Secret", deviceSecret)
	}
	if pairToken != "" {
		request.Header.Set("X-Salcara-Pair-Token", pairToken)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	return recorder
}

func TestGroupAllowlistAndOneToOnePairing(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/salcara-hub/key" {
			http.NotFound(w, r)
			return
		}
		switch r.Header.Get("Authorization") {
		case "Bearer sk-allowed":
			_, _ = w.Write([]byte(`{"group_id":7}`))
		case "Bearer sk-other":
			_, _ = w.Write([]byte(`{"group_id":8}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer upstream.Close()
	dataDir := t.TempDir()
	newService := func() *Hub {
		h, err := New(Config{Sub2APIURL: upstream.URL, DataDir: dataDir, AllowedGroupIDs: []int64{7}})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	h := newService()
	defer h.Close()
	secret := strings.Repeat("a", 64)
	register := requestHub(t, h, http.MethodPost, "/bridge/register", "sk-allowed", secret, "", map[string]any{"deviceId": "pc-1", "name": "Test PC"})
	if register.Code != 200 {
		t.Fatalf("register: %d %s", register.Code, register.Body.String())
	}
	other := requestHub(t, h, http.MethodGet, "/me", "sk-other", "", "", nil)
	if other.Code != http.StatusForbidden {
		t.Fatalf("other group accepted: %d", other.Code)
	}
	unauthorized := requestHub(t, h, http.MethodGet, "/app/devices", "sk-allowed", "", "", nil)
	if unauthorized.Code != 403 {
		t.Fatalf("unpaired app accepted: %d", unauthorized.Code)
	}
	h.mu.Lock()
	h.accountLocked(AccountID("sk-allowed")).devices["pc-1"].conn = &bridgeConn{cmds: make(chan []byte, 1), closed: make(chan struct{})}
	h.mu.Unlock()
	issue := func() string {
		response := requestHub(t, h, http.MethodPost, "/bridge/pair/start", "sk-allowed", secret, "", map[string]string{"deviceId": "pc-1"})
		if response.Code != 200 {
			t.Fatalf("pair start: %d %s", response.Code, response.Body.String())
		}
		var payload struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Code) != 8 {
			t.Fatalf("bad code %q", payload.Code)
		}
		return payload.Code
	}
	wrongSecret := requestHub(t, h, http.MethodPost, "/bridge/pair/start", "sk-allowed", strings.Repeat("b", 64), "", map[string]string{"deviceId": "pc-1"})
	if wrongSecret.Code != 403 {
		t.Fatalf("wrong desktop secret accepted: %d", wrongSecret.Code)
	}
	confirm := func(code string) string {
		response := requestHub(t, h, http.MethodPost, "/app/pair/confirm", "sk-allowed", "", "", map[string]string{"code": code})
		if response.Code != 200 {
			t.Fatalf("pair confirm: %d %s", response.Code, response.Body.String())
		}
		var payload struct {
			PairToken string `json:"pair_token"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.PairToken) != 64 {
			t.Fatalf("bad pair token length: %d", len(payload.PairToken))
		}
		return payload.PairToken
	}
	firstToken := confirm(issue())
	paired := requestHub(t, h, http.MethodGet, "/app/devices", "sk-allowed", "", firstToken, nil)
	if paired.Code != 200 || !strings.Contains(paired.Body.String(), "pc-1") {
		t.Fatalf("paired app: %d %s", paired.Code, paired.Body.String())
	}
	secondToken := confirm(issue())
	if secondToken == firstToken {
		t.Fatal("re-pair did not rotate token")
	}
	old := requestHub(t, h, http.MethodGet, "/app/devices", "sk-allowed", "", firstToken, nil)
	if old.Code != 403 {
		t.Fatalf("old token accepted: %d", old.Code)
	}
	h.saveNow()
	h.Close()
	restarted := newService()
	defer restarted.Close()
	retained := requestHub(t, restarted, http.MethodGet, "/app/devices", "sk-allowed", "", secondToken, nil)
	if retained.Code != 200 {
		t.Fatalf("pair not persisted: %d %s", retained.Code, retained.Body.String())
	}
}
