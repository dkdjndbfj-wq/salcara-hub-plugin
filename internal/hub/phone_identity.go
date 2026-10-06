package hub

import "encoding/hex"

// These values are matching metadata, never authentication credentials.
// Phone access still requires the exact station/device pair token.
func validPairHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && hex.EncodeToString(b) == s
}
