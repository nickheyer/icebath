package shellutil

import (
	"errors"
	"runtime"
	"strings"
)

// Error of the splitters for a quote that is never closed
var ErrUnterminatedQuote = errors.New("unterminated quote")

// Error of the POSIX splitters for a backslash ending the input with nothing left to escape
var ErrTrailingBackslash = errors.New("trailing backslash")

// Words of s split the way a POSIX shell splits them: blanks separate words, single quotes keep everything, double quotes keep everything but the backslash escapes of $ ` " \ and newline, a backslash outside quotes keeps the next character, quotes are removed
func Split(s string) ([]string, error) { return split(s, 0) }

// Words of s split as Split does, a # where a word would start beginning a comment that runs to the end of its line
func SplitComments(s string) ([]string, error) { return split(s, comments) }

// Words of s split at blanks with single or double quotes grouping and removed, every backslash kept as typed the way Windows command lines and paths use them
func SplitRaw(s string) ([]string, error) { return split(s, literal) }

// Words and shell operators of s split as SplitComments does, unquoted ( ) ; < > | & cut into the longest operators such as && || ;; >> |& and given as tokens of their own
func SplitOperators(s string) ([]string, error) { return split(s, comments|operators) }

// Words of a command line written for this host: Split on POSIX systems, SplitRaw on Windows where backslashes separate path elements
func SplitCommandLine(s string) ([]string, error) {
	if runtime.GOOS == "windows" {
		return SplitRaw(s)
	}
	return Split(s)
}

// Splitting rules beyond plain POSIX words: comments, backslashes taken literally, operator tokens
type mode uint8

const (
	comments mode = 1 << iota
	literal
	operators
)

// Operators longer than one character, longest first so the scan takes the longest match
var longOperators = []string{"<<-", "<<<", ";;&", "&>>", "&&", "||", ";;", ";&", "|&", "<<", ">>", "<&", ">&", "<>", ">|", "&>"}

type scanner struct {
	s   string
	i   int
	m   mode
	buf []byte
}

func split(s string, m mode) ([]string, error) {
	sc := scanner{s: s, m: m}
	var words []string
	for {
		for sc.i < len(s) && isBlank(s[sc.i]) {
			sc.i++
		}
		if sc.i == len(s) {
			return words, nil
		}
		c := s[sc.i]
		if c == '#' && m&comments != 0 {
			if nl := strings.IndexByte(s[sc.i:], '\n'); nl >= 0 {
				sc.i += nl + 1
			} else {
				sc.i = len(s)
			}
			continue
		}
		if m&operators != 0 && isOperator(c) {
			words = append(words, sc.operator())
			continue
		}
		word, ok, err := sc.word()
		if err != nil {
			return nil, err
		}
		if ok {
			words = append(words, word)
		}
	}
}

// Longest operator at the scan position
func (sc *scanner) operator() string {
	rest := sc.s[sc.i:]
	n := 1
	for _, op := range longOperators {
		if strings.HasPrefix(rest, op) {
			n = len(op)
			break
		}
	}
	sc.i += n
	return rest[:n]
}

// Word at the scan position, a slice of the input when it holds no quote or escape; false when it held nothing, not even empty quotes
func (sc *scanner) word() (string, bool, error) {
	s, start, i := sc.s, sc.i, sc.i
	b := sc.buf[:0]
	copied, quoted := false, false
	for i < len(s) {
		c := s[i]
		if isBlank(c) || sc.m&operators != 0 && isOperator(c) {
			break
		}
		switch {
		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return "", false, ErrUnterminatedQuote
			}
			if !copied {
				b, copied = append(b, s[start:i]...), true
			}
			b = append(b, s[i+1:i+1+end]...)
			i += end + 2
			quoted = true
		case c == '"':
			if !copied {
				b, copied = append(b, s[start:i]...), true
			}
			var err error
			if b, i, err = sc.doubleQuoted(b, i+1); err != nil {
				return "", false, err
			}
			quoted = true
		case c == '\\' && sc.m&literal == 0:
			if i+1 == len(s) {
				return "", false, ErrTrailingBackslash
			}
			if !copied {
				b, copied = append(b, s[start:i]...), true
			}
			if s[i+1] != '\n' {
				b = append(b, s[i+1])
			}
			i += 2
		default:
			if copied {
				b = append(b, c)
			}
			i++
		}
	}
	sc.i, sc.buf = i, b
	if !copied {
		return s[start:i], i > start, nil
	}
	return string(b), len(b) > 0 || quoted, nil
}

// Appends the inside of a double-quoted run that starts at i, after its opening quote, and gives the position after the closing quote
func (sc *scanner) doubleQuoted(b []byte, i int) ([]byte, int, error) {
	s := sc.s
	escapes := sc.m&literal == 0
	for {
		j := i
		for j < len(s) && s[j] != '"' && (s[j] != '\\' || !escapes) {
			j++
		}
		b = append(b, s[i:j]...)
		if j == len(s) || s[j] == '\\' && j+1 == len(s) {
			return b, len(s), ErrUnterminatedQuote
		}
		if s[j] == '"' {
			return b, j + 1, nil
		}
		switch next := s[j+1]; next {
		case '$', '`', '"', '\\':
			b = append(b, next)
		case '\n':
		default:
			b = append(b, '\\', next)
		}
		i = j + 2
	}
}

func isBlank(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func isOperator(c byte) bool {
	switch c {
	case '(', ')', ';', '<', '>', '|', '&':
		return true
	}
	return false
}
