package hub

import (
	"encoding/json"
	"errors"
	"strings"
)

// The phone selects an authorized CLI session, never the internal owner used
// by the Bridge's temporary agent. Other payload fields are validated there.
func validateToolsSideQuestionCommand(meta map[string]json.RawMessage) error {
	if commandString(meta, "type") != "session.side-question.tools" {
		return nil
	}
	for key := range meta {
		if strings.EqualFold(key, "owner") || strings.EqualFold(key, "action") && key != "action" {
			return errors.New("invalid temporary tool command owner or action")
		}
	}
	var surface, action, session string
	if json.Unmarshal(meta["controlSurface"], &surface) != nil || surface != "cli" || json.Unmarshal(meta["sessionKey"], &session) != nil || strings.TrimSpace(session) == "" || json.Unmarshal(meta["action"], &action) != nil {
		return errors.New("temporary tool command requires a CLI session")
	}
	switch action {
	case "open", "read", "features", "send", "stop", "approve", "close":
		return nil
	}
	return errors.New("unknown temporary tool action")
}
