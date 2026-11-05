package tools

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Shell linters by extension, tsx deliberately absent so LSP covers it
var shellLinters = map[string]string{
	".py": "python -m py_compile {file} 2>&1",
	".js": "node --check {file} 2>&1",
	".ts": "npx tsc --noEmit {file} 2>&1",
	".go": "go vet {file} 2>&1",
	".rs": "rustfmt --check {file} 2>&1",
}

// Node linters run on the host under the managed Node, never the user's
var managedNodeLinters = map[string][]string{".js": {"node", "--check"}, ".ts": {"npx", "tsc", "--noEmit"}}

// Per file shell linters that flood phantom errors, skipped when an LSP server claims the file
var shellLinterLSPRedundant = setOf(".ts", ".go", ".rs")

// Shell linters that resolve the toolchain from the checkout they run in
var shellLinterRunsProjectToolchain = setOf(".ts", ".rs")

// Lower case output fragments meaning the linter binary exists but could not run
var linterUnusablePatterns = map[string][]string{
	"npx":     {"this is not the tsc command you are looking for", "could not determine executable to run", "not found in npm registry"},
	"rustfmt": {"no input filename given", "error: not a workspace"},
	"go":      {"cannot find package", "go: cannot find main module"},
}

// Reports whether the linter output says the tool itself could not run
func looksLikeLinterUnusable(baseCmd, output string) bool {
	patterns := linterUnusablePatterns[baseCmd]
	if len(patterns) == 0 {
		return false
	}
	lower := pyLower(output)
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// Error string the in process linters return when their dependency is unavailable
const lintSkip = "__SKIP__"

// In process linters preferred over the shell table, each returning ok and the error text
var lintersInproc = map[string]func(string) (bool, string){
	".py":   lintPythonInproc,
	".json": lintJSONInproc,
	".yaml": lintYAMLInproc,
	".yml":  lintYAMLInproc,
	".toml": lintTOMLInproc,
}

// Extensions whose write is refused when the candidate content fails to parse
var failClosedInprocExts = setOf(".json", ".yaml", ".yml", ".toml")

// JSON syntax verdict with the json.loads exception rendered as the lint error
func lintJSONInproc(content string) (bool, string) {
	excType, msg, jerr := pyJSONLoadsCheck(content)
	switch {
	case excType == "":
		return true, ""
	case jerr != nil:
		return false, "JSONDecodeError: " + jerr.Msg + " (line " + strconv.Itoa(jerr.Lineno) + ", column " + strconv.Itoa(jerr.Colno) + ")"
	}
	return false, excType + ": " + msg
}

// YAML event level syntax verdict matching the ruamel C parser the write gate uses
func lintYAMLInproc(content string) (bool, string) {
	if msg := ruamelCParseError(content); msg != "" {
		return false, msg
	}
	return true, ""
}

// TOML syntax verdict matching tomllib.loads
func lintTOMLInproc(content string) (bool, string) {
	if msg := tomllibLoadsError(content); msg != "" {
		return false, msg
	}
	return true, ""
}

// Parses the stdin bytes with ast.parse and prints the SyntaxError the way the lint reports it
const pythonLintScript = `import ast, sys
data = sys.stdin.buffer.read()
try:
    ast.parse(data)
except Exception as e:
    if isinstance(e, SyntaxError):
        loc = f" (line {e.lineno}, column {e.offset})" if e.lineno else ""
        msg = f"{type(e).__name__}: {e.msg}{loc}"
    else:
        msg = f"{type(e).__name__}: {e}"
    sys.stdout.buffer.write(msg.encode("utf-8", "surrogatepass"))
    sys.stdout.flush()
    sys.exit(3)
`

// Python syntax verdict from ast.parse of the surrogateescape encoded content in the host interpreter
func lintPythonInproc(content string) (bool, string) {
	data, err := pystr.SurrogateEscapeEncode(content)
	if err != nil {
		return false, "UnicodeEncodeError: " + err.Error()
	}
	py := hostPythonInterpreter()
	if py == "" {
		return true, lintSkip
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, py, "-I", "-S", "-c", pythonLintScript)
	cmd.Stdin = bytes.NewReader(data)
	cmd.Env = pythonLintEnv()
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 3 {
			return false, out.String()
		}
		return true, lintSkip
	}
	return true, ""
}

// Minimal child environment for the parse only interpreter
func pythonLintEnv() []string {
	env := []string{"PATH=" + os.Getenv("PATH")}
	for _, k := range []string{"SYSTEMROOT", "SystemRoot", "TEMP", "TMP"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// Deepest container nesting json.loads decodes before its stack guard fires
const pyJSONMaxDepth = 52154

// Python int string conversion digit limit
const pyIntMaxStrDigits = 4300

// json.loads outcome as exception type and message, the decode error set for JSONDecodeError
func pyJSONLoadsCheck(text string) (string, string, *pyjson.DecodeError) {
	rs := pystr.Runes(text)
	if len(rs) > 0 && rs[0] == 0xfeff {
		return "JSONDecodeError", "", &pyjson.DecodeError{Msg: "Unexpected UTF-8 BOM (decode using utf-8-sig)", Pos: 0, Lineno: 1, Colno: 1}
	}
	scan := scanJSONHazards(rs)
	parseText := text
	if scan.overflowAt >= 0 {
		parseText = pystr.FromRunes(rs[:scan.overflowAt]) + "0"
	}
	_, err := pyjson.Loads(parseText)
	errPos := -1
	var jerr *pyjson.DecodeError
	if err != nil {
		if !errors.As(err, &jerr) {
			return "ValueError", err.Error(), nil
		}
		errPos = jerr.Pos
	}
	limit := len(rs)
	if scan.overflowAt >= 0 {
		limit = scan.overflowAt
	}
	if scan.bigIntAt >= 0 && scan.bigIntAt < limit && (errPos < 0 || errPos > scan.bigIntAt) {
		return "ValueError", "Exceeds the limit (" + strconv.Itoa(pyIntMaxStrDigits) + " digits) for integer string conversion: value has " +
			strconv.Itoa(scan.bigIntDigits) + " digits; use sys.set_int_max_str_digits() to increase the limit", nil
	}
	if scan.overflowAt >= 0 && (errPos < 0 || errPos > scan.overflowAt) {
		kind := "array"
		if rs[scan.overflowAt] == '{' {
			kind = "object"
		}
		return "RecursionError", "Stack overflow (used 8156 kB) while decoding a JSON " + kind + " from a unicode string", nil
	}
	if jerr != nil {
		return "JSONDecodeError", "", jerr
	}
	return "", "", nil
}

// First over deep container and first over long integer outside strings, minus one when absent
type jsonHazards struct {
	overflowAt   int
	bigIntAt     int
	bigIntDigits int
}

// String aware scan of the positions where json.loads stops on depth or integer size
func scanJSONHazards(rs []rune) jsonHazards {
	h := jsonHazards{overflowAt: -1, bigIntAt: -1}
	depth := 0
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '"':
			for i++; i < len(rs) && rs[i] != '"'; i++ {
				if rs[i] == '\\' {
					i++
				}
			}
		case c == '[' || c == '{':
			depth++
			if depth > pyJSONMaxDepth && h.overflowAt < 0 {
				h.overflowAt = i
				return h
			}
		case c == ']' || c == '}':
			depth--
		case c == '-' || (c >= '0' && c <= '9'):
			start := i
			j := i
			if rs[j] == '-' {
				j++
			}
			digitsStart := j
			if j < len(rs) && rs[j] == '0' {
				j++
			} else {
				for j < len(rs) && rs[j] >= '0' && rs[j] <= '9' {
					j++
				}
			}
			digits := j - digitsStart
			isFloat := false
			if j+1 < len(rs) && rs[j] == '.' && rs[j+1] >= '0' && rs[j+1] <= '9' {
				isFloat = true
			} else if j < len(rs) && (rs[j] == 'e' || rs[j] == 'E') {
				k := j + 1
				if k < len(rs) && (rs[k] == '+' || rs[k] == '-') {
					k++
				}
				isFloat = k < len(rs) && rs[k] >= '0' && rs[k] <= '9'
			}
			if digits > 0 && !isFloat && digits > pyIntMaxStrDigits && h.bigIntAt < 0 {
				h.bigIntAt, h.bigIntDigits = start, digits
			}
			for j < len(rs) && (rs[j] == '.' || rs[j] == 'e' || rs[j] == 'E' || rs[j] == '+' || rs[j] == '-' || (rs[j] >= '0' && rs[j] <= '9')) {
				j++
			}
			i = j - 1
		}
	}
	return h
}

// First NaN, Infinity or minus Infinity constant of otherwise valid JSON, empty when none
func jsonNonstandardConstant(text string) string {
	if !strings.Contains(text, "NaN") && !strings.Contains(text, "Infinity") {
		return ""
	}
	body, _ := stripBOM(text)
	if excType, _, _ := pyJSONLoadsCheck(body); excType != "" {
		return ""
	}
	rs := []rune(body)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '"':
			for i++; i < len(rs) && rs[i] != '"'; i++ {
				if rs[i] == '\\' {
					i++
				}
			}
		case 'N':
			if hasRunesAt(rs, i, "NaN") {
				return "NaN"
			}
		case 'I':
			if hasRunesAt(rs, i, "Infinity") {
				return "Infinity"
			}
		case '-':
			if hasRunesAt(rs, i+1, "Infinity") {
				return "-Infinity"
			}
		}
	}
	return ""
}

// Reports whether the runes at i spell word
func hasRunesAt(rs []rune, i int, word string) bool {
	w := []rune(word)
	if i < 0 || i+len(w) > len(rs) {
		return false
	}
	for k, r := range w {
		if rs[i+k] != r {
			return false
		}
	}
	return true
}

// Refusal of a JSON write that introduces a nonstandard constant the old content lacked
func refuseIntroducedJSONConstant(path, content string, preContent *string) *WriteResult {
	constant := jsonNonstandardConstant(content)
	if constant == "" {
		return nil
	}
	if preContent != nil && jsonNonstandardConstant(*preContent) != "" {
		return nil
	}
	return &WriteResult{Error: "Refusing to write '" + path + "': candidate content uses " + constant + ", which is " +
		"not valid JSON. The file was NOT created or modified. Use null or a " +
		"string instead and retry."}
}

// Syntax check of a path through the in process linter for its extension, else the shell linter table
func (s *shellFileOps) checkLint(path string, content *string) *LintResult {
	ext := pyLower(pySplitextExt(path))
	if inproc, ok := lintersInproc[ext]; ok {
		if content == nil {
			readResult := s.exec("cat "+s.escapeShellArg(path, true)+" 2>/dev/null", "", 0)
			if readResult.ExitCode != 0 {
				return &LintResult{Success: true, Skipped: true, Message: "Failed to read " + path + " for lint"}
			}
			content = &readResult.Stdout
		}
		ok, errText := inproc(*content)
		if errText == lintSkip {
			return &LintResult{Success: true, Skipped: true, Message: "No linter available for " + ext + " (missing dependency)"}
		}
		res := newLintResult()
		res.Success = ok
		if !ok {
			res.Output = errText
		}
		return res
	}
	linterCmd, ok := shellLinters[ext]
	if !ok {
		return &LintResult{Success: true, Skipped: true, Message: "No linter for " + ext + " files"}
	}
	if ext == ".ts" && s.hasAncestorTsconfig(path) {
		return &LintResult{Success: true, Skipped: true, Message: "Project tsconfig.json detected — per-file tsc skipped " +
			"(single-file tsc can't resolve project aliases/globals; " +
			"use the LSP tier or `tsc -p tsconfig.json` for real " +
			"diagnostics)."}
	}
	if shellLinterLSPRedundant[ext] && s.lspWillHandle(path) {
		return &LintResult{Success: true, Skipped: true, Message: "LSP server handles " + ext + " — shell linter skipped"}
	}
	if shellLinterRunsProjectToolchain[ext] && s.localWorkspaceUntrusted() {
		return &LintResult{Success: true, Skipped: true, Message: pyFields(linterCmd)[0] + " skipped: untrusted workspace (add it to lsp.trusted_workspaces to lint " + ext + " here)"}
	}
	var result *ExecuteResult
	var baseCmd string
	if _, managed := managedNodeLinters[ext]; managed && s.lspLocalOnly() {
		baseCmd = managedNodeLinters[ext][0]
		result = s.runManagedNodeLinter(ext, path)
		if result == nil {
			return &LintResult{Success: true, Skipped: true, Message: baseCmd + " not available (Icebath-managed Node not installed)"}
		}
	} else {
		baseCmd = pyFields(linterCmd)[0]
		if !s.hasCommand(baseCmd) {
			return &LintResult{Success: true, Skipped: true, Message: baseCmd + " not available"}
		}
		result = s.exec(strings.ReplaceAll(linterCmd, "{file}", s.escapeNativeToolArg(path)), "", 30*time.Second)
	}
	if result.ExitCode != 0 && looksLikeLinterUnusable(baseCmd, result.Stdout) {
		cleaned := pyStrip(StripANSI(result.Stdout))
		firstLine := runePrefix(cleaned, 120)
		for _, ln := range pySplitLines(cleaned) {
			if t := pyStrip(ln); t != "" {
				firstLine = t
				break
			}
		}
		return &LintResult{Success: true, Skipped: true, Message: baseCmd + " not usable: " + runePrefix(firstLine, 200)}
	}
	res := newLintResult()
	res.Success = result.ExitCode == 0
	res.Output = pyStrip(result.Stdout)
	return res
}

// Runs the extension's Node linter on the host under the managed Node, nil when none is installed
func (s *shellFileOps) runManagedNodeLinter(ext, path string) *ExecuteResult {
	spec := managedNodeLinters[ext]
	tool, args := spec[0], spec[1:]
	executable := lintWhich(tool, managedNodePathEnv(map[string]string{"PATH": ""})["PATH"])
	if executable == "" {
		return nil
	}
	cwd := s.liveCwd()
	if isWindowsHost {
		path = msysToWindowsPath(path)
		if cwd != "" {
			cwd = msysToWindowsPath(cwd)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, append(append([]string{}, args...), path)...)
	cmd.Dir = cwd
	childEnv := managedNodePathEnv(IcebathSubprocessEnv(s.env, false, nil))
	cmd.Env = make([]string, 0, len(childEnv))
	for k, v := range childEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	hideConsoleWindow(cmd)
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return &ExecuteResult{Stdout: tool + " timed out after 30s", ExitCode: 124}
	}
	text := decodeUTF8Replace(out.Bytes())
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			panic(err)
		}
		return &ExecuteResult{Stdout: text, ExitCode: pyReturnCode(exitErr)}
	}
	return &ExecuteResult{Stdout: text, ExitCode: 0}
}

// Base environment with the installed package manager npm and its Node composed in, never installing
func withIcebathNodePath(base map[string]string) map[string]string {
	return pmEnvFor([]string{"npm"}, base)
}

// Managed Node environment composer, swapped by tests for a stand in store
var managedNodePathEnv = withIcebathNodePath

// Child return code the way subprocess reports it, the negated signal for a signalled child
func pyReturnCode(exitErr *exec.ExitError) int {
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && runtime.GOOS != "windows" && ws.Signaled() {
		return -int(ws.Signal())
	}
	return exitErr.ExitCode()
}

// Executable named tool on a PATH list the way shutil.which searches it, empty when absent
func lintWhich(tool, pathList string) string {
	exts := []string{""}
	if isWindowsHost {
		exts = nil
		for _, e := range strings.Split(os.Getenv("PATHEXT"), string(os.PathListSeparator)) {
			if e != "" {
				exts = append(exts, strings.ToLower(e))
			}
		}
		if len(exts) == 0 {
			exts = []string{".com", ".exe", ".bat", ".cmd"}
		}
		if strings.Contains(strings.ToLower(tool), ".") {
			exts = append([]string{""}, exts...)
		}
	}
	for _, dir := range filepath.SplitList(pathList) {
		if dir == "" {
			continue
		}
		for _, e := range exts {
			candidate := filepath.Join(dir, tool+e)
			st, err := os.Stat(candidate)
			if err != nil || st.IsDir() {
				continue
			}
			if isWindowsHost || st.Mode()&0o111 != 0 {
				return candidate
			}
		}
	}
	return ""
}

// Post write lint reporting only errors this edit introduced when the pre edit content is known
func (s *shellFileOps) checkLintDelta(path string, preContent, postContent *string) *LintResult {
	post := s.checkLint(path, postContent)
	if post.Success || post.Skipped || preContent == nil {
		return post
	}
	pre := s.checkLint(path, preContent)
	if pre.Success || pre.Skipped || pre.Output == "" {
		return post
	}
	preLines := map[string]bool{}
	for _, ln := range pySplitLines(pre.Output) {
		if t := pyStrip(ln); t != "" {
			preLines[t] = true
		}
	}
	var postLines []string
	for _, ln := range pySplitLines(post.Output) {
		if t := pyStrip(ln); t != "" && !preLines[t] {
			postLines = append(postLines, ln)
		}
	}
	if len(postLines) == 0 {
		return &LintResult{Success: false, Output: post.Output, Message: "Pre-existing lint errors — this edit didn't introduce new ones but the file is still broken."}
	}
	return &LintResult{Success: false, Output: "New lint errors introduced by this edit " +
		"(pre-existing errors filtered out):\n" + strings.Join(postLines, "\n")}
}

// True when the backend's files are the host's, the only place host side LSP servers can see them
func (s *shellFileOps) lspLocalOnly() bool {
	return EnvUsesHostFilesystem(s.terminalEnv)
}

// True on a local backend whose cwd lies outside every trusted workspace
func (s *shellFileOps) localWorkspaceUntrusted() bool {
	if !s.lspLocalOnly() {
		return false
	}
	var raw any
	if lspCfg, ok := cfgTree(s.env)["lsp"].(map[string]any); ok {
		raw = lspCfg["trusted_workspaces"]
	}
	trusted := lspParseTrustedWorkspaces(raw)
	return !lspIsTrustedWorkspace(s.liveCwd(), trusted, lspOperatorWorkspaceRoots(s.env))
}

// lspService is the language server manager surface the write path consults
type lspService interface {
	HandlesExtension(ext string) (bool, error)
	EnabledFor(path string) (bool, error)
	// Running post write check of the edit, nil when LSP does not handle the file
	StartEditCheck(path string, preText, postText *string, owner string, carry []lspNote) (lspEditFuture, error)
}

// lspNote is one undelivered diagnostics note with the pre edit text it was diffed against
type lspNote struct {
	Text         string
	BaselineText *string
}

// lspEditCheckResult is one post write verdict, HasVerdict false meaning the check reached none
type lspEditCheckResult struct {
	Diagnostics  []map[string]any
	HasVerdict   bool
	BaselineText *string
	Superseded   bool
	Carry        []lspNote
}

// lspEditFuture is one running post write check
type lspEditFuture interface {
	// Verdict within the timeout, ready false when the check is still running
	Wait(timeout time.Duration) (res *lspEditCheckResult, ready bool, err error)
	// Runs fn once on the LSP loop when the check finishes or is cancelled
	OnDone(fn func(res *lspEditCheckResult, cancelled bool, err error))
}

// Active LSP service, nil on a non host backend or any failure
func (s *shellFileOps) lspService() (svc lspService) {
	if !s.lspLocalOnly() {
		return nil
	}
	defer func() {
		if recover() != nil {
			svc = nil
		}
	}()
	return lspServiceFor(s.env)
}

// Whether the active service or the static server registry claims an extension
func (s *shellFileOps) lspHandlesExtension(ext string) (handled bool) {
	if ext == "" {
		return false
	}
	defer func() {
		if recover() != nil {
			handled = false
		}
	}()
	if svc := s.lspService(); svc != nil {
		ok, err := svc.HandlesExtension(ext)
		return err == nil && ok
	}
	return lspStaticRegistryHandles(pyLower(ext))
}

// Whether a tsconfig.json sits in the path's directory or any ancestor, false off the host
func (s *shellFileOps) hasAncestorTsconfig(path string) bool {
	if !s.lspLocalOnly() {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	d := filepath.Dir(abs)
	for {
		if st, err := os.Stat(filepath.Join(d, "tsconfig.json")); err == nil && st.Mode().IsRegular() {
			return true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return false
		}
		d = parent
	}
}

// Whether the active service will run for the path, false on any failure
func (s *shellFileOps) lspWillHandle(path string) (handled bool) {
	svc := s.lspService()
	if svc == nil {
		return false
	}
	defer func() {
		if recover() != nil {
			handled = false
		}
	}()
	ok, err := svc.EnabledFor(path)
	return err == nil && ok
}

// LSP diagnostics this edit introduced, empty when unavailable, clean, or the verdict arrives late
func (s *shellFileOps) maybeLSPDiagnostics(path string, preContent, postContent *string, started time.Time) (text string) {
	svc := s.lspService()
	if svc == nil || !s.lspWillHandle(path) {
		return ""
	}
	defer func() {
		if recover() != nil {
			text = ""
		}
	}()
	absPath, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	owner := lspDeferredCurrentOwner(s.ctx, s.env)
	var carry []lspNote
	if older := lspDeferredTake(owner, absPath); older != nil {
		carry = []lspNote{*older}
	}
	future, err := svc.StartEditCheck(path, preContent, postContent, owner, carry)
	if err != nil {
		return ""
	}
	if future == nil {
		lspDeferredRestore(owner, absPath, carry)
		return ""
	}
	result, ready, err := future.Wait(lspInlineWait(started))
	if err != nil {
		lspDeferredRestore(owner, absPath, carry)
		return ""
	}
	if !ready {
		if owner != "" {
			token := lspDeferredTrackPending(owner)
			future.OnDone(func(res *lspEditCheckResult, cancelled bool, err error) {
				postDeferredVerdict(owner, absPath, res, cancelled, err, carry, token)
			})
		}
		return ""
	}
	if result.Superseded {
		return ""
	}
	if !result.HasVerdict {
		lspDeferredRestore(owner, absPath, result.Carry)
		return ""
	}
	return formatLSPBlock(path, result.Diagnostics, "LSP diagnostics introduced by this edit:\n")
}

// Formatted and truncated diagnostics block, empty when nothing passes the reporter
func formatLSPBlock(path string, diagnostics []map[string]any, header string) (text string) {
	if len(diagnostics) == 0 {
		return ""
	}
	defer func() {
		if recover() != nil {
			text = ""
		}
	}()
	block := lspReportForFile(path, diagnostics)
	if block == "" {
		return ""
	}
	return lspTruncate(header + block)
}

// Done callback queuing a late verdict for the editing session, then releasing a waiting turn end
func postDeferredVerdict(owner, absPath string, res *lspEditCheckResult, cancelled bool, err error, carry []lspNote, token any) {
	defer lspDeferredSettled(owner, token)
	queueLateVerdict(owner, absPath, res, cancelled, err, carry)
}

// Posts a late verdict, a superseded check posting nothing and a failed one handing notes back
func queueLateVerdict(owner, absPath string, res *lspEditCheckResult, cancelled bool, err error, carry []lspNote) {
	if err != nil || cancelled || res == nil {
		lspDeferredRestore(owner, absPath, carry)
		return
	}
	if res.Superseded {
		return
	}
	if !res.HasVerdict {
		lspDeferredRestore(owner, absPath, res.Carry)
		return
	}
	text := formatLSPBlock(absPath, res.Diagnostics, "LSP diagnostics introduced by your earlier edit of "+absPath+" "+
		"(the language server answered after that tool result returned):\n")
	if text != "" {
		lspDeferredPost(owner, absPath, text, res.BaselineText)
	}
}

// Holds CPython defaults and counts the script module plus the hermes linter frame that sit beneath loads
const (
	tomllibRecursionLimit  = 1000
	tomllibCallerFrames    = 2
	tomllibMaxKeyParts     = tomllibRecursionLimit
	tomllibIntMaxStrDigits = 4300
	tomllibFrozen          = uint8(1)
	tomllibExplicitNest    = uint8(2)
	tomllibKindScalar      = 0
	tomllibKindTable       = 1
	tomllibKindArray       = 2
)

var (
	tomllibTzMu    sync.Mutex
	tomllibTzCache = map[string]struct{}{}
)

type tomllibFailure struct{ text string }

type tomllibNode struct {
	kind  int
	table map[string]*tomllibNode
	items []*tomllibNode
}

type tomllibFlagNode struct {
	flags     uint8
	recursive uint8
	nested    map[string]*tomllibFlagNode
}

type tomllibPending struct {
	key  []string
	flag uint8
}

type tomllibFlags struct {
	root    map[string]*tomllibFlagNode
	pending map[string]tomllibPending
}

type tomllibParser struct {
	src    []rune
	depth  int
	data   *tomllibNode
	flags  *tomllibFlags
	scalar *tomllibNode
}

// Returns the CPython 3.14 TOML loader failure as type name and message, or empty when parsing succeeds
func tomllibLoadsError(content string) (result string) {
	defer func() {
		if r := recover(); r != nil {
			f, ok := r.(tomllibFailure)
			if !ok {
				panic(r)
			}
			result = f.text
		}
	}()
	p := &tomllibParser{
		src:    tomllibDecode(content),
		depth:  tomllibCallerFrames,
		scalar: &tomllibNode{kind: tomllibKindScalar},
	}
	p.loads()
	return ""
}

// Code points of the str with surrogates whole and stray bytes as surrogateescape chars, CRLF pairs folded into LF
func tomllibDecode(s string) []rune {
	out := make([]rune, 0, len(s))
	for i := 0; i < len(s); {
		r, size := pystr.DecodeRune(s[i:])
		if r == '\n' && len(out) > 0 && out[len(out)-1] == '\r' {
			out[len(out)-1] = '\n'
		} else {
			out = append(out, r)
		}
		i += size
	}
	return out
}

// Pushes one Python frame and raises RecursionError once the interpreter limit of frames is exceeded
func (p *tomllibParser) enter() {
	if p.depth >= tomllibRecursionLimit {
		panic(tomllibFailure{"RecursionError: maximum recursion depth exceeded"})
	}
	p.depth++
}

func (p *tomllibParser) leave() { p.depth-- }

// Accounts for a Python frame that runs no further calls such as an init or a generator step
func (p *tomllibParser) call() {
	p.enter()
	p.leave()
}

// Builds TOMLDecodeError after pushing its init frame, with the line and column suffix the Python constructor computes
func (p *tomllibParser) decodeError(msg string, pos int) tomllibFailure {
	p.enter()
	end := min(pos, len(p.src))
	lineno := 1
	last := -1
	for i := 0; i < end; i++ {
		if p.src[i] == '\n' {
			lineno++
			last = i
		}
	}
	coord := "end of document"
	if pos < len(p.src) {
		colno := pos + 1
		if lineno != 1 {
			colno = pos - last
		}
		coord = "line " + strconv.Itoa(lineno) + ", column " + strconv.Itoa(colno)
	}
	return tomllibFailure{"TOMLDecodeError: " + msg + " (at " + coord + ")"}
}

func (p *tomllibParser) at(pos int) rune {
	if pos < len(p.src) {
		return p.src[pos]
	}
	return -1
}

// Returns the clamped rune range from start to stop the way a Python string slice clamps indices
func (p *tomllibParser) slice(start, stop int) []rune {
	stop = min(stop, len(p.src))
	start = min(start, stop)
	return p.src[start:stop]
}

func (p *tomllibParser) startsWith(pos int, s string) bool {
	if pos+len(s) > len(p.src) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if p.src[pos+i] != rune(s[i]) {
			return false
		}
	}
	return true
}

// Finds the first occurrence of the ASCII needle at or after pos, returning minus one when absent
func (p *tomllibParser) index(needle string, pos int) int {
	for i := pos; i+len(needle) <= len(p.src); i++ {
		if p.startsWith(i, needle) {
			return i
		}
	}
	return -1
}

func tomllibIsCtrl(r rune) bool { return r < 32 || r == 127 }

func tomllibIllegalBasic(r rune) bool { return tomllibIsCtrl(r) && r != '\t' }

func tomllibIllegalMultiline(r rune) bool { return tomllibIsCtrl(r) && r != '\t' && r != '\n' }

func tomllibIsWS(r rune) bool { return r == ' ' || r == '\t' }

func tomllibIsWSOrNewline(r rune) bool { return r == ' ' || r == '\t' || r == '\n' }

func tomllibIsBareKey(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
}

func tomllibIsKeyInitial(r rune) bool { return tomllibIsBareKey(r) || r == '"' || r == '\'' }

func tomllibIsHex(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

func tomllibIsDigit(r rune) bool { return r >= '0' && r <= '9' }

func tomllibIsBin(r rune) bool { return r == '0' || r == '1' }

func tomllibIsOct(r rune) bool { return r >= '0' && r <= '7' }

// Encodes code points three bytes each so surrogates stay distinct from every other key code point
func tomllibPack(rs []rune) string {
	b := make([]byte, 0, 3*len(rs))
	for _, r := range rs {
		b = append(b, byte(r>>16), byte(r>>8), byte(r))
	}
	return string(b)
}

func tomllibUnpack(s string) []rune {
	rs := make([]rune, 0, len(s)/3)
	for i := 0; i+2 < len(s); i += 3 {
		rs = append(rs, rune(s[i])<<16|rune(s[i+1])<<8|rune(s[i+2]))
	}
	return rs
}

// Builds a fresh key holding the parts of both inputs so later appends never alias shared storage
func tomllibJoinKey(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	out = append(out, a...)
	return append(out, b...)
}

// Produces a collision free identity string for a key and flag pair used by the pending set
func tomllibPendingID(key []string, flag uint8) string {
	var b strings.Builder
	for _, k := range key {
		b.WriteString(strconv.Itoa(len(k)))
		b.WriteByte('.')
		b.WriteString(k)
	}
	b.WriteByte('#')
	b.WriteByte('0' + flag)
	return b.String()
}

// Reports printability using the Python 3.14 Unicode 16 tables rather than the Go unicode package
func tomllibIsPrintable(r rune) bool {
	lo, hi := 0, len(tomllibNonPrintable)/2
	for lo < hi {
		mid := (lo + hi) / 2
		if uint32(r) > tomllibNonPrintable[2*mid+1] {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo == len(tomllibNonPrintable)/2 || uint32(r) < tomllibNonPrintable[2*lo]
}

func tomllibHex(b *strings.Builder, prefix string, v rune, width int) {
	h := strconv.FormatUint(uint64(v), 16)
	b.WriteString(prefix)
	b.WriteString(strings.Repeat("0", width-len(h)))
	b.WriteString(h)
}

// Renders runes exactly like Python str repr including quote choice and escapes for nonprintable points
func tomllibRepr(rs []rune) string {
	squote, dquote := false, false
	for _, r := range rs {
		switch r {
		case '\'':
			squote = true
		case '"':
			dquote = true
		}
	}
	quote := '\''
	if squote && !dquote {
		quote = '"'
	}
	var b strings.Builder
	b.WriteRune(quote)
	for _, r := range rs {
		switch {
		case r == quote || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			tomllibHex(&b, `\x`, r, 2)
		case r < 0x7f:
			b.WriteRune(r)
		case tomllibIsPrintable(r):
			b.WriteRune(r)
		case r < 0x100:
			tomllibHex(&b, `\x`, r, 2)
		case r < 0x10000:
			tomllibHex(&b, `\u`, r, 4)
		default:
			tomllibHex(&b, `\U`, r, 8)
		}
	}
	b.WriteRune(quote)
	return b.String()
}

// Renders a key exactly like the Python repr of a tuple of strings including the singleton comma
func tomllibReprKey(key []string) string {
	var b strings.Builder
	b.WriteByte('(')
	for i, k := range key {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(tomllibRepr(tomllibUnpack(k)))
	}
	if len(key) == 1 {
		b.WriteByte(',')
	}
	b.WriteByte(')')
	return b.String()
}

func tomllibNewTable() *tomllibNode {
	return &tomllibNode{kind: tomllibKindTable, table: map[string]*tomllibNode{}}
}

func (p *tomllibParser) newNestedDict() *tomllibNode {
	p.call()
	return tomllibNewTable()
}

func (p *tomllibParser) newFlags() *tomllibFlags {
	p.call()
	return &tomllibFlags{root: map[string]*tomllibFlagNode{}, pending: map[string]tomllibPending{}}
}

func (p *tomllibParser) addPending(f *tomllibFlags, key []string, flag uint8) {
	p.enter()
	defer p.leave()
	f.pending[tomllibPendingID(key, flag)] = tomllibPending{key: key, flag: flag}
}

func (p *tomllibParser) finalizePending(f *tomllibFlags) {
	p.enter()
	defer p.leave()
	for _, e := range f.pending {
		p.flagsSet(f, e.key, e.flag, false)
	}
	clear(f.pending)
}

func (p *tomllibParser) unsetAll(f *tomllibFlags, key []string) {
	p.enter()
	defer p.leave()
	cont := f.root
	for _, k := range key[:len(key)-1] {
		n, ok := cont[k]
		if !ok {
			return
		}
		cont = n.nested
	}
	delete(cont, key[len(key)-1])
}

func (p *tomllibParser) flagsSet(f *tomllibFlags, key []string, flag uint8, recursive bool) {
	p.enter()
	defer p.leave()
	cont := f.root
	for _, k := range key[:len(key)-1] {
		cont = tomllibFlagChild(cont, k).nested
	}
	n := tomllibFlagChild(cont, key[len(key)-1])
	if recursive {
		n.recursive |= flag
	} else {
		n.flags |= flag
	}
}

func tomllibFlagChild(cont map[string]*tomllibFlagNode, k string) *tomllibFlagNode {
	n, ok := cont[k]
	if !ok {
		n = &tomllibFlagNode{nested: map[string]*tomllibFlagNode{}}
		cont[k] = n
	}
	return n
}

func (p *tomllibParser) flagsIs(f *tomllibFlags, key []string, flag uint8) bool {
	p.enter()
	defer p.leave()
	if len(key) == 0 {
		return false
	}
	cont := f.root
	for _, k := range key[:len(key)-1] {
		n, ok := cont[k]
		if !ok {
			return false
		}
		if n.recursive&flag != 0 {
			return true
		}
		cont = n.nested
	}
	if n, ok := cont[key[len(key)-1]]; ok {
		return (n.flags|n.recursive)&flag != 0
	}
	return false
}

// Walks or creates tables along key returning nil where Python raises KeyError for a non table value
func (p *tomllibParser) getOrCreateNest(root *tomllibNode, key []string, accessLists bool) *tomllibNode {
	p.enter()
	defer p.leave()
	cont := root
	for _, k := range key {
		child, ok := cont.table[k]
		if !ok {
			child = tomllibNewTable()
			cont.table[k] = child
		}
		cont = child
		if accessLists && cont.kind == tomllibKindArray {
			if len(cont.items) == 0 {
				panic(tomllibFailure{"IndexError: list index out of range"})
			}
			cont = cont.items[len(cont.items)-1]
		}
		if cont.kind != tomllibKindTable {
			return nil
		}
	}
	return cont
}

func (p *tomllibParser) appendNestToList(key []string) bool {
	p.enter()
	defer p.leave()
	cont := p.getOrCreateNest(p.data, key[:len(key)-1], true)
	if cont == nil {
		return false
	}
	last := key[len(key)-1]
	if list, ok := cont.table[last]; ok {
		if list.kind != tomllibKindArray {
			return false
		}
		list.items = append(list.items, tomllibNewTable())
	} else {
		cont.table[last] = &tomllibNode{kind: tomllibKindArray, items: []*tomllibNode{tomllibNewTable()}}
	}
	return true
}

// Parses statements one line at a time mirroring the order of every check in the Python loop
func (p *tomllibParser) loads() {
	p.enter()
	defer p.leave()
	src := p.src
	pos := 0
	p.newOutput()
	var header []string
	p.makeSafeParseFloat()
	for {
		pos = p.skipChars(pos, tomllibIsWS)
		if pos >= len(src) {
			break
		}
		char := src[pos]
		if char == '\n' {
			pos++
			continue
		}
		if tomllibIsKeyInitial(char) {
			pos = p.keyValueRule(pos, header)
			pos = p.skipChars(pos, tomllibIsWS)
		} else if char == '[' {
			second := p.at(pos + 1)
			p.finalizePending(p.flags)
			if second == '[' {
				pos, header = p.createListRule(pos)
			} else {
				pos, header = p.createDictRule(pos)
			}
			pos = p.skipChars(pos, tomllibIsWS)
		} else if char != '#' {
			panic(p.decodeError("Invalid statement", pos))
		}
		pos = p.skipComment(pos)
		if pos >= len(src) {
			break
		}
		if src[pos] != '\n' {
			panic(p.decodeError("Expected newline or end of document after a statement", pos))
		}
		pos++
	}
}

func (p *tomllibParser) makeSafeParseFloat() { p.call() }

// Steps the generator of relative container keys, which pushes one frame per step including the final one
func (p *tomllibParser) contKeysNext() { p.call() }

func (p *tomllibParser) matchToLocaltime() { p.call() }

func (p *tomllibParser) newOutput() {
	p.enter()
	defer p.leave()
	p.data = p.newNestedDict()
	p.flags = p.newFlags()
}

func (p *tomllibParser) skipChars(pos int, in func(rune) bool) int {
	p.enter()
	defer p.leave()
	for pos < len(p.src) && in(p.src[pos]) {
		pos++
	}
	return pos
}

// Returns the index of expect while rejecting illegal characters before it and optionally a missing terminator
func (p *tomllibParser) skipUntil(pos int, expect string, errorOn func(rune) bool, errorOnEOF bool) int {
	p.enter()
	defer p.leave()
	newPos := p.index(expect, pos)
	if newPos < 0 {
		newPos = len(p.src)
		if errorOnEOF {
			panic(p.decodeError("Expected "+tomllibRepr([]rune(expect)), newPos))
		}
	}
	for i := pos; i < newPos; i++ {
		if errorOn(p.src[i]) {
			panic(p.decodeError("Found invalid character "+tomllibRepr([]rune{p.src[i]}), i))
		}
	}
	return newPos
}

func (p *tomllibParser) skipComment(pos int) int {
	p.enter()
	defer p.leave()
	if p.at(pos) == '#' {
		return p.skipUntil(pos+1, "\n", tomllibIllegalBasic, false)
	}
	return pos
}

func (p *tomllibParser) skipCommentsAndArrayWS(pos int) int {
	p.enter()
	defer p.leave()
	for {
		before := pos
		pos = p.skipChars(pos, tomllibIsWSOrNewline)
		pos = p.skipComment(pos)
		if pos == before {
			return pos
		}
	}
}

func (p *tomllibParser) createDictRule(pos int) (int, []string) {
	p.enter()
	defer p.leave()
	pos++
	pos = p.skipChars(pos, tomllibIsWS)
	pos, key := p.parseKey(pos)
	if p.flagsIs(p.flags, key, tomllibExplicitNest) || p.flagsIs(p.flags, key, tomllibFrozen) {
		panic(p.decodeError("Cannot declare "+tomllibReprKey(key)+" twice", pos))
	}
	p.flagsSet(p.flags, key, tomllibExplicitNest, false)
	if p.getOrCreateNest(p.data, key, true) == nil {
		panic(p.decodeError("Cannot overwrite a value", pos))
	}
	if !p.startsWith(pos, "]") {
		panic(p.decodeError("Expected ']' at the end of a table declaration", pos))
	}
	return pos + 1, key
}

func (p *tomllibParser) createListRule(pos int) (int, []string) {
	p.enter()
	defer p.leave()
	pos += 2
	pos = p.skipChars(pos, tomllibIsWS)
	pos, key := p.parseKey(pos)
	if p.flagsIs(p.flags, key, tomllibFrozen) {
		panic(p.decodeError("Cannot mutate immutable namespace "+tomllibReprKey(key), pos))
	}
	p.unsetAll(p.flags, key)
	p.flagsSet(p.flags, key, tomllibExplicitNest, false)
	if !p.appendNestToList(key) {
		panic(p.decodeError("Cannot overwrite a value", pos))
	}
	if !p.startsWith(pos, "]]") {
		panic(p.decodeError("Expected ']]' at the end of an array declaration", pos))
	}
	return pos + 2, key
}

// Assigns a dotted key value under the current header enforcing namespace, mutability and overwrite rules
func (p *tomllibParser) keyValueRule(pos int, header []string) int {
	p.enter()
	defer p.leave()
	pos, key, value := p.parseKeyValuePair(pos)
	keyParent, keyStem := key[:len(key)-1], key[len(key)-1]
	absKeyParent := tomllibJoinKey(header, keyParent)
	for i := 1; i < len(key); i++ {
		p.contKeysNext()
		contKey := tomllibJoinKey(header, key[:i])
		if p.flagsIs(p.flags, contKey, tomllibExplicitNest) {
			panic(p.decodeError("Cannot redefine namespace "+tomllibReprKey(contKey), pos))
		}
		p.addPending(p.flags, contKey, tomllibExplicitNest)
	}
	p.contKeysNext()
	if p.flagsIs(p.flags, absKeyParent, tomllibFrozen) {
		panic(p.decodeError("Cannot mutate immutable namespace "+tomllibReprKey(absKeyParent), pos))
	}
	nest := p.getOrCreateNest(p.data, absKeyParent, true)
	if nest == nil {
		panic(p.decodeError("Cannot overwrite a value", pos))
	}
	if _, ok := nest.table[keyStem]; ok {
		panic(p.decodeError("Cannot overwrite a value", pos))
	}
	if value.kind != tomllibKindScalar {
		p.flagsSet(p.flags, tomllibJoinKey(header, key), tomllibFrozen, true)
	}
	nest.table[keyStem] = value
	return pos
}

func (p *tomllibParser) parseKeyValuePair(pos int) (int, []string, *tomllibNode) {
	p.enter()
	defer p.leave()
	pos, key := p.parseKey(pos)
	if p.at(pos) != '=' {
		panic(p.decodeError("Expected '=' after a key in a key/value pair", pos))
	}
	pos++
	pos = p.skipChars(pos, tomllibIsWS)
	pos, value := p.parseValue(pos)
	return pos, key, value
}

func (p *tomllibParser) parseKey(pos int) (int, []string) {
	p.enter()
	defer p.leave()
	pos, part := p.parseKeyPart(pos)
	key := []string{part}
	pos = p.skipChars(pos, tomllibIsWS)
	for {
		if p.at(pos) != '.' {
			return pos, key
		}
		pos++
		pos = p.skipChars(pos, tomllibIsWS)
		pos, part = p.parseKeyPart(pos)
		key = append(key, part)
		if len(key) > tomllibMaxKeyParts {
			panic(tomllibFailure{"RecursionError: TOML key has more than the allowed " + strconv.Itoa(tomllibMaxKeyParts) + " parts"})
		}
		pos = p.skipChars(pos, tomllibIsWS)
	}
}

func (p *tomllibParser) parseKeyPart(pos int) (int, string) {
	p.enter()
	defer p.leave()
	char := p.at(pos)
	if char >= 0 && tomllibIsBareKey(char) {
		start := pos
		pos = p.skipChars(pos, tomllibIsBareKey)
		return pos, tomllibPack(p.src[start:pos])
	}
	if char == '\'' {
		pos, s := p.parseLiteralStr(pos)
		return pos, tomllibPack(s)
	}
	if char == '"' {
		pos, s := p.parseOneLineBasicStr(pos)
		return pos, tomllibPack(s)
	}
	panic(p.decodeError("Invalid initial character for a key part", pos))
}

func (p *tomllibParser) parseOneLineBasicStr(pos int) (int, []rune) {
	p.enter()
	defer p.leave()
	pos++
	return p.parseBasicStr(pos, false)
}

func (p *tomllibParser) parseArray(pos int) (int, *tomllibNode) {
	p.enter()
	defer p.leave()
	pos++
	array := &tomllibNode{kind: tomllibKindArray}
	pos = p.skipCommentsAndArrayWS(pos)
	if p.startsWith(pos, "]") {
		return pos + 1, array
	}
	for {
		var val *tomllibNode
		pos, val = p.parseValue(pos)
		array.items = append(array.items, val)
		pos = p.skipCommentsAndArrayWS(pos)
		c := p.at(pos)
		if c == ']' {
			return pos + 1, array
		}
		if c != ',' {
			panic(p.decodeError("Unclosed array", pos))
		}
		pos++
		pos = p.skipCommentsAndArrayWS(pos)
		if p.startsWith(pos, "]") {
			return pos + 1, array
		}
	}
}

// Parses a one line inline table with its own namespace flags and duplicate key detection
func (p *tomllibParser) parseInlineTable(pos int) (int, *tomllibNode) {
	p.enter()
	defer p.leave()
	pos++
	nested := p.newNestedDict()
	flags := p.newFlags()
	pos = p.skipChars(pos, tomllibIsWS)
	if p.startsWith(pos, "}") {
		return pos + 1, nested
	}
	for {
		var key []string
		var value *tomllibNode
		pos, key, value = p.parseKeyValuePair(pos)
		keyParent, keyStem := key[:len(key)-1], key[len(key)-1]
		if p.flagsIs(flags, key, tomllibFrozen) {
			panic(p.decodeError("Cannot mutate immutable namespace "+tomllibReprKey(key), pos))
		}
		nest := p.getOrCreateNest(nested, keyParent, false)
		if nest == nil {
			panic(p.decodeError("Cannot overwrite a value", pos))
		}
		if _, ok := nest.table[keyStem]; ok {
			panic(p.decodeError("Duplicate inline table key "+tomllibRepr(tomllibUnpack(keyStem)), pos))
		}
		nest.table[keyStem] = value
		pos = p.skipChars(pos, tomllibIsWS)
		c := p.at(pos)
		if c == '}' {
			return pos + 1, nested
		}
		if c != ',' {
			panic(p.decodeError("Unclosed inline table", pos))
		}
		if value.kind != tomllibKindScalar {
			p.flagsSet(flags, key, tomllibFrozen, true)
		}
		pos++
		pos = p.skipChars(pos, tomllibIsWS)
	}
}

// Decodes one backslash escape, including the multiline line ending backslash form that trims following whitespace and newlines
func (p *tomllibParser) parseBasicStrEscape(pos int, multiline bool) (int, []rune) {
	p.enter()
	defer p.leave()
	escapeID := string(p.slice(pos, pos+2))
	pos += 2
	if multiline && (escapeID == "\\ " || escapeID == "\\\t" || escapeID == "\\\n") {
		if escapeID != "\\\n" {
			pos = p.skipChars(pos, tomllibIsWS)
			if pos >= len(p.src) {
				return pos, nil
			}
			if p.src[pos] != '\n' {
				panic(p.decodeError("Unescaped '\\' in a string", pos))
			}
			pos++
		}
		pos = p.skipChars(pos, tomllibIsWSOrNewline)
		return pos, nil
	}
	switch escapeID {
	case "\\u":
		return p.parseHexChar(pos, 4)
	case "\\U":
		return p.parseHexChar(pos, 8)
	case "\\b":
		return pos, []rune{'\b'}
	case "\\t":
		return pos, []rune{'\t'}
	case "\\n":
		return pos, []rune{'\n'}
	case "\\f":
		return pos, []rune{'\f'}
	case "\\r":
		return pos, []rune{'\r'}
	case "\\\"":
		return pos, []rune{'"'}
	case "\\\\":
		return pos, []rune{'\\'}
	}
	panic(p.decodeError("Unescaped '\\' in a string", pos))
}

func (p *tomllibParser) parseBasicStrEscapeMultiline(pos int) (int, []rune) {
	p.enter()
	defer p.leave()
	return p.parseBasicStrEscape(pos, true)
}

func (p *tomllibParser) parseHexChar(pos, hexLen int) (int, []rune) {
	p.enter()
	defer p.leave()
	hexStr := p.slice(pos, pos+hexLen)
	valid := len(hexStr) == hexLen
	for _, r := range hexStr {
		valid = valid && tomllibIsHex(r)
	}
	if !valid {
		panic(p.decodeError("Invalid hex value", pos))
	}
	v, _ := strconv.ParseUint(string(hexStr), 16, 64)
	pos += hexLen
	if !p.isUnicodeScalarValue(v) {
		panic(p.decodeError("Escaped character is not a Unicode scalar value", pos))
	}
	return pos, []rune{rune(v)}
}

func (p *tomllibParser) isUnicodeScalarValue(v uint64) bool {
	p.call()
	return v <= 55295 || (v >= 57344 && v <= 1114111)
}

func (p *tomllibParser) parseLiteralStr(pos int) (int, []rune) {
	p.enter()
	defer p.leave()
	pos++
	start := pos
	pos = p.skipUntil(pos, "'", tomllibIllegalBasic, true)
	return pos + 1, p.src[start:pos]
}

// Parses a triple quoted string and absorbs up to two extra closing delimiters into the content
func (p *tomllibParser) parseMultilineStr(pos int, literal bool) int {
	p.enter()
	defer p.leave()
	pos += 3
	if p.startsWith(pos, "\n") {
		pos++
	}
	delim := "\""
	if literal {
		delim = "'"
		pos = p.skipUntil(pos, "'''", tomllibIllegalMultiline, true) + 3
	} else {
		pos, _ = p.parseBasicStr(pos, true)
	}
	if !p.startsWith(pos, delim) {
		return pos
	}
	pos++
	if !p.startsWith(pos, delim) {
		return pos
	}
	return pos + 1
}

// Scans a basic string, decoding escapes and rejecting control characters, until the closing quote or end of input
func (p *tomllibParser) parseBasicStr(pos int, multiline bool) (int, []rune) {
	p.enter()
	defer p.leave()
	errorOn := tomllibIllegalBasic
	if multiline {
		errorOn = tomllibIllegalMultiline
	}
	var result []rune
	start := pos
	for {
		if pos >= len(p.src) {
			panic(p.decodeError("Unterminated string", pos))
		}
		char := p.src[pos]
		if char == '"' {
			if !multiline {
				return pos + 1, append(result, p.src[start:pos]...)
			}
			if p.startsWith(pos, `"""`) {
				return pos + 3, append(result, p.src[start:pos]...)
			}
			pos++
			continue
		}
		if char == '\\' {
			result = append(result, p.src[start:pos]...)
			var esc []rune
			if multiline {
				pos, esc = p.parseBasicStrEscapeMultiline(pos)
			} else {
				pos, esc = p.parseBasicStrEscape(pos, false)
			}
			result = append(result, esc...)
			start = pos
			continue
		}
		if errorOn(char) {
			panic(p.decodeError("Illegal character "+tomllibRepr([]rune{char}), pos))
		}
		pos++
	}
}

// Dispatches on the first character then tries datetime, time, number and special float forms in order
func (p *tomllibParser) parseValue(pos int) (int, *tomllibNode) {
	p.enter()
	defer p.leave()
	char := p.at(pos)
	switch char {
	case '"':
		if p.startsWith(pos, `"""`) {
			return p.parseMultilineStr(pos, false), p.scalar
		}
		pos, _ = p.parseOneLineBasicStr(pos)
		return pos, p.scalar
	case '\'':
		if p.startsWith(pos, "'''") {
			return p.parseMultilineStr(pos, true), p.scalar
		}
		pos, _ = p.parseLiteralStr(pos)
		return pos, p.scalar
	case 't':
		if p.startsWith(pos, "true") {
			return pos + 4, p.scalar
		}
	case 'f':
		if p.startsWith(pos, "false") {
			return pos + 5, p.scalar
		}
	case '[':
		return p.parseArray(pos)
	case '{':
		return p.parseInlineTable(pos)
	}
	if end, offset, ok := p.matchDatetime(pos); ok {
		if !p.matchToDatetime(pos, offset) {
			panic(p.decodeError("Invalid date or datetime", pos))
		}
		return end, p.scalar
	}
	if end, ok := p.matchTime(pos); ok {
		p.matchToLocaltime()
		return end, p.scalar
	}
	if end, decimalInt, ok := p.matchNumber(pos); ok {
		p.matchToNumber(pos, end, decimalInt)
		return end, p.scalar
	}
	switch string(p.slice(pos, pos+3)) {
	case "inf", "nan":
		return pos + 3, p.scalar
	}
	switch string(p.slice(pos, pos+4)) {
	case "-inf", "+inf", "-nan", "+nan":
		return pos + 4, p.scalar
	}
	panic(p.decodeError("Invalid value", pos))
}

func (p *tomllibParser) inRange(pos int, lo, hi rune) bool {
	r := p.at(pos)
	return r >= lo && r <= hi
}

// Matches the two digit hour group of the time pattern accepting 00 through 23 only
func (p *tomllibParser) matchHour(pos int) bool {
	return (p.inRange(pos, '0', '1') && p.inRange(pos+1, '0', '9')) || (p.at(pos) == '2' && p.inRange(pos+1, '0', '3'))
}

func (p *tomllibParser) matchMinute(pos int) bool {
	return p.inRange(pos, '0', '5') && p.inRange(pos+1, '0', '9')
}

// Matches the time pattern with an optional fraction whose digits beyond six are consumed and dropped
func (p *tomllibParser) matchTime(pos int) (int, bool) {
	if !p.matchHour(pos) || p.at(pos+2) != ':' || !p.matchMinute(pos+3) || p.at(pos+5) != ':' || !p.matchMinute(pos+6) {
		return 0, false
	}
	end := pos + 8
	if p.at(end) == '.' && p.inRange(end+1, '0', '9') {
		end += 2
		for p.inRange(end, '0', '9') {
			end++
		}
	}
	return end, true
}

// Matches the date pattern with optional time and offset, returning the end and the offset sign index
func (p *tomllibParser) matchDatetime(pos int) (end, offset int, ok bool) {
	for i := 0; i < 4; i++ {
		if !p.inRange(pos+i, '0', '9') {
			return 0, -1, false
		}
	}
	if p.at(pos+4) != '-' || p.at(pos+7) != '-' {
		return 0, -1, false
	}
	month := (p.at(pos+5) == '0' && p.inRange(pos+6, '1', '9')) || (p.at(pos+5) == '1' && p.inRange(pos+6, '0', '2'))
	day := (p.at(pos+8) == '0' && p.inRange(pos+9, '1', '9')) || (p.inRange(pos+8, '1', '2') && p.inRange(pos+9, '0', '9')) || (p.at(pos+8) == '3' && p.inRange(pos+9, '0', '1'))
	if !month || !day {
		return 0, -1, false
	}
	end = pos + 10
	sep := p.at(end)
	if sep != 'T' && sep != 't' && sep != ' ' {
		return end, -1, true
	}
	tend, timeOK := p.matchTime(end + 1)
	if !timeOK {
		return end, -1, true
	}
	switch p.at(tend) {
	case 'Z', 'z':
		return tend + 1, -1, true
	case '+', '-':
		if p.matchHour(tend+1) && p.at(tend+3) == ':' && p.matchMinute(tend+4) {
			return tend + 6, tend, true
		}
	}
	return tend, -1, true
}

func tomllibDigitsAt(src []rune, pos, n int) int {
	v := 0
	for i := 0; i < n; i++ {
		v = v*10 + int(src[pos+i]-'0')
	}
	return v
}

// Validates the matched date the way the datetime constructors do and calls the offset cache first
func (p *tomllibParser) matchToDatetime(pos, offset int) bool {
	p.enter()
	defer p.leave()
	src := p.src
	year := tomllibDigitsAt(src, pos, 4)
	month := tomllibDigitsAt(src, pos+5, 2)
	day := tomllibDigitsAt(src, pos+8, 2)
	if offset >= 0 {
		p.cachedTz(string(src[offset+1:offset+3]) + string(src[offset+4:offset+6]) + string(src[offset]))
	}
	if year < 1 {
		return false
	}
	days := [13]int{0, 31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}
	if year%4 == 0 && (year%100 != 0 || year%400 == 0) {
		days[2] = 29
	}
	return day <= days[month]
}

// Pushes the cached offset helper frame only on a cache miss, as the process wide lru_cache does
func (p *tomllibParser) cachedTz(key string) {
	tomllibTzMu.Lock()
	_, hit := tomllibTzCache[key]
	tomllibTzMu.Unlock()
	if hit {
		return
	}
	p.call()
	tomllibTzMu.Lock()
	tomllibTzCache[key] = struct{}{}
	tomllibTzMu.Unlock()
}

// Advances over a run of digits satisfying the class predicate where single underscores may separate consecutive digits
func (p *tomllibParser) matchDigitRun(pos int, class func(rune) bool) int {
	for {
		if p.at(pos) == '_' && pos+1 < len(p.src) && class(p.src[pos+1]) {
			pos += 2
		} else if pos < len(p.src) && class(p.src[pos]) {
			pos++
		} else {
			return pos
		}
	}
}

// Matches prefixed integers first, then signed decimals, reporting whether the result is a decimal integer
func (p *tomllibParser) matchNumber(pos int) (end int, decimalInt, ok bool) {
	if p.at(pos) == '0' {
		for _, alt := range []struct {
			prefix rune
			class  func(rune) bool
		}{{'x', tomllibIsHex}, {'b', tomllibIsBin}, {'o', tomllibIsOct}} {
			if p.at(pos+1) == alt.prefix && pos+2 < len(p.src) && alt.class(p.src[pos+2]) {
				return p.matchDigitRun(pos+3, alt.class), false, true
			}
		}
	}
	i := pos
	if c := p.at(i); c == '+' || c == '-' {
		i++
	}
	switch {
	case p.at(i) == '0':
		i++
	case p.inRange(i, '1', '9'):
		i = p.matchDigitRun(i+1, tomllibIsDigit)
	default:
		return 0, false, false
	}
	intEnd := i
	if p.at(i) == '.' && p.inRange(i+1, '0', '9') {
		i = p.matchDigitRun(i+2, tomllibIsDigit)
	}
	if c := p.at(i); c == 'e' || c == 'E' {
		j := i + 1
		if s := p.at(j); s == '+' || s == '-' {
			j++
		}
		if p.inRange(j, '0', '9') {
			i = p.matchDigitRun(j+1, tomllibIsDigit)
		}
	}
	return i, i == intEnd, true
}

// Converts a matched number and raises the integer digit limit ValueError for long decimal literals
func (p *tomllibParser) matchToNumber(pos, end int, decimalInt bool) {
	p.enter()
	defer p.leave()
	if !decimalInt {
		return
	}
	digits := 0
	for _, r := range p.src[pos:end] {
		if tomllibIsDigit(r) {
			digits++
		}
	}
	if digits > tomllibIntMaxStrDigits {
		panic(tomllibFailure{"ValueError: Exceeds the limit (" + strconv.Itoa(tomllibIntMaxStrDigits) +
			" digits) for integer string conversion: value has " + strconv.Itoa(digits) +
			" digits; use sys.set_int_max_str_digits() to increase the limit"})
	}
}

// Lists inclusive code point ranges that Python 3.14 str.isprintable rejects under its bundled Unicode 16 database
var tomllibNonPrintable = [...]uint32{
	0x0, 0x1f, 0x7f, 0xa0, 0xad, 0xad, 0x378, 0x379,
	0x380, 0x383, 0x38b, 0x38b, 0x38d, 0x38d, 0x3a2, 0x3a2,
	0x530, 0x530, 0x557, 0x558, 0x58b, 0x58c, 0x590, 0x590,
	0x5c8, 0x5cf, 0x5eb, 0x5ee, 0x5f5, 0x605, 0x61c, 0x61c,
	0x6dd, 0x6dd, 0x70e, 0x70f, 0x74b, 0x74c, 0x7b2, 0x7bf,
	0x7fb, 0x7fc, 0x82e, 0x82f, 0x83f, 0x83f, 0x85c, 0x85d,
	0x85f, 0x85f, 0x86b, 0x86f, 0x88f, 0x896, 0x8e2, 0x8e2,
	0x984, 0x984, 0x98d, 0x98e, 0x991, 0x992, 0x9a9, 0x9a9,
	0x9b1, 0x9b1, 0x9b3, 0x9b5, 0x9ba, 0x9bb, 0x9c5, 0x9c6,
	0x9c9, 0x9ca, 0x9cf, 0x9d6, 0x9d8, 0x9db, 0x9de, 0x9de,
	0x9e4, 0x9e5, 0x9ff, 0xa00, 0xa04, 0xa04, 0xa0b, 0xa0e,
	0xa11, 0xa12, 0xa29, 0xa29, 0xa31, 0xa31, 0xa34, 0xa34,
	0xa37, 0xa37, 0xa3a, 0xa3b, 0xa3d, 0xa3d, 0xa43, 0xa46,
	0xa49, 0xa4a, 0xa4e, 0xa50, 0xa52, 0xa58, 0xa5d, 0xa5d,
	0xa5f, 0xa65, 0xa77, 0xa80, 0xa84, 0xa84, 0xa8e, 0xa8e,
	0xa92, 0xa92, 0xaa9, 0xaa9, 0xab1, 0xab1, 0xab4, 0xab4,
	0xaba, 0xabb, 0xac6, 0xac6, 0xaca, 0xaca, 0xace, 0xacf,
	0xad1, 0xadf, 0xae4, 0xae5, 0xaf2, 0xaf8, 0xb00, 0xb00,
	0xb04, 0xb04, 0xb0d, 0xb0e, 0xb11, 0xb12, 0xb29, 0xb29,
	0xb31, 0xb31, 0xb34, 0xb34, 0xb3a, 0xb3b, 0xb45, 0xb46,
	0xb49, 0xb4a, 0xb4e, 0xb54, 0xb58, 0xb5b, 0xb5e, 0xb5e,
	0xb64, 0xb65, 0xb78, 0xb81, 0xb84, 0xb84, 0xb8b, 0xb8d,
	0xb91, 0xb91, 0xb96, 0xb98, 0xb9b, 0xb9b, 0xb9d, 0xb9d,
	0xba0, 0xba2, 0xba5, 0xba7, 0xbab, 0xbad, 0xbba, 0xbbd,
	0xbc3, 0xbc5, 0xbc9, 0xbc9, 0xbce, 0xbcf, 0xbd1, 0xbd6,
	0xbd8, 0xbe5, 0xbfb, 0xbff, 0xc0d, 0xc0d, 0xc11, 0xc11,
	0xc29, 0xc29, 0xc3a, 0xc3b, 0xc45, 0xc45, 0xc49, 0xc49,
	0xc4e, 0xc54, 0xc57, 0xc57, 0xc5b, 0xc5c, 0xc5e, 0xc5f,
	0xc64, 0xc65, 0xc70, 0xc76, 0xc8d, 0xc8d, 0xc91, 0xc91,
	0xca9, 0xca9, 0xcb4, 0xcb4, 0xcba, 0xcbb, 0xcc5, 0xcc5,
	0xcc9, 0xcc9, 0xcce, 0xcd4, 0xcd7, 0xcdc, 0xcdf, 0xcdf,
	0xce4, 0xce5, 0xcf0, 0xcf0, 0xcf4, 0xcff, 0xd0d, 0xd0d,
	0xd11, 0xd11, 0xd45, 0xd45, 0xd49, 0xd49, 0xd50, 0xd53,
	0xd64, 0xd65, 0xd80, 0xd80, 0xd84, 0xd84, 0xd97, 0xd99,
	0xdb2, 0xdb2, 0xdbc, 0xdbc, 0xdbe, 0xdbf, 0xdc7, 0xdc9,
	0xdcb, 0xdce, 0xdd5, 0xdd5, 0xdd7, 0xdd7, 0xde0, 0xde5,
	0xdf0, 0xdf1, 0xdf5, 0xe00, 0xe3b, 0xe3e, 0xe5c, 0xe80,
	0xe83, 0xe83, 0xe85, 0xe85, 0xe8b, 0xe8b, 0xea4, 0xea4,
	0xea6, 0xea6, 0xebe, 0xebf, 0xec5, 0xec5, 0xec7, 0xec7,
	0xecf, 0xecf, 0xeda, 0xedb, 0xee0, 0xeff, 0xf48, 0xf48,
	0xf6d, 0xf70, 0xf98, 0xf98, 0xfbd, 0xfbd, 0xfcd, 0xfcd,
	0xfdb, 0xfff, 0x10c6, 0x10c6, 0x10c8, 0x10cc, 0x10ce, 0x10cf,
	0x1249, 0x1249, 0x124e, 0x124f, 0x1257, 0x1257, 0x1259, 0x1259,
	0x125e, 0x125f, 0x1289, 0x1289, 0x128e, 0x128f, 0x12b1, 0x12b1,
	0x12b6, 0x12b7, 0x12bf, 0x12bf, 0x12c1, 0x12c1, 0x12c6, 0x12c7,
	0x12d7, 0x12d7, 0x1311, 0x1311, 0x1316, 0x1317, 0x135b, 0x135c,
	0x137d, 0x137f, 0x139a, 0x139f, 0x13f6, 0x13f7, 0x13fe, 0x13ff,
	0x1680, 0x1680, 0x169d, 0x169f, 0x16f9, 0x16ff, 0x1716, 0x171e,
	0x1737, 0x173f, 0x1754, 0x175f, 0x176d, 0x176d, 0x1771, 0x1771,
	0x1774, 0x177f, 0x17de, 0x17df, 0x17ea, 0x17ef, 0x17fa, 0x17ff,
	0x180e, 0x180e, 0x181a, 0x181f, 0x1879, 0x187f, 0x18ab, 0x18af,
	0x18f6, 0x18ff, 0x191f, 0x191f, 0x192c, 0x192f, 0x193c, 0x193f,
	0x1941, 0x1943, 0x196e, 0x196f, 0x1975, 0x197f, 0x19ac, 0x19af,
	0x19ca, 0x19cf, 0x19db, 0x19dd, 0x1a1c, 0x1a1d, 0x1a5f, 0x1a5f,
	0x1a7d, 0x1a7e, 0x1a8a, 0x1a8f, 0x1a9a, 0x1a9f, 0x1aae, 0x1aaf,
	0x1acf, 0x1aff, 0x1b4d, 0x1b4d, 0x1bf4, 0x1bfb, 0x1c38, 0x1c3a,
	0x1c4a, 0x1c4c, 0x1c8b, 0x1c8f, 0x1cbb, 0x1cbc, 0x1cc8, 0x1ccf,
	0x1cfb, 0x1cff, 0x1f16, 0x1f17, 0x1f1e, 0x1f1f, 0x1f46, 0x1f47,
	0x1f4e, 0x1f4f, 0x1f58, 0x1f58, 0x1f5a, 0x1f5a, 0x1f5c, 0x1f5c,
	0x1f5e, 0x1f5e, 0x1f7e, 0x1f7f, 0x1fb5, 0x1fb5, 0x1fc5, 0x1fc5,
	0x1fd4, 0x1fd5, 0x1fdc, 0x1fdc, 0x1ff0, 0x1ff1, 0x1ff5, 0x1ff5,
	0x1fff, 0x200f, 0x2028, 0x202f, 0x205f, 0x206f, 0x2072, 0x2073,
	0x208f, 0x208f, 0x209d, 0x209f, 0x20c1, 0x20cf, 0x20f1, 0x20ff,
	0x218c, 0x218f, 0x242a, 0x243f, 0x244b, 0x245f, 0x2b74, 0x2b75,
	0x2b96, 0x2b96, 0x2cf4, 0x2cf8, 0x2d26, 0x2d26, 0x2d28, 0x2d2c,
	0x2d2e, 0x2d2f, 0x2d68, 0x2d6e, 0x2d71, 0x2d7e, 0x2d97, 0x2d9f,
	0x2da7, 0x2da7, 0x2daf, 0x2daf, 0x2db7, 0x2db7, 0x2dbf, 0x2dbf,
	0x2dc7, 0x2dc7, 0x2dcf, 0x2dcf, 0x2dd7, 0x2dd7, 0x2ddf, 0x2ddf,
	0x2e5e, 0x2e7f, 0x2e9a, 0x2e9a, 0x2ef4, 0x2eff, 0x2fd6, 0x2fef,
	0x3000, 0x3000, 0x3040, 0x3040, 0x3097, 0x3098, 0x3100, 0x3104,
	0x3130, 0x3130, 0x318f, 0x318f, 0x31e6, 0x31ee, 0x321f, 0x321f,
	0xa48d, 0xa48f, 0xa4c7, 0xa4cf, 0xa62c, 0xa63f, 0xa6f8, 0xa6ff,
	0xa7ce, 0xa7cf, 0xa7d2, 0xa7d2, 0xa7d4, 0xa7d4, 0xa7dd, 0xa7f1,
	0xa82d, 0xa82f, 0xa83a, 0xa83f, 0xa878, 0xa87f, 0xa8c6, 0xa8cd,
	0xa8da, 0xa8df, 0xa954, 0xa95e, 0xa97d, 0xa97f, 0xa9ce, 0xa9ce,
	0xa9da, 0xa9dd, 0xa9ff, 0xa9ff, 0xaa37, 0xaa3f, 0xaa4e, 0xaa4f,
	0xaa5a, 0xaa5b, 0xaac3, 0xaada, 0xaaf7, 0xab00, 0xab07, 0xab08,
	0xab0f, 0xab10, 0xab17, 0xab1f, 0xab27, 0xab27, 0xab2f, 0xab2f,
	0xab6c, 0xab6f, 0xabee, 0xabef, 0xabfa, 0xabff, 0xd7a4, 0xd7af,
	0xd7c7, 0xd7ca, 0xd7fc, 0xf8ff, 0xfa6e, 0xfa6f, 0xfada, 0xfaff,
	0xfb07, 0xfb12, 0xfb18, 0xfb1c, 0xfb37, 0xfb37, 0xfb3d, 0xfb3d,
	0xfb3f, 0xfb3f, 0xfb42, 0xfb42, 0xfb45, 0xfb45, 0xfbc3, 0xfbd2,
	0xfd90, 0xfd91, 0xfdc8, 0xfdce, 0xfdd0, 0xfdef, 0xfe1a, 0xfe1f,
	0xfe53, 0xfe53, 0xfe67, 0xfe67, 0xfe6c, 0xfe6f, 0xfe75, 0xfe75,
	0xfefd, 0xff00, 0xffbf, 0xffc1, 0xffc8, 0xffc9, 0xffd0, 0xffd1,
	0xffd8, 0xffd9, 0xffdd, 0xffdf, 0xffe7, 0xffe7, 0xffef, 0xfffb,
	0xfffe, 0xffff, 0x1000c, 0x1000c, 0x10027, 0x10027, 0x1003b, 0x1003b,
	0x1003e, 0x1003e, 0x1004e, 0x1004f, 0x1005e, 0x1007f, 0x100fb, 0x100ff,
	0x10103, 0x10106, 0x10134, 0x10136, 0x1018f, 0x1018f, 0x1019d, 0x1019f,
	0x101a1, 0x101cf, 0x101fe, 0x1027f, 0x1029d, 0x1029f, 0x102d1, 0x102df,
	0x102fc, 0x102ff, 0x10324, 0x1032c, 0x1034b, 0x1034f, 0x1037b, 0x1037f,
	0x1039e, 0x1039e, 0x103c4, 0x103c7, 0x103d6, 0x103ff, 0x1049e, 0x1049f,
	0x104aa, 0x104af, 0x104d4, 0x104d7, 0x104fc, 0x104ff, 0x10528, 0x1052f,
	0x10564, 0x1056e, 0x1057b, 0x1057b, 0x1058b, 0x1058b, 0x10593, 0x10593,
	0x10596, 0x10596, 0x105a2, 0x105a2, 0x105b2, 0x105b2, 0x105ba, 0x105ba,
	0x105bd, 0x105bf, 0x105f4, 0x105ff, 0x10737, 0x1073f, 0x10756, 0x1075f,
	0x10768, 0x1077f, 0x10786, 0x10786, 0x107b1, 0x107b1, 0x107bb, 0x107ff,
	0x10806, 0x10807, 0x10809, 0x10809, 0x10836, 0x10836, 0x10839, 0x1083b,
	0x1083d, 0x1083e, 0x10856, 0x10856, 0x1089f, 0x108a6, 0x108b0, 0x108df,
	0x108f3, 0x108f3, 0x108f6, 0x108fa, 0x1091c, 0x1091e, 0x1093a, 0x1093e,
	0x10940, 0x1097f, 0x109b8, 0x109bb, 0x109d0, 0x109d1, 0x10a04, 0x10a04,
	0x10a07, 0x10a0b, 0x10a14, 0x10a14, 0x10a18, 0x10a18, 0x10a36, 0x10a37,
	0x10a3b, 0x10a3e, 0x10a49, 0x10a4f, 0x10a59, 0x10a5f, 0x10aa0, 0x10abf,
	0x10ae7, 0x10aea, 0x10af7, 0x10aff, 0x10b36, 0x10b38, 0x10b56, 0x10b57,
	0x10b73, 0x10b77, 0x10b92, 0x10b98, 0x10b9d, 0x10ba8, 0x10bb0, 0x10bff,
	0x10c49, 0x10c7f, 0x10cb3, 0x10cbf, 0x10cf3, 0x10cf9, 0x10d28, 0x10d2f,
	0x10d3a, 0x10d3f, 0x10d66, 0x10d68, 0x10d86, 0x10d8d, 0x10d90, 0x10e5f,
	0x10e7f, 0x10e7f, 0x10eaa, 0x10eaa, 0x10eae, 0x10eaf, 0x10eb2, 0x10ec1,
	0x10ec5, 0x10efb, 0x10f28, 0x10f2f, 0x10f5a, 0x10f6f, 0x10f8a, 0x10faf,
	0x10fcc, 0x10fdf, 0x10ff7, 0x10fff, 0x1104e, 0x11051, 0x11076, 0x1107e,
	0x110bd, 0x110bd, 0x110c3, 0x110cf, 0x110e9, 0x110ef, 0x110fa, 0x110ff,
	0x11135, 0x11135, 0x11148, 0x1114f, 0x11177, 0x1117f, 0x111e0, 0x111e0,
	0x111f5, 0x111ff, 0x11212, 0x11212, 0x11242, 0x1127f, 0x11287, 0x11287,
	0x11289, 0x11289, 0x1128e, 0x1128e, 0x1129e, 0x1129e, 0x112aa, 0x112af,
	0x112eb, 0x112ef, 0x112fa, 0x112ff, 0x11304, 0x11304, 0x1130d, 0x1130e,
	0x11311, 0x11312, 0x11329, 0x11329, 0x11331, 0x11331, 0x11334, 0x11334,
	0x1133a, 0x1133a, 0x11345, 0x11346, 0x11349, 0x1134a, 0x1134e, 0x1134f,
	0x11351, 0x11356, 0x11358, 0x1135c, 0x11364, 0x11365, 0x1136d, 0x1136f,
	0x11375, 0x1137f, 0x1138a, 0x1138a, 0x1138c, 0x1138d, 0x1138f, 0x1138f,
	0x113b6, 0x113b6, 0x113c1, 0x113c1, 0x113c3, 0x113c4, 0x113c6, 0x113c6,
	0x113cb, 0x113cb, 0x113d6, 0x113d6, 0x113d9, 0x113e0, 0x113e3, 0x113ff,
	0x1145c, 0x1145c, 0x11462, 0x1147f, 0x114c8, 0x114cf, 0x114da, 0x1157f,
	0x115b6, 0x115b7, 0x115de, 0x115ff, 0x11645, 0x1164f, 0x1165a, 0x1165f,
	0x1166d, 0x1167f, 0x116ba, 0x116bf, 0x116ca, 0x116cf, 0x116e4, 0x116ff,
	0x1171b, 0x1171c, 0x1172c, 0x1172f, 0x11747, 0x117ff, 0x1183c, 0x1189f,
	0x118f3, 0x118fe, 0x11907, 0x11908, 0x1190a, 0x1190b, 0x11914, 0x11914,
	0x11917, 0x11917, 0x11936, 0x11936, 0x11939, 0x1193a, 0x11947, 0x1194f,
	0x1195a, 0x1199f, 0x119a8, 0x119a9, 0x119d8, 0x119d9, 0x119e5, 0x119ff,
	0x11a48, 0x11a4f, 0x11aa3, 0x11aaf, 0x11af9, 0x11aff, 0x11b0a, 0x11bbf,
	0x11be2, 0x11bef, 0x11bfa, 0x11bff, 0x11c09, 0x11c09, 0x11c37, 0x11c37,
	0x11c46, 0x11c4f, 0x11c6d, 0x11c6f, 0x11c90, 0x11c91, 0x11ca8, 0x11ca8,
	0x11cb7, 0x11cff, 0x11d07, 0x11d07, 0x11d0a, 0x11d0a, 0x11d37, 0x11d39,
	0x11d3b, 0x11d3b, 0x11d3e, 0x11d3e, 0x11d48, 0x11d4f, 0x11d5a, 0x11d5f,
	0x11d66, 0x11d66, 0x11d69, 0x11d69, 0x11d8f, 0x11d8f, 0x11d92, 0x11d92,
	0x11d99, 0x11d9f, 0x11daa, 0x11edf, 0x11ef9, 0x11eff, 0x11f11, 0x11f11,
	0x11f3b, 0x11f3d, 0x11f5b, 0x11faf, 0x11fb1, 0x11fbf, 0x11ff2, 0x11ffe,
	0x1239a, 0x123ff, 0x1246f, 0x1246f, 0x12475, 0x1247f, 0x12544, 0x12f8f,
	0x12ff3, 0x12fff, 0x13430, 0x1343f, 0x13456, 0x1345f, 0x143fb, 0x143ff,
	0x14647, 0x160ff, 0x1613a, 0x167ff, 0x16a39, 0x16a3f, 0x16a5f, 0x16a5f,
	0x16a6a, 0x16a6d, 0x16abf, 0x16abf, 0x16aca, 0x16acf, 0x16aee, 0x16aef,
	0x16af6, 0x16aff, 0x16b46, 0x16b4f, 0x16b5a, 0x16b5a, 0x16b62, 0x16b62,
	0x16b78, 0x16b7c, 0x16b90, 0x16d3f, 0x16d7a, 0x16e3f, 0x16e9b, 0x16eff,
	0x16f4b, 0x16f4e, 0x16f88, 0x16f8e, 0x16fa0, 0x16fdf, 0x16fe5, 0x16fef,
	0x16ff2, 0x16fff, 0x187f8, 0x187ff, 0x18cd6, 0x18cfe, 0x18d09, 0x1afef,
	0x1aff4, 0x1aff4, 0x1affc, 0x1affc, 0x1afff, 0x1afff, 0x1b123, 0x1b131,
	0x1b133, 0x1b14f, 0x1b153, 0x1b154, 0x1b156, 0x1b163, 0x1b168, 0x1b16f,
	0x1b2fc, 0x1bbff, 0x1bc6b, 0x1bc6f, 0x1bc7d, 0x1bc7f, 0x1bc89, 0x1bc8f,
	0x1bc9a, 0x1bc9b, 0x1bca0, 0x1cbff, 0x1ccfa, 0x1ccff, 0x1ceb4, 0x1ceff,
	0x1cf2e, 0x1cf2f, 0x1cf47, 0x1cf4f, 0x1cfc4, 0x1cfff, 0x1d0f6, 0x1d0ff,
	0x1d127, 0x1d128, 0x1d173, 0x1d17a, 0x1d1eb, 0x1d1ff, 0x1d246, 0x1d2bf,
	0x1d2d4, 0x1d2df, 0x1d2f4, 0x1d2ff, 0x1d357, 0x1d35f, 0x1d379, 0x1d3ff,
	0x1d455, 0x1d455, 0x1d49d, 0x1d49d, 0x1d4a0, 0x1d4a1, 0x1d4a3, 0x1d4a4,
	0x1d4a7, 0x1d4a8, 0x1d4ad, 0x1d4ad, 0x1d4ba, 0x1d4ba, 0x1d4bc, 0x1d4bc,
	0x1d4c4, 0x1d4c4, 0x1d506, 0x1d506, 0x1d50b, 0x1d50c, 0x1d515, 0x1d515,
	0x1d51d, 0x1d51d, 0x1d53a, 0x1d53a, 0x1d53f, 0x1d53f, 0x1d545, 0x1d545,
	0x1d547, 0x1d549, 0x1d551, 0x1d551, 0x1d6a6, 0x1d6a7, 0x1d7cc, 0x1d7cd,
	0x1da8c, 0x1da9a, 0x1daa0, 0x1daa0, 0x1dab0, 0x1deff, 0x1df1f, 0x1df24,
	0x1df2b, 0x1dfff, 0x1e007, 0x1e007, 0x1e019, 0x1e01a, 0x1e022, 0x1e022,
	0x1e025, 0x1e025, 0x1e02b, 0x1e02f, 0x1e06e, 0x1e08e, 0x1e090, 0x1e0ff,
	0x1e12d, 0x1e12f, 0x1e13e, 0x1e13f, 0x1e14a, 0x1e14d, 0x1e150, 0x1e28f,
	0x1e2af, 0x1e2bf, 0x1e2fa, 0x1e2fe, 0x1e300, 0x1e4cf, 0x1e4fa, 0x1e5cf,
	0x1e5fb, 0x1e5fe, 0x1e600, 0x1e7df, 0x1e7e7, 0x1e7e7, 0x1e7ec, 0x1e7ec,
	0x1e7ef, 0x1e7ef, 0x1e7ff, 0x1e7ff, 0x1e8c5, 0x1e8c6, 0x1e8d7, 0x1e8ff,
	0x1e94c, 0x1e94f, 0x1e95a, 0x1e95d, 0x1e960, 0x1ec70, 0x1ecb5, 0x1ed00,
	0x1ed3e, 0x1edff, 0x1ee04, 0x1ee04, 0x1ee20, 0x1ee20, 0x1ee23, 0x1ee23,
	0x1ee25, 0x1ee26, 0x1ee28, 0x1ee28, 0x1ee33, 0x1ee33, 0x1ee38, 0x1ee38,
	0x1ee3a, 0x1ee3a, 0x1ee3c, 0x1ee41, 0x1ee43, 0x1ee46, 0x1ee48, 0x1ee48,
	0x1ee4a, 0x1ee4a, 0x1ee4c, 0x1ee4c, 0x1ee50, 0x1ee50, 0x1ee53, 0x1ee53,
	0x1ee55, 0x1ee56, 0x1ee58, 0x1ee58, 0x1ee5a, 0x1ee5a, 0x1ee5c, 0x1ee5c,
	0x1ee5e, 0x1ee5e, 0x1ee60, 0x1ee60, 0x1ee63, 0x1ee63, 0x1ee65, 0x1ee66,
	0x1ee6b, 0x1ee6b, 0x1ee73, 0x1ee73, 0x1ee78, 0x1ee78, 0x1ee7d, 0x1ee7d,
	0x1ee7f, 0x1ee7f, 0x1ee8a, 0x1ee8a, 0x1ee9c, 0x1eea0, 0x1eea4, 0x1eea4,
	0x1eeaa, 0x1eeaa, 0x1eebc, 0x1eeef, 0x1eef2, 0x1efff, 0x1f02c, 0x1f02f,
	0x1f094, 0x1f09f, 0x1f0af, 0x1f0b0, 0x1f0c0, 0x1f0c0, 0x1f0d0, 0x1f0d0,
	0x1f0f6, 0x1f0ff, 0x1f1ae, 0x1f1e5, 0x1f203, 0x1f20f, 0x1f23c, 0x1f23f,
	0x1f249, 0x1f24f, 0x1f252, 0x1f25f, 0x1f266, 0x1f2ff, 0x1f6d8, 0x1f6db,
	0x1f6ed, 0x1f6ef, 0x1f6fd, 0x1f6ff, 0x1f777, 0x1f77a, 0x1f7da, 0x1f7df,
	0x1f7ec, 0x1f7ef, 0x1f7f1, 0x1f7ff, 0x1f80c, 0x1f80f, 0x1f848, 0x1f84f,
	0x1f85a, 0x1f85f, 0x1f888, 0x1f88f, 0x1f8ae, 0x1f8af, 0x1f8bc, 0x1f8bf,
	0x1f8c2, 0x1f8ff, 0x1fa54, 0x1fa5f, 0x1fa6e, 0x1fa6f, 0x1fa7d, 0x1fa7f,
	0x1fa8a, 0x1fa8e, 0x1fac7, 0x1facd, 0x1fadd, 0x1fade, 0x1faea, 0x1faef,
	0x1faf9, 0x1faff, 0x1fb93, 0x1fb93, 0x1fbfa, 0x1ffff, 0x2a6e0, 0x2a6ff,
	0x2b73a, 0x2b73f, 0x2b81e, 0x2b81f, 0x2cea2, 0x2ceaf, 0x2ebe1, 0x2ebef,
	0x2ee5e, 0x2f7ff, 0x2fa1e, 0x2ffff, 0x3134b, 0x3134f, 0x323b0, 0xe00ff,
	0xe01f0, 0x10ffff,
}

// Derived from libyaml 0.1.7 as bundled in ruamel.yaml.clib 0.2.15, MIT license, Kirill Simonov and Ingy dot Net

const (
	libyamlInputRawBufferSize = 16384
	libyamlInputBufferSize    = libyamlInputRawBufferSize * 3
	libyamlMaxFileSize        = math.MaxInt64
	libyamlInitialStackSize   = 16
	libyamlInitialStringSize  = 16
	libyamlStackByteLimit     = math.MaxInt32 / 2
	libyamlMaxNumberLength    = 9
	libyamlSizeofInt          = 4
	libyamlSizeofMark         = 24
	libyamlSizeofSimpleKey    = 40
	libyamlSizeofState        = 4
	libyamlSizeofTagDirective = 16
)

type yamlMark struct {
	index, line, column int
}

// Collection start position kept on the marks stack, which only ever renders line and column
type yamlStackMark struct {
	line, column int
}

type yamlErrorType int8

const (
	yamlNoError yamlErrorType = iota
	yamlMemoryError
	yamlReaderError
	yamlScannerError
	yamlParserError
)

type yamlEncoding int8

const (
	yamlAnyEncoding yamlEncoding = iota
	yamlUTF8Encoding
	yamlUTF16LEEncoding
	yamlUTF16BEEncoding
)

type yamlTokenType int8

const (
	yamlNoToken yamlTokenType = iota
	yamlStreamStartToken
	yamlStreamEndToken
	yamlVersionDirectiveToken
	yamlTagDirectiveToken
	yamlDocumentStartToken
	yamlDocumentEndToken
	yamlBlockSequenceStartToken
	yamlBlockMappingStartToken
	yamlBlockEndToken
	yamlFlowSequenceStartToken
	yamlFlowSequenceEndToken
	yamlFlowMappingStartToken
	yamlFlowMappingEndToken
	yamlBlockEntryToken
	yamlFlowEntryToken
	yamlKeyToken
	yamlValueToken
	yamlAliasToken
	yamlAnchorToken
	yamlTagToken
	yamlScalarToken
)

// Scanner output unit with the scalar, alias or anchor value, tag handle and suffix, and version numbers
type yamlToken struct {
	typ       yamlTokenType
	startMark yamlMark
	value     []byte
	handle    []byte
	suffix    []byte
	major     int32
	minor     int32
}

type yamlEventType int8

const (
	yamlNoEvent yamlEventType = iota
	yamlStreamStartEvent
	yamlStreamEndEvent
	yamlDocumentStartEvent
	yamlDocumentEndEvent
	yamlAliasEvent
	yamlScalarEvent
	yamlSequenceStartEvent
	yamlSequenceEndEvent
	yamlMappingStartEvent
	yamlMappingEndEvent
)

// Parser output with the strings the pyx layer decodes while building its Python object
type yamlEvent struct {
	typ           yamlEventType
	anchor        []byte
	tag           []byte
	value         []byte
	tagDirectives []yamlTagDirective
}

type yamlTagDirective struct {
	handle []byte
	prefix []byte
}

type yamlSimpleKey struct {
	possible    bool
	required    bool
	tokenNumber int
	mark        yamlMark
}

type yamlParserState int8

const (
	yamlParseStreamStartState yamlParserState = iota
	yamlParseImplicitDocumentStartState
	yamlParseDocumentStartState
	yamlParseDocumentContentState
	yamlParseDocumentEndState
	yamlParseBlockNodeState
	yamlParseBlockNodeOrIndentlessSequenceState
	yamlParseFlowNodeState
	yamlParseBlockSequenceFirstEntryState
	yamlParseBlockSequenceEntryState
	yamlParseIndentlessSequenceEntryState
	yamlParseBlockMappingFirstKeyState
	yamlParseBlockMappingKeyState
	yamlParseBlockMappingValueState
	yamlParseFlowSequenceFirstEntryState
	yamlParseFlowSequenceEntryState
	yamlParseFlowSequenceEntryMappingKeyState
	yamlParseFlowSequenceEntryMappingValueState
	yamlParseFlowSequenceEntryMappingEndState
	yamlParseFlowMappingFirstKeyState
	yamlParseFlowMappingKeyState
	yamlParseFlowMappingValueState
	yamlParseFlowMappingEmptyValueState
	yamlParseEndState
)

// Mirror of the libyaml C state with each stack capacity tracked so memory limits trip at the same depth
type yamlParser struct {
	errorType     yamlErrorType
	problem       string
	problemOffset int
	problemValue  int
	problemMark   yamlMark
	context       string
	contextMark   yamlMark

	input    string
	inputPos int
	eof      bool

	buffer     []byte
	bufferPos  int
	bufferLast int
	unread     int

	rawBuffer []byte
	rawPos    int
	rawLast   int

	encoding yamlEncoding
	offset   int
	mark     yamlMark

	streamStartProduced bool
	streamEndProduced   bool
	flowLevel           int32

	tokens         []yamlToken
	tokensHead     int
	tokensParsed   int
	tokenAvailable bool

	indents          []int32
	indentsCap       int
	indent           int32
	simpleKeyAllowed bool
	simpleKeys       []yamlSimpleKey
	simpleKeysCap    int
	states           []yamlParserState
	statesCap        int
	state            yamlParserState
	marks            []yamlStackMark
	marksCap         int
	tagDirectives    []yamlTagDirective
	tagDirectivesCap int
}

// Python verdict of YAML(typ="safe").parse over a str, empty when every event converts cleanly
func ruamelCParseError(content string) string {
	if msg := ruamelUTF8EncodeError(content); msg != "" {
		return msg
	}
	return ruamelCParseStream(content, "<unicode string>")
}

// Drives the CParser event loop over the raw input bytes and renders the first failure as hermes does
func ruamelCParseStream(input string, name string) string {
	p := yamlParserInitialize(input)
	for {
		var ev yamlEvent
		if !yamlParserParse(p, &ev) {
			return ruamelParserError(p, name)
		}
		if ev.typ == yamlNoEvent {
			return ""
		}
		if msg := ruamelEventToObject(&ev); msg != "" {
			return msg
		}
	}
}

func yamlParserInitialize(input string) *yamlParser {
	return &yamlParser{
		input:            input,
		buffer:           make([]byte, libyamlInputBufferSize),
		rawBuffer:        make([]byte, libyamlInputRawBufferSize),
		indentsCap:       libyamlInitialStackSize,
		simpleKeysCap:    libyamlInitialStackSize,
		statesCap:        libyamlInitialStackSize,
		marksCap:         libyamlInitialStackSize,
		tagDirectivesCap: libyamlInitialStackSize,
	}
}

// Message CPython raises strictly encoding the str to UTF-8 at its first run of surrogates
func ruamelUTF8EncodeError(content string) string {
	pos := 0
	for i := 0; i < len(content); {
		r, w := pystr.DecodeRune(content[i:])
		if !pystr.IsSurrogate(r) {
			i += w
			pos++
			continue
		}
		bad := r
		run := 0
		for i < len(content) {
			r, w = pystr.DecodeRune(content[i:])
			if !pystr.IsSurrogate(r) {
				break
			}
			run++
			i += w
		}
		if run == 1 {
			return "UnicodeEncodeError: 'utf-8' codec can't encode character '\\u" + strconv.FormatInt(int64(bad), 16) + "' in position " + strconv.Itoa(pos) + ": surrogates not allowed"
		}
		return "UnicodeEncodeError: 'utf-8' codec can't encode characters in position " + strconv.Itoa(pos) + "-" + strconv.Itoa(pos+run-1) + ": surrogates not allowed"
	}
	return ""
}

// Decodes every string the pyx event conversion passes through strict UTF-8 in the same order
func ruamelEventToObject(ev *yamlEvent) string {
	switch ev.typ {
	case yamlDocumentStartEvent:
		for _, td := range ev.tagDirectives {
			if msg := ruamelUTF8DecodeError(td.handle); msg != "" {
				return msg
			}
			if msg := ruamelUTF8DecodeError(td.prefix); msg != "" {
				return msg
			}
		}
	case yamlAliasEvent:
		return ruamelUTF8DecodeError(ev.anchor)
	case yamlScalarEvent:
		if msg := ruamelUTF8DecodeError(ev.anchor); msg != "" {
			return msg
		}
		if msg := ruamelUTF8DecodeError(ev.tag); msg != "" {
			return msg
		}
		return ruamelUTF8DecodeError(ev.value)
	case yamlSequenceStartEvent, yamlMappingStartEvent:
		if msg := ruamelUTF8DecodeError(ev.anchor); msg != "" {
			return msg
		}
		return ruamelUTF8DecodeError(ev.tag)
	}
	return ""
}

// Python message for the first strict UTF-8 failure CPython reports, empty when the bytes decode
func ruamelUTF8DecodeError(b []byte) string {
	start, end, reason := ruamelUTF8DecodeFailure(b)
	if reason == "" {
		return ""
	}
	if end == start+1 {
		hex := strconv.FormatInt(int64(b[start]), 16)
		if len(hex) < 2 {
			hex = "0" + hex
		}
		return "UnicodeDecodeError: 'utf-8' codec can't decode byte 0x" + hex + " in position " + strconv.Itoa(start) + ": " + reason
	}
	return "UnicodeDecodeError: 'utf-8' codec can't decode bytes in position " + strconv.Itoa(start) + "-" + strconv.Itoa(end-1) + ": " + reason
}

// Locates the first error range and reason of the CPython stringlib UTF-8 decoder
func ruamelUTF8DecodeFailure(b []byte) (int, int, string) {
	n := len(b)
	cont := func(c byte) bool { return c >= 0x80 && c < 0xC0 }
	for s := 0; s < n; {
		ch := b[s]
		switch {
		case ch < 0x80:
			s++
		case ch < 0xC2:
			return s, s + 1, "invalid start byte"
		case ch < 0xE0:
			if n-s < 2 {
				return s, n, "unexpected end of data"
			}
			if !cont(b[s+1]) {
				return s, s + 1, "invalid continuation byte"
			}
			s += 2
		case ch < 0xF0:
			if n-s < 3 {
				if n-s >= 2 {
					ch2 := b[s+1]
					if !cont(ch2) || (ch2 < 0xA0 && ch == 0xE0) || (ch2 >= 0xA0 && ch == 0xED) {
						return s, s + 1, "invalid continuation byte"
					}
				}
				return s, n, "unexpected end of data"
			}
			ch2 := b[s+1]
			if !cont(ch2) || (ch == 0xE0 && ch2 < 0xA0) || (ch == 0xED && ch2 >= 0xA0) {
				return s, s + 1, "invalid continuation byte"
			}
			if !cont(b[s+2]) {
				return s, s + 2, "invalid continuation byte"
			}
			s += 3
		case ch < 0xF5:
			if n-s < 4 {
				if n-s >= 2 {
					ch2 := b[s+1]
					if !cont(ch2) || (ch2 < 0x90 && ch == 0xF0) || (ch2 >= 0x90 && ch == 0xF4) {
						return s, s + 1, "invalid continuation byte"
					}
					if n-s >= 3 && !cont(b[s+2]) {
						return s, s + 2, "invalid continuation byte"
					}
				}
				return s, n, "unexpected end of data"
			}
			ch2 := b[s+1]
			if !cont(ch2) || (ch == 0xF0 && ch2 < 0x90) || (ch == 0xF4 && ch2 >= 0x90) {
				return s, s + 1, "invalid continuation byte"
			}
			if !cont(b[s+2]) {
				return s, s + 2, "invalid continuation byte"
			}
			if !cont(b[s+3]) {
				return s, s + 3, "invalid continuation byte"
			}
			s += 4
		default:
			return s, s + 1, "invalid start byte"
		}
	}
	return 0, 0, ""
}

// Renders the exception the pyx layer builds from the libyaml failure through the ruamel string methods with the hermes prefix
func ruamelParserError(p *yamlParser, name string) string {
	switch p.errorType {
	case yamlMemoryError:
		return "MemoryError: "
	case yamlReaderError:
		return "YAMLError: unacceptable character #x" + ruamelFormatHex04(p.problemValue) + ": " + p.problem + "\n  in \"" + name + "\", position " + strconv.Itoa(p.problemOffset)
	case yamlScannerError, yamlParserError:
		lines := make([]string, 0, 4)
		if p.context != "" {
			lines = append(lines, p.context)
			if p.contextMark.line != p.problemMark.line || p.contextMark.column != p.problemMark.column {
				lines = append(lines, ruamelMarkString(name, p.contextMark))
			}
		}
		lines = append(lines, p.problem, ruamelMarkString(name, p.problemMark))
		return "YAMLError: " + strings.Join(lines, "\n")
	}
	return "ValueError: no parser error"
}

func ruamelMarkString(name string, m yamlMark) string {
	return "  in \"" + name + "\", line " + strconv.Itoa(m.line+1) + ", column " + strconv.Itoa(m.column+1)
}

// Renders an int the way the Python spec 04x does, sign first and then zero padding
func ruamelFormatHex04(v int) string {
	neg := v < 0
	if neg {
		v = -v
	}
	digits := strconv.FormatInt(int64(v), 16)
	width := 4
	if neg {
		width = 3
	}
	if len(digits) < width {
		digits = strings.Repeat("0", width-len(digits)) + digits
	}
	if neg {
		return "-" + digits
	}
	return digits
}

// Appends to a C stack whose capacity doubles from 16, failing like yaml_stack_extend at INT_MAX over two bytes
func libyamlPush[T any](p *yamlParser, stack *[]T, capacity *int, elemSize int, v T) bool {
	if len(*stack) == *capacity {
		if *capacity*elemSize >= libyamlStackByteLimit {
			p.errorType = yamlMemoryError
			return false
		}
		*capacity *= 2
	}
	if len(*stack) == cap(*stack) {
		grown := make([]T, len(*stack), *capacity)
		copy(grown, *stack)
		*stack = grown
	}
	*stack = append(*stack, v)
	return true
}

func yamlParserSetReaderError(p *yamlParser, problem string, offset int, value int) bool {
	p.errorType = yamlReaderError
	p.problem = problem
	p.problemOffset = offset
	p.problemValue = value
	return false
}

// Detects a UTF-16 or UTF-8 byte order mark and consumes it, defaulting to UTF-8
func yamlParserDetermineEncoding(p *yamlParser) bool {
	for !p.eof && p.rawLast-p.rawPos < 3 {
		if !yamlParserUpdateRawBuffer(p) {
			return false
		}
	}
	raw := p.rawBuffer[p.rawPos:p.rawLast]
	switch {
	case len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xFE:
		p.encoding = yamlUTF16LEEncoding
		p.rawPos += 2
		p.offset += 2
	case len(raw) >= 2 && raw[0] == 0xFE && raw[1] == 0xFF:
		p.encoding = yamlUTF16BEEncoding
		p.rawPos += 2
		p.offset += 2
	case len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF:
		p.encoding = yamlUTF8Encoding
		p.rawPos += 3
		p.offset += 3
	default:
		p.encoding = yamlUTF8Encoding
	}
	return true
}

// Shifts unread raw bytes to the front and copies the next input chunk like the string read handler
func yamlParserUpdateRawBuffer(p *yamlParser) bool {
	if p.rawPos == 0 && p.rawLast == len(p.rawBuffer) {
		return true
	}
	if p.eof {
		return true
	}
	if 0 < p.rawPos && p.rawPos < p.rawLast {
		copy(p.rawBuffer, p.rawBuffer[p.rawPos:p.rawLast])
	}
	p.rawLast -= p.rawPos
	p.rawPos = 0
	n := copy(p.rawBuffer[p.rawLast:], p.input[p.inputPos:])
	p.inputPos += n
	p.rawLast += n
	if n == 0 {
		p.eof = true
	}
	return true
}

// Decodes whole raw chunks into the UTF-8 working buffer until enough characters are unread
func yamlParserUpdateBuffer(p *yamlParser, length int) bool {
	first := true
	if p.eof && p.rawPos == p.rawLast {
		return true
	}
	if p.unread >= length {
		return true
	}
	if p.encoding == yamlAnyEncoding {
		if !yamlParserDetermineEncoding(p) {
			return false
		}
	}
	if 0 < p.bufferPos && p.bufferPos < p.bufferLast {
		size := p.bufferLast - p.bufferPos
		copy(p.buffer, p.buffer[p.bufferPos:p.bufferLast])
		p.bufferPos = 0
		p.bufferLast = size
	} else if p.bufferPos == p.bufferLast {
		p.bufferPos = 0
		p.bufferLast = 0
	}
	for p.unread < length {
		if !first || p.rawPos == p.rawLast {
			if !yamlParserUpdateRawBuffer(p) {
				return false
			}
		}
		first = false
		for p.rawPos != p.rawLast {
			var value, width int
			incomplete := false
			rawUnread := p.rawLast - p.rawPos
			raw := p.rawBuffer[p.rawPos:p.rawLast]
			if p.encoding == yamlUTF8Encoding {
				octet := raw[0]
				switch {
				case octet&0x80 == 0x00:
					width = 1
				case octet&0xE0 == 0xC0:
					width = 2
				case octet&0xF0 == 0xE0:
					width = 3
				case octet&0xF8 == 0xF0:
					width = 4
				}
				if width == 0 {
					return yamlParserSetReaderError(p, "invalid leading UTF-8 octet", p.offset, int(octet))
				}
				if width > rawUnread {
					if p.eof {
						return yamlParserSetReaderError(p, "incomplete UTF-8 octet sequence", p.offset, -1)
					}
					incomplete = true
				} else {
					switch width {
					case 1:
						value = int(octet & 0x7F)
					case 2:
						value = int(octet & 0x1F)
					case 3:
						value = int(octet & 0x0F)
					case 4:
						value = int(octet & 0x07)
					}
					for k := 1; k < width; k++ {
						octet = raw[k]
						if octet&0xC0 != 0x80 {
							return yamlParserSetReaderError(p, "invalid trailing UTF-8 octet", p.offset+k, int(octet))
						}
						value = (value << 6) + int(octet&0x3F)
					}
					if !(width == 1 || (width == 2 && value >= 0x80) || (width == 3 && value >= 0x800) || (width == 4 && value >= 0x10000)) {
						return yamlParserSetReaderError(p, "invalid length of a UTF-8 sequence", p.offset, -1)
					}
					if (value >= 0xD800 && value <= 0xDFFF) || value > 0x10FFFF {
						return yamlParserSetReaderError(p, "invalid Unicode character", p.offset, value)
					}
				}
			} else {
				low, high := 0, 1
				if p.encoding == yamlUTF16BEEncoding {
					low, high = 1, 0
				}
				if rawUnread < 2 {
					if p.eof {
						return yamlParserSetReaderError(p, "incomplete UTF-16 character", p.offset, -1)
					}
					incomplete = true
				} else {
					value = int(raw[low]) + int(raw[high])<<8
					if value&0xFC00 == 0xDC00 {
						return yamlParserSetReaderError(p, "unexpected low surrogate area", p.offset, value)
					}
					if value&0xFC00 == 0xD800 {
						width = 4
						if rawUnread < 4 {
							if p.eof {
								return yamlParserSetReaderError(p, "incomplete UTF-16 surrogate pair", p.offset, -1)
							}
							incomplete = true
						} else {
							value2 := int(raw[low+2]) + int(raw[high+2])<<8
							if value2&0xFC00 != 0xDC00 {
								return yamlParserSetReaderError(p, "expected low surrogate area", p.offset+2, value2)
							}
							value = 0x10000 + (value&0x3FF)<<10 + (value2 & 0x3FF)
						}
					} else {
						width = 2
					}
				}
			}
			if incomplete {
				break
			}
			if !(value == 0x09 || value == 0x0A || value == 0x0D || (value >= 0x20 && value <= 0x7E) ||
				value == 0x85 || (value >= 0xA0 && value <= 0xD7FF) || (value >= 0xE000 && value <= 0xFFFD) ||
				(value >= 0x10000 && value <= 0x10FFFF)) {
				return yamlParserSetReaderError(p, "control characters are not allowed", p.offset, value)
			}
			p.rawPos += width
			p.offset += width
			p.bufferLast += utf8.EncodeRune(p.buffer[p.bufferLast:], rune(value))
			p.unread++
		}
		if p.eof {
			p.buffer[p.bufferLast] = 0
			p.bufferLast++
			p.unread++
			return true
		}
	}
	if p.offset >= libyamlMaxFileSize {
		return yamlParserSetReaderError(p, "input is too long", p.offset, -1)
	}
	return true
}

func yamlCache(p *yamlParser, length int) bool {
	return p.unread >= length || yamlParserUpdateBuffer(p, length)
}

func yamlAt(p *yamlParser, k int) byte {
	return p.buffer[p.bufferPos+k]
}

func yamlIsAlpha(p *yamlParser, k int) bool {
	c := yamlAt(p, k)
	return (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_' || c == '-'
}

func yamlIsDigit(p *yamlParser, k int) bool {
	c := yamlAt(p, k)
	return c >= '0' && c <= '9'
}

func yamlAsDigit(p *yamlParser, k int) int32 {
	return int32(yamlAt(p, k) - '0')
}

func yamlIsHex(p *yamlParser, k int) bool {
	c := yamlAt(p, k)
	return (c >= '0' && c <= '9') || (c >= 'A' && c <= 'F') || (c >= 'a' && c <= 'f')
}

func yamlAsHex(p *yamlParser, k int) int {
	c := yamlAt(p, k)
	switch {
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	}
	return int(c - '0')
}

func yamlIsZ(p *yamlParser, k int) bool {
	return yamlAt(p, k) == 0
}

func yamlIsBOM(p *yamlParser, k int) bool {
	return yamlAt(p, k) == 0xEF && yamlAt(p, k+1) == 0xBB && yamlAt(p, k+2) == 0xBF
}

func yamlIsSpace(p *yamlParser, k int) bool {
	return yamlAt(p, k) == ' '
}

func yamlIsTab(p *yamlParser, k int) bool {
	return yamlAt(p, k) == '\t'
}

func yamlIsBlank(p *yamlParser, k int) bool {
	c := yamlAt(p, k)
	return c == ' ' || c == '\t'
}

func yamlIsBreak(p *yamlParser, k int) bool {
	c := yamlAt(p, k)
	return c == '\r' || c == '\n' ||
		(c == 0xC2 && yamlAt(p, k+1) == 0x85) ||
		(c == 0xE2 && yamlAt(p, k+1) == 0x80 && yamlAt(p, k+2) == 0xA8) ||
		(c == 0xE2 && yamlAt(p, k+1) == 0x80 && yamlAt(p, k+2) == 0xA9)
}

func yamlIsCRLF(p *yamlParser, k int) bool {
	return yamlAt(p, k) == '\r' && yamlAt(p, k+1) == '\n'
}

func yamlIsBreakZ(p *yamlParser, k int) bool {
	return yamlIsBreak(p, k) || yamlIsZ(p, k)
}

func yamlIsBlankZ(p *yamlParser, k int) bool {
	return yamlIsBlank(p, k) || yamlIsBreakZ(p, k)
}

func yamlWidth(c byte) int {
	switch {
	case c&0x80 == 0x00:
		return 1
	case c&0xE0 == 0xC0:
		return 2
	case c&0xF0 == 0xE0:
		return 3
	case c&0xF8 == 0xF0:
		return 4
	}
	return 0
}

func yamlSkip(p *yamlParser) {
	p.mark.index++
	p.mark.column++
	p.unread--
	p.bufferPos += yamlWidth(p.buffer[p.bufferPos])
}

func yamlSkipLine(p *yamlParser) {
	if yamlIsCRLF(p, 0) {
		p.mark.index += 2
		p.mark.column = 0
		p.mark.line++
		p.unread -= 2
		p.bufferPos += 2
	} else if yamlIsBreak(p, 0) {
		p.mark.index++
		p.mark.column = 0
		p.mark.line++
		p.unread--
		p.bufferPos += yamlWidth(p.buffer[p.bufferPos])
	}
}

func yamlRead(p *yamlParser, s []byte) []byte {
	w := yamlWidth(p.buffer[p.bufferPos])
	s = append(s, p.buffer[p.bufferPos:p.bufferPos+w]...)
	p.bufferPos += w
	p.mark.index++
	p.mark.column++
	p.unread--
	return s
}

// Appends one line break normalized to LF, keeping LS and PS verbatim
func yamlReadLine(p *yamlParser, s []byte) []byte {
	c := yamlAt(p, 0)
	switch {
	case c == '\r' && yamlAt(p, 1) == '\n':
		s = append(s, '\n')
		p.bufferPos += 2
		p.mark.index += 2
		p.mark.column = 0
		p.mark.line++
		p.unread -= 2
	case c == '\r' || c == '\n':
		s = append(s, '\n')
		p.bufferPos++
		p.mark.index++
		p.mark.column = 0
		p.mark.line++
		p.unread--
	case c == 0xC2 && yamlAt(p, 1) == 0x85:
		s = append(s, '\n')
		p.bufferPos += 2
		p.mark.index++
		p.mark.column = 0
		p.mark.line++
		p.unread--
	case c == 0xE2 && yamlAt(p, 1) == 0x80 && (yamlAt(p, 2) == 0xA8 || yamlAt(p, 2) == 0xA9):
		s = append(s, p.buffer[p.bufferPos:p.bufferPos+3]...)
		p.bufferPos += 3
		p.mark.index++
		p.mark.column = 0
		p.mark.line++
		p.unread--
	}
	return s
}

// Cuts scanned bytes at the first NUL, matching what strlen based consumers in libyaml and CPython see
func yamlCString(s []byte) []byte {
	if i := bytes.IndexByte(s, 0); i >= 0 {
		return s[:i]
	}
	return s
}

func yamlNewString() []byte {
	return make([]byte, 0, libyamlInitialStringSize)
}

func yamlParserSetScannerError(p *yamlParser, context string, contextMark yamlMark, problem string) bool {
	p.errorType = yamlScannerError
	p.context = context
	p.contextMark = contextMark
	p.problem = problem
	p.problemMark = p.mark
	return false
}

func yamlEnqueueToken(p *yamlParser, t yamlToken) {
	if p.tokensHead > 0 && len(p.tokens) == cap(p.tokens) {
		n := copy(p.tokens, p.tokens[p.tokensHead:])
		p.tokens = p.tokens[:n]
		p.tokensHead = 0
	}
	p.tokens = append(p.tokens, t)
}

func yamlInsertToken(p *yamlParser, pos int, t yamlToken) {
	yamlEnqueueToken(p, yamlToken{typ: yamlNoToken})
	i := p.tokensHead + pos
	copy(p.tokens[i+1:], p.tokens[i:len(p.tokens)-1])
	p.tokens[i] = t
}

// Queues scanner output until the head entry can no longer turn into a simple key
func yamlParserFetchMoreTokens(p *yamlParser) bool {
	for {
		need := false
		if p.tokensHead == len(p.tokens) {
			need = true
		} else {
			if !yamlParserStaleSimpleKeys(p) {
				return false
			}
			for i := range p.simpleKeys {
				k := &p.simpleKeys[i]
				if k.possible && k.tokenNumber == p.tokensParsed {
					need = true
					break
				}
			}
		}
		if !need {
			break
		}
		if !yamlParserFetchNextToken(p) {
			return false
		}
	}
	p.tokenAvailable = true
	return true
}

// Dispatches on the next significant character to the matching scanner routine
func yamlParserFetchNextToken(p *yamlParser) bool {
	if !yamlCache(p, 1) {
		return false
	}
	if !p.streamStartProduced {
		return yamlParserFetchStreamStart(p)
	}
	if !yamlParserScanToNextToken(p) {
		return false
	}
	if !yamlParserStaleSimpleKeys(p) {
		return false
	}
	yamlParserUnrollIndent(p, p.mark.column)
	if !yamlCache(p, 4) {
		return false
	}
	if yamlIsZ(p, 0) {
		return yamlParserFetchStreamEnd(p)
	}
	c := yamlAt(p, 0)
	if p.mark.column == 0 && c == '%' {
		return yamlParserFetchDirective(p)
	}
	if p.mark.column == 0 && c == '-' && yamlAt(p, 1) == '-' && yamlAt(p, 2) == '-' && yamlIsBlankZ(p, 3) {
		return yamlParserFetchDocumentIndicator(p, yamlDocumentStartToken)
	}
	if p.mark.column == 0 && c == '.' && yamlAt(p, 1) == '.' && yamlAt(p, 2) == '.' && yamlIsBlankZ(p, 3) {
		return yamlParserFetchDocumentIndicator(p, yamlDocumentEndToken)
	}
	switch c {
	case '[':
		return yamlParserFetchFlowCollectionStart(p, yamlFlowSequenceStartToken)
	case '{':
		return yamlParserFetchFlowCollectionStart(p, yamlFlowMappingStartToken)
	case ']':
		return yamlParserFetchFlowCollectionEnd(p, yamlFlowSequenceEndToken)
	case '}':
		return yamlParserFetchFlowCollectionEnd(p, yamlFlowMappingEndToken)
	case ',':
		return yamlParserFetchFlowEntry(p)
	}
	if c == '-' && yamlIsBlankZ(p, 1) {
		return yamlParserFetchBlockEntry(p)
	}
	if c == '?' && (p.flowLevel != 0 || yamlIsBlankZ(p, 1)) {
		return yamlParserFetchKey(p)
	}
	if c == ':' && (p.flowLevel != 0 || yamlIsBlankZ(p, 1)) {
		return yamlParserFetchValue(p)
	}
	switch c {
	case '*':
		return yamlParserFetchAnchor(p, yamlAliasToken)
	case '&':
		return yamlParserFetchAnchor(p, yamlAnchorToken)
	case '!':
		return yamlParserFetchTag(p)
	}
	if c == '|' && p.flowLevel == 0 {
		return yamlParserFetchBlockScalar(p, true)
	}
	if c == '>' && p.flowLevel == 0 {
		return yamlParserFetchBlockScalar(p, false)
	}
	if c == '\'' {
		return yamlParserFetchFlowScalar(p, true)
	}
	if c == '"' {
		return yamlParserFetchFlowScalar(p, false)
	}
	if !(yamlIsBlankZ(p, 0) || strings.IndexByte("-?:,[]{}#&*!|>'\"%@`", c) >= 0) ||
		(c == '-' && !yamlIsBlank(p, 1)) ||
		(p.flowLevel == 0 && (c == '?' || c == ':') && !yamlIsBlankZ(p, 1)) {
		return yamlParserFetchPlainScalar(p)
	}
	return yamlParserSetScannerError(p, "while scanning for the next token", p.mark, "found character that cannot start any token")
}

// Expires pending keys left on an earlier line or over 1024 characters back, failing on required ones
func yamlParserStaleSimpleKeys(p *yamlParser) bool {
	for i := range p.simpleKeys {
		k := &p.simpleKeys[i]
		if k.possible && (k.mark.line < p.mark.line || k.mark.index+1024 < p.mark.index) {
			if k.required {
				return yamlParserSetScannerError(p, "while scanning a simple key", k.mark, "could not find expected ':'")
			}
			k.possible = false
		}
	}
	return true
}

func yamlParserSaveSimpleKey(p *yamlParser) bool {
	required := p.flowLevel == 0 && int(p.indent) == p.mark.column
	if p.simpleKeyAllowed {
		key := yamlSimpleKey{
			possible:    true,
			required:    required,
			tokenNumber: p.tokensParsed + (len(p.tokens) - p.tokensHead),
			mark:        p.mark,
		}
		if !yamlParserRemoveSimpleKey(p) {
			return false
		}
		p.simpleKeys[len(p.simpleKeys)-1] = key
	}
	return true
}

func yamlParserRemoveSimpleKey(p *yamlParser) bool {
	k := &p.simpleKeys[len(p.simpleKeys)-1]
	if k.possible && k.required {
		return yamlParserSetScannerError(p, "while scanning a simple key", k.mark, "could not find expected ':'")
	}
	k.possible = false
	return true
}

func yamlParserIncreaseFlowLevel(p *yamlParser) bool {
	if !libyamlPush(p, &p.simpleKeys, &p.simpleKeysCap, libyamlSizeofSimpleKey, yamlSimpleKey{}) {
		return false
	}
	if p.flowLevel == math.MaxInt32 {
		p.errorType = yamlMemoryError
		return false
	}
	p.flowLevel++
	return true
}

func yamlParserDecreaseFlowLevel(p *yamlParser) {
	if p.flowLevel != 0 {
		p.flowLevel--
		p.simpleKeys = p.simpleKeys[:len(p.simpleKeys)-1]
	}
}

// Opens a block collection at a deeper column by queueing its start token
func yamlParserRollIndent(p *yamlParser, column int, number int, typ yamlTokenType, mark yamlMark) bool {
	if p.flowLevel != 0 {
		return true
	}
	if int(p.indent) < column {
		if !libyamlPush(p, &p.indents, &p.indentsCap, libyamlSizeofInt, p.indent) {
			return false
		}
		if column > math.MaxInt32 {
			p.errorType = yamlMemoryError
			return false
		}
		p.indent = int32(column)
		t := yamlToken{typ: typ, startMark: mark}
		if number == -1 {
			yamlEnqueueToken(p, t)
		} else {
			yamlInsertToken(p, number-p.tokensParsed, t)
		}
	}
	return true
}

// Closes block collections deeper than the column with block end tokens
func yamlParserUnrollIndent(p *yamlParser, column int) {
	if p.flowLevel != 0 {
		return
	}
	for int(p.indent) > column {
		yamlEnqueueToken(p, yamlToken{typ: yamlBlockEndToken, startMark: p.mark})
		p.indent = p.indents[len(p.indents)-1]
		p.indents = p.indents[:len(p.indents)-1]
	}
}

func yamlParserFetchStreamStart(p *yamlParser) bool {
	p.indent = -1
	if !libyamlPush(p, &p.simpleKeys, &p.simpleKeysCap, libyamlSizeofSimpleKey, yamlSimpleKey{}) {
		return false
	}
	p.simpleKeyAllowed = true
	p.streamStartProduced = true
	yamlEnqueueToken(p, yamlToken{typ: yamlStreamStartToken, startMark: p.mark})
	return true
}

func yamlParserFetchStreamEnd(p *yamlParser) bool {
	if p.mark.column != 0 {
		p.mark.column = 0
		p.mark.line++
	}
	yamlParserUnrollIndent(p, -1)
	if !yamlParserRemoveSimpleKey(p) {
		return false
	}
	p.simpleKeyAllowed = false
	yamlEnqueueToken(p, yamlToken{typ: yamlStreamEndToken, startMark: p.mark})
	return true
}

func yamlParserFetchDirective(p *yamlParser) bool {
	yamlParserUnrollIndent(p, -1)
	if !yamlParserRemoveSimpleKey(p) {
		return false
	}
	p.simpleKeyAllowed = false
	var t yamlToken
	if !yamlParserScanDirective(p, &t) {
		return false
	}
	yamlEnqueueToken(p, t)
	return true
}

func yamlParserFetchDocumentIndicator(p *yamlParser, typ yamlTokenType) bool {
	yamlParserUnrollIndent(p, -1)
	if !yamlParserRemoveSimpleKey(p) {
		return false
	}
	p.simpleKeyAllowed = false
	startMark := p.mark
	yamlSkip(p)
	yamlSkip(p)
	yamlSkip(p)
	yamlEnqueueToken(p, yamlToken{typ: typ, startMark: startMark})
	return true
}

func yamlParserFetchFlowCollectionStart(p *yamlParser, typ yamlTokenType) bool {
	if !yamlParserSaveSimpleKey(p) {
		return false
	}
	if !yamlParserIncreaseFlowLevel(p) {
		return false
	}
	p.simpleKeyAllowed = true
	startMark := p.mark
	yamlSkip(p)
	yamlEnqueueToken(p, yamlToken{typ: typ, startMark: startMark})
	return true
}

func yamlParserFetchFlowCollectionEnd(p *yamlParser, typ yamlTokenType) bool {
	if !yamlParserRemoveSimpleKey(p) {
		return false
	}
	yamlParserDecreaseFlowLevel(p)
	p.simpleKeyAllowed = false
	startMark := p.mark
	yamlSkip(p)
	yamlEnqueueToken(p, yamlToken{typ: typ, startMark: startMark})
	return true
}

func yamlParserFetchFlowEntry(p *yamlParser) bool {
	if !yamlParserRemoveSimpleKey(p) {
		return false
	}
	p.simpleKeyAllowed = true
	startMark := p.mark
	yamlSkip(p)
	yamlEnqueueToken(p, yamlToken{typ: yamlFlowEntryToken, startMark: startMark})
	return true
}

func yamlParserFetchBlockEntry(p *yamlParser) bool {
	if p.flowLevel == 0 {
		if !p.simpleKeyAllowed {
			return yamlParserSetScannerError(p, "", p.mark, "block sequence entries are not allowed in this context")
		}
		if !yamlParserRollIndent(p, p.mark.column, -1, yamlBlockSequenceStartToken, p.mark) {
			return false
		}
	}
	if !yamlParserRemoveSimpleKey(p) {
		return false
	}
	p.simpleKeyAllowed = true
	startMark := p.mark
	yamlSkip(p)
	yamlEnqueueToken(p, yamlToken{typ: yamlBlockEntryToken, startMark: startMark})
	return true
}

func yamlParserFetchKey(p *yamlParser) bool {
	if p.flowLevel == 0 {
		if !p.simpleKeyAllowed {
			return yamlParserSetScannerError(p, "", p.mark, "mapping keys are not allowed in this context")
		}
		if !yamlParserRollIndent(p, p.mark.column, -1, yamlBlockMappingStartToken, p.mark) {
			return false
		}
	}
	if !yamlParserRemoveSimpleKey(p) {
		return false
	}
	p.simpleKeyAllowed = p.flowLevel == 0
	startMark := p.mark
	yamlSkip(p)
	yamlEnqueueToken(p, yamlToken{typ: yamlKeyToken, startMark: startMark})
	return true
}

// Queues the colon indicator, first inserting the key and mapping start for a pending simple key
func yamlParserFetchValue(p *yamlParser) bool {
	k := &p.simpleKeys[len(p.simpleKeys)-1]
	if k.possible {
		yamlInsertToken(p, k.tokenNumber-p.tokensParsed, yamlToken{typ: yamlKeyToken, startMark: k.mark})
		if !yamlParserRollIndent(p, k.mark.column, k.tokenNumber, yamlBlockMappingStartToken, k.mark) {
			return false
		}
		k.possible = false
		p.simpleKeyAllowed = false
	} else {
		if p.flowLevel == 0 {
			if !p.simpleKeyAllowed {
				return yamlParserSetScannerError(p, "", p.mark, "mapping values are not allowed in this context")
			}
			if !yamlParserRollIndent(p, p.mark.column, -1, yamlBlockMappingStartToken, p.mark) {
				return false
			}
		}
		p.simpleKeyAllowed = p.flowLevel == 0
	}
	startMark := p.mark
	yamlSkip(p)
	yamlEnqueueToken(p, yamlToken{typ: yamlValueToken, startMark: startMark})
	return true
}

func yamlParserFetchAnchor(p *yamlParser, typ yamlTokenType) bool {
	if !yamlParserSaveSimpleKey(p) {
		return false
	}
	p.simpleKeyAllowed = false
	var t yamlToken
	if !yamlParserScanAnchor(p, &t, typ) {
		return false
	}
	yamlEnqueueToken(p, t)
	return true
}

func yamlParserFetchTag(p *yamlParser) bool {
	if !yamlParserSaveSimpleKey(p) {
		return false
	}
	p.simpleKeyAllowed = false
	var t yamlToken
	if !yamlParserScanTag(p, &t) {
		return false
	}
	yamlEnqueueToken(p, t)
	return true
}

func yamlParserFetchBlockScalar(p *yamlParser, literal bool) bool {
	if !yamlParserRemoveSimpleKey(p) {
		return false
	}
	p.simpleKeyAllowed = true
	var t yamlToken
	if !yamlParserScanBlockScalar(p, &t, literal) {
		return false
	}
	yamlEnqueueToken(p, t)
	return true
}

func yamlParserFetchFlowScalar(p *yamlParser, single bool) bool {
	if !yamlParserSaveSimpleKey(p) {
		return false
	}
	p.simpleKeyAllowed = false
	var t yamlToken
	if !yamlParserScanFlowScalar(p, &t, single) {
		return false
	}
	yamlEnqueueToken(p, t)
	return true
}

func yamlParserFetchPlainScalar(p *yamlParser) bool {
	if !yamlParserSaveSimpleKey(p) {
		return false
	}
	p.simpleKeyAllowed = false
	var t yamlToken
	if !yamlParserScanPlainScalar(p, &t) {
		return false
	}
	yamlEnqueueToken(p, t)
	return true
}

// Skips spaces, permitted tabs, comments, line breaks and column zero byte order marks
func yamlParserScanToNextToken(p *yamlParser) bool {
	for {
		if !yamlCache(p, 1) {
			return false
		}
		if p.mark.column == 0 && yamlIsBOM(p, 0) {
			yamlSkip(p)
		}
		if !yamlCache(p, 1) {
			return false
		}
		for yamlAt(p, 0) == ' ' || ((p.flowLevel != 0 || !p.simpleKeyAllowed) && yamlAt(p, 0) == '\t') {
			yamlSkip(p)
			if !yamlCache(p, 1) {
				return false
			}
		}
		if yamlAt(p, 0) == '#' {
			for !yamlIsBreakZ(p, 0) {
				yamlSkip(p)
				if !yamlCache(p, 1) {
					return false
				}
			}
		}
		if !yamlIsBreak(p, 0) {
			break
		}
		if !yamlCache(p, 2) {
			return false
		}
		yamlSkipLine(p)
		if p.flowLevel == 0 {
			p.simpleKeyAllowed = true
		}
	}
	return true
}

// Reads a YAML or TAG directive line including its trailing comment and line break
func yamlParserScanDirective(p *yamlParser, t *yamlToken) bool {
	startMark := p.mark
	yamlSkip(p)
	name, ok := yamlParserScanDirectiveName(p, startMark)
	if !ok {
		return false
	}
	switch string(name) {
	case "YAML":
		major, minor, ok := yamlParserScanVersionDirectiveValue(p, startMark)
		if !ok {
			return false
		}
		*t = yamlToken{typ: yamlVersionDirectiveToken, startMark: startMark, major: major, minor: minor}
	case "TAG":
		handle, prefix, ok := yamlParserScanTagDirectiveValue(p, startMark)
		if !ok {
			return false
		}
		*t = yamlToken{typ: yamlTagDirectiveToken, startMark: startMark, handle: handle, suffix: prefix}
	default:
		return yamlParserSetScannerError(p, "while scanning a directive", startMark, "found unknown directive name")
	}
	if !yamlCache(p, 1) {
		return false
	}
	for yamlIsBlank(p, 0) {
		yamlSkip(p)
		if !yamlCache(p, 1) {
			return false
		}
	}
	if yamlAt(p, 0) == '#' {
		for !yamlIsBreakZ(p, 0) {
			yamlSkip(p)
			if !yamlCache(p, 1) {
				return false
			}
		}
	}
	if !yamlIsBreakZ(p, 0) {
		return yamlParserSetScannerError(p, "while scanning a directive", startMark, "did not find expected comment or line break")
	}
	if yamlIsBreak(p, 0) {
		if !yamlCache(p, 2) {
			return false
		}
		yamlSkipLine(p)
	}
	return true
}

func yamlParserScanDirectiveName(p *yamlParser, startMark yamlMark) ([]byte, bool) {
	s := yamlNewString()
	if !yamlCache(p, 1) {
		return nil, false
	}
	for yamlIsAlpha(p, 0) {
		s = yamlRead(p, s)
		if !yamlCache(p, 1) {
			return nil, false
		}
	}
	if len(s) == 0 {
		return nil, yamlParserSetScannerError(p, "while scanning a directive", startMark, "could not find expected directive name")
	}
	if !yamlIsBlankZ(p, 0) {
		return nil, yamlParserSetScannerError(p, "while scanning a directive", startMark, "found unexpected non-alphabetical character")
	}
	return s, true
}

func yamlParserScanVersionDirectiveValue(p *yamlParser, startMark yamlMark) (int32, int32, bool) {
	if !yamlCache(p, 1) {
		return 0, 0, false
	}
	for yamlIsBlank(p, 0) {
		yamlSkip(p)
		if !yamlCache(p, 1) {
			return 0, 0, false
		}
	}
	major, ok := yamlParserScanVersionDirectiveNumber(p, startMark)
	if !ok {
		return 0, 0, false
	}
	if yamlAt(p, 0) != '.' {
		return 0, 0, yamlParserSetScannerError(p, "while scanning a %YAML directive", startMark, "did not find expected digit or '.' character")
	}
	yamlSkip(p)
	minor, ok := yamlParserScanVersionDirectiveNumber(p, startMark)
	if !ok {
		return 0, 0, false
	}
	return major, minor, true
}

func yamlParserScanVersionDirectiveNumber(p *yamlParser, startMark yamlMark) (int32, bool) {
	var value int32
	length := 0
	if !yamlCache(p, 1) {
		return 0, false
	}
	for yamlIsDigit(p, 0) {
		length++
		if length > libyamlMaxNumberLength {
			return 0, yamlParserSetScannerError(p, "while scanning a %YAML directive", startMark, "found extremely long version number")
		}
		value = value*10 + yamlAsDigit(p, 0)
		yamlSkip(p)
		if !yamlCache(p, 1) {
			return 0, false
		}
	}
	if length == 0 {
		return 0, yamlParserSetScannerError(p, "while scanning a %YAML directive", startMark, "did not find expected version number")
	}
	return value, true
}

func yamlParserScanTagDirectiveValue(p *yamlParser, startMark yamlMark) ([]byte, []byte, bool) {
	if !yamlCache(p, 1) {
		return nil, nil, false
	}
	for yamlIsBlank(p, 0) {
		yamlSkip(p)
		if !yamlCache(p, 1) {
			return nil, nil, false
		}
	}
	handle, ok := yamlParserScanTagHandle(p, true, startMark)
	if !ok {
		return nil, nil, false
	}
	if !yamlCache(p, 1) {
		return nil, nil, false
	}
	if !yamlIsBlank(p, 0) {
		return nil, nil, yamlParserSetScannerError(p, "while scanning a %TAG directive", startMark, "did not find expected whitespace")
	}
	for yamlIsBlank(p, 0) {
		yamlSkip(p)
		if !yamlCache(p, 1) {
			return nil, nil, false
		}
	}
	prefix, ok := yamlParserScanTagURI(p, true, nil, startMark)
	if !ok {
		return nil, nil, false
	}
	if !yamlCache(p, 1) {
		return nil, nil, false
	}
	if !yamlIsBlankZ(p, 0) {
		return nil, nil, yamlParserSetScannerError(p, "while scanning a %TAG directive", startMark, "did not find expected whitespace or line break")
	}
	return handle, prefix, true
}

func yamlParserScanAnchor(p *yamlParser, t *yamlToken, typ yamlTokenType) bool {
	s := yamlNewString()
	startMark := p.mark
	yamlSkip(p)
	if !yamlCache(p, 1) {
		return false
	}
	length := 0
	for yamlIsAlpha(p, 0) {
		s = yamlRead(p, s)
		if !yamlCache(p, 1) {
			return false
		}
		length++
	}
	if length == 0 || !(yamlIsBlankZ(p, 0) || strings.IndexByte("?:,]}%@`", yamlAt(p, 0)) >= 0) {
		context := "while scanning an alias"
		if typ == yamlAnchorToken {
			context = "while scanning an anchor"
		}
		return yamlParserSetScannerError(p, context, startMark, "did not find expected alphabetic or numeric character")
	}
	*t = yamlToken{typ: typ, startMark: startMark, value: s}
	return true
}

// Reads a verbatim, shorthand or bare tag and splits it into handle and suffix
func yamlParserScanTag(p *yamlParser, t *yamlToken) bool {
	var handle, suffix []byte
	var ok bool
	startMark := p.mark
	if !yamlCache(p, 2) {
		return false
	}
	if yamlAt(p, 1) == '<' {
		handle = yamlNewString()
		yamlSkip(p)
		yamlSkip(p)
		if suffix, ok = yamlParserScanTagURI(p, false, nil, startMark); !ok {
			return false
		}
		if yamlAt(p, 0) != '>' {
			return yamlParserSetScannerError(p, "while scanning a tag", startMark, "did not find the expected '>'")
		}
		yamlSkip(p)
	} else {
		if handle, ok = yamlParserScanTagHandle(p, false, startMark); !ok {
			return false
		}
		if handle[0] == '!' && len(handle) > 1 && handle[len(handle)-1] == '!' {
			if suffix, ok = yamlParserScanTagURI(p, false, nil, startMark); !ok {
				return false
			}
		} else {
			if suffix, ok = yamlParserScanTagURI(p, false, handle, startMark); !ok {
				return false
			}
			handle = append(yamlNewString(), '!')
			if len(suffix) == 0 {
				handle, suffix = suffix, handle
			}
		}
	}
	if !yamlCache(p, 1) {
		return false
	}
	if !yamlIsBlankZ(p, 0) {
		return yamlParserSetScannerError(p, "while scanning a tag", startMark, "did not find expected whitespace or line break")
	}
	*t = yamlToken{typ: yamlTagToken, startMark: startMark, handle: handle, suffix: suffix}
	return true
}

func yamlParserScanTagHandle(p *yamlParser, directive bool, startMark yamlMark) ([]byte, bool) {
	s := yamlNewString()
	if !yamlCache(p, 1) {
		return nil, false
	}
	if yamlAt(p, 0) != '!' {
		context := "while scanning a tag"
		if directive {
			context = "while scanning a tag directive"
		}
		return nil, yamlParserSetScannerError(p, context, startMark, "did not find expected '!'")
	}
	s = yamlRead(p, s)
	if !yamlCache(p, 1) {
		return nil, false
	}
	for yamlIsAlpha(p, 0) {
		s = yamlRead(p, s)
		if !yamlCache(p, 1) {
			return nil, false
		}
	}
	if yamlAt(p, 0) == '!' {
		s = yamlRead(p, s)
	} else if directive && !(len(s) == 1 && s[0] == '!') {
		return nil, yamlParserSetScannerError(p, "while parsing a tag directive", startMark, "did not find expected '!'")
	}
	return s, true
}

// Collects URI characters and percent escapes after the head handle, cut at the first NUL
func yamlParserScanTagURI(p *yamlParser, directive bool, head []byte, startMark yamlMark) ([]byte, bool) {
	length := len(head)
	s := yamlNewString()
	if length > 1 {
		s = append(s, head[1:]...)
	}
	if !yamlCache(p, 1) {
		return nil, false
	}
	for yamlIsAlpha(p, 0) || strings.IndexByte(";/?:@&=+$,.!~*'()[]%", yamlAt(p, 0)) >= 0 {
		if yamlAt(p, 0) == '%' {
			var ok bool
			if s, ok = yamlParserScanURIEscapes(p, directive, startMark, s); !ok {
				return nil, false
			}
		} else {
			s = yamlRead(p, s)
		}
		length++
		if !yamlCache(p, 1) {
			return nil, false
		}
	}
	if length == 0 {
		context := "while parsing a tag"
		if directive {
			context = "while parsing a %TAG directive"
		}
		return nil, yamlParserSetScannerError(p, context, startMark, "did not find expected tag URI")
	}
	return yamlCString(s), true
}

// Decodes one percent escaped UTF-8 sequence, checking leading and trailing octet shapes
func yamlParserScanURIEscapes(p *yamlParser, directive bool, startMark yamlMark, s []byte) ([]byte, bool) {
	context := "while parsing a tag"
	if directive {
		context = "while parsing a %TAG directive"
	}
	width := 0
	for {
		if !yamlCache(p, 3) {
			return nil, false
		}
		if !(yamlAt(p, 0) == '%' && yamlIsHex(p, 1) && yamlIsHex(p, 2)) {
			return nil, yamlParserSetScannerError(p, context, startMark, "did not find URI escaped octet")
		}
		octet := byte(yamlAsHex(p, 1)<<4 + yamlAsHex(p, 2))
		if width == 0 {
			width = yamlWidth(octet)
			if width == 0 {
				return nil, yamlParserSetScannerError(p, context, startMark, "found an incorrect leading UTF-8 octet")
			}
		} else if octet&0xC0 != 0x80 {
			return nil, yamlParserSetScannerError(p, context, startMark, "found an incorrect trailing UTF-8 octet")
		}
		s = append(s, octet)
		yamlSkip(p)
		yamlSkip(p)
		yamlSkip(p)
		width--
		if width == 0 {
			return s, true
		}
	}
}

// Reads a literal or folded scalar with its chomping and indentation indicators
func yamlParserScanBlockScalar(p *yamlParser, t *yamlToken, literal bool) bool {
	s := yamlNewString()
	leadingBreak := yamlNewString()
	trailingBreaks := yamlNewString()
	chomping := 0
	var increment, indent int32
	leadingBlank, trailingBlank := false, false
	startMark := p.mark
	yamlSkip(p)
	if !yamlCache(p, 1) {
		return false
	}
	if yamlAt(p, 0) == '+' || yamlAt(p, 0) == '-' {
		chomping = -1
		if yamlAt(p, 0) == '+' {
			chomping = 1
		}
		yamlSkip(p)
		if !yamlCache(p, 1) {
			return false
		}
		if yamlIsDigit(p, 0) {
			if yamlAt(p, 0) == '0' {
				return yamlParserSetScannerError(p, "while scanning a block scalar", startMark, "found an indentation indicator equal to 0")
			}
			increment = yamlAsDigit(p, 0)
			yamlSkip(p)
		}
	} else if yamlIsDigit(p, 0) {
		if yamlAt(p, 0) == '0' {
			return yamlParserSetScannerError(p, "while scanning a block scalar", startMark, "found an indentation indicator equal to 0")
		}
		increment = yamlAsDigit(p, 0)
		yamlSkip(p)
		if !yamlCache(p, 1) {
			return false
		}
		if yamlAt(p, 0) == '+' || yamlAt(p, 0) == '-' {
			chomping = -1
			if yamlAt(p, 0) == '+' {
				chomping = 1
			}
			yamlSkip(p)
		}
	}
	if !yamlCache(p, 1) {
		return false
	}
	for yamlIsBlank(p, 0) {
		yamlSkip(p)
		if !yamlCache(p, 1) {
			return false
		}
	}
	if yamlAt(p, 0) == '#' {
		for !yamlIsBreakZ(p, 0) {
			yamlSkip(p)
			if !yamlCache(p, 1) {
				return false
			}
		}
	}
	if !yamlIsBreakZ(p, 0) {
		return yamlParserSetScannerError(p, "while scanning a block scalar", startMark, "did not find expected comment or line break")
	}
	if yamlIsBreak(p, 0) {
		if !yamlCache(p, 2) {
			return false
		}
		yamlSkipLine(p)
	}
	if increment != 0 {
		if p.indent >= 0 {
			indent = p.indent + increment
		} else {
			indent = increment
		}
	}
	if !yamlParserScanBlockScalarBreaks(p, &indent, &trailingBreaks, startMark) {
		return false
	}
	if !yamlCache(p, 1) {
		return false
	}
	for int32(p.mark.column) == indent && !yamlIsZ(p, 0) {
		trailingBlank = yamlIsBlank(p, 0)
		if !literal && len(leadingBreak) > 0 && leadingBreak[0] == '\n' && !leadingBlank && !trailingBlank {
			if len(trailingBreaks) == 0 {
				s = append(s, ' ')
			}
		} else {
			s = append(s, leadingBreak...)
		}
		leadingBreak = leadingBreak[:0]
		s = append(s, trailingBreaks...)
		trailingBreaks = trailingBreaks[:0]
		leadingBlank = yamlIsBlank(p, 0)
		for !yamlIsBreakZ(p, 0) {
			s = yamlRead(p, s)
			if !yamlCache(p, 1) {
				return false
			}
		}
		if !yamlCache(p, 2) {
			return false
		}
		leadingBreak = yamlReadLine(p, leadingBreak)
		if !yamlParserScanBlockScalarBreaks(p, &indent, &trailingBreaks, startMark) {
			return false
		}
	}
	if chomping != -1 {
		s = append(s, leadingBreak...)
	}
	if chomping == 1 {
		s = append(s, trailingBreaks...)
	}
	*t = yamlToken{typ: yamlScalarToken, startMark: startMark, value: s}
	return true
}

// Consumes indentation and empty lines before literal or folded content and settles the auto indent
func yamlParserScanBlockScalarBreaks(p *yamlParser, indent *int32, breaks *[]byte, startMark yamlMark) bool {
	var maxIndent int32
	for {
		if !yamlCache(p, 1) {
			return false
		}
		for (*indent == 0 || int32(p.mark.column) < *indent) && yamlIsSpace(p, 0) {
			yamlSkip(p)
			if !yamlCache(p, 1) {
				return false
			}
		}
		if int32(p.mark.column) > maxIndent {
			maxIndent = int32(p.mark.column)
		}
		if (*indent == 0 || int32(p.mark.column) < *indent) && yamlIsTab(p, 0) {
			return yamlParserSetScannerError(p, "while scanning a block scalar", startMark, "found a tab character where an indentation space is expected")
		}
		if !yamlIsBreak(p, 0) {
			break
		}
		if !yamlCache(p, 2) {
			return false
		}
		*breaks = yamlReadLine(p, *breaks)
	}
	if *indent == 0 {
		*indent = maxIndent
		if *indent < p.indent+1 {
			*indent = p.indent + 1
		}
		if *indent < 1 {
			*indent = 1
		}
	}
	return true
}

// Reads a single or double quoted scalar with escapes and line folding
func yamlParserScanFlowScalar(p *yamlParser, t *yamlToken, single bool) bool {
	s := yamlNewString()
	leadingBreak := yamlNewString()
	trailingBreaks := yamlNewString()
	whitespaces := yamlNewString()
	quote := byte('"')
	if single {
		quote = '\''
	}
	startMark := p.mark
	yamlSkip(p)
	for {
		if !yamlCache(p, 4) {
			return false
		}
		if p.mark.column == 0 &&
			((yamlAt(p, 0) == '-' && yamlAt(p, 1) == '-' && yamlAt(p, 2) == '-') ||
				(yamlAt(p, 0) == '.' && yamlAt(p, 1) == '.' && yamlAt(p, 2) == '.')) &&
			yamlIsBlankZ(p, 3) {
			return yamlParserSetScannerError(p, "while scanning a quoted scalar", startMark, "found unexpected document indicator")
		}
		if yamlIsZ(p, 0) {
			return yamlParserSetScannerError(p, "while scanning a quoted scalar", startMark, "found unexpected end of stream")
		}
		if !yamlCache(p, 2) {
			return false
		}
		leadingBlanks := false
		for !yamlIsBlankZ(p, 0) {
			if single && yamlAt(p, 0) == '\'' && yamlAt(p, 1) == '\'' {
				s = append(s, '\'')
				yamlSkip(p)
				yamlSkip(p)
			} else if yamlAt(p, 0) == quote {
				break
			} else if !single && yamlAt(p, 0) == '\\' && yamlIsBreak(p, 1) {
				if !yamlCache(p, 3) {
					return false
				}
				yamlSkip(p)
				yamlSkipLine(p)
				leadingBlanks = true
				break
			} else if !single && yamlAt(p, 0) == '\\' {
				codeLength := 0
				switch yamlAt(p, 1) {
				case '0':
					s = append(s, 0)
				case 'a':
					s = append(s, '\x07')
				case 'b':
					s = append(s, '\x08')
				case 't', '\t':
					s = append(s, '\x09')
				case 'n':
					s = append(s, '\x0A')
				case 'v':
					s = append(s, '\x0B')
				case 'f':
					s = append(s, '\x0C')
				case 'r':
					s = append(s, '\x0D')
				case 'e':
					s = append(s, '\x1B')
				case ' ':
					s = append(s, '\x20')
				case '"':
					s = append(s, '"')
				case '\\':
					s = append(s, '\\')
				case 'N':
					s = append(s, 0xC2, 0x85)
				case '_':
					s = append(s, 0xC2, 0xA0)
				case 'L':
					s = append(s, 0xE2, 0x80, 0xA8)
				case 'P':
					s = append(s, 0xE2, 0x80, 0xA9)
				case 'x':
					codeLength = 2
				case 'u':
					codeLength = 4
				case 'U':
					codeLength = 8
				case '/':
					s = append(s, '/')
				default:
					return yamlParserSetScannerError(p, "while parsing a quoted scalar", startMark, "found unknown escape character")
				}
				yamlSkip(p)
				yamlSkip(p)
				if codeLength != 0 {
					if !yamlCache(p, codeLength) {
						return false
					}
					value := 0
					for k := 0; k < codeLength; k++ {
						if !yamlIsHex(p, k) {
							return yamlParserSetScannerError(p, "while parsing a quoted scalar", startMark, "did not find expected hexdecimal number")
						}
						value = (value << 4) + yamlAsHex(p, k)
					}
					if (value >= 0xD800 && value <= 0xDFFF) || value > 0x10FFFF {
						return yamlParserSetScannerError(p, "while parsing a quoted scalar", startMark, "found invalid Unicode character escape code")
					}
					s = utf8.AppendRune(s, rune(value))
					for k := 0; k < codeLength; k++ {
						yamlSkip(p)
					}
				}
			} else {
				s = yamlRead(p, s)
			}
			if !yamlCache(p, 2) {
				return false
			}
		}
		if yamlAt(p, 0) == quote {
			break
		}
		if !yamlCache(p, 1) {
			return false
		}
		for yamlIsBlank(p, 0) || yamlIsBreak(p, 0) {
			if yamlIsBlank(p, 0) {
				if !leadingBlanks {
					whitespaces = yamlRead(p, whitespaces)
				} else {
					yamlSkip(p)
				}
			} else {
				if !yamlCache(p, 2) {
					return false
				}
				if !leadingBlanks {
					whitespaces = whitespaces[:0]
					leadingBreak = yamlReadLine(p, leadingBreak)
					leadingBlanks = true
				} else {
					trailingBreaks = yamlReadLine(p, trailingBreaks)
				}
			}
			if !yamlCache(p, 1) {
				return false
			}
		}
		if leadingBlanks {
			if len(leadingBreak) > 0 && leadingBreak[0] == '\n' {
				if len(trailingBreaks) == 0 {
					s = append(s, ' ')
				} else {
					s = append(s, trailingBreaks...)
					trailingBreaks = trailingBreaks[:0]
				}
				leadingBreak = leadingBreak[:0]
			} else {
				s = append(s, leadingBreak...)
				s = append(s, trailingBreaks...)
				leadingBreak = leadingBreak[:0]
				trailingBreaks = trailingBreaks[:0]
			}
		} else {
			s = append(s, whitespaces...)
			whitespaces = whitespaces[:0]
		}
	}
	yamlSkip(p)
	*t = yamlToken{typ: yamlScalarToken, startMark: startMark, value: s}
	return true
}

// Reads an unquoted scalar across lines, stopping at indicators, comments, document markers or dedent
func yamlParserScanPlainScalar(p *yamlParser, t *yamlToken) bool {
	s := yamlNewString()
	leadingBreak := yamlNewString()
	trailingBreaks := yamlNewString()
	whitespaces := yamlNewString()
	leadingBlanks := false
	indent := p.indent + 1
	startMark := p.mark
	for {
		if !yamlCache(p, 4) {
			return false
		}
		if p.mark.column == 0 &&
			((yamlAt(p, 0) == '-' && yamlAt(p, 1) == '-' && yamlAt(p, 2) == '-') ||
				(yamlAt(p, 0) == '.' && yamlAt(p, 1) == '.' && yamlAt(p, 2) == '.')) &&
			yamlIsBlankZ(p, 3) {
			break
		}
		if yamlAt(p, 0) == '#' {
			break
		}
		for !yamlIsBlankZ(p, 0) {
			if p.flowLevel != 0 && yamlAt(p, 0) == ':' && !yamlIsBlankZ(p, 1) {
				return yamlParserSetScannerError(p, "while scanning a plain scalar", startMark, "found unexpected ':'")
			}
			if (yamlAt(p, 0) == ':' && yamlIsBlankZ(p, 1)) ||
				(p.flowLevel != 0 && strings.IndexByte(",:?[]{}", yamlAt(p, 0)) >= 0) {
				break
			}
			if leadingBlanks || len(whitespaces) != 0 {
				if leadingBlanks {
					if len(leadingBreak) > 0 && leadingBreak[0] == '\n' {
						if len(trailingBreaks) == 0 {
							s = append(s, ' ')
						} else {
							s = append(s, trailingBreaks...)
							trailingBreaks = trailingBreaks[:0]
						}
						leadingBreak = leadingBreak[:0]
					} else {
						s = append(s, leadingBreak...)
						s = append(s, trailingBreaks...)
						leadingBreak = leadingBreak[:0]
						trailingBreaks = trailingBreaks[:0]
					}
					leadingBlanks = false
				} else {
					s = append(s, whitespaces...)
					whitespaces = whitespaces[:0]
				}
			}
			s = yamlRead(p, s)
			if !yamlCache(p, 2) {
				return false
			}
		}
		if !(yamlIsBlank(p, 0) || yamlIsBreak(p, 0)) {
			break
		}
		if !yamlCache(p, 1) {
			return false
		}
		for yamlIsBlank(p, 0) || yamlIsBreak(p, 0) {
			if yamlIsBlank(p, 0) {
				if leadingBlanks && int32(p.mark.column) < indent && yamlIsTab(p, 0) {
					return yamlParserSetScannerError(p, "while scanning a plain scalar", startMark, "found a tab character that violates indentation")
				}
				if !leadingBlanks {
					whitespaces = yamlRead(p, whitespaces)
				} else {
					yamlSkip(p)
				}
			} else {
				if !yamlCache(p, 2) {
					return false
				}
				if !leadingBlanks {
					whitespaces = whitespaces[:0]
					leadingBreak = yamlReadLine(p, leadingBreak)
					leadingBlanks = true
				} else {
					trailingBreaks = yamlReadLine(p, trailingBreaks)
				}
			}
			if !yamlCache(p, 1) {
				return false
			}
		}
		if p.flowLevel == 0 && int32(p.mark.column) < indent {
			break
		}
	}
	*t = yamlToken{typ: yamlScalarToken, startMark: startMark, value: s}
	if leadingBlanks {
		p.simpleKeyAllowed = true
	}
	return true
}

func yamlPeekToken(p *yamlParser) *yamlToken {
	if p.tokenAvailable || yamlParserFetchMoreTokens(p) {
		return &p.tokens[p.tokensHead]
	}
	return nil
}

func yamlSkipToken(p *yamlParser) {
	p.tokenAvailable = false
	p.tokensParsed++
	p.streamEndProduced = p.tokens[p.tokensHead].typ == yamlStreamEndToken
	p.tokensHead++
}

func yamlPushState(p *yamlParser, s yamlParserState) bool {
	return libyamlPush(p, &p.states, &p.statesCap, libyamlSizeofState, s)
}

func yamlPopState(p *yamlParser) yamlParserState {
	s := p.states[len(p.states)-1]
	p.states = p.states[:len(p.states)-1]
	return s
}

func yamlPushMark(p *yamlParser, m yamlMark) bool {
	return libyamlPush(p, &p.marks, &p.marksCap, libyamlSizeofMark, yamlStackMark{line: m.line, column: m.column})
}

func yamlPopMark(p *yamlParser) yamlMark {
	m := p.marks[len(p.marks)-1]
	p.marks = p.marks[:len(p.marks)-1]
	return yamlMark{line: m.line, column: m.column}
}

func yamlParserSetParserError(p *yamlParser, problem string, problemMark yamlMark) bool {
	p.errorType = yamlParserError
	p.problem = problem
	p.problemMark = problemMark
	return false
}

func yamlParserSetParserErrorContext(p *yamlParser, context string, contextMark yamlMark, problem string, problemMark yamlMark) bool {
	p.errorType = yamlParserError
	p.context = context
	p.contextMark = contextMark
	p.problem = problem
	p.problemMark = problemMark
	return false
}

// Produces the next event, doing nothing once the stream ended or an error was recorded
func yamlParserParse(p *yamlParser, ev *yamlEvent) bool {
	*ev = yamlEvent{}
	if p.streamEndProduced || p.errorType != yamlNoError || p.state == yamlParseEndState {
		return true
	}
	switch p.state {
	case yamlParseStreamStartState:
		return yamlParserParseStreamStart(p, ev)
	case yamlParseImplicitDocumentStartState:
		return yamlParserParseDocumentStart(p, ev, true)
	case yamlParseDocumentStartState:
		return yamlParserParseDocumentStart(p, ev, false)
	case yamlParseDocumentContentState:
		return yamlParserParseDocumentContent(p, ev)
	case yamlParseDocumentEndState:
		return yamlParserParseDocumentEnd(p, ev)
	case yamlParseBlockNodeState:
		return yamlParserParseNode(p, ev, true, false)
	case yamlParseBlockNodeOrIndentlessSequenceState:
		return yamlParserParseNode(p, ev, true, true)
	case yamlParseFlowNodeState:
		return yamlParserParseNode(p, ev, false, false)
	case yamlParseBlockSequenceFirstEntryState:
		return yamlParserParseBlockSequenceEntry(p, ev, true)
	case yamlParseBlockSequenceEntryState:
		return yamlParserParseBlockSequenceEntry(p, ev, false)
	case yamlParseIndentlessSequenceEntryState:
		return yamlParserParseIndentlessSequenceEntry(p, ev)
	case yamlParseBlockMappingFirstKeyState:
		return yamlParserParseBlockMappingKey(p, ev, true)
	case yamlParseBlockMappingKeyState:
		return yamlParserParseBlockMappingKey(p, ev, false)
	case yamlParseBlockMappingValueState:
		return yamlParserParseBlockMappingValue(p, ev)
	case yamlParseFlowSequenceFirstEntryState:
		return yamlParserParseFlowSequenceEntry(p, ev, true)
	case yamlParseFlowSequenceEntryState:
		return yamlParserParseFlowSequenceEntry(p, ev, false)
	case yamlParseFlowSequenceEntryMappingKeyState:
		return yamlParserParseFlowSequenceEntryMappingKey(p, ev)
	case yamlParseFlowSequenceEntryMappingValueState:
		return yamlParserParseFlowSequenceEntryMappingValue(p, ev)
	case yamlParseFlowSequenceEntryMappingEndState:
		return yamlParserParseFlowSequenceEntryMappingEnd(p, ev)
	case yamlParseFlowMappingFirstKeyState:
		return yamlParserParseFlowMappingKey(p, ev, true)
	case yamlParseFlowMappingKeyState:
		return yamlParserParseFlowMappingKey(p, ev, false)
	case yamlParseFlowMappingValueState:
		return yamlParserParseFlowMappingValue(p, ev, false)
	case yamlParseFlowMappingEmptyValueState:
		return yamlParserParseFlowMappingValue(p, ev, true)
	}
	return false
}

func yamlParserParseStreamStart(p *yamlParser, ev *yamlEvent) bool {
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	if token.typ != yamlStreamStartToken {
		return yamlParserSetParserError(p, "did not find expected <stream-start>", token.startMark)
	}
	p.state = yamlParseImplicitDocumentStartState
	ev.typ = yamlStreamStartEvent
	yamlSkipToken(p)
	return true
}

// Starts an implicit or explicit document after its directives, or ends the stream
func yamlParserParseDocumentStart(p *yamlParser, ev *yamlEvent, implicit bool) bool {
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	if !implicit {
		for token.typ == yamlDocumentEndToken {
			yamlSkipToken(p)
			if token = yamlPeekToken(p); token == nil {
				return false
			}
		}
	}
	if implicit && token.typ != yamlVersionDirectiveToken && token.typ != yamlTagDirectiveToken &&
		token.typ != yamlDocumentStartToken && token.typ != yamlStreamEndToken {
		if _, ok := yamlParserProcessDirectives(p); !ok {
			return false
		}
		if !yamlPushState(p, yamlParseDocumentEndState) {
			return false
		}
		p.state = yamlParseBlockNodeState
		ev.typ = yamlDocumentStartEvent
		return true
	}
	if token.typ != yamlStreamEndToken {
		tagDirectives, ok := yamlParserProcessDirectives(p)
		if !ok {
			return false
		}
		if token = yamlPeekToken(p); token == nil {
			return false
		}
		if token.typ != yamlDocumentStartToken {
			return yamlParserSetParserError(p, "did not find expected <document start>", token.startMark)
		}
		if !yamlPushState(p, yamlParseDocumentEndState) {
			return false
		}
		p.state = yamlParseDocumentContentState
		ev.typ = yamlDocumentStartEvent
		ev.tagDirectives = tagDirectives
		yamlSkipToken(p)
		return true
	}
	p.state = yamlParseEndState
	ev.typ = yamlStreamEndEvent
	yamlSkipToken(p)
	return true
}

func yamlParserParseDocumentContent(p *yamlParser, ev *yamlEvent) bool {
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	switch token.typ {
	case yamlVersionDirectiveToken, yamlTagDirectiveToken, yamlDocumentStartToken, yamlDocumentEndToken, yamlStreamEndToken:
		p.state = yamlPopState(p)
		return yamlParserProcessEmptyScalar(ev)
	}
	return yamlParserParseNode(p, ev, true, false)
}

func yamlParserParseDocumentEnd(p *yamlParser, ev *yamlEvent) bool {
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	if token.typ == yamlDocumentEndToken {
		yamlSkipToken(p)
	}
	p.tagDirectives = p.tagDirectives[:0]
	p.state = yamlParseDocumentStartState
	ev.typ = yamlDocumentEndEvent
	return true
}

// Handles an alias or a node with its anchor and tag properties, resolving the tag handle
func yamlParserParseNode(p *yamlParser, ev *yamlEvent, block, indentlessSequence bool) bool {
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	if token.typ == yamlAliasToken {
		p.state = yamlPopState(p)
		ev.typ = yamlAliasEvent
		ev.anchor = token.value
		yamlSkipToken(p)
		return true
	}
	var anchor, tagHandle, tagSuffix, tag []byte
	var tagMark yamlMark
	startMark := token.startMark
	if token.typ == yamlAnchorToken {
		anchor = token.value
		yamlSkipToken(p)
		if token = yamlPeekToken(p); token == nil {
			return false
		}
		if token.typ == yamlTagToken {
			tagHandle, tagSuffix = token.handle, token.suffix
			tagMark = token.startMark
			yamlSkipToken(p)
			if token = yamlPeekToken(p); token == nil {
				return false
			}
		}
	} else if token.typ == yamlTagToken {
		tagHandle, tagSuffix = token.handle, token.suffix
		tagMark = token.startMark
		yamlSkipToken(p)
		if token = yamlPeekToken(p); token == nil {
			return false
		}
		if token.typ == yamlAnchorToken {
			anchor = token.value
			yamlSkipToken(p)
			if token = yamlPeekToken(p); token == nil {
				return false
			}
		}
	}
	if tagHandle != nil {
		if len(tagHandle) == 0 {
			tag = tagSuffix
		} else {
			for _, d := range p.tagDirectives {
				if bytes.Equal(d.handle, tagHandle) {
					tag = append(append(make([]byte, 0, len(d.prefix)+len(tagSuffix)), d.prefix...), tagSuffix...)
					break
				}
			}
			if tag == nil {
				return yamlParserSetParserErrorContext(p, "while parsing a node", startMark, "found undefined tag handle", tagMark)
			}
		}
	}
	if indentlessSequence && token.typ == yamlBlockEntryToken {
		p.state = yamlParseIndentlessSequenceEntryState
		*ev = yamlEvent{typ: yamlSequenceStartEvent, anchor: anchor, tag: tag}
		return true
	}
	switch {
	case token.typ == yamlScalarToken:
		p.state = yamlPopState(p)
		*ev = yamlEvent{typ: yamlScalarEvent, anchor: anchor, tag: tag, value: token.value}
		yamlSkipToken(p)
		return true
	case token.typ == yamlFlowSequenceStartToken:
		p.state = yamlParseFlowSequenceFirstEntryState
		*ev = yamlEvent{typ: yamlSequenceStartEvent, anchor: anchor, tag: tag}
		return true
	case token.typ == yamlFlowMappingStartToken:
		p.state = yamlParseFlowMappingFirstKeyState
		*ev = yamlEvent{typ: yamlMappingStartEvent, anchor: anchor, tag: tag}
		return true
	case block && token.typ == yamlBlockSequenceStartToken:
		p.state = yamlParseBlockSequenceFirstEntryState
		*ev = yamlEvent{typ: yamlSequenceStartEvent, anchor: anchor, tag: tag}
		return true
	case block && token.typ == yamlBlockMappingStartToken:
		p.state = yamlParseBlockMappingFirstKeyState
		*ev = yamlEvent{typ: yamlMappingStartEvent, anchor: anchor, tag: tag}
		return true
	case anchor != nil || tag != nil:
		p.state = yamlPopState(p)
		*ev = yamlEvent{typ: yamlScalarEvent, anchor: anchor, tag: tag, value: []byte{}}
		return true
	}
	context := "while parsing a flow node"
	if block {
		context = "while parsing a block node"
	}
	return yamlParserSetParserErrorContext(p, context, startMark, "did not find expected node content", token.startMark)
}

func yamlParserParseBlockSequenceEntry(p *yamlParser, ev *yamlEvent, first bool) bool {
	if first {
		token := yamlPeekToken(p)
		if token == nil {
			return false
		}
		if !yamlPushMark(p, token.startMark) {
			return false
		}
		yamlSkipToken(p)
	}
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	if token.typ == yamlBlockEntryToken {
		yamlSkipToken(p)
		if token = yamlPeekToken(p); token == nil {
			return false
		}
		if token.typ != yamlBlockEntryToken && token.typ != yamlBlockEndToken {
			if !yamlPushState(p, yamlParseBlockSequenceEntryState) {
				return false
			}
			return yamlParserParseNode(p, ev, true, false)
		}
		p.state = yamlParseBlockSequenceEntryState
		return yamlParserProcessEmptyScalar(ev)
	}
	if token.typ == yamlBlockEndToken {
		p.state = yamlPopState(p)
		yamlPopMark(p)
		ev.typ = yamlSequenceEndEvent
		yamlSkipToken(p)
		return true
	}
	return yamlParserSetParserErrorContext(p, "while parsing a block collection", yamlPopMark(p), "did not find expected '-' indicator", token.startMark)
}

func yamlParserParseIndentlessSequenceEntry(p *yamlParser, ev *yamlEvent) bool {
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	if token.typ == yamlBlockEntryToken {
		yamlSkipToken(p)
		if token = yamlPeekToken(p); token == nil {
			return false
		}
		if token.typ != yamlBlockEntryToken && token.typ != yamlKeyToken && token.typ != yamlValueToken && token.typ != yamlBlockEndToken {
			if !yamlPushState(p, yamlParseIndentlessSequenceEntryState) {
				return false
			}
			return yamlParserParseNode(p, ev, true, false)
		}
		p.state = yamlParseIndentlessSequenceEntryState
		return yamlParserProcessEmptyScalar(ev)
	}
	p.state = yamlPopState(p)
	ev.typ = yamlSequenceEndEvent
	return true
}

func yamlParserParseBlockMappingKey(p *yamlParser, ev *yamlEvent, first bool) bool {
	if first {
		token := yamlPeekToken(p)
		if token == nil {
			return false
		}
		if !yamlPushMark(p, token.startMark) {
			return false
		}
		yamlSkipToken(p)
	}
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	if token.typ == yamlKeyToken {
		yamlSkipToken(p)
		if token = yamlPeekToken(p); token == nil {
			return false
		}
		if token.typ != yamlKeyToken && token.typ != yamlValueToken && token.typ != yamlBlockEndToken {
			if !yamlPushState(p, yamlParseBlockMappingValueState) {
				return false
			}
			return yamlParserParseNode(p, ev, true, true)
		}
		p.state = yamlParseBlockMappingValueState
		return yamlParserProcessEmptyScalar(ev)
	}
	if token.typ == yamlBlockEndToken {
		p.state = yamlPopState(p)
		yamlPopMark(p)
		ev.typ = yamlMappingEndEvent
		yamlSkipToken(p)
		return true
	}
	return yamlParserSetParserErrorContext(p, "while parsing a block mapping", yamlPopMark(p), "did not find expected key", token.startMark)
}

func yamlParserParseBlockMappingValue(p *yamlParser, ev *yamlEvent) bool {
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	if token.typ == yamlValueToken {
		yamlSkipToken(p)
		if token = yamlPeekToken(p); token == nil {
			return false
		}
		if token.typ != yamlKeyToken && token.typ != yamlValueToken && token.typ != yamlBlockEndToken {
			if !yamlPushState(p, yamlParseBlockMappingKeyState) {
				return false
			}
			return yamlParserParseNode(p, ev, true, true)
		}
	}
	p.state = yamlParseBlockMappingKeyState
	return yamlParserProcessEmptyScalar(ev)
}

func yamlParserParseFlowSequenceEntry(p *yamlParser, ev *yamlEvent, first bool) bool {
	if first {
		token := yamlPeekToken(p)
		if token == nil {
			return false
		}
		if !yamlPushMark(p, token.startMark) {
			return false
		}
		yamlSkipToken(p)
	}
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	if token.typ != yamlFlowSequenceEndToken {
		if !first {
			if token.typ != yamlFlowEntryToken {
				return yamlParserSetParserErrorContext(p, "while parsing a flow sequence", yamlPopMark(p), "did not find expected ',' or ']'", token.startMark)
			}
			yamlSkipToken(p)
			if token = yamlPeekToken(p); token == nil {
				return false
			}
		}
		if token.typ == yamlKeyToken {
			p.state = yamlParseFlowSequenceEntryMappingKeyState
			ev.typ = yamlMappingStartEvent
			yamlSkipToken(p)
			return true
		}
		if token.typ != yamlFlowSequenceEndToken {
			if !yamlPushState(p, yamlParseFlowSequenceEntryState) {
				return false
			}
			return yamlParserParseNode(p, ev, false, false)
		}
	}
	p.state = yamlPopState(p)
	yamlPopMark(p)
	ev.typ = yamlSequenceEndEvent
	yamlSkipToken(p)
	return true
}

// Handles the key of a single pair mapping inside a flow sequence, consuming the token when the key is empty
func yamlParserParseFlowSequenceEntryMappingKey(p *yamlParser, ev *yamlEvent) bool {
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	if token.typ != yamlValueToken && token.typ != yamlFlowEntryToken && token.typ != yamlFlowSequenceEndToken {
		if !yamlPushState(p, yamlParseFlowSequenceEntryMappingValueState) {
			return false
		}
		return yamlParserParseNode(p, ev, false, false)
	}
	yamlSkipToken(p)
	p.state = yamlParseFlowSequenceEntryMappingValueState
	return yamlParserProcessEmptyScalar(ev)
}

func yamlParserParseFlowSequenceEntryMappingValue(p *yamlParser, ev *yamlEvent) bool {
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	if token.typ == yamlValueToken {
		yamlSkipToken(p)
		if token = yamlPeekToken(p); token == nil {
			return false
		}
		if token.typ != yamlFlowEntryToken && token.typ != yamlFlowSequenceEndToken {
			if !yamlPushState(p, yamlParseFlowSequenceEntryMappingEndState) {
				return false
			}
			return yamlParserParseNode(p, ev, false, false)
		}
	}
	p.state = yamlParseFlowSequenceEntryMappingEndState
	return yamlParserProcessEmptyScalar(ev)
}

func yamlParserParseFlowSequenceEntryMappingEnd(p *yamlParser, ev *yamlEvent) bool {
	if yamlPeekToken(p) == nil {
		return false
	}
	p.state = yamlParseFlowSequenceEntryState
	ev.typ = yamlMappingEndEvent
	return true
}

func yamlParserParseFlowMappingKey(p *yamlParser, ev *yamlEvent, first bool) bool {
	if first {
		token := yamlPeekToken(p)
		if token == nil {
			return false
		}
		if !yamlPushMark(p, token.startMark) {
			return false
		}
		yamlSkipToken(p)
	}
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	if token.typ != yamlFlowMappingEndToken {
		if !first {
			if token.typ != yamlFlowEntryToken {
				return yamlParserSetParserErrorContext(p, "while parsing a flow mapping", yamlPopMark(p), "did not find expected ',' or '}'", token.startMark)
			}
			yamlSkipToken(p)
			if token = yamlPeekToken(p); token == nil {
				return false
			}
		}
		if token.typ == yamlKeyToken {
			yamlSkipToken(p)
			if token = yamlPeekToken(p); token == nil {
				return false
			}
			if token.typ != yamlValueToken && token.typ != yamlFlowEntryToken && token.typ != yamlFlowMappingEndToken {
				if !yamlPushState(p, yamlParseFlowMappingValueState) {
					return false
				}
				return yamlParserParseNode(p, ev, false, false)
			}
			p.state = yamlParseFlowMappingValueState
			return yamlParserProcessEmptyScalar(ev)
		}
		if token.typ != yamlFlowMappingEndToken {
			if !yamlPushState(p, yamlParseFlowMappingEmptyValueState) {
				return false
			}
			return yamlParserParseNode(p, ev, false, false)
		}
	}
	p.state = yamlPopState(p)
	yamlPopMark(p)
	ev.typ = yamlMappingEndEvent
	yamlSkipToken(p)
	return true
}

func yamlParserParseFlowMappingValue(p *yamlParser, ev *yamlEvent, empty bool) bool {
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	if empty {
		p.state = yamlParseFlowMappingKeyState
		return yamlParserProcessEmptyScalar(ev)
	}
	if token.typ == yamlValueToken {
		yamlSkipToken(p)
		if token = yamlPeekToken(p); token == nil {
			return false
		}
		if token.typ != yamlFlowEntryToken && token.typ != yamlFlowMappingEndToken {
			if !yamlPushState(p, yamlParseFlowMappingKeyState) {
				return false
			}
			return yamlParserParseNode(p, ev, false, false)
		}
	}
	p.state = yamlParseFlowMappingKeyState
	return yamlParserProcessEmptyScalar(ev)
}

func yamlParserProcessEmptyScalar(ev *yamlEvent) bool {
	*ev = yamlEvent{typ: yamlScalarEvent, value: []byte{}}
	return true
}

// Applies the YAML and TAG directives before a document and installs the default tag handles
func yamlParserProcessDirectives(p *yamlParser) ([]yamlTagDirective, bool) {
	haveVersion := false
	var local []yamlTagDirective
	localCap := libyamlInitialStackSize
	token := yamlPeekToken(p)
	if token == nil {
		return nil, false
	}
	for token.typ == yamlVersionDirectiveToken || token.typ == yamlTagDirectiveToken {
		if token.typ == yamlVersionDirectiveToken {
			if haveVersion {
				return nil, yamlParserSetParserError(p, "found duplicate %YAML directive", token.startMark)
			}
			if token.major != 1 || (token.minor != 1 && token.minor != 2) {
				return nil, yamlParserSetParserError(p, "found incompatible YAML document", token.startMark)
			}
			haveVersion = true
		} else {
			value := yamlTagDirective{handle: token.handle, prefix: token.suffix}
			if !yamlParserAppendTagDirective(p, value, false, token.startMark) {
				return nil, false
			}
			if !libyamlPush(p, &local, &localCap, libyamlSizeofTagDirective, value) {
				return nil, false
			}
		}
		yamlSkipToken(p)
		if token = yamlPeekToken(p); token == nil {
			return nil, false
		}
	}
	defaults := [...]yamlTagDirective{
		{handle: []byte("!"), prefix: []byte("!")},
		{handle: []byte("!!"), prefix: []byte("tag:yaml.org,2002:")},
	}
	for _, d := range defaults {
		if !yamlParserAppendTagDirective(p, d, true, token.startMark) {
			return nil, false
		}
	}
	return local, true
}

func yamlParserAppendTagDirective(p *yamlParser, value yamlTagDirective, allowDuplicates bool, mark yamlMark) bool {
	for _, d := range p.tagDirectives {
		if bytes.Equal(value.handle, d.handle) {
			if allowDuplicates {
				return true
			}
			return yamlParserSetParserError(p, "found duplicate %TAG directive", mark)
		}
	}
	return libyamlPush(p, &p.tagDirectives, &p.tagDirectivesCap, libyamlSizeofTagDirective, value)
}
