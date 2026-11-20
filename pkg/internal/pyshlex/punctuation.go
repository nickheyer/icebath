package pyshlex

import (
	"errors"
	"strings"
)

const (
	punctuationWhitespace = " \t\r\n"
	punctuationChars      = "();<>|&"
)

// Tokens of a posix shlex.shlex with punctuation_chars and whitespace_split set, hash comments honoured
func SplitPOSIXPunctuation(s string) ([]string, error) {
	l := &punctuationLexer{src: []rune(s), state: ' '}
	var out []string
	for {
		tok, ok, err := l.readToken()
		if err != nil {
			return nil, err
		}
		if !ok {
			return out, nil
		}
		out = append(out, tok)
	}
}

type punctuationLexer struct {
	src      []rune
	pos      int
	pushback []rune
	state    rune
}

// Next character, a pushed back one first, false at end of input
func (l *punctuationLexer) read() (rune, bool) {
	if n := len(l.pushback); n > 0 {
		c := l.pushback[n-1]
		l.pushback = l.pushback[:n-1]
		return c, true
	}
	if l.pos >= len(l.src) {
		return 0, false
	}
	c := l.src[l.pos]
	l.pos++
	return c, true
}

// Consumes the rest of the current line through its newline
func (l *punctuationLexer) readline() {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		l.pos++
		if c == '\n' {
			return
		}
	}
}

// One token of read_token, false once the input is exhausted with nothing quoted pending
func (l *punctuationLexer) readToken() (string, bool, error) {
	var token strings.Builder
	quoted := false
	escaped := ' '
	isQuote := func(c rune) bool { return c == '\'' || c == '"' }
loop:
	for {
		c, more := l.read()
		switch {
		case l.state == 0:
			token.Reset()
			break loop
		case l.state == ' ':
			switch {
			case !more:
				l.state = 0
				break loop
			case strings.ContainsRune(punctuationWhitespace, c):
				if token.Len() > 0 || quoted {
					break loop
				}
			case c == '#':
				l.readline()
			case c == '\\':
				escaped, l.state = 'a', '\\'
			case strings.ContainsRune(punctuationChars, c):
				token.Reset()
				token.WriteRune(c)
				l.state = 'c'
			case isQuote(c):
				l.state = c
			default:
				token.Reset()
				token.WriteRune(c)
				l.state = 'a'
			}
		case isQuote(l.state):
			quoted = true
			switch {
			case !more:
				return "", false, errors.New("No closing quotation")
			case c == l.state:
				l.state = 'a'
			case c == '\\' && l.state == '"':
				escaped, l.state = l.state, '\\'
			default:
				token.WriteRune(c)
			}
		case l.state == '\\':
			if !more {
				return "", false, errors.New("No escaped character")
			}
			if isQuote(escaped) && c != l.state && c != escaped {
				token.WriteRune(l.state)
			}
			token.WriteRune(c)
			l.state = escaped
		default:
			switch {
			case !more:
				l.state = 0
				break loop
			case strings.ContainsRune(punctuationWhitespace, c):
				l.state = ' '
				if token.Len() > 0 || quoted {
					break loop
				}
			case c == '#':
				l.readline()
				l.state = ' '
				if token.Len() > 0 || quoted {
					break loop
				}
			case l.state == 'c':
				if strings.ContainsRune(punctuationChars, c) {
					token.WriteRune(c)
					continue
				}
				l.pushback = append(l.pushback, c)
				l.state = ' '
				break loop
			case isQuote(c):
				l.state = c
			case c == '\\':
				escaped, l.state = 'a', '\\'
			case !strings.ContainsRune(punctuationChars, c):
				token.WriteRune(c)
			default:
				l.pushback = append(l.pushback, c)
				l.state = ' '
				if token.Len() > 0 || quoted {
					break loop
				}
			}
		}
	}
	result := token.String()
	if !quoted && result == "" {
		return "", false, nil
	}
	return result, true, nil
}
