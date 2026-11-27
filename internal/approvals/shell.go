package approvals

import (
	"errors"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Shell text is read here as it is written, never run: quotes, escapes, substitutions and comments are told apart so the rules see the
// words a shell would run

func runeAt(s string, i int) (rune, int) {
	r, size := utf8.DecodeRuneInString(s[i:])
	if size == 0 {
		return 0, 1
	}
	return r, size
}

func runeBefore(s string, i int) rune {
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return r
}

// Everything after the last slash
func base(p string) string {
	return p[strings.LastIndex(p, "/")+1:]
}

func spaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}

// Kinds of step a scan takes
const (
	stepChar = iota
	stepEscape
	stepQuote
	stepSubst
	stepComment
)

type step struct {
	kind  int
	i, j  int
	quote byte
}

type scanOptions struct {
	// Where substitutions are stepped over whole: "u" outside quotes, "q" inside double quotes
	subst string
	// ${...} counts as a substitution outside quotes
	brace bool
	// An unclosed substitution ends the scan with a step whose end is -1
	stopUnclosed bool
	// A backtick closes at the next backtick, escapes and all
	naiveBacktick bool
	comments      bool
}

// Lexical walk over text without expanding anything: the quote and escape state every reader shares
type scanner struct {
	text  string
	n, i  int
	quote byte
	o     scanOptions
	done  bool
}

func scan(text string, start, end int, o scanOptions) *scanner {
	if end < 0 {
		end = len(text)
	}
	return &scanner{text: text, n: end, i: start, o: o}
}

func (s *scanner) next() (step, bool) {
	if s.done || s.i >= s.n {
		return step{}, false
	}
	text, o, i := s.text, s.o, s.i
	ch, size := runeAt(text, i)
	kind, j := stepChar, i+size
	wanted := "u"
	if s.quote != 0 {
		wanted = "q"
	}
	switch {
	case o.comments && s.quote == 0 && commentAt(text, i):
		kind = stepComment
		if k := strings.Index(text[i:s.n], "\n"); k >= 0 {
			j = i + k
		} else {
			j = s.n
		}
	case s.quote != '\'' && ch == '\\' && i+1 < s.n:
		_, size2 := runeAt(text, i+1)
		kind, j = stepEscape, i+1+size2
	case s.quote != 0 && ch == rune(s.quote) || s.quote == 0 && (ch == '\'' || ch == '"'):
		kind = stepQuote
	case s.quote != '\'' && strings.Contains(o.subst, wanted) &&
		(ch == '`' || strings.HasPrefix(text[i:], "$(") || o.brace && s.quote == 0 && strings.HasPrefix(text[i:], "${")):
		end := -1
		switch {
		case ch == '`' && o.naiveBacktick:
			if k := strings.Index(text[i+1:], "`"); k >= 0 {
				end = i + 1 + k + 1
			}
		case ch == '`':
			end = backtickEnd(text, i)
		case text[i+1] == '(':
			end = dollarParenEnd(text, i)
		default:
			if k := strings.Index(text[i+2:], "}"); k >= 0 {
				end = i + 2 + k + 1
			}
		}
		if end >= 0 {
			kind, j = stepSubst, end
		} else if o.stopUnclosed {
			s.done = true
			return step{stepSubst, i, -1, s.quote}, true
		}
	}
	st := step{kind, i, j, s.quote}
	if kind == stepQuote {
		if s.quote != 0 {
			s.quote = 0
		} else {
			s.quote = byte(ch)
		}
	}
	s.i = j
	return st, true
}

// Offset after the $( at start is balanced, -1 when it never is
func dollarParenEnd(command string, start int) int {
	depth := 1
	for sc := scan(command, start+2, -1, scanOptions{}); ; {
		st, ok := sc.next()
		if !ok {
			return -1
		}
		if st.kind != stepChar || st.quote != 0 {
			continue
		}
		if strings.HasPrefix(command[st.i:], "$(") {
			depth++
		}
		if command[st.i] == ')' {
			depth--
		}
		if depth == 0 {
			return st.i + 1
		}
	}
}

// Offset after the backtick closing the one at start, only a backslash escaping inside; -1 when none does
func backtickEnd(command string, start int) int {
	for i := start + 1; i < len(command); {
		switch command[i] {
		case '\\':
			if i+1 >= len(command) {
				return -1
			}
			_, size := runeAt(command, i+1)
			i += 1 + size
			continue
		case '`':
			return i + 1
		}
		_, size := runeAt(command, i)
		i += size
	}
	return -1
}

// Index of the backtick closing the one at i, skipping escaped ones, or -1
func backtickFrom(segment string, i int) int {
	j := strings.Index(segment[i+1:], "`")
	if j < 0 {
		return -1
	}
	j += i + 1
	for segment[j-1] == '\\' {
		k := strings.Index(segment[j+1:], "`")
		if k < 0 {
			return -1
		}
		j += k + 1
	}
	return j
}

func skipSpace(command string, pos int) int {
	for pos < len(command) {
		r, size := runeAt(command, pos)
		if !unicode.IsSpace(r) {
			break
		}
		pos += size
	}
	return pos
}

// One shell word from pos, read without running its expansions
func readWord(command string, pos int) (start, end int, word string) {
	start = skipSpace(command, pos)
	end = start
	for sc := scan(command, start, -1, scanOptions{subst: "u", brace: true}); ; {
		st, ok := sc.next()
		if !ok {
			break
		}
		if st.kind == stepChar && st.quote == 0 {
			if r, _ := runeAt(command, st.i); unicode.IsSpace(r) || strings.ContainsRune(";&|<>()", r) {
				break
			}
		}
		end = st.j
	}
	return start, end, command[start:end]
}

var (
	paramReplacement = regexp.MustCompile(`\$\{[^}/\s]+/[^}/]*/(?P<replacement>[^}]*)\}`)
	paramDefault     = regexp.MustCompile(`\$\{[^}:}\s]+:-(?P<default>[^}]*)\}`)
	shellLiteral     = regexp.MustCompile(`^[A-Za-z0-9_./:@%+=,-]+$`)
	assignment       = regexp.MustCompile(`\A[A-Za-z_][A-Za-z0-9_]*=[^\n]*\z`)
	echoFlags        = regexp.MustCompile(`\A-[nEe]+\z`)
)

// What a tiny echo or printf of one literal would print, without running it
func literalOutput(script string) (string, bool) {
	tokens, _ := terminal.Split(script)
	if len(tokens) == 0 {
		return "", false
	}
	command, args := strings.ToLower(tokens[0]), tokens[1:]
	switch command {
	case "echo":
		for len(args) > 0 && echoFlags.MatchString(args[0]) {
			args = args[1:]
		}
	case "printf":
	default:
		return "", false
	}
	if len(args) == 1 && shellLiteral.MatchString(args[0]) {
		return args[0], true
	}
	if command == "printf" && len(args) == 2 && args[0] == "%s" && shellLiteral.MatchString(args[1]) {
		return args[1], true
	}
	return "", false
}

// Word with its literal echo and printf substitutions replaced by what they print
func replaceSubstitutions(word string) string {
	var b strings.Builder
	for i := 0; i < len(word); {
		opener, end := 0, -1
		switch {
		case strings.HasPrefix(word[i:], "$("):
			opener, end = 2, dollarParenEnd(word, i)
		case word[i] == '`':
			opener, end = 1, backtickEnd(word, i)
		}
		replacement, ok := "", false
		if end >= 0 {
			replacement, ok = literalOutput(word[i+opener : end-1])
		}
		if !ok {
			_, size := runeAt(word, i)
			replacement, end = word[i:i+size], i+size
		}
		b.WriteString(replacement)
		i = end
	}
	return b.String()
}

func stripWordSyntax(word string) string {
	var b strings.Builder
	for sc := scan(word, 0, -1, scanOptions{}); ; {
		st, ok := sc.next()
		if !ok {
			return b.String()
		}
		switch st.kind {
		case stepEscape:
			b.WriteString(word[st.i+1 : st.j])
		case stepQuote:
		default:
			b.WriteString(word[st.i:st.j])
		}
	}
}

// Word with its quoting, escaping and literal substitutions collapsed, as the shell would hand it to the command
func plainWord(word string) string {
	for range 2 {
		before := word
		word = replaceSubstitutions(word)
		word = paramReplacement.ReplaceAllString(word, "${replacement}")
		word = stripWordSyntax(paramDefault.ReplaceAllString(word, "${default}"))
		if word == before {
			break
		}
	}
	return word
}

func commentAt(command string, i int) bool {
	if command[i] != '#' {
		return false
	}
	if i == 0 {
		return true
	}
	prev := runeBefore(command, i)
	return unicode.IsSpace(prev) || strings.ContainsRune(";&|()<>", prev)
}

// Words after which another command starts
var transitions = strs.Set("if", "then", "else", "elif", "do", "while", "until", "!")

// Offsets where a command starts, quote aware, inside substitutions, subshells and groups too
func commandStarts(command string) []int {
	starts := []int{0}
	var walk func(start, end int)
	walk = func(start, end int) {
		skip := -1
		for sc := scan(command, start, end, scanOptions{subst: "uq", stopUnclosed: true, comments: true}); ; {
			st, ok := sc.next()
			if !ok {
				return
			}
			switch {
			case st.kind == stepSubst:
				inner := st.i + 2
				if command[st.i] == '`' {
					inner = st.i + 1
				}
				starts = append(starts, inner)
				innerEnd := end
				if st.j >= 0 {
					innerEnd = st.j - 1
				}
				walk(inner, innerEnd)
			case st.kind == stepChar && st.quote == 0 && st.i != skip:
				c := command[st.i]
				switch {
				case strings.IndexByte("(;\n", c) >= 0 ||
					c == '{' && (st.i == 0 || unicode.IsSpace(runeBefore(command, st.i)) || strings.ContainsRune("(;&|)", runeBefore(command, st.i))):
					starts = append(starts, st.i+1)
				case c == '&' || c == '|':
					next := st.i + 1
					if st.i+1 < end && command[st.i+1] == c {
						skip = st.i + 1
						next++
					}
					starts = append(starts, next)
				}
			}
		}
	}
	walk(0, len(command))
	seen := map[int]bool{}
	var out []int
	for i := 0; i < len(starts); i++ {
		start := skipSpace(command, starts[i])
		if start >= len(command) || seen[start] || commentAt(command, start) {
			continue
		}
		seen[start] = true
		out = append(out, start)
		if _, end, word := readWord(command, start); transitions[word] {
			starts = append(starts, end)
		}
	}
	return out
}

type edit struct {
	start, end int
	text       string
}

// Applies sorted edits that don't overlap in one pass
func splice(command string, edits []edit) string {
	var b strings.Builder
	previous := 0
	for _, e := range edits {
		b.WriteString(command[previous:e.start])
		b.WriteString(e.text)
		previous = e.end
	}
	b.WriteString(command[previous:])
	return b.String()
}

// Command with the marker put before every command start, so rules anchored at a line start see group openers
func markStarts(command, marker string) string {
	var edits []edit
	for _, at := range commandStarts(command) {
		if at > 0 {
			edits = append(edits, edit{at, at, marker})
		}
	}
	if len(edits) == 0 {
		return command
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	return splice(command, edits)
}

// Command with raw newlines inside quotes turned into spaces, for detection only
func maskQuotedNewlines(command string) string {
	if !strings.Contains(command, "\n") {
		return command
	}
	return maskNewlinesIn(command, 0, len(command))
}

func maskNewlinesIn(command string, start, end int) string {
	var b strings.Builder
	for sc := scan(command, start, end, scanOptions{subst: "q"}); ; {
		st, ok := sc.next()
		if !ok {
			return b.String()
		}
		switch {
		case st.kind == stepSubst:
			body := st.i + 1
			if strings.HasPrefix(command[st.i:], "$(") {
				body = st.i + 2
			}
			bodyEnd := st.j - 1
			b.WriteString(command[st.i:body])
			b.WriteString(maskNewlinesIn(command, body, bodyEnd))
			b.WriteString(command[bodyEnd:st.j])
		case st.quote != 0 && st.kind == stepChar && command[st.i] == '\n':
			b.WriteByte(' ')
		default:
			b.WriteString(command[st.i:st.j])
		}
	}
}

// End of a redirection operator at pos, -1 when none is there
func redirection(command string, pos int) int {
	k := pos
	for k < len(command) && command[k] >= '0' && command[k] <= '9' {
		k++
	}
	rest := command[k:]
	for _, op := range []string{">>", "<<", "<>", ">&", "<&", ">|"} {
		if strings.HasPrefix(rest, op) {
			return k + 2
		}
	}
	if strings.HasPrefix(rest, "<") || strings.HasPrefix(rest, ">") {
		return k + 1
	}
	return -1
}

type span struct {
	start, end int
	word       string
}

// Wrappers that run the command after them
var wrappers = strs.Set("sudo", "env", "exec", "nohup", "setsid", "time", "command", "builtin", "nice", "timeout", "stdbuf", "ionice", "chrt",
	"taskset", "chroot")

var wrapperArgOptions = map[string]map[string]bool{
	"chroot":  strs.Set("--groups", "--userspec"),
	"sudo":    strs.Set("-c", "--close-from", "-g", "--group", "-h", "--host", "-p", "--prompt", "-u", "--user"),
	"env":     strs.Set("-a", "--argv0", "-C", "--chdir", "-S", "--split-string", "-u", "--unset"),
	"exec":    strs.Set("-a"),
	"nice":    strs.Set("-n", "--adjustment"),
	"time":    strs.Set("-f", "--format", "-o", "--output"),
	"timeout": strs.Set("-k", "--kill-after", "-s", "--signal"),
	"stdbuf":  strs.Set("-e", "--error", "-i", "--input", "-o", "--output"),
	"ionice":  strs.Set("-c", "--class", "-n", "--classdata"),
}

// Wrapper options that query rather than run the command
var wrapperQueries = map[string]map[string]bool{
	"command": strs.Set("-v", "-V"),
	"chrt":    strs.Set("-p", "--pid"),
	"ionice":  strs.Set("-p", "--pid", "--pgid", "--uid"),
	"taskset": strs.Set("-p", "--pid"),
}

// Positional arguments a wrapper takes before the command it runs
var wrapperOperands = map[string]int{"chroot": 1, "chrt": 1, "taskset": 1, "timeout": 1}

// Words in command position, each one a wrapper or the command it runs
func commandWords(command string) []span {
	var out []span
	for _, pos := range commandStarts(command) {
		wrapper, operands, options, skipArg := "", 0, true, false
		for pos < len(command) {
			if at := redirection(command, skipSpace(command, pos)); at >= 0 {
				_, pos, _ = readWord(command, at)
				continue
			}
			start, end, word := readWord(command, pos)
			if start == end {
				break
			}
			pos = end
			plain := plainWord(word)
			name := strings.ToLower(base(plain))
			if skipArg {
				skipArg = false
				continue
			}
			if wrapper != "" && options && plain == "--" {
				options = false
				continue
			}
			if wrapper != "" && options && strings.HasPrefix(plain, "-") {
				option, _, _ := strings.Cut(plain, "=")
				if wrapper == "env" && (option == "--split-string" || strings.HasPrefix(plain, "-S")) {
					break
				}
				if wrapperQueries[wrapper][option] || wrapper == "command" && !strings.HasPrefix(option, "--") && strings.ContainsAny(option[1:], "vV") {
					break
				}
				skipArg = !strings.Contains(plain, "=") && wrapperArgOptions[wrapper][option]
				continue
			}
			if operands > 0 {
				operands--
				continue
			}
			if assignment.MatchString(word) {
				continue
			}
			out = append(out, span{start, end, word})
			if !wrappers[name] {
				break
			}
			wrapper, options, operands = name, true, wrapperOperands[name]
		}
	}
	return out
}

// The command starting at start, up to what ends it, its quoted bytes kept
func commandAt(command string, start int) string {
	end := len(command)
	for sc := scan(command, start, -1, scanOptions{subst: "uq", brace: true, comments: true}); ; {
		st, ok := sc.next()
		if !ok {
			break
		}
		if st.kind == stepComment || st.kind == stepChar && st.quote == 0 && strings.IndexByte(";&|\n)`", command[st.i]) >= 0 {
			end = st.i
			break
		}
	}
	return strings.TrimSpace(command[start:end])
}

type token struct {
	value      string
	start, end int
	// Single quoted, so the shell hands it over as it is
	inert bool
}

// Words of one simple command with where each sits, false on quoting that doesn't close
func spannedTokens(segment string, start int) ([]token, bool) {
	var tokens []token
	var value strings.Builder
	tokenStart := -1
	var quote byte
	depth, inBacktick := 0, false
	flush := func(end int) {
		raw := segment[tokenStart:end]
		inert := strings.HasPrefix(raw, "'") && strings.HasSuffix(raw, "'") || strings.Contains(raw, "='") && strings.HasSuffix(raw, "'")
		tokens = append(tokens, token{value.String(), tokenStart, end, inert})
	}
	endAt := len(segment)
	for sc := scan(segment, start, -1, scanOptions{}); ; {
		st, ok := sc.next()
		if !ok {
			break
		}
		ch, _ := runeAt(segment, st.i)
		if st.kind == stepChar && quote == 0 {
			if unicode.IsSpace(ch) && ch != '\n' {
				if tokenStart >= 0 {
					flush(st.i)
					value.Reset()
					tokenStart = -1
				}
				continue
			}
			stop := false
			switch {
			case strings.HasPrefix(segment[st.i:], "$("):
				depth++
			case ch == '`':
				switch {
				case inBacktick:
					inBacktick = false
				case depth == 0 && backtickFrom(segment, st.i) < 0:
					endAt, stop = st.i, true
				default:
					inBacktick = true
				}
			case ch == ')':
				if depth == 0 {
					endAt, stop = st.i, true
				} else {
					depth--
				}
			case strings.ContainsRune(";|&\n", ch):
				endAt, stop = st.i, true
			}
			if stop {
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
				quote = byte(ch)
			}
		case st.kind == stepEscape:
			value.WriteString(segment[st.i+1 : st.j])
		case ch == '\\' && quote == 0:
			return nil, false
		default:
			value.WriteString(segment[st.i:st.j])
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
	grepArgOptions = strs.Set("--after-context", "--before-context", "--binary-files", "--context", "--directories", "--devices", "--exclude",
		"--exclude-dir", "--exclude-from", "--include", "--label", "--max-count", "--regexp", "--file")
	grepArgLetters = "ABCDdefm"
)

// Where quoted grep -P patterns sit, true when the command can't be read surely
func grepPatterns(command string) ([][2]int, bool) {
	var spans [][2]int
	offset := 0
	for _, segment := range topSegments(command) {
		at := offset + strings.Index(command[offset:], segment)
		offset = at + len(segment)
		for _, ws := range commandWords(segment) {
			if name := strings.ToLower(base(plainWord(ws.word))); name != "grep" && name != "egrep" {
				continue
			}
			tokens, ok := spannedTokens(segment, ws.start)
			if !ok {
				return nil, true
			}
			var args []token
			if len(tokens) > 0 {
				args = tokens[1:]
			}
			var patterns []int
			pcre, explicit, operand, options := false, false, -1, true
			for i := 0; i < len(args); i++ {
				arg := args[i].value
				switch {
				case options && arg == "--":
					options = false
				case options && strings.HasPrefix(arg, "--"):
					option, _, equals := strings.Cut(arg, "=")
					pcre = pcre || option == "--perl-regexp"
					explicit = explicit || option == "--regexp" || option == "--file"
					next := grepArgOptions[option] && !equals
					if next && i+1 >= len(args) {
						return nil, true
					}
					if option == "--regexp" {
						if next {
							patterns = append(patterns, i+1)
						} else {
							patterns = append(patterns, i)
						}
					}
					if next {
						i++
					}
				case options && strings.HasPrefix(arg, "-") && arg != "-":
					letters := []rune(arg[1:])
					for j, letter := range letters {
						pcre = pcre || letter == 'P'
						explicit = explicit || letter == 'e' || letter == 'f'
						if !strings.ContainsRune(grepArgLetters, letter) {
							continue
						}
						attached := j+1 < len(letters)
						if !attached && i+1 >= len(args) {
							return nil, true
						}
						if letter == 'e' {
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
				for _, i := range patterns {
					if args[i].inert {
						spans = append(spans, [2]int{at + args[i].start, at + args[i].end})
					}
				}
			}
		}
	}
	return spans, false
}

// Command with quoted grep -P patterns blanked, whose text is no command; true when the command can't be read surely
func grepSafe(command string) (string, bool) {
	spans, malformed := grepPatterns(command)
	if malformed || len(spans) == 0 {
		return command, malformed
	}
	edits := make([]edit, len(spans))
	for i, s := range spans {
		edits[i] = edit{s[0], s[1], strings.Repeat(" ", utf8.RuneCountInString(command[s[0]:s[1]]))}
	}
	return splice(command, edits), false
}

// Top level commands of the text, in order
func topSegments(command string) []string {
	var out []string
	start := 0
	for sc := scan(command, 0, -1, scanOptions{comments: true}); ; {
		st, ok := sc.next()
		if !ok {
			break
		}
		if st.kind == stepComment || st.kind == stepChar && st.quote == 0 && strings.IndexByte(";&|\n", command[st.i]) >= 0 {
			if start < st.i {
				out = append(out, command[start:st.i])
			}
			start = st.j
		}
	}
	if start < len(command) {
		out = append(out, command[start:])
	}
	return out
}

// Words of a segment from start with < and > as words of their own, false on quoting that doesn't close
func segmentTokens(segment string, start int) ([]string, bool) {
	tokens, err := splitPunct(segment[start:], "<>", false)
	return tokens, err == nil
}

var errUnclosed = errors.New("No closing quotation")

// Words of a line as a POSIX shell splits them, runs of the punctuation characters as words of their own, comments dropped when asked
func splitPunct(s, punct string, comments bool) ([]string, error) {
	var words []string
	var cur strings.Builder
	in := false
	flush := func() {
		if in {
			words = append(words, cur.String())
			cur.Reset()
			in = false
		}
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case comments && c == '#' && !in:
			end := strings.IndexByte(s[i:], '\n')
			if end < 0 {
				i = len(s)
			} else {
				i += end
			}
		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, errUnclosed
			}
			cur.WriteString(s[i+1 : i+1+end])
			in, i = true, i+end+2
		case c == '"':
			i++
			closed := false
			for i < len(s) {
				d := s[i]
				if d == '"' {
					closed = true
					i++
					break
				}
				if d == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`\n", s[i+1]) >= 0 {
					cur.WriteByte(s[i+1])
					i += 2
					continue
				}
				cur.WriteByte(d)
				i++
			}
			if !closed {
				return nil, errUnclosed
			}
			in = true
		case c == '\\':
			if i+1 >= len(s) {
				return nil, errors.New("No escaped character")
			}
			cur.WriteByte(s[i+1])
			in, i = true, i+2
		case spaceByte(c):
			flush()
			i++
		case strings.IndexByte(punct, c) >= 0:
			flush()
			j := i
			for j < len(s) && strings.IndexByte(punct, s[j]) >= 0 {
				j++
			}
			words = append(words, s[i:j])
			i = j
		default:
			cur.WriteByte(c)
			in, i = true, i+1
		}
	}
	flush()
	return words, nil
}

// Command with quoted prose blanked for rules that match anywhere, substitutions inside double quotes left as they are
func maskQuotedProse(command string) string {
	var b strings.Builder
	for sc := scan(command, 0, -1, scanOptions{subst: "q", naiveBacktick: true}); ; {
		st, ok := sc.next()
		if !ok {
			return b.String()
		}
		if st.quote == 0 || st.kind == stepQuote || st.kind == stepSubst {
			b.WriteString(command[st.i:st.j])
		} else {
			b.WriteString(strings.Repeat(" ", utf8.RuneCountInString(command[st.i:st.j])))
		}
	}
}

// Path made absolute with its links followed as far as they exist
func resolve(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	dir, name := filepath.Split(abs)
	dir = strings.TrimSuffix(dir, string(filepath.Separator))
	if dir == "" || dir == abs {
		return abs
	}
	return filepath.Join(resolve(dir), name)
}
