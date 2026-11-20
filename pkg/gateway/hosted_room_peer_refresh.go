package gateway

import (
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Default seconds before dispatch expiry that a grant refresh is scheduled
const RoomGrantRefreshLeewaySeconds = 5 * 60

// Whether grant timing alone calls for a dispatch refresh, true for any unreadable token
func RoomGrantNeedsDispatchRefresh(token string, now *float64, leewaySeconds float64) bool {
	encoded, _, err := hostedSplitToken(token)
	if err != nil {
		return true
	}
	for _, b := range encoded {
		if b >= utf8.RuneSelf {
			return true
		}
	}
	decoded, err := pyjson.Loads(string(encoded))
	if err != nil {
		return true
	}
	payload, isDict := decoded.(*pyjson.Object)
	if !isDict {
		return true
	}
	raw, has := payload.Get("expires_at")
	if !has {
		return true
	}
	expiresAt, err := pyint.Float(raw)
	if err != nil {
		return true
	}
	return hostedClock(now)+max(0.0, leewaySeconds) >= expiresAt
}
