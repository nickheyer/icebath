package pyjwt

import (
	"errors"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Base class of each PyJWT exception class
var classParents = map[string]string{
	"DecodeError": "InvalidTokenError", "InvalidSignatureError": "DecodeError", "ExpiredSignatureError": "InvalidTokenError",
	"InvalidAudienceError": "InvalidTokenError", "InvalidIssuerError": "InvalidTokenError", "InvalidIssuedAtError": "InvalidTokenError",
	"ImmatureSignatureError": "InvalidTokenError", "InvalidJTIError": "InvalidTokenError", "InvalidSubjectError": "InvalidTokenError",
	"MissingRequiredClaimError": "InvalidTokenError", "InvalidAlgorithmError": "InvalidTokenError", "InvalidTokenError": "PyJWTError",
	"InvalidKeyError": "PyJWTError", "PyJWKError": "PyJWTError", "MissingCryptographyError": "PyJWKError", "PyJWKSetError": "PyJWTError",
	"PyJWKClientError": "PyJWTError", "PyJWKClientConnectionError": "PyJWKClientError",
}

// Whether a PyJWT exception class is the named class or one of its subclasses
func ClassIs(class, base string) bool {
	for c := class; c != ""; c = classParents[c] {
		if c == base {
			return true
		}
	}
	return false
}

// Whether err is a PyJWT exception of the named class or one of its subclasses
func IsInstance(err error, base string) bool {
	var jwtErr *Error
	return errors.As(err, &jwtErr) && ClassIs(jwtErr.Class, base)
}

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
