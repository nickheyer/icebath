package imaging

import (
	"encoding/base64"
	"errors"
	"strings"
)

// Base64 data URL of the bytes under the media type
func DataURL(mediaType string, data []byte) string {
	var b strings.Builder
	b.Grow(len("data:;base64,") + len(mediaType) + base64.StdEncoding.EncodedLen(len(data)))
	b.WriteString("data:")
	b.WriteString(mediaType)
	b.WriteString(";base64,")
	b.WriteString(base64.StdEncoding.EncodeToString(data))
	return b.String()
}

// Length of the data URL the media type and byte count would produce
func dataURLLen(mediaType string, n int) int {
	return len("data:;base64,") + len(mediaType) + base64.StdEncoding.EncodedLen(n)
}

// Media type and payload of a data URL header, the type empty when the header names none
func splitDataURL(u string) (mediaType, payload string) {
	header, payload, _ := strings.Cut(u, ",")
	if strings.HasPrefix(header, "data:") {
		mediaType = strings.TrimSpace(strings.SplitN(header[len("data:"):], ";", 2)[0])
	}
	return mediaType, payload
}

// Error for base64 text whose data characters stop one short of a whole quantum or lack padding
var errBase64Padding = errors.New("imaging: incorrect base64 padding")

// Decodes base64 discarding characters outside the alphabet and stopping at a completed padding sequence
func decodeBase64Lenient(s string) ([]byte, error) {
	out := make([]byte, 0, len(s)*3/4)
	var left byte
	quad, pads := 0, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '=' {
			if quad >= 2 {
				pads++
				if quad+pads >= 4 {
					return out, nil
				}
			}
			continue
		}
		v, ok := base64Value(c)
		if !ok {
			continue
		}
		pads = 0
		switch quad {
		case 0:
			left, quad = v, 1
		case 1:
			out = append(out, left<<2|v>>4)
			left, quad = v&0x0f, 2
		case 2:
			out = append(out, left<<4|v>>2)
			left, quad = v&0x03, 3
		case 3:
			out = append(out, left<<6|v)
			left, quad = 0, 0
		}
	}
	if quad != 0 {
		return nil, errBase64Padding
	}
	return out, nil
}

// Six bit value of a standard alphabet character
func base64Value(c byte) (byte, bool) {
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
