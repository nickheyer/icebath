package executil

import "strings"

// Windows command line of the arguments on any host, each quoted so CommandLineToArgvW and the C runtime read it back unchanged
func WindowsCommandLine(args []string) string {
	n := len(args)
	for _, arg := range args {
		n += len(arg) + 2
	}
	var b strings.Builder
	b.Grow(n)
	for i, arg := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		writeWindowsArg(&b, arg)
	}
	return b.String()
}

// Writes arg in double quotes when it is empty or holds a space or tab, backslashes doubled before a quote mark and before the closing quote
func writeWindowsArg(b *strings.Builder, arg string) {
	quote := arg == "" || strings.ContainsAny(arg, " \t")
	if !quote && strings.IndexByte(arg, '"') < 0 {
		b.WriteString(arg)
		return
	}
	if quote {
		b.WriteByte('"')
	}
	slashes := 0
	for i := range len(arg) {
		c := arg[i]
		switch c {
		case '\\':
			slashes++
		case '"':
			for ; slashes > 0; slashes-- {
				b.WriteByte('\\')
			}
			b.WriteByte('\\')
		default:
			slashes = 0
		}
		b.WriteByte(c)
	}
	if quote {
		for ; slashes > 0; slashes-- {
			b.WriteByte('\\')
		}
		b.WriteByte('"')
	}
}
