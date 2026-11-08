package edgepool

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const kernelVersion = "v1.19.33"

var kernelArchives = map[string]struct{ Name, Hash string }{
	"linux/amd64":   {"mihomo-linux-amd64-compatible-v1.19.33.gz", "386508bfb1c72c43b6a0eda73708ecdce82546581b0ebcb16366c53757c622b6"},
	"linux/arm64":   {"mihomo-linux-arm64-v1.19.33.gz", "ba408b317080ee265c46cb923a50018c1b82cbd708551cdd46a1e3e307396187"},
	"windows/amd64": {"mihomo-windows-amd64-compatible-v1.19.33.zip", "7aaf636a4f2172dcd8ec34ac4f52797b059195e5afd6409508c963e545f8864d"},
}

type kernelInstallation struct {
	Version    string `json:"version"`
	BinaryHash string `json:"binary_hash"`
}
type kernelGeneration struct {
	ID        string                `json:"id"`
	Control   string                `json:"control"`
	Secret    string                `json:"secret"`
	URLs      map[string]string     `json:"urls"`
	Config    map[string]any        `json:"config"`
	CreatedAt int64                 `json:"created_at"`
	Members   map[string]sourceNode `json:"members,omitempty"`
	cmd       *exec.Cmd
	done      chan struct{}
	retiring  bool
	holds     int // an uncommitted rollback owns this runtime
	failures  int
	retryAt   time.Time
}
type kernelManager struct {
	opMu             sync.Mutex // process operations; never held by View
	mu               sync.Mutex
	dir              string
	ctx              context.Context
	cancel           context.CancelFunc
	phase, lastError string
	installed        bool
	generations      map[string]*kernelGeneration
	active           string
	closed           bool
	pin              func(context.Context, map[string]any) (map[string]any, error)
}

func newKernelManager(dir string, ctx context.Context) *kernelManager {
	ctx, cancel := context.WithCancel(ctx)
	m := &kernelManager{dir: filepath.Join(dir, "managed-kernel"), ctx: ctx, cancel: cancel, phase: "not_installed", generations: map[string]*kernelGeneration{}, pin: pinKernelProxy}
	if info, err := os.Lstat(m.dir); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		m.lastError = "invalid_kernel_directory"
		return m
	}
	if os.MkdirAll(m.dir, 0700) != nil {
		m.lastError = "kernel_storage_failed"
		return m
	}
	if m.verifyBinary() == nil {
		m.installed = true
		m.phase = "ready"
	}
	// Runtime files can always be reconstructed from the encrypted registry.
	// At startup no managed process owns these directories yet.
	entries, _ := os.ReadDir(m.dir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "generation-") {
			m.removeGeneration(strings.TrimPrefix(entry.Name(), "generation-"))
		}
	}
	return m
}
func (m *kernelManager) binary() string {
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	return filepath.Join(m.dir, "mihomo-"+kernelVersion+suffix)
}
func (m *kernelManager) verifyBinary() error {
	raw, err := readSourcePrivate(filepath.Join(m.dir, "installation.json"), 4096)
	if err != nil {
		return err
	}
	var v kernelInstallation
	if json.Unmarshal(raw, &v) != nil || v.Version != kernelVersion {
		return errors.New("kernel_manifest_invalid")
	}
	info, err := os.Lstat(m.binary())
	if err != nil || !info.Mode().IsRegular() || !privateMode(info) || info.Size() > 200<<20 {
		return errors.New("kernel_binary_invalid")
	}
	f, err := os.Open(m.binary())
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, io.LimitReader(f, 200<<20)); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != v.BinaryHash {
		return errors.New("kernel_hash_mismatch")
	}
	return nil
}
func (m *kernelManager) View() KernelView {
	m.mu.Lock()
	defer m.mu.Unlock()
	return KernelView{Version: kernelVersion, Installed: m.installed, Status: m.phase, Error: m.lastError, Generations: len(m.generations)}
}
func (m *kernelManager) setError(code string) {
	m.mu.Lock()
	m.lastError = code
	m.phase = "failed"
	m.mu.Unlock()
}
func (m *kernelManager) install() error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if info, err := os.Lstat(m.dir); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !privateMode(info) {
		return errors.New("kernel_storage_failed")
	}
	asset, ok := kernelArchives[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return errors.New("kernel_platform_unsupported")
	}
	m.mu.Lock()
	if m.closed || m.phase == "installing" {
		m.mu.Unlock()
		return errors.New("source_busy")
	}
	if m.installed {
		m.mu.Unlock()
		if m.verifyBinary() == nil {
			return nil
		}
		m.mu.Lock()
		if len(m.generations) > 0 {
			m.mu.Unlock()
			return errors.New("kernel_hash_mismatch")
		}
		m.installed = false
	}
	m.phase = "installing"
	m.lastError = ""
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(m.ctx, 3*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 3 * time.Minute, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) > 4 || r.URL.Scheme != "https" || (r.URL.Hostname() != "github.com" && r.URL.Hostname() != "release-assets.githubusercontent.com" && r.URL.Hostname() != "objects.githubusercontent.com") {
			return errors.New("kernel_download_redirect")
		}
		return nil
	}}
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://github.com/MetaCubeX/mihomo/releases/download/"+kernelVersion+"/"+asset.Name, nil)
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("kernel_download_failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.ContentLength > 64<<20 {
		return errors.New("kernel_download_failed")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (64<<20)+1))
	if err != nil || len(raw) > 64<<20 {
		return errors.New("kernel_download_failed")
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != asset.Hash {
		return errors.New("kernel_hash_mismatch")
	}
	var binary []byte
	if strings.HasSuffix(asset.Name, ".gz") {
		gz, e := gzip.NewReader(bytes.NewReader(raw))
		if e != nil {
			return errors.New("kernel_archive_invalid")
		}
		binary, err = io.ReadAll(io.LimitReader(gz, (200<<20)+1))
		_ = gz.Close()
	} else {
		z, e := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
		if e != nil {
			return errors.New("kernel_archive_invalid")
		}
		for _, entry := range z.File {
			if strings.HasSuffix(entry.Name, ".exe") {
				if binary != nil || entry.UncompressedSize64 > 200<<20 {
					return errors.New("kernel_archive_invalid")
				}
				f, e := entry.Open()
				if e != nil {
					return errors.New("kernel_archive_invalid")
				}
				binary, err = io.ReadAll(io.LimitReader(f, (200<<20)+1))
				_ = f.Close()
			}
		}
	}
	if err != nil || len(binary) == 0 || len(binary) > 200<<20 {
		return errors.New("kernel_archive_invalid")
	}
	if writeSourcePrivate(m.binary(), binary) != nil || os.Chmod(m.binary(), 0700) != nil {
		return errors.New("kernel_storage_failed")
	}
	sum = sha256.Sum256(binary)
	manifest, _ := json.Marshal(kernelInstallation{kernelVersion, hex.EncodeToString(sum[:])})
	if writeSourcePrivate(filepath.Join(m.dir, "installation.json"), manifest) != nil {
		return errors.New("kernel_storage_failed")
	}
	m.mu.Lock()
	m.installed = true
	m.phase = "ready"
	m.lastError = ""
	m.mu.Unlock()
	return nil
}
func kernelNodeDigest(proxy map[string]any) string {
	copy := map[string]any{}
	for k, v := range proxy {
		if k != "name" {
			copy[k] = v
		}
	}
	raw, _ := json.Marshal(copy)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func reserveKernelPort() (net.Listener, int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, err
	}
	return l, l.Addr().(*net.TCPAddr).Port, nil
}
func kernelCommand(ctx context.Context, binary string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, args...)
	configureKernelProcess(cmd)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.Env = []string{}
	for _, key := range []string{"SystemRoot", "WINDIR", "PATH", "TEMP", "TMP", "TMPDIR", "HOME"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.WaitDelay = 2 * time.Second
	return cmd
}
func (m *kernelManager) Prepare(ctx context.Context, nodes map[string]map[string]any) (*kernelGeneration, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	if m.closed || !m.installed {
		m.mu.Unlock()
		return nil, errors.New("kernel_not_installed")
	}
	if len(m.generations) >= 2 {
		m.mu.Unlock()
		return nil, errors.New("kernel_draining")
	}
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(m.ctx, cancel)
	defer func() { stop(); cancel() }()
	if err := m.verifyBinary(); err != nil {
		return nil, errors.New("kernel_hash_mismatch")
	}
	g := &kernelGeneration{ID: sourceID(), Secret: sourceID() + sourceID(), URLs: map[string]string{}, CreatedAt: time.Now().UnixMilli()}
	listeners := []any{}
	proxies := []any{}
	held := []net.Listener{}
	defer func() {
		for _, l := range held {
			_ = l.Close()
		}
	}()
	control, port, err := reserveKernelPort()
	if err != nil {
		return nil, errors.New("kernel_ports_unavailable")
	}
	held = append(held, control)
	g.Control = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for key, original := range nodes {
		proxy, err := m.pin(ctx, original)
		if err != nil {
			return nil, err
		}
		name := "node-" + key
		proxy["name"] = name
		l, port, err := reserveKernelPort()
		if err != nil {
			return nil, errors.New("kernel_ports_unavailable")
		}
		held = append(held, l)
		listeners = append(listeners, map[string]any{"name": "in-" + key, "type": "http", "listen": "127.0.0.1", "port": port, "proxy": name, "users": []any{map[string]any{"username": "salcara", "password": g.Secret}}})
		proxies = append(proxies, proxy)
		g.URLs[key] = (&url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), User: url.UserPassword("salcara", g.Secret)}).String()
	}
	g.Config = map[string]any{"mode": "rule", "log-level": "silent", "allow-lan": false, "ipv6": false, "find-process-mode": "off", "external-controller": g.Control, "secret": g.Secret, "listeners": listeners, "proxies": proxies, "rules": []string{"MATCH,REJECT"}, "dns": map[string]any{"enable": false}, "tun": map[string]any{"enable": false}, "sniffer": map[string]any{"enable": false}, "profile": map[string]any{"store-selected": false, "store-fake-ip": false}, "geo-auto-update": false}
	dir := filepath.Join(m.dir, "generation-"+g.ID)
	if os.Mkdir(dir, 0700) != nil {
		return nil, errors.New("kernel_storage_failed")
	}
	prepared := false
	defer func() {
		if !prepared {
			stopKernelGeneration(g)
			m.removeGeneration(g.ID)
		}
	}()
	raw, _ := json.Marshal(g.Config)
	if writeSourcePrivate(filepath.Join(dir, "config.json"), raw) != nil {
		return nil, errors.New("kernel_storage_failed")
	}
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	check := kernelCommand(checkCtx, m.binary(), "-t", "-d", dir, "-f", filepath.Join(dir, "config.json"))
	cleanup, startErr := startKernelProcess(check)
	err = startErr
	if err == nil {
		err = check.Wait()
		cleanup()
	}
	cancel()
	if err != nil {
		return nil, errors.New("kernel_configuration_rejected")
	}
	for _, l := range held {
		_ = l.Close()
	}
	held = nil
	if err = m.startLocked(g); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.generations[g.ID] = g
	m.mu.Unlock()
	prepared = true
	return g, nil
}
func (m *kernelManager) startLocked(g *kernelGeneration) error {
	dir := filepath.Join(m.dir, "generation-"+g.ID)
	g.cmd = kernelCommand(m.ctx, m.binary(), "-d", dir, "-f", filepath.Join(dir, "config.json"))
	cleanup, err := startKernelProcess(g.cmd)
	if err != nil {
		return errors.New("kernel_start_failed")
	}
	g.done = make(chan struct{})
	go func(cmd *exec.Cmd, done chan struct{}) { _ = cmd.Wait(); cleanup(); close(done) }(g.cmd, g.done)
	ctx, cancel := context.WithTimeout(m.ctx, 8*time.Second)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-g.done:
			return errors.New("kernel_start_failed")
		case <-ctx.Done():
			stopKernelGeneration(g)
			return errors.New("kernel_start_failed")
		case <-tick.C:
			if _, err := kernelControl(ctx, g, "/version"); err == nil {
				return nil
			}
		}
	}
}

func (m *kernelManager) Restore(ctx context.Context, saved *kernelGeneration) (*kernelGeneration, bool, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	if m.closed || !m.installed {
		m.mu.Unlock()
		return nil, false, errors.New("kernel_not_installed")
	}
	if existing := m.generations[saved.ID]; existing != nil {
		m.mu.Unlock()
		if _, err := kernelControl(ctx, existing, "/version"); err != nil {
			return nil, false, errors.New("kernel_process_failed")
		}
		existing.holds++
		return existing, false, nil
	}
	full := len(m.generations) >= 2
	m.mu.Unlock()
	if full {
		return nil, false, errors.New("kernel_draining")
	}
	if ctx.Err() != nil {
		return nil, false, errors.New("operation_interrupted")
	}
	if m.verifyBinary() != nil {
		return nil, false, errors.New("kernel_hash_mismatch")
	}
	if len(saved.ID) != 32 || strings.Trim(saved.ID, "0123456789abcdef") != "" {
		return nil, false, errors.New("invalid_kernel_generation")
	}
	g := *saved
	g.cmd, g.done, g.retiring = nil, nil, false
	g.holds = 1
	dir := filepath.Join(m.dir, "generation-"+g.ID)
	if info, err := os.Lstat(dir); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return nil, false, errors.New("invalid_kernel_generation")
	}
	if os.MkdirAll(dir, 0700) != nil {
		return nil, false, errors.New("kernel_storage_failed")
	}
	raw, err := json.Marshal(g.Config)
	if err != nil || writeSourcePrivate(filepath.Join(dir, "config.json"), raw) != nil {
		m.removeGeneration(g.ID)
		return nil, false, errors.New("kernel_storage_failed")
	}
	if err = m.startLocked(&g); err != nil {
		if stopKernelGeneration(&g) {
			m.removeGeneration(g.ID)
		}
		return nil, false, err
	}
	m.mu.Lock()
	m.generations[g.ID] = &g
	m.mu.Unlock()
	return &g, true, nil
}

func (m *kernelManager) ReleaseRestore(g *kernelGeneration) {
	if g == nil {
		return
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if g.holds > 0 {
		g.holds--
	}
}

func kernelReferenced(s savedConfig, g *kernelGeneration) bool {
	check := func(nodes []nodeConfig) bool {
		for _, n := range nodes {
			if n.ProxyURL != "" && g.URLs[n.ID] == n.ProxyURL {
				return true
			}
		}
		return false
	}
	if check(s.Nodes) {
		return true
	}
	if s.Trial != nil && check(s.Trial.Previous.Nodes) {
		return true
	}
	for _, version := range s.History {
		if check(version.Snapshot.Nodes) {
			return true
		}
	}
	return false
}
func kernelControl(ctx context.Context, g *kernelGeneration, path string) (map[string]json.RawMessage, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+g.Control+path, nil)
	req.Header.Set("Authorization", "Bearer "+g.Secret)
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errors.New("kernel_control_failed")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(body) > 4<<20 {
		return nil, errors.New("kernel_control_failed")
	}
	var out map[string]json.RawMessage
	err = json.Unmarshal(body, &out)
	return out, err
}
func (m *kernelManager) Activate(g *kernelGeneration) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, old := range m.generations {
		if g == nil || old.ID != g.ID {
			old.retiring = true
		}
	}
	m.active = ""
	if g != nil {
		g.retiring = false
		m.active = g.ID
	}
	m.phase = "ready"
	m.lastError = ""
}
func (m *kernelManager) Discard(g *kernelGeneration) {
	if g == nil {
		return
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if stopKernelGeneration(g) {
		m.mu.Lock()
		delete(m.generations, g.ID)
		m.mu.Unlock()
		m.removeGeneration(g.ID)
	}
}
func (m *kernelManager) reap() {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	items := make(map[string]*kernelGeneration, len(m.generations))
	for id, g := range m.generations {
		items[id] = g
	}
	m.mu.Unlock()
	for id, g := range items {
		if g.holds > 0 {
			continue
		}
		select {
		case <-g.done:
			if g.retiring {
				m.mu.Lock()
				delete(m.generations, id)
				m.mu.Unlock()
				m.removeGeneration(id)
				continue
			}
			if time.Now().Before(g.retryAt) {
				continue
			}
			g.failures++
			g.retryAt = time.Now().Add(time.Second * time.Duration(1<<min(g.failures, 6)))
			err := m.startLocked(g)
			m.mu.Lock()
			if err != nil {
				m.phase = "failed"
				m.lastError = "kernel_process_failed"
			} else {
				m.phase = "ready"
				m.lastError = ""
			}
			m.mu.Unlock()
		default:
			if g.retiring {
				ctx, cancel := context.WithTimeout(m.ctx, time.Second)
				value, err := kernelControl(ctx, g, "/connections")
				cancel()
				var connections []json.RawMessage
				if err == nil && json.Unmarshal(value["connections"], &connections) == nil && len(connections) == 0 {
					if stopKernelGeneration(g) {
						m.mu.Lock()
						delete(m.generations, id)
						m.mu.Unlock()
						m.removeGeneration(id)
					}
				}
			}
		}
	}
}
func (m *kernelManager) Close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.cancel()
	m.opMu.Lock()
	defer m.opMu.Unlock()
	for _, g := range m.generations {
		if stopKernelGeneration(g) {
			m.removeGeneration(g.ID)
		}
	}
}

// Kill is followed by Wait before releasing ports, deleting config, or allowing
// a replacement process to start. A failed OS termination retains its files.
func stopKernelGeneration(g *kernelGeneration) bool {
	if g.cmd == nil || g.cmd.Process == nil || g.done == nil {
		return true
	}
	_ = g.cmd.Process.Kill()
	select {
	case <-g.done:
		return true
	case <-time.After(5 * time.Second):
		return false
	}
}

func (m *kernelManager) removeGeneration(id string) {
	if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
		return
	}
	root, err := filepath.Abs(m.dir)
	if err != nil {
		return
	}
	dir := filepath.Join(root, "generation-"+id)
	if filepath.Dir(dir) != root {
		return
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	_ = os.RemoveAll(dir)
}
func pinKernelProxy(ctx context.Context, original map[string]any) (map[string]any, error) {
	proxy := map[string]any{}
	for k, v := range original {
		proxy[k] = v
	}
	host, _ := proxy["server"].(string)
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 || len(addresses) > 64 {
		return nil, errors.New("kernel_node_dns_failed")
	}
	for _, ip := range addresses {
		if !publicSubscriptionIP(ip) {
			return nil, errors.New("unsafe_proxy_address")
		}
	}
	chosen := addresses[0]
	for _, ip := range addresses {
		if ip.Is4() {
			chosen = ip
			break
		}
	}
	proxy["server"] = chosen.String()
	protocol, _ := proxy["type"].(string)
	if _, err := netip.ParseAddr(host); err != nil {
		if protocol == "trojan" || protocol == "hysteria2" {
			if _, ok := proxy["sni"]; !ok {
				proxy["sni"] = host
			}
		} else if enabled, _ := proxy["tls"].(bool); enabled {
			if _, ok := proxy["servername"]; !ok {
				proxy["servername"] = host
			}
		}
	}
	return proxy, nil
}
