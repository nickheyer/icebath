package tools

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pylaunch"
)

// Embedded interpreter an existing launcher starts, false when the layout is not one icebath or hermes publishes
func launcherPython(target string) (string, bool) {
	data, err := os.ReadFile(target)
	if err != nil {
		return "", false
	}
	suffix := strings.ToLower(pathlibSuffix(target))
	if suffix == ".exe" {
		return pylaunch.ExeInterpreter(data)
	}
	text, ok := decodeUTF8Sig(data)
	if !ok {
		return "", false
	}
	lines := pySplitLines(text)
	if suffix == ".cmd" {
		for _, entry := range lines[min(1, len(lines)):] {
			if strings.TrimFunc(entry, pyIsSpace) != "" {
				return launcherFirstToken(entry)
			}
		}
		return "", false
	}
	for _, entry := range lines {
		if strings.HasPrefix(entry, "exec ") {
			token, present, err := shlexPosixFirstToken(strings.TrimPrefix(entry, "exec "))
			if err != nil || !present || token == "" {
				return "", false
			}
			return token, true
		}
	}
	return "", false
}

// Strict UTF-8 text with one leading byte order mark dropped, false when the bytes are not UTF-8
func decodeUTF8Sig(data []byte) (string, bool) {
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	if !utf8.Valid(data) {
		return "", false
	}
	return string(data), true
}

// First whitespace delimited token of a line, honouring one quote pair, false when the line is blank or unclosed
func launcherFirstToken(line string) (string, bool) {
	text := strings.TrimFunc(line, pyIsSpace)
	if text == "" {
		return "", false
	}
	if text[0] == '"' || text[0] == '\'' {
		end := strings.IndexByte(text[1:], text[0])
		if end == -1 {
			return "", false
		}
		return text[1 : end+1], true
	}
	return strings.FieldsFunc(text, pyIsSpace)[0], true
}

// Error shlex raises when input ends right after an escape character
var errShlexNoEscapedCharacter = errors.New("No escaped character")

// First token of a POSIX whitespace split shlex, false at end of input, an error on unclosed quotes or escapes
func shlexPosixFirstToken(s string) (string, bool, error) {
	const whitespace, quotes = " \t\r\n", `'"`
	runes := []rune(s)
	var token strings.Builder
	state, escaped := ' ', ' '
	quoted := false
	pos := 0
	next := func() (rune, bool) {
		if pos >= len(runes) {
			return 0, false
		}
		r := runes[pos]
		pos++
		return r, true
	}
	skipLine := func() {
		for pos < len(runes) {
			r := runes[pos]
			pos++
			if r == '\n' {
				return
			}
		}
	}
	for {
		c, ok := next()
		switch {
		case state == ' ':
			switch {
			case !ok:
				return shlexResult(token.String(), quoted)
			case strings.ContainsRune(whitespace, c):
				if token.Len() > 0 || quoted {
					return shlexResult(token.String(), quoted)
				}
			case c == '#':
				skipLine()
			case c == '\\':
				escaped, state = 'a', '\\'
			case strings.ContainsRune(quotes, c):
				state = c
			default:
				token.WriteRune(c)
				state = 'a'
			}
		case state == '\'' || state == '"':
			quoted = true
			switch {
			case !ok:
				return "", false, errUnbalancedQuotes
			case c == state:
				state = 'a'
			case c == '\\' && state == '"':
				escaped, state = state, '\\'
			default:
				token.WriteRune(c)
			}
		case state == '\\':
			if !ok {
				return "", false, errShlexNoEscapedCharacter
			}
			if (escaped == '\'' || escaped == '"') && c != state && c != escaped {
				token.WriteRune(state)
			}
			token.WriteRune(c)
			state = escaped
		case state == 'a':
			switch {
			case !ok:
				return shlexResult(token.String(), quoted)
			case strings.ContainsRune(whitespace, c):
				state = ' '
				if token.Len() > 0 || quoted {
					return shlexResult(token.String(), quoted)
				}
			case c == '#':
				skipLine()
				state = ' '
				if token.Len() > 0 || quoted {
					return shlexResult(token.String(), quoted)
				}
			case strings.ContainsRune(quotes, c):
				state = c
			case c == '\\':
				escaped, state = 'a', '\\'
			default:
				token.WriteRune(c)
			}
		}
	}
}

// Token shlex returns, an unquoted empty token meaning the end of input
func shlexResult(token string, quoted bool) (string, bool, error) {
	if !quoted && token == "" {
		return "", false, nil
	}
	return token, true, nil
}

// Last suffix of the final path component the way pathlib reports it, leading dots ignored
func pathlibSuffix(p string) string {
	name := strings.TrimLeft(pathlibName(p), ".")
	if i := strings.LastIndexByte(name, '.'); i != -1 {
		return name[i:]
	}
	return ""
}
