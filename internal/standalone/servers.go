package standalone

import (
	"net/http"
	"salcara/hub/internal/hostfleet"
	"strings"
)

func (s *Server) serveServersAdmin(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, Prefix+"/_admin/v1/servers/")
	method, ok := map[string]string{"state": "GET", "discover": "POST", "check": "POST", "save": "POST", "plan": "POST", "deploy": "POST", "activate": "POST", "delete": "POST"}[path]
	if !ok {
		failure(w, 404, "服务器管理接口不存在")
		return
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		failure(w, 405, "method not allowed")
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		failure(w, 400, "服务器管理接口不接受 URL 参数")
		return
	}
	if s.servers == nil {
		failure(w, 503, "服务器管理不可用")
		return
	}
	var result any
	var err error
	switch path {
	case "state":
		result = s.servers.State()
	case "discover":
		var in struct {
			Host string `json:"host"`
			Port int    `json:"port"`
		}
		if !strictJSONLimit(w, r, &in, 4096) {
			return
		}
		result, err = s.servers.Discover(r.Context(), in.Host, in.Port)
	case "check":
		var in hostfleet.Input
		if !strictJSONLimit(w, r, &in, 96<<10) {
			return
		}
		result, err = s.servers.Check(r.Context(), in)
	case "save":
		var in struct {
			CheckID  string `json:"check_id"`
			Revision int64  `json:"expected_revision"`
		}
		if !strictJSONLimit(w, r, &in, 4096) {
			return
		}
		result, err = s.servers.Save(in.CheckID, in.Revision)
	case "plan":
		var in struct {
			ID     string `json:"id"`
			Origin string `json:"origin"`
		}
		if !strictJSONLimit(w, r, &in, 8192) {
			return
		}
		result, err = s.servers.Plan(in.ID, in.Origin)
	case "deploy":
		var in struct {
			PlanID   string `json:"plan_id"`
			Revision int64  `json:"expected_revision"`
		}
		if !strictJSONLimit(w, r, &in, 4096) {
			return
		}
		result, err = s.servers.Deploy(in.PlanID, in.Revision)
	case "activate", "delete":
		var in struct {
			ID       string `json:"id"`
			Revision int64  `json:"expected_revision"`
		}
		if !strictJSONLimit(w, r, &in, 4096) {
			return
		}
		if path == "activate" {
			result, err = s.servers.Activate(r.Context(), in.ID, in.Revision)
		} else {
			result, err = s.servers.Delete(in.ID, in.Revision)
		}
	}
	if err != nil {
		status := 400
		if strings.Contains(err.Error(), "版本") || strings.Contains(err.Error(), "配置已变化") {
			status = 409
		}
		failure(w, status, err.Error())
		return
	}
	status := 200
	if path == "deploy" {
		status = 202
	}
	respond(w, status, result)
}
