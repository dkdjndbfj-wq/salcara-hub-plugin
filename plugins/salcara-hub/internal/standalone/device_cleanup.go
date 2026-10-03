package standalone

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"

	"salcara/hubplugin/internal/hub"
)

const deviceCleanupFile = "device-cleanup.json"

type cleanupSetting struct {
	UnpairedDays int `json:"unpaired_days"`
	PairedDays   int `json:"paired_days"`
}

func validSetting(c cleanupSetting) bool {
	return c.UnpairedDays >= 0 && c.UnpairedDays <= hub.MaxDeviceCleanupDays && c.PairedDays >= 0 && c.PairedDays <= hub.MaxDeviceCleanupDays
}

// loadDeviceCleanup returns the admin-saved policy, or the environment defaults
// when nothing has been saved yet.
func loadDeviceCleanup(dir string, fallback cleanupSetting) (cleanupSetting, error) {
	path := filepath.Join(dir, deviceCleanupFile)
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fallback, nil
	}
	if err != nil || !before.Mode().IsRegular() || before.Size() < 2 || before.Size() > 256 {
		return cleanupSetting{}, errors.New("saved device cleanup setting must be a small regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return cleanupSetting{}, errors.New("saved device cleanup setting is unavailable")
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		return cleanupSetting{}, errors.New("saved device cleanup setting changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 257))
	if err != nil || len(raw) > 256 {
		return cleanupSetting{}, errors.New("saved device cleanup setting is invalid")
	}
	var c cleanupSetting
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || !validSetting(c) {
		return cleanupSetting{}, errors.New("saved device cleanup setting is invalid")
	}
	return c, nil
}

// saveDeviceCleanup commits through an atomic rename, like the resource mode.
func saveDeviceCleanup(dir string, c cleanupSetting) (warning bool, err error) {
	if !validSetting(c) {
		return false, errors.New("invalid device cleanup setting")
	}
	path := filepath.Join(dir, deviceCleanupFile)
	if info, e := os.Lstat(path); e == nil && !info.Mode().IsRegular() {
		return false, errors.New("device cleanup file must not be a symlink or directory")
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return false, errors.New("cannot inspect device cleanup file")
	}
	raw, _ := json.Marshal(c)
	f, e := os.CreateTemp(dir, ".device-cleanup-*.tmp")
	if e != nil {
		return false, errors.New("cannot persist device cleanup setting")
	}
	temp := f.Name()
	defer os.Remove(temp)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(append(raw, '\n'))
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil || closeErr != nil {
		return false, errors.New("device cleanup write failed")
	}
	if e = os.Rename(temp, path); e != nil {
		return false, errors.New("device cleanup commit failed")
	}
	if runtime.GOOS != "windows" {
		if directory, e := os.Open(dir); e == nil {
			e = directory.Sync()
			directory.Close()
			if e != nil {
				return true, nil
			}
		} else {
			return true, nil
		}
	}
	return false, nil
}

func (s *Server) serveDeviceCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		failure(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		failure(w, http.StatusBadRequest, "清理设置接口不接受 URL 参数")
		return
	}
	var in struct {
		UnpairedDays *int `json:"unpaired_days"`
		PairedDays   *int `json:"paired_days"`
		RunNow       bool `json:"run_now"`
		Confirm      bool `json:"confirm"`
	}
	if !strictJSON(w, r, &in) {
		return
	}
	if !in.Confirm {
		failure(w, http.StatusBadRequest, "请明确确认清理设置")
		return
	}
	s.resourceMu.Lock()
	defer s.resourceMu.Unlock()
	if s.stopped.Load() {
		failure(w, http.StatusServiceUnavailable, "Hub 正在重启")
		return
	}
	if in.RunNow && in.UnpairedDays == nil && in.PairedDays == nil {
		removed := s.hub.RunDeviceCleanup()
		respond(w, http.StatusOK, map[string]any{"device_cleanup": s.hub.CurrentDeviceCleanup(), "removed": removed})
		return
	}
	if in.UnpairedDays == nil || in.PairedDays == nil || in.RunNow {
		failure(w, http.StatusBadRequest, "请同时提供未配对和已配对电脑的清理天数")
		return
	}
	next := cleanupSetting{UnpairedDays: *in.UnpairedDays, PairedDays: *in.PairedDays}
	if !validSetting(next) {
		failure(w, http.StatusBadRequest, "清理天数需在 0（不清理）到 3650 之间")
		return
	}
	var warning bool
	removed, err := s.hub.ApplyDeviceCleanup(next.UnpairedDays, next.PairedDays, func() error { var err error; warning, err = saveDeviceCleanup(s.cfg.DataDir, next); return err })
	if err != nil {
		failure(w, http.StatusInternalServerError, "清理设置未保存，请检查数据卷是否可写且配置文件不是链接")
		return
	}
	respond(w, http.StatusOK, map[string]any{"device_cleanup": s.hub.CurrentDeviceCleanup(), "removed": removed, "durability_warning": warning})
}
