package standalone

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"

	"salcara/hub/internal/hub"
)

const resourceScope = "event caches and phone streams per account; pending commands and explicit stream admission also service-wide; existing tasks are not cancelled; cache gaps may reload recent history from an online authorized desktop, not a full cloud backup"
const resourceModeFile = "resource-mode"

func loadResourceMode(dir, fallback string) (string, error) {
	if fallback == "" {
		fallback = "economy"
	}
	path := filepath.Join(dir, resourceModeFile)
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fallback, nil
	}
	if err != nil || !before.Mode().IsRegular() || before.Size() < 1 || before.Size() > 64 {
		return "", errors.New("saved resource mode must be a small regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("saved resource mode is unavailable")
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		return "", errors.New("saved resource mode changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 65))
	if err != nil || len(raw) > 64 {
		return "", errors.New("saved resource mode is invalid")
	}
	mode := strings.TrimSpace(string(raw))
	if _, ok := hub.ResourceMode(mode); !ok {
		return "", errors.New("saved resource mode is unsupported")
	}
	return mode, nil
}

// Atomic rename is the commit point. Failures before it keep both the live
// policy and prior file unchanged. A directory fsync failure after commit is
// reported as a durability warning, not falsely as an unchanged old mode.
func saveResourceMode(dir, mode string) (warning bool, err error) {
	if _, ok := hub.ResourceMode(mode); !ok {
		return false, errors.New("unknown resource mode")
	}
	path := filepath.Join(dir, resourceModeFile)
	if info, e := os.Lstat(path); e == nil && !info.Mode().IsRegular() {
		return false, errors.New("resource mode file must not be a symlink or directory")
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return false, errors.New("cannot inspect resource mode file")
	}
	f, e := os.CreateTemp(dir, ".resource-mode-*.tmp")
	if e != nil {
		return false, errors.New("cannot persist resource mode; current mode unchanged")
	}
	temp := f.Name()
	defer os.Remove(temp)
	if e = f.Chmod(0600); e == nil {
		_, e = io.WriteString(f, mode+"\n")
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil || closeErr != nil {
		return false, errors.New("resource mode write failed; current mode unchanged")
	}
	if e = os.Rename(temp, path); e != nil {
		return false, errors.New("resource mode commit failed; current mode unchanged")
	}
	if runtime.GOOS != "windows" {
		directory, e := os.Open(dir)
		if e != nil {
			return true, nil
		}
		e = directory.Sync()
		directory.Close()
		if e != nil {
			return true, nil
		}
	}
	return false, nil
}

func (s *Server) serveResourceMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		failure(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		failure(w, http.StatusBadRequest, "资源模式接口不接受 URL 参数")
		return
	}
	var in struct {
		Mode    string `json:"mode"`
		Confirm bool   `json:"confirm"`
	}
	if !strictJSON(w, r, &in) {
		return
	}
	preset, ok := hub.ResourceMode(in.Mode)
	if !ok || !in.Confirm {
		failure(w, http.StatusBadRequest, "请选择三种资源模式之一，并确认切换")
		return
	}
	s.resourceMu.Lock()
	defer s.resourceMu.Unlock()
	if s.stopped.Load() {
		failure(w, http.StatusServiceUnavailable, "Hub 正在重启")
		return
	}
	var warning bool
	err := s.hub.ApplyResourceMode(in.Mode, func() error { var err error; warning, err = saveResourceMode(s.cfg.DataDir, in.Mode); return err })
	if err != nil {
		failure(w, http.StatusInternalServerError, "资源模式未切换，请检查数据卷是否可写且配置文件不是链接")
		return
	}
	debug.SetMemoryLimit(preset.MemoryLimitMiB << 20)
	message := "资源模式已切换；不会取消正在执行的电脑任务。手机同步时会尝试从在线且已授权的电脑重读最近内容，不保证全部历史补齐。"
	if warning {
		message = "资源模式已切换，但目录同步失败；断电后请检查保存的模式。"
	}
	respond(w, http.StatusOK, map[string]any{"resource_mode": in.Mode, "resource_modes": hub.ResourceModes(), "resource_scope": resourceScope, "memory_limit_kind": "Go soft target; not RSS or hard guarantee", "durability_warning": warning, "message": message})
}
