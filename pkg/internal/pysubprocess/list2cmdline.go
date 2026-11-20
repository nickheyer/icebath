package pysubprocess

import "strings"

// Windows command line of the argument sequence with the MS C runtime quoting subprocess uses
func List2cmdline(seq []string) string {
	var result strings.Builder
	for i, arg := range seq {
		if i > 0 {
			result.WriteByte(' ')
		}
		needquote := strings.ContainsAny(arg, " \t") || arg == ""
		if needquote {
			result.WriteByte('"')
		}
		backslashes := 0
		for _, c := range arg {
			switch c {
			case '\\':
				backslashes++
			case '"':
				result.WriteString(strings.Repeat(`\`, backslashes*2))
				backslashes = 0
				result.WriteString(`\"`)
			default:
				result.WriteString(strings.Repeat(`\`, backslashes))
				backslashes = 0
				result.WriteRune(c)
			}
		}
		result.WriteString(strings.Repeat(`\`, backslashes))
		if needquote {
			result.WriteString(strings.Repeat(`\`, backslashes))
			result.WriteByte('"')
		}
	}
	return result.String()
}
