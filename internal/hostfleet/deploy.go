package hostfleet

import (
	"archive/tar"
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
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

type Plan struct {
	PlanID         string   `json:"plan_id"`
	ExpiresAt      int64    `json:"expires_at"`
	HostID         string   `json:"host_id"`
	Origin         string   `json:"origin"`
	Arch           string   `json:"arch"`
	ArtifactSHA256 string   `json:"artifact_sha256"`
	Changes        []string `json:"changes"`
	Warnings       []string `json:"warnings"`
	revision       int64
}

func (m *Manager) artifactInfo(arch string) (string, error) {
	if m.artifactDir == "" || (arch != "amd64" && arch != "arm64") {
		return "", errors.New("此 Hub 未配置相应架构的 Edge 部署文件")
	}
	path := filepath.Join(m.artifactDir, "salcara-edge-linux-"+arch)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1024 || info.Size() > 64<<20 {
		return "", errors.New("Edge 部署文件缺失或无效")
	}
	return path, nil
}
func (m *Manager) artifact(arch string) (string, string, error) {
	path, err := m.artifactInfo(arch)
	if err != nil {
		return "", "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", "", errors.New("无法读取 Edge 部署文件")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", "", errors.New("部署文件已变化")
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, io.LimitReader(f, 64<<20+1)); err != nil {
		return "", "", err
	}
	return path, hex.EncodeToString(hash.Sum(nil)), nil
}
func validOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || u.Opaque != "" || (u.Path != "" && u.Path != "/" && u.Path != "/salcara-hub") || strings.ContainsAny(raw, "\x00\r\n\t \"'\\") {
		return "", errors.New("回源地址必须是独立的 HTTPS Hub 地址，不含凭据和参数")
	}
	if _, err = endpoint(u.Hostname(), 443); err != nil {
		return "", errors.New("回源主机无效")
	}
	u.Path = ""
	return u.String(), nil
}
func (m *Manager) Plan(id, origin string) (Plan, error) {
	origin, err := validOrigin(origin)
	if err != nil {
		return Plan{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	h, ok := m.findLocked(id)
	if m.closed || !ok || !h.CanDeploy {
		return Plan{}, errors.New("服务器不存在或不满足自动部署条件")
	}
	if m.busyLocked(id) {
		return Plan{}, errors.New("服务器正在部署")
	}
	if len(m.plans) >= 8 {
		return Plan{}, errors.New("部署预览已达上限，请稍后重试")
	}
	_, hash, err := m.artifact(h.Arch)
	if err != nil {
		return Plan{}, err
	}
	p := Plan{PlanID: randomID(), ExpiresAt: time.Now().Add(10 * time.Minute).UnixMilli(), HostID: id, Origin: origin, Arch: h.Arch, ArtifactSHA256: hash, revision: m.saved.Revision,
		Changes:  []string{"安装经过 SHA-256 校验的 Salcara Edge 二进制到 /opt/salcara-edge-managed", "创建专用运行账户、私有数据目录和独立控制令牌", "配置 salcara-edge-managed.service 并启用开机运行", "仅监听 127.0.0.1:8790（用户转发）与 127.0.0.1:8791（管理）", "启动后通过 SSH 通道验证控制认证和固定 Hub 回源"},
		Warnings: []string{"不安装或修改现有公网反向代理、DNS、防火墙或系统默认路由；公开 HTTPS 入口须单独接通。", "更新已有托管 Edge 会短暂重启服务，已建立连接需要重连。", "只替换本功能创建的服务；端口或目录冲突会拒绝部署。"}}
	m.plans[p.PlanID] = p
	return p, nil
}
func (m *Manager) Deploy(planID string, revision int64) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	p, ok := m.plans[planID]
	if m.closed || !ok {
		return State{}, errors.New("部署方案已过期，请重新生成")
	}
	if revision != m.saved.Revision || p.revision != revision {
		return State{}, errors.New("配置已变化，请重新生成部署方案")
	}
	h, ok := m.findLocked(p.HostID)
	if !ok || m.busyLocked(h.ID) {
		return State{}, errors.New("服务器不存在或已有部署任务")
	}
	running := 0
	for _, j := range m.saved.Jobs {
		if j.Status == "running" {
			running++
		}
	}
	if running >= 2 {
		return State{}, errors.New("最多同时部署两台服务器，请稍后重试")
	}
	if h.Token == "" {
		h.Token = randomID() + randomID()
	}
	now := time.Now().UnixMilli()
	job := Job{ID: randomID(), HostID: h.ID, Status: "running", CreatedAt: now, UpdatedAt: now, Steps: []Step{{At: now, Message: "部署任务已创建，准备重新验证主机与安装文件。"}}}
	next := m.saved
	next.Hosts = append([]record{}, next.Hosts...)
	for i := range next.Hosts {
		if next.Hosts[i].ID == h.ID {
			next.Hosts[i] = h
		}
	}
	next.Jobs = append(append([]Job{}, next.Jobs...), job)
	if len(next.Jobs) > 32 {
		for i, j := range next.Jobs {
			if j.Status != "running" {
				next.Jobs = append(next.Jobs[:i], next.Jobs[i+1:]...)
				break
			}
		}
	}
	next.Revision++
	if err := m.persistLocked(next); err != nil {
		return State{}, err
	}
	delete(m.plans, planID)
	m.wg.Add(1)
	go m.deployJob(job.ID, h, p)
	return m.stateLocked(), nil
}
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

// This fixed, project-owned installer consumes only generated archive members.
// A marker protects unrelated services; rollback restores the previous bundle.
const installScript = `#!/bin/sh
set -eu
stage="$1"
base=/opt/salcara-edge-managed
data=/var/lib/salcara-edge-managed
unit=/etc/systemd/system/salcara-edge-managed.service
test "$(id -u)" = 0
test ! -L "$base" && test ! -L "$data" && test ! -L "$unit"
command -v flock >/dev/null
exec 9>/run/lock/salcara-edge-managed.lock
flock -n 9
if [ -e "$base" ]; then
 test ! -L "$base/.owner" && test -f "$base/.owner"
 test "$(cat "$base/.owner")" = "$(cat "$stage/owner")"
 for file in salcara-edge environment control.token; do test ! -L "$base/$file"; done
else
 test ! -e "$data" && test ! -e "$unit"
 if id salcara-edge-managed >/dev/null 2>&1; then exit 21; fi
 if ss -ltnH | awk '{print $4}' | grep -Eq ':(8790|8791)$'; then exit 22; fi
fi
cd "$stage"
sha256sum -c binary.sha256 >/dev/null
test "$(uname -s)" = Linux
test "$(uname -m)" = "$(cat machine)"
command -v systemctl >/dev/null
command -v curl >/dev/null
old=0
if [ -f "$base/.owner" ] && [ -f "$base/salcara-edge" ] && [ -f "$base/environment" ] && [ -f "$base/control.token" ] && [ -f "$unit" ]; then
 old=1
 cp -a "$base" "$stage/old"
 cp "$unit" "$stage/old.service"
fi
changed=0
installed_unit=0
rollback() {
 rc=$?
 if [ "$rc" -ne 0 ] && [ "$changed" = 1 ]; then
  systemctl stop salcara-edge-managed.service || true
  if [ "$old" = 1 ]; then
   cp "$stage/old/salcara-edge" "$base/salcara-edge"
   cp "$stage/old/environment" "$base/environment"
   cp "$stage/old/control.token" "$base/control.token"
   cp "$stage/old.service" "$unit"
   if [ -f "$stage/old-pool.json" ]; then
    install -m 0600 -o salcara-edge-managed -g salcara-edge-managed "$stage/old-pool.json" "$data/edge-pool.json"
   fi
   systemctl daemon-reload
   systemctl start salcara-edge-managed.service || true
  else
   systemctl disable salcara-edge-managed.service || true
   if [ "$installed_unit" = 1 ]; then rm -f -- "$unit"; systemctl daemon-reload; fi
  fi
 fi
 exit "$rc"
}
trap rollback EXIT
if [ "$old" = 0 ]; then
 if [ ! -d "$base" ]; then
  initial=$(mktemp -d /opt/.salcara-edge-managed.XXXXXXXX)
  install -m 0600 "$stage/owner" "$initial/.owner"
  chmod 0755 "$initial"
  mv "$initial" "$base"
 fi
 changed=1
 if ! id salcara-edge-managed >/dev/null 2>&1; then
  useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin salcara-edge-managed
 fi
 test "$(id -u salcara-edge-managed)" != 0
 install -d -m 0700 -o salcara-edge-managed -g salcara-edge-managed "$data"
fi
changed=1
systemctl stop salcara-edge-managed.service 2>/dev/null || true
if [ "$old" = 1 ]; then
 test ! -L "$data/edge-pool.json" && test -f "$data/edge-pool.json"
 cp -p "$data/edge-pool.json" "$stage/old-pool.json"
fi
install -m 0755 salcara-edge "$base/salcara-edge"
install -m 0600 environment "$base/environment"
install -m 0400 -o salcara-edge-managed -g salcara-edge-managed control.token "$base/control.token"
install -m 0644 service "$unit"
installed_unit=1
systemctl daemon-reload
systemctl enable salcara-edge-managed.service >/dev/null
systemctl start salcara-edge-managed.service
ok=0
for attempt in 1 2 3 4 5 6 7 8; do
 if curl --fail --silent --max-time 3 --config "$stage/curl-control" http://127.0.0.1:8791/v1/state > "$stage/state"; then
  if grep -q '"status":"ready"' "$stage/state"; then ok=1; break; fi
 fi
 sleep 1
done
test "$ok" = 1
printf '%s\n' deployed
`
const serviceUnit = `[Unit]
Description=Salcara managed ingress
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
User=salcara-edge-managed
Group=salcara-edge-managed
EnvironmentFile=/opt/salcara-edge-managed/environment
ExecStart=/opt/salcara-edge-managed/salcara-edge
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/salcara-edge-managed
LimitNOFILE=8192
MemoryMax=512M
TasksMax=128
[Install]
WantedBy=multi-user.target
`

func writeBundle(w io.Writer, binary string, h record, p Plan) error {
	tw := tar.NewWriter(w)
	defer tw.Close()
	add := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data))}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	f, err := os.Open(binary)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() > 64<<20 {
		return errors.New("invalid artifact")
	}
	hash := sha256.New()
	if err = tw.WriteHeader(&tar.Header{Name: "salcara-edge", Mode: 0700, Size: info.Size()}); err != nil {
		return err
	}
	if _, err = io.Copy(io.MultiWriter(tw, hash), f); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != p.ArtifactSHA256 {
		return errors.New("artifact changed during upload")
	}
	env := "SALCARA_EDGE_ORIGIN=" + p.Origin + "\nSALCARA_EDGE_DATA_DIR=/var/lib/salcara-edge-managed\nSALCARA_EDGE_CONTROL_TOKEN_FILE=/opt/salcara-edge-managed/control.token\nSALCARA_EDGE_LISTEN=127.0.0.1:8790\nSALCARA_EDGE_CONTROL_LISTEN=127.0.0.1:8791\nSALCARA_EDGE_TRUSTED_PROXY_CIDRS=127.0.0.1/32,::1/128\n"
	files := map[string]string{"install.sh": installScript, "service": serviceUnit, "environment": env, "control.token": h.Token, "binary.sha256": p.ArtifactSHA256 + "  salcara-edge\n", "machine": map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[p.Arch], "curl-control": "header = \"Authorization: Bearer " + h.Token + "\"\n"}
	ownerHash := sha256.Sum256([]byte(h.Token))
	files["owner"] = "salcara-hostfleet-v1:" + hex.EncodeToString(ownerHash[:])
	for name, data := range files {
		if err = add(name, []byte(data)); err != nil {
			return err
		}
	}
	return tw.Close()
}
func (m *Manager) deployJob(id string, h record, p Plan) {
	defer m.wg.Done()
	ctx, cancel := context.WithTimeout(m.ctx, 3*time.Minute)
	defer cancel()
	err := func() error {
		path, hash, err := m.artifact(p.Arch)
		if err != nil || hash != p.ArtifactSHA256 {
			return errors.New("部署文件已变化，请重新生成方案")
		}
		c, _, err := m.connect(ctx, inputFor(h), false)
		if err != nil {
			return err
		}
		defer c.Close()
		_, arch, okay, _, err := inspect(ctx, c)
		if err != nil || !okay || arch != p.Arch {
			return errors.New("远端环境已变化，已停止部署")
		}
		if err = m.jobStep(id, "主机指纹和环境已确认；正在上传校验过的安装包。"); err != nil {
			return err
		}
		reader, writer := io.Pipe()
		uploaded := make(chan error, 1)
		go func() { e := writeBundle(writer, path, h, p); writer.CloseWithError(e); uploaded <- e }()
		script := "set -eu; umask 077; stage=$(mktemp -d /tmp/salcara-edge.XXXXXXXX); trap 'rm -rf -- \"$stage\"' EXIT; tar -xf - -C \"$stage\"; sh \"$stage/install.sh\" \"$stage\""
		command := "sh -c " + quote(script)
		if h.Username != "root" {
			command = "sudo -n " + command
		}
		output, runErr := run(ctx, c, command, reader)
		reader.Close()
		uploadErr := <-uploaded
		if runErr != nil || uploadErr != nil || strings.TrimSpace(output) != "deployed" {
			return errors.New("安装或回源验证失败；已有托管版本已尝试恢复，请检查远端服务和端口")
		}
		return m.jobStep(id, "Edge 已启动，控制认证与 HTTPS 回源通过；公网 HTTPS 反向代理仍需接通。")
	}()
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.saved
	next.Jobs = append([]Job{}, next.Jobs...)
	next.Hosts = append([]record{}, next.Hosts...)
	for i := range next.Jobs {
		if next.Jobs[i].ID == id {
			next.Jobs[i].UpdatedAt = time.Now().UnixMilli()
			if err != nil {
				next.Jobs[i].Status = "failed"
				next.Jobs[i].Error = err.Error()
				if m.ctx.Err() != nil {
					next.Jobs[i].Status = "interrupted"
				}
			} else {
				next.Jobs[i].Status = "succeeded"
			}
		}
	}
	if err == nil {
		for i := range next.Hosts {
			if next.Hosts[i].ID == h.ID {
				next.Hosts[i].Deployed = true
				next.Hosts[i].Origin = p.Origin
			}
		}
	}
	next.Revision++
	if e := m.persistLocked(next); e != nil { // Persisted running job becomes interrupted on restart.
		for i := range next.Jobs {
			if next.Jobs[i].ID == id {
				next.Jobs[i].Status = "interrupted"
				next.Jobs[i].Error = "无法保存任务结果；请检查远端服务与 Hub 存储。"
			}
		}
		m.saved = next
	}
}
func (m *Manager) jobStep(id, message string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.saved
	next.Jobs = append([]Job{}, next.Jobs...)
	for i := range next.Jobs {
		if next.Jobs[i].ID == id {
			next.Jobs[i].Steps = append(append([]Step{}, next.Jobs[i].Steps...), Step{time.Now().UnixMilli(), message})
			next.Jobs[i].UpdatedAt = time.Now().UnixMilli()
		}
	}
	return m.persistLocked(next)
}
func controlOverSSH(ctx context.Context, c *ssh.Client, token, method, path string, body []byte) (int, []byte, error) {
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return c.DialContext(ctx, "tcp", "127.0.0.1:8791")
	}, ResponseHeaderTimeout: 6 * time.Second, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, method, "http://edge-control/v1/"+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if path == "releases/rollback" {
		client.Timeout = 25 * time.Second
		transport.ResponseHeaderTimeout = 25 * time.Second
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, nil, errors.New("无法通过 SSH 访问 Edge 管理接口")
	}
	defer res.Body.Close()
	limit := int64(1 << 20)
	if path == "sources/state" || path == "observability" || path == "observability/query" {
		limit = 2 << 20
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return 0, nil, errors.New("Edge 管理响应无效")
	}
	return res.StatusCode, raw, nil
}
func (m *Manager) CallControl(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	return m.CallControlFor(ctx, m.ControllerID(), method, path, body)
}

var ErrControllerChanged = errors.New("controller identity changed")

func controllerID(h record) string {
	sum := sha256.Sum256([]byte(h.Token + "\x00" + h.Origin))
	return "host-" + h.ID + "-" + hex.EncodeToString(sum[:8])
}
func (m *Manager) ControllerID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.findLocked(m.saved.ActiveID)
	if m.closed || !ok || !h.Deployed {
		return ""
	}
	return controllerID(h)
}
func (m *Manager) CallControlFor(ctx context.Context, expectedID, method, path string, body []byte) (int, []byte, error) {
	// Defense in depth: no arbitrary SSH forwarding destination or control path.
	allowed := map[string]string{"state": "GET", "nodes": "POST", "nodes/delete": "POST", "policy": "POST", "probe": "POST", "probe/cancel": "POST", "subscriptions/inspect": "POST", "subscriptions/import": "POST"}
	for _, operation := range []string{"sources/save", "sources/refresh", "sources/apply", "sources/delete", "kernel/install", "observability/query", "releases/prepare", "releases/start", "releases/confirm", "releases/rollback"} {
		allowed[operation] = "POST"
	}
	allowed["sources/state"] = "GET"
	allowed["observability"] = "GET"
	allowed["geodata/state"] = "GET"
	for _, operation := range []string{"geodata/install", "geodata/observe", "geodata/configure"} {
		allowed[operation] = "POST"
	}
	if allowed[path] != method {
		return 0, nil, errors.New("unsupported controller operation")
	}
	ctx, done, err := m.begin(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer done()
	m.mu.Lock()
	h, ok := m.findLocked(m.saved.ActiveID)
	m.mu.Unlock()
	if !ok || controllerID(h) != expectedID {
		return 0, nil, ErrControllerChanged
	}
	if !ok || !h.Deployed {
		return 0, nil, errors.New("尚未选择已部署的入口")
	}
	c, _, err := m.connect(ctx, inputFor(h), false)
	if err != nil {
		return 0, nil, err
	}
	defer c.Close()
	return controlOverSSH(ctx, c, h.Token, method, path, body)
}
func (m *Manager) Activate(ctx context.Context, id string, revision int64) (State, error) {
	if id != "" {
		ctx, done, err := m.begin(ctx)
		if err != nil {
			return State{}, err
		}
		defer done()
		m.mu.Lock()
		h, ok := m.findLocked(id)
		valid := !m.closed && m.saved.Revision == revision && !m.busyLocked(id)
		m.mu.Unlock()
		if !ok || !h.Deployed || !valid {
			return State{}, errors.New("请刷新并选择已完成部署的服务器")
		}
		c, _, err := m.connect(ctx, inputFor(h), false)
		if err != nil {
			return State{}, err
		}
		defer c.Close()
		status, raw, err := controlOverSSH(ctx, c, h.Token, "GET", "state", nil)
		var state struct {
			Origin string `json:"origin"`
		}
		if err != nil || status != 200 || json.Unmarshal(raw, &state) != nil || state.Origin != h.Origin {
			return State{}, errors.New("远程控制认证或固定回源验证失败")
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.saved.Revision != revision {
		return State{}, errors.New("配置版本已变化，请刷新后重试")
	}
	next := m.saved
	next.ActiveID = id
	next.Revision++
	if err := m.persistLocked(next); err != nil {
		return State{}, err
	}
	return m.stateLocked(), nil
}
