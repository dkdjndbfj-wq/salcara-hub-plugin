package hub

import (
	"strings"
	"testing"
)

func TestPushStaleScopeUnregisterCannotRemoveReplacement(t *testing.T) {
	h, google := pushHub(t, t.TempDir())
	id, secret := "scope-unregister", strings.Repeat("a", 64)
	pair := pairPhone(t, h, id, secret)
	registerPushScope(t, h, id, pair, fcmToken, notificationScopeA)
	registerPushScope(t, h, id, pair, fcmToken, notificationScopeB)
	registerPushScope(t, h, id, pair, "", notificationScopeA)
	postScopeTurn(t, h, id, secret, "after-old-unregister")
	data := waitMessages(t, google, 1)[0]["message"].(map[string]any)["data"].(map[string]any)
	if data["notificationScope"] != notificationScopeB {
		t.Fatalf("replacement scope lost: %v", data)
	}
}
