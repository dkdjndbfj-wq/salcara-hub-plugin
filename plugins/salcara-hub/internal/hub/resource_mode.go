package hub

import "errors"

// ResourcePreset describes bounded transient caches and admission limits, not
// stored device identities or an authoritative desktop conversation history.
// Cache and phone limits are per account; explicit global limits bound new
// concurrent streams/commands. Existing commands are never cancelled on mode
// changes. Go's memory limit is a soft runtime target, not an RSS guarantee.
type ResourcePreset struct {
	ID                        string `json:"id"`
	Label                     string `json:"label"`
	Description               string `json:"description"`
	MemoryLimitMiB            int64  `json:"memory_limit_mib"`
	AccountEvents             int    `json:"account_events"`
	AccountEventBytes         int64  `json:"account_event_bytes"`
	SessionEvents             int    `json:"session_events"`
	SessionEventBytes         int64  `json:"session_event_bytes"`
	MaxCachedSessions         int    `json:"max_cached_sessions"`
	TotalSessionEventBytes    int64  `json:"total_session_event_bytes"`
	GlobalEventCacheBytes     int64  `json:"global_event_cache_bytes"`
	MaxAppStreams             int    `json:"max_app_streams"`
	MaxGlobalAppStreams       int    `json:"max_global_app_streams"`
	AppQueueMessages          int    `json:"app_queue_messages"`
	AppQueueBytes             int64  `json:"app_queue_bytes"`
	MaxPendingCommands        int    `json:"max_pending_commands"`
	MaxAccountPendingCommands int    `json:"max_account_pending_commands"`
	BridgeQueueMessages       int    `json:"bridge_queue_messages"`
	MaxBridgeStreams          int    `json:"max_bridge_streams"`
}

func ResourceModes() []ResourcePreset {
	return []ResourcePreset{
		{ID: "economy", Label: "省资源", Description: "较小暂存与并发，适合轻量服务器；历史仍在电脑端。", MemoryLimitMiB: 96, AccountEvents: 256, AccountEventBytes: 2 << 20, SessionEvents: 128, SessionEventBytes: 512 << 10, MaxCachedSessions: 32, TotalSessionEventBytes: 4 << 20, GlobalEventCacheBytes: 12 << 20, MaxAppStreams: 4, MaxGlobalAppStreams: 32, AppQueueMessages: 32, AppQueueBytes: 512 << 10, MaxPendingCommands: 64, MaxAccountPendingCommands: 8, BridgeQueueMessages: 16, MaxBridgeStreams: 64},
		{ID: "balanced", Label: "均衡", Description: "增加暂存和并发，兼顾响应与服务器资源。", MemoryLimitMiB: 192, AccountEvents: 1000, AccountEventBytes: 4 << 20, SessionEvents: 256, SessionEventBytes: 1 << 20, MaxCachedSessions: 128, TotalSessionEventBytes: 16 << 20, GlobalEventCacheBytes: 48 << 20, MaxAppStreams: 8, MaxGlobalAppStreams: 128, AppQueueMessages: 128, AppQueueBytes: 2 << 20, MaxPendingCommands: 256, MaxAccountPendingCommands: 32, BridgeQueueMessages: 32, MaxBridgeStreams: 256},
		{ID: "performance", Label: "高负载", Description: "更长暂存和更多并发；请确保服务器有足够余量。", MemoryLimitMiB: 320, AccountEvents: 2000, AccountEventBytes: 8 << 20, SessionEvents: 500, SessionEventBytes: 2 << 20, MaxCachedSessions: 300, TotalSessionEventBytes: 32 << 20, GlobalEventCacheBytes: 96 << 20, MaxAppStreams: 32, MaxGlobalAppStreams: 256, AppQueueMessages: 512, AppQueueBytes: 4 << 20, MaxPendingCommands: 512, MaxAccountPendingCommands: 64, BridgeQueueMessages: 64, MaxBridgeStreams: 1024},
	}
}

func ResourceMode(id string) (ResourcePreset, bool) {
	for _, preset := range ResourceModes() {
		if preset.ID == id {
			return preset, true
		}
	}
	return ResourcePreset{}, false
}

func legacyResourceMode() ResourcePreset {
	// Preserve legacy/plugin count and admission defaults. Byte limits close
	// the old worst case of thousands of multi-megabyte cached events.
	return ResourcePreset{AccountEvents: accountRingSize, AccountEventBytes: 8 << 20, SessionEvents: sessionRingSize, SessionEventBytes: 2 << 20, MaxCachedSessions: maxSessions, TotalSessionEventBytes: 32 << 20, MaxAppStreams: maxAppStreams, AppQueueMessages: appQueueSize, AppQueueBytes: 4 << 20, MaxPendingCommands: maxPendingCommands, BridgeQueueMessages: commandQueueSize}
}

func (h *Hub) CurrentResourceMode() ResourcePreset {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.resource
}

// ApplyResourceMode joins the Hub drain barrier. before commits the standalone
// mode file before the in-memory change; an error leaves the prior policy
// untouched. It runs under the Hub lock and must not call back into the Hub.
func (h *Hub) ApplyResourceMode(id string, before func() error) error {
	preset, ok := ResourceMode(id)
	if !ok {
		return errors.New("unknown resource mode")
	}
	h.lifecycleMu.Lock()
	if h.stopped {
		h.lifecycleMu.Unlock()
		return errors.New("Hub is stopping")
	}
	h.activeRequests.Add(1)
	h.lifecycleMu.Unlock()
	defer h.activeRequests.Done()
	h.mu.Lock()
	defer h.mu.Unlock()
	if before != nil {
		if err := before(); err != nil {
			return err
		}
	}
	h.resource = preset
	for _, a := range h.accounts {
		a.resource = preset
		if a.events != nil {
			a.events.resize(preset.AccountEvents, preset.AccountEventBytes)
		}
		for _, sb := range a.sessions {
			sb.ring.resize(preset.SessionEvents, preset.SessionEventBytes)
		}
		for len(a.sessions) > preset.MaxCachedSessions {
			a.evictSessionLocked()
		}
		a.trimSessionBytesLocked()
		a.syncCacheBytesLocked(false)
		// Do not close/reallocate existing streams or command queues. Lower
		// admission limits apply to the next request, not an accepted task.
	}
	h.trimGlobalCacheLocked()
	h.adminAudit = append(h.adminAudit, AdminAudit{At: nowMs(), Action: "resource-mode", Reason: id, Actor: "standalone-admin"})
	if len(h.adminAudit) > 200 {
		h.adminAudit = h.adminAudit[len(h.adminAudit)-200:]
	}
	h.markDirty()
	return nil
}

func (h *Hub) appStreamCountLocked() int {
	n := 0
	for _, a := range h.accounts {
		n += len(a.apps)
	}
	return n
}
func (h *Hub) bridgeStreamCountLocked() int {
	n := 0
	for _, a := range h.accounts {
		for _, d := range a.devices {
			if d.conn != nil {
				n++
			}
		}
	}
	return n
}
func (h *Hub) accountPendingCountLocked(id string) int {
	n := 0
	for _, p := range h.pending {
		if p.account == id {
			n++
		}
	}
	return n
}

func (a *account) trimSessionBytesLocked() {
	var total int64
	for _, sb := range a.sessions {
		total += sb.ring.bytes
	}
	for total > a.resource.TotalSessionEventBytes && len(a.sessions) > 0 {
		old := len(a.sessions)
		a.evictSessionLocked()
		if len(a.sessions) == old {
			break
		}
		total = 0
		for _, sb := range a.sessions {
			total += sb.ring.bytes
		}
	}
}

func (a *account) syncCacheBytesLocked(touch bool) {
	var next int64
	if a.events != nil {
		next += a.events.bytes
	}
	for _, sb := range a.sessions {
		next += sb.ring.bytes
	}
	if a.owner != nil {
		h := a.owner
		h.eventCacheBytes += next - a.cacheBytes
		if next == 0 {
			if a.cacheEntry != nil {
				h.cacheOrder.Remove(a.cacheEntry)
				a.cacheEntry = nil
			}
		} else if a.cacheEntry == nil {
			a.cacheEntry = h.cacheOrder.PushBack(a)
		} else if touch {
			h.cacheOrder.MoveToBack(a.cacheEntry)
		}
	}
	a.cacheBytes = next
}

// Event bytes are counted conservatively in each referring ring, even if the
// same byte slice is shared. An O(1) LRU list avoids scanning every registered
// device for each event. Trimming clears payloads only; session/device metadata
// and cursor gap markers survive, never the authoritative desktop history.
func (h *Hub) trimGlobalCacheLocked() {
	limit := h.resource.GlobalEventCacheBytes
	if limit <= 0 {
		return
	}
	for h.eventCacheBytes > limit {
		front := h.cacheOrder.Front()
		if front == nil {
			break
		}
		a := front.Value.(*account)
		if a.events != nil {
			a.events.clearCache()
		}
		for _, sb := range a.sessions {
			sb.ring.clearCache()
		}
		a.syncCacheBytesLocked(false)
	}
}
