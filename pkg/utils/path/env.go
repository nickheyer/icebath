package pathutil

import (
	"os"
	"runtime"
	"strings"
)

// Text with $name and ${name} references, and on Windows also %name%, $$, %% and literal 'quoted' spans, replaced from the environment, references to unset variables left as written
func ExpandEnv(s string) string {
	if runtime.GOOS == "windows" {
		return expandWindowsVars(s, os.LookupEnv)
	}
	return expandPosixVars(s, os.LookupEnv)
}

func isVarNameByte(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_'
}

// Length of the run of variable name bytes at the start of s, a hyphen counting when dash is set
func varNameLen(s string, dash bool) int {
	n := 0
	for n < len(s) && (isVarNameByte(s[n]) || dash && s[n] == '-') {
		n++
	}
	return n
}

// Value of the variable for a reference, or the reference as written when the variable is unset
func lookupOr(b *strings.Builder, lookup func(string) (string, bool), name, ref string) {
	if v, ok := lookup(name); ok && name != "" {
		b.WriteString(v)
		return
	}
	b.WriteString(ref)
}

func expandPosixVars(s string, lookup func(string) (string, bool)) string {
	i := strings.IndexByte(s, '$')
	if i < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for ; i >= 0; i = strings.IndexByte(s, '$') {
		b.WriteString(s[:i])
		s = s[i:]
		switch {
		case len(s) > 1 && s[1] == '{':
			end := strings.IndexByte(s, '}')
			if end < 0 {
				b.WriteString(s)
				return b.String()
			}
			lookupOr(&b, lookup, s[2:end], s[:end+1])
			s = s[end+1:]
		default:
			n := varNameLen(s[1:], false)
			if n == 0 {
				b.WriteByte('$')
				s = s[1:]
				continue
			}
			lookupOr(&b, lookup, s[1:1+n], s[:1+n])
			s = s[1+n:]
		}
	}
	b.WriteString(s)
	return b.String()
}

func expandWindowsVars(s string, lookup func(string) (string, bool)) string {
	if !strings.ContainsAny(s, "$%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for s != "" {
		i := strings.IndexAny(s, `'%$`)
		if i < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:i])
		s = s[i:]
		switch {
		case s[0] == '\'':
			end := strings.IndexByte(s[1:], '\'')
			if end < 0 {
				b.WriteString(s)
				return b.String()
			}
			b.WriteString(s[:end+2])
			s = s[end+2:]
		case strings.HasPrefix(s, "%%"), strings.HasPrefix(s, "$$"):
			b.WriteByte(s[0])
			s = s[2:]
		case s[0] == '%':
			end := strings.IndexByte(s[1:], '%')
			if end < 0 {
				b.WriteString(s)
				return b.String()
			}
			lookupOr(&b, lookup, s[1:end+1], s[:end+2])
			s = s[end+2:]
		case len(s) > 1 && s[1] == '{':
			end := strings.IndexByte(s, '}')
			if end < 0 {
				b.WriteString(s)
				return b.String()
			}
			lookupOr(&b, lookup, s[2:end], s[:end+1])
			s = s[end+1:]
		default:
			n := varNameLen(s[1:], true)
			if n == 0 {
				b.WriteByte('$')
				s = s[1:]
				continue
			}
			lookupOr(&b, lookup, s[1:1+n], s[:1+n])
			s = s[1+n:]
		}
	}
	return b.String()
}
