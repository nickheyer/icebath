package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"time"
)

const relayDefaultUpgradeTTLSeconds = 300

// HMAC SHA256 hex digest of the payload under the secret, the connector sign
func RelaySign(payload, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// Unpadded base64url of payload, expiry and signature, expiry zero when the ttl is not positive
func RelayMakeToken(payload, secret string, ttlSeconds int) string {
	exp := int64(0)
	if ttlSeconds > 0 {
		exp = time.Now().Unix() + int64(ttlSeconds)
	}
	signed := payload + ":" + strconv.FormatInt(exp, 10)
	raw := signed + ":" + RelaySign(signed, secret)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// WS upgrade bearer whose payload is the gateway id the connector indexes its verify list by
func RelayMakeUpgradeToken(gatewayID, secret string, ttlSeconds int) string {
	return RelayMakeToken(gatewayID, secret, ttlSeconds)
}
