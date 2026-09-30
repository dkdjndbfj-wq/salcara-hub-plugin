package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"salcara/hubplugin/internal/hub"
)

func TestGroupAllowlistConfigFailsClosed(t *testing.T) {
	cfg, _, err := normalizeSettings([]byte(`{"allowed_group_ids":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AllowedGroupIDs) != 0 {
		t.Fatal("an empty selection must not grant legacy API-Key remote access")
	}
	if _, _, err := normalizeSettings([]byte(`{"allowed_group_ids":[7,7]}`)); err == nil {
		t.Fatal("duplicate group IDs must be rejected")
	}
	if _, _, err := normalizeSettings([]byte(`{"allowed_group_ids":[-1]}`)); err == nil {
		t.Fatal("invalid group IDs must be rejected")
	}
	if _, _, err := normalizeSettings([]byte(`{"allowed_group_ids":[1],"unknown":true}`)); err == nil {
		t.Fatal("unknown configuration fields must be rejected")
	}
	if _, _, err := normalizeSettings([]byte(`{"allowed_group_ids":[1],"sub2api_url":"http://user:pass@localhost"}`)); err == nil {
		t.Fatal("credentials must not be embedded in the internal URL")
	}
}

type fixtureForwardStream struct {
	pluginv1.TransportPlugin_ForwardServer
	input  []*pluginv1.ForwardRequest
	output []*pluginv1.ForwardResponse
}

func (s *fixtureForwardStream) Context() context.Context { return context.Background() }
func (s *fixtureForwardStream) Recv() (*pluginv1.ForwardRequest, error) {
	if len(s.input) == 0 {
		return nil, io.EOF
	}
	first := s.input[0]
	s.input = s.input[1:]
	return first, nil
}
func (s *fixtureForwardStream) Send(r *pluginv1.ForwardResponse) error {
	s.output = append(s.output, r)
	return nil
}

func TestOnlyPrivateHostRPCMetadataCanReachAdministratorOperations(t *testing.T) {
	service, err := hub.New(hub.Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	p := &hubPlugin{hub: service}
	for _, tc := range []struct {
		id             int64
		platform, kind string
		want           int
	}{
		{0, "salcara", "http", 403},
		{1, "salcara", "http", 403},
		{-1, "openai", "http", 403},
		{-1, "salcara", "oauth", 403},
		{-1, "salcara", "http", 200},
	} {
		stream := &fixtureForwardStream{input: []*pluginv1.ForwardRequest{
			{Frame: &pluginv1.ForwardRequest_Start{Start: &pluginv1.ForwardRequestStart{Method: "GET", Url: "/salcara-hub/_admin/snapshot", AccountId: tc.id, Platform: tc.platform, AccountType: tc.kind, Headers: map[string]*pluginv1.HeaderValues{"X-Salcara-Admin": {Values: []string{"true"}}, "X-Salcara-Account-ID": {Values: []string{"-1"}}}}}},
			{Frame: &pluginv1.ForwardRequest_BodyEnd{BodyEnd: true}},
		}}
		if err := p.Forward(stream); err != nil {
			t.Fatal(err)
		}
		var status int
		for _, frame := range stream.output {
			if start := frame.GetStart(); start != nil {
				status = int(start.StatusCode)
			}
		}
		if status != tc.want {
			t.Fatalf("provenance id=%d %s/%s: %d want%d", tc.id, tc.platform, tc.kind, status, tc.want)
		}
	}
}

type cancelledForwardStream struct {
	pluginv1.TransportPlugin_ForwardServer
	ctx   context.Context
	sends int
}

func (s *cancelledForwardStream) Context() context.Context             { return s.ctx }
func (s *cancelledForwardStream) Send(*pluginv1.ForwardResponse) error { s.sends++; return nil }

func TestCancelledForwardDoesNotFabricateHTTP200(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stream := &cancelledForwardStream{ctx: ctx}
	writer := &streamResponseWriter{stream: stream, header: make(http.Header)}
	if err := writer.finish(); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled finish: %v", err)
	}
	if writer.wrote || stream.sends != 0 {
		t.Fatal("cancelled request fabricated a successful response")
	}
}

func TestApplyConfigReloadsFreshPairingAfterDrainingOldHub(t *testing.T) {
	p := &hubPlugin{dataDir: t.TempDir()}
	apply := func() {
		t.Helper()
		result, err := p.ApplyConfig(context.Background(), &pluginv1.ApplyConfigRequest{ConfigJson: []byte(`{}`)})
		if err != nil || !result.Applied {
			t.Fatalf("apply config: %+v %v", result, err)
		}
	}
	apply()
	t.Cleanup(func() {
		if p.hub != nil {
			p.hub.Close()
		}
	})
	first := httptest.NewServer(p.hub)
	t.Cleanup(first.Close)
	secret := strings.Repeat("c", 64)
	do := func(base, path, token, body string) *http.Response {
		t.Helper()
		method := http.MethodPost
		if body == "" {
			method = http.MethodGet
		}
		req, err := http.NewRequest(method, base+"/salcara-hub/v1"+path, bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("X-Salcara-Pair-Token", token)
		} else {
			req.Header.Set("X-Salcara-Device-Id", "reload-pc")
			req.Header.Set("X-Salcara-Device-Secret", secret)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != 200 {
			response.Body.Close()
			t.Fatalf("%s: HTTP %d", path, response.StatusCode)
		}
		return response
	}
	register := do(first.URL, "/device/register", "", `{"deviceId":"reload-pc"}`)
	register.Body.Close()
	bridge := do(first.URL, "/bridge/stream?deviceId=reload-pc", "", "")
	t.Cleanup(func() { bridge.Body.Close() })
	start := do(first.URL, "/bridge/pair/start", "", `{"deviceId":"reload-pc"}`)
	var ticket struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(start.Body).Decode(&ticket); err != nil {
		t.Fatal(err)
	}
	start.Body.Close()
	request, err := http.NewRequest(http.MethodPost, first.URL+"/salcara-hub/v1/app/pair/qr", bytes.NewBufferString(`{"deviceId":"reload-pc","ticket":"`+ticket.Ticket+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	paired, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var token struct {
		Token string `json:"pair_token"`
	}
	if err := json.NewDecoder(paired.Body).Decode(&token); err != nil {
		t.Fatal(err)
	}
	paired.Body.Close()
	if paired.StatusCode != 200 || len(token.Token) != 64 {
		t.Fatal("pair before reload failed")
	}
	apply()
	second := httptest.NewServer(p.hub)
	t.Cleanup(second.Close)
	retained := do(second.URL, "/app/devices", token.Token, "")
	retained.Body.Close()
	old := httptest.NewRecorder()
	first.Config.Handler.ServeHTTP(old, httptest.NewRequest("GET", "http://example/salcara-hub/v1/ping", nil))
	if old.Code != 503 {
		t.Fatal("old Hub still accepts requests after reconfiguration")
	}
}

func TestDeviceOnlyConfigTestDoesNotCallStationKeyEndpoint(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.NotFound(w, r) }))
	defer upstream.Close()
	p := &hubPlugin{dataDir: t.TempDir()}
	result, err := p.TestConfig(context.Background(), &pluginv1.TestConfigRequest{ConfigJson: []byte(`{"sub2api_url":"` + upstream.URL + `","allowed_group_ids":[]}`)})
	if err != nil || !result.Success {
		t.Fatalf("device config requires legacy endpoint: %+v %v", result, err)
	}
	if calls.Load() != 0 {
		t.Fatal("configuration check consulted station/model API")
	}
	if !strings.Contains(result.Message, "不需要") {
		t.Fatal("config check did not explain device-only mode")
	}
	invalid, err := (&hubPlugin{}).TestConfig(context.Background(), &pluginv1.TestConfigRequest{ConfigJson: []byte(`{}`)})
	if err != nil || invalid.Success {
		t.Fatal("missing persistent data directory accepted")
	}
}

func TestTransportAcceptsOnlyHostValidatedSocketPeer(t *testing.T) {
	for _, tc := range []struct{ peer, want string }{
		{"192.0.2.20", "192.0.2.20:0"},
		{"2001:db8::20", "[2001:db8::20]:0"},
		{"not-an-ip", "unknown-host-peer"},
		{"", "unknown-host-peer"},
	} {
		r := httptest.NewRequest("GET", "http://example.test/salcara-hub/v1/ping", nil)
		r.Header.Set("X-Salcara-Peer-IP", tc.peer)
		r.Header.Set("X-Forwarded-For", "198.51.100.99")
		r.Header.Set("X-Real-IP", "198.51.100.99")
		r.Header.Set("Forwarded", "for=198.51.100.99")
		applyHostPeer(r)
		if r.RemoteAddr != tc.want {
			t.Fatalf("peer %q: %q", tc.peer, r.RemoteAddr)
		}
		for _, header := range []string{"X-Salcara-Peer-IP", "X-Forwarded-For", "X-Real-IP", "Forwarded"} {
			if r.Header.Get(header) != "" {
				t.Fatalf("untrusted header retained %s", header)
			}
		}
	}
	cfg, _, err := normalizeSettings(nil)
	if err != nil || cfg.TrustProxy {
		t.Fatal("plugin must not trust public proxy headers by default")
	}
}

func TestGroupAllowlistLimit(t *testing.T) {
	var ids []string
	for i := 1; i <= 33; i++ {
		ids = append(ids, strconv.Itoa(i))
	}
	if _, _, err := normalizeSettings([]byte(`{"allowed_group_ids":[` + strings.Join(ids, ",") + `]}`)); err == nil {
		t.Fatal("more than 32 groups must be rejected")
	}
}
