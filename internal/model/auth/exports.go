package auth

import (
	"encoding/json"
	"time"
)

func IsExpiring(expiresAt string, skew time.Duration) bool { return isExpiringISO(expiresAt, skew) }

func ParseISOTimestamp(value string) (time.Time, bool) {
	epoch, ok := parseISOStamp(value)
	if !ok {
		return time.Time{}, false
	}
	return timeOf(epoch), true
}

func UTCNowZ() string { return isoStampZ(time.Now()) }

func OptionalBaseURL(value string) string { return optionalBaseURL(value) }

func CoerceTTLSeconds(raw json.RawMessage) int64 {
	v, err := parseJSON(raw)
	if err != nil {
		return 0
	}
	return coerceTTL(v)
}

func TLSStateJSON(t TLSState) json.RawMessage { return rawOf(tlsState(t.Insecure, t.CABundle)) }

func TLSStateFrom(state json.RawMessage) TLSState {
	d, err := parseObject(state)
	if err != nil {
		return TLSState{}
	}
	return tlsStateOf(d)
}

func LastAuthErrorMarker(provider string, err *Error, reason, defaultCode string) json.RawMessage {
	return rawOf(lastAuthErrorMarker(provider, err, reason, defaultCode))
}

func QuarantineFlatOAuthState(state json.RawMessage, provider string, err *Error) (json.RawMessage, error) {
	d, perr := parseObject(state)
	if perr != nil {
		return nil, perr
	}
	quarantineFlatOAuthState(d, provider, err)
	return rawOf(d), nil
}

func JWTClaims(token string) (json.RawMessage, bool) {
	claims := decodeJWTClaims(token)
	if claims == nil {
		return nil, false
	}
	return rawOf(claims), true
}

func OAuthIdentity(row json.RawMessage) string {
	d, err := parseObject(row)
	if err != nil {
		return ""
	}
	return oauthIdentity(d)
}

func OAuthPairOrder(a, b json.RawMessage) (order int, known bool) {
	left, err := parseObject(a)
	if err != nil {
		return 0, false
	}
	right, err := parseObject(b)
	if err != nil {
		return 0, false
	}
	return oauthPairOrder(left, right)
}
