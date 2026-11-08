package hub

import (
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

// The compatibility floor closes only admission of new work. Registration,
// events, approvals, interrupts and an already-dispatched reply stay available.
func desktopVersion(text string) ([3]uint64, bool) {
	var result [3]uint64
	if len(text) > 64 {
		return result, false
	}
	parts := strings.Split(text, ".")
	if len(parts) != 3 {
		return result, false
	}
	for i, part := range parts {
		if part == "" {
			return result, false
		}
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				return result, false
			}
		}
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return result, false
		}
		result[i] = n
	}
	return result, true
}

func ValidateDesktopCompatibility(minimum, updateURL string) error {
	if minimum != "" {
		if _, ok := desktopVersion(minimum); !ok {
			return errors.New("desktop minimum version must be three numeric components")
		}
	}
	if updateURL != "" {
		u, err := url.Parse(updateURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || len(updateURL) > 2048 {
			return errors.New("desktop update URL must be an HTTPS URL")
		}
	}
	return nil
}

func newDesktopWork(raw json.RawMessage) bool {
	var command struct {
		Type   string `json:"type"`
		Action string `json:"action"`
		Kind   string `json:"kind"`
	}
	if json.Unmarshal(raw, &command) != nil {
		return false
	}
	switch command.Type {
	case "session.start", "session.send", "session.continue", "desktop.session.send", "session.compact", "session.cooperation.prepare", "session.side-question.ask", "session.side-question.send", "workspace.terminal.open":
		return true
	case "session.workflow":
		return command.Kind != "" && command.Kind != "goal.pause" && command.Kind != "goal.clear"
	case "session.action":
		return command.Action == "fork"
	case "session.side-question.tools":
		return command.Action == "send"
	case "workspace.project.preview":
		return command.Action == "start"
	}
	return false
}

func (h *Hub) desktopUpdateRequired(version string, command json.RawMessage) bool {
	if h.cfg.DesktopMinimumVersion == "" || !newDesktopWork(command) {
		return false
	}
	minimum, _ := desktopVersion(h.cfg.DesktopMinimumVersion)
	current, valid := desktopVersion(strings.TrimPrefix(strings.SplitN(strings.SplitN(version, "+", 2)[0], "-", 2)[0], "v"))
	if !valid {
		return true
	}
	for i := range current {
		if current[i] != minimum[i] {
			return current[i] < minimum[i]
		}
	}
	return false
}

func (h *Hub) desktopCompatibility() map[string]string {
	return map[string]string{"minimumSupportedVersion": h.cfg.DesktopMinimumVersion, "downloadURL": h.cfg.DesktopUpdateURL}
}

func (h *Hub) desktopUpdateMessage() string {
	message := "update_required: 请将电脑端升级到 " + h.cfg.DesktopMinimumVersion + " 或更高版本后再发起新任务；已有任务、审批和停止仍可使用"
	if h.cfg.DesktopUpdateURL != "" {
		message += "。下载：" + h.cfg.DesktopUpdateURL
	}
	return message
}
