package pybase64

import (
	"errors"
	"fmt"
)

// ErrNonASCII is the ValueError base64.b64decode raises for a str holding non ASCII characters
var ErrNonASCII = errors.New("string argument should contain only ASCII characters")

// Error is a binascii.Error raised by the decoder
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// Decodes like base64.b64decode without validate, skipping non alphabet bytes and stopping at completed padding
func B64Decode(s string) ([]byte, error) {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return nil, ErrNonASCII
		}
	}
	out := make([]byte, 0, len(s)/4*3+3)
	quadPos, pads := 0, 0
	var left byte
	for i := range len(s) {
		c := s[i]
		if c == '=' {
			if quadPos >= 2 {
				pads++
				if quadPos+pads >= 4 {
					return out, nil
				}
			}
			continue
		}
		v, ok := alphabetValue(c)
		if !ok {
			continue
		}
		pads = 0
		switch quadPos {
		case 0:
			quadPos, left = 1, v
		case 1:
			out = append(out, left<<2|v>>4)
			quadPos, left = 2, v&0x0f
		case 2:
			out = append(out, left<<4|v>>2)
			quadPos, left = 3, v&0x03
		case 3:
			out = append(out, left<<6|v)
			quadPos, left = 0, 0
		}
	}
	switch quadPos {
	case 0:
		return out, nil
	case 1:
		return nil, &Error{fmt.Sprintf("Invalid base64-encoded string: number of data characters (%d) cannot be 1 more than a multiple of 4", len(out)/3*4+1)}
	}
	return nil, &Error{"Incorrect padding"}
}

// Six bit value of a standard alphabet byte
func alphabetValue(c byte) (byte, bool) {
	switch {
	case c >= 'A' && c <= 'Z':
		return c - 'A', true
	case c >= 'a' && c <= 'z':
		return c - 'a' + 26, true
	case c >= '0' && c <= '9':
		return c - '0' + 52, true
	case c == '+':
		return 62, true
	case c == '/':
		return 63, true
	}
	return 0, false
}

// Decodes like base64.b64decode with validate=True, binascii strict mode rejecting stray bytes and padding misuse
func B64DecodeValidate(s string) ([]byte, error) {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return nil, ErrNonASCII
		}
	}
	out := make([]byte, 0, len(s)/4*3+3)
	quadPos, pads := 0, 0
	paddingStarted := false
	var left byte
	for i := range len(s) {
		c := s[i]
		if c == '=' {
			paddingStarted = true
			if quadPos == 0 {
				if i == 0 {
					return nil, &Error{"Leading padding not allowed"}
				}
				return nil, &Error{"Excess padding not allowed"}
			}
			if quadPos >= 2 {
				pads++
				if quadPos+pads >= 4 {
					if i+1 < len(s) {
						return nil, &Error{"Excess data after padding"}
					}
					return out, nil
				}
			}
			continue
		}
		v, ok := alphabetValue(c)
		if !ok {
			return nil, &Error{"Only base64 data is allowed"}
		}
		if paddingStarted {
			return nil, &Error{"Discontinuous padding not allowed"}
		}
		pads = 0
		switch quadPos {
		case 0:
			quadPos, left = 1, v
		case 1:
			out = append(out, left<<2|v>>4)
			quadPos, left = 2, v&0x0f
		case 2:
			out = append(out, left<<4|v>>2)
			quadPos, left = 3, v&0x03
		case 3:
			out = append(out, left<<6|v)
			quadPos, left = 0, 0
		}
	}
	switch quadPos {
	case 0:
		return out, nil
	case 1:
		return nil, &Error{fmt.Sprintf("Invalid base64-encoded string: number of data characters (%d) cannot be 1 more than a multiple of 4", len(out)/3*4+1)}
	}
	return nil, &Error{"Incorrect padding"}
}
