package standalone

import (
	"net/http"
	"salcara/hub/internal/hub"
)

// This endpoint is behind the standalone administrator session + CSRF gate.
// It returns aggregates only, never conversation text, images or identifiers.
func (s *Server) serveDurableStorage(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		failure(w, 400, "存储管理不接受 URL 参数")
		return
	}
	if r.Method == http.MethodGet {
		respond(w, 200, s.hub.DurableStorageStatus())
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		failure(w, 405, "method not allowed")
		return
	}
	var in struct {
		Policy *hub.StoragePolicy `json:"policy"`
		RunNow bool               `json:"runNow"`
	}
	if !strictJSON(w, r, &in) {
		return
	}
	s.resourceMu.Lock()
	defer s.resourceMu.Unlock()
	if s.stopped.Load() {
		failure(w, 503, "Hub 正在重启")
		return
	}
	if in.Policy == nil && !in.RunNow {
		failure(w, 400, "缺少存储设置")
		return
	}
	if in.Policy != nil {
		if err := s.hub.SetStoragePolicy(*in.Policy); err != nil {
			failure(w, 400, "存储设置无效或未能保存：保留 1–30 天、全站至少 64 MiB、每绑定至少 16 MiB 且不超过全站容量")
			return
		}
	}
	if in.RunNow {
		if err := s.hub.RunStorageCleanup(); err != nil {
			failure(w, 503, "清理尚未完成，可安全重试")
			return
		}
	}
	respond(w, 200, s.hub.DurableStorageStatus())
}
