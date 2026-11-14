//go:build windows

package aws

import (
	"errors"
	"os"
	"strings"
)

func shellSplit(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var components []string
	var buff []string
	quoted := false
	backslashes := 0
	flush := func() {
		if backslashes > 0 {
			buff = append(buff, strings.Repeat(`\`, backslashes))
			backslashes = 0
		}
	}
	for _, ch := range s {
		switch {
		case ch == '\\':
			backslashes++
		case ch == '"':
			if backslashes > 0 {
				buff = append(buff, strings.Repeat(`\`, backslashes/2))
				odd := backslashes%2 == 1
				backslashes = 0
				if odd {
					buff = append(buff, `"`)
					continue
				}
			}
			quoted = !quoted
			buff = append(buff, "")
		case (ch == ' ' || ch == '\t') && !quoted:
			flush()
			if len(buff) > 0 {
				components = append(components, strings.Join(buff, ""))
				buff = nil
			}
		default:
			flush()
			buff = append(buff, string(ch))
		}
	}
	if quoted {
		return nil, errors.New("no closing quotation")
	}
	flush()
	if len(buff) > 0 {
		components = append(components, strings.Join(buff, ""))
	}
	return components, nil
}

func expandPlatformVars(path string) string {
	if !strings.Contains(path, "%") {
		return path
	}
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		if path[i] != '%' {
			b.WriteByte(path[i])
			continue
		}
		end := strings.IndexByte(path[i+1:], '%')
		if end < 0 {
			b.WriteString(path[i:])
			break
		}
		name := path[i+1 : i+1+end]
		if value, ok := os.LookupEnv(name); ok && name != "" {
			b.WriteString(value)
		} else {
			b.WriteString(path[i : i+end+2])
		}
		i += end + 1
	}
	return b.String()
}
