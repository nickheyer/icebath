package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/settings"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pythonutil "github.com/nickheyer/icebath/pkg/utils/python"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
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
	lower := cases.Lower(language.Und).String(output)
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

// JSON syntax verdict at any nesting depth and number length, the lint error naming the mistake with its line and column
func lintJSONInproc(content string) (bool, string) {
	if hasBOM(content) {
		return false, "invalid JSON: unexpected byte order mark (U+FEFF) at the start of the text (line 1, column 1)"
	}
	err := jsonutil.Validate(content)
	if err == nil {
		return true, ""
	}
	var se *jsonutil.SyntaxError
	if errors.As(err, &se) {
		return false, "invalid JSON: " + se.Msg + " (line " + strconv.Itoa(se.Line) + ", column " + strconv.Itoa(se.Column) + ")"
	}
	return false, "invalid JSON: " + err.Error()
}

// YAML event level syntax verdict matching the ruamel C parser the write gate uses
func lintYAMLInproc(content string) (bool, string) {
	if msg := ruamelCParseError(content); msg != "" {
		return false, msg
	}
	return true, ""
}

// TOML syntax verdict, the lint error naming the first problem with its line and column
func lintTOMLInproc(content string) (bool, string) {
	if msg := tomllibLoadsError(content); msg != "" {
		return false, msg
	}
	return true, ""
}

// Python syntax verdict from parsing the content's bytes, with the error kind, message and position
func lintPythonInproc(content string) (bool, string) {
	if _, err := pythonutil.ParseBytes([]byte(content)); err != nil {
		var se *pythonutil.SyntaxError
		if errors.As(err, &se) {
			loc := ""
			if se.Line != 0 {
				loc = " (line " + strconv.Itoa(se.Line) + ", column " + strconv.Itoa(se.Column) + ")"
			}
			return false, se.Kind.String() + ": " + se.Msg + loc
		}
		return false, err.Error()
	}
	return true, ""
}

// First NaN, Infinity or minus Infinity constant of otherwise valid JSON, empty when none
func jsonNonstandardConstant(text string) string {
	if !strings.Contains(text, "NaN") && !strings.Contains(text, "Infinity") {
		return ""
	}
	body, _ := stripBOM(text)
	if jsonutil.Validate(body) != nil {
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
	ext := cases.Lower(language.Und).String(splitextExt(path))
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
		return &LintResult{Success: true, Skipped: true, Message: fieldsValue(linterCmd)[0] + " skipped: untrusted workspace (add it to lsp.trusted_workspaces to lint " + ext + " here)"}
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
		baseCmd = fieldsValue(linterCmd)[0]
		if !s.hasCommand(baseCmd) {
			return &LintResult{Success: true, Skipped: true, Message: baseCmd + " not available"}
		}
		result = s.exec(strings.ReplaceAll(linterCmd, "{file}", s.escapeNativeToolArg(path)), "", 30*time.Second)
	}
	if result.ExitCode != 0 && looksLikeLinterUnusable(baseCmd, result.Stdout) {
		cleaned := strings.TrimSpace(StripANSI(result.Stdout))
		firstLine := runePrefix(cleaned, 120)
		for _, ln := range splitLinesValue(cleaned) {
			if t := strings.TrimSpace(ln); t != "" {
				firstLine = t
				break
			}
		}
		return &LintResult{Success: true, Skipped: true, Message: baseCmd + " not usable: " + runePrefix(firstLine, 200)}
	}
	res := newLintResult()
	res.Success = result.ExitCode == 0
	res.Output = strings.TrimSpace(result.Stdout)
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
		return &ExecuteResult{Stdout: text, ExitCode: returnCode(exitErr)}
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
func returnCode(exitErr *exec.ExitError) int {
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
	for _, ln := range splitLinesValue(pre.Output) {
		if t := strings.TrimSpace(ln); t != "" {
			preLines[t] = true
		}
	}
	var postLines []string
	for _, ln := range splitLinesValue(post.Output) {
		if t := strings.TrimSpace(ln); t != "" && !preLines[t] {
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
	trusted := lspParseTrustedWorkspaces(settings.LspTrustedWorkspaces.Get(envHomeCtx(s.ctx, s.env)))
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
	return lspStaticRegistryHandles(cases.Lower(language.Und).String(ext))
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
	if lspDiagnosticsBlock == nil {
		return ""
	}
	defer func() {
		if recover() != nil {
			text = ""
		}
	}()
	return lspDiagnosticsBlock(path, diagnostics, header)
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

// Namespace flags and node kinds of the TOML parser, with the bounds that keep its work and stack use in check
const (
	// Most arrays and inline tables one value may nest, keeping the recursive descent well inside the goroutine stack
	tomllibMaxNesting = 10000
	// Most parts a dotted key may have, bounding the per key work of the namespace checks that grows with the square of its length
	tomllibMaxKeyParts  = 1000
	tomllibFrozen       = uint8(1)
	tomllibExplicitNest = uint8(2)
	tomllibKindScalar   = 0
	tomllibKindTable    = 1
	tomllibKindArray    = 2
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
	src     []rune
	nesting int
	data    *tomllibNode
	flags   *tomllibFlags
	scalar  *tomllibNode
}

// TOML syntax verdict of content as the lint message, empty when the document parses
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
		scalar: &tomllibNode{kind: tomllibKindScalar},
	}
	p.loads()
	return ""
}

// Code points of the text with each byte that is not UTF-8 kept as its own stand in, CRLF pairs folded into LF
func tomllibDecode(s string) []rune {
	out := make([]rune, 0, len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			r = tomllibByteStandIn + rune(s[i])
		}
		if r == '\n' && len(out) > 0 && out[len(out)-1] == '\r' {
			out[len(out)-1] = '\n'
		} else {
			out = append(out, r)
		}
		i += size
	}
	return out
}

// Base of the stand ins for bytes that are not UTF-8: byte b becomes this plus b, a surrogate code point no decoded text holds
const tomllibByteStandIn = 0xdc00

// Original bytes of decoded runes, each stand in turned back into the byte it holds
func tomllibBytes(rs []rune) string {
	b := make([]byte, 0, len(rs))
	for _, r := range rs {
		if r >= tomllibByteStandIn+0x80 && r <= tomllibByteStandIn+0xff {
			b = append(b, byte(r-tomllibByteStandIn))
		} else {
			b = utf8.AppendRune(b, r)
		}
	}
	return string(b)
}

// Opens one array or inline table, refusing nesting past the limit at its opening bracket
func (p *tomllibParser) nest(pos int) {
	if p.nesting++; p.nesting > tomllibMaxNesting {
		panic(p.decodeError("Arrays and inline tables nest more than "+strconv.Itoa(tomllibMaxNesting)+" levels deep", pos))
	}
}

// Closes the innermost array or inline table
func (p *tomllibParser) unnest() { p.nesting-- }

// Lint failure for msg with the line and column of pos, or the end of the document
func (p *tomllibParser) decodeError(msg string, pos int) tomllibFailure {
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
	return tomllibFailure{"invalid TOML: " + msg + " (at " + coord + ")"}
}

func (p *tomllibParser) at(pos int) rune {
	if pos < len(p.src) {
		return p.src[pos]
	}
	return -1
}

// Runes from start to stop, both clamped to the end of the source
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

// Encodes code points three bytes each so byte stand ins stay distinct from every other key code point
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

// Key as its parts in Go quoted form joined by dots, the bytes of each part as the document holds them
func tomllibQuoteKey(key []string) string {
	parts := make([]string, len(key))
	for i, k := range key {
		parts[i] = strconv.Quote(tomllibBytes(tomllibUnpack(k)))
	}
	return strings.Join(parts, ".")
}

func tomllibNewTable() *tomllibNode {
	return &tomllibNode{kind: tomllibKindTable, table: map[string]*tomllibNode{}}
}

func (p *tomllibParser) newNestedDict() *tomllibNode {
	return tomllibNewTable()
}

func (p *tomllibParser) newFlags() *tomllibFlags {
	return &tomllibFlags{root: map[string]*tomllibFlagNode{}, pending: map[string]tomllibPending{}}
}

func (p *tomllibParser) addPending(f *tomllibFlags, key []string, flag uint8) {
	f.pending[tomllibPendingID(key, flag)] = tomllibPending{key: key, flag: flag}
}

func (p *tomllibParser) finalizePending(f *tomllibFlags) {
	for _, e := range f.pending {
		p.flagsSet(f, e.key, e.flag, false)
	}
	clear(f.pending)
}

func (p *tomllibParser) unsetAll(f *tomllibFlags, key []string) {
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

// Walks or creates tables along key, nil where a value that is not a table is in the way
func (p *tomllibParser) getOrCreateNest(root *tomllibNode, key []string, accessLists bool) *tomllibNode {
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
				panic(tomllibFailure{"invalid TOML: a key points into an empty array of tables"})
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

// Parses statements one line at a time, running every check of a statement in document order
func (p *tomllibParser) loads() {
	src := p.src
	pos := 0
	p.newOutput()
	var header []string
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

func (p *tomllibParser) newOutput() {
	p.data = p.newNestedDict()
	p.flags = p.newFlags()
}

func (p *tomllibParser) skipChars(pos int, in func(rune) bool) int {
	for pos < len(p.src) && in(p.src[pos]) {
		pos++
	}
	return pos
}

// Returns the index of expect while rejecting illegal characters before it and optionally a missing terminator
func (p *tomllibParser) skipUntil(pos int, expect string, errorOn func(rune) bool, errorOnEOF bool) int {
	newPos := p.index(expect, pos)
	if newPos < 0 {
		newPos = len(p.src)
		if errorOnEOF {
			panic(p.decodeError("Expected "+strconv.Quote(expect), newPos))
		}
	}
	for i := pos; i < newPos; i++ {
		if errorOn(p.src[i]) {
			panic(p.decodeError("Found invalid character "+strconv.QuoteRune(p.src[i]), i))
		}
	}
	return newPos
}

func (p *tomllibParser) skipComment(pos int) int {
	if p.at(pos) == '#' {
		return p.skipUntil(pos+1, "\n", tomllibIllegalBasic, false)
	}
	return pos
}

func (p *tomllibParser) skipCommentsAndArrayWS(pos int) int {
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
	pos++
	pos = p.skipChars(pos, tomllibIsWS)
	pos, key := p.parseKey(pos)
	if p.flagsIs(p.flags, key, tomllibExplicitNest) || p.flagsIs(p.flags, key, tomllibFrozen) {
		panic(p.decodeError("Cannot declare "+tomllibQuoteKey(key)+" twice", pos))
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
	pos += 2
	pos = p.skipChars(pos, tomllibIsWS)
	pos, key := p.parseKey(pos)
	if p.flagsIs(p.flags, key, tomllibFrozen) {
		panic(p.decodeError("Cannot mutate immutable namespace "+tomllibQuoteKey(key), pos))
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
	pos, key, value := p.parseKeyValuePair(pos)
	keyParent, keyStem := key[:len(key)-1], key[len(key)-1]
	absKeyParent := tomllibJoinKey(header, keyParent)
	for i := 1; i < len(key); i++ {
		contKey := tomllibJoinKey(header, key[:i])
		if p.flagsIs(p.flags, contKey, tomllibExplicitNest) {
			panic(p.decodeError("Cannot redefine namespace "+tomllibQuoteKey(contKey), pos))
		}
		p.addPending(p.flags, contKey, tomllibExplicitNest)
	}
	if p.flagsIs(p.flags, absKeyParent, tomllibFrozen) {
		panic(p.decodeError("Cannot mutate immutable namespace "+tomllibQuoteKey(absKeyParent), pos))
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
	start := pos
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
			panic(p.decodeError("Dotted key has more than "+strconv.Itoa(tomllibMaxKeyParts)+" parts", start))
		}
		pos = p.skipChars(pos, tomllibIsWS)
	}
}

func (p *tomllibParser) parseKeyPart(pos int) (int, string) {
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
	pos++
	return p.parseBasicStr(pos, false)
}

func (p *tomllibParser) parseArray(pos int) (int, *tomllibNode) {
	p.nest(pos)
	defer p.unnest()
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
	p.nest(pos)
	defer p.unnest()
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
			panic(p.decodeError("Cannot mutate immutable namespace "+tomllibQuoteKey(key), pos))
		}
		nest := p.getOrCreateNest(nested, keyParent, false)
		if nest == nil {
			panic(p.decodeError("Cannot overwrite a value", pos))
		}
		if _, ok := nest.table[keyStem]; ok {
			panic(p.decodeError("Duplicate inline table key "+strconv.Quote(tomllibBytes(tomllibUnpack(keyStem))), pos))
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
	return p.parseBasicStrEscape(pos, true)
}

func (p *tomllibParser) parseHexChar(pos, hexLen int) (int, []rune) {
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
	return v <= 55295 || (v >= 57344 && v <= 1114111)
}

func (p *tomllibParser) parseLiteralStr(pos int) (int, []rune) {
	pos++
	start := pos
	pos = p.skipUntil(pos, "'", tomllibIllegalBasic, true)
	return pos + 1, p.src[start:pos]
}

// Parses a triple quoted string and absorbs up to two extra closing delimiters into the content
func (p *tomllibParser) parseMultilineStr(pos int, literal bool) int {
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
			panic(p.decodeError("Illegal character "+strconv.QuoteRune(char), pos))
		}
		pos++
	}
}

// Dispatches on the first character then tries datetime, time, number and special float forms in order
func (p *tomllibParser) parseValue(pos int) (int, *tomllibNode) {
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
	if end, ok := p.matchDatetime(pos); ok {
		if !p.matchToDatetime(pos) {
			panic(p.decodeError("Invalid date or datetime", pos))
		}
		return end, p.scalar
	}
	if end, ok := p.matchTime(pos); ok {
		return end, p.scalar
	}
	if end, ok := p.matchNumber(pos); ok {
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

// Matches the date pattern with optional time and offset, returning where the match ends
func (p *tomllibParser) matchDatetime(pos int) (end int, ok bool) {
	for i := 0; i < 4; i++ {
		if !p.inRange(pos+i, '0', '9') {
			return 0, false
		}
	}
	if p.at(pos+4) != '-' || p.at(pos+7) != '-' {
		return 0, false
	}
	month := (p.at(pos+5) == '0' && p.inRange(pos+6, '1', '9')) || (p.at(pos+5) == '1' && p.inRange(pos+6, '0', '2'))
	day := (p.at(pos+8) == '0' && p.inRange(pos+9, '1', '9')) || (p.inRange(pos+8, '1', '2') && p.inRange(pos+9, '0', '9')) || (p.at(pos+8) == '3' && p.inRange(pos+9, '0', '1'))
	if !month || !day {
		return 0, false
	}
	end = pos + 10
	sep := p.at(end)
	if sep != 'T' && sep != 't' && sep != ' ' {
		return end, true
	}
	tend, timeOK := p.matchTime(end + 1)
	if !timeOK {
		return end, true
	}
	switch p.at(tend) {
	case 'Z', 'z':
		return tend + 1, true
	case '+', '-':
		if p.matchHour(tend+1) && p.at(tend+3) == ':' && p.matchMinute(tend+4) {
			return tend + 6, true
		}
	}
	return tend, true
}

func tomllibDigitsAt(src []rune, pos, n int) int {
	v := 0
	for i := 0; i < n; i++ {
		v = v*10 + int(src[pos+i]-'0')
	}
	return v
}

// Reports whether the matched date exists: a year from 1 on and a day inside its month
func (p *tomllibParser) matchToDatetime(pos int) bool {
	src := p.src
	year := tomllibDigitsAt(src, pos, 4)
	month := tomllibDigitsAt(src, pos+5, 2)
	day := tomllibDigitsAt(src, pos+8, 2)
	if year < 1 {
		return false
	}
	days := [13]int{0, 31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}
	if year%4 == 0 && (year%100 != 0 || year%400 == 0) {
		days[2] = 29
	}
	return day <= days[month]
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

// Matches prefixed integers first, then signed decimals and floats, of any length
func (p *tomllibParser) matchNumber(pos int) (end int, ok bool) {
	if p.at(pos) == '0' {
		for _, alt := range []struct {
			prefix rune
			class  func(rune) bool
		}{{'x', tomllibIsHex}, {'b', tomllibIsBin}, {'o', tomllibIsOct}} {
			if p.at(pos+1) == alt.prefix && pos+2 < len(p.src) && alt.class(p.src[pos+2]) {
				return p.matchDigitRun(pos+3, alt.class), true
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
		return 0, false
	}
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
	return i, true
}

// Derived from libyaml 0.1.7 as bundled in ruamel.yaml.clib 0.2.15, MIT license, Kirill Simonov and Ingy dot Net

const (
	libyamlInputRawBufferSize = 16384
	libyamlInputBufferSize    = libyamlInputRawBufferSize * 3
	libyamlMaxFileSize        = math.MaxInt64
	libyamlInitialStackSize   = 16
	libyamlInitialStringSize  = 16
	libyamlMaxNumberLength    = 9
	libyamlSizeofInt          = 4
	libyamlSizeofMark         = 24
	libyamlSizeofSimpleKey    = 40
	libyamlSizeofState        = 4
	libyamlSizeofTagDirective = 16
)

// Bytes a libyaml stack may reach before growth fails with MemoryError, lowered by the scaled stack limit test
var libyamlStackByteLimit = math.MaxInt32 / 2

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

// YAML syntax verdict of content, bytes that are not UTF-8 refused before parsing, empty when every event converts cleanly
func ruamelCParseError(content string) string {
	if msg := yamlUTF8Error(content); msg != "" {
		return msg
	}
	return ruamelCParseStream(content, "<unicode string>")
}

// Drives the CParser event loop over the raw input bytes and renders the first failure
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

// Most undecodable bytes in a row a UTF-8 failure message spells out
const yamlUTF8BytesShown = 8

// Message for content that is not valid UTF-8: the line and column of the first undecodable run, its bytes and why the first fails
func yamlUTF8Error(content string) string {
	var bad *textutil.InvalidUTF8Error
	if !errors.As(textutil.ValidateUTF8(content), &bad) {
		return ""
	}
	start, end := bad.Offset, bad.Offset+1
	for end < len(content) {
		if r, size := utf8.DecodeRuneInString(content[end:]); r != utf8.RuneError || size != 1 {
			break
		}
		end++
	}
	head := content[:start]
	line := 1 + strings.Count(head, "\n")
	column := 1 + utf8.RuneCountInString(head[strings.LastIndexByte(head, '\n')+1:])
	_, _, reason := ruamelUTF8DecodeFailure([]byte(content[start:min(start+utf8.UTFMax, len(content))]))
	shown := make([]string, 0, min(end-start, yamlUTF8BytesShown))
	for i := start; i < end && len(shown) < yamlUTF8BytesShown; i++ {
		shown = append(shown, fmt.Sprintf("0x%02x", content[i]))
	}
	noun, more := "byte", ""
	if end-start > 1 {
		noun = "bytes"
	}
	if end-start > yamlUTF8BytesShown {
		more = " and " + strconv.Itoa(end-start-yamlUTF8BytesShown) + " more"
	}
	return fmt.Sprintf("invalid YAML: the file is not valid UTF-8 at line %d, column %d: %s %s%s (%s)", line, column, noun, strings.Join(shown, " "), more, reason)
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
		return fmt.Sprintf("invalid YAML: a tag or anchor is not valid UTF-8 at byte %d (0x%02x, %s)", start, b[start], reason)
	}
	return fmt.Sprintf("invalid YAML: a tag or anchor is not valid UTF-8 at bytes %d to %d (%s)", start, end-1, reason)
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

// Renders the exception the pyx layer builds from the libyaml failure through the ruamel string methods with the prefix
func ruamelParserError(p *yamlParser, name string) string {
	switch p.errorType {
	case yamlMemoryError:
		return "invalid YAML: the document nests too deep to parse"
	case yamlReaderError:
		return "invalid YAML: unacceptable character #x" + ruamelFormatHex04(p.problemValue) + ": " + p.problem + "\n  in \"" + name + "\", position " + strconv.Itoa(p.problemOffset)
	case yamlScannerError, yamlParserError:
		lines := make([]string, 0, 4)
		if p.context != "" {
			lines = append(lines, p.context)
			if p.contextMark.line != p.problemMark.line || p.contextMark.column != p.problemMark.column {
				lines = append(lines, ruamelMarkString(name, p.contextMark))
			}
		}
		lines = append(lines, p.problem, ruamelMarkString(name, p.problemMark))
		return "invalid YAML: " + strings.Join(lines, "\n")
	}
	return "invalid YAML: the parser stopped without naming the problem"
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
