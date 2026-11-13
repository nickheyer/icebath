package shell

import (
	"errors"
	"fmt"
	"strings"
)

// A cmd.exe script argument held a line break that would end its logical command line
var ErrNewline = errors.New("refusing to quote value containing newline")

// One word made safe for a POSIX shell, single quoted unless every byte is a letter, digit or safe punctuation
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_@%+=:,./-", c) >= 0 {
			continue
		}
		return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
	}
	return s
}

// POSIX command line of words, each quoted and separated by single spaces
func Join(words []string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = Quote(w)
	}
	return strings.Join(quoted, " ")
}

// Text safe to place between single quotes that a POSIX shell template already supplies
func InSingleQuotes(s string) string { return strings.ReplaceAll(s, "'", `'\''`) }

// Text safe to place between double quotes that a POSIX shell template already supplies
func InDoubleQuotes(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace(s)
}

// PowerShell single quoted literal of s, the ASCII quote and the four typographic quotes PowerShell also closes on doubled
func PowerShellLiteral(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\'', '‘', '’', '‚', '‛':
			b.WriteRune(r)
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
}

// One argument on a line of a cmd.exe script, received verbatim by a program or by cd, percent signs doubled
func CmdArg(s string) (string, error) {
	if strings.ContainsAny(s, "\r\n") {
		return "", fmt.Errorf("%w: %q", ErrNewline, s)
	}
	crt := windowsArg(s, s == "" || strings.ContainsAny(s, " \t\"&|<>^(),;="))
	var b strings.Builder
	open := false
	for i := 0; i < len(crt); i++ {
		switch c := crt[i]; {
		case c == '%':
			b.WriteString("%%")
		case c == '"':
			open = !open
			b.WriteByte(c)
		case !open && strings.IndexByte("&|<>^()", c) >= 0:
			b.WriteByte('^')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

// CreateProcess command line of args under the Microsoft C runtime parsing rules
func WindowsCommandLine(args []string) string {
	var b strings.Builder
	for i, arg := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(windowsArg(arg, arg == "" || strings.ContainsAny(arg, " \t")))
	}
	return b.String()
}

// One argument under the C runtime rules, backslashes doubled only where a quote follows them
func windowsArg(arg string, quote bool) string {
	var b strings.Builder
	if quote {
		b.WriteByte('"')
	}
	slashes := 0
	for j := 0; j < len(arg); j++ {
		switch c := arg[j]; c {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, 2*slashes+1))
			slashes = 0
			b.WriteByte('"')
		default:
			b.WriteString(strings.Repeat(`\`, slashes))
			slashes = 0
			b.WriteByte(c)
		}
	}
	b.WriteString(strings.Repeat(`\`, slashes))
	if quote {
		b.WriteString(strings.Repeat(`\`, slashes))
		b.WriteByte('"')
	}
	return b.String()
}
