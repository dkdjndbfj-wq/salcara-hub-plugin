package hub

import "net/http"

// Public service discovery never interprets credentials. Effective user policy
// is a separate read so a stale binding cannot hide a healthy Hub or leak an
// invitation-specific source list into anonymous discovery.
func discoveryPolicyView(p RemoteAccessPolicy) map[string]any {
	return map[string]any{"mode": p.Mode, "revision": p.Revision, "issuerCount": len(p.Issuers), "station": p.Station}
}

func writeCodedError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": message, "code": code})
}

func (h *Hub) handleDiscoveryPolicy(w http.ResponseWriter, r *http.Request, _ string) {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeCodedError(w, http.StatusBadRequest, "invalid_request", "来源策略查询不接受 URL 参数")
		return
	}
	claim, id, secret := r.Header.Get("X-Salcara-Enrollment-Claim"), r.Header.Get("X-Salcara-Device-Id"), r.Header.Get("X-Salcara-Device-Secret")
	for _, name := range []string{"X-Salcara-Enrollment-Claim", "X-Salcara-Device-Id", "X-Salcara-Device-Secret", "Authorization", "X-Salcara-Pair-Token", "X-Salcara-Bot-Token"} {
		if len(r.Header.Values(name)) > 1 {
			writeCodedError(w, http.StatusBadRequest, "ambiguous_credentials", "请只使用一组设备凭证或登记码")
			return
		}
	}
	if claim != "" && (id != "" || secret != "") || r.Header.Get("Authorization") != "" || r.Header.Get("X-Salcara-Pair-Token") != "" || r.Header.Get("X-Salcara-Bot-Token") != "" {
		writeCodedError(w, http.StatusBadRequest, "ambiguous_credentials", "请只使用一组设备凭证或登记码")
		return
	}
	if claim == "" && id == "" && secret == "" {
		writeCodedError(w, http.StatusUnauthorized, "device_credentials_required", "来源策略查询需要设备凭证或登记码")
		return
	}
	p, scope, code := h.scopedDiscoveryPolicy(r)
	if code != "" {
		if code == "user_disabled" {
			writeManagedUserDisabled(w)
			return
		}
		ip := clientIP(r, h.cfg.TrustProxy)
		if h.limiter.blocked(ip) {
			writeCodedError(w, http.StatusTooManyRequests, "remote_rate_limited", "身份验证失败次数过多，请稍后重试")
			return
		}
		h.limiter.fail(ip)
		writeCodedError(w, http.StatusForbidden, code, "设备凭据或登记码无效")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "scope": scope, "remoteAccess": discoveryPolicyView(p)})
}
