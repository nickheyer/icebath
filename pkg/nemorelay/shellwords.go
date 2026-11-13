package nemorelay

import (
	"errors"
	"strings"
)

var errShellWordsQuote = errors.New("missing closing quote")

type shellWordsState int

const (
	shellDelimiter shellWordsState = iota
	shellBackslash
	shellUnquoted
	shellUnquotedBackslash
	shellSingleQuoted
	shellDoubleQuoted
	shellDoubleQuotedBackslash
	shellComment
)

// Splits a command into words with POSIX shell quoting, escapes and comments
func splitShellWords(input string) ([]string, error) {
	words := make([]string, 0)
	var word strings.Builder
	state := shellDelimiter
	runes := []rune(input)
	for index := 0; ; index++ {
		end := index >= len(runes)
		var c rune
		if !end {
			c = runes[index]
		}
		switch state {
		case shellDelimiter:
			switch {
			case end:
				return words, nil
			case c == '\'':
				state = shellSingleQuoted
			case c == '"':
				state = shellDoubleQuoted
			case c == '\\':
				state = shellBackslash
			case c == '\t' || c == ' ' || c == '\n':
			case c == '#':
				state = shellComment
			default:
				word.WriteRune(c)
				state = shellUnquoted
			}
		case shellBackslash:
			switch {
			case end:
				word.WriteRune('\\')
				return append(words, word.String()), nil
			case c == '\n':
				state = shellDelimiter
			default:
				word.WriteRune(c)
				state = shellUnquoted
			}
		case shellUnquoted:
			switch {
			case end:
				return append(words, word.String()), nil
			case c == '\'':
				state = shellSingleQuoted
			case c == '"':
				state = shellDoubleQuoted
			case c == '\\':
				state = shellUnquotedBackslash
			case c == '\t' || c == ' ' || c == '\n':
				words = append(words, word.String())
				word.Reset()
				state = shellDelimiter
			default:
				word.WriteRune(c)
			}
		case shellUnquotedBackslash:
			switch {
			case end:
				word.WriteRune('\\')
				return append(words, word.String()), nil
			case c == '\n':
				state = shellUnquoted
			default:
				word.WriteRune(c)
				state = shellUnquoted
			}
		case shellSingleQuoted:
			switch {
			case end:
				return nil, errShellWordsQuote
			case c == '\'':
				state = shellUnquoted
			default:
				word.WriteRune(c)
			}
		case shellDoubleQuoted:
			switch {
			case end:
				return nil, errShellWordsQuote
			case c == '"':
				state = shellUnquoted
			case c == '\\':
				state = shellDoubleQuotedBackslash
			default:
				word.WriteRune(c)
			}
		case shellDoubleQuotedBackslash:
			switch {
			case end:
				return nil, errShellWordsQuote
			case c == '\n':
				state = shellDoubleQuoted
			case c == '$' || c == '`' || c == '"' || c == '\\':
				word.WriteRune(c)
				state = shellDoubleQuoted
			default:
				word.WriteRune('\\')
				word.WriteRune(c)
				state = shellDoubleQuoted
			}
		case shellComment:
			switch {
			case end:
				return words, nil
			case c == '\n':
				state = shellDelimiter
			}
		}
	}
}
