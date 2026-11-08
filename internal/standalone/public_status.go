package standalone

// Public source status deliberately contains only fixed category/state labels.
// It describes authorization configuration, never account health or upstream
// reachability. No background probes or per-visitor server history are needed.
type publicSourceStatus struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type publicConnectionStatus struct {
	Status  string                `json:"status"`
	Sources [4]publicSourceStatus `json:"sources"`
}

func (s *Server) publicSourceStatuses() [4]publicSourceStatus {
	out := [4]publicSourceStatus{{ID: "api", Status: "unknown"}, {ID: "external_api", Status: "unknown"}, {ID: "reverse_proxy", Status: "unknown"}, {ID: "official_account", Status: "unknown"}}
	policy := s.hub.CurrentRemoteAccessPolicy()
	for _, source := range policy.SourceAuthorizationStatus() {
		id, _ := source["id"].(string)
		state, _ := source["status"].(string)
		switch state {
		case "disabled", "paused", "allowlist", "computer_verification", "issuer_configured", "issuer_missing":
		default:
			continue
		}
		for i := range out {
			if out[i].ID == id {
				out[i].Status = state
			}
		}
	}
	return out
}
