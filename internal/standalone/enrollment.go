package standalone

import (
	"net/http"
	"time"
)

func (s *Server) serveEnrollmentClaim(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, POST")
		failure(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		failure(w, http.StatusBadRequest, "登记码接口不接受 URL 参数")
		return
	}
	if r.Method == http.MethodGet {
		respond(w, 200, s.hub.AdminEnrollmentClaims())
		return
	}
	var in struct {
		Confirm bool   `json:"confirm"`
		UserID  string `json:"user_id"`
		Revoke  string `json:"revoke"`
	}
	if !strictJSON(w, r, &in) {
		return
	}
	if !in.Confirm {
		failure(w, http.StatusBadRequest, "请明确确认生成一次性登记码")
		return
	}
	s.resourceMu.Lock()
	defer s.resourceMu.Unlock()
	if s.stopped.Load() {
		failure(w, http.StatusServiceUnavailable, "Hub 正在重启")
		return
	}
	if in.Revoke != "" {
		if err := s.hub.RevokeEnrollmentClaim(in.Revoke); err != nil {
			failure(w, 400, err.Error())
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	claim, expires, err := s.hub.CreateDeviceEnrollmentClaimForUser(in.UserID)
	if err != nil {
		failure(w, http.StatusServiceUnavailable, "无法安全生成登记码，请检查 Hub 状态与数据卷")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, http.StatusCreated, map[string]any{"claim": claim, "expires_at": expires.UTC().Format(time.RFC3339)})
}
