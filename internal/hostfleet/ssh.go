package hostfleet

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

type dialFunc func(context.Context, string, string) (net.Conn, error)
type Input struct {
	Name        string `json:"name"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Username    string `json:"username"`
	Fingerprint string `json:"fingerprint"`
	Credentials
}
type Discovery struct {
	Fingerprint string `json:"fingerprint"`
	Algorithm   string `json:"algorithm"`
}
type Check struct {
	CheckID   string   `json:"check_id"`
	ExpiresAt int64    `json:"expires_at"`
	Platform  string   `json:"platform"`
	Arch      string   `json:"arch"`
	CanDeploy bool     `json:"can_deploy"`
	Checks    []string `json:"checks"`
	Warnings  []string `json:"warnings"`
}

var usernameRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
var hostRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]{0,252}$`)

func endpoint(host string, port int) (string, error) {
	if port < 1 || port > 65535 || strings.TrimSpace(host) != host {
		return "", errors.New("请输入有效主机和 SSH 端口")
	}
	if ip, err := netip.ParseAddr(host); err != nil {
		if !hostRE.MatchString(host) || strings.Contains(host, "..") {
			return "", errors.New("主机必须是 IP 或域名")
		}
	} else if ip.Zone() != "" {
		return "", errors.New("不支持带区域标识的 IP")
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}
func publicDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("invalid SSH address")
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 || len(ips) > 32 {
		return nil, errors.New("SSH DNS lookup failed")
	}
	for _, ip := range ips {
		ip = ip.Unmap()
		if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || netip.MustParsePrefix("100.64.0.0/10").Contains(ip) || netip.MustParsePrefix("198.18.0.0/15").Contains(ip) || netip.MustParsePrefix("192.0.0.0/24").Contains(ip) || netip.MustParsePrefix("2001:db8::/32").Contains(ip) || netip.MustParsePrefix("64:ff9b::/96").Contains(ip) {
			return nil, errors.New("SSH target must resolve only to public IP addresses")
		}
	}
	for _, ip := range ips {
		c, e := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if e == nil {
			return c, nil
		}
	}
	return nil, errors.New("SSH connection failed")
}
func (m *Manager) connect(ctx context.Context, in Input, discover bool) (*ssh.Client, Discovery, error) {
	address, err := endpoint(in.Host, in.Port)
	if err != nil {
		return nil, Discovery{}, err
	}
	if !discover && (!usernameRE.MatchString(in.Username) || !strings.HasPrefix(in.Fingerprint, "SHA256:")) {
		return nil, Discovery{}, errors.New("请填写有效用户名并先核对主机指纹")
	}
	conn, err := m.dial(ctx, "tcp", address)
	if err != nil {
		return nil, Discovery{}, errors.New("无法连接 SSH；请检查公网地址、端口与网络")
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	observed := Discovery{}
	mismatch := false
	cfg := &ssh.ClientConfig{User: in.Username, Timeout: 5 * time.Second, HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
		observed = Discovery{ssh.FingerprintSHA256(key), key.Type()}
		if discover {
			return errors.New("discovery complete before authentication")
		}
		if observed.Fingerprint != in.Fingerprint {
			mismatch = true
			return errors.New("host key mismatch")
		}
		return nil
	}}
	if !discover {
		switch in.AuthType {
		case "password":
			if in.Password == "" || len(in.Password) > 4096 {
				conn.Close()
				return nil, observed, errors.New("SSH 密码为空或过长")
			}
			cfg.Auth = []ssh.AuthMethod{ssh.Password(in.Password)}
		case "key":
			if len(in.PrivateKey) > 64<<10 || len(in.Passphrase) > 4096 {
				conn.Close()
				return nil, observed, errors.New("SSH 密钥过长")
			}
			var key ssh.Signer
			if in.Passphrase != "" {
				key, err = ssh.ParsePrivateKeyWithPassphrase([]byte(in.PrivateKey), []byte(in.Passphrase))
			} else {
				key, err = ssh.ParsePrivateKey([]byte(in.PrivateKey))
			}
			if err != nil {
				conn.Close()
				return nil, observed, errors.New("SSH 私钥或口令无效")
			}
			cfg.Auth = []ssh.AuthMethod{ssh.PublicKeys(key)}
		default:
			conn.Close()
			return nil, observed, errors.New("请选择密码或私钥认证")
		}
	}
	cc, ch, reqs, err := ssh.NewClientConn(conn, address, cfg)
	if err != nil {
		conn.Close()
		if discover && observed.Fingerprint != "" {
			return nil, observed, nil
		}
		if mismatch {
			return nil, observed, errors.New("主机指纹已变化，已停止认证；请重新核对")
		}
		return nil, observed, errors.New("SSH 握手或认证失败，请检查用户名与凭据")
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(cc, ch, reqs), observed, nil
}
func inputFor(h record) Input {
	return Input{Name: h.Name, Host: h.Host.Host, Port: h.Port, Username: h.Username, Fingerprint: h.Fingerprint, Credentials: h.Secret}
}

type boundedOutput struct {
	bytes.Buffer
	limit int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len() < b.limit {
		keep := min(len(p), b.limit-b.Len())
		_, _ = b.Buffer.Write(p[:keep])
	}
	return n, nil
}
func run(ctx context.Context, c *ssh.Client, command string, stdin io.Reader) (string, error) {
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	s, err := c.NewSession()
	if err != nil {
		return "", errors.New("无法创建 SSH 执行通道")
	}
	defer s.Close()
	out := &boundedOutput{limit: 32 << 10}
	s.Stdout = out
	s.Stderr = io.Discard
	s.Stdin = stdin
	if err = s.Run(command); err != nil {
		return "", errors.New("远程命令失败或超时")
	}
	return out.String(), nil
}
func (m *Manager) Discover(ctx context.Context, host string, port int) (Discovery, error) {
	ctx, done, err := m.begin(ctx)
	if err != nil {
		return Discovery{}, err
	}
	defer done()
	_, d, err := m.connect(ctx, Input{Host: host, Port: port}, true)
	return d, err
}

const inspectCommand = `set -eu; uname -s; uname -m; if [ "$(id -u)" = 0 ]; then echo root; elif sudo -n true 2>/dev/null; then echo sudo; else echo unprivileged; fi; if command -v systemctl >/dev/null && [ -d /run/systemd/system ]; then echo systemd; else echo no-systemd; fi; if command -v sha256sum >/dev/null && command -v curl >/dev/null && command -v ss >/dev/null && command -v flock >/dev/null && command -v tar >/dev/null && command -v useradd >/dev/null && command -v install >/dev/null; then echo tools-ready; else echo tools-missing; fi`

func inspect(ctx context.Context, c *ssh.Client) (string, string, bool, []string, error) {
	out, err := run(ctx, c, inspectCommand, nil)
	if err != nil {
		return "", "", false, nil, errors.New("服务器环境检查失败")
	}
	parts := strings.Fields(out)
	if len(parts) != 5 {
		return "", "", false, nil, errors.New("服务器环境返回无效")
	}
	platform := strings.ToLower(parts[0])
	arch := map[string]string{"x86_64": "amd64", "aarch64": "arm64"}[parts[1]]
	okay := platform == "linux" && arch != "" && (parts[2] == "root" || parts[2] == "sudo") && parts[3] == "systemd" && parts[4] == "tools-ready"
	checks := []string{"系统：" + parts[0] + " / " + parts[1], "权限：" + parts[2], "服务管理：" + parts[3], "校验与探测工具：" + parts[4]}
	return platform, arch, okay, checks, nil
}
func (m *Manager) Check(ctx context.Context, in Input) (Check, error) {
	ctx, done, err := m.begin(ctx)
	if err != nil {
		return Check{}, err
	}
	defer done()
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > 160 || strings.ContainsAny(in.Name, "\x00\r\n") {
		return Check{}, errors.New("请输入不超过160字节的服务器名称")
	}
	c, _, err := m.connect(ctx, in, false)
	if err != nil {
		return Check{}, err
	}
	defer c.Close()
	platform, arch, okay, checks, err := inspect(ctx, c)
	if err != nil {
		return Check{}, err
	}
	h := record{Host: Host{ID: randomID(), Name: strings.TrimSpace(in.Name), Host: in.Host, Port: in.Port, Username: in.Username, AuthType: in.AuthType, Fingerprint: in.Fingerprint, Platform: platform, Arch: arch, CanDeploy: okay, LastCheckedAt: time.Now().UnixMilli()}, Secret: in.Credentials}
	// Retain only the selected authentication material.
	if h.Secret.AuthType == "key" {
		h.Secret.Password = ""
	} else {
		h.Secret.PrivateKey = ""
		h.Secret.Passphrase = ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	if m.closed || len(m.checks) >= 8 {
		return Check{}, errors.New("检查结果已达上限或服务停止，请稍后重试")
	}
	id := randomID()
	expiry := time.Now().Add(10 * time.Minute)
	m.checks[id] = checked{h, expiry}
	warnings := []string{"SSH 可登录仅证明管理连接可用，尚未部署或验证公网入口。"}
	if !okay {
		warnings = append(warnings, "自动部署要求 Linux amd64/arm64、systemd、curl、sha256sum，以及 root 或免交互 sudo。")
	}
	return Check{id, expiry.UnixMilli(), platform, arch, okay, checks, warnings}, nil
}
func (m *Manager) Save(checkID string, revision int64) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	if m.closed || revision != m.saved.Revision {
		return State{}, errors.New("配置版本已变化，请刷新后重试")
	}
	check, ok := m.checks[checkID]
	if !ok {
		return State{}, errors.New("检查结果已过期，请重新检查")
	}
	next := m.saved
	next.Hosts = append([]record{}, next.Hosts...)
	found := false
	for i, h := range next.Hosts {
		if h.Host.Host == check.host.Host.Host && h.Port == check.host.Port && h.Username == check.host.Username {
			if m.busyLocked(h.ID) {
				return State{}, errors.New("该服务器正在部署")
			}
			check.host.ID = h.ID
			check.host.Deployed = h.Deployed
			check.host.Origin = h.Origin
			check.host.Token = h.Token
			next.Hosts[i] = check.host
			found = true
		}
	}
	if !found {
		if len(next.Hosts) >= 16 {
			return State{}, errors.New("最多管理16台服务器")
		}
		next.Hosts = append(next.Hosts, check.host)
	}
	next.Revision++
	if err := m.persistLocked(next); err != nil {
		return State{}, err
	}
	delete(m.checks, checkID)
	return m.stateLocked(), nil
}
func (m *Manager) Delete(id string, revision int64) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || revision != m.saved.Revision {
		return State{}, errors.New("配置版本已变化，请刷新后重试")
	}
	if m.saved.ActiveID == id || m.busyLocked(id) {
		return State{}, errors.New("请先取消活动入口或等待部署结束")
	}
	next := m.saved
	next.Hosts = append([]record{}, next.Hosts...)
	found := false
	for i, h := range next.Hosts {
		if h.ID == id {
			next.Hosts = append(next.Hosts[:i], next.Hosts[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		return State{}, errors.New("服务器不存在")
	}
	next.Revision++
	if err := m.persistLocked(next); err != nil {
		return State{}, err
	}
	return m.stateLocked(), nil
}
