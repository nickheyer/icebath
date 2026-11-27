package skills

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

// One place in a skill's Python that loads code or reaches attributes by a name computed at run time, for a person to look at
type deepFinding struct {
	File        string
	Line        int
	PatternID   string
	Description string
}

// Folders of Python the deep scan leaves out
var deepSkipped = map[string]bool{"__pycache__": true, ".venv": true, "venv": true, "node_modules": true}

// Kinds of Python token the deep scan reads
const (
	tokName = iota
	tokNumber
	tokString
	// A formatted string, whose expressions are scanned on their own
	tokFormatted
	tokOp
	// End of a logical line
	tokEnd
)

type pyToken struct {
	kind int
	text string
	line int
}

// Python keywords, which are never the name a call or a subscript follows
var pyKeywords = map[string]bool{
	"False": true, "None": true, "True": true, "and": true, "as": true, "assert": true, "async": true, "await": true, "break": true, "class": true,
	"continue": true, "def": true, "del": true, "elif": true, "else": true, "except": true, "finally": true, "for": true, "from": true,
	"global": true, "if": true, "import": true, "in": true, "is": true, "lambda": true, "nonlocal": true, "not": true, "or": true, "pass": true,
	"raise": true, "return": true, "try": true, "while": true, "with": true, "yield": true,
}

// Operators of more than one character, longest first
var pyOperators = []string{"**=", "//=", ">>=", "<<=", "...", "->", ":=", "==", "!=", "<=", ">=", "**", "//", "<<", ">>", "+=", "-=", "*=", "/=",
	"%=", "&=", "|=", "^=", "@="}

// Tokens of Python source and, apart, the tokens of each expression its formatted strings hold; the line of the first given
type pyLexer struct {
	src    []rune
	at     int
	line   int
	depth  int
	tokens []pyToken
	// Expressions of formatted strings, each scanned as its own source
	nested [][]pyToken
}

func lexPython(source string, line int) ([]pyToken, [][]pyToken) {
	l := &pyLexer{src: []rune(source), line: line}
	l.run()
	return l.tokens, l.nested
}

func (l *pyLexer) peek(offset int) rune {
	if l.at+offset < len(l.src) {
		return l.src[l.at+offset]
	}
	return 0
}

func (l *pyLexer) emit(kind int, text string, line int) {
	l.tokens = append(l.tokens, pyToken{kind, text, line})
}

func (l *pyLexer) run() {
	for l.at < len(l.src) {
		c := l.src[l.at]
		switch {
		case c == '\n':
			if l.depth == 0 {
				l.emit(tokEnd, "", l.line)
			}
			l.line++
			l.at++
		case c == '\\' && l.peek(1) == '\n':
			l.line++
			l.at += 2
		case c == '#':
			for l.at < len(l.src) && l.src[l.at] != '\n' {
				l.at++
			}
		case unicode.IsSpace(c):
			l.at++
		case l.stringStart():
		case unicode.IsLetter(c) || c == '_':
			start := l.at
			for l.at < len(l.src) && (unicode.IsLetter(l.src[l.at]) || unicode.IsDigit(l.src[l.at]) || l.src[l.at] == '_') {
				l.at++
			}
			l.emit(tokName, string(l.src[start:l.at]), l.line)
		case unicode.IsDigit(c) || c == '.' && unicode.IsDigit(l.peek(1)):
			start := l.at
			for l.at < len(l.src) {
				r := l.src[l.at]
				if unicode.IsDigit(r) || unicode.IsLetter(r) || r == '_' || r == '.' ||
					(r == '+' || r == '-') && (l.src[l.at-1] == 'e' || l.src[l.at-1] == 'E') && !strings.ContainsAny(string(l.src[start:l.at]), "xX") {
					l.at++
					continue
				}
				break
			}
			l.emit(tokNumber, string(l.src[start:l.at]), l.line)
		default:
			l.operator()
		}
	}
	l.emit(tokEnd, "", l.line)
}

func (l *pyLexer) operator() {
	for _, op := range pyOperators {
		if strings.HasPrefix(string(l.src[l.at:min(len(l.src), l.at+len(op))]), op) {
			l.emit(tokOp, op, l.line)
			l.at += len(op)
			return
		}
	}
	c := l.src[l.at]
	switch c {
	case '(', '[', '{':
		l.depth++
	case ')', ']', '}':
		l.depth = max(0, l.depth-1)
	}
	l.emit(tokOp, string(c), l.line)
	l.at++
}

// Reads a string literal starting here with its prefix, false when none starts here
func (l *pyLexer) stringStart() bool {
	prefix := 0
	for prefix < 3 && l.at+prefix < len(l.src) && strings.ContainsRune("rRbBuUfF", l.src[l.at+prefix]) {
		prefix++
	}
	if l.at+prefix >= len(l.src) || l.src[l.at+prefix] != '"' && l.src[l.at+prefix] != '\'' {
		return false
	}
	letters := strings.ToLower(string(l.src[l.at : l.at+prefix]))
	if prefix > 0 && strings.Trim(letters, "rbuf") != "" {
		return false
	}
	raw, formatted := strings.Contains(letters, "r"), strings.Contains(letters, "f")
	line := l.line
	l.at += prefix
	quote := l.src[l.at]
	triple := l.peek(1) == quote && l.peek(2) == quote
	width := 1
	if triple {
		width = 3
	}
	l.at += width
	start := l.at
	for l.at < len(l.src) {
		c := l.src[l.at]
		if c == '\\' && !raw && l.at+1 < len(l.src) {
			if l.src[l.at+1] == '\n' {
				l.line++
			}
			l.at += 2
			continue
		}
		if c == '\n' {
			if !triple {
				break
			}
			l.line++
		}
		if c == quote && (!triple || l.peek(1) == quote && l.peek(2) == quote) {
			break
		}
		l.at++
	}
	body := string(l.src[start:min(l.at, len(l.src))])
	if l.at < len(l.src) && l.src[l.at] != '\n' {
		l.at += width
	}
	if formatted {
		l.emit(tokFormatted, body, line)
		l.fields(body, line)
		return true
	}
	l.emit(tokString, body, line)
	return true
}

// Expressions of the replacement fields of a formatted string's body, lexed apart
func (l *pyLexer) fields(body string, line int) {
	runes := []rune(body)
	for i := 0; i < len(runes); i++ {
		switch {
		case runes[i] == '\n':
			line++
		case runes[i] == '{' && i+1 < len(runes) && runes[i+1] == '{':
			i++
		case runes[i] == '{':
			depth, start, startLine := 0, i+1, line
			end := len(runes)
			var quote rune
			for j := i + 1; j < len(runes); j++ {
				r := runes[j]
				switch {
				case quote != 0:
					if r == quote {
						quote = 0
					}
				case r == '\'' || r == '"':
					quote = r
				case r == '(' || r == '[' || r == '{':
					depth++
				case r == ')' || r == ']':
					depth--
				case r == '}' && depth > 0:
					depth--
				case depth == 0 && (r == '}' || r == ':' || r == '!' && j+1 < len(runes) && runes[j+1] != '='):
					end = j
				}
				if r == '\n' {
					line++
				}
				if end != len(runes) {
					break
				}
			}
			tokens, nested := lexPython(string(runes[start:end]), startLine)
			l.nested = append(append(l.nested, tokens), nested...)
			i = end
		}
	}
}

// Index of the bracket closing each opening one, and opening each closing one
func brackets(tokens []pyToken) map[int]int {
	match := map[int]int{}
	var stack []int
	for i, t := range tokens {
		if t.kind != tokOp {
			continue
		}
		switch t.text {
		case "(", "[", "{":
			stack = append(stack, i)
		case ")", "]", "}":
			if len(stack) > 0 {
				open := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				match[open], match[i] = i, open
			}
		}
	}
	return match
}

func isOp(t pyToken, text string) bool { return t.kind == tokOp && t.text == text }

// Line where the expression holding the token at the index starts, walking back over its attributes, calls and subscripts
func exprStart(tokens []pyToken, match map[int]int, j int) int {
	for j > 0 {
		t := tokens[j]
		if t.kind == tokOp && (t.text == ")" || t.text == "]" || t.text == "}") {
			open, ok := match[j]
			if !ok {
				break
			}
			j = open
			prev := tokens[max(0, j-1)]
			trailer := j > 0 && (tokens[j].text == "(" || tokens[j].text == "[") &&
				(prev.kind == tokName && !pyKeywords[prev.text] || prev.kind == tokString || prev.kind == tokFormatted ||
					prev.kind == tokOp && (prev.text == ")" || prev.text == "]"))
			if !trailer {
				break
			}
			j--
			continue
		}
		if isOp(tokens[j-1], ".") && j >= 2 {
			j -= 2
			continue
		}
		break
	}
	return tokens[j].line
}

// Whether the tokens of an argument or a subscript make one constant: strings, a number, None, True, False or ..., in parentheses or not
func constant(tokens []pyToken) bool {
	for len(tokens) >= 2 && isOp(tokens[0], "(") && isOp(tokens[len(tokens)-1], ")") {
		if m := brackets(tokens); m[0] != len(tokens)-1 {
			break
		}
		tokens = tokens[1 : len(tokens)-1]
	}
	if len(tokens) == 0 {
		return false
	}
	if !slices.ContainsFunc(tokens, func(t pyToken) bool { return t.kind != tokString }) {
		return true
	}
	if len(tokens) != 1 {
		return false
	}
	t := tokens[0]
	return t.kind == tokNumber || t.kind == tokName && (t.text == "None" || t.text == "True" || t.text == "False") || isOp(t, "...")
}

// Positional arguments of the call whose parentheses are at open and close, each as its tokens
func positional(tokens []pyToken, match map[int]int, open, close int) [][]pyToken {
	var args [][]pyToken
	start := open + 1
	flush := func(end int) {
		arg := tokens[start:end]
		keyword := len(arg) >= 2 && arg[0].kind == tokName && isOp(arg[1], "=")
		if len(arg) > 0 && !keyword && !isOp(arg[0], "**") {
			args = append(args, arg)
		}
	}
	for i := open + 1; i < close; i++ {
		if other, ok := match[i]; ok && other > i {
			i = other
			continue
		}
		if isOp(tokens[i], ",") {
			flush(i)
			start = i + 1
		}
	}
	flush(close)
	return args
}

// Calls of builtins that load code or reach attributes by name, whose argument at the index is checked
var dynamicCalls = map[string]struct {
	arg             int
	id, description string
}{
	"__import__": {0, "dynamic_import_computed", "__import__ with non-literal module name"},
	"getattr":    {1, "dynamic_getattr", "getattr with non-literal attribute name"},
}

func importlib(name string) bool {
	return name == "importlib" || strings.HasPrefix(name, "importlib.")
}

// Dotted name starting at the index and the index after it
func dotted(tokens []pyToken, i int) (string, int) {
	var parts []string
	for i < len(tokens) && tokens[i].kind == tokName {
		parts = append(parts, tokens[i].text)
		if i+1 < len(tokens) && isOp(tokens[i+1], ".") {
			i += 2
			continue
		}
		i++
		break
	}
	return strings.Join(parts, "."), i
}

// Findings of one token stream of a file
func deepTokens(tokens []pyToken, rel string) []deepFinding {
	match := brackets(tokens)
	var out []deepFinding
	add := func(line int, id, description string) {
		out = append(out, deepFinding{rel, line, id, description})
	}
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		switch {
		case t.kind == tokName && t.text == "from":
			j := i + 1
			for j < len(tokens) && isOp(tokens[j], ".") || j < len(tokens) && isOp(tokens[j], "...") {
				j++
			}
			module, next := dotted(tokens, j)
			if next < len(tokens) && tokens[next].kind == tokName && tokens[next].text == "import" {
				if importlib(module) {
					add(t.line, "importlib_import", "from "+module+" import ... — enables dynamic module loading")
				}
				for i = next + 1; i < len(tokens) && tokens[i].kind != tokEnd && !isOp(tokens[i], ";"); i++ {
				}
			}
		case t.kind == tokName && t.text == "import":
			for j := i + 1; j < len(tokens) && tokens[j].kind != tokEnd && !isOp(tokens[j], ";"); {
				name, next := dotted(tokens, j)
				if name != "" && importlib(name) {
					add(t.line, "importlib_import", "import "+name+" — enables dynamic module loading")
				}
				if next < len(tokens) && tokens[next].kind == tokName && tokens[next].text == "as" {
					next += 2
				}
				if next < len(tokens) && isOp(tokens[next], ",") {
					next++
				}
				if next == j {
					next++
				}
				j, i = next, next-1
			}
		case t.kind == tokName && i+1 < len(tokens) && isOp(tokens[i+1], "("):
			attribute := i > 0 && isOp(tokens[i-1], ".")
			close, ok := match[i+1]
			if !ok {
				continue
			}
			if attribute && t.text == "import_module" {
				add(exprStart(tokens, match, i), "dynamic_import", "importlib.import_module() — loads arbitrary modules at runtime")
			}
			if call, known := dynamicCalls[t.text]; known && !attribute {
				if args := positional(tokens, match, i+1, close); len(args) > call.arg && !constant(args[call.arg]) {
					add(t.line, call.id, call.description)
				}
			}
		case t.kind == tokName && t.text == "__dict__" && i > 0 && isOp(tokens[i-1], ".") && i+1 < len(tokens) && isOp(tokens[i+1], "["):
			if close, ok := match[i+1]; ok && !constant(tokens[i+2:close]) {
				add(exprStart(tokens, match, i), "dict_access", "__dict__[<computed>] — dynamic attribute access")
			}
		}
	}
	return out
}

// Findings of one Python file, read as UTF-8 with what is not replaced
func deepFile(path, rel string) []deepFinding {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	text := strings.TrimPrefix(strings.ToValidUTF8(string(data), "\uFFFD"), "\uFEFF")
	tokens, nested := lexPython(text, 1)
	out := deepTokens(tokens, rel)
	for _, expression := range nested {
		out = append(out, deepTokens(expression, rel)...)
	}
	return out
}

// Findings of a Python file, or of every Python file in a folder outside the folders left out
func deepScan(path string) []deepFinding {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if !info.IsDir() {
		if strings.ToLower(filepath.Ext(path)) == ".py" {
			return deepFile(path, filepath.Base(path))
		}
		return nil
	}
	var files []string
	for _, rel := range walkFiles(path) {
		parts := strings.Split(rel, "/")
		if strings.HasSuffix(rel, ".py") && !slices.ContainsFunc(parts[:len(parts)-1], func(p string) bool { return deepSkipped[p] }) {
			files = append(files, rel)
		}
	}
	slices.SortFunc(files, func(a, b string) int { return slices.Compare(strings.Split(a, "/"), strings.Split(b, "/")) })
	var out []deepFinding
	for _, rel := range files {
		out = append(out, deepFile(filepath.Join(path, filepath.FromSlash(rel)), rel)...)
	}
	return out
}

// The deep scan as the user reads it, grouped by file
func deepReport(findings []deepFinding, name string) string {
	header := "AST deep scan"
	if name != "" {
		header += ": " + name
	}
	if len(findings) == 0 {
		return header + "\n  No dynamic import/access patterns detected."
	}
	sorted := slices.Clone(findings)
	slices.SortFunc(sorted, func(a, b deepFinding) int {
		return cmp.Or(cmp.Compare(a.File, b.File), cmp.Compare(a.Line, b.Line), cmp.Compare(a.PatternID, b.PatternID), cmp.Compare(a.Description, b.Description))
	})
	lines := []string{header, fmt.Sprintf("  %d finding(s):", len(findings))}
	current := "\x00"
	for _, f := range sorted {
		if f.File != current {
			current = f.File
			lines = append(lines, "  "+f.File)
		}
		lines = append(lines, fmt.Sprintf("    L%d  %s  — %s", f.Line, f.PatternID, f.Description))
	}
	return strings.Join(append(lines, "", "  Note: diagnostic hints for human review, not security verdicts."), "\n")
}
