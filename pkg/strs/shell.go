package strs

import (
	"errors"
	"strings"
)

// Words of a command line split as a POSIX shell splits them, honoring quotes and backslashes: a backslash takes the next character as it
// is, inside double quotes only a $, a backquote, a double quote or another backslash, any other keeping its backslash, and a backslash
// before the end of a line joins the line to the next; an error, said for a person, when a quote or a backslash at the end is left open
func Shell(line string) ([]string, error) {
	var words []string
	var cur strings.Builder
	// Whether a word is begun, whether a backslash came last, and whether a word was begun before that backslash
	in, esc, begun := false, false, false
	quote := rune(0)
	for _, r := range line {
		switch {
		case esc:
			esc = false
			switch {
			case r == '\n':
				in = begun
			case quote == '"' && !strings.ContainsRune("$`\"\\", r):
				cur.WriteRune('\\')
				cur.WriteRune(r)
			default:
				cur.WriteRune(r)
			}
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch r {
			case '"':
				quote = 0
			case '\\':
				esc, begun = true, true
			default:
				cur.WriteRune(r)
			}
		case r == '\\':
			esc, in, begun = true, true, in
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if in {
				words = append(words, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	switch {
	case quote != 0:
		return nil, errors.New("A quote " + string(quote) + " is opened and never closed.")
	case esc:
		return nil, errors.New("The line ends with a backslash.")
	case in:
		words = append(words, cur.String())
	}
	return words, nil
}

// A command line as a shell would read the words back: a word with a space, a quote or nothing in it quoted
func Quote(words []string) string {
	out := make([]string, len(words))
	for i, word := range words {
		if word != "" && !strings.ContainsAny(word, " \t\n'\"\\$`") {
			out[i] = word
			continue
		}
		out[i] = "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
	}
	return strings.Join(out, " ")
}
