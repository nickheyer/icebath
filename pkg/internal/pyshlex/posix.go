package pyshlex

import (
	"errors"
	"strings"
)

// Tokens of shlex.split in POSIX mode, a hash starting a comment through end of line when comments is set
func SplitPOSIX(s string, comments bool) ([]string, error) {
	const whitespace = " \t\r\n"
	src := []rune(s)
	pos := 0
	var out []string
	for {
		var token strings.Builder
		state, escapedState, quoted := ' ', 'a', false
		skipLine := func() {
			for pos < len(src) && src[pos] != '\n' {
				pos++
			}
			if pos < len(src) {
				pos++
			}
		}
	read:
		for {
			if pos >= len(src) {
				switch state {
				case '\'', '"':
					return nil, errors.New("No closing quotation")
				case '\\':
					return nil, errors.New("No escaped character")
				}
				break read
			}
			c := src[pos]
			pos++
			switch state {
			case ' ':
				switch {
				case strings.ContainsRune(whitespace, c):
					if token.Len() > 0 || quoted {
						break read
					}
				case comments && c == '#':
					skipLine()
				case c == '\\':
					escapedState, state = 'a', '\\'
				case c == '\'' || c == '"':
					state = c
				default:
					token.WriteRune(c)
					state = 'a'
				}
			case 'a':
				switch {
				case strings.ContainsRune(whitespace, c):
					state = ' '
					if token.Len() > 0 || quoted {
						break read
					}
				case comments && c == '#':
					skipLine()
					state = ' '
					if token.Len() > 0 || quoted {
						break read
					}
				case c == '\'' || c == '"':
					state = c
				case c == '\\':
					escapedState, state = 'a', '\\'
				default:
					token.WriteRune(c)
				}
			case '\'', '"':
				quoted = true
				switch {
				case c == state:
					state = 'a'
				case c == '\\' && state == '"':
					escapedState, state = state, '\\'
				default:
					token.WriteRune(c)
				}
			case '\\':
				if (escapedState == '\'' || escapedState == '"') && c != state && c != escapedState {
					token.WriteRune(state)
				}
				token.WriteRune(c)
				state = escapedState
			}
		}
		if token.Len() == 0 && !quoted {
			return out, nil
		}
		out = append(out, token.String())
	}
}
