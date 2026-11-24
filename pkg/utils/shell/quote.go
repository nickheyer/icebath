package shellutil

import "strings"

// One argument quoted for a POSIX shell: as is when every byte is a letter, a digit or one of _@%+=:,./- and it does not start with the = that zsh expands to a command path, otherwise in single quotes
func Quote(s string) string {
	if !needsQuotes(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 2)
	writeQuoted(&b, s)
	return b.String()
}

// Arguments quoted for a POSIX shell and joined by single spaces, the first also quoted when bare it would be read as a variable assignment, a reserved word or a job rather than the command it names
func Join(args []string) string {
	if len(args) == 0 {
		return ""
	}
	n := len(args) - 1
	for _, arg := range args {
		n += len(arg) + 2
	}
	var b strings.Builder
	b.Grow(n)
	for i, arg := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		if needsQuotes(arg) || i == 0 && notCommandName(arg) {
			writeQuoted(&b, arg)
		} else {
			b.WriteString(arg)
		}
	}
	return b.String()
}

func needsQuotes(s string) bool {
	if s == "" || s[0] == '=' {
		return true
	}
	for i := range len(s) {
		if !safeByte(s[i]) {
			return true
		}
	}
	return false
}

// Whether a non-empty word of safe bytes, left bare where a command name goes, means something else to the shell: NAME=value or NAME+=value assigns, a reserved word of sh, bash, ksh or zsh opens or closes a compound command, and a leading % names a job
func notCommandName(s string) bool {
	if s[0] == '%' {
		return true
	}
	if i := strings.IndexByte(s, '='); i > 0 && isName(strings.TrimSuffix(s[:i], "+")) {
		return true
	}
	switch s {
	case "case", "coproc", "do", "done", "elif", "else", "end", "esac", "fi", "for", "foreach", "function", "if", "in",
		"nocorrect", "repeat", "select", "then", "time", "until", "while":
		return true
	}
	return false
}

// Whether s is a shell variable name: a letter or underscore, then letters, digits and underscores
func isName(s string) bool {
	if s == "" || '0' <= s[0] && s[0] <= '9' {
		return false
	}
	for i := range len(s) {
		if c := s[i]; c != '_' && !('a' <= c && c <= 'z') && !('A' <= c && c <= 'Z') && !('0' <= c && c <= '9') {
			return false
		}
	}
	return true
}

// Writes s inside single quotes, each single quote of s written as '"'"' so the quoting closes around it
func writeQuoted(b *strings.Builder, s string) {
	b.WriteByte('\'')
	for {
		i := strings.IndexByte(s, '\'')
		if i < 0 {
			break
		}
		b.WriteString(s[:i])
		b.WriteString(`'"'"'`)
		s = s[i+1:]
	}
	b.WriteString(s)
	b.WriteByte('\'')
}

func safeByte(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	switch c {
	case '_', '@', '%', '+', '=', ':', ',', '.', '/', '-':
		return true
	}
	return false
}
