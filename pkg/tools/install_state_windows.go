package tools

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"

	"golang.org/x/sys/windows/registry"
)

// Homes older installers saved for this user under HKCU Environment, icebath first then hermes
func installRegistryHome(yield func(string) bool) {
	key, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE)
	if err != nil {
		return
	}
	defer key.Close()
	for _, name := range []string{"ICEBATH_HOME", "HERMES_HOME"} {
		text, ok := winregValueText(key, name)
		if !ok || strings.TrimFunc(text, pyIsSpace) == "" {
			continue
		}
		if !yield(osPathExpandVars(text)) {
			return
		}
	}
}

// The str of a registry value as winreg returns it, false when the value cannot be read
func winregValueText(key registry.Key, name string) (string, bool) {
	size, kind, err := key.GetValue(name, nil)
	if err != nil {
		return "", false
	}
	buf := make([]byte, size)
	if size > 0 {
		if size, kind, err = key.GetValue(name, buf); err != nil {
			return "", false
		}
		buf = buf[:size]
	}
	units := func() []uint16 {
		u := make([]uint16, len(buf)/2)
		for i := range u {
			u[i] = binary.LittleEndian.Uint16(buf[2*i:])
		}
		return u
	}
	switch kind {
	case registry.DWORD:
		if len(buf) < 4 {
			return "0", true
		}
		return strconv.FormatUint(uint64(binary.LittleEndian.Uint32(buf)), 10), true
	case registry.QWORD:
		if len(buf) < 8 {
			return "0", true
		}
		return strconv.FormatUint(binary.LittleEndian.Uint64(buf), 10), true
	case registry.SZ, registry.EXPAND_SZ:
		u := units()
		for i, c := range u {
			if c == 0 {
				u = u[:i]
				break
			}
		}
		return string(utf16.Decode(u)), true
	case registry.MULTI_SZ:
		u := units()
		var items []string
		start := 0
		for i := 0; i <= len(u); i++ {
			if i == len(u) || u[i] == 0 {
				if i == start {
					break
				}
				items = append(items, pyReprStr(string(utf16.Decode(u[start:i]))))
				start = i + 1
			}
		}
		return "[" + strings.Join(items, ", ") + "]", true
	}
	if len(buf) == 0 {
		return "None", true
	}
	return winregBytesRepr(buf), true
}

// Python repr of a bytes value with its quote choice and escapes
func winregBytesRepr(b []byte) string {
	quote := byte('\'')
	if strings.IndexByte(string(b), '\'') >= 0 && strings.IndexByte(string(b), '"') < 0 {
		quote = '"'
	}
	var sb strings.Builder
	sb.WriteString("b")
	sb.WriteByte(quote)
	for _, c := range b {
		switch {
		case c == quote || c == '\\':
			sb.WriteByte('\\')
			sb.WriteByte(c)
		case c == '\t':
			sb.WriteString(`\t`)
		case c == '\n':
			sb.WriteString(`\n`)
		case c == '\r':
			sb.WriteString(`\r`)
		case c < ' ' || c >= 0x7f:
			fmt.Fprintf(&sb, `\x%02x`, c)
		default:
			sb.WriteByte(c)
		}
	}
	sb.WriteByte(quote)
	return sb.String()
}
