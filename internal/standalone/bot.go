package standalone

import (
	"errors"
	"net/http"
	"strings"

	"salcara/hub/internal/hub"
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
	if r.URL.Path == Prefix+"/_admin/v1/bot/phone/reset" {
		s.serveBotPhoneReset(w, r)
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
			if errors.Is(err, hub.ErrBotPhoneNotFound) {
				failure(w, http.StatusNotFound, "Bot 组不存在")
			} else {
				failure(w, http.StatusServiceUnavailable, "停止新增绑定尚未确认，请保留原组并重试")
			}
			return
		}
		respond(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	failure(w, http.StatusNotFound, "管理接口不存在")
}

func (s *Server) serveBotPhoneReset(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		failure(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var in hub.BotPhoneReset
	if !strictJSON(w, r, &in) {
		return
	}
	if err := hub.ValidateBotPhoneReset(in); err != nil {
		failure(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !s.annotateBotPhoneReset(w, in) {
		return
	}
	s.resourceMu.Lock()
	defer s.resourceMu.Unlock()
	if s.stopped.Load() {
		failure(w, http.StatusServiceUnavailable, "bot_phone_reset_pending")
		return
	}
	result, err := s.hub.ResetBotPhone(in)
	if err != nil {
		switch {
		case errors.Is(err, hub.ErrBotPhoneConflict):
			failure(w, http.StatusConflict, "bot_phone_revision_conflict")
		case errors.Is(err, hub.ErrBotPhoneUnavailable):
			failure(w, http.StatusServiceUnavailable, "bot_phone_reset_pending")
		case errors.Is(err, hub.ErrBotPhoneNotFound):
			failure(w, http.StatusNotFound, "bot_group_not_found")
		default:
			failure(w, http.StatusBadRequest, "invalid_request")
		}
		return
	}
	respond(w, http.StatusOK, map[string]any{"ok": true, "groupId": in.GroupID, "phoneRevision": result.PhoneRevision, "binding": result.Binding})
}

func (s *Server) annotateBotPhoneReset(w http.ResponseWriter, in hub.BotPhoneReset) bool {
	out, ok := w.(*auditWriter)
	if !ok {
		failure(w, http.StatusServiceUnavailable, "手机重置尚未执行：缺少审计记录")
		return false
	}
	s.operations.mu.Lock()
	defer s.operations.mu.Unlock()
	for i := range s.operations.Records {
		if s.operations.Records[i].ID != out.id {
			continue
		}
		// Only validated public group identity and the operator's reason. Never
		// persist the request body, replacement invitation or phone credentials.
		s.operations.Records[i].Detail = "bot-phone-reset"
		s.operations.Records[i].Target = in.GroupID
		s.operations.Records[i].Reason = in.Reason
		if err := s.operations.saveLocked(); err != nil || s.operations.warning {
			failure(w, http.StatusServiceUnavailable, "手机重置尚未执行：审计记录无法保存")
			return false
		}
		return true
	}
	failure(w, http.StatusServiceUnavailable, "手机重置尚未执行：缺少审计记录")
	return false
}
