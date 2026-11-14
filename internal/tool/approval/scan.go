package approval

import (
	"iter"
	"slices"
	"strings"
)

type stepKind uint8

const (
	stepChar stepKind = iota
	stepEsc
	stepQuote
	stepSubst
	stepComment
)

type step struct {
	kind  stepKind
	i, j  int
	quote byte
}

type scanMode struct {
	substUnquoted    bool
	substQuoted      bool
	brace            bool
	stopUnterminated bool
	naiveBacktick    bool
	comments         bool
}

func scanShell(text string, start, end int, m scanMode) iter.Seq[step] {
	return func(yield func(step) bool) {
		var quote byte
		for i := start; i < end; {
			ch := text[i]
			kind, j := stepChar, i+runeWidth(text, i)
			subst := (quote == 0 && m.substUnquoted) || (quote == '"' && m.substQuoted)
			switch {
			case m.comments && quote == 0 && commentStart(text, i):
				kind = stepComment
				if k := strings.IndexByte(text[i:end], '\n'); k >= 0 {
					j = i + k
				} else {
					j = end
				}
			case quote != '\'' && ch == '\\' && i+1 < end:
				kind, j = stepEsc, i+1+runeWidth(text, i+1)
			case (quote != 0 && ch == quote) || (quote == 0 && (ch == '\'' || ch == '"')):
				kind = stepQuote
			case quote != '\'' && subst && (ch == '`' || strings.HasPrefix(text[i:], "$(") ||
				(m.brace && quote == 0 && strings.HasPrefix(text[i:], "${"))):
				closer := -1
				switch {
				case ch == '`' && m.naiveBacktick:
					if k := strings.IndexByte(text[i+1:], '`'); k >= 0 {
						closer = i + 1 + k + 1
					}
				case ch == '`':
					closer = backtickEnd(text, i)
				case text[i+1] == '(':
					closer = dollarParenEnd(text, i)
				default:
					if k := strings.IndexByte(text[i+2:], '}'); k >= 0 {
						closer = i + 2 + k + 1
					}
				}
				if closer >= 0 {
					kind, j = stepSubst, closer
				} else if m.stopUnterminated {
					yield(step{stepSubst, i, -1, quote})
					return
				}
			}
			if !yield(step{kind, i, j, quote}) {
				return
			}
			if kind == stepQuote {
				if quote != 0 {
					quote = 0
				} else {
					quote = ch
				}
			}
			i = j
		}
	}
}

func commentStart(text string, i int) bool {
	if text[i] != '#' {
		return false
	}
	if i == 0 {
		return true
	}
	prev := runeBefore(text, i)
	return pySpace(prev) || strings.ContainsRune(";&|()<>", prev)
}

func backtickEnd(text string, start int) int {
	for k := start + 1; k < len(text); {
		switch text[k] {
		case '\\':
			if k+1 >= len(text) {
				return -1
			}
			k += 1 + runeWidth(text, k+1)
		case '`':
			return k + 1
		default:
			k++
		}
	}
	return -1
}

func dollarParenEnd(text string, start int) int {
	depth := 1
	for st := range scanShell(text, start+2, len(text), scanMode{}) {
		if st.kind == stepChar && st.quote == 0 {
			if strings.HasPrefix(text[st.i:], "$(") {
				depth++
			} else if text[st.i] == ')' {
				depth--
			}
			if depth == 0 {
				return st.i + 1
			}
		}
	}
	return -1
}

func readShellWord(command string, pos int) (int, int, string) {
	start := skipSpace(command, pos)
	end := start
	for st := range scanShell(command, start, len(command), scanMode{substUnquoted: true, brace: true}) {
		if st.kind == stepChar && st.quote == 0 && (spaceAt(command, st.i) || strings.IndexByte(";&|<>()", command[st.i]) >= 0) {
			break
		}
		end = st.j
	}
	return start, end, command[start:end]
}

var commandTransitions = map[string]bool{"if": true, "then": true, "else": true, "elif": true, "do": true, "while": true, "until": true, "!": true}

func commandStarts(command string) []int {
	starts := []int{0}
	var scan func(start, end int)
	scan = func(start, end int) {
		skip := -1
		for st := range scanShell(command, start, end, scanMode{substUnquoted: true, substQuoted: true, stopUnterminated: true, comments: true}) {
			switch {
			case st.kind == stepSubst:
				inner := st.i + 2
				if command[st.i] == '`' {
					inner = st.i + 1
				}
				starts = append(starts, inner)
				if st.j < 0 {
					scan(inner, end)
				} else {
					scan(inner, st.j-1)
				}
			case st.kind == stepChar && st.quote == 0 && st.i != skip:
				c := command[st.i]
				switch {
				case c == '(' || c == ';' || c == '\n' ||
					(c == '{' && (st.i == 0 || pySpace(runeBefore(command, st.i)) || strings.IndexByte("(;&|)", command[st.i-1]) >= 0)):
					starts = append(starts, st.i+1)
				case c == '&' || c == '|':
					if st.i+1 < end && command[st.i+1] == c {
						skip = st.i + 1
						starts = append(starts, st.i+2)
					} else {
						starts = append(starts, st.i+1)
					}
				}
			}
		}
	}
	scan(0, len(command))
	seen := map[int]bool{}
	var out []int
	for k := 0; k < len(starts); k++ {
		s := skipSpace(command, starts[k])
		if s >= len(command) || seen[s] || commentStart(command, s) {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if _, end, word := readShellWord(command, s); commandTransitions[word] {
			starts = append(starts, end)
		}
	}
	return out
}

func markCommandStarts(command, marker string) string {
	var offsets []int
	for _, o := range commandStarts(command) {
		if o > 0 {
			offsets = append(offsets, o)
		}
	}
	if len(offsets) == 0 {
		return command
	}
	slices.Sort(offsets)
	edits := make([]edit, len(offsets))
	for k, o := range offsets {
		edits[k] = edit{o, o, marker}
	}
	return splice(command, edits)
}

func maskQuotedNewlines(command string) string {
	if !strings.Contains(command, "\n") {
		return command
	}
	return maskQuotedNewlinesSpan(command, 0, len(command))
}

func maskQuotedNewlinesSpan(command string, start, end int) string {
	var b strings.Builder
	for st := range scanShell(command, start, end, scanMode{substQuoted: true}) {
		switch {
		case st.kind == stepSubst:
			bodyStart := st.i + 1
			if strings.HasPrefix(command[st.i:], "$(") {
				bodyStart = st.i + 2
			}
			bodyEnd := st.j - 1
			b.WriteString(pySlice(command, st.i, bodyStart))
			b.WriteString(maskQuotedNewlinesSpan(command, bodyStart, bodyEnd))
			b.WriteString(pySlice(command, bodyEnd, st.j))
		case st.quote != 0 && st.kind == stepChar && command[st.i] == '\n':
			b.WriteByte(' ')
		default:
			b.WriteString(command[st.i:st.j])
		}
	}
	return b.String()
}

func maskQuotedProse(command string) string {
	var b strings.Builder
	for st := range scanShell(command, 0, len(command), scanMode{substQuoted: true, naiveBacktick: true}) {
		if st.quote == 0 || st.kind == stepQuote || st.kind == stepSubst {
			b.WriteString(command[st.i:st.j])
		} else {
			b.WriteString(blank(command[st.i:st.j]))
		}
	}
	return b.String()
}

func redirectionAt(command string, p int) (int, bool) {
	q := p
	for q < len(command) && command[q] >= '0' && command[q] <= '9' {
		q++
	}
	rest := command[q:]
	for _, op := range []string{">>", "<<", "<>", ">&", "<&", ">|"} {
		if strings.HasPrefix(rest, op) {
			return q + 2, true
		}
	}
	if strings.HasPrefix(rest, "<") || strings.HasPrefix(rest, ">") {
		return q + 1, true
	}
	return 0, false
}

func envAssignment(word string) bool {
	if word == "" || !(word[0] == '_' || isASCIILetter(word[0])) {
		return false
	}
	k := 1
	for k < len(word) && (word[k] == '_' || isASCIILetter(word[k]) || (word[k] >= '0' && word[k] <= '9')) {
		k++
	}
	return k < len(word) && word[k] == '=' && !strings.Contains(word[k+1:], "\n")
}

func envAssignmentPrefix(word string) bool {
	if word == "" || !(word[0] == '_' || isASCIILetter(word[0])) {
		return false
	}
	k := 1
	for k < len(word) && (word[k] == '_' || isASCIILetter(word[k]) || (word[k] >= '0' && word[k] <= '9')) {
		k++
	}
	return k < len(word) && word[k] == '='
}

func isASCIILetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

var (
	wrapperWords = map[string]bool{"sudo": true, "env": true, "exec": true, "nohup": true, "setsid": true, "time": true,
		"command": true, "builtin": true, "nice": true, "timeout": true, "stdbuf": true, "ionice": true, "chrt": true,
		"taskset": true, "chroot": true}
	sudoOptionsWithArg = set("-c", "--close-from", "-g", "--group", "-h", "--host", "-p", "--prompt", "-u", "--user")
	wrapperArgOptions  = map[string]map[string]bool{
		"chroot":  set("--groups", "--userspec"),
		"sudo":    sudoOptionsWithArg,
		"env":     set("-a", "--argv0", "-C", "--chdir", "-S", "--split-string", "-u", "--unset"),
		"exec":    set("-a"),
		"nice":    set("-n", "--adjustment"),
		"time":    set("-f", "--format", "-o", "--output"),
		"timeout": set("-k", "--kill-after", "-s", "--signal"),
		"stdbuf":  set("-e", "--error", "-i", "--input", "-o", "--output"),
		"ionice":  set("-c", "--class", "-n", "--classdata"),
	}
	wrapperNonExecuting = map[string]map[string]bool{
		"command": set("-v", "-V"),
		"chrt":    set("-p", "--pid"),
		"ionice":  set("-p", "--pid", "--pgid", "--uid"),
		"taskset": set("-p", "--pid"),
	}
	wrapperPositionals = map[string]int{"chroot": 1, "chrt": 1, "taskset": 1, "timeout": 1}
)

func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[it] = true
	}
	return m
}

type wordSpan struct {
	start, end int
	word       string
}

func wordSpans(command string) []wordSpan {
	var out []wordSpan
	for _, pos := range commandStarts(command) {
		wrapper, positionals, options, skipArg := "", 0, true, false
		for pos < len(command) {
			if end, ok := redirectionAt(command, skipSpace(command, pos)); ok {
				_, pos, _ = readShellWord(command, end)
				continue
			}
			ws, we, word := readShellWord(command, pos)
			if ws == we {
				break
			}
			pos = we
			deob := deobfuscateWord(word)
			name := pyLower(pyBasename(deob))
			if skipArg {
				skipArg = false
				continue
			}
			if wrapper != "" && options && deob == "--" {
				options = false
				continue
			}
			if wrapper != "" && options && strings.HasPrefix(deob, "-") {
				option, _, _ := strings.Cut(deob, "=")
				if wrapper == "env" && (option == "--split-string" || strings.HasPrefix(deob, "-S")) {
					break
				}
				if wrapperNonExecuting[wrapper][option] || (wrapper == "command" && !strings.HasPrefix(option, "--") && strings.ContainsAny(option[1:], "vV")) {
					break
				}
				skipArg = !strings.Contains(deob, "=") && wrapperArgOptions[wrapper][option]
				continue
			}
			if positionals > 0 {
				positionals--
				continue
			}
			if envAssignment(word) {
				continue
			}
			out = append(out, wordSpan{ws, we, word})
			if !wrapperWords[name] {
				break
			}
			wrapper, options = name, true
			positionals = wrapperPositionals[name]
		}
	}
	return out
}

func shellCommandSegment(command string, start int) string {
	end := len(command)
	for st := range scanShell(command, start, len(command), scanMode{substUnquoted: true, substQuoted: true, brace: true, comments: true}) {
		if st.kind == stepComment || (st.kind == stepChar && st.quote == 0 && strings.IndexByte(";&|\n)`", command[st.i]) >= 0) {
			end = st.i
			break
		}
	}
	return pyStrip(command[start:end])
}

func topSegments(command string) []string {
	var out []string
	start := 0
	for st := range scanShell(command, 0, len(command), scanMode{comments: true}) {
		if st.kind == stepComment || (st.kind == stepChar && st.quote == 0 && strings.IndexByte(";&|\n", command[st.i]) >= 0) {
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

func stripWordSyntax(word string) string {
	var b strings.Builder
	for st := range scanShell(word, 0, len(word), scanMode{}) {
		switch st.kind {
		case stepQuote:
		case stepEsc:
			b.WriteString(word[st.i+1 : st.j])
		default:
			b.WriteString(word[st.i : st.i+runeWidth(word, st.i)])
		}
	}
	return b.String()
}

func deobfuscateWord(word string) string {
	for range 2 {
		prev := word
		word = stripWordSyntax(replaceSimpleExpansions(word))
		if word == prev {
			break
		}
	}
	return word
}

func containsShellCarrier(command string) bool {
	for _, sp := range wordSpans(command) {
		switch pyLower(pyBasename(deobfuscateWord(sp.word))) {
		case "eval", "sh", "bash", "zsh", "ksh", "dash", "source", ".":
			return true
		}
	}
	return false
}
