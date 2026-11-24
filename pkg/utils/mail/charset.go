package mailutil

import (
	"io"
	"strings"
	"unicode/utf8"

	charsetutil "github.com/nickheyer/icebath/pkg/utils/charset"
)

// Text of bytes declared in a character set, forgiving the labels and mislabelled text real mail carries: bytes that are valid UTF-8 stay UTF-8 when the label is missing, unknown, ASCII or Latin-1, Windows-1252 reads bytes that are not, and sequences a known set cannot decode become U+FFFD
func decodeText(data []byte, label string) string {
	var canonical string
	known := false
	if name := charsetLabel(label); name != "" {
		canonical, known = charsetutil.Canonical(name)
	}
	var text string
	switch {
	case !known, canonical == "US-ASCII", canonical == "ISO-8859-1":
		if utf8.Valid(data) {
			text = string(data)
		} else {
			text = decodeReplacing(data, "windows-1252")
		}
	case canonical == "UTF-8":
		if utf8.Valid(data) {
			text = string(data)
		} else {
			text = decodeReplacing(data, canonical)
		}
	default:
		if decoded, err := charsetutil.Decode(data, canonical); err == nil {
			text = decoded
		} else if utf8.Valid(data) {
			text = string(data)
		} else {
			text = decodeReplacing(data, canonical)
		}
	}
	return strings.TrimPrefix(text, "\ufeff")
}

// Text of bytes in a known character set with undecodable sequences as U+FFFD
func decodeReplacing(data []byte, name string) string {
	if text, err := charsetutil.DecodeReplace(data, name); err == nil {
		return text
	}
	return strings.ToValidUTF8(string(data), "\uFFFD")
}

// Character set a label found in mail stands for, quotes and an RFC 2231 language suffix removed, placeholders such as unknown-8bit empty and labels that name a subset widened to the set senders actually use
func charsetLabel(label string) string {
	label = strings.ToLower(strings.Trim(strings.TrimSpace(label), `"'`))
	label, _, _ = strings.Cut(label, "*")
	switch label {
	case "unknown", "unknown-8bit", "x-unknown", "default":
		return ""
	case "ansi_x3.110-1983":
		return "ISO-8859-1"
	case "gb2312", "gbk":
		return "GB18030"
	}
	return label
}

// Header text that is not encoded, kept when it is UTF-8 and read as Windows-1252 when it is not
func plainText(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return decodeText([]byte(s), "")
}

// Reader of text in a character set for mime.WordDecoder, every label accepted
func charsetReader(label string, input io.Reader) (io.Reader, error) {
	data, err := io.ReadAll(input)
	if err != nil {
		return nil, err
	}
	return strings.NewReader(decodeText(data, label)), nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// Text with invalid UTF-8 replaced by U+FFFD
func validText(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "\uFFFD")
}
