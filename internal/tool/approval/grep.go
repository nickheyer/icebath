package approval

import (
	"strings"
)

type spanToken struct {
	value      string
	start, end int
	inert      bool
}

func backtickEndFrom(segment string, i int) bool {
	j := strings.IndexByte(segment[i+1:], '`')
	if j >= 0 {
		j += i + 1
	}
	for j > 0 && segment[j-1] == '\\' {
		k := strings.IndexByte(segment[j+1:], '`')
		if k < 0 {
			return false
		}
		j += 1 + k
	}
	return j >= 0
}

func tokensWithSpans(segment string, start int) ([]spanToken, bool) {
	var tokens []spanToken
	var value strings.Builder
	tokenStart := -1
	var quote byte
	depth := 0
	inBacktick := false
	flush := func(end int) {
		raw := segment[tokenStart:end]
		inert := (strings.HasPrefix(raw, "'") && strings.HasSuffix(raw, "'")) || (strings.Contains(raw, "='") && strings.HasSuffix(raw, "'"))
		tokens = append(tokens, spanToken{value.String(), tokenStart, end, inert})
	}
	endAt := len(segment)
	stopped := false
	for st := range scanShell(segment, start, len(segment), scanMode{}) {
		ch := segment[st.i]
		if st.kind == stepChar && quote == 0 {
			if spaceAt(segment, st.i) && ch != '\n' {
				if tokenStart >= 0 {
					flush(st.i)
					value.Reset()
					tokenStart = -1
				}
				continue
			}
			switch {
			case strings.HasPrefix(segment[st.i:], "$("):
				depth++
			case ch == '`':
				switch {
				case inBacktick:
					inBacktick = false
				case depth == 0 && !backtickEndFrom(segment, st.i):
					endAt, stopped = st.i, true
				default:
					inBacktick = true
				}
			case ch == ')':
				if depth == 0 {
					endAt, stopped = st.i, true
				} else {
					depth--
				}
			case ch == ';' || ch == '|' || ch == '&' || ch == '\n':
				endAt, stopped = st.i, true
			}
			if stopped {
				break
			}
		}
		if tokenStart < 0 {
			tokenStart = st.i
		}
		switch {
		case st.kind == stepQuote:
			if quote != 0 {
				quote = 0
			} else {
				quote = ch
			}
		case st.kind == stepEsc:
			value.WriteString(segment[st.i+1 : st.j])
		case ch == '\\' && quote == 0:
			return nil, false
		default:
			value.WriteString(segment[st.i : st.i+runeWidth(segment, st.i)])
		}
	}
	if quote != 0 {
		return nil, false
	}
	if tokenStart >= 0 {
		flush(endAt)
	}
	return tokens, true
}

var (
	grepOptionsWithArg = set("--after-context", "--before-context", "--binary-files", "--context", "--directories", "--devices",
		"--exclude", "--exclude-dir", "--exclude-from", "--include", "--label", "--max-count", "--regexp", "--file")
	grepShortWithArg = "ABCDdefm"
)

type textSpan struct{ start, end int }

func quotedGrepSpans(command string) ([]textSpan, bool) {
	var spans []textSpan
	offset := 0
	for _, segment := range topSegments(command) {
		at := strings.Index(command[offset:], segment)
		if at >= 0 {
			at += offset
		}
		offset = at + len(segment)
		for _, sp := range wordSpans(segment) {
			switch pyLower(pyBasename(deobfuscateWord(sp.word))) {
			case "grep", "egrep":
			default:
				continue
			}
			tokens, ok := tokensWithSpans(segment, sp.start)
			if !ok {
				return nil, true
			}
			args := tokens[1:]
			var patterns []int
			pcre, explicit := false, false
			operand, options := -1, true
			for i := 0; i < len(args); i++ {
				token := args[i].value
				switch {
				case options && token == "--":
					options = false
				case options && strings.HasPrefix(token, "--"):
					option, _, equals := strings.Cut(token, "=")
					pcre = pcre || option == "--perl-regexp"
					explicit = explicit || option == "--regexp" || option == "--file"
					takesNext := grepOptionsWithArg[option] && !equals
					if takesNext && i+1 >= len(args) {
						return nil, true
					}
					if option == "--regexp" {
						if takesNext {
							patterns = append(patterns, i+1)
						} else {
							patterns = append(patterns, i)
						}
					}
					if takesNext {
						i++
					}
				case options && strings.HasPrefix(token, "-") && token != "-":
					chars := []rune(token[1:])
					for j, char := range chars {
						pcre = pcre || char == 'P'
						explicit = explicit || char == 'e' || char == 'f'
						if strings.ContainsRune(grepShortWithArg, char) {
							attached := j+1 < len(chars)
							if !attached && i+1 >= len(args) {
								return nil, true
							}
							if char == 'e' {
								if attached {
									patterns = append(patterns, i)
								} else {
									patterns = append(patterns, i+1)
								}
							}
							if !attached {
								i++
							}
							break
						}
					}
				case operand < 0:
					operand = i
				}
			}
			if !explicit {
				if operand < 0 {
					return nil, pcre
				}
				patterns = append(patterns, operand)
			}
			if pcre {
				for _, k := range patterns {
					if args[k].inert {
						spans = append(spans, textSpan{at + args[k].start, at + args[k].end})
					}
				}
			}
		}
	}
	return spans, false
}

func grepSafe(command string) (string, bool) {
	spans, malformed := quotedGrepSpans(command)
	if malformed || len(spans) == 0 {
		return command, malformed
	}
	edits := make([]edit, len(spans))
	for k, sp := range spans {
		edits[k] = edit{sp.start, sp.end, blank(pySlice(command, sp.start, sp.end))}
	}
	return splice(command, edits), false
}
