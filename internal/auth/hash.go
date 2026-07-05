package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// HashAPIKey returns the hex-encoded HMAC-SHA256 of raw keyed by secret. This
// is the only representation of an API key that is ever stored or compared: a
// leaked database row reveals no usable credential, and lookup stays a
// deterministic O(1) hash compare. The output is stable for a given
// (secret, raw) pair, which is what makes seeding and verification agree.
func HashAPIKey(secret, raw string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(raw))
	return hex.EncodeToString(mac.Sum(nil))
}
