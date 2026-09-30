package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"salcara/hubplugin/internal/hub"
)

const (
	pluginID      = "top.salcara.hub"
 pluginVersion = "0.3.1"
	maxBody       = 4 << 20
)

type settings struct {
	Sub2APIURL         string  `json:"sub2api_url"`
	TrustProxy         bool    `json:"trust_proxy"`
	CommandTimeoutSecs int     `json:"command_timeout_seconds"`
	AllowedGroupIDs    []int64 `json:"allowed_group_ids"`
}

func normalizeSettings(raw []byte) (settings, []byte, error) {
	cfg := settings{Sub2APIURL: "http://127.0.0.1:8080", TrustProxy: false, CommandTimeoutSecs: 45}
	if len(bytes.TrimSpace(raw)) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&cfg); err != nil {
			return settings{}, nil, err
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			return settings{}, nil, errors.New("配置只能包含一个 JSON 对象")
		}
	}
	if cfg.CommandTimeoutSecs < 1 || cfg.CommandTimeoutSecs > 300 {
		return settings{}, nil, errors.New("指令等待时间必须为 1–300 秒")
	}
	if len(cfg.AllowedGroupIDs) > 32 {
		return settings{}, nil, errors.New("最多选择 32 个允许远程功能的 API 分组")
	}
	sort.Slice(cfg.AllowedGroupIDs, func(i, j int) bool { return cfg.AllowedGroupIDs[i] < cfg.AllowedGroupIDs[j] })
	for i, groupID := range cfg.AllowedGroupIDs {
		if groupID <= 0 || (i > 0 && groupID == cfg.AllowedGroupIDs[i-1]) {
			return settings{}, nil, errors.New("API 分组 ID 必须是互不重复的正整数")
		}
	}
	var err error
	cfg.Sub2APIURL, err = validBaseURL(cfg.Sub2APIURL)
	if err != nil {
		return settings{}, nil, fmt.Errorf("Sub2API 内部地址: %w", err)
	}
	canonical, err := json.Marshal(cfg)
	return cfg, canonical, err
}

func validBaseURL(value string) (string, error) {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	u, err := url.Parse(value)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("必须填写不含账号、查询参数的 http(s) 地址")
	}
	return value, nil
}

type hubPlugin struct {
	pluginv1.UnimplementedTransportPluginServer
	mu      sync.RWMutex
	hub     *hub.Hub
	cfg     settings
	dataDir string
}

func newHubPlugin() *hubPlugin {
	return &hubPlugin{dataDir: os.Getenv("SALCARA_PLUGIN_DATA_DIR")}
}

func (p *hubPlugin) GetInfo(context.Context, *pluginv1.GetInfoRequest) (*pluginv1.GetInfoResponse, error) {
	return &pluginv1.GetInfoResponse{
		PluginId: pluginID, PluginVersion: pluginVersion,
		ProtocolVersion:     pluginv1.ProtocolVersion,
		TransportApiVersion: pluginv1.TransportAPIVersion,
		Capabilities:        []string{"salcara.hub.http.v1"},
	}, nil
}

func (p *hubPlugin) Health(context.Context, *pluginv1.HealthRequest) (*pluginv1.HealthResponse, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	status := struct {
		Configured bool      `json:"configured"`
		Version    string    `json:"version"`
		Stats      hub.Stats `json:"stats"`
	}{Configured: p.hub != nil, Version: hub.Version}
	if p.hub != nil {
		status.Stats = p.hub.Stats()
	}
	blob, _ := json.Marshal(status)
	message := "等待配置"
	if p.hub != nil {
		message = "Hub 正常运行"
	}
	return &pluginv1.HealthResponse{Healthy: true, Message: message, StatusJson: string(blob)}, nil
}

func (p *hubPlugin) ValidateConfig(_ context.Context, request *pluginv1.ValidateConfigRequest) (*pluginv1.ValidateConfigResponse, error) {
	_, normalized, err := normalizeSettings(request.ConfigJson)
	if err != nil {
		return &pluginv1.ValidateConfigResponse{Valid: false, Message: err.Error()}, nil
	}
	if strings.TrimSpace(p.dataDir) == "" {
		return &pluginv1.ValidateConfigResponse{Valid: false, Message: "宿主未提供持久化数据目录"}, nil
	}
	return &pluginv1.ValidateConfigResponse{Valid: true, NormalizedConfigJson: normalized}, nil
}

func (p *hubPlugin) ApplyConfig(_ context.Context, request *pluginv1.ApplyConfigRequest) (*pluginv1.ApplyConfigResponse, error) {
	cfg, _, err := normalizeSettings(request.ConfigJson)
	if err != nil {
		return &pluginv1.ApplyConfigResponse{Applied: false, Message: err.Error()}, nil
	}
	if strings.TrimSpace(p.dataDir) == "" {
		return &pluginv1.ApplyConfigResponse{Applied: false, Message: "宿主未提供持久化数据目录"}, nil
	}
	if err := os.MkdirAll(p.dataDir, 0o700); err != nil {
		return &pluginv1.ApplyConfigResponse{Applied: false, Message: err.Error()}, nil
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	p.mu.Lock()
	defer p.mu.Unlock()
	// Drain and flush the old service before loading the same state directory.
	// Two live savers otherwise race and reconfiguration can lose fresh pairing.
	previous := p.hub
	p.hub = nil
	if previous != nil {
		previous.Close()
	}
	service, err := hub.New(hub.Config{
		Sub2APIURL: cfg.Sub2APIURL, DataDir: p.dataDir,
		AllowedGroupIDs: cfg.AllowedGroupIDs, ValidTTL: 20 * time.Second,
		Prefix: "/salcara-hub", TrustProxy: cfg.TrustProxy,
		// Downloads are intentionally disabled: the admin UI is a status and
		// configuration surface, not a software-distribution page.
		DownloadsDir:   "",
		CommandTimeout: time.Duration(cfg.CommandTimeoutSecs) * time.Second,
		HTTPClient: &http.Client{Transport: &http.Transport{
			Proxy: nil, MaxIdleConnsPerHost: 16, IdleConnTimeout: 90 * time.Second,
		}},
		Logger: logger,
	})
	if err != nil {
		return &pluginv1.ApplyConfigResponse{Applied: false, Message: err.Error()}, nil
	}
	p.hub = service
	p.cfg = cfg
	return &pluginv1.ApplyConfigResponse{Applied: true}, nil
}

func (p *hubPlugin) TestConfig(_ context.Context, request *pluginv1.TestConfigRequest) (*pluginv1.TestConfigResponse, error) {
	_, _, err := normalizeSettings(request.ConfigJson)
	if err != nil {
		return &pluginv1.TestConfigResponse{Success: false, Message: err.Error()}, nil
	}
	if strings.TrimSpace(p.dataDir) == "" {
		return &pluginv1.TestConfigResponse{Success: false, Message: "宿主未提供持久化数据目录"}, nil
	}
	return &pluginv1.TestConfigResponse{Success: true, Message: "设备配对配置有效；不需要中转站用户账号或模型 API Key 验证接口（未测试旧版 Key 模式）"}, nil
}

func (p *hubPlugin) Forward(stream pluginv1.TransportPlugin_ForwardServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil || !strings.HasPrefix(start.Url, "/salcara-hub") {
		return errors.New("Hub 请求路径无效")
	}
	parsed, err := url.ParseRequestURI(start.Url)
	if err != nil || parsed.IsAbs() || (parsed.Path != "/salcara-hub" && !strings.HasPrefix(parsed.Path, "/salcara-hub/")) {
		return errors.New("Hub 请求路径无效")
	}
	body := make([]byte, 0, 4096)
	for {
		frame, recvErr := stream.Recv()
		if recvErr != nil {
			return recvErr
		}
		if frame.GetBodyEnd() {
			break
		}
		chunk := frame.GetBodyChunk()
		if len(body)+len(chunk) > maxBody {
			return errors.New("Hub 请求体过大")
		}
		body = append(body, chunk...)
	}
	request, err := http.NewRequestWithContext(stream.Context(), start.Method, start.Url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Host = start.Host
	request.ContentLength = int64(len(body))
	for name, values := range start.Headers {
		if values == nil {
			continue
		}
		request.Header[name] = append([]string(nil), values.Values...)
	}
	applyHostPeer(request)
	p.mu.RLock()
	service := p.hub
	p.mu.RUnlock()
	if service == nil {
		return errors.New("Hub 尚未配置")
	}
	writer := &streamResponseWriter{stream: stream, header: make(http.Header)}
	if strings.HasPrefix(parsed.Path, "/salcara-hub/_admin/") {
		// AccountId is host-created gRPC metadata, never an HTTP header. The
		// public forwarding entrypoint always sends zero. Only admin JWT /
		// step-up protected host endpoints construct this reserved marker.
		if start.AccountId != -1 || start.Platform != "salcara" || start.AccountType != "http" {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusForbidden)
			_, _ = writer.Write([]byte(`{"error":"管理操作仅允许宿主管理员通道"}`))
		} else {
			service.ServeAdminHTTP(writer, request)
		}
	} else {
		service.ServeHTTP(writer, request)
	}
	return writer.finish()
}

// Only the trusted Sub2API host can assert the socket peer. Public forwarding
// headers are never trusted by the plugin, including on legacy configurations.
func applyHostPeer(request *http.Request) {
	peer := strings.TrimSpace(request.Header.Get("X-Salcara-Peer-IP"))
	request.Header.Del("X-Salcara-Peer-IP")
	request.Header.Del("X-Forwarded-For")
	request.Header.Del("X-Real-IP")
	request.Header.Del("Forwarded")
	request.RemoteAddr = "unknown-host-peer"
	if ip := net.ParseIP(peer); ip != nil {
		request.RemoteAddr = net.JoinHostPort(ip.String(), "0")
	}
}

type streamResponseWriter struct {
	stream pluginv1.TransportPlugin_ForwardServer
	header http.Header
	wrote  bool
	count  int64
	err    error
}

func (w *streamResponseWriter) Header() http.Header { return w.header }

func (w *streamResponseWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	headers := make(map[string]*pluginv1.HeaderValues, len(w.header))
	for name, values := range w.header {
		headers[name] = &pluginv1.HeaderValues{Values: append([]string(nil), values...)}
	}
	length := int64(-1)
	if value := w.header.Get("Content-Length"); value != "" {
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil && parsed >= 0 {
			length = parsed
		}
	}
	w.err = w.stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Start{Start: &pluginv1.ForwardResponseStart{
		StatusCode: int32(status), Status: fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Protocol: "HTTP/1.1", ProtocolMajor: 1, ProtocolMinor: 1,
		Headers: headers, ContentLength: length,
	}}})
}

func (w *streamResponseWriter) Write(payload []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if w.err != nil {
		return 0, w.err
	}
	if len(payload) == 0 {
		return 0, nil
	}
	w.err = w.stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: append([]byte(nil), payload...)}})
	if w.err != nil {
		return 0, w.err
	}
	w.count += int64(len(payload))
	return len(payload), nil
}

func (w *streamResponseWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
}

func (w *streamResponseWriter) finish() error {
	if err := w.stream.Context().Err(); err != nil {
		return err
	}
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if w.err != nil {
		return w.err
	}
	return w.stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_End{End: &pluginv1.ForwardResponseEnd{BytesReceived: w.count}}})
}

var _ http.ResponseWriter = (*streamResponseWriter)(nil)
var _ http.Flusher = (*streamResponseWriter)(nil)
