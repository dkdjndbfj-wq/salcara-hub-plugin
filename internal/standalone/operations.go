package standalone

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const operationsFile = "operations-audit.json"
const maxOperations = 5000

type operationRecord struct {
	ID     uint64 `json:"id"`
	At     int64  `json:"at"`
	Actor  string `json:"actor"`
	Name   string `json:"name"`
	Role   string `json:"role"`
	Action string `json:"action"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	Target string `json:"target,omitempty"`
	Reason string `json:"reason,omitempty"`
}
type operationJournal struct {
	mu      sync.Mutex
	root    *os.Root
	Next    uint64            `json:"next"`
	Records []operationRecord `json:"records"`
	warning bool
}

func openOperations(dir string) (*operationJournal, error) {
	root, err := openAuthRoot(dir)
	if err != nil {
		return nil, err
	}
	j := &operationJournal{root: root, Records: []operationRecord{}}
	if _, err = root.Lstat(operationsFile); errors.Is(err, os.ErrNotExist) {
		return j, nil
	}
	raw, err := readPrivateFile(root, operationsFile, 4<<20)
	if err != nil {
		root.Close()
		return nil, err
	}
	if json.Unmarshal(raw, j) != nil || len(j.Records) > maxOperations {
		root.Close()
		return nil, errors.New("invalid operations audit file")
	}
	var prev uint64
	for _, r := range j.Records {
		if r.ID <= prev || r.ID > j.Next || len(r.Action) > 100 || len(r.Name) > 80 || len(r.Actor) > 32 || len(r.Reason) > 256 || r.Status < 0 || r.Status > 599 {
			root.Close()
			return nil, errors.New("invalid operations audit record")
		}
		prev = r.ID
	}
	return j, nil
}
func (j *operationJournal) close() {
	if j != nil {
		j.mu.Lock()
		defer j.mu.Unlock()
		j.root.Close()
	}
}
func (j *operationJournal) saveLocked() error {
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	warning, err := replacePrivateBounded(j.root, operationsFile, raw, 4<<20)
	j.warning = warning || err != nil
	return err
}

func auditAction(path string) string {
	p := strings.TrimPrefix(path, Prefix+"/_admin/v1/")
	if p == "users" {
		return "users"
	}
	// No request body, query, URL credentials, device IDs or arbitrary paths.
	for _, v := range []string{"action", "resource-mode", "remote-access", "device-cleanup", "storage", "enrollment-claims", "auth/logout", "auth/password", "operations/members", "update/check", "update/apply", "servers/discover", "servers/check", "servers/save", "servers/plan", "servers/deploy", "servers/activate", "servers/delete", "ingress/save", "ingress/verify", "bot/invites", "bot/invites/revoke", "network/nodes", "network/nodes/delete", "network/policy", "network/probe", "network/probe/cancel", "network/subscriptions/inspect", "network/subscriptions/import", "network/sources/save", "network/sources/refresh", "network/sources/apply", "network/sources/delete", "network/kernel/install", "network/releases/prepare", "network/releases/start", "network/releases/confirm", "network/releases/rollback", "network/observability/query", "network/geodata/install", "network/geodata/observe", "network/geodata/configure"} {
		if p == v {
			return v
		}
	}
	return "unknown"
}

func (j *operationJournal) begin(session adminSession, path string) (uint64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	before := append([]operationRecord{}, j.Records...)
	next := j.Next
	cutoff := time.Now().Add(-30 * 24 * time.Hour).UnixMilli()
	kept := make([]operationRecord, 0, len(j.Records)+1)
	for _, r := range j.Records {
		if r.At >= cutoff {
			kept = append(kept, r)
		}
	}
	if len(kept) >= maxOperations {
		kept = kept[len(kept)-maxOperations+1:]
	}
	id, name, role := sessionActor(session)
	j.Next++
	j.Records = append(kept, operationRecord{ID: j.Next, At: time.Now().UnixMilli(), Actor: id, Name: name, Role: role, Action: auditAction(path)})
	if err := j.saveLocked(); err != nil {
		j.Records = before
		j.Next = next
		return 0, err
	}
	if j.warning {
		return 0, errors.New("audit directory durability uncertain")
	}
	return j.Next, nil
}
func (j *operationJournal) finish(id uint64, status int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for i := range j.Records {
		if j.Records[i].ID == id {
			j.Records[i].Status = status
			break
		}
	}
	_ = j.saveLocked() // If this fails the on-disk pending record remains truthful.
}

type auditWriter struct {
	http.ResponseWriter
	status int
	id     uint64
}

func (w *auditWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *auditWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(b)
}
func (w *auditWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (s *Server) auditMutation(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, func(), bool) {
	if r.Method == "GET" || r.Method == "HEAD" {
		return w, func() {}, true
	}
	_, session, ok := s.auth.session(r)
	if !ok {
		failure(w, 401, "请重新登录")
		return w, func() {}, false
	}
	id, err := s.operations.begin(session, r.URL.Path)
	if err != nil {
		failure(w, 503, "操作记录无法持久保存，本次操作未执行，请检查数据卷")
		return w, func() {}, false
	}
	out := &auditWriter{ResponseWriter: w, id: id}
	return out, func() {
		status := out.status
		if status == 0 {
			status = 500
		}
		s.operations.finish(id, status)
	}, true
}

func (s *Server) annotateDeviceAction(w http.ResponseWriter, action, ref string) bool {
	out, ok := w.(*auditWriter)
	if !ok {
		return false
	}
	switch action {
	case "ping", "ban", "unban", "revoke", "disconnect":
	default:
		return true
	}
	decoded, err := hex.DecodeString(ref)
	if err != nil || len(decoded) != 32 {
		ref = ""
	}
	s.operations.mu.Lock()
	defer s.operations.mu.Unlock()
	for i := range s.operations.Records {
		if s.operations.Records[i].ID == out.id {
			s.operations.Records[i].Detail = action
			s.operations.Records[i].Target = ref
			break
		}
	}
	if err := s.operations.saveLocked(); err != nil || s.operations.warning {
		failure(w, 503, "设备操作尚未执行：审计记录无法持久保存")
		return false
	}
	return true
}

func (s *Server) serveOperations(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, Prefix+"/_admin/v1/operations/")
	if p == "members" {
		s.serveMembers(w, r)
		return
	}
	if r.Method != "GET" {
		w.Header().Set("Allow", "GET")
		failure(w, 405, "method not allowed")
		return
	}
	if p == "health" {
		if r.URL.RawQuery != "" || r.URL.ForceQuery {
			failure(w, 400, "状态不接受 URL 参数")
			return
		}
		respond(w, 200, s.operationalHealth())
		return
	}
	if p != "audit" && p != "audit/export" {
		failure(w, 404, "管理接口不存在")
		return
	}
	q, err := r.URL.Query(), error(nil)
	for k, v := range q {
		if len(v) != 1 || (k != "query" && k != "before" && k != "result") {
			err = errors.New("query")
		}
	}
	query := strings.TrimSpace(q.Get("query"))
	result := q.Get("result")
	var before uint64
	if q.Get("before") != "" {
		var parseErr error
		before, parseErr = strconv.ParseUint(q.Get("before"), 10, 64)
		if parseErr != nil {
			err = parseErr
		}
	}
	if err != nil || len(query) > 100 || (result != "" && result != "success" && result != "failed" && result != "pending") {
		failure(w, 400, "审计查询无效")
		return
	}
	s.operations.mu.Lock()
	records := append([]operationRecord{}, s.operations.Records...)
	warning := s.operations.warning
	s.operations.mu.Unlock()
	items := []operationRecord{}
	now := time.Now().Add(-30 * 24 * time.Hour).UnixMilli()
	for i := len(records) - 1; i >= 0; i-- {
		a := records[i]
		if a.At < now || before != 0 && a.ID >= before {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(a.Name+" "+a.Action+" "+a.Actor+" "+a.Detail+" "+a.Target), strings.ToLower(query)) {
			continue
		}
		if result == "success" && (a.Status < 200 || a.Status >= 400) || result == "failed" && a.Status < 400 || result == "pending" && a.Status != 0 {
			continue
		}
		items = append(items, a)
	}
	if p == "audit/export" {
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="salcara-audit.ndjson"`)
		for _, a := range items {
			if json.NewEncoder(w).Encode(a) != nil {
				return
			}
		}
		return
	}
	var next uint64
	if len(items) > 100 {
		items = items[:100]
		next = items[len(items)-1].ID
	}
	respond(w, 200, map[string]any{"records": items, "before": next, "retention_days": 30, "max_records": maxOperations, "durability_warning": warning})
}

type healthIssue struct {
	Code   string `json:"code"`
	Level  string `json:"level"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

func (s *Server) operationalHealth() map[string]any {
	storage := s.hub.DurableStorageStatus()
	issues := []healthIssue{}
	if storage.LowDisk {
		issues = append(issues, healthIssue{"disk_low", "critical", "磁盘预留空间不足", "新的消息和图片可能暂停接收，请扩容或清理其他磁盘文件。"})
	} else if storage.Warning {
		issues = append(issues, healthIssue{"storage_high", "warning", "消息存储接近容量上限", "检查存储配额与保留天数，清理仍受引用保护约束。"})
	}
	s.operations.mu.Lock()
	auditWarning := s.operations.warning
	s.operations.mu.Unlock()
	if auditWarning {
		issues = append(issues, healthIssue{"audit_write", "critical", "操作记录写入异常", "检查数据卷可写状态；未完成的记录表示结果尚未确认。"})
	}
	var backup backupReceipt
	if raw, err := readPrivateFile(s.operations.root, backupReceiptFile, 4096); err == nil {
		_ = json.Unmarshal(raw, &backup)
	}
	if backup.CreatedAt == 0 || time.Since(time.UnixMilli(backup.CreatedAt)) > 7*24*time.Hour {
		issues = append(issues, healthIssue{"backup_due", "warning", "需要创建备份", "最近 7 天没有经过校验的备份记录。请使用离线备份工具，并将备份另存到独立存储。"})
	}
	if s.cfg.FCMCredentialsFile == "" {
		issues = append(issues, healthIssue{"push_unconfigured", "info", "未配置服务端推送", "手机后台连接关闭时，通知不能仅靠 Hub 保证送达。"})
	}
	status := "healthy"
	for _, i := range issues {
		if i.Level == "critical" {
			status = "critical"
			break
		}
		if i.Level == "warning" {
			status = "attention"
		}
	}
	return map[string]any{"status": status, "checked_at": time.Now().UnixMilli(), "issues": issues, "storage": storage, "backup": backup, "audit_warning": auditWarning, "push_configured": s.cfg.FCMCredentialsFile != "", "running": s.hub.IsRunning()}
}

func (s *Server) serveMonitor(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		failure(w, 400, "invalid monitor request")
		return
	}
	if len(r.Header.Values("Authorization")) != 1 {
		failure(w, 401, "monitor credential required")
		return
	}
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") {
		failure(w, 401, "monitor credential required")
		return
	}
	s.auth.mu.Lock()
	m, ok := s.auth.memberForKeyLocked(strings.TrimPrefix(value, "Bearer "))
	closed := s.auth.closed
	s.auth.mu.Unlock()
	if !ok || closed || m.Role != "monitor" {
		failure(w, 401, "monitor credential required")
		return
	}
	health := s.operationalHealth()
	switch r.URL.Path {
	case Prefix + "/_monitor/v1/health":
		respond(w, 200, health)
	case Prefix + "/_monitor/v1/metrics":
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		storage := s.hub.DurableStorageStatus()
		critical := 0
		warning := 0
		for _, issue := range health["issues"].([]healthIssue) {
			if issue.Level == "critical" {
				critical++
			}
			if issue.Level == "warning" {
				warning++
			}
		}
		fmt.Fprintf(w, "# TYPE salcara_hub_running gauge\nsalcara_hub_running %d\n# TYPE salcara_hub_critical_issues gauge\nsalcara_hub_critical_issues %d\n# TYPE salcara_hub_warning_issues gauge\nsalcara_hub_warning_issues %d\n# TYPE salcara_hub_storage_bytes gauge\nsalcara_hub_storage_bytes %d\n# TYPE salcara_hub_disk_free_bytes gauge\nsalcara_hub_disk_free_bytes %d\n# TYPE salcara_hub_backup_timestamp_seconds gauge\nsalcara_hub_backup_timestamp_seconds %d\n", boolNumber(s.hub.IsRunning()), critical, warning, storage.DiskBytes, storage.FreeDiskBytes, health["backup"].(backupReceipt).CreatedAt/1000)
	default:
		failure(w, 404, "monitor endpoint not found")
	}
}
func boolNumber(v bool) int {
	if v {
		return 1
	}
	return 0
}
