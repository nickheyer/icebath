package webtext

import (
	"bytes"
	"regexp"
	"strings"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/unicode"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
)

const metaScanBytes = 8192

// JavaScript whitespace class, wider than the ASCII class of RE2
const ws = `\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

var (
	headerCharset = regexp.MustCompile(`(?i)(?:^|;)[` + ws + `]*charset[` + ws + `]*=[` + ws + `]*(?:"([^"]+)"|'([^']+)'|([^;` + ws + `]+))`)
	metaTag       = regexp.MustCompile(`(?i)<meta\b[^>]*>`)
	metaCharset   = regexp.MustCompile(`(?i)\bcharset[` + ws + `]*=[` + ws + `]*(?:"([^"]+)"|'([^']+)'|([^` + ws + `"'/>;]+))`)
	metaContent   = regexp.MustCompile(`(?i)\bcontent[` + ws + `]*=[` + ws + `]*(?:"([^"]*)"|'([^']*)'|([^` + ws + `>]+))`)
)

// Trims the whitespace JavaScript String.prototype.trim removes
func jsTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		switch r {
		case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
			return true
		}
		return r >= 0x2000 && r <= 0x200a
	})
}

// First 64 UTF-16 units of s
func jsSlice64(s string) string {
	units := 0
	for i, r := range s {
		w := 1
		if r > 0xffff {
			w = 2
		}
		if units+w > 64 {
			if units < 64 {
				return s[:i] + "�"
			}
			return s[:i]
		}
		units += w
	}
	return s
}

// First non empty capture of a match
func firstGroup(m []string) string {
	for _, g := range m[1:] {
		if g != "" {
			return g
		}
	}
	return ""
}

func charsetFromContentType(contentType string) string {
	m := headerCharset.FindStringSubmatch(contentType)
	if m == nil {
		return ""
	}
	return jsSlice64(jsTrim(firstGroup(m)))
}

// Charset an HTML meta declaration names within the first 8 KiB, read as windows-1252
func charsetFromHTML(data []byte) string {
	head := data
	if len(head) > metaScanBytes {
		head = head[:metaScanBytes]
	}
	text, _ := decodeWith("windows-1252", head)
	for _, tag := range metaTag.FindAllString(text, -1) {
		label := ""
		if m := metaCharset.FindStringSubmatch(tag); m != nil {
			label = firstGroup(m)
		}
		if label == "" {
			if c := metaContent.FindStringSubmatch(tag); c != nil {
				if n := headerCharset.FindStringSubmatch(firstGroup(c)); n != nil {
					label = firstGroup(n)
				}
			}
		}
		if label != "" {
			return jsSlice64(jsTrim(label))
		}
	}
	return ""
}

// Decodes data as a TextDecoder for label does, false for a label the Encoding standard does not know
func decodeWith(label string, data []byte) (string, bool) {
	enc, err := htmlindex.Get(label)
	if err != nil || enc == encoding.Replacement {
		return "", false
	}
	name, _ := htmlindex.Name(enc)
	switch name {
	case "utf-8":
		text, _ := pycodecs.DecodeReplace(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf}), "utf-8")
		return text, true
	case "utf-16le":
		enc = unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM)
		data = bytes.TrimPrefix(data, []byte{0xff, 0xfe})
	case "utf-16be":
		enc = unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM)
		data = bytes.TrimPrefix(data, []byte{0xfe, 0xff})
	}
	out, err := decodeAll(enc, data)
	if err != nil {
		return "", false
	}
	return out, true
}

func decodeAll(enc encoding.Encoding, data []byte) (string, error) {
	out, err := enc.NewDecoder().Bytes(data)
	return string(out), err
}

// Decodes fetched page bytes as a browser picks the encoding, header charset then meta declaration then UTF-8
func DecodeWebText(data []byte, contentType string) string {
	labels := []string{charsetFromContentType(contentType), charsetFromHTML(data), "utf-8"}
	for i, label := range labels {
		if label == "" || indexOf(labels, label) != i {
			continue
		}
		if text, ok := decodeWith(label, data); ok {
			return text
		}
	}
	text, _ := decodeWith("utf-8", data)
	return text
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}
