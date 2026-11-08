package standalone

import (
	"net/http"
	"strings"

	"salcara/hub/internal/hub"
)

func (s *Server) serveIngressAdmin(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		failure(w, 400, "ingress_query_not_allowed")
		return
	}
	if s.stopped.Load() {
		failure(w, 503, "stopped")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, Prefix+"/_admin/v1/ingress/")
	method, known := map[string]string{"state": "GET", "save": "POST", "verify": "POST"}[path]
	if !known {
		failure(w, 404, "not_found")
		return
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		failure(w, 405, "method_not_allowed")
		return
	}
	if path == "state" {
		respond(w, 200, s.hub.IngressState())
		return
	}
	var state hub.IngressAdminState
	var err error
	if path == "save" {
		var body hub.IngressSave
		if !strictJSONLimit(w, r, &body, 16<<10) {
			return
		}
		state, err = s.hub.SaveIngress(body)
	} else {
		var body struct {
			ExpectedRevision *int64 `json:"expected_revision"`
			ID               string `json:"id"`
		}
		if !strictJSONLimit(w, r, &body, 4096) {
			return
		}
		state, err = s.hub.VerifyIngress(body.ExpectedRevision, body.ID)
	}
	if err != nil {
		status := 400
		switch err.Error() {
		case "revision_conflict", "ingress_busy":
			status = 409
		case "ingress_not_configured", "stopped":
			status = 503
		case "ingress_storage_failed":
			status = 500
		}
		failure(w, status, err.Error())
		return
	}
	status := 200
	if path == "verify" {
		status = 202
	}
	respond(w, status, state)
}
