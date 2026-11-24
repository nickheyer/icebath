package charsetutil

import (
	"bytes"
	"errors"
	"slices"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/transform"
)

// Multi-byte character set an x/text encoding serves, fffd the byte sequence that stands for U+FFFD itself when the set has one
type xcodec struct {
	enc  encoding.Encoding
	fffd string
}

// The x/text decoders write U+FFFD for what they cannot decode, so a whole decode free of it is valid and anything else is located sequence by sequence
func (c xcodec) decode(data []byte, replace bool) (string, int, string) {
	out, err := c.enc.NewDecoder().Bytes(data)
	if err == nil && (replace || !bytes.ContainsRune(out, utf8.RuneError)) {
		return string(out), -1, ""
	}
	return c.decodeSteps(data, replace)
}

// Decode feeding the decoder one more byte until it takes a sequence, so the first sequence that yields U+FFFD without standing for it is found at its exact offset
func (c xcodec) decodeSteps(data []byte, replace bool) (string, int, string) {
	dec := c.enc.NewDecoder()
	out := make([]byte, 0, 2*len(data))
	var dst [16]byte
	for p := 0; p < len(data); {
		n, written, stuck, incomplete := 0, 0, false, false
		for end := p + 1; n == 0; end++ {
			nDst, nSrc, err := dec.Transform(dst[:], data[p:end], false)
			switch {
			case nSrc > 0:
				n, written = nSrc, nDst
			case !errors.Is(err, transform.ErrShortSrc):
				n, stuck = 1, true
			case end == len(data):
				nDst, nSrc, _ = dec.Transform(dst[:], data[p:end], true)
				n, written, incomplete = nSrc, nDst, true
				if n == 0 {
					n, written, stuck = end-p, 0, true
				}
			}
		}
		chunk := dst[:written]
		bad := stuck || bytes.ContainsRune(chunk, utf8.RuneError) && string(data[p:p+n]) != c.fffd
		switch {
		case !bad:
			out = append(out, chunk...)
		case !replace && incomplete:
			return "", p, "incomplete byte sequence"
		case !replace:
			return "", p, "invalid byte sequence"
		case stuck:
			out = append(out, replacement...)
		default:
			out = append(out, chunk...)
		}
		p += n
	}
	return string(out), -1, ""
}

func (c xcodec) encode(text string) ([]byte, int) {
	enc := c.enc.NewEncoder()
	src := []byte(text)
	out := make([]byte, 0, len(src)+len(src)/2+8)
	for p := 0; ; {
		nDst, nSrc, err := enc.Transform(out[len(out):cap(out)], src[p:], true)
		out, p = out[:len(out)+nDst], p+nSrc
		switch {
		case err == nil:
			return out, -1
		case errors.Is(err, transform.ErrShortDst):
			out = slices.Grow(out, len(src)-p+16)
		default:
			return nil, p
		}
	}
}
