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
		data = data[de.Pos+1:]
	}
	return b.String(), nil
}
