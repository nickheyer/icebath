// Shell word splitting and quoting for POSIX sh, cmd.exe, PowerShell and the Windows CreateProcess command line
package shell

import (
	"errors"
	"strings"
)

var (
	// A quoted word reached the end of the input without its closing quote
	ErrUnclosedQuote = errors.New("No closing quotation")
	// A backslash ended the input with nothing left to escape
	ErrTrailingEscape = errors.New("No escaped character")
)

// Control operators a POSIX shell command line separates into words of their own
const ShellOperators = "();<>|&"

// Word rules of a split, the zero value being plain POSIX shell words without comments
type Lexer struct {
	// A # outside quotes starts a comment that runs through the end of its line
	Comments bool
	// Characters that group into words of their own, such as ShellOperators
	Operators string
	// Quotes stay inside the words and backslash is an ordinary character
	Literal bool
}

// POSIX shell words of s with quotes removed and backslash escapes applied
func Split(s string) ([]string, error) { return Lexer{}.Split(s) }

// Words of s under the rules of l
func (l Lexer) Split(s string) ([]string, error) {
	sc := scanner{src: s, lex: l, state: stateSpace}
	var out []string
	for {
		word, ok, err := sc.next()
		if err != nil {
			return nil, err
		}
		if !ok {
			return out, nil
		}
		out = append(out, word)
	}
}

// Only the first word of s, a malformed tail after it left unread
func (l Lexer) First(s string) (string, bool, error) {
	sc := scanner{src: s, lex: l, state: stateSpace}
	return sc.next()
}

// Scanner states, a quote character standing for its own quoted state
const (
	stateEOF    = 0
	stateSpace  = ' '
	stateWord   = 'a'
	stateOp     = 'c'
	stateEscape = '\\'
)

// Incremental word reader holding the position and state between words
type scanner struct {
	src   string
	pos   int
	lex   Lexer
	state byte
	back  []byte
}

// Next input byte, a pushed back operator boundary first
func (s *scanner) read() (byte, bool) {
	if n := len(s.back); n > 0 {
		c := s.back[n-1]
		s.back = s.back[:n-1]
		return c, true
	}
	if s.pos >= len(s.src) {
		return 0, false
	}
	c := s.src[s.pos]
	s.pos++
	return c, true
}

// Consumes the rest of the current line through its newline
func (s *scanner) skipLine() {
	for s.pos < len(s.src) {
		c := s.src[s.pos]
		s.pos++
		if c == '\n' {
			return
		}
	}
}

func isBlank(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

func isQuote(c byte) bool { return c == '\'' || c == '"' }

func (s *scanner) isOp(c byte) bool { return strings.IndexByte(s.lex.Operators, c) >= 0 }

// Reads one word, false once the input is exhausted with nothing quoted pending
func (s *scanner) next() (string, bool, error) {
	var word strings.Builder
	posix := !s.lex.Literal
	quoted := false
	escaped := byte(stateSpace)
	emit := func() bool { return word.Len() > 0 || (posix && quoted) }
loop:
	for {
		c, more := s.read()
		switch s.state {
		case stateEOF:
			word.Reset()
			break loop
		case stateSpace:
			switch {
			case !more:
				s.state = stateEOF
				break loop
			case isBlank(c):
				if emit() {
					break loop
				}
			case s.lex.Comments && c == '#':
				s.skipLine()
			case posix && c == '\\':
				escaped, s.state = stateWord, stateEscape
			case s.isOp(c):
				word.Reset()
				word.WriteByte(c)
				s.state = stateOp
			case isQuote(c):
				if !posix {
					word.Reset()
					word.WriteByte(c)
				}
				s.state = c
			default:
				word.Reset()
				word.WriteByte(c)
				s.state = stateWord
			}
		case '\'', '"':
			quoted = true
			switch {
			case !more:
				return "", false, ErrUnclosedQuote
			case c == s.state && !posix:
				word.WriteByte(c)
				s.state = stateSpace
				break loop
			case c == s.state:
				s.state = stateWord
			case posix && c == '\\' && s.state == '"':
				escaped, s.state = s.state, stateEscape
			default:
				word.WriteByte(c)
			}
		case stateEscape:
			if !more {
				return "", false, ErrTrailingEscape
			}
			if isQuote(escaped) && c != s.state && c != escaped {
				word.WriteByte(s.state)
			}
			word.WriteByte(c)
			s.state = escaped
		default:
			switch {
			case !more:
				s.state = stateEOF
				break loop
			case isBlank(c):
				s.state = stateSpace
				if emit() {
					break loop
				}
			case s.lex.Comments && c == '#':
				s.skipLine()
				if posix {
					s.state = stateSpace
					if emit() {
						break loop
					}
				}
			case s.state == stateOp:
				if s.isOp(c) {
					word.WriteByte(c)
					continue
				}
				s.back = append(s.back, c)
				s.state = stateSpace
				break loop
			case posix && isQuote(c):
				s.state = c
			case posix && c == '\\':
				escaped, s.state = stateWord, stateEscape
			case !s.isOp(c):
				word.WriteByte(c)
			default:
				s.back = append(s.back, c)
				s.state = stateSpace
				if emit() {
					break loop
				}
			}
		}
	}
	result := word.String()
	if result == "" && !(posix && quoted) {
		return "", false, nil
	}
	return result, true, nil
}
