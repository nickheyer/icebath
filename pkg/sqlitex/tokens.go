package sqlitex

import "strings"

// Token of SQL text at its place in the text
type token struct {
	text       string
	kind       byte
	start, end int
}

// Kinds of token: a word, number or operator; a string literal; a quoted name; one of the marks ( ) , ;
const (
	word    = 'w'
	literal = 's'
	quoted  = 'q'
	mark    = 'p'
)

// Statement as any spelling of it writes it: its comments and spacing dropped, names and keywords in lower case and unquoted where they
// need no quotes, string literals as they are, one space between tokens
func Normalize(statement string) string {
	tokens := tokenize(statement)
	for len(tokens) > 0 && tokens[len(tokens)-1].text == ";" {
		tokens = tokens[:len(tokens)-1]
	}
	return joined(tokens)
}

// Name quoted for a statement
func Quote(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func joined(tokens []token) string {
	words := make([]string, len(tokens))
	for i, t := range tokens {
		words[i] = t.norm()
	}
	return strings.Join(words, " ")
}

// Tokens of SQL text, its spaces and comments left out
func tokenize(text string) []token {
	var out []token
	for i := 0; i < len(text); {
		c := text[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
		case c == '-' && i+1 < len(text) && text[i+1] == '-':
			if end := strings.IndexByte(text[i:], '\n'); end >= 0 {
				i += end + 1
			} else {
				i = len(text)
			}
		case c == '/' && i+1 < len(text) && text[i+1] == '*':
			if end := strings.Index(text[i+2:], "*/"); end >= 0 {
				i += end + 4
			} else {
				i = len(text)
			}
		case c == '\'' || c == '"' || c == '`' || c == '[':
			end := closing(text, i)
			kind := byte(quoted)
			if c == '\'' {
				kind = literal
			}
			out = append(out, token{text: text[i:end], kind: kind, start: i, end: end})
			i = end
		case c == '(' || c == ')' || c == ',' || c == ';':
			out = append(out, token{text: text[i : i+1], kind: mark, start: i, end: i + 1})
			i++
		case nameByte(c):
			end := i
			for end < len(text) && nameByte(text[end]) {
				end++
			}
			out = append(out, token{text: text[i:end], kind: word, start: i, end: end})
			i = end
		default:
			out = append(out, token{text: text[i : i+1], kind: word, start: i, end: i + 1})
			i++
		}
	}
	return out
}

// End of the quoted run that opens at i: past its closing quote, a doubled quote standing for one inside it
func closing(text string, i int) int {
	quote := text[i]
	if quote == '[' {
		quote = ']'
	}
	for j := i + 1; j < len(text); j++ {
		if text[j] != quote {
			continue
		}
		if quote != ']' && j+1 < len(text) && text[j+1] == quote {
			j++
			continue
		}
		return j + 1
	}
	return len(text)
}

// Whether the byte can be part of a bare name or number, every byte of a character past ASCII included
func nameByte(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// Token as any spelling of it writes it
func (t token) norm() string {
	switch t.kind {
	case literal:
		return t.text
	case quoted:
		name := strings.ToLower(unquote(t.text))
		if bare(name) {
			return name
		}
		return Quote(name)
	}
	return strings.ToLower(t.text)
}

// Name a token spells, its quotes taken off
func unquote(text string) string {
	if len(text) < 2 {
		return text
	}
	switch first, last := text[0], text[len(text)-1]; {
	case first == '"' && last == '"', first == '`' && last == '`':
		return strings.ReplaceAll(text[1:len(text)-1], string([]byte{first, first}), string(first))
	case first == '[' && last == ']':
		return text[1 : len(text)-1]
	}
	return text
}

// Whether the name needs no quotes: a letter or underscore, then letters, digits and underscores
func bare(name string) bool {
	if name == "" || name[0] >= '0' && name[0] <= '9' {
		return false
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; c != '_' && (c < '0' || c > '9') && (c < 'a' || c > 'z') {
			return false
		}
	}
	return true
}
