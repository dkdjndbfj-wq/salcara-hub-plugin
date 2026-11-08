package standalone

import (
	"net/http"
	"strings"
)

func (s *Server) serveBotAdmin(w http.ResponseWriter, r *http.Request) {
	if s.cfg.DeploymentMode != "bot" {
		failure(w, http.StatusNotFound, "Bot 模式未启用")
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == Prefix+"/_admin/v1/bot/invites" {
		respond(w, http.StatusOK, map[string]any{"groups": s.hub.BotGroupSummaries()})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		failure(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.URL.Path == Prefix+"/_admin/v1/bot/invites" {
		var in struct {
			Confirm bool `json:"confirm"`
		}
		if !strictJSON(w, r, &in) {
			return
		}
		if !in.Confirm {
			failure(w, http.StatusBadRequest, "请明确确认生成绑定包")
			return
		}
		pkg, err := s.hub.CreateBotBindingPackage()
		if err != nil {
			failure(w, http.StatusServiceUnavailable, "无法生成 Bot 绑定包")
			return
		}
		respond(w, http.StatusCreated, pkg)
		return
	}
	if r.URL.Path == Prefix+"/_admin/v1/bot/invites/revoke" {
		var in struct {
			GroupID string `json:"groupId"`
			Confirm bool   `json:"confirm"`
		}
		if !strictJSON(w, r, &in) {
			return
		}
		if !in.Confirm || strings.TrimSpace(in.GroupID) == "" {
			failure(w, http.StatusBadRequest, "需要确认和 groupId")
			return
		}
		if err := s.hub.RevokeBotGroup(in.GroupID); err != nil {
			failure(w, http.StatusNotFound, "Bot 组不存在")
			return
		}
		respond(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	failure(w, http.StatusNotFound, "管理接口不存在")
}
