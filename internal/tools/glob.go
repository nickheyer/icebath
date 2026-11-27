package tools

import (
	"regexp"
	"strings"
)

// Whether the whole name matches the glob, case kept: * and ? match any character, slashes included, [...] is a set with ! for none of it
func Glob(name, pattern string) bool {
	re := CompileGlob(pattern)
	return re != nil && re.MatchString(name)
}

// Expression of the glob as Glob reads it, for a caller matching many names; nil for a pattern that makes none
func CompileGlob(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString(`(?s)\A`)
	p := []rune(pattern)
	for i := 0; i < len(p); {
		c := p[i]
		i++
		switch c {
		case '*':
			for i < len(p) && p[i] == '*' {
				i++
			}
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '[':
			j := i
			if j < len(p) && p[j] == '!' {
				j++
			}
			if j < len(p) && p[j] == ']' {
				j++
			}
			for j < len(p) && p[j] != ']' {
				j++
			}
			if j >= len(p) {
				b.WriteString(`\[`)
				continue
			}
			set := string(p[i:j])
			i = j + 1
			switch {
			case set == "":
				b.WriteString(`[^\x00-\x{10FFFF}]`)
			case set == "!":
				b.WriteString(".")
			default:
				set = strings.ReplaceAll(set, `\`, `\\`)
				if strings.HasPrefix(set, "!") {
					set = "^" + set[1:]
				} else if strings.HasPrefix(set, "^") {
					set = `\` + set
				}
				b.WriteString("[" + strings.ReplaceAll(set, "[", `\[`) + "]")
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(`\z`)
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}
