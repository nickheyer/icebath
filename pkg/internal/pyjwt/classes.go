package pyjwt

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Payload of a compact token decoded with signature and expiry verification off
func UnverifiedClaims(token string) (*pyjson.Object, error) {
	if _, err := UnverifiedHeader(token); err != nil {
		return nil, err
	}
	parts := strings.Split(token, ".")
	raw, err := b64Segment(parts[1], "payload")
	if err != nil {
		return nil, err
	}
	payload, err := pyjson.LoadsDict(string(raw))
	if err != nil {
		return nil, jwtErr("DecodeError", "Invalid payload string: must be a json object")
	}
	return payload, nil
}
