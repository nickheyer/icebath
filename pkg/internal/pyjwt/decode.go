package pyjwt

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Error is a PyJWT exception with its class name and message
type Error struct {
	Class string
	Msg   string
}

func (e *Error) Error() string { return e.Msg }

func jwtErr(class, format string, args ...any) error {
	return &Error{Class: class, Msg: fmt.Sprintf(format, args...)}
}

func b64Segment(seg, what string) ([]byte, error) {
	out, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(seg, "="))
	if err != nil {
		return nil, jwtErr("DecodeError", "Invalid %s padding", what)
	}
	return out, nil
}

// Header of a compact token read without verifying its signature
func UnverifiedHeader(token string) (*pyjson.Object, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, jwtErr("DecodeError", "Not enough segments")
	}
	raw, err := b64Segment(parts[0], "header")
	if err != nil {
		return nil, err
	}
	v, err := pyjson.Loads(string(raw))
	header, ok := v.(*pyjson.Object)
	if err != nil || !ok {
		return nil, jwtErr("DecodeError", "Invalid header string: must be a json object")
	}
	return header, nil
}
