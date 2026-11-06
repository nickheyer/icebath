package pypath

import (
	"os"
	"regexp"
	"strings"
)

var (
	posixVarRe = regexp.MustCompile(`\$(\w+|\{[^}]*\}?)`)
	ntVarRe    = regexp.MustCompile(`'[^']*'?|%(%|[^%]*%?)|\$(\$|[-\w]+|\{[^}]*\}?)`)
)

// os.path.expandvars of the host over the process environment, unknown references left unchanged
func Expandvars(p string) string {
	if windows {
		return NtExpandvars(p, os.LookupEnv)
	}
	return PosixExpandvars(p, os.LookupEnv)
}

// posixpath.expandvars of CPython 3.14 over lookup
func PosixExpandvars(p string, lookup func(string) (string, bool)) string {
	if !strings.Contains(p, "$") {
		return p
	}
	return replaceGroups(posixVarRe, p, func(m []string, _ []bool) string {
		name := m[1]
		if strings.HasPrefix(name, "{") {
			if !strings.HasSuffix(name, "}") || len(name) < 2 {
				return m[0]
			}
			name = name[1 : len(name)-1]
		}
		if v, ok := lookup(name); ok {
			return v
		}
		return m[0]
	})
}

// ntpath.expandvars of CPython 3.14 over lookup, the single quoted spans kept literal
func NtExpandvars(p string, lookup func(string) (string, bool)) string {
	if !strings.Contains(p, "$") && !strings.Contains(p, "%") {
		return p
	}
	return replaceGroups(ntVarRe, p, func(m []string, took []bool) string {
		var name string
		switch {
		case took[2]:
			name = m[2]
			if name == "$" {
				return name
			}
			if strings.HasPrefix(name, "{") {
				if !strings.HasSuffix(name, "}") || len(name) < 2 {
					return m[0]
				}
				name = name[1 : len(name)-1]
			}
		case took[1]:
			name = m[1]
			if name == "%" {
				return name
			}
			if !strings.HasSuffix(name, "%") {
				return m[0]
			}
			name = name[:len(name)-1]
		default:
			return m[0]
		}
		if v, ok := lookup(name); ok {
			return v
		}
		return m[0]
	})
}

// Every match replaced by fn over its groups and which groups took part
func replaceGroups(re *regexp.Regexp, s string, fn func([]string, []bool) string) string {
	var b strings.Builder
	last := 0
	for _, loc := range re.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(s[last:loc[0]])
		groups := make([]string, len(loc)/2)
		took := make([]bool, len(loc)/2)
		for g := range groups {
			if loc[2*g] >= 0 {
				groups[g], took[g] = s[loc[2*g]:loc[2*g+1]], true
			}
		}
		b.WriteString(fn(groups, took))
		last = loc[1]
	}
	b.WriteString(s[last:])
	return b.String()
}
