package standalone

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const membersFile = "admin-members.json"

type adminMember struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	Hash      string `json:"hash,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

func loadMembers(root *os.Root) ([]adminMember, error) {
	if _, err := root.Lstat(membersFile); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	raw, err := readPrivateFile(root, membersFile, 32768)
	if err != nil {
		return nil, err
	}
	var members []adminMember
	if json.Unmarshal(raw, &members) != nil || len(members) > 32 {
		return nil, errors.New("invalid administrator members")
	}
	seen := map[string]bool{}
	for _, m := range members {
		hash, err := hex.DecodeString(m.Hash)
		if !validMemberName(m.Name) || (m.Role != "operator" && m.Role != "viewer" && m.Role != "monitor") || len(m.ID) != 24 || seen[m.ID] || err != nil || len(hash) != 32 || !strings.HasPrefix(m.Hash, m.ID) || m.CreatedAt <= 0 {
			return nil, errors.New("invalid administrator member record")
		}
		seen[m.ID] = true
	}
	return members, nil
}

func validMemberName(name string) bool {
	if strings.TrimSpace(name) != name || len(name) < 1 || len(name) > 80 || !utf8.ValidString(name) {
		return false
	}
	for _, c := range name {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}

// Generated member keys have 256 bits of entropy; only their SHA-256 is saved.
// The owner key continues to use its existing PBKDF2 credential record.
func (a *adminAuth) memberForKeyLocked(key string) (adminMember, bool) {
	if !strings.HasPrefix(key, "scm_") || len(key) != 47 {
		return adminMember{}, false
	}
	sum := sha256.Sum256([]byte(key))
	encoded := hex.EncodeToString(sum[:])
	for _, m := range a.members {
		if subtle.ConstantTimeCompare([]byte(encoded), []byte(m.Hash)) == 1 {
			return m, true
		}
	}
	return adminMember{}, false
}

func sessionActor(session adminSession) (string, string, string) {
	if session.Actor == "" {
		return "owner", "主管理员", "owner"
	}
	return session.Actor, session.Name, session.Role
}

func adminPermission(session adminSession, r *http.Request) bool {
	_, _, role := sessionActor(session)
	if role == "owner" {
		return true
	}
	p := strings.TrimPrefix(r.URL.Path, Prefix+"/_admin/v1/")
	if (p == "auth/session" && r.Method == "GET") || (p == "auth/logout" && r.Method == "POST") {
		return true
	}
	// Explicit allowlist: newly added APIs stay owner-only until reviewed.
	if r.Method == "GET" {
		switch p {
		case "state", "users", "storage", "operations/health", "operations/audit", "operations/audit/export":
			return role == "viewer" || role == "operator"
		}
	}
	return role == "operator" && r.Method == "POST" && p == "action"
}

func sessionResponse(session adminSession) map[string]string {
	id, name, role := sessionActor(session)
	username := session.Username
	if username == "" {
		username = "admin"
	}
	return map[string]string{"csrf_token": session.CSRF, "actor": id, "name": name, "role": role, "username": username}
}

func (s *Server) serveMembers(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		failure(w, 400, "管理员接口不接受 URL 参数")
		return
	}
	if r.Method == "GET" {
		s.auth.mu.Lock()
		members := append([]adminMember{}, s.auth.members...)
		s.auth.mu.Unlock()
		for i := range members {
			members[i].Hash = ""
		}
		respond(w, 200, map[string]any{"members": members, "limit": 32})
		return
	}
	if r.Method != "POST" {
		w.Header().Set("Allow", "GET, POST")
		failure(w, 405, "method not allowed")
		return
	}
	var in struct {
		Action  string `json:"action"`
		Name    string `json:"name"`
		Role    string `json:"role"`
		ID      string `json:"id"`
		Confirm bool   `json:"confirm"`
	}
	if !strictJSON(w, r, &in) {
		return
	}
	if !in.Confirm {
		failure(w, 400, "请确认管理员变更")
		return
	}
	s.auth.mu.Lock()
	defer s.auth.mu.Unlock()
	if s.auth.closed {
		failure(w, 503, "服务正在关闭")
		return
	}
	next := append([]adminMember{}, s.auth.members...)
	var secret string
	switch in.Action {
	case "create":
		if !validMemberName(in.Name) || (in.Role != "operator" && in.Role != "viewer" && in.Role != "monitor") || len(next) >= 32 {
			failure(w, 400, "名称、角色无效或管理员已达 32 个上限")
			return
		}
		for _, m := range next {
			if strings.EqualFold(m.Name, in.Name) {
				failure(w, 409, "管理员名称已存在")
				return
			}
		}
		key, err := randomSecret()
		if err != nil {
			failure(w, 503, "无法生成管理密钥")
			return
		}
		secret = "scm_" + key
		sum := sha256.Sum256([]byte(secret))
		id := hex.EncodeToString(sum[:12])
		next = append(next, adminMember{ID: id, Name: in.Name, Role: in.Role, Hash: hex.EncodeToString(sum[:]), CreatedAt: time.Now().UnixMilli()})
	case "revoke":
		found := false
		for i, m := range next {
			if m.ID == in.ID {
				next = append(next[:i], next[i+1:]...)
				found = true
				break
			}
		}
		if !found {
			failure(w, 404, "管理员不存在")
			return
		}
	default:
		failure(w, 400, "未知管理员操作")
		return
	}
	raw, _ := json.Marshal(next)
	warning, err := replacePrivateBounded(s.auth.root, membersFile, raw, 32768)
	if err != nil {
		failure(w, 503, "管理员变更未保存，请检查数据卷")
		return
	}
	s.auth.members = next
	s.auth.generation++
	for k, session := range s.auth.sessions {
		if in.Action == "revoke" && session.Actor == in.ID {
			delete(s.auth.sessions, k)
		}
	}
	respond(w, 200, map[string]any{"ok": true, "key": secret, "durability_warning": warning})
}

func replacePrivateBounded(root *os.Root, name string, raw []byte, limit int64) (bool, error) {
	if int64(len(raw)) > limit {
		return false, errors.New("private record too large")
	}
	if _, err := root.Lstat(name); err == nil {
		b, e := readPrivateFile(root, name, limit)
		clear(b)
		if e != nil {
			return false, e
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	id, err := randomSecret()
	if err != nil {
		return false, err
	}
	temp := ".ops-" + id + ".tmp"
	defer root.Remove(temp)
	if err = writePrivateNew(root, temp, raw); err != nil {
		return false, err
	}
	if err = root.Rename(temp, name); err != nil {
		return false, err
	}
	return syncAuthDir(root), nil
}
