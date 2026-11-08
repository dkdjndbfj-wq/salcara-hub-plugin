// Package hostfleet manages administrator-owned Linux ingress hosts over SSH.
// Public snapshots never contain SSH credentials or the Edge control token.
package hostfleet

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

type Credentials struct {
	AuthType   string `json:"auth_type"`
	Password   string `json:"password,omitempty"`
	PrivateKey string `json:"private_key,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
}
type Host struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	Username      string `json:"username"`
	AuthType      string `json:"auth_type"`
	Fingerprint   string `json:"fingerprint"`
	Platform      string `json:"platform"`
	Arch          string `json:"arch"`
	CanDeploy     bool   `json:"can_deploy"`
	Deployed      bool   `json:"deployed"`
	Origin        string `json:"origin"`
	LastCheckedAt int64  `json:"last_checked_at"`
}
type record struct {
	Host
	Secret Credentials `json:"secret"`
	Token  string      `json:"token"`
}
type Step struct {
	At      int64  `json:"at"`
	Message string `json:"message"`
}
type Job struct {
	ID        string `json:"id"`
	HostID    string `json:"host_id"`
	Status    string `json:"status"`
	Steps     []Step `json:"steps"`
	Error     string `json:"error,omitempty"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}
type diskState struct {
	Revision int64    `json:"revision"`
	ActiveID string   `json:"active_id"`
	Hosts    []record `json:"hosts"`
	Jobs     []Job    `json:"jobs"`
}
type State struct {
	Revision  int64           `json:"revision"`
	ActiveID  string          `json:"active_id"`
	Hosts     []Host          `json:"hosts"`
	Jobs      []Job           `json:"jobs"`
	Artifacts map[string]bool `json:"artifacts"`
}
type checked struct {
	host    record
	expires time.Time
}
type Manager struct {
	mu               sync.Mutex
	dir, artifactDir string
	key              []byte
	saved            diskState
	checks           map[string]checked
	plans            map[string]Plan
	ctx              context.Context
	cancel           context.CancelFunc
	closed           bool
	wg               sync.WaitGroup
	operations       chan struct{}
	// Tests inject a loopback SSH dialer; production always resolves public addresses.
	dial dialFunc
}

func New(dir, artifactDir string) (*Manager, error) {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{dir: dir, artifactDir: artifactDir, checks: map[string]checked{}, plans: map[string]Plan{}, ctx: ctx, cancel: cancel, operations: make(chan struct{}, 2), dial: publicDial, saved: diskState{Revision: 1, Hosts: []record{}, Jobs: []Job{}}}
	key, keyErr := readPrivate(filepath.Join(dir, "servers.key"), 32)
	if keyErr == nil {
		if len(key) != 32 {
			cancel()
			return nil, errors.New("invalid server registry key")
		}
		m.key = key
	} else if !errors.Is(keyErr, os.ErrNotExist) {
		cancel()
		return nil, errors.New("cannot read server registry key")
	}
	data, err := readPrivate(filepath.Join(dir, "servers.enc"), 2<<20)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		cancel()
		return nil, errors.New("cannot read encrypted server registry")
	}
	if len(m.key) != 32 {
		cancel()
		return nil, errors.New("server registry encryption key unavailable")
	}
	aead, _ := newAEAD(m.key)
	if len(data) < aead.NonceSize() {
		cancel()
		return nil, errors.New("invalid server registry")
	}
	clear, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], []byte("salcara-hostfleet-v1"))
	if err != nil || json.Unmarshal(clear, &m.saved) != nil || m.saved.Revision < 1 || len(m.saved.Hosts) > 16 || len(m.saved.Jobs) > 32 {
		cancel()
		return nil, errors.New("invalid encrypted server registry")
	}
	changed := false
	for i := range m.saved.Jobs {
		if m.saved.Jobs[i].Status == "running" {
			m.saved.Jobs[i].Status = "interrupted"
			m.saved.Jobs[i].Error = "Hub 已重启；请先检查远端服务，再生成新部署方案。"
			changed = true
		}
	}
	if changed {
		if err = m.persistLocked(m.saved); err != nil {
			cancel()
			return nil, err
		}
	}
	return m, nil
}
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("OS random source unavailable")
	}
	return hex.EncodeToString(b[:])
}
func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func readPrivate(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("private regular file required")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("file changed")
	}
	return io.ReadAll(io.LimitReader(f, limit+1))
}
func (m *Manager) persistLocked(next diskState) error {
	if m.key == nil {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return errors.New("cannot create registry key")
		}
		f, err := os.OpenFile(filepath.Join(m.dir, "servers.key"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return errors.New("cannot create private registry key")
		}
		_, err = f.Write(key)
		if err == nil {
			err = f.Sync()
		}
		ce := f.Close()
		if err != nil || ce != nil {
			return errors.New("cannot save registry key")
		}
		m.key = key
	}
	clear, err := json.Marshal(next)
	if err != nil {
		return err
	}
	aead, _ := newAEAD(m.key)
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	data := aead.Seal(nonce, nonce, clear, []byte("salcara-hostfleet-v1"))
	path := filepath.Join(m.dir, "servers.enc")
	if info, e := os.Lstat(path); e == nil && (!info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return errors.New("invalid registry file")
	}
	f, err := os.CreateTemp(m.dir, ".servers-*")
	if err != nil {
		return errors.New("cannot save server registry")
	}
	defer os.Remove(f.Name())
	err = f.Chmod(0600)
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err != nil || ce != nil {
		return errors.New("cannot sync server registry")
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return errors.New("cannot commit server registry")
	}
	m.saved = next
	// Rename has committed; a directory sync failure must not imply rollback.
	if runtime.GOOS != "windows" {
		if d, e := os.Open(m.dir); e == nil {
			_ = d.Sync()
			d.Close()
		}
	}
	return nil
}
func (m *Manager) stateLocked() State {
	s := State{Revision: m.saved.Revision, ActiveID: m.saved.ActiveID, Hosts: []Host{}, Jobs: append([]Job{}, m.saved.Jobs...), Artifacts: map[string]bool{}}
	for _, h := range m.saved.Hosts {
		s.Hosts = append(s.Hosts, h.Host)
	}
	for i := range s.Jobs {
		s.Jobs[i].Steps = append([]Step{}, s.Jobs[i].Steps...)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		_, err := m.artifactInfo(arch)
		s.Artifacts[arch] = err == nil
	}
	return s
}
func (m *Manager) State() State { m.mu.Lock(); defer m.mu.Unlock(); return m.stateLocked() }
func (m *Manager) Active() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.closed && m.saved.ActiveID != ""
}
func (m *Manager) findLocked(id string) (record, bool) {
	for _, h := range m.saved.Hosts {
		if h.ID == id {
			return h, true
		}
	}
	return record{}, false
}
func (m *Manager) busyLocked(id string) bool {
	for _, j := range m.saved.Jobs {
		if j.HostID == id && j.Status == "running" {
			return true
		}
	}
	return false
}
func (m *Manager) expireLocked() {
	now := time.Now()
	for id, c := range m.checks {
		if now.After(c.expires) {
			delete(m.checks, id)
		}
	}
	for id, p := range m.plans {
		if now.UnixMilli() >= p.ExpiresAt {
			delete(m.plans, id)
		}
	}
}
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.cancel()
	m.checks = map[string]checked{}
	m.plans = map[string]Plan{}
	m.mu.Unlock()
	m.wg.Wait()
}
func (m *Manager) begin(ctx context.Context) (context.Context, func(), error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, nil, errors.New("服务器管理已停止")
	}
	select {
	case m.operations <- struct{}{}:
	default:
		m.mu.Unlock()
		return nil, nil, errors.New("已有服务器操作进行中，请稍后重试")
	}
	m.wg.Add(1)
	m.mu.Unlock()
	work, cancel := context.WithTimeout(ctx, 15*time.Second)
	stop := context.AfterFunc(m.ctx, cancel)
	return work, func() { stop(); cancel(); <-m.operations; m.wg.Done() }, nil
}
