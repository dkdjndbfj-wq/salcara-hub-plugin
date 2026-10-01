package standalone

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

type updateStatus struct {
	Configured     bool   `json:"configured"`
	CurrentVersion string `json:"current_version"`
	LatestVersion  string `json:"latest_version,omitempty"`
	Status         string `json:"status"`
	SHA256         string `json:"sha256,omitempty"`
	ReleaseNotes   string `json:"release_notes,omitempty"`
	Message        string `json:"message"`
	JobID          string `json:"job_id,omitempty"`
}

type controlClient struct {
	client *http.Client
	token  string
}

func newControlClient(cfg Config) (*controlClient, error) {
	if cfg.ControlSocket == "" {
		return nil, nil
	}
	token, err := readAdminToken(cfg.ControlTokenFile)
	if err != nil {
		return nil, errors.New("Hub update control credential is unavailable or unsafe")
	}
	defer clear(token)
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			// The target is set only by the local deployment environment. No
			// request value, URL, proxy variable or redirect can select a TCP
			// address, Docker socket, or alternate update service.
			return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", cfg.ControlSocket)
		}, ResponseHeaderTimeout: 20 * time.Second, MaxResponseHeaderBytes: 8 << 10}
	return &controlClient{token: string(token), client: &http.Client{Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("update control redirects are forbidden")
		}}}, nil
}

var updateVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)
var updateDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (c *controlClient) call(ctx context.Context, method, path string, body []byte, timeout time.Duration) (updateStatus, int, error) {
	var result updateStatus
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://launcher"+path, bytes.NewReader(body))
	if err != nil {
		return result, 0, errors.New("update request unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return result, 0, errors.New("更新组件连接不可用；未自动重试，若刚提交更新请稍后刷新状态")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusConflict && resp.StatusCode != http.StatusServiceUnavailable {
		return result, 0, errors.New("更新组件拒绝请求；请检查部署配置")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 || json.Unmarshal(raw, &result) != nil {
		return result, 0, errors.New("更新组件返回格式无效")
	}
	if !validUpdateStatus(result) || strings.Contains(result.Message, c.token) || strings.Contains(result.ReleaseNotes, c.token) || strings.Contains(result.JobID, c.token) {
		return updateStatus{}, 0, errors.New("更新组件状态无效")
	}
	return result, resp.StatusCode, nil
}

func validUpdateStatus(s updateStatus) bool {
	if len(s.CurrentVersion) > 96 || !updateVersionPattern.MatchString(s.CurrentVersion) || len(s.LatestVersion) > 96 || s.LatestVersion != "" && !updateVersionPattern.MatchString(s.LatestVersion) {
		return false
	}
	if s.SHA256 != "" && !updateDigestPattern.MatchString(s.SHA256) || len(s.ReleaseNotes) > 4096 || len(s.Message) > 2048 || len(s.JobID) > 128 {
		return false
	}
	switch s.Status {
	case "current", "available", "unavailable", "updating", "failed", "unconfigured":
		return true
	default:
		return false
	}
}

func (s *Server) serveUpdate(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, Prefix+"/_admin/v1/update")
	method, timeout := http.MethodPost, 20*time.Second
	if path == "/status" {
		method, timeout = http.MethodGet, 3*time.Second
	} else if path == "/apply" {
		timeout = 15 * time.Second
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		failure(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		failure(w, http.StatusBadRequest, "更新接口不接受 URL 参数")
		return
	}
	var raw []byte
	if path == "/check" {
		if !strictJSON(w, r, &struct{}{}) {
			return
		}
		raw = []byte(`{}`)
	} else if path == "/apply" {
		var in struct {
			Version string `json:"version"`
			SHA256  string `json:"sha256"`
			Confirm bool   `json:"confirm"`
		}
		if !strictJSON(w, r, &in) {
			return
		}
		if !in.Confirm || len(in.Version) > 96 || !updateVersionPattern.MatchString(in.Version) || !updateDigestPattern.MatchString(in.SHA256) {
			failure(w, http.StatusBadRequest, "请确认检查结果中的精确版本和 SHA-256 后再更新")
			return
		}
		raw, _ = json.Marshal(in)
	}
	if s.control == nil {
		status := http.StatusOK
		if method == http.MethodPost {
			status = http.StatusServiceUnavailable
		}
		respond(w, status, updateStatus{Configured: false, CurrentVersion: Version, Status: "unconfigured", Message: "未配置部署侧更新组件；请使用 Docker Compose 更新。Sub2API 不需要修改。"})
		return
	}
	result, status, err := s.control.call(r.Context(), method, path, raw, timeout)
	if err != nil {
		failure(w, http.StatusBadGateway, err.Error())
		return
	}
	respond(w, status, result)
}
