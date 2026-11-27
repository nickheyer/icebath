package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/packages"
	"github.com/nickheyer/icebath/internal/tools"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
	"github.com/nickheyer/icebath/internal/tools/diff"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/term"
)

// Checks run where commands run, by extension; .tsx is left to a language server
var shellLinters = map[string]string{
	".js": "node --check {file} 2>&1",
	".ts": "npx tsc --noEmit {file} 2>&1",
	".go": "go vet {file} 2>&1",
	".rs": "rustfmt --check {file} 2>&1",
}

// Checks run on this machine with the package manager's Node, by extension, in place of the shell ones
var managedLinters = map[string][]string{".js": {"node", "--check"}, ".ts": {"npx", "tsc", "--noEmit"}}

// Per file checks that flood phantom errors, left out when a language server checks the file
var serverChecked = strs.Set(".ts", ".go", ".rs")

// Checks that run the toolchain of the checkout they run in
var projectToolchain = strs.Set(".ts", ".rs")

// Lower case output fragments saying a check's program is there but could not run
var unusable = map[string][]string{
	"npx":     {"this is not the tsc command you are looking for", "could not determine executable to run", "not found in npm registry"},
	"rustfmt": {"no input filename given", "error: not a workspace"},
	"go":      {"cannot find package", "go: cannot find main module"},
}

// What a check run in this process says when the program it needs is missing
const lintSkip = "__SKIP__"

// Parsers of structured files, whose write is refused when the content does not parse
var parsers = map[string]func(content string) (bool, string){".json": lintJSON, ".yaml": lintYAML, ".yml": lintYAML, ".toml": lintTOML}

// Whether the extension's check runs on the content itself rather than on the file, so a write keeps the content it replaces
func contentChecked(extension string) bool { return parsers[extension] != nil || extension == ".py" }

// Line and column of a byte offset of the text, both counted from one
func position(text string, offset int) (int, int) {
	offset = min(max(offset, 0), len(text))
	line := strings.Count(text[:offset], "\n") + 1
	start := strings.LastIndexByte(text[:offset], '\n') + 1
	return line, utf8.RuneCountInString(text[start:offset]) + 1
}

// JSON with NaN, Infinity and -Infinity outside strings blanked to numbers of the same length, as a reader that accepts them sees it
func plainConstants(text string) string {
	out := []byte(text)
	for i := 0; i < len(out); i++ {
		switch {
		case out[i] == '"':
			for i++; i < len(out) && out[i] != '"'; i++ {
				if out[i] == '\\' {
					i++
				}
			}
		case strings.HasPrefix(text[i:], "NaN"):
			copy(out[i:], "0  ")
			i += 2
		case strings.HasPrefix(text[i:], "Infinity"):
			copy(out[i:], "0       ")
			i += 7
		case strings.HasPrefix(text[i:], "-Infinity"):
			copy(out[i:], "-0       ")
			i += 8
		}
	}
	return string(out)
}

// JSON verdict at any depth and number length, NaN and Infinity taken as values; the error names the mistake with its line and column
func lintJSON(content string) (bool, string) {
	if strings.HasPrefix(content, utf8BOM) {
		return false, "invalid JSON: unexpected byte order mark (U+FEFF) at the start of the text (line 1, column 1)"
	}
	text := plainConstants(content)
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var value any
	err := decoder.Decode(&value)
	if err == nil {
		if _, more := decoder.Token(); more != io.EOF {
			line, column := position(text, int(decoder.InputOffset()))
			return false, "invalid JSON: extra data after the document (line " + strconv.Itoa(line) + ", column " + strconv.Itoa(column) + ")"
		}
		return true, ""
	}
	offset := len(text)
	var syntax *json.SyntaxError
	message := err.Error()
	switch {
	case errors.As(err, &syntax):
		offset = int(syntax.Offset)
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		message = "unexpected end of the document"
	}
	line, column := position(text, offset)
	return false, "invalid JSON: " + message + " (line " + strconv.Itoa(line) + ", column " + strconv.Itoa(column) + ")"
}

// YAML verdict over every document of the stream
func lintYAML(content string) (bool, string) {
	decoder := yaml.NewDecoder(strings.NewReader(content))
	for {
		var node yaml.Node
		err := decoder.Decode(&node)
		if errors.Is(err, io.EOF) {
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}
	}
}

// TOML verdict, the error naming the first problem with its line and column
func lintTOML(content string) (bool, string) {
	var document map[string]any
	err := toml.Unmarshal([]byte(content), &document)
	if err == nil {
		return true, ""
	}
	var decoding *toml.DecodeError
	if errors.As(err, &decoding) {
		line, column := decoding.Position()
		return false, strings.TrimPrefix(decoding.Error(), "toml: ") + " (at line " + strconv.Itoa(line) + ", column " + strconv.Itoa(column) + ")"
	}
	return false, err.Error()
}

// Python source parsed by the Python where commands run, the error giving its kind, message and position; skipped where there is none
func (o *ops) lintPython(content string) (bool, string) {
	code := "import sys, ast\n" +
		"src = sys.stdin.buffer.read()\n" +
		"try:\n" +
		"    compile(src, '<file>', 'exec', flags=ast.PyCF_ONLY_AST, dont_inherit=True)\n" +
		"except SyntaxError as e:\n" +
		"    kind = {'IndentationError': 'indentation error', 'TabError': 'tab error'}.get(type(e).__name__, 'syntax error')\n" +
		"    loc = ' (line %d, column %d)' % (e.lineno, e.offset or 0) if e.lineno else ''\n" +
		"    print('ICEBATH_LINT:' + kind + ': ' + str(e.msg) + loc)\n" +
		"except (ValueError, MemoryError, RecursionError) as e:\n" +
		"    print('ICEBATH_LINT:syntax error: ' + str(e))\n" +
		"else:\n" +
		"    print('ICEBATH_LINT:OK')\n"
	encoded := base64.StdEncoding.EncodeToString([]byte(code))
	command := func(py string) string {
		return py + ` -c "import base64; exec(base64.b64decode('` + encoded + `').decode())"`
	}
	result := o.execStdin(command(o.python()), "", 0, &content)
	if result.code != 0 && strings.Contains(result.output, "python3") {
		result = o.execStdin(command("python"), "", 0, &content)
	}
	out := clean(result.output)
	at := strings.LastIndex(out, "ICEBATH_LINT:")
	if at < 0 {
		return true, lintSkip
	}
	verdict, _, _ := strings.Cut(out[at+len("ICEBATH_LINT:"):], "\n")
	if verdict = strings.TrimSpace(verdict); verdict == "OK" {
		return true, ""
	}
	return false, verdict
}

// First NaN, Infinity or -Infinity of otherwise valid JSON, empty when it has none
func jsonConstant(text string) string {
	if !strings.Contains(text, "NaN") && !strings.Contains(text, "Infinity") {
		return ""
	}
	body := strings.TrimPrefix(text, utf8BOM)
	if ok, _ := lintJSON(body); !ok {
		return ""
	}
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '"':
			for i++; i < len(body) && body[i] != '"'; i++ {
				if body[i] == '\\' {
					i++
				}
			}
		case 'N':
			return "NaN"
		case 'I':
			return "Infinity"
		case '-':
			if strings.HasPrefix(body[i+1:], "Infinity") {
				return "-Infinity"
			}
		}
	}
	return ""
}

// Refusal of a JSON write that brings in a constant JSON has not, when the content it replaces had none
func refuseConstant(path, content string, before *string) string {
	constant := jsonConstant(content)
	if constant == "" || before != nil && jsonConstant(*before) != "" {
		return ""
	}
	return "Refusing to write '" + path + "': candidate content uses " + constant + ", which is not valid JSON. The file was NOT created or " +
		"modified. Use null or a string instead and retry."
}

// Refusal of JSON, YAML or TOML content that does not parse, before anything is written
func refuseUnparsed(path, content string) string {
	extension := ext(path)
	parse := parsers[extension]
	if parse == nil {
		return ""
	}
	ok, message := parse(content)
	if ok || message == lintSkip {
		return ""
	}
	return "Refusing to write '" + path + "': candidate content fails " + extension + " syntax validation (" + message + "). The file was NOT " +
		"created or modified. Fix the content and retry."
}

// Syntax check of the content of a path, through the check run in this process for its extension, else one run where commands run
func (o *ops) lint(path string, content *string) *lintResult {
	extension := ext(path)
	check := parsers[extension]
	if extension == ".py" {
		check = o.lintPython
	}
	if check != nil {
		if content == nil {
			read := o.exec("cat "+o.path(path)+" 2>/dev/null", "", 0)
			if read.code != 0 {
				return &lintResult{ok: true, skipped: true, message: "Failed to read " + path + " for lint"}
			}
			content = &read.output
		}
		ok, message := check(*content)
		if message == lintSkip {
			return &lintResult{ok: true, skipped: true, message: "No linter available for " + extension + " (missing dependency)"}
		}
		if ok {
			return &lintResult{ok: true}
		}
		return &lintResult{output: message}
	}
	command, ok := shellLinters[extension]
	if !ok {
		return &lintResult{ok: true, skipped: true, message: "No linter for " + extension + " files"}
	}
	program := fields(command)[0]
	switch {
	case extension == ".ts" && o.tsconfig(path):
		return &lintResult{ok: true, skipped: true, message: "Project tsconfig.json detected — per-file tsc skipped (single-file tsc can't resolve " +
			"project aliases/globals; use the LSP tier or `tsc -p tsconfig.json` for real diagnostics)."}
	case serverChecked[extension] && o.serverChecks(path):
		return &lintResult{ok: true, skipped: true, message: "LSP server handles " + extension + " — shell linter skipped"}
	case projectToolchain[extension] && o.host && !tools.Trusted(o.cfg, o.base, o.working()...):
		return &lintResult{ok: true, skipped: true, message: program + " skipped: untrusted workspace (add it to " + toolsconfig.TrustedWorkspaces +
			" to lint " + extension + " here)"}
	}
	var result run
	if spec, managed := managedLinters[extension]; managed && o.host {
		program = spec[0]
		checked, found := o.managedLint(spec, path)
		if !found {
			return &lintResult{ok: true, skipped: true, message: program + " not available (Icebath-managed Node not installed)"}
		}
		result = checked
	} else if !o.has(program) {
		return &lintResult{ok: true, skipped: true, message: program + " not available"}
	} else {
		result = o.exec(strings.ReplaceAll(command, "{file}", o.nativeArg(path)), "", 30*time.Second)
	}
	if result.code != 0 && unusableLinter(program, result.output) {
		cleaned := strings.TrimSpace(term.StripANSI(result.output))
		line := strs.Head(cleaned, 120)
		for _, l := range diff.Split(cleaned) {
			if t := strings.TrimSpace(l); t != "" {
				line = t
				break
			}
		}
		return &lintResult{ok: true, skipped: true, message: program + " not usable: " + strs.Head(line, 200)}
	}
	return &lintResult{ok: result.code == 0, output: strings.TrimSpace(result.output)}
}

// Runs a check on this machine under the package manager's Node, never installing it; false when that Node is not installed
func (o *ops) managedLint(spec []string, path string) (run, bool) {
	executable := packages.Which(spec[0], o.r.packages.EnvFor([]string{packages.NPM}, map[string]string{"PATH": ""})["PATH"])
	if executable == "" {
		return run{}, false
	}
	file, dir := o.full(path), o.base
	if windows {
		file, dir = msysToWindows(file), msysToWindows(dir)
	}
	ctx, cancel := context.WithTimeout(o.ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, append(slices.Clone(spec[1:]), file)...)
	cmd.Dir = dir
	for name, value := range o.r.packages.EnvFor([]string{packages.NPM}, o.r.terminal.ChildEnv(o.cfg, nil)) {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return run{output: spec[0] + " timed out after 30s", code: 124}, true
	}
	out := run{output: strings.ToValidUTF8(string(output), "\uFFFD")}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		out.code = exit.ExitCode()
	} else if err != nil {
		out.output, out.code = err.Error(), 1
	}
	return out, true
}

func unusableLinter(program, output string) bool {
	lower := strings.ToLower(output)
	for _, fragment := range unusable[program] {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}

// Whether a tsconfig.json is in the path's folder or above it, on this machine's files
func (o *ops) tsconfig(path string) bool {
	if !o.host {
		return false
	}
	dir := filepath.Dir(o.full(path))
	for {
		if st, err := os.Stat(filepath.Join(dir, "tsconfig.json")); err == nil && st.Mode().IsRegular() {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

// Check after a write that reports only the errors it brought in when the content it replaced is known
func (o *ops) lintDelta(path string, before, after *string) *lintResult {
	post := o.lint(path, after)
	if post.ok || post.skipped || before == nil {
		return post
	}
	pre := o.lint(path, before)
	if pre.ok || pre.skipped || pre.output == "" {
		return post
	}
	old := map[string]bool{}
	for _, line := range diff.Split(pre.output) {
		if t := strings.TrimSpace(line); t != "" {
			old[t] = true
		}
	}
	var added []string
	for _, line := range diff.Split(post.output) {
		if t := strings.TrimSpace(line); t != "" && !old[t] {
			added = append(added, line)
		}
	}
	if len(added) == 0 {
		return &lintResult{output: post.output, message: "Pre-existing lint errors — this edit didn't introduce new ones but the file is still broken."}
	}
	return &lintResult{output: "New lint errors introduced by this edit (pre-existing errors filtered out):\n" + strings.Join(added, "\n")}
}

// Folders the user works in that vouch for a workspace besides where icebath started: the session's, unless it runs as a scheduled job,
// which no one points at a folder
func (o *ops) working() []string {
	if tools.Scheduled(o.call.Env.Platform) {
		return nil
	}
	return []string{o.call.Env.Cwd}
}
