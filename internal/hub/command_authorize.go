package hub

import "net/http"

func (h *Hub) authorizeAppCommand(acct string, r *http.Request, dev *device) bool {
	return h.pairedDeviceLocked(acct, r) == dev
}
