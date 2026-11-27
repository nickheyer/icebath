package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/terminal"
)

// Canonical command of a check the project names none for: a script the agent wrote for the change
const adHocCommand = "ad-hoc verification script"

// Most characters of a check's output the ledger keeps, its start and end around what was cut
const summaryChars = 2000

var (
	// Names of the temporary scripts the agent writes to check a change of a project with no checks of its own
	adHocPrefixes = []string{"icebath-verify-", "icebath-ad-hoc-"}
	exeSuffix     = regexp.MustCompile(`(?i)\.(?:exe|bat|cmd)$`)
	interpreter   = regexp.MustCompile(`^(?:python3|python|node|bash|ruby|perl|py|sh)(?:[0-9]+(?:\.[0-9]+)*)?$`)
	version       = regexp.MustCompile(`[0-9.]+$`)
	// Python flags after which the interpreter still runs the module named next
	harmlessFlag = regexp.MustCompile(`^-(?:[IBuEsSOd]+|3(?:\.\d+)?|[WX].*)$`)
	duration     = regexp.MustCompile(`^\d+(?:\.\d+)?[smhd]?$`)
	dotSlash     = regexp.MustCompile(`^(?:\./)+`)
	// Arguments that name a target, so the check ran some of the suite
	targetExtensions = []string{".py", ".js", ".jsx", ".ts", ".tsx", ".rs", ".go", ".java"}
	targetPrefixes   = []string{"test_", "tests", "spec", "__tests__"}
	// Kinds of check by the words of their command, the first group matching
	kinds = []struct {
		words []string
		kind  string
	}{
		{[]string{"lint", "eslint", "ruff"}, "lint"},
		{[]string{"typecheck", "tsc", "mypy", "pyright", "ty"}, "typecheck"},
		{[]string{"build"}, "build"},
		{[]string{"fmt", "format"}, "format"},
	}
	// Spellings that run pytest
	pytests = [][]string{{"python", "-m", "pytest"}, {"python3", "-m", "pytest"}, {"uv", "run", "pytest"}, {"poetry", "run", "pytest"},
		{"pipenv", "run", "pytest"}}
	// Commands that run the command after them keeping its exit status, with the flags they take alone and those that take a value
	wrappers = map[string]struct{ flags, valued []string }{
		"timeout": {[]string{"-p", "--preserve-status", "-f", "--foreground", "-v", "--verbose"}, []string{"-s", "--signal", "-k", "--kill-after"}},
		"nice":    {nil, []string{"-n", "--adjustment"}},
		"nohup":   {nil, nil},
		"stdbuf":  {nil, []string{"-i", "-o", "-e", "--input", "--output", "--error"}},
	}
)

// One top level command of a shell line, and the control operator after it, empty for none
type segment struct {
	words    []string
	operator string
}

// Top level commands of a shell line with the operators between them, quotes and escapes respected; none for a line that can't be read so
func segments(command string, posix bool) []segment {
	type piece struct{ raw, operator string }
	var pieces []piece
	start, i := 0, 0
	var quote byte
	escaped := false
	for i < len(command) {
		c := command[i]
		if escaped || c == '\\' && quote != '\'' {
			escaped = !escaped
			i++
			continue
		}
		if quote != 0 || c == '\'' || c == '"' {
			switch {
			case c == quote:
				quote = 0
			case quote == 0:
				quote = c
			}
			i++
			continue
		}
		operator := ""
		rest := command[i:]
		switch {
		case strings.HasPrefix(rest, "&&") || strings.HasPrefix(rest, "||") || strings.HasPrefix(rest, "|&"):
			operator = rest[:2]
		case c == '\n':
			operator = ";"
		case c == ';' || c == '|' || c == '&' && (i == 0 || command[i-1] != '<' && command[i-1] != '>') && !strings.HasPrefix(rest, "&>"):
			operator = string(c)
		}
		if operator == "" {
			i++
			continue
		}
		raw := strings.TrimSpace(command[start:i])
		if raw == "" {
			return nil
		}
		pieces = append(pieces, piece{raw, operator})
		if c == '\n' {
			i++
		} else {
			i += len(operator)
		}
		start = i
	}
	if quote != 0 || escaped {
		return nil
	}
	if trailing := strings.TrimSpace(command[start:]); trailing != "" {
		pieces = append(pieces, piece{trailing, ""})
	} else if len(pieces) > 0 && pieces[len(pieces)-1].operator != ";" {
		return nil
	}
	out := make([]segment, 0, len(pieces))
	for _, p := range pieces {
		split := terminal.Split
		if !posix {
			split = literalSplit
		}
		words, err := split(p.raw)
		if err != nil || len(words) == 0 {
			return nil
		}
		out = append(out, segment{words, p.operator})
	}
	return out
}

// Words of a command line with quotes taken out and backslashes kept as they are, as a Windows path is written
func literalSplit(line string) ([]string, error) {
	var words []string
	var word strings.Builder
	in := false
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == '\'' || c == '"':
			end := strings.IndexByte(line[i+1:], c)
			if end < 0 {
				return nil, fmt.Errorf("No closing quotation")
			}
			word.WriteString(line[i+1 : i+1+end])
			in, i = true, i+1+end
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if in {
				words = append(words, word.String())
				word.Reset()
				in = false
			}
		default:
			word.WriteByte(c)
			in = true
		}
	}
	if in {
		words = append(words, word.String())
	}
	return words, nil
}

// Exit status of the segment at the index when the shell proves it is that segment's own: none for a line that backgrounds anything, a
// segment before the last ; sequence, a sequence with || in it, or a pipeline segment other than the first whose statuses the shell gave
func ownStatus(segs []segment, at, code int, pipeline []int) (int, bool) {
	if len(segs) == 0 || at < 0 || at >= len(segs) {
		return 0, false
	}
	for _, s := range segs {
		if s.operator == "&" {
			return 0, false
		}
	}
	start := 0
	for i, s := range segs[:len(segs)-1] {
		if s.operator == ";" {
			start = i + 1
		}
	}
	if at < start {
		return 0, false
	}
	sequence := segs[start : len(segs)-1]
	operators := map[string]bool{}
	for _, s := range sequence {
		operators[s.operator] = true
	}
	switch {
	case operators["||"]:
		return 0, false
	case operators["|"] || operators["|&"]:
		first := start
		for i, s := range sequence {
			if s.operator != "|" && s.operator != "|&" {
				first = start + i + 1
			}
		}
		ran := !operators["&&"] || code == 0
		if ran && len(pipeline) >= 2 && at == first {
			return pipeline[0], true
		}
		return 0, false
	case len(operators) == 0:
		return code, true
	case len(operators) == 1 && operators["&&"] && code == 0:
		return code, true
	}
	return 0, false
}

// Words of a canonical command with leading ./ taken off; none when it can't be split
func canonicalWords(canonical string) []string {
	words, err := terminal.Split(canonical)
	if err != nil {
		return nil
	}
	var out []string
	for _, w := range words {
		if w != "" {
			out = append(out, dotSlash.ReplaceAllString(strings.TrimSpace(w), ""))
		}
	}
	return out
}

// Words one wrapper option takes: two for one with its value apart, one for a flag or a value joined to it, zero for an unknown one
func optionWidth(word string, flags, valued []string) int {
	if slices.Contains(flags, word) {
		return 1
	}
	if slices.Contains(valued, word) {
		return 2
	}
	for _, v := range valued {
		if strings.HasPrefix(v, "--") && strings.HasPrefix(word, v+"=") || !strings.HasPrefix(v, "--") && strings.HasPrefix(word, v) {
			return 1
		}
	}
	return 0
}

// Index of the command a wrapper at the index runs; false unless it runs one keeping its status
func skipWrapper(words []string, i int) (int, bool) {
	options := wrappers[words[i]]
	timeout := words[i] == "timeout"
	i++
	for i < len(words) && strings.HasPrefix(words[i], "-") && words[i] != "--" {
		width := optionWidth(words[i], options.flags, options.valued)
		if width == 0 {
			return 0, false
		}
		i += width
	}
	if i < len(words) && words[i] == "--" {
		i++
	}
	if timeout {
		if i >= len(words) || !duration.MatchString(words[i]) {
			return 0, false
		}
		i++
	}
	return i, true
}

// Words of the command a segment runs, without assignments, env, command, time, noglob and the wrappers that keep its status
func stripPrefix(words []string) []string {
	i := 0
	if len(words) > 0 && words[0] == "env" {
		i = 1
	}
	for i < len(words) && strings.Contains(words[i], "=") && !strings.HasPrefix(words[i], "-") {
		i++
	}
	for i < len(words) {
		if words[i] == "command" || words[i] == "time" || words[i] == "noglob" {
			i++
			continue
		}
		if _, ok := wrappers[words[i]]; ok {
			if child, ok := skipWrapper(words, i); ok {
				i = child
				continue
			}
		}
		break
	}
	return slices.Clone(words[min(i, len(words)):])
}

// Last part of a path, backslashes read as slashes
func base(word string) string {
	name := strings.ReplaceAll(word, `\`, "/")
	return name[strings.LastIndex(name, "/")+1:]
}

// Interpreter a word names wherever it lives and whatever its version, empty when it names none
func interpreterOf(word string) string {
	name := exeSuffix.ReplaceAllString(dotSlash.ReplaceAllString(base(word), ""), "")
	if !interpreter.MatchString(name) {
		return ""
	}
	if family := version.ReplaceAllString(name, ""); family != "py" {
		return family
	}
	return "python"
}

// Words as they are compared: leading ./ gone, interpreters by family, and the Python flags that change nothing of what runs dropped
func normalize(words []string) []string {
	out := make([]string, 0, len(words))
	for i := 0; i < len(words); {
		family := interpreterOf(words[i])
		if family != "" {
			out = append(out, family)
		} else {
			out = append(out, dotSlash.ReplaceAllString(words[i], ""))
		}
		i++
		if family == "python" {
			for i < len(words) && harmlessFlag.MatchString(words[i]) {
				if words[i] == "-W" || words[i] == "-X" {
					i += 2
				} else {
					i++
				}
			}
		}
	}
	return out
}

// Spellings that run the same command as the canonical one
func spellings(needle []string) [][]string {
	out := [][]string{needle}
	if len(needle) >= 3 && needle[1] == "run" && slices.Contains([]string{"npm", "pnpm", "yarn", "bun"}, needle[0]) {
		out = append(out, []string{needle[0], needle[2]})
	}
	if len(needle) == 1 && strings.Contains(needle[0], "/") {
		out = append(out, []string{"bash", needle[0]}, []string{"sh", needle[0]})
	}
	if len(needle) == 1 && needle[0] == "pytest" {
		out = append(out, pytests...)
	}
	return out
}

// Canonical command a segment of the line runs, with the words after it and its own status when the shell proves it
type match struct {
	canonical string
	trailing  []string
	status    int
	known     bool
}

// First canonical command a segment of the line runs; with attributed set only one whose own status the shell proves
func findCanonical(command string, canonicals []string, code int, pipeline []int, attributed bool) *match {
	segs := segments(command, true)
	for _, canonical := range canonicals {
		needle := canonicalWords(canonical)
		if len(needle) == 0 {
			continue
		}
		for i, seg := range segs {
			words := normalize(stripPrefix(seg.words))
			status, known := ownStatus(segs, i, code, pipeline)
			if !known && attributed {
				continue
			}
			for _, spelling := range spellings(needle) {
				spelling = normalize(spelling)
				if len(words) >= len(spelling) && slices.Equal(words[:len(spelling)], spelling) {
					return &match{canonical, words[len(spelling):], status, known}
				}
			}
		}
	}
	return nil
}

// Kind of check a canonical command is: lint, typecheck, build, format, check or test
func kindOf(canonical string) string {
	lower := strings.ToLower(canonical)
	for _, group := range kinds {
		for _, word := range group.words {
			if strings.Contains(lower, word) {
				return group.kind
			}
		}
	}
	if strings.Contains(lower, "check") && !strings.Contains(lower, "test") {
		return "check"
	}
	return "test"
}

// Whether an argument after the check names a target rather than an option
func target(arg string) bool {
	if arg == "" || strings.HasPrefix(arg, "-") || strings.Contains(arg, "=") {
		return false
	}
	if strings.ContainsAny(arg, `/\`) || strings.Contains(arg, "::") {
		return true
	}
	return slices.ContainsFunc(targetExtensions, func(ext string) bool { return strings.HasSuffix(arg, ext) }) ||
		slices.ContainsFunc(targetPrefixes, func(prefix string) bool { return strings.HasPrefix(arg, prefix) })
}

// Path with a leading ~ made the user's home
func expand(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	return path
}

// Path made absolute with a leading ~ expanded and its links resolved where they exist
func resolve(path string) string {
	abs, err := filepath.Abs(expand(path))
	if err != nil {
		return filepath.Clean(path)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// Whether the absolute path is the folder or lies under it
func under(path, folder string) bool {
	if folder == "" || !filepath.IsAbs(expand(path)) {
		return false
	}
	rel, err := filepath.Rel(resolve(folder), resolve(path))
	return err == nil && filepath.IsLocal(rel)
}

// Whether a word is a script the agent wrote to check a change: its name prefixed so, in the temporary folder and outside the project
func tempScript(word, root string) bool {
	name := base(word)
	if !slices.ContainsFunc(adHocPrefixes, func(prefix string) bool { return strings.HasPrefix(name, prefix) }) {
		return false
	}
	return under(word, os.TempDir()) && !under(word, root)
}

// Whether a word names an interpreter at any path or version
func interpreterWord(word string) bool {
	return interpreter.MatchString(exeSuffix.ReplaceAllString(base(word), ""))
}

// Whether a segment runs such a script, alone or through an interpreter
func adHocRun(words []string, root string) bool {
	words = stripPrefix(words)
	if len(words) == 0 {
		return false
	}
	command := words[0]
	if tempScript(command, root) {
		return true
	}
	if base(command) == "env" && len(words) > 1 && interpreterWord(words[1]) {
		words, command = words[1:], words[1]
	}
	if !interpreterWord(command) {
		return false
	}
	for _, word := range words[1:] {
		if tempScript(word, root) {
			return true
		}
		if word != "--" && !strings.HasPrefix(word, "-") {
			return false
		}
	}
	return false
}

// Whether the line runs such a script whose own status the shell proves, read the POSIX way and then with backslashes kept
func adHoc(command, root string, code int) bool {
	for _, posix := range []bool{true, false} {
		segs := segments(command, posix)
		for i, seg := range segs {
			if _, known := ownStatus(segs, i, code, nil); known && adHocRun(seg.words, root) {
				return true
			}
		}
	}
	return false
}

// Output trimmed, cut to its start and end with how much was left out between when it is long
func summarize(output string) string {
	runes := []rune(strings.TrimSpace(output))
	if len(runes) <= summaryChars {
		return string(runes)
	}
	head := summaryChars / 3
	return fmt.Sprintf("%s\n... [%d chars omitted] ...\n%s", string(runes[:head]), len(runes)-summaryChars, string(runes[len(runes)-(summaryChars-head):]))
}
