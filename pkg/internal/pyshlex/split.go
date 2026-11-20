package pyshlex

import (
	"errors"
	"strings"
)

// Error of shlex.split for an unterminated quoted token
var ErrNoClosingQuotation = errors.New("No closing quotation")

// Tokens of shlex.split(s, posix=False), quotes kept inside tokens and closing a token right after them
func SplitNonPOSIX(s string) ([]string, error) {
	const whitespace = " \t\r\n"
	var tokens []string
	var token strings.Builder
	state := ' '
	for _, ch := range s {
		switch {
		case state == ' ':
			switch {
			case strings.ContainsRune(whitespace, ch):
			case ch == '\'' || ch == '"':
				token.WriteRune(ch)
				state = ch
			default:
				token.WriteRune(ch)
				state = 'a'
			}
		case state == '\'' || state == '"':
			token.WriteRune(ch)
			if ch == state {
				tokens = append(tokens, token.String())
				token.Reset()
				state = ' '
			}
		default:
			if strings.ContainsRune(whitespace, ch) {
				tokens = append(tokens, token.String())
				token.Reset()
				state = ' '
			} else {
				token.WriteRune(ch)
			}
		}
	}
	switch state {
	case '\'', '"':
		return nil, ErrNoClosingQuotation
	case 'a':
		tokens = append(tokens, token.String())
	}
	return tokens, nil
}
