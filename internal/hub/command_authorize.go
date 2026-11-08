package hub

import "net/http"

func (h *Hub) authorizeAppCommand(acct string, r *http.Request, dev *device) bool {
	if dev == nil || h.userDisabledLocked(dev.info.DeviceID) {
		return false
	}
	// Receipt workers resolve a Bot target to its device account before they
	// start. Preserve the Bot group's authorization at that boundary; a Bot
	// phone does not need a separate QR pair for every computer in its group.
	if g := h.botGroupForTokenLocked(r); g != nil && dev != nil && g.Computers[dev.info.DeviceID] {
		return true
	}
	return h.pairedDeviceLocked(acct, r) == dev
}
