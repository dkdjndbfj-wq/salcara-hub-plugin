package launcher

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

type Status struct {
	Configured     bool   `json:"configured"`
	CurrentVersion string `json:"current_version"`
	LatestVersion  string `json:"latest_version,omitempty"`
	Status         string `json:"status"`
	SHA256         string `json:"sha256,omitempty"`
	ReleaseNotes   string `json:"release_notes,omitempty"`
	Message        string `json:"message"`
	JobID          string `json:"job_id,omitempty"`
}
type processRunner interface {
	Start(context.Context, release) error
	Stop() error
}
type manager struct {
	cfg       Config
	store     *store
	client    *http.Client
	key       ed25519.PublicKey
	runner    processRunner
	ctx       context.Context
	mu        sync.Mutex
	operation sync.Mutex
	job       sync.WaitGroup
	state     diskState
	status    Status
	ready     bool
	closed    bool
}

func newManager(ctx context.Context, cfg Config, s *store, state diskState, runner processRunner) *manager {
	r, _ := s.resolve(state.Current)
	m := &manager{ctx: ctx, cfg: cfg, store: s, state: state, runner: runner, client: updateClient(), key: fixedPublicKey(), status: Status{Configured: !cfg.Disabled, CurrentVersion: r.Version, Status: "current", Message: "正在使用已确认版本；仅点击检查时联网"}}
	if cfg.Disabled {
		m.status.Status = "unavailable"
		m.status.Message = "运营者已关闭应用更新"
	}
	if state.Job != nil {
		m.status.JobID = state.Job.ID
		if state.Job.Status == "updating" {
			m.status.Status = "failed"
			m.status.Message = "上次更新被中断，已恢复最后确认的签名版本"
		} else if state.Job.Status == "failed" {
			m.status.Status = "failed"
			m.status.Message = state.Job.Message
		}
	}
	return m
}
func (m *manager) snapshot() Status   { m.mu.Lock(); defer m.mu.Unlock(); return m.status }
func (m *manager) setStatus(s Status) { m.mu.Lock(); m.status = s; m.mu.Unlock() }
func (m *manager) fetchFeed(ctx context.Context) (Feed, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	res, err := get(ctx, m.client, m.cfg.FeedURL, maxFeedBytes)
	if err != nil {
		return Feed{}, nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxFeedBytes+1))
	if err != nil || len(raw) > maxFeedBytes {
		return Feed{}, nil, errors.New("更新清单超过大小限制或下载中断")
	}
	f, err := verifyFeed(raw, m.key)
	return f, raw, err
}
func (m *manager) check() Status {
	if !m.operation.TryLock() {
		return m.snapshot()
	}
	defer m.operation.Unlock()
	s := m.snapshot()
	if m.cfg.Disabled {
		s.Status = "unavailable"
		s.Message = "运营者已关闭应用更新"
		m.setStatus(s)
		return s
	}
	f, _, err := m.fetchFeed(m.ctx)
	if err != nil {
		s.Status = "unavailable"
		s.LatestVersion = ""
		s.SHA256 = ""
		s.ReleaseNotes = ""
		s.Message = err.Error()
		m.setStatus(s)
		return s
	}
	b, ok := f.Binaries[m.store.platform]
	if !ok {
		s.Status = "unavailable"
		s.Message = "该平台不支持签名应用更新"
		m.setStatus(s)
		return s
	}
	s.LatestVersion, s.SHA256, s.ReleaseNotes = f.Version, b.SHA256, f.ReleaseNotes
	if newer(f.Version, s.CurrentVersion) {
		s.Status = "available"
		s.Message = "发现签名新版；确认后仅重启 Hub 应用，不更换 Docker 镜像"
	} else {
		s.Status = "current"
		s.Message = "当前已确认版本不低于更新源；不会降级"
	}
	m.setStatus(s)
	return s
}

type applyRequest struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	Confirm bool   `json:"confirm"`
}

func (m *manager) apply(in applyRequest) (Status, int, func(bool)) {
	if !m.operation.TryLock() {
		return m.snapshot(), http.StatusConflict, nil
	}
	s := m.snapshot()
	m.mu.Lock()
	ready := m.ready && !m.closed
	m.mu.Unlock()
	if m.cfg.Disabled || !ready || m.ctx.Err() != nil {
		s.Message = "更新不可用或服务正在停止"
		m.operation.Unlock()
		return s, http.StatusServiceUnavailable, nil
	}
	if !in.Confirm || !hashPattern.MatchString(in.SHA256) || s.Status != "available" || in.Version != s.LatestVersion || in.SHA256 != s.SHA256 || !newer(in.Version, s.CurrentVersion) {
		s.Message = "请重新检查版本并确认当前显示的更新"
		m.operation.Unlock()
		return s, http.StatusConflict, nil
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		s.Message = "无法创建更新任务"
		m.operation.Unlock()
		return s, http.StatusInternalServerError, nil
	}
	// Seal all Add calls before shutdown can Wait. The same mutex protects
	// closed, so an accepted job can never be added after shutdown's barrier.
	m.mu.Lock()
	if m.closed || m.ctx.Err() != nil {
		m.mu.Unlock()
		m.operation.Unlock()
		s.Message = "服务正在停止"
		return s, http.StatusServiceUnavailable, nil
	}
	m.job.Add(1)
	m.mu.Unlock()
	s.Status, s.JobID, s.Message = "updating", hex.EncodeToString(random[:]), "更新已受理；验证通过前不会停止现有服务"
	m.state.Job = &jobRecord{ID: s.JobID, Status: s.Status, Message: s.Message}
	if err := m.store.save(m.state); err != nil {
		s.Status = "failed"
		s.Message = "无法保存更新任务；现有服务未停止"
		m.setStatus(s)
		m.job.Done()
		m.operation.Unlock()
		return s, http.StatusInternalServerError, nil
	}
	m.setStatus(s)
	var once sync.Once
	start := func(accepted bool) {
		once.Do(func() {
			go func() {
				defer m.job.Done()
				defer m.operation.Unlock()
				if !accepted {
					m.finish("failed", "受理回执未能发送；现有服务未停止")
					return
				}
				m.runUpdate(in)
			}()
		})
	}
	// The private HTTP handler flushes 202 before releasing this barrier.
	return s, http.StatusAccepted, start
}
func (m *manager) download(ctx context.Context, b Binary) error {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	res, err := get(ctx, m.client, b.URL, maxBinaryBytes)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.ContentLength >= 0 && res.ContentLength != b.Size {
		return errors.New("更新文件声明大小不一致")
	}
	f, err := os.CreateTemp(m.store.dir, ".download-*.tmp")
	if err != nil {
		return errors.New("无法写入独立更新目录")
	}
	name := f.Name()
	defer os.Remove(name)
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(res.Body, b.Size+1))
	if copyErr == nil {
		copyErr = f.Sync()
	}
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil || n != b.Size || hex.EncodeToString(h.Sum(nil)) != b.SHA256 {
		return errors.New("更新文件下载中断或哈希/大小不符；现有服务未停止")
	}
	return m.store.keepVerifiedBinary(name, b)
}
func (m *manager) finish(status, message string) {
	s := m.snapshot()
	s.Status, s.Message = status, message
	m.state.Job = &jobRecord{ID: s.JobID, Status: status, Message: message}
	if err := m.store.save(m.state); err != nil {
		s.Status = "failed"
		s.Message = "应用状态已变更，但更新记录保存失败；请检查持久卷"
	}
	m.setStatus(s)
}
func (m *manager) currentVersion(version string) {
	s := m.snapshot()
	s.CurrentVersion = version
	m.setStatus(s)
}
func (m *manager) runUpdate(in applyRequest) {
	f, raw, err := m.fetchFeed(m.ctx)
	if err != nil {
		m.finish("failed", err.Error())
		return
	}
	b, ok := f.Binaries[m.store.platform]
	if !ok || f.Version != in.Version || b.SHA256 != in.SHA256 || !newer(f.Version, m.snapshot().CurrentVersion) {
		m.finish("failed", "更新源已变更或目标不兼容；现有服务未停止")
		return
	}
	if err = m.download(m.ctx, b); err != nil {
		m.finish("failed", err.Error())
		return
	}
	old, err := m.store.resolve(m.state.Current)
	if err != nil {
		m.finish("failed", "原版本校验失败；拒绝停止或执行任意磁盘指针")
		return
	}
	ref := reference{Envelope: raw}
	next, err := m.store.resolve(ref)
	if err != nil {
		m.finish("failed", "新版本执行前校验失败；现有服务未停止")
		return
	}
	if m.ctx.Err() != nil {
		m.finish("failed", "服务正在停止；未切换应用版本")
		return
	}
	if err = m.runner.Stop(); err != nil {
		m.finish("failed", "旧应用未能安全停止；不会启动第二个写入进程")
		return
	}
	if m.ctx.Err() != nil {
		m.finish("failed", "服务正在停止；下次启动将恢复原确认版本")
		return
	}
	if err = m.runner.Start(m.ctx, next); err != nil {
		if m.ctx.Err() != nil {
			m.finish("failed", "启动中断；下次启动将恢复原确认版本")
			return
		}
		if fallback := m.runner.Start(m.ctx, old); fallback != nil {
			m.finish("failed", "新应用健康验证失败，原版本恢复也失败；请查看容器健康与日志")
		} else {
			m.currentVersion(old.Version)
			m.finish("failed", "新应用健康或版本验证失败，已回退原版本；未回滚用户数据")
		}
		return
	}
	m.currentVersion(next.Version)
	previous := m.state.Current
	candidate := m.state
	candidate.Current, candidate.Previous = ref, &previous
	candidate.Job = &jobRecord{ID: m.snapshot().JobID, Status: "current", Message: "已验证并更新 Hub 应用；Docker 镜像及启动器未改变"}
	if err = m.store.save(candidate); err != nil {
		if stopErr := m.runner.Stop(); stopErr != nil {
			m.finish("failed", "新应用记录保存失败且未能停止；需人工检查，不启动第二个写入者")
			return
		}
		if fallback := m.runner.Start(m.ctx, old); fallback != nil {
			m.finish("failed", "提交更新状态失败，原应用恢复失败；需人工检查")
		} else {
			m.currentVersion(old.Version)
			m.finish("failed", "提交更新状态失败，已回退原应用；未回滚用户数据")
		}
		return
	}
	m.state = candidate
	m.finish("current", "Hub 应用更新完成并通过版本/健康验证；绑定与管理令牌保留")
}
