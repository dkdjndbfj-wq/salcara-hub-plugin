package hub

import "net/http"

func (h *Hub) bridgeEventScope(acct, deviceID string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if a := h.accounts[acct]; a != nil {
		return h.durableScopeLocked(acct, a.devices[deviceID])
	}
	return ""
}

// Bot ingress has no QR binding/phone headers. A frozen generation is therefore
// mandatory, including for old clients: accepting a missing scope after phone
// replacement would relabel old offline content as the new phone's content.
func (h *Hub) eventScopeMatchesLocked(acct string, dev *device, provided string) bool {
	if provided == "" {
		return h.cfg.DeploymentMode != "bot"
	}
	return hexHash(provided) && provided == h.durableScopeLocked(acct, dev)
}
func writeEventScopeChanged(w http.ResponseWriter) {
	writeJSON(w, http.StatusConflict, map[string]any{"code": "event_scope_changed", "error": "手机绑定代次已更换，请重新建立连接；旧批次不会转投新绑定", "retryable": false})
}

func (h *Hub) notifyBotScopeChangedLocked(g *botGroup) {
	if g == nil {
		return
	}
	for id := range g.Computers {
		if a := h.accounts[deviceAccount(id)]; a != nil {
			if d := a.devices[id]; d != nil {
				if d.conn != nil {
					d.conn.close()
				}
				if d.standby != nil {
					d.standby.close()
				}
				a.wakeEventWaitersLocked()
			}
		}
	}
}

func (h *Hub) hasMailboxPhoneLocked(d *device) bool {
	if d == nil || h.userDisabledLocked(d.info.DeviceID) {
		return false
	}
	if d.hasPair {
		return h.managedPairAllowedLocked(d)
	}
	if h.cfg.DeploymentMode == "bot" {
		for _, g := range h.botGroups {
			if g.Computers[d.info.DeviceID] && g.PhoneTokenHash != "" {
				return true
			}
		}
	}
	return false
}
