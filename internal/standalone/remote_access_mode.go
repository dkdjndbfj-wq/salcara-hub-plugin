package standalone

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"salcara/hub/internal/hub"
)

// Administrative mode/station settings are persisted separately from the
// deployment-owned issuer/audience policy file. No private key is persisted.
const remoteAccessModeFile = "remote-access-mode"

// BaseRevision fingerprints the complete normalized deployment policy,
// including the trusted PublicURL-derived scope. A changed deployment never
// restores an older administrative scope or authorization epoch.
type remoteAccessModeSetting struct {
	Mode         string             `json:"mode"`
	Revision     string             `json:"revision"`
	BaseRevision string             `json:"baseRevision"`
	Station      *hub.StationPolicy `json:"station,omitempty"`
}

func deploymentRemoteAccessRevision(p hub.RemoteAccessPolicy) string {
	raw, _ := json.Marshal(p)
	sum := sha256.Sum256(raw)
	return "deploy-" + hex.EncodeToString(sum[:])
}

func exactRemoteAccessObject(raw []byte, fields []string, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return errors.New("remote access setting must be an object")
	}
	allowed, seen := map[string]bool{}, map[string]bool{}
	for _, field := range fields {
		allowed[field] = true
	}
	for d.More() {
		key, err := d.Token()
		name, ok := key.(string)
		if err != nil || !ok || !allowed[name] || seen[name] {
			return errors.New("invalid remote access field")
		}
		seen[name] = true
		var value json.RawMessage
		if d.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errors.New("invalid remote access field")
		}
	}
	if end, err := d.Token(); err != nil || end != json.Delim('}') || d.Decode(new(any)) != io.EOF {
		return errors.New("invalid remote access object")
	}
	return json.Unmarshal(raw, out)
}

func (s *remoteAccessModeSetting) UnmarshalJSON(raw []byte) error {
	type plain remoteAccessModeSetting
	return exactRemoteAccessObject(raw, []string{"mode", "revision", "baseRevision", "station"}, (*plain)(s))
}

type remoteAccessChange struct {
	Mode             string             `json:"mode"`
	Confirm          bool               `json:"confirm"`
	Station          *hub.StationPolicy `json:"station,omitempty"`
	ExpectedRevision string             `json:"expectedRevision,omitempty"`
}

func (s *remoteAccessChange) UnmarshalJSON(raw []byte) error {
	type plain remoteAccessChange
	return exactRemoteAccessObject(raw, []string{"mode", "confirm", "station", "expectedRevision"}, (*plain)(s))
}

func remoteAccessModes() []map[string]string {
	return []map[string]string{
		{"id": "open", "label": "按来源名单开放", "description": "已配对电脑按分类名单验证实际 API 和反代来源；不额外要求来源发行方签名。"},
		{"id": "restricted", "label": "来源签名验证", "description": "在分类名单内额外验证来源签名；官方账号由电脑确认后取得本站短期授权。"},
		{"id": "disabled", "label": "关闭远程执行", "description": "保留配对、状态和历史读取；不接受新的远程任务、发送或审批。"},
	}
}

func loadRemoteAccessMode(dir, fallback string) (string, error) {
	setting, err := loadRemoteAccessModeSetting(dir, fallback, "1")
	return setting.Mode, err
}

func validRemoteAccessRevision(revision string) bool {
	if len(revision) < 1 || len(revision) > 128 {
		return false
	}
	for _, ch := range revision {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.') {
			return false
		}
	}
	return true
}

func loadRemoteAccessModeSetting(dir, fallback, baseRevision string) (remoteAccessModeSetting, error) {
	if fallback == "" {
		fallback = "open"
	}
	if baseRevision == "" {
		baseRevision = "1"
	}
	setting := remoteAccessModeSetting{Mode: fallback, Revision: baseRevision, BaseRevision: baseRevision}
	if !isRemoteAccessMode(fallback) {
		return setting, errors.New("remote access mode is unsupported")
	}
	path := filepath.Join(dir, remoteAccessModeFile)
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return setting, nil
	}
	if err != nil || !before.Mode().IsRegular() || before.Size() < 1 || before.Size() > hub.MaxSourcePolicyWireBytes {
		return setting, errors.New("saved remote access mode must be a small regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return setting, errors.New("saved remote access mode is unavailable")
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		return setting, errors.New("saved remote access mode changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(f, hub.MaxSourcePolicyWireBytes+1))
	if err != nil || len(raw) > hub.MaxSourcePolicyWireBytes {
		return setting, errors.New("saved remote access mode is invalid")
	}
	// Previous installations wrote a single mode line. Keep that migration
	// path; the next administrator change commits mode and epoch atomically.
	if mode := strings.TrimSpace(string(raw)); isRemoteAccessMode(mode) {
		setting.Mode = mode
		return setting, nil
	}
	var saved remoteAccessModeSetting
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&saved) != nil || decoder.Decode(new(any)) != io.EOF || !isRemoteAccessMode(saved.Mode) || !validRemoteAccessRevision(saved.Revision) || !validRemoteAccessRevision(saved.BaseRevision) {
		return setting, errors.New("saved remote access mode is unsupported")
	}
	if saved.Station != nil {
		raw, _ := json.Marshal(saved.Station)
		if saved.Station, err = hub.ParseStationPolicy(raw); err != nil {
			return setting, errors.New("saved remote access station is invalid")
		}
	}
	setting.Mode = saved.Mode
	if saved.BaseRevision == baseRevision {
		setting.Revision = saved.Revision
		setting.Station = saved.Station
	}
	return setting, nil
}

func isRemoteAccessMode(mode string) bool {
	return mode == "open" || mode == "restricted" || mode == "disabled"
}

// saveRemoteAccessMode uses a same-directory temporary file and atomic rename.
// Existing symlinks/directories are rejected; a failed fsync is reported as a
// durability warning only after the new file has become the commit point.
func saveRemoteAccessMode(dir, mode string) (warning bool, err error) {
	if !isRemoteAccessMode(mode) {
		return false, errors.New("unknown remote access mode")
	}
	return saveRemoteAccessModeData(dir, []byte(mode+"\n"))
}

func saveRemoteAccessModeSetting(dir string, setting remoteAccessModeSetting) (warning bool, err error) {
	if !isRemoteAccessMode(setting.Mode) || !validRemoteAccessRevision(setting.Revision) || !validRemoteAccessRevision(setting.BaseRevision) {
		return false, errors.New("invalid remote access mode epoch")
	}
	if setting.Station != nil {
		raw, _ := json.Marshal(setting.Station)
		if setting.Station, err = hub.ParseStationPolicy(raw); err != nil {
			return false, err
		}
	}
	data, err := json.Marshal(setting)
	if err != nil {
		return false, err
	}
	return saveRemoteAccessModeData(dir, append(data, '\n'))
}

func saveRemoteAccessModeData(dir string, data []byte) (warning bool, err error) {
	if len(data) > hub.MaxSourcePolicyWireBytes {
		return false, errors.New("remote access setting is too large to reload")
	}
	path := filepath.Join(dir, remoteAccessModeFile)
	if info, e := os.Lstat(path); e == nil && !info.Mode().IsRegular() {
		return false, errors.New("remote access mode file must not be a symlink or directory")
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return false, errors.New("cannot inspect remote access mode file")
	}
	f, e := os.CreateTemp(dir, ".remote-access-mode-*.tmp")
	if e != nil {
		return false, errors.New("cannot persist remote access mode; current mode unchanged")
	}
	temp := f.Name()
	defer os.Remove(temp)
	if e = f.Chmod(0o600); e == nil {
		_, e = f.Write(data)
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil || closeErr != nil {
		return false, errors.New("remote access mode write failed; current mode unchanged")
	}
	if e = os.Rename(temp, path); e != nil {
		return false, errors.New("remote access mode commit failed; current mode unchanged")
	}
	if runtime.GOOS != "windows" {
		directory, e := os.Open(dir)
		if e != nil {
			return true, nil
		}
		e = directory.Sync()
		_ = directory.Close()
		if e != nil {
			return true, nil
		}
	}
	return false, nil
}

func (s *Server) serveRemoteAccess(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		failure(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		failure(w, http.StatusBadRequest, "远程来源策略接口不接受 URL 参数")
		return
	}
	var in remoteAccessChange
	if !strictJSONLimit(w, r, &in, hub.MaxSourcePolicyWireBytes) {
		return
	}
	if !isRemoteAccessMode(in.Mode) || !in.Confirm {
		failure(w, http.StatusBadRequest, "请选择远程来源策略并确认切换")
		return
	}
	if s.cfg.RemoteAccessMode != "" {
		failure(w, http.StatusConflict, "远程来源策略由部署环境变量锁定，不能在管理页修改")
		return
	}
	s.resourceMu.Lock()
	defer s.resourceMu.Unlock()
	if s.stopped.Load() {
		failure(w, http.StatusServiceUnavailable, "Hub 正在重启")
		return
	}
	var warning bool
	p := s.hub.CurrentRemoteAccessPolicy()
	if in.Station != nil && in.ExpectedRevision == "" || in.ExpectedRevision != "" && in.ExpectedRevision != p.Revision {
		failure(w, http.StatusConflict, "远程来源设置已变化，请刷新后重新保存")
		return
	}
	p.Mode = in.Mode
	// Restore the deployment audience before deriving the official-only default,
	// so enabling then disabling official access has identical restart behavior.
	p.Audience = s.remoteAccessBaseAudience
	if in.Station != nil {
		raw, _ := json.Marshal(in.Station)
		station, err := hub.ParseStationPolicy(raw)
		if err != nil {
			failure(w, http.StatusBadRequest, "API 地址无效或重复，请保留完整路径且不要填写密钥、查询参数或片段")
			return
		}
		p.Station = station
	}
	err := s.hub.ApplyRemoteAccessPolicyCommit(p, func(applied hub.RemoteAccessPolicy) error {
		var err error
		warning, err = saveRemoteAccessModeSetting(s.cfg.DataDir, remoteAccessModeSetting{Mode: applied.Mode, Revision: applied.Revision, BaseRevision: s.remoteAccessBaseRevision, Station: applied.Station})
		return err
	})
	if err != nil {
		failure(w, http.StatusInternalServerError, "远程来源策略未切换，请检查数据卷是否可写且配置文件不是链接")
		return
	}
	message := "远程来源策略已保存并生效；不会停止已在电脑上运行的任务。"
	if warning {
		message = "远程来源策略已生效，但目录同步未完全确认；断电后请重新检查保存的模式。"
	}
	p = s.hub.CurrentRemoteAccessPolicy()
	respond(w, http.StatusOK, map[string]any{"remote_access_mode": p.Mode, "remote_access_modes": remoteAccessModes(), "remote_access_station": p.Station, "remote_access_revision": p.Revision, "durability_warning": warning, "message": message})
}
