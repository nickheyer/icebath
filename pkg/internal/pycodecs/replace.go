package pycodecs

import (
	"errors"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// bytes.decode with errors replace, each undecodable byte becoming U+FFFD as the CPython codecs do
func DecodeReplace(data []byte, name string) (string, error) {
	mod, ok := Lookup(name)
	if !ok {
		return "", &LookupError{Name: name}
	}
	switch mod {
	case "utf_8", "cp65001":
		return pystr.DecodeUTF8Replace(data), nil
	case "utf_8_sig":
		return pystr.DecodeUTF8Replace([]byte(strings.TrimPrefix(string(data), "\xef\xbb\xbf"))), nil
	case "utf_7":
		return decodeUTF7(data, true)
	case "unicode_escape":
		return decodeUnicodeEscape(data, true)
	case "raw_unicode_escape":
		return decodeRawUnicodeEscape(data, true)
	case "punycode":
		return decodePunycode(data, false)
	case "idna":
		return "", &UnicodeError{Msg: "Unsupported error handling: replace"}
	case "undefined":
		return "", &UnicodeError{Msg: "undefined encoding"}
	case "hz":
		return decodeHZ(data, true)
	}
	if v := isoVariants[mod]; v != nil {
		return v.decode(data, true)
	}
	if statelessModules[mod] {
		return loadStateless(mod).decode(data, true)
	}
	if nonTextModules[mod] {
		return "", &LookupError{Name: name, NotText: "decode"}
	}
	var b strings.Builder
	for len(data) > 0 {
		text, err := Decode(data, name)
		var de *UnicodeDecodeError
		if !errors.As(err, &de) {
			b.WriteString(text)
			return b.String(), err
		}
		head, err := Decode(data[:de.Pos], name)
		if err != nil {
			return "", err
		}
		b.WriteString(head)
		b.WriteRune('�')
		skip := de.Pos + 1
		if de.End > skip {
			skip = de.End
		}
		data = data[skip:]
	}
	return b.String(), nil
}
