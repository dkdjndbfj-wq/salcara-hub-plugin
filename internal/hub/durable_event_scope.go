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
			// RAM replay has no credential-generation column. Retire it at
			// the same boundary as the authenticated durable mailbox scope;
			// otherwise a replacement phone can read its predecessor's SSE
			// replay or session cache. Device accounts contain one computer.
			for sub := range a.apps {
				delete(a.apps, sub)
				sub.close()
			}
			if a.events != nil {
				a.events.clearCache()
				if a.events.evictedThrough < a.seq {
					a.events.evictedThrough = a.seq
				}
			}
			a.sessions = map[string]*sessionBuf{}
			a.sessionEvictedThrough = a.seq
			a.eventBatches = map[string]eventBatchReceipt{}
			a.eventBatchOrder = nil
			a.syncCacheBytesLocked(false)
			a.wakeEventWaitersLocked()
			if d := a.devices[id]; d != nil {
				if d.conn != nil {
					d.conn.close()
				}
				if d.standby != nil {
					d.standby.close()
				}
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
			if g.Computers[d.info.DeviceID] && g.PhoneTokenHash != "" && !g.PhonePending {
				return true
			}
		}
	}
	return false
}
