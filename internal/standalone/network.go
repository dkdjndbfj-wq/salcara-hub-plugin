package standalone

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"salcara/hub/internal/edgepool"
	"salcara/hub/internal/hostfleet"
	"strconv"
	"strings"
	"time"
)

// The edge controller is an operator-configured endpoint, never an address
// supplied by an HTTP request. Its token is independent of the browser session
// and the Hub launcher's update token. No environment proxy or redirect may
// carry this control credential to another service.
type networkControlClient struct {
	base, token string
	client      *http.Client
}

func validateNetworkControlConfig(c Config) error {
	if c.NetworkControlURL == "" && c.NetworkControlTokenFile == "" {
		return nil
	}
	if c.NetworkControlURL == "" || c.NetworkControlTokenFile == "" {
		return errors.New("network control URL and independent token file must both be configured")
	}
	u, err := url.Parse(c.NetworkControlURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || u.Opaque != "" {
		return errors.New("network control URL must be an HTTP(S) origin without credentials or a path")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("network control port is invalid")
		}
	}
	// Cleartext control is allowed only over explicit loopback/private IPs or
	// single-label Docker service names. Use HTTPS for a remote public endpoint.
	if u.Scheme == "http" {
		host := strings.ToLower(u.Hostname())
		ip := net.ParseIP(host)
		private := ip != nil && (ip.IsLoopback() || ip.IsPrivate())
		service := ip == nil && !strings.ContainsAny(host, ".:%") && host != ""
		if !private && !service {
			return errors.New("public network control endpoints require HTTPS")
		}
	}
	p := c.NetworkControlTokenFile
	if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == c.AdminAccountFile || p == c.ControlTokenFile || p == filepath.Join(c.DataDir, initialLoginFile) {
		return errors.New("network control requires a separate absolute clean token file")
	}
	return nil
}

func newNetworkControlClient(c Config) (*networkControlClient, error) {
	if err := validateNetworkControlConfig(c); err != nil {
		return nil, err
	}
	if c.NetworkControlURL == "" {
		return nil, nil
	}
	token, err := readControlToken(c.NetworkControlTokenFile)
	if err != nil {
		return nil, errors.New("cannot load network control token; use a private regular file containing an independent 64-character hexadecimal token")
	}
	return &networkControlClient{
		base: strings.TrimSuffix(c.NetworkControlURL, "/"), token: string(token),
		client: &http.Client{
			Timeout: 8 * time.Second,
			Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
				TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 5 * time.Second, IdleConnTimeout: 60 * time.Second, MaxIdleConnsPerHost: 2},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (s *Server) serveNetworkAdmin(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, Prefix+"/_admin/v1/network/")
	method, known := map[string]string{"state": "GET", "nodes": "POST", "nodes/delete": "POST", "policy": "POST", "probe": "POST", "probe/cancel": "POST", "subscriptions/inspect": "POST", "subscriptions/import": "POST", "sources/state": "GET", "sources/save": "POST", "sources/refresh": "POST", "sources/apply": "POST", "sources/delete": "POST", "kernel/install": "POST", "releases/prepare": "POST", "releases/start": "POST", "releases/confirm": "POST", "releases/rollback": "POST", "observability": "GET", "observability/query": "POST"}[path]
	if geoMethod, ok := map[string]string{"geodata/state": "GET", "geodata/install": "POST", "geodata/observe": "POST", "geodata/configure": "POST"}[path]; ok {
		method, known = geoMethod, true
	}
	if !known {
		failure(w, http.StatusNotFound, "线路管理接口不存在")
		return
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		failure(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		failure(w, http.StatusBadRequest, "线路管理接口不接受 URL 参数")
		return
	}
	if s.stopped.Load() {
		failure(w, http.StatusServiceUnavailable, "Hub 正在重启")
		return
	}
	controllerID := ""
	if s.servers != nil {
		controllerID = s.servers.ControllerID()
	}
	managed := controllerID != ""
	if !managed && s.network != nil {
		sum := sha256.Sum256([]byte(s.network.base + "\x00" + s.network.token))
		controllerID = "external-" + hex.EncodeToString(sum[:12])
	}
	if s.network == nil && !managed {
		if path == "subscriptions/inspect" {
			var input struct {
				URL string `json:"url"`
			}
			if !strictJSONLimit(w, r, &input, 32<<10) {
				return
			}
			preview, err := s.subscriptions.Inspect(r.Context(), input.URL)
			if err != nil {
				failure(w, http.StatusBadRequest, subscriptionErrorMessage(err.Error()))
				return
			}
			// This preview has no remote pool to import into. It is intentionally
			// useful for compatibility checks before provisioning an ingress.
			preview.Warnings = append(preview.Warnings, "入口尚未配置，本次仅检测兼容性；接通入口后需重新检测再导入。")
			respond(w, http.StatusOK, preview)
			return
		}
		if path == "state" {
			respond(w, http.StatusOK, map[string]any{"configured": false, "status": "unconfigured", "reason": "尚未接通入口控制服务。配置 Salcara Edge 后可在这里管理 SOCKS5、HTTP 和 Mihomo 线路。"})
		} else {
			failure(w, http.StatusServiceUnavailable, "入口控制服务尚未配置，不能更改线路")
		}
		return
	}
	var body []byte
	if method == "POST" {
		var input json.RawMessage
		if !strictJSONLimit(w, r, &input, 32<<10) {
			return
		}
		body = bytes.TrimSpace(input)
		if len(body) == 0 || len(body) > 32<<10 || body[0] != '{' {
			failure(w, http.StatusBadRequest, "线路配置必须是小于 32 KiB 的 JSON 对象")
			return
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(body, &fields) != nil {
			failure(w, 400, "线路配置必须是 JSON 对象")
			return
		}
		var expectedID string
		if raw, ok := fields["expected_controller_id"]; ok {
			if json.Unmarshal(raw, &expectedID) != nil {
				failure(w, 400, "入口标识无效")
				return
			}
		}
		// Existing deployment-configured clients may omit the identity; managed
		// controllers require it because the target can change without restart.
		if (managed || expectedID != "") && expectedID != controllerID {
			failure(w, 409, "入口已切换，请刷新后重新打开操作")
			return
		}
		delete(fields, "expected_controller_id")
		body, _ = json.Marshal(fields)
	}
	var status int
	var output []byte
	var err error
	if managed {
		status, output, err = s.servers.CallControlFor(r.Context(), controllerID, method, path, body)
	} else {
		status, output, err = s.network.call(r.Context(), method, path, body)
	}
	if err != nil {
		if errors.Is(err, hostfleet.ErrControllerChanged) {
			failure(w, 409, "入口已切换，请刷新后重试")
			return
		}
		failure(w, http.StatusBadGateway, "无法联系入口控制服务，请检查连接；未确认操作是否完成，刷新状态后再操作")
		return
	}
	if status < 200 || status >= 300 {
		// Never relay arbitrary controller errors, which could contain upstream
		// URLs, credentials, or HTML. A conflict remains distinguishable to UI.
		messages := map[int]string{
			http.StatusBadRequest:      "线路配置无效，请检查名称、代理地址及策略选项",
			http.StatusNotFound:        "线路已不存在，请刷新后重试",
			http.StatusConflict:        "线路配置已被修改，请刷新最新配置后重试",
			http.StatusTooManyRequests: "检测或配置操作正在进行，请稍后刷新",
		}
		var problem struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(output, &problem)
		codes := map[string]string{
			"invalid_source_limits":           "订阅共享额度无效，请填写 1–4096 的请求和长连接上限",
			"source_limits_require_release":   "订阅共享额度必须先检查并试用，再确认发布",
			"geo_policy_dependency":           "当前严格地区规则依赖网络归属，请先在策略中取消使用观察地区，再暂停观察",
			"geo_busy":                        "网络归属任务正在运行，请等待完成",
			"geo_not_installed":               "请先安装本地网络归属数据库",
			"geo_not_enabled":                 "请先开启网络归属观察",
			"geo_database_expired":            "本地网络归属数据库已过期，请更新后重试",
			"source_busy":                     "订阅或内核任务正在运行，请等待任务完成",
			"source_in_use":                   "活动配置或保留的历史版本仍引用此订阅，暂不能删除",
			"source_identity_ambiguous":       "订阅包含无法区分身份的节点，已保留原配置，请核对重复节点",
			"source_candidate_unreachable":    "候选节点未通过固定 Hub 回源验证，原配置继续保留",
			"source_candidate_expired":        "候选版本已过期，请重新刷新订阅",
			"invalid_source_selection":        "请选择有效节点，活动池最多 32 条线路",
			"source_storage_failed":           "订阅存储未能完成，请检查数据卷后刷新实际状态",
			"kernel_not_installed":            "请先安装受管 Mihomo 内核，再应用复杂协议节点",
			"kernel_draining":                 "上一版内核仍在承载连接，请排空后再发布下一版",
			"no_eligible_route":               "发布后没有符合当前策略且可达的线路，原配置已保留",
			"rollback_dependency_unavailable": "历史版本所需的订阅或内核尚不可用，已保留当前配置",
			"trial_in_progress":               "配置正在试用，请先确认或回退，再进行其他修改",
			"release_preview_expired":         "候选验证结果已过期，请重新检查配置",
			"invalid_node_region":             "地区标签需为不超过 32 字节的文字，不能包含控制字符",
			"invalid_policy_settings":         "检测或调度设置无效：请核对数值范围、超时与间隔，以及地区偏好",
			"invalid_probe_scope":             "检测范围无效，请选择启用节点或全部节点",
			"invalid_probe_nodes":             "待检测节点已变化或选择无效，请刷新后重试",
			"https_origin_required":           "经代理回源必须使用 HTTPS，请先修改入口服务的固定 Hub 回源地址",
			"subscription_url_invalid":        "订阅地址无效，需要不含用户名密码的公网 HTTPS 地址",
			"subscription_fetch_failed":       "无法获取订阅，请检查地址、有效期和服务端连通性",
			"subscription_format_unsupported": "未识别到支持的订阅格式，请使用 Clash 节点配置或代理 URI 列表",
			"subscription_no_supported_nodes": "没有可直接导入的节点；复杂协议请通过 Mihomo 的 SOCKS5 或 mixed 端口接入",
			"preview_expired":                 "检测结果已过期，请重新检测订阅",
			"node_limit":                      "节点池最多支持 32 条线路，请先移除不需要的线路",
			"subscription_limit":              "订阅检测正在进行或预览数量已达上限，请稍后重试",
		}
		if translated, ok := codes[problem.Code]; ok && (status == http.StatusBadRequest || status == http.StatusConflict) {
			messages[status] = translated
		}
		message, ok := messages[status]
		if !ok {
			status, message = http.StatusBadGateway, "入口控制服务拒绝或未能完成操作，请检查服务连接与凭据"
		}
		failure(w, status, message)
		return
	}
	if strings.HasPrefix(path, "geodata/") {
		var state edgepool.GeoState
		if json.Unmarshal(output, &state) != nil || state.Revision < 1 {
			failure(w, 502, "网络归属状态无效")
			return
		}
		respond(w, status, struct {
			edgepool.GeoState
			Configured   bool   `json:"configured"`
			ControllerID string `json:"controller_id"`
		}{state, true, controllerID})
		return
	}
	var value map[string]json.RawMessage
	if json.Unmarshal(output, &value) != nil || value == nil {
		failure(w, http.StatusBadGateway, "入口控制服务返回的数据无效")
		return
	}
	// Only the controller's public state schema can reach the browser.
	allowed := map[string]bool{"revision": true, "origin": true, "mode": true, "selected_id": true, "active_id": true, "nodes": true, "events": true, "status": true, "probe_running": true, "durability": true, "accepted": true, "format": true, "total": true, "supported": true, "unsupported": true, "warnings": true, "preview_id": true, "expires_at": true, "limits": true, "runtime": true, "health_policy": true, "selection_policy": true, "next_probe_at": true, "probe_run": true}
	for _, key := range []string{"release", "source_revision", "sources", "kernel", "busy", "sample_interval_ms", "retention_ms", "counter_semantics", "samples", "decision", "last_error", "geo_available"} {
		allowed[key] = true
	}
	for key, clean := range map[string]func(json.RawMessage) (json.RawMessage, error){"source_limits": publicNetworkJSON[[]edgepool.SourceLimit], "source_groups": publicNetworkJSON[[]edgepool.SourceGroupView]} {
		allowed[key] = true
		if raw, ok := value[key]; ok {
			filtered, err := clean(raw)
			if err != nil {
				failure(w, 502, "订阅共享容量数据无效")
				return
			}
			value[key] = filtered
		}
	}
	for key := range value {
		if !allowed[key] {
			delete(value, key)
		}
	}
	for key, clean := range map[string]func(json.RawMessage) (json.RawMessage, error){"limits": publicNetworkJSON[edgepool.Limits], "runtime": publicNetworkJSON[edgepool.Runtime], "health_policy": publicNetworkJSON[edgepool.HealthPolicy], "selection_policy": publicNetworkJSON[edgepool.SelectionPolicy], "next_probe_at": publicNetworkJSON[int64], "probe_run": publicNetworkJSON[*edgepool.ProbeRun], "release": publicNetworkJSON[edgepool.ReleaseState], "source_revision": publicNetworkJSON[int64], "sources": publicNetworkJSON[[]edgepool.SourceView], "kernel": publicNetworkJSON[edgepool.KernelView], "busy": publicNetworkJSON[bool], "sample_interval_ms": publicNetworkJSON[int64], "retention_ms": publicNetworkJSON[int64], "counter_semantics": publicNetworkJSON[string], "samples": publicNetworkJSON[[]edgepool.TrendSample], "decision": publicNetworkJSON[edgepool.RouteDecision], "last_error": publicNetworkJSON[string]} {
		if raw, ok := value[key]; ok {
			filtered, e := clean(raw)
			if e != nil {
				failure(w, 502, "入口统计数据无效")
				return
			}
			value[key] = filtered
		}
	}
	if raw, ok := value["nodes"]; ok {
		var nodes []map[string]json.RawMessage
		if json.Unmarshal(raw, &nodes) != nil {
			failure(w, http.StatusBadGateway, "入口节点数据无效")
			return
		}
		fields := map[string]bool{"id": true, "index": true, "name": true, "proxy_url": true, "enabled": true, "has_credentials": true, "health": true, "protocol": true, "supported": true, "reason": true, "weight": true, "max_requests": true, "max_streams": true, "draining": true, "runtime": true, "recovering": true, "region": true}
		fields["source_id"] = true
		fields["network"] = true
		fields["effective_region"] = true
		for _, node := range nodes {
			for key := range node {
				if !fields[key] {
					delete(node, key)
				}
			}
			for key, clean := range map[string]func(json.RawMessage) (json.RawMessage, error){"runtime": publicNetworkJSON[edgepool.Runtime], "health": publicNetworkJSON[edgepool.Health], "region": publicNetworkJSON[string], "effective_region": publicNetworkJSON[string], "network": publicNetworkJSON[edgepool.NetworkInfo]} {
				if raw, ok := node[key]; ok {
					filtered, e := clean(raw)
					if e != nil {
						failure(w, 502, "入口负载数据无效")
						return
					}
					node[key] = filtered
				}
			}
			if rawURL, ok := node["proxy_url"]; ok {
				var endpoint string
				_ = json.Unmarshal(rawURL, &endpoint)
				if u, err := url.Parse(endpoint); err == nil && u.Hostname() != "" {
					u.User, u.RawQuery, u.Fragment, u.Path, u.RawPath = nil, "", "", "", ""
					endpoint = u.String()
				} else {
					endpoint = ""
				}
				node["proxy_url"], _ = json.Marshal(endpoint)
			}
		}
		value["nodes"], _ = json.Marshal(nodes)
	}
	value["configured"] = json.RawMessage("true")
	value["controller_id"], _ = json.Marshal(controllerID)
	respond(w, status, value)
}

func publicNetworkJSON[T any](raw json.RawMessage) (json.RawMessage, error) {
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func subscriptionErrorMessage(code string) string {
	switch code {
	case "subscription_url_invalid":
		return "订阅地址无效，请使用不含用户名密码的公网 HTTPS 地址；不接受内网或本机地址"
	case "subscription_limit":
		return "订阅检测正在进行或预览数量已达上限，请稍后重试"
	case "subscription_format_unsupported":
		return "未识别到支持的订阅格式，请使用 Clash 节点配置或代理 URI 列表"
	default:
		return "订阅检测失败，请检查地址、订阅有效期和服务端网络；原有线路未改变"
	}
}

func (c *networkControlClient) call(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/v1/"+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, errors.New("invalid control request")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := *c.client
	if path == "releases/rollback" {
		client.Timeout = 25 * time.Second
		transport := c.client.Transport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = 25 * time.Second
		client.Transport = transport
		defer transport.CloseIdleConnections()
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, nil, errors.New("network controller unavailable")
	}
	defer res.Body.Close()
	limit := int64(256 << 10)
	if path == "sources/state" || path == "observability" || path == "observability/query" {
		limit = 2 << 20
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return 0, nil, errors.New("network controller response too large or incomplete")
	}
	return res.StatusCode, data, nil
}
