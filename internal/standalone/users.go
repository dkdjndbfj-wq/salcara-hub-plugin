package standalone

import (
	"errors"
	"net/http"
	"salcara/hub/internal/hub"
)

func (s *Server) serveManagedUsers(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		failure(w, 400, "用户接口不接受 URL 参数")
		return
	}
	if r.Method == http.MethodGet {
		respond(w, 200, s.hub.ManagedUsersSnapshot())
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		failure(w, 405, "method not allowed")
		return
	}
	var in hub.ManagedUserChange
	if !strictJSONLimit(w, r, &in, 128<<10) {
		return
	}
	if err := hub.ValidateManagedUserChange(in); err != nil {
		failure(w, 400, err.Error())
		return
	}
	if !s.annotateManagedUserAction(w, in) {
		return
	}
	s.resourceMu.Lock()
	defer s.resourceMu.Unlock()
	if s.stopped.Load() {
		failure(w, 503, "Hub 正在重启")
		return
	}
	if err := s.hub.ApplyManagedUserChange(in); err != nil {
		status := 400
		if errors.Is(err, hub.ErrManagedUserConflict) {
			status = 409
		} else if errors.Is(err, hub.ErrManagedUserUnavailable) {
			status = 503
		}
		failure(w, status, err.Error())
		return
	}
	respond(w, 200, s.hub.ManagedUsersSnapshot())
}

func (s *Server) annotateManagedUserAction(w http.ResponseWriter, in hub.ManagedUserChange) bool {
	out, ok := w.(*auditWriter)
	if !ok {
		return false
	}
	s.operations.mu.Lock()
	defer s.operations.mu.Unlock()
	for i := range s.operations.Records {
		if s.operations.Records[i].ID == out.id {
			s.operations.Records[i].Detail = "user-" + in.Action
			s.operations.Records[i].Reason = in.Reason
			// Only bounded public identifiers; never store request bodies or source URLs.
			if len(in.ID) <= 64 {
				s.operations.Records[i].Target = in.ID
			}
			break
		}
	}
	if err := s.operations.saveLocked(); err != nil || s.operations.warning {
		failure(w, 503, "用户操作尚未执行：审计记录无法保存")
		return false
	}
	return true
}
