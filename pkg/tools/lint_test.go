package tools

import (
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

func TestLintLooksLikeLinterUnusable(t *testing.T) {
	cases := []struct {
		cmd, out string
		want     bool
	}{
		{"npx", "This is not the tsc command you are looking for", true},
		{"npx", "npm ERR! could not determine executable to run", true},
		{"rustfmt", "Error: not a workspace", true},
		{"go", "go: cannot find main module, but found .git", true},
		{"go", "vet: ./a.go:3: unreachable code", false},
		{"node", "could not determine executable to run", false},
	}
	for _, c := range cases {
		if got := looksLikeLinterUnusable(c.cmd, c.out); got != c.want {
			t.Fatalf("%s %q gave %v", c.cmd, c.out, got)
		}
	}
}

func TestLintJSONInprocMessages(t *testing.T) {
	big := strings.Repeat("1", 4301)
	limit := "ValueError: Exceeds the limit (4300 digits) for integer string conversion: value has 4301 digits; use sys.set_int_max_str_digits() to increase the limit"
	cases := []struct {
		in, want string
	}{
		{`{"a": 1}`, ""},
		{`{"a": 1,`, "JSONDecodeError: Expecting property name enclosed in double quotes (line 1, column 9)"},
		{"[1 2]", "JSONDecodeError: Expecting ',' delimiter (line 1, column 4)"},
		{"\xef\xbb\xbf{}", "JSONDecodeError: Unexpected UTF-8 BOM (decode using utf-8-sig) (line 1, column 1)"},
		{"", "JSONDecodeError: Expecting value (line 1, column 1)"},
		{`{"a":1,}`, "JSONDecodeError: Illegal trailing comma before end of object (line 1, column 7)"},
		{"[" + big + "]", limit},
		{"[" + big + ", ]", limit},
		{"[1,]" + strings.Repeat("1", 5000), "JSONDecodeError: Illegal trailing comma before end of array (line 1, column 3)"},
		{`["x" ` + strings.Repeat("1", 5000) + "]", "JSONDecodeError: Expecting ',' delimiter (line 1, column 6)"},
		{"-" + big, limit},
		{strings.Repeat("1", 4300), ""},
		{big + ".5", ""},
		{"{\"a\":\n  tru}", "JSONDecodeError: Expecting value (line 2, column 3)"},
		{"[NaN, Infinity, -Infinity]", ""},
		{`"abc`, "JSONDecodeError: Unterminated string starting at (line 1, column 1)"},
		{`["\q"]`, "JSONDecodeError: Invalid \\escape (line 1, column 3)"},
		{`{"k" 1}`, "JSONDecodeError: Expecting ':' delimiter (line 1, column 6)"},
		{strings.Repeat("[", 52151) + strings.Repeat("]", 52151), ""},
		{"[1, 2" + strings.Repeat("[", 60000), "JSONDecodeError: Expecting ',' delimiter (line 1, column 6)"},
	}
	for _, c := range cases {
		ok, got := lintJSONInproc(c.in)
		if got != c.want || ok != (c.want == "") {
			t.Fatalf("%q gave %v %q want %q", trimForLog(c.in), ok, got, c.want)
		}
	}
	for _, deep := range []struct{ in, kind string }{
		{strings.Repeat("[", 60000), "array"},
		{strings.Repeat(`{"a":`, 60000) + "1" + strings.Repeat("}", 60000), "object"},
	} {
		ok, got := lintJSONInproc(deep.in)
		if ok || !strings.HasPrefix(got, "RecursionError: Stack overflow (used ") || !strings.HasSuffix(got, "kB) while decoding a JSON "+deep.kind+" from a unicode string") {
			t.Fatalf("%s gave %q", deep.kind, got)
		}
	}
}

func TestLintJSONNonstandardConstant(t *testing.T) {
	cases := map[string]string{
		`{"value": NaN}`:                 "NaN",
		`{"value": -Infinity, "x": NaN}`: "-Infinity",
		`[1, Infinity]`:                  "Infinity",
		`{"NaN": "Infinity", "n": 1}`:    "",
		`{"value": NaN`:                  "",
		"\xef\xbb\xbf[NaN]":              "NaN",
		`{"a": 1}`:                       "",
	}
	for in, want := range cases {
		if got := jsonNonstandardConstant(in); got != want {
			t.Fatalf("%q gave %q want %q", in, got, want)
		}
	}
	pre := `{"a": NaN}`
	if r := refuseIntroducedJSONConstant("f.json", `{"a": NaN, "b": 1}`, &pre); r != nil {
		t.Fatalf("legacy constant refused %+v", r)
	}
	r := refuseIntroducedJSONConstant("f.json", `{"a": Infinity}`, nil)
	want := "Refusing to write 'f.json': candidate content uses Infinity, which is not valid JSON. The file was NOT created or modified. Use null or a string instead and retry."
	if r == nil || r.Error != want {
		t.Fatalf("got %+v", r)
	}
}

// Scripted backend answering command probes with yes and every other command with the given reply
func lintScriptedOps(t *testing.T, haveCommand bool, rc int, output string) (*shellFileOps, *fakeTermEnv) {
	t.Helper()
	env := mockFileOpsEnv(func(command string, _ ExecOptions) *ExecResult {
		if strings.HasPrefix(command, "command -v ") {
			if haveCommand {
				return &ExecResult{Output: "yes\n"}
			}
			return &ExecResult{ReturnCode: 1}
		}
		return &ExecResult{Output: output, ReturnCode: rc}
	})
	return newShellFileOps(remoteTestEnv(t), env, "/tmp"), env
}

func TestLintCheckLintShellTable(t *testing.T) {
	ops, env := lintScriptedOps(t, true, 0, "")
	r := ops.checkLint("/tmp/{test}_file.js", nil)
	calls := envCommands(env)
	if !r.Success || r.Skipped || !strings.Contains(calls[len(calls)-1], "{test}") || !strings.HasPrefix(calls[len(calls)-1], "node --check ") {
		t.Fatalf("brace path %+v %q", r, calls)
	}
	if r := ops.checkLint("/tmp/file.unknown_ext", nil); !r.Skipped || r.Message != "No linter for .unknown_ext files" {
		t.Fatalf("unknown ext %+v", r)
	}
	missing, _ := lintScriptedOps(t, false, 0, "")
	if r := missing.checkLint("/tmp/test.js", nil); !r.Skipped || r.Message != "node not available" {
		t.Fatalf("missing linter %+v", r)
	}
	failing, _ := lintScriptedOps(t, true, 1, "SyntaxError: invalid syntax\n")
	if r := failing.checkLint("/tmp/bad.js", nil); r.Success || r.Output != "SyntaxError: invalid syntax" {
		t.Fatalf("failure %+v", r)
	}
	unusable, _ := lintScriptedOps(t, true, 1, "\x1b[31m\n\n  npm ERR! could not determine executable to run  \nmore\n")
	if r := unusable.checkLint("/tmp/a.ts", nil); !r.Skipped || r.Message != "npx not usable: npm ERR! could not determine executable to run" {
		t.Fatalf("unusable %+v", r)
	}
}

func TestLintShellLinterUsesNativeFormWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("native tool arguments differ on Windows only")
	}
	ops, env := lintScriptedOps(t, true, 0, "")
	r := ops.checkLint(`C:\Users\alice\app\main.js`, nil)
	var node string
	for _, c := range envCommands(env) {
		if strings.Contains(c, "node --check") {
			node = c
		}
	}
	if r.Skipped || !strings.Contains(node, "'C:/Users/alice/app/main.js'") || strings.Contains(node, "/c/Users") {
		t.Fatalf("%+v %q", r, node)
	}
}

func TestLintCheckLintInproc(t *testing.T) {
	ops, env := lintScriptedOps(t, true, 0, "")
	content := `{"a": 1}`
	if r := ops.checkLint("/tmp/a.json", &content); !r.Success || r.Skipped || r.Output != "" {
		t.Fatalf("json %+v", r)
	}
	bad := `{"a": 1,`
	if r := ops.checkLint("/tmp/A.JSON", &bad); r.Success || r.Output != "JSONDecodeError: Expecting property name enclosed in double quotes (line 1, column 9)" {
		t.Fatalf("json error %+v", r)
	}
	if n := len(envCommands(env)); n != 0 {
		t.Fatalf("in process lint ran %d commands", n)
	}
	unreadable, _ := lintScriptedOps(t, true, 1, "")
	if r := unreadable.checkLint("/tmp/gone.json", nil); !r.Skipped || r.Message != "Failed to read /tmp/gone.json for lint" {
		t.Fatalf("unreadable %+v", r)
	}
	if hostPythonInterpreter() == "" {
		t.Skip("no host python for the ast lint")
	}
	py := "x = 1\n"
	if r := ops.checkLint("/tmp/ok.py", &py); !r.Success || r.Skipped || r.Output != "" {
		t.Fatalf("python %+v", r)
	}
	badPy := "def foo(:\n    pass\n"
	if r := ops.checkLint("/tmp/bad.py", &badPy); r.Success || !strings.HasPrefix(r.Output, "SyntaxError: ") || !strings.Contains(r.Output, "(line 1, column ") {
		t.Fatalf("python error %+v", r)
	}
	nul := "x = 1\x00\n"
	if r := ops.checkLint("/tmp/nul.py", &nul); r.Success || r.Output == "" {
		t.Fatalf("python nul %+v", r)
	}
}

func TestLintCheckLintDelta(t *testing.T) {
	ops, _ := lintScriptedOps(t, true, 0, "")
	pre, post := `{"a": 1}`, `{"a": 2}`
	if r := ops.checkLintDelta("/tmp/d.json", &pre, &post); !r.Success {
		t.Fatalf("clean %+v", r)
	}
	brokenPost := `{"a": 2,`
	if r := ops.checkLintDelta("/tmp/d.json", &pre, &brokenPost); r.Success || r.Output != "JSONDecodeError: Expecting property name enclosed in double quotes (line 1, column 9)" {
		t.Fatalf("new error over clean pre %+v", r)
	}
	brokenPre := `{"a": 1,`
	r := ops.checkLintDelta("/tmp/d.json", &brokenPre, &brokenPost)
	if r.Success || !strings.Contains(strings.ToLower(r.Message), "pre-existing") || r.Output != "JSONDecodeError: Expecting property name enclosed in double quotes (line 1, column 9)" {
		t.Fatalf("pre-existing %+v", r)
	}
	otherPre := `[1 2]`
	r = ops.checkLintDelta("/tmp/d.json", &otherPre, &brokenPost)
	if r.Success || r.Output != "New lint errors introduced by this edit (pre-existing errors filtered out):\nJSONDecodeError: Expecting property name enclosed in double quotes (line 1, column 9)" {
		t.Fatalf("filtered %+v", r)
	}
	if r := ops.checkLintDelta("/tmp/d.json", nil, &brokenPost); r.Success || r.Message != "" {
		t.Fatalf("no pre %+v", r)
	}
	if hostPythonInterpreter() == "" {
		return
	}
	pyPre := "def a(:\n    pass\n"
	pyPost := "def a(:\n    pass\n\nprint(42)\n"
	if r := ops.checkLintDelta("/tmp/d.py", &pyPre, &pyPost); r.Success || !strings.Contains(strings.ToLower(r.Message), "pre-existing") {
		t.Fatalf("python pre-existing %+v", r)
	}
}

func TestLintFailClosedSyntaxGateRouting(t *testing.T) {
	r := failClosedSyntaxError("/tmp/c.json", ".json", `{"a": 1,`)
	want := "Refusing to write '/tmp/c.json': candidate content fails .json syntax validation " +
		"(JSONDecodeError: Expecting property name enclosed in double quotes (line 1, column 9)). The file was NOT created or modified. Fix the content and retry."
	if r == nil || r.Error != want {
		t.Fatalf("json %+v", r)
	}
	if r := failClosedSyntaxError("/tmp/c.json", ".json", `{"a": 1}`); r != nil {
		t.Fatalf("valid json refused %+v", r)
	}
	if r := failClosedSyntaxError("/tmp/c.py", ".py", "def foo(:\n"); r != nil {
		t.Fatalf("python is not fail closed %+v", r)
	}
	for _, ext := range []string{".json", ".yaml", ".yml", ".toml"} {
		if !failClosedInprocExts[ext] || lintersInproc[ext] == nil {
			t.Fatalf("%s not routed", ext)
		}
	}
	if failClosedInprocExts[".py"] || lintersInproc[".py"] == nil {
		t.Fatal("python routing")
	}
}

func TestLintTsconfigAncestorSkipsPerFileTsc(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src", "deep"), 0o755)
	writeTestFile(t, dir, "tsconfig.json", []byte("{}"))
	ops := newShellFileOps(remoteTestEnv(t), bashFileOpsEnv(dir, true), dir)
	r := ops.checkLint(filepath.Join(dir, "src", "deep", "a.ts"), nil)
	want := "Project tsconfig.json detected \xe2\x80\x94 per-file tsc skipped (single-file tsc can't resolve project aliases/globals; use the LSP tier or `tsc -p tsconfig.json` for real diagnostics)."
	if !r.Skipped || r.Message != want {
		t.Fatalf("%+v", r)
	}
	remote := newShellFileOps(remoteTestEnv(t), mockFileOpsEnv(nil), dir)
	if remote.hasAncestorTsconfig(filepath.Join(dir, "src", "deep", "a.ts")) {
		t.Fatal("remote backend answered from the host")
	}
}

func TestLintLocalJSWithoutManagedNodeSkips(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix stand ins")
	}
	dir := t.TempDir()
	userBin := filepath.Join(dir, "user-bin")
	os.Mkdir(userBin, 0o755)
	writeTestFile(t, userBin, "node", []byte("#!/bin/sh\necho user-node \"$@\"\nexit 1\n"))
	os.Chmod(filepath.Join(userBin, "node"), 0o755)
	t.Setenv("PATH", userBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	target := writeTestFile(t, dir, "a.js", []byte("x\n"))
	ops := newShellFileOps(remoteTestEnv(t), bashFileOpsEnv(dir, true), dir)
	if lintWhich("node", withIcebathNodePath(map[string]string{"PATH": ""})["PATH"]) != "" {
		t.Skip("a managed Node is installed")
	}
	r := ops.checkLint(target, nil)
	if !r.Skipped || r.Message != "node not available (Icebath-managed Node not installed)" {
		t.Fatalf("%+v", r)
	}
}

func TestLintWhichAndManagedLinterRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix stand ins")
	}
	dir := t.TempDir()
	store := filepath.Join(dir, "store-node")
	os.Mkdir(store, 0o755)
	writeTestFile(t, store, "node", []byte("#!/bin/sh\necho pm-node \"$@\"\nexit 1\n"))
	os.Chmod(filepath.Join(store, "node"), 0o755)
	writeTestFile(t, store, "plain", []byte("x"))
	if got := lintWhich("node", "/nonexistent"+string(os.PathListSeparator)+store); got != filepath.Join(store, "node") {
		t.Fatalf("which %q", got)
	}
	if got := lintWhich("plain", store); got != "" {
		t.Fatalf("non executable found %q", got)
	}
}

func TestLintLocalJSRunsManagedNodeNeverTheUsers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix stand ins")
	}
	dir := t.TempDir()
	standIn := func(name, label string) string {
		d := filepath.Join(dir, name)
		os.Mkdir(d, 0o755)
		writeTestFile(t, d, "node", []byte("#!/bin/sh\necho "+label+" \"$@\"\nexit 1\n"))
		os.Chmod(filepath.Join(d, "node"), 0o755)
		return d
	}
	installTestEnvHooks(t)
	userBin := standIn("user-bin", "user-node")
	storeDirs := []string{standIn("store-node", "pm-node")}
	t.Setenv("PATH", userBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	prev := managedNodePathEnv
	managedNodePathEnv = func(env map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range env {
			out[k] = v
		}
		out["PATH"] = strings.Trim(strings.Join(append(append([]string{}, storeDirs...), env["PATH"]), string(os.PathListSeparator)), string(os.PathListSeparator))
		return out
	}
	t.Cleanup(func() { managedNodePathEnv = prev })
	target := writeTestFile(t, dir, "a.js", []byte("x\n"))
	ops := newShellFileOps(remoteTestEnv(t), bashFileOpsEnv(dir, true), dir)
	if r := ops.checkLint(target, nil); r.Output != "pm-node --check "+target {
		t.Fatalf("%+v", r)
	}
	storeDirs = nil
	if r := ops.checkLint(target, nil); !r.Skipped || !strings.Contains(r.Message, "Icebath-managed Node") {
		t.Fatalf("%+v", r)
	}
}

func TestLintLSPAbsentNeverBlocksTheWrite(t *testing.T) {
	ops := newShellFileOps(remoteTestEnv(t), mockFileOpsEnv(nil), "/tmp")
	if ops.lspService() != nil || ops.lspWillHandle("/tmp/a.ts") || ops.lspHandlesExtension("") {
		t.Fatal("remote backend consulted the LSP")
	}
	content := "x"
	if got := formatLSPBlock("/tmp/a.ts", nil, "h"); got != "" {
		t.Fatalf("empty diagnostics formatted %q", got)
	}
	if got := ops.maybeLSPDiagnostics("/tmp/a.ts", nil, &content, time.Now()); got != "" {
		t.Fatalf("got %q", got)
	}
}

// Inputs with json.loads outcomes captured from Python 3.14 inside a worker thread
var lintJSONGolden = []struct {
	in       string
	ok       bool
	err      string
	constant string
}{
	{"{}", true, "", ""},
	{"[]", true, "", ""},
	{"", false, "JSONDecodeError: Expecting value (line 1, column 1)", ""},
	{" ", false, "JSONDecodeError: Expecting value (line 1, column 2)", ""},
	{"{\x22a\x22:1,}", false, "JSONDecodeError: Illegal trailing comma before end of object (line 1, column 7)", ""},
	{"[1,]", false, "JSONDecodeError: Illegal trailing comma before end of array (line 1, column 3)", ""},
	{"[1 2]", false, "JSONDecodeError: Expecting ',' delimiter (line 1, column 4)", ""},
	{"{\x22a\x22 1}", false, "JSONDecodeError: Expecting ':' delimiter (line 1, column 6)", ""},
	{"{1:2}", false, "JSONDecodeError: Expecting property name enclosed in double quotes (line 1, column 2)", ""},
	{"\xef\xbb\xbf{}", false, "JSONDecodeError: Unexpected UTF-8 BOM (decode using utf-8-sig) (line 1, column 1)", ""},
	{"\x22abc", false, "JSONDecodeError: Unterminated string starting at (line 1, column 1)", ""},
	{"\x22a\x5cx\x22", false, "JSONDecodeError: Invalid \x5cescape (line 1, column 3)", ""},
	{"\x22\x5cud800\x22", true, "", ""},
	{"[NaN]", true, "", "NaN"},
	{"{\x22a\x22: Infinity, \x22b\x22: -Infinity}", true, "", "Infinity"},
	{"[-Infinity, NaN]", true, "", "-Infinity"},
	{"{\x22NaN\x22: 1}", true, "", ""},
	{"\x22Infinity\x22", true, "", ""},
	{"{\x22a\x22:NaN,\x22a\x22:1}", true, "", "NaN"},
	{"[1e5, 1.5, -0, 0.0]", true, "", ""},
	{"01", false, "JSONDecodeError: Extra data (line 1, column 2)", ""},
	{"[01]", false, "JSONDecodeError: Expecting ',' delimiter (line 1, column 3)", ""},
	{"-", false, "JSONDecodeError: Expecting value (line 1, column 1)", ""},
	{"-a", false, "JSONDecodeError: Expecting value (line 1, column 1)", ""},
	{"1.", false, "JSONDecodeError: Extra data (line 1, column 2)", ""},
	{"1e", false, "JSONDecodeError: Extra data (line 1, column 2)", ""},
	{"1e+", false, "JSONDecodeError: Extra data (line 1, column 2)", ""},
	{"tru", false, "JSONDecodeError: Expecting value (line 1, column 1)", ""},
	{"nul", false, "JSONDecodeError: Expecting value (line 1, column 1)", ""},
	{"{\x22a\x22:\x0a  [1,\x0a   2,,]}", false, "JSONDecodeError: Expecting value (line 3, column 6)", ""},
	{"\x09[\x0d\x0a]", true, "", ""},
	{"[1]x", false, "JSONDecodeError: Extra data (line 1, column 4)", ""},
	{"{\x22a\x22:\x22\x01\x22}", false, "JSONDecodeError: Invalid control character at (line 1, column 7)", ""},
	{"{\x22a\x22:[1,2,{\x22b\x22:null}],\x22c\x22:true}", true, "", ""},
	{"\x22\xc3\xa9\xe4\xb8\xad\x22", true, "", ""},
	{"[\x22\xf0\x9f\x98\x80\x22]", true, "", ""},
	{"{\x22a\x22:1}{\x22b\x22:2}", false, "JSONDecodeError: Extra data (line 1, column 8)", ""},
	{"NaN", true, "", "NaN"},
	{"-NaN", false, "JSONDecodeError: Expecting value (line 1, column 1)", ""},
	{"Infinityx", false, "JSONDecodeError: Extra data (line 1, column 9)", ""},
	{"[1, 2, NaN", false, "JSONDecodeError: Expecting ',' delimiter (line 1, column 11)", ""},
	{"{\x22\x5c\x22\x22: NaN}", true, "", "NaN"},
	{" \x0a\x0a  {\x22x\x22 : [ -Infinity ] } ", true, "", "-Infinity"},
}

func TestLintJSONGoldenParity(t *testing.T) {
	for i, c := range lintJSONGolden {
		ok, err := lintJSONInproc(c.in)
		if ok != c.ok || err != c.err {
			t.Errorf("%d %q got %v %q want %v %q", i, c.in, ok, err, c.ok, c.err)
		}
		if k := jsonNonstandardConstant(c.in); k != c.constant {
			t.Errorf("%d %q constant got %q want %q", i, c.in, k, c.constant)
		}
	}
}

func TestLintJSONDepthAndIntLimitParity(t *testing.T) {
	r := strings.Repeat
	big := r("1", 4301)
	tooBig := "ValueError: Exceeds the limit (4300 digits) for integer string conversion: value has 4301 digits; use sys.set_int_max_str_digits() to increase the limit"
	arrayRE := "RecursionError: Stack overflow (used 8156 kB) while decoding a JSON array from a unicode string"
	objectRE := "RecursionError: Stack overflow (used 8156 kB) while decoding a JSON object from a unicode string"
	cases := []struct{ in, want string }{
		{"[" + big + "]", tooBig},
		{"[" + r("1", 4300) + "]", ""},
		{"[" + big + ",]", tooBig},
		{"[," + big + "]", "JSONDecodeError: Expecting value (line 1, column 2)"},
		{"-" + big, tooBig},
		{big + ".5", ""},
		{big + "e3", ""},
		{r("[", 52154) + r("]", 52154), ""},
		{r("[", 52155) + r("]", 52155), arrayRE},
		{r(`{"a":`, 52155) + "1" + r("}", 52155), objectRE},
		{"[1 " + r("[", 52155), "JSONDecodeError: Expecting ',' delimiter (line 1, column 4)"},
		{r("[", 52154) + "{", objectRE},
		{r("[", 52155), arrayRE},
		{r("[", 52155) + "]", arrayRE},
		{r("[", 52153) + `{"k":[]]}` + r("]", 52153), arrayRE},
		{r("[", 52154) + "{}", objectRE},
	}
	for i, c := range cases {
		ok, err := lintJSONInproc(c.in)
		if ok != (c.want == "") || err != c.want {
			t.Errorf("%d got %v %q want %q", i, ok, err, c.want)
		}
	}
}

type tomllibGoldenCase struct {
	name    string
	in      string
	want    string
	resetTz bool
}

// Holds inputs with the verdicts CPython 3.14 tomllib produced when called from a script top level linter
var tomllibGoldenCases = []tomllibGoldenCase{
	{"empty document", "", "", true},
	{"blank lines", "\n\n\n", "", false},
	{"comment only", "# only a comment", "", false},
	{"indented comment", "   \t  # indented comment\n", "", false},
	{"toml example", "# This is a TOML document\n\ntitle = \"TOML Example\"\n\n[owner]\nname = \"Tom Preston-Werner\"\ndob = 1979-05-27T07:32:00-08:00\n\n[database]\nenabled = true\nports = [ 8000, 8001, 8002 ]\ndata = [ [\"delta\", \"phi\"], [3.14] ]\ntemp_targets = { cpu = 79.5, case = 72.0 }\n\n[servers]\n\n[servers.alpha]\nip = \"10.0.0.1\"\nrole = \"frontend\"\n\n[servers.beta]\nip = \"10.0.0.2\"\nrole = \"backend\"\n", "", false},
	{"fruits array of tables", "[[fruits]]\nname = \"apple\"\n\n[fruits.physical]  # subtable\ncolor = \"red\"\nshape = \"round\"\n\n[[fruits.varieties]]  # nested array of tables\nname = \"red delicious\"\n\n[[fruits.varieties]]\nname = \"granny smith\"\n\n\n[[fruits]]\nname = \"banana\"\n\n[[fruits.varieties]]\nname = \"plantain\"\n", "", false},
	{"basic string escapes", "a = \"\\b\\t\\n\\f\\r\\\"\\\\\\u00e9\\U0001F600\"", "", false},
	{"basic string with tab", "a = \"x\ty\"", "", false},
	{"literal string", "a = 'C:\\Users\\nodejs\\templates'", "", false},
	{"multiline basic string", "a = \"\"\"\nRoses are red\nViolets are blue\"\"\"", "", false},
	{"multiline basic line ending backslash", "a = \"\"\"\\\n    The quick brown \\\n\n\n    fox jumps over \\\n    the lazy dog.\"\"\"", "", false},
	{"multiline basic backslash space newline", "a = \"\"\"x\\   \n   y\"\"\"", "", false},
	{"multiline basic four quotes", "a = \"\"\"x\"\"\"\"", "", false},
	{"multiline basic five quotes", "a = \"\"\"x\"\"\"\"\"", "", false},
	{"multiline literal string", "a = '''\nThe first newline is\ntrimmed in raw strings.\n   All other whitespace\n   is preserved.\n'''", "", false},
	{"multiline literal four quotes", "a = '''x''''", "", false},
	{"multiline literal five quotes", "a = '''x'''''", "", false},
	{"multiline literal eight quotes", "a = ''''''''", "", false},
	{"integers", "a = +99\nb = 42\nc = 0\nd = -17\ne = 1_000\nf = 5_349_221\ng = 53_49_221\nh = 1_2_3_4_5\ni = -0\nj = +0", "", false},
	{"prefixed integers", "a = 0xDEADBEEF\nb = 0xdead_beef\nc = 0o01234567\nd = 0o755\ne = 0b11010110\nf = 0x0", "", false},
	{"floats", "a = +1.0\nb = 3.1415\nc = -0.01\nd = 5e+22\ne = 1e06\nf = -2E-2\ng = 6.626e-34\nh = 224_617.445_991_228\ni = -0.0\nj = +0.0\nk = 1e1_0", "", false},
	{"special floats", "a = inf\nb = +inf\nc = -inf\nd = nan\ne = +nan\nf = -nan", "", false},
	{"booleans", "a = true\nb = false", "", false},
	{"offset datetimes", "a = 1979-05-27T07:32:00Z\nb = 1979-05-27T00:32:00-07:00\nc = 1979-05-27T00:32:00.999999-07:00\nd = 1979-05-27 07:32:00Z\ne = 1979-05-27t07:32:00z\nf = 1979-05-27T00:32:00.1234567890+23:59", "", false},
	{"local datetimes", "a = 1979-05-27T07:32:00\nb = 1979-05-27T00:32:00.999999\nc = 1979-05-27 00:32:00.5", "", false},
	{"local date", "a = 1979-05-27\nb = 2000-02-29\nc = 2024-02-29\nd = 9999-12-31\ne = 0001-01-01", "", false},
	{"local time", "a = 07:32:00\nb = 00:32:00.999999\nc = 23:59:59.9999999", "", false},
	{"arrays", "a = [ 1, 2, 3 ]\nb = [ \"red\", 'yellow', \"\"\"green\"\"\" ]\nc = [ [ 1, 2 ], [3, 4, 5] ]\nd = [ \"all\", 'strings', \"\"\"are the same\"\"\", '''type''' ]\ne = [ [ 1, 2 ], [\"a\", \"b\", \"c\"] ]\nf = [ 0.1, 0.2, 0.5, 1, 2, 5 ]\ng = []\nh = [1,]", "", false},
	{"multiline array with comments", "a = [ # start\n  1, # one\n\n  2 # two\n  , # comma\n  # alone\n]", "", false},
	{"inline tables", "name = { first = \"Tom\", last = \"Preston-Werner\" }\npoint = { x = 1, y = 2 }\nanimal = { type.name = \"pug\" }\ne = {}\nn = { a = { b = { c = [1, {d = 2}] } } }", "", false},
	{"tables and dotted keys", "[table-1]\nkey1 = \"some string\"\nkey2 = 123\n\n[table-2]\nkey1 = \"another string\"\nkey2 = 456\n\n[dog.\"tater.man\"]\ntype.name = \"pug\"\n\n[ j . \"k\" . 'l' ]\nm = 1", "", false},
	{"implicit super table", "[x.y.z.w]\na = 1\n[x]\nb = 2", "", false},
	{"dotted keys", "name = \"Orange\"\nphysical.color = \"orange\"\nphysical.shape = \"round\"\nsite.\"google.com\" = true\n3.14159 = \"pi\"\nfruit . flavor = \"banana\"", "", false},
	{"quoted keys", "\"127.0.0.1\" = \"value\"\n\"character encoding\" = \"value\"\n\"\xca\x83\xc9\x94\xca\x87\" = \"value\"\n'key2' = \"value\"\n'quoted \"value\"' = \"value\"\n\"\" = \"blank\"", "", false},
	{"bare key with digits and dashes", "bare_key = 1\nbare-key = 2\n1234 = 3\n-_- = 4", "", false},
	{"table after dotted subtable", "[fruit]\napple.color = \"red\"\napple.taste.sweet = true\n\n[fruit.apple.texture]\nsmooth = true", "", false},
	{"array of tables with subtable reopen", "[[a]]\n[a.b]\nx = 1\n[[a]]\n[a.b]\nx = 2", "", false},
	{"crlf document", "a = 1\r\n[t]\r\nb = \"x\"\r\nc = '''\r\nline\r\n'''\r\n", "", false},
	{"unicode content", "name = \"J\xc3\xbcrgen \xf0\x9f\x98\x80\"\n# coment\xc3\xa1rio \xe2\x80\x94 ok\n'cl\xc3\xa9' = '\xe6\x97\xa5\xe6\x9c\xac'", "", false},
	{"invalid utf8 in strings and comments", "a = \"\xff\xfe\"\nb = '\xc3'\n# \xed\xa0\x80 comment\nc = \"\"\"\x80\n\"\"\"", "", false},
	{"invalid utf8 quoted key", "'\xff' = 1\n\"\xfe\" = 2", "", false},
	{"4300 digit integer", "a = 9999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999", "", false},
	{"4300 digits with underscores", "a = -1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1_1", "", false},
	{"long hex integer", "a = 0xffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", "", false},
	{"long float", "a = 11111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111.5", "", false},
	{"1000 part key", "a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a = 1", "", false},
	{"1000 part table header", "[a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a]", "", false},
	{"trailing whitespace and comment", "a = 1   # comment   \n[t]   # c\n[[u]]\t# c", "", false},
	{"tab whitespace", "\ta\t=\t1\t\n[\tt\t]", "", false},
	{"comment with tab and unicode", "# \tcomment \xc3\xa9\na = 1 #\t\xc3\xa9", "", false},
	{"whitespace escape at end of multiline", "a = \"\"\"x\\\n\"\"\"", "", false},
	{"invalid statement equals", "= 1", "TOMLDecodeError: Invalid statement (at line 1, column 1)", false},
	{"invalid statement nul", "\x00", "TOMLDecodeError: Invalid statement (at line 1, column 1)", false},
	{"invalid statement bom", "\xef\xbb\xbfa = 1", "TOMLDecodeError: Invalid statement (at line 1, column 1)", false},
	{"invalid statement invalid utf8", "\xff = 1", "TOMLDecodeError: Invalid statement (at line 1, column 1)", false},
	{"invalid statement second line", "a = 1\n  ?", "TOMLDecodeError: Invalid statement (at line 2, column 3)", false},
	{"invalid statement bare cr", "\r", "TOMLDecodeError: Invalid statement (at line 1, column 1)", false},
	{"expected newline after value", "a = 1 b = 2", "TOMLDecodeError: Expected newline or end of document after a statement (at line 1, column 7)", false},
	{"expected newline after table", "[a] b = 1", "TOMLDecodeError: Expected newline or end of document after a statement (at line 1, column 5)", false},
	{"expected newline after array table", "[[a]] x", "TOMLDecodeError: Expected newline or end of document after a statement (at line 1, column 7)", false},
	{"expected newline bare cr after value", "a = 1\r", "TOMLDecodeError: Expected newline or end of document after a statement (at line 1, column 6)", false},
	{"expected newline after crlf lines", "a = 1\r\nb = 2\r\nc = 3 4\r\n", "TOMLDecodeError: Expected newline or end of document after a statement (at line 3, column 7)", false},
	{"expected newline after datetime", "a = 1979-05-27T07:32:00Zfoo", "TOMLDecodeError: Expected newline or end of document after a statement (at line 1, column 25)", false},
	{"expected newline after number prefix", "a = 0x", "TOMLDecodeError: Expected newline or end of document after a statement (at line 1, column 6)", false},
	{"expected newline after leading zero", "a = 0123", "TOMLDecodeError: Expected newline or end of document after a statement (at line 1, column 6)", false},
	{"expected newline after float dot", "a = 1.", "TOMLDecodeError: Expected newline or end of document after a statement (at line 1, column 6)", false},
	{"expected newline after exponent", "a = 1e", "TOMLDecodeError: Expected newline or end of document after a statement (at line 1, column 6)", false},
	{"expected newline after partial time", "a = 1979-05-27T07:32", "TOMLDecodeError: Expected newline or end of document after a statement (at line 1, column 15)", false},
	{"expected newline after bad offset", "a = 1979-05-27T07:32:00+25:00", "TOMLDecodeError: Expected newline or end of document after a statement (at line 1, column 24)", false},
	{"expected newline after local time offset", "a = 07:32:00Z", "TOMLDecodeError: Expected newline or end of document after a statement (at line 1, column 13)", false},
	{"expected newline after special float", "a = infinity", "TOMLDecodeError: Expected newline or end of document after a statement (at line 1, column 8)", false},
	{"expected newline after true", "a = truex", "TOMLDecodeError: Expected newline or end of document after a statement (at line 1, column 9)", false},
	{"expected newline after six quotes literal", "a = '''x''''''", "TOMLDecodeError: Expected newline or end of document after a statement (at line 1, column 14)", false},
	{"expected newline after six quotes basic", "a = \"\"\"x\"\"\"\"\"\"", "TOMLDecodeError: Expected newline or end of document after a statement (at line 1, column 14)", false},
	{"expected apostrophe", "a = 'abc", "TOMLDecodeError: Expected \"'\" (at end of document)", false},
	{"expected apostrophe key", "'abc = 1", "TOMLDecodeError: Expected \"'\" (at end of document)", false},
	{"expected triple apostrophe", "a = '''abc\ndef", "TOMLDecodeError: Expected \"'''\" (at end of document)", false},
	{"expected apostrophe before illegal char", "a = 'abc\x00", "TOMLDecodeError: Expected \"'\" (at end of document)", false},
	{"comment nul", "# \x00", "TOMLDecodeError: Found invalid character '\\x00' (at line 1, column 3)", false},
	{"comment control char", "a = 1 # x\x01y", "TOMLDecodeError: Found invalid character '\\x01' (at line 1, column 10)", false},
	{"comment del", "# \x7f", "TOMLDecodeError: Found invalid character '\\x7f' (at line 1, column 3)", false},
	{"comment bare cr", "# x\ry", "TOMLDecodeError: Found invalid character '\\r' (at line 1, column 4)", false},
	{"comment vertical tab", "a = [1, # \x0b\n]", "TOMLDecodeError: Found invalid character '\\x0b' (at line 1, column 11)", false},
	{"literal string newline", "a = 'x\ny'", "TOMLDecodeError: Found invalid character '\\n' (at line 1, column 7)", false},
	{"literal string control", "a = 'x\x1fy'", "TOMLDecodeError: Found invalid character '\\x1f' (at line 1, column 7)", false},
	{"literal string bare cr", "a = 'x\ry'", "TOMLDecodeError: Found invalid character '\\r' (at line 1, column 7)", false},
	{"multiline literal control", "a = '''x\x00y'''", "TOMLDecodeError: Found invalid character '\\x00' (at line 1, column 9)", false},
	{"multiline literal bare cr", "a = '''x\ry'''", "TOMLDecodeError: Found invalid character '\\r' (at line 1, column 9)", false},
	{"literal key control", "'x\x08y' = 1", "TOMLDecodeError: Found invalid character '\\x08' (at line 1, column 3)", false},
	{"table declared twice", "[a]\n[a]", "TOMLDecodeError: Cannot declare ('a',) twice (at line 2, column 3)", false},
	{"dotted table declared twice", "[a.b]\nx = 1\n[a.b]", "TOMLDecodeError: Cannot declare ('a', 'b') twice (at line 3, column 5)", false},
	{"table over inline table", "a = {}\n[a]", "TOMLDecodeError: Cannot declare ('a',) twice (at line 2, column 3)", false},
	{"table over array value", "a = [1]\n[a]", "TOMLDecodeError: Cannot declare ('a',) twice (at line 2, column 3)", false},
	{"table over dotted key table", "a.b = 1\n[a]", "TOMLDecodeError: Cannot declare ('a',) twice (at line 2, column 3)", false},
	{"table over nested dotted key table", "[t]\na.b = 1\n[t.a]", "TOMLDecodeError: Cannot declare ('t', 'a') twice (at line 3, column 5)", false},
	{"table over array of tables", "[[a]]\n[a]", "TOMLDecodeError: Cannot declare ('a',) twice (at line 2, column 3)", false},
	{"table over value", "a = 1\n[a]", "TOMLDecodeError: Cannot overwrite a value (at line 2, column 3)", false},
	{"table under value", "a = 1\n[a.b]", "TOMLDecodeError: Cannot overwrite a value (at line 2, column 5)", false},
	{"table missing bracket", "[a", "TOMLDecodeError: Expected ']' at the end of a table declaration (at end of document)", false},
	{"table missing bracket space", "[a b]", "TOMLDecodeError: Expected ']' at the end of a table declaration (at line 1, column 4)", false},
	{"table extra bracket", "[a]]", "TOMLDecodeError: Expected newline or end of document after a statement (at line 1, column 4)", false},
	{"table bad key", "[]", "TOMLDecodeError: Invalid initial character for a key part (at line 1, column 2)", false},
	{"table dot first", "[.a]", "TOMLDecodeError: Invalid initial character for a key part (at line 1, column 2)", false},
	{"table repr with quotes", "[\"it's\"]\n[\"it's\"]", "TOMLDecodeError: Cannot declare (\"it's\",) twice (at line 2, column 8)", false},
	{"table repr with both quotes", "[\"'\\\"\"]\n[\"'\\\"\"]\n[\"a\\\"b\"]\n[\"a\\\"b\"]", "TOMLDecodeError: Cannot declare ('\\'\"',) twice (at line 2, column 7)", false},
	{"table repr with backslash", "['a\\b']\n['a\\b']", "TOMLDecodeError: Cannot declare ('a\\\\b',) twice (at line 2, column 7)", false},
	{"table repr nonprintable", "[\"\\u0000\\u00ad\\u2028\\uE000\\U000E0001\\u0378\"]\n[\"\\u0000\\u00ad\\u2028\\uE000\\U000E0001\\u0378\"]", "TOMLDecodeError: Cannot declare ('\\x00\\xad\\u2028\\ue000\\U000e0001\\u0378',) twice (at line 2, column 44)", false},
	{"table repr unicode 17 point", "[\"\\u088F\\U0001FAEA\"]\n[\"\\u088F\\U0001FAEA\"]", "TOMLDecodeError: Cannot declare ('\\u088f\\U0001faea',) twice (at line 2, column 20)", false},
	{"table repr printable unicode", "['\xc3\xa9\xf0\x9f\x98\x80\xe6\x97\xa5']\n['\xc3\xa9\xf0\x9f\x98\x80\xe6\x97\xa5']", "TOMLDecodeError: Cannot declare ('\xc3\xa9\xf0\x9f\x98\x80\xe6\x97\xa5',) twice (at line 2, column 7)", false},
	{"table repr control escapes", "[\"\\t\\n\\r\\b\\f\\u007f\"]\n[\"\\t\\n\\r\\b\\f\\u007f\"]", "TOMLDecodeError: Cannot declare ('\\t\\n\\r\\x08\\x0c\\x7f',) twice (at line 2, column 20)", false},
	{"table repr surrogate", pystr.SurrogateEscapeDecode([]byte("['\xed\xa0\x80']\n['\xed\xa0\x80']")), "TOMLDecodeError: Cannot declare ('\\udced\\udca0\\udc80',) twice (at line 2, column 7)", false},
	{"table repr lone surrogate", "['\xed\xa0\x80']\n['\xed\xa0\x80']", "TOMLDecodeError: Cannot declare ('\\ud800',) twice (at line 2, column 5)", false},
	{"lone surrogate value", "a = '\xed\xa0\x80'", "", false},
	{"lone surrogate bare key", "k\xed\xa0\x80 = 1", "TOMLDecodeError: Expected '=' after a key in a key/value pair (at line 1, column 2)", false},
	{"table repr multi part", "[a.\"b c\".'d']\n[a.\"b c\".'d']", "TOMLDecodeError: Cannot declare ('a', 'b c', 'd') twice (at line 2, column 13)", false},
	{"table equal keys different quoting", "[a.\"b\"]\n['a'.b]", "TOMLDecodeError: Cannot declare ('a', 'b') twice (at line 2, column 7)", false},
	{"array of tables over array value", "a = []\n[[a]]", "TOMLDecodeError: Cannot mutate immutable namespace ('a',) (at line 2, column 4)", false},
	{"array of tables over inline table", "a = {}\n[[a]]", "TOMLDecodeError: Cannot mutate immutable namespace ('a',) (at line 2, column 4)", false},
	{"array of tables inside inline table", "a = {b = []}\n[[a.b]]", "TOMLDecodeError: Cannot mutate immutable namespace ('a', 'b') (at line 2, column 6)", false},
	{"array of tables over table", "[a]\n[[a]]", "TOMLDecodeError: Cannot overwrite a value (at line 2, column 4)", false},
	{"array of tables over value", "a = 1\n[[a]]", "TOMLDecodeError: Cannot overwrite a value (at line 2, column 4)", false},
	{"array of tables under value", "a = 1\n[[a.b]]", "TOMLDecodeError: Cannot overwrite a value (at line 2, column 6)", false},
	{"array of tables missing brackets", "[[a]", "TOMLDecodeError: Expected ']]' at the end of an array declaration (at line 1, column 4)", false},
	{"array of tables space in brackets", "[[a] ]", "TOMLDecodeError: Expected ']]' at the end of an array declaration (at line 1, column 4)", false},
	{"array of tables missing all", "[[a", "TOMLDecodeError: Expected ']]' at the end of an array declaration (at end of document)", false},
	{"redefine namespace", "[a.b]\nx = 1\n[a]\nb.y = 2", "TOMLDecodeError: Cannot redefine namespace ('a', 'b') (at end of document)", false},
	{"redefine namespace nested", "[a.b.c]\n[a]\nb.c.d = 1", "TOMLDecodeError: Cannot redefine namespace ('a', 'b', 'c') (at end of document)", false},
	{"mutate inline table dotted", "a = {}\na.b = 1", "TOMLDecodeError: Cannot mutate immutable namespace ('a',) (at end of document)", false},
	{"mutate inline table nested", "a = {b = {}}\na.b.c = 1", "TOMLDecodeError: Cannot mutate immutable namespace ('a', 'b') (at end of document)", false},
	{"mutate array dotted", "a = []\na.b = 1", "TOMLDecodeError: Cannot mutate immutable namespace ('a',) (at end of document)", false},
	{"mutate inline table under header", "[t]\na = {x = 1}\na.y = 2", "TOMLDecodeError: Cannot mutate immutable namespace ('t', 'a') (at end of document)", false},
	{"overwrite value via dotted", "a = 1\na.b = 2", "TOMLDecodeError: Cannot overwrite a value (at end of document)", false},
	{"overwrite value", "a = 1\na = 2", "TOMLDecodeError: Cannot overwrite a value (at end of document)", false},
	{"overwrite value quoted", "a = 1\n\"a\" = 2", "TOMLDecodeError: Cannot overwrite a value (at end of document)", false},
	{"overwrite value escaped", "a = 1\n\"\\u0061\" = 2", "TOMLDecodeError: Cannot overwrite a value (at end of document)", false},
	{"overwrite table with value", "[a.b]\n[a]\nb = 1", "TOMLDecodeError: Cannot overwrite a value (at end of document)", false},
	{"overwrite empty keys", "\"\" = 1\n'' = 2", "TOMLDecodeError: Cannot overwrite a value (at end of document)", false},
	{"overwrite in array of tables element", "[[a]]\nb = 1\nb = 2", "TOMLDecodeError: Cannot overwrite a value (at end of document)", false},
	{"table over dotted keys under header", "[t]\na.b = 1\n[t.a.c]\n[t]\na.b.d = 2", "TOMLDecodeError: Cannot declare ('t',) twice (at line 4, column 3)", false},
	{"redefine namespace under header", "[t.a.b]\n[t]\na.b.c = 1", "TOMLDecodeError: Cannot redefine namespace ('t', 'a', 'b') (at end of document)", false},
	{"expected equals end of document", "a", "TOMLDecodeError: Expected '=' after a key in a key/value pair (at end of document)", false},
	{"expected equals after space", "a 1", "TOMLDecodeError: Expected '=' after a key in a key/value pair (at line 1, column 3)", false},
	{"expected equals dotted", "a.b", "TOMLDecodeError: Expected '=' after a key in a key/value pair (at end of document)", false},
	{"expected equals after quoted key", "\"a\"x = 1", "TOMLDecodeError: Expected '=' after a key in a key/value pair (at line 1, column 4)", false},
	{"expected equals triple quote key", "\"\"\" = 1", "TOMLDecodeError: Expected '=' after a key in a key/value pair (at line 1, column 3)", false},
	{"expected equals newline", "a\n= 1", "TOMLDecodeError: Expected '=' after a key in a key/value pair (at line 1, column 2)", false},
	{"expected equals in inline table", "a = {b}", "TOMLDecodeError: Expected '=' after a key in a key/value pair (at line 1, column 7)", false},
	{"key parts limit", "a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a = 1", "RecursionError: TOML key has more than the allowed 1000 parts", false},
	{"key parts limit header", "[a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a]", "RecursionError: TOML key has more than the allowed 1000 parts", false},
	{"key parts limit array header", "[[a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a]]", "RecursionError: TOML key has more than the allowed 1000 parts", false},
	{"key parts limit inline", "x = {a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a = 1}", "RecursionError: TOML key has more than the allowed 1000 parts", false},
	{"key parts limit after bad part", "a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.? = 1", "TOMLDecodeError: Invalid initial character for a key part (at line 1, column 2001)", false},
	{"invalid key part after dot", "a. = 1", "TOMLDecodeError: Invalid initial character for a key part (at line 1, column 4)", false},
	{"invalid key part double dot", "a..b = 1", "TOMLDecodeError: Invalid initial character for a key part (at line 1, column 3)", false},
	{"invalid key part end of document", "a.", "TOMLDecodeError: Invalid initial character for a key part (at end of document)", false},
	{"invalid key part inline newline", "a = {\n}", "TOMLDecodeError: Invalid initial character for a key part (at line 1, column 6)", false},
	{"invalid key part inline comma", "a = {,}", "TOMLDecodeError: Invalid initial character for a key part (at line 1, column 6)", false},
	{"invalid key part inline trailing comma", "a = {b = 1,}", "TOMLDecodeError: Invalid initial character for a key part (at line 1, column 12)", false},
	{"invalid key part unicode", "a.\xc3\xa9 = 1", "TOMLDecodeError: Invalid initial character for a key part (at line 1, column 3)", false},
	{"unclosed array missing comma", "a = [1 2]", "TOMLDecodeError: Unclosed array (at line 1, column 8)", false},
	{"unclosed array eof", "a = [1", "TOMLDecodeError: Unclosed array (at end of document)", false},
	{"unclosed array nested eof", "a = [[1], [2", "TOMLDecodeError: Unclosed array (at end of document)", false},
	{"unclosed array bad separator", "a = [1; 2]", "TOMLDecodeError: Unclosed array (at line 1, column 7)", false},
	{"array comma then eof", "a = [1,", "TOMLDecodeError: Invalid value (at end of document)", false},
	{"array empty element", "a = [,]", "TOMLDecodeError: Invalid value (at line 1, column 6)", false},
	{"array double comma", "a = [1,,]", "TOMLDecodeError: Invalid value (at line 1, column 8)", false},
	{"inline mutate frozen table", "a = {b = {}, b.c = 1}", "TOMLDecodeError: Cannot mutate immutable namespace ('b', 'c') (at line 1, column 21)", false},
	{"inline mutate frozen array", "a = {b = [], b.c = 1}", "TOMLDecodeError: Cannot mutate immutable namespace ('b', 'c') (at line 1, column 21)", false},
	{"inline overwrite value", "a = {b = 1, b.c = 1}", "TOMLDecodeError: Cannot overwrite a value (at line 1, column 20)", false},
	{"inline duplicate key", "a = {b = 1, b = 2}", "TOMLDecodeError: Duplicate inline table key 'b' (at line 1, column 18)", false},
	{"inline duplicate key after dotted", "a = {b.c = 1, b = 2}", "TOMLDecodeError: Duplicate inline table key 'b' (at line 1, column 20)", false},
	{"inline duplicate nested key", "a = {b.c = 1, b.c = 2}", "TOMLDecodeError: Duplicate inline table key 'c' (at line 1, column 22)", false},
	{"inline duplicate key repr", "a = {\"it's\" = 1, \"it's\" = 2}", "TOMLDecodeError: Duplicate inline table key \"it's\" (at line 1, column 28)", false},
	{"inline duplicate key repr nonprintable", "a = {\"\\u00a0\" = 1, \"\\u00a0\" = 2}", "TOMLDecodeError: Duplicate inline table key '\\xa0' (at line 1, column 32)", false},
	{"inline duplicate empty key", "a = {\"\" = 1, '' = 2}", "TOMLDecodeError: Duplicate inline table key '' (at line 1, column 20)", false},
	{"unclosed inline table", "a = {b = 1 c = 2}", "TOMLDecodeError: Unclosed inline table (at line 1, column 12)", false},
	{"unclosed inline table eof", "a = {b = 1", "TOMLDecodeError: Unclosed inline table (at end of document)", false},
	{"unclosed inline table newline", "a = {b = 1\n}", "TOMLDecodeError: Unclosed inline table (at line 1, column 11)", false},
	{"inline table not frozen before last pair", "a = {b = {}}\n", "", false},
	{"unescaped backslash", "a = \"\\q\"", "TOMLDecodeError: Unescaped '\\' in a string (at line 1, column 8)", false},
	{"unescaped backslash space", "a = \"\\ \"", "TOMLDecodeError: Unescaped '\\' in a string (at line 1, column 8)", false},
	{"unescaped backslash at eof", "a = \"\\", "TOMLDecodeError: Unescaped '\\' in a string (at end of document)", false},
	{"unescaped backslash multiline at eof", "a = \"\"\"\\", "TOMLDecodeError: Unescaped '\\' in a string (at end of document)", false},
	{"unescaped backslash multiline whitespace then text", "a = \"\"\"\\   x\"\"\"", "TOMLDecodeError: Unescaped '\\' in a string (at line 1, column 12)", false},
	{"unescaped backslash multiline tab then text", "a = \"\"\"\\\tx\"\"\"", "TOMLDecodeError: Unescaped '\\' in a string (at line 1, column 10)", false},
	{"multiline whitespace escape at eof", "a = \"\"\"\\ ", "TOMLDecodeError: Unterminated string (at end of document)", false},
	{"unescaped backslash x escape", "a = \"\\x41\"", "TOMLDecodeError: Unescaped '\\' in a string (at line 1, column 8)", false},
	{"unescaped backslash in key", "\"\\a\" = 1", "TOMLDecodeError: Unescaped '\\' in a string (at line 1, column 4)", false},
	{"invalid hex short", "a = \"\\u12\"", "TOMLDecodeError: Invalid hex value (at line 1, column 8)", false},
	{"invalid hex chars", "a = \"\\uZZZZ\"", "TOMLDecodeError: Invalid hex value (at line 1, column 8)", false},
	{"invalid hex long escape short", "a = \"\\U1234567\"", "TOMLDecodeError: Invalid hex value (at line 1, column 8)", false},
	{"invalid hex at eof", "a = \"\\u", "TOMLDecodeError: Invalid hex value (at end of document)", false},
	{"invalid hex lowercase u long", "a = \"\\U0000004G\"", "TOMLDecodeError: Invalid hex value (at line 1, column 8)", false},
	{"invalid hex multiline", "a = \"\"\"\\uGGGG\"\"\"", "TOMLDecodeError: Invalid hex value (at line 1, column 10)", false},
	{"not scalar surrogate", "a = \"\\uD800\"", "TOMLDecodeError: Escaped character is not a Unicode scalar value (at line 1, column 12)", false},
	{"not scalar low surrogate", "a = \"\\uDFFF\"", "TOMLDecodeError: Escaped character is not a Unicode scalar value (at line 1, column 12)", false},
	{"not scalar too large", "a = \"\\U00110000\"", "TOMLDecodeError: Escaped character is not a Unicode scalar value (at line 1, column 16)", false},
	{"not scalar max", "a = \"\\UFFFFFFFF\"", "TOMLDecodeError: Escaped character is not a Unicode scalar value (at line 1, column 16)", false},
	{"not scalar in key", "\"\\uDC80\" = 1", "TOMLDecodeError: Escaped character is not a Unicode scalar value (at line 1, column 8)", false},
	{"unterminated string", "a = \"abc", "TOMLDecodeError: Unterminated string (at end of document)", false},
	{"unterminated string key", "\"abc = 1", "TOMLDecodeError: Unterminated string (at end of document)", false},
	{"unterminated multiline string", "a = \"\"\"abc\n", "TOMLDecodeError: Unterminated string (at end of document)", false},
	{"unterminated multiline two quotes", "a = \"\"\"abc\"\"", "TOMLDecodeError: Unterminated string (at end of document)", false},
	{"illegal character newline", "a = \"x\ny\"", "TOMLDecodeError: Illegal character '\\n' (at line 1, column 7)", false},
	{"illegal character nul", "a = \"x\x00y\"", "TOMLDecodeError: Illegal character '\\x00' (at line 1, column 7)", false},
	{"illegal character del", "a = \"x\x7fy\"", "TOMLDecodeError: Illegal character '\\x7f' (at line 1, column 7)", false},
	{"illegal character escape char", "a = \"x\x1by\"", "TOMLDecodeError: Illegal character '\\x1b' (at line 1, column 7)", false},
	{"illegal character bare cr", "a = \"x\ry\"", "TOMLDecodeError: Illegal character '\\r' (at line 1, column 7)", false},
	{"illegal character multiline control", "a = \"\"\"x\x0cy\"\"\"", "TOMLDecodeError: Illegal character '\\x0c' (at line 1, column 9)", false},
	{"illegal character multiline bare cr", "a = \"\"\"x\ry\"\"\"", "TOMLDecodeError: Illegal character '\\r' (at line 1, column 9)", false},
	{"illegal character key", "\"x\x01\" = 1", "TOMLDecodeError: Illegal character '\\x01' (at line 1, column 3)", false},
	{"invalid date february 30", "a = 2021-02-30", "TOMLDecodeError: Invalid date or datetime (at line 1, column 5)", false},
	{"invalid date year zero", "a = 0000-01-01", "TOMLDecodeError: Invalid date or datetime (at line 1, column 5)", false},
	{"invalid date not leap", "a = 2023-02-29T00:00:00Z", "TOMLDecodeError: Invalid date or datetime (at line 1, column 5)", false},
	{"invalid date century", "a = 1900-02-29 12:00:00", "TOMLDecodeError: Invalid date or datetime (at line 1, column 5)", false},
	{"invalid date april 31", "a = 2021-04-31T00:00:00+01:00", "TOMLDecodeError: Invalid date or datetime (at line 1, column 5)", false},
	{"invalid date in array", "a = [1979-05-27, 1979-06-31]", "TOMLDecodeError: Invalid date or datetime (at line 1, column 18)", false},
	{"invalid value", "a = ?", "TOMLDecodeError: Invalid value (at line 1, column 5)", false},
	{"invalid value eof", "a =", "TOMLDecodeError: Invalid value (at end of document)", false},
	{"invalid value eof spaces", "a = ", "TOMLDecodeError: Invalid value (at end of document)", false},
	{"invalid value tru", "a = tru", "TOMLDecodeError: Invalid value (at line 1, column 5)", false},
	{"invalid value leading dot", "a = .5", "TOMLDecodeError: Invalid value (at line 1, column 5)", false},
	{"invalid value plus", "a = +", "TOMLDecodeError: Invalid value (at line 1, column 5)", false},
	{"invalid value bare word", "a = hello", "TOMLDecodeError: Invalid value (at line 1, column 5)", false},
	{"invalid value surrogate", "a = \xff", "TOMLDecodeError: Invalid value (at line 1, column 5)", false},
	{"invalid value newline", "a =\n1", "TOMLDecodeError: Invalid value (at line 1, column 4)", false},
	{"expected newline after month 13", "a = 2024-13-01", "TOMLDecodeError: Expected newline or end of document after a statement (at line 1, column 9)", false},
	{"invalid value inline", "a = {b = }", "TOMLDecodeError: Invalid value (at line 1, column 10)", false},
	{"int digit limit", "a = 99999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999", "ValueError: Exceeds the limit (4300 digits) for integer string conversion: value has 4301 digits; use sys.set_int_max_str_digits() to increase the limit", false},
	{"int digit limit underscores", "a = -9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9_9", "ValueError: Exceeds the limit (4300 digits) for integer string conversion: value has 4301 digits; use sys.set_int_max_str_digits() to increase the limit", false},
	{"int digit limit plus", "a = +11111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111111", "ValueError: Exceeds the limit (4300 digits) for integer string conversion: value has 5000 digits; use sys.set_int_max_str_digits() to increase the limit", false},
	{"int digit limit in array", "a = [1, 22222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222222]", "ValueError: Exceeds the limit (4300 digits) for integer string conversion: value has 4400 digits; use sys.set_int_max_str_digits() to increase the limit", false},
	{"arrays below limit", "a=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]", "", false},
	{"arrays at limit", "a=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]", "RecursionError: maximum recursion depth exceeded", false},
	{"inline tables below limit", "a={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b=1}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}", "", false},
	{"inline tables at limit", "a={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b={b=1}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}", "RecursionError: maximum recursion depth exceeded", false},
	{"arrays with inline table leaf below limit", "a=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[{}]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]", "", false},
	{"arrays with inline table leaf at limit", "a=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[{}]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]", "RecursionError: maximum recursion depth exceeded", false},
	{"arrays with error leaf below limit", "a=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[x", "TOMLDecodeError: Invalid value (at line 1, column 499)", false},
	{"arrays with error leaf at limit", "a=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[x", "RecursionError: maximum recursion depth exceeded", false},
	{"arrays with hex escape leaf below limit", "a=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[\"\\u00e9\"]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]", "", false},
	{"arrays with hex escape leaf at limit", "a=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[\"\\u00e9\"]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]", "RecursionError: maximum recursion depth exceeded", false},
	{"arrays with multiline escape leaf below limit", "a=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[\"\"\"\\U0001F600\"\"\"]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]", "", false},
	{"arrays with multiline escape leaf at limit", "a=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[\"\"\"\\U0001F600\"\"\"]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]", "RecursionError: maximum recursion depth exceeded", false},
	{"arrays with deep comment below limit", "a=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[# deep\n]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]", "", false},
	{"arrays with deep comment at limit", "a=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[# deep\n]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]", "RecursionError: maximum recursion depth exceeded", false},
	{"arrays with bad comment below limit", "a=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[# \x00\n]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]", "TOMLDecodeError: Found invalid character '\\x00' (at line 1, column 500)", false},
	{"arrays with bad comment at limit", "a=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[# \x00\n]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]", "RecursionError: maximum recursion depth exceeded", false},
	{"mixed nesting below limit", "a=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=1}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]", "", false},
	{"mixed nesting at limit", "a=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=[{b=1}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]}]", "RecursionError: maximum recursion depth exceeded", false},
	{"dotted inline nesting below limit", "a={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c=1}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}", "", false},
	{"dotted inline nesting at limit", "a={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c={b.c=1}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}", "RecursionError: maximum recursion depth exceeded", false},
	{"arrays with invalid date leaf below limit", "a=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[2021-02-30]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]", "TOMLDecodeError: Invalid date or datetime (at line 1, column 499)", false},
	{"arrays with invalid date leaf at limit", "a=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[2021-02-30]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]", "RecursionError: maximum recursion depth exceeded", false},
	{"arrays with local time leaf below limit", "a=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[07:32:00]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]", "", false},
	{"arrays with local time leaf at limit", "a=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[07:32:00]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]", "RecursionError: maximum recursion depth exceeded", false},
	{"arrays with bad escape leaf below limit", "a=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[\"\\q\"", "TOMLDecodeError: Unescaped '\\' in a string (at line 1, column 501)", false},
	{"arrays with bad escape leaf at limit", "a=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[\"\\q\"", "RecursionError: maximum recursion depth exceeded", false},
	{"arrays with unicode key leaf below limit", "a=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[{'\xc3\xa9'=1}]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]", "", false},
	{"arrays with unicode key leaf at limit", "a=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[{'\xc3\xa9'=1}]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]", "RecursionError: maximum recursion depth exceeded", false},
	{"arrays under table below limit", "[t]\nx.y=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]", "", false},
	{"arrays under table at limit", "[t]\nx.y=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]", "RecursionError: maximum recursion depth exceeded", false},
	{"arrays with offset datetime leaf below limit", "a=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[1979-05-27T07:32:00+13:47]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]", "", true},
	{"arrays with offset datetime leaf at limit", "a=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[1979-05-27T07:32:00+13:47]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]", "RecursionError: maximum recursion depth exceeded", true},
	{"arrays with offset datetime leaf warm cache primer", "x = 1979-05-27T07:32:00+13:47", "", true},
	{"arrays with offset datetime leaf warm cache at limit", "a=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[1979-05-27T07:32:00+13:47]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]", "RecursionError: maximum recursion depth exceeded", false},
	{"arrays with offset datetime leaf warm cache above limit", "a=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[1979-05-27T07:32:00+13:47]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]", "RecursionError: maximum recursion depth exceeded", false},
	{"wrapped arrays with offset datetime leaf below limit", "a={b=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[1979-05-27T07:32:00-09:13]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]}", "", true},
	{"wrapped arrays with offset datetime leaf at limit", "a={b=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[1979-05-27T07:32:00-09:13]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]}", "RecursionError: maximum recursion depth exceeded", true},
	{"wrapped arrays with offset datetime leaf warm cache primer", "x = 1979-05-27T07:32:00-09:13", "", true},
	{"wrapped arrays with offset datetime leaf warm cache at limit", "a={b=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[1979-05-27T07:32:00-09:13]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]}", "", false},
	{"wrapped arrays with offset datetime leaf warm cache above limit", "a={b=[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[1979-05-27T07:32:00-09:13]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]}", "RecursionError: maximum recursion depth exceeded", false},
}

// Runs every golden case in order, clearing the offset cache where the recorded run cleared it
func TestTomllibLoadsErrorGolden(t *testing.T) {
	for i, c := range tomllibGoldenCases {
		if c.resetTz {
			tomllibTzMu.Lock()
			clear(tomllibTzCache)
			tomllibTzMu.Unlock()
		}
		if got := tomllibLoadsError(c.in); got != c.want {
			t.Errorf("case %d %s\n got %q\nwant %q", i, c.name, got, c.want)
		}
	}
}

type ruamelCParseCase struct {
	name string
	in   string
	want string
}

// Compares each case against the verdict the real ruamel CParser produced for it
func ruamelCParseCheck(t *testing.T, run func(string) string, cases []ruamelCParseCase) {
	t.Helper()
	for _, c := range cases {
		if got := run(c.in); got != c.want {
			t.Errorf("%s\n got  %q\n want %q", c.name, got, c.want)
		}
	}
}

func ruamelCParseBytesRun(in string) string {
	return ruamelCParseStream(in, "<byte string>")
}

// Builds n mapping keys, each line indented one column deeper than the line before
func ruamelCParseStaircase(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(strings.Repeat(" ", i))
		b.WriteString("k:\n")
	}
	return b.String()
}

// Valid streams covering every block, flow, scalar, property and directive construct
func TestRuamelCParseValidDocuments(t *testing.T) {
	ruamelCParseCheck(t, ruamelCParseError, []ruamelCParseCase{
		{"empty", "", ""},
		{"newline only", "\n", ""},
		{"comment only", "# just a comment\n", ""},
		{"blank lines", "   \n\n  \n", ""},
		{"block mapping", "a: 1\nb: 2\n", ""},
		{"block sequence", "- a\n- b\n", ""},
		{"nested mapping sequence", "a:\n  - b\n  - c\nd: e\n", ""},
		{"indentless sequence", "a:\n- b\n- c\nd:\n- e\n", ""},
		{"sequence of mappings", "- a: 1\n  b: 2\n- c\n", ""},
		{"sequence of sequences", "- - a\n  - b\n- - c\n", ""},
		{"explicit key", "? a\n: b\n", ""},
		{"complex sequence key", "? - a\n  - b\n: - c\n", ""},
		{"block scalar key", "? |\n  block key\n: value\n", ""},
		{"explicit keys no values", "? a\n? b\n", ""},
		{"empty values", "a:\nb:\n", ""},
		{"empty sequence entries", "-\n- \n-\n", ""},
		{"flow sequence", "[a, b, c]\n", ""},
		{"flow mapping", "{a: b, c: d}\n", ""},
		{"nested flow", "[a, [b, c], {d: e}]\n", ""},
		{"nested flow mapping", "{a: [b], c: {d: e}}\n", ""},
		{"empty flow", "[]\n---\n{}\n", ""},
		{"flow trailing comma", "[a, b, ]\n", ""},
		{"flow mapping trailing comma", "{a: b, }\n", ""},
		{"flow keys without values", "{a, b}\n", ""},
		{"flow single pair", "[a: b]\n", ""},
		{"flow single pairs", "[a: b, c: d]\n", ""},
		{"flow explicit key in sequence", "[? a : b]\n", ""},
		{"flow explicit key in mapping", "{? a : b}\n", ""},
		{"flow multiline", "[a,\n b,\n  c]\n", ""},
		{"flow in block", "a: [1, 2]\nb: {c: d}\n", ""},
		{"flow quoted adjacent value", "{\"a\":b}\n", ""},
		{"plain multiline", "a b\n c d\n", ""},
		{"plain with colon inside", "a:b: c\n", ""},
		{"plain url", "url: http://example.com/a?b=c#frag\n", ""},
		{"plain dash start", "-a: b\n--c: d\n", ""},
		{"plain question start", "?a: b\n", ""},
		{"plain with hash inside", "a#b: c\n", ""},
		{"plain with tab inside", "a\tb: c\n", ""},
		{"plain unicode", "\u00e9: \u4e2d\nemoji: \U0001f600\n", ""},
		{"single quoted", "'a''b'\n", ""},
		{"single quoted multiline", "'multi\n  line\n\n  text'\n", ""},
		{"double quoted escapes", "\"\\0\\a\\b\\t\\\t\\n\\v\\f\\r\\e\\ \\\"\\\\\\N\\_\\L\\P\\x41\\u263A\\U0001F600\\/\"\n", ""},
		{"double quoted line continuation", "\"a\\\n  b\"\n", ""},
		{"double quoted multiline", "\"multi\n  line\n\n  text\"\n", ""},
		{"double quoted crlf folding", "\"a\r\n  b\"\r\n", ""},
		{"literal", "|\n a\n b\n", ""},
		{"literal strip", "|-\n a\n", ""},
		{"literal keep", "|+\n a\n\n", ""},
		{"literal indent indicator", "|2\n   a\n  b\n", ""},
		{"literal indicator then chomp", "|1-\n  a\n", ""},
		{"literal chomp then indicator", "|+2\n   a\n", ""},
		{"literal leading blank lines", "|\n\n\n  a\n", ""},
		{"folded", ">\n a\n b\n\n c\n", ""},
		{"folded strip", ">-\n a\n", ""},
		{"folded keep", ">+\n a\n\n", ""},
		{"folded indent indicator", ">2\n   a\n", ""},
		{"folded comment after indicator", "> # comment\n a\n", ""},
		{"folded more indented", ">\n a\n   b\n c\n", ""},
		{"block scalar in mapping", "a: |\n  x\nb: >\n  y\n", ""},
		{"block scalar in sequence", "- |\n  x\n- >-\n  y\n", ""},
		{"block scalar empty", "a: |\nb: >\n", ""},
		{"anchor and alias", "a: &x 1\nb: *x\n", ""},
		{"anchor on collection", "- &a [1]\n- *a\n", ""},
		{"undefined alias", "*undefined\n", ""},
		{"alias in flow", "[&a x, *a, *b]\n", ""},
		{"anchor empty value", "a: &x\nb: c\n", ""},
		{"duplicate anchor", "- &a 1\n- &a 2\n", ""},
		{"duplicate keys", "a: 1\na: 2\n", ""},
		{"anchor alias as key", "&k a: 1\n*k : 2\n", ""},
		{"secondary tag", "!!str a\n", ""},
		{"local tag", "!local a\n", ""},
		{"verbatim tag", "!<tag:yaml.org,2002:str> a\n", ""},
		{"non specific tag", "! a\n", ""},
		{"tag on flow mapping", "!!map {a: b}\n", ""},
		{"tag on document", "--- !foo\na: b\n", ""},
		{"anchor then tag", "&a !t x\n", ""},
		{"tag then anchor", "!t &a x\n", ""},
		{"tag escaped ascii", "!%41 x\n", ""},
		{"verbatim tag escaped euro", "!<%E2%82%AC> x\n", ""},
		{"tag with flow chars", "!a,b x\n", ""},
		{"tag in flow sequence", "[!!str, a]\n", ""},
		{"tag only value", "a: !t\nb: c\n", ""},
		{"unknown tag", "!foo bar\n", ""},
		{"tag nul truncates", "!<%00%C0%80> x\n", ""},
		{"yaml directive 1.1", "%YAML 1.1\n---\na\n", ""},
		{"yaml directive 1.2", "%YAML 1.2\n--- a\n", ""},
		{"yaml directive comment", "%YAML 1.1 # comment\n---\n", ""},
		{"tag directive primary", "%TAG ! tag:x,2000:\n--- !y z\n", ""},
		{"tag directive secondary", "%TAG !! tag:x:\n--- !!y z\n", ""},
		{"tag directive named", "%YAML 1.1\n%TAG !e! tag:e,2000:\n--- !e!x y\n", ""},
		{"tag directive emoji suffix", "%TAG !e! tag:e:\n--- !e!%F0%9F%98%80 x\n", ""},
		{"directives per document", "%TAG !e! a:\n--- !e!x 1\n...\n%TAG !e! b:\n--- !e!y 2\n", ""},
		{"multi document", "a\n---\nb\n...\n---\nc\n", ""},
		{"empty documents", "---\n...\n---\n---\n", ""},
		{"documents inline content", "--- a\n--- b\n", ""},
		{"directive after end", "a\n...\n%YAML 1.1\n---\nb\n", ""},
		{"repeated document end", "a\n...\n...\n", ""},
		{"bom at start", "\ufeffa: b\n", ""},
		{"crlf", "a: b\r\nc: d\r\n", ""},
		{"cr", "a: b\rc: d\r", ""},
		{"nel", "a: b\u0085c: d", ""},
		{"line separator", "a: b\u2028c: d", ""},
		{"paragraph separator", "a: b\u2029c: d\u2029", ""},
		{"tab in flow", "[a,\tb]\n", ""},
		{"tab before comment", "a: b\t# c\n", ""},
		{"tab after quoted", "- \"a\"\t\n", ""},
		{"tab trailing value", "key:    value\t\n", ""},
		{"tab after flow entry", "{a: b,\tc: d}\n", ""},
		{"comments everywhere", "# c\na: b # c\n# c\nc: d # c\n", ""},
		{"tab after implicit key colon", "a:\tb\n", ""},
		{"tab after colon and space", "a: \tb\n", ""},
		{"tag with at sign", "!a@ b\n", ""},
		{"tab in plain continuation at indent", "k:\n  a\n \tb\n", ""},
		{"document end after content", "a\n...\n", ""},
		{"long plain key", strings.Repeat("k", 1023) + ": v\n", ""},
		{"max plain value", "a: " + strings.Repeat("v", 5000) + "\n", ""},
		{"nul escape in double quoted", "\"a\\0b\"\n", ""},
		{"allowed boundary characters", "\"\ud7ff\ue000\ufffd\U00010000\U0010ffff\u00a0\u0085\"\n", ""},
		{"json style", "{\"a\": [1, 2.5, true, null, \"s\"]}\n", ""},
		{"block scalar crlf", "a: |\r\n  x\r\n  y\r\n", ""},
		{"key with spaces", "a key with spaces: value\n", ""},
		{"sequence in mapping value same line flow", "a: [b, c]\n", ""},
		{"quoted keys", "\"a\": 1\n'b': 2\n", ""},
		{"nested indentation", "a:\n  b:\n    c:\n      d: e\n  f: g\n", ""},
		{"plain trailing spaces", "a: b   \n", ""},
		{"document start comment", "--- # c\na\n", ""},
		{"indicator chars inside plain", "a: b-c?d:e,f[g]h{i}j\n", ""},
	})
}

// Every reachable scanner error with each context variant
func TestRuamelCParseScannerErrors(t *testing.T) {
	ruamelCParseCheck(t, ruamelCParseError, []ruamelCParseCase{
		{"cannot start token at", "@a\n", "YAMLError: while scanning for the next token\nfound character that cannot start any token\n  in \"<unicode string>\", line 1, column 1"},
		{"cannot start token backtick", "`a\n", "YAMLError: while scanning for the next token\nfound character that cannot start any token\n  in \"<unicode string>\", line 1, column 1"},
		{"cannot start token percent mid line", "a: %x\n", "YAMLError: while scanning for the next token\nfound character that cannot start any token\n  in \"<unicode string>\", line 1, column 4"},
		{"cannot start token pipe in flow", "[|]\n", "YAMLError: while scanning for the next token\nfound character that cannot start any token\n  in \"<unicode string>\", line 1, column 2"},
		{"cannot start token gt in flow", "[>]\n", "YAMLError: while scanning for the next token\nfound character that cannot start any token\n  in \"<unicode string>\", line 1, column 2"},
		{"cannot start token leading tab", "\ta: b\n", "YAMLError: while scanning for the next token\nfound character that cannot start any token\n  in \"<unicode string>\", line 1, column 1"},
		{"simple key stale required", "a: 1\nb\nc: 2\n", "YAMLError: while scanning a simple key\n  in \"<unicode string>\", line 2, column 1\ncould not find expected ':'\n  in \"<unicode string>\", line 3, column 1"},
		{"simple key required at stream end", "a: 1\nb", "YAMLError: while scanning a simple key\n  in \"<unicode string>\", line 2, column 1\ncould not find expected ':'\n  in \"<unicode string>\", line 3, column 1"},
		{"simple key required before flow", "a: 1\nb [c]\n", "YAMLError: while scanning a simple key\n  in \"<unicode string>\", line 2, column 1\ncould not find expected ':'\n  in \"<unicode string>\", line 3, column 1"},
		{"simple key required document start", "a: 1\nb\n---\n", "YAMLError: while scanning a simple key\n  in \"<unicode string>\", line 2, column 1\ncould not find expected ':'\n  in \"<unicode string>\", line 3, column 1"},
		{"simple key required too long", "a: 1\n" + strings.Repeat("k", 1025) + ": v\n", "YAMLError: while scanning a simple key\n  in \"<unicode string>\", line 2, column 1\ncould not find expected ':'\n  in \"<unicode string>\", line 2, column 1026"},
		{"block entry not allowed", "\"a\" - b\n", "YAMLError: block sequence entries are not allowed in this context\n  in \"<unicode string>\", line 1, column 5"},
		{"block entry not allowed after tag", "a: !t - b\n", "YAMLError: block sequence entries are not allowed in this context\n  in \"<unicode string>\", line 1, column 7"},
		{"mapping key not allowed", "\"a\" ? b\n", "YAMLError: mapping keys are not allowed in this context\n  in \"<unicode string>\", line 1, column 5"},
		{"mapping value not allowed", "a: b: c\n", "YAMLError: mapping values are not allowed in this context\n  in \"<unicode string>\", line 1, column 5"},
		{"mapping value inside sequence entry line", "- a\n  b: c\n- d\nx: y\n", "YAMLError: mapping values are not allowed in this context\n  in \"<unicode string>\", line 2, column 4"},
		{"simple key required anchored line", "- a\n&x b\n", "YAMLError: while scanning a simple key\n  in \"<unicode string>\", line 2, column 1\ncould not find expected ':'\n  in \"<unicode string>\", line 3, column 1"},
		{"mapping value after stale key", strings.Repeat("k", 1025) + ": v\n", "YAMLError: mapping values are not allowed in this context\n  in \"<unicode string>\", line 1, column 1026"},
		{"directive unknown name", "%FOO bar\n", "YAMLError: while scanning a directive\n  in \"<unicode string>\", line 1, column 1\nfound unknown directive name\n  in \"<unicode string>\", line 1, column 5"},
		{"directive trailing junk", "%YAML 1.1 x\n", "YAMLError: while scanning a directive\n  in \"<unicode string>\", line 1, column 1\ndid not find expected comment or line break\n  in \"<unicode string>\", line 1, column 11"},
		{"directive empty name", "% YAML\n", "YAMLError: while scanning a directive\n  in \"<unicode string>\", line 1, column 1\ncould not find expected directive name\n  in \"<unicode string>\", line 1, column 2"},
		{"directive bad name char", "%YA!ML 1.1\n", "YAMLError: while scanning a directive\n  in \"<unicode string>\", line 1, column 1\nfound unexpected non-alphabetical character\n  in \"<unicode string>\", line 1, column 4"},
		{"yaml directive bad separator", "%YAML 1x1\n", "YAMLError: while scanning a %YAML directive\n  in \"<unicode string>\", line 1, column 1\ndid not find expected digit or '.' character\n  in \"<unicode string>\", line 1, column 8"},
		{"yaml directive long number", "%YAML 1234567890.1\n", "YAMLError: while scanning a %YAML directive\n  in \"<unicode string>\", line 1, column 1\nfound extremely long version number\n  in \"<unicode string>\", line 1, column 16"},
		{"yaml directive long minor", "%YAML 1.0123456789\n", "YAMLError: while scanning a %YAML directive\n  in \"<unicode string>\", line 1, column 1\nfound extremely long version number\n  in \"<unicode string>\", line 1, column 18"},
		{"yaml directive missing number", "%YAML .1\n", "YAMLError: while scanning a %YAML directive\n  in \"<unicode string>\", line 1, column 1\ndid not find expected version number\n  in \"<unicode string>\", line 1, column 7"},
		{"yaml directive missing all", "%YAML\n", "YAMLError: while scanning a %YAML directive\n  in \"<unicode string>\", line 1, column 1\ndid not find expected version number\n  in \"<unicode string>\", line 1, column 6"},
		{"tag directive missing whitespace", "%TAG !e!x tag:x\n", "YAMLError: while scanning a %TAG directive\n  in \"<unicode string>\", line 1, column 1\ndid not find expected whitespace\n  in \"<unicode string>\", line 1, column 9"},
		{"tag directive junk after uri", "%TAG !e! tag:x\"y\n", "YAMLError: while scanning a %TAG directive\n  in \"<unicode string>\", line 1, column 1\ndid not find expected whitespace or line break\n  in \"<unicode string>\", line 1, column 15"},
		{"tag directive handle without bang", "%TAG e tag:x\n", "YAMLError: while scanning a tag directive\n  in \"<unicode string>\", line 1, column 1\ndid not find expected '!'\n  in \"<unicode string>\", line 1, column 6"},
		{"tag directive handle unterminated", "%TAG !e tag:x\n", "YAMLError: while parsing a tag directive\n  in \"<unicode string>\", line 1, column 1\ndid not find expected '!'\n  in \"<unicode string>\", line 1, column 8"},
		{"tag directive bad escape", "%TAG !e! %ZZ\n", "YAMLError: while parsing a %TAG directive\n  in \"<unicode string>\", line 1, column 1\ndid not find URI escaped octet\n  in \"<unicode string>\", line 1, column 10"},
		{"tag directive truncated escape", "%TAG !e! %4\n", "YAMLError: while parsing a %TAG directive\n  in \"<unicode string>\", line 1, column 1\ndid not find URI escaped octet\n  in \"<unicode string>\", line 1, column 10"},
		{"tag directive bad leading octet", "%TAG !e! %FF\n", "YAMLError: while parsing a %TAG directive\n  in \"<unicode string>\", line 1, column 1\nfound an incorrect leading UTF-8 octet\n  in \"<unicode string>\", line 1, column 10"},
		{"tag directive bad trailing octet", "%TAG !e! %C3%41\n", "YAMLError: while parsing a %TAG directive\n  in \"<unicode string>\", line 1, column 1\nfound an incorrect trailing UTF-8 octet\n  in \"<unicode string>\", line 1, column 13"},
		{"tag directive missing uri", "%TAG !e! \n", "YAMLError: while parsing a %TAG directive\n  in \"<unicode string>\", line 1, column 1\ndid not find expected tag URI\n  in \"<unicode string>\", line 1, column 10"},
		{"tag directive missing everything", "%TAG\n", "YAMLError: while scanning a tag directive\n  in \"<unicode string>\", line 1, column 1\ndid not find expected '!'\n  in \"<unicode string>\", line 1, column 5"},
		{"anchor empty", "&\n", "YAMLError: while scanning an anchor\n  in \"<unicode string>\", line 1, column 1\ndid not find expected alphabetic or numeric character\n  in \"<unicode string>\", line 1, column 2"},
		{"anchor ends at at sign", "&a@ b\n", "YAMLError: while scanning for the next token\nfound character that cannot start any token\n  in \"<unicode string>\", line 1, column 3"},
		{"anchor followed by quote", "&a\"b\"\n", "YAMLError: while scanning an anchor\n  in \"<unicode string>\", line 1, column 1\ndid not find expected alphabetic or numeric character\n  in \"<unicode string>\", line 1, column 3"},
		{"alias empty", "*\n", "YAMLError: while scanning an alias\n  in \"<unicode string>\", line 1, column 1\ndid not find expected alphabetic or numeric character\n  in \"<unicode string>\", line 1, column 2"},
		{"alias ends at at sign", "- *a@\n", "YAMLError: while scanning for the next token\nfound character that cannot start any token\n  in \"<unicode string>\", line 1, column 5"},
		{"verbatim tag unterminated", "!<a b\n", "YAMLError: while scanning a tag\n  in \"<unicode string>\", line 1, column 1\ndid not find the expected '>'\n  in \"<unicode string>\", line 1, column 4"},
		{"verbatim tag empty", "!<>\n", "YAMLError: while parsing a tag\n  in \"<unicode string>\", line 1, column 1\ndid not find expected tag URI\n  in \"<unicode string>\", line 1, column 3"},
		{"tag directive handle missing uri", "!e! x\n", "YAMLError: while parsing a tag\n  in \"<unicode string>\", line 1, column 1\ndid not find expected tag URI\n  in \"<unicode string>\", line 1, column 4"},
		{"tag bad escape", "!%ZZ x\n", "YAMLError: while parsing a tag\n  in \"<unicode string>\", line 1, column 1\ndid not find URI escaped octet\n  in \"<unicode string>\", line 1, column 2"},
		{"tag bad leading octet", "!%FF x\n", "YAMLError: while parsing a tag\n  in \"<unicode string>\", line 1, column 1\nfound an incorrect leading UTF-8 octet\n  in \"<unicode string>\", line 1, column 2"},
		{"tag bad trailing octet", "!%C3%41 x\n", "YAMLError: while parsing a tag\n  in \"<unicode string>\", line 1, column 1\nfound an incorrect trailing UTF-8 octet\n  in \"<unicode string>\", line 1, column 5"},
		{"verbatim tag bad trailing octet", "!<%E2%82> x\n", "YAMLError: while parsing a tag\n  in \"<unicode string>\", line 1, column 1\ndid not find URI escaped octet\n  in \"<unicode string>\", line 1, column 9"},
		{"tag quote after", "!t\"x\"\n", "YAMLError: while scanning a tag\n  in \"<unicode string>\", line 1, column 1\ndid not find expected whitespace or line break\n  in \"<unicode string>\", line 1, column 3"},
		{"block scalar zero indent", "|0\n a\n", "YAMLError: while scanning a block scalar\n  in \"<unicode string>\", line 1, column 1\nfound an indentation indicator equal to 0\n  in \"<unicode string>\", line 1, column 2"},
		{"block scalar chomp then zero indent", "|-0\n a\n", "YAMLError: while scanning a block scalar\n  in \"<unicode string>\", line 1, column 1\nfound an indentation indicator equal to 0\n  in \"<unicode string>\", line 1, column 3"},
		{"block scalar junk header", "|x\n a\n", "YAMLError: while scanning a block scalar\n  in \"<unicode string>\", line 1, column 1\ndid not find expected comment or line break\n  in \"<unicode string>\", line 1, column 2"},
		{"block scalar tab indentation", "|\n\ta\n", "YAMLError: while scanning a block scalar\n  in \"<unicode string>\", line 1, column 1\nfound a tab character where an indentation space is expected\n  in \"<unicode string>\", line 2, column 1"},
		{"block scalar tab after spaces", "a: |\n  x\n \ty\n", "YAMLError: while scanning a block scalar\n  in \"<unicode string>\", line 1, column 4\nfound a tab character where an indentation space is expected\n  in \"<unicode string>\", line 3, column 2"},
		{"quoted document start inside", "'a\n---\n'\n", "YAMLError: while scanning a quoted scalar\n  in \"<unicode string>\", line 1, column 1\nfound unexpected document indicator\n  in \"<unicode string>\", line 2, column 1"},
		{"quoted document end inside", "\"a\n...\n\"\n", "YAMLError: while scanning a quoted scalar\n  in \"<unicode string>\", line 1, column 1\nfound unexpected document indicator\n  in \"<unicode string>\", line 2, column 1"},
		{"quoted unterminated", "\"abc", "YAMLError: while scanning a quoted scalar\n  in \"<unicode string>\", line 1, column 1\nfound unexpected end of stream\n  in \"<unicode string>\", line 1, column 5"},
		{"single quoted unterminated", "'abc\n", "YAMLError: while scanning a quoted scalar\n  in \"<unicode string>\", line 1, column 1\nfound unexpected end of stream\n  in \"<unicode string>\", line 2, column 1"},
		{"unknown escape", "\"\\q\"\n", "YAMLError: while parsing a quoted scalar\n  in \"<unicode string>\", line 1, column 1\nfound unknown escape character\n  in \"<unicode string>\", line 1, column 2"},
		{"escaped single quote", "\"\\'\"\n", "YAMLError: while parsing a quoted scalar\n  in \"<unicode string>\", line 1, column 1\nfound unknown escape character\n  in \"<unicode string>\", line 1, column 2"},
		{"escape at end of stream", "\"\\", "YAMLError: while parsing a quoted scalar\n  in \"<unicode string>\", line 1, column 1\nfound unknown escape character\n  in \"<unicode string>\", line 1, column 2"},
		{"short hex escape", "\"\\x4\"\n", "YAMLError: while parsing a quoted scalar\n  in \"<unicode string>\", line 1, column 1\ndid not find expected hexdecimal number\n  in \"<unicode string>\", line 1, column 4"},
		{"bad hex escape", "\"\\xZZ\"\n", "YAMLError: while parsing a quoted scalar\n  in \"<unicode string>\", line 1, column 1\ndid not find expected hexdecimal number\n  in \"<unicode string>\", line 1, column 4"},
		{"bad unicode escape", "\"\\u12G4\"\n", "YAMLError: while parsing a quoted scalar\n  in \"<unicode string>\", line 1, column 1\ndid not find expected hexdecimal number\n  in \"<unicode string>\", line 1, column 4"},
		{"surrogate escape", "\"\\uD800\"\n", "YAMLError: while parsing a quoted scalar\n  in \"<unicode string>\", line 1, column 1\nfound invalid Unicode character escape code\n  in \"<unicode string>\", line 1, column 4"},
		{"out of range escape", "\"\\U00110000\"\n", "YAMLError: while parsing a quoted scalar\n  in \"<unicode string>\", line 1, column 1\nfound invalid Unicode character escape code\n  in \"<unicode string>\", line 1, column 4"},
		{"flow plain colon", "[a:b]\n", "YAMLError: while scanning a plain scalar\n  in \"<unicode string>\", line 1, column 2\nfound unexpected ':'\n  in \"<unicode string>\", line 1, column 3"},
		{"flow plain colon in mapping", "{a:b}\n", "YAMLError: while scanning a plain scalar\n  in \"<unicode string>\", line 1, column 2\nfound unexpected ':'\n  in \"<unicode string>\", line 1, column 3"},
		{"plain tab violates indentation", "a: b\n\tc\n", "YAMLError: while scanning a plain scalar\n  in \"<unicode string>\", line 1, column 4\nfound a tab character that violates indentation\n  in \"<unicode string>\", line 2, column 1"},
		{"plain tab violates indentation nested", "k:\n  a\n\tb\n", "YAMLError: while scanning a plain scalar\n  in \"<unicode string>\", line 2, column 3\nfound a tab character that violates indentation\n  in \"<unicode string>\", line 3, column 1"},
	})
}

// Every reachable parser error including the flow pair key token skip
func TestRuamelCParseParserErrors(t *testing.T) {
	ruamelCParseCheck(t, ruamelCParseError, []ruamelCParseCase{
		{"block collection missing dash", "- a\nb: c\n", "YAMLError: while parsing a block collection\n  in \"<unicode string>\", line 1, column 1\ndid not find expected '-' indicator\n  in \"<unicode string>\", line 2, column 1"},
		{"document start missing after scalar", "\"a\"\nb\n", "YAMLError: did not find expected <document start>\n  in \"<unicode string>\", line 2, column 1"},
		{"document start missing after directive", "%YAML 1.1\na\n", "YAMLError: did not find expected <document start>\n  in \"<unicode string>\", line 2, column 1"},
		{"document start missing after document end", "a\n...\nb\n", "YAMLError: did not find expected <document start>\n  in \"<unicode string>\", line 3, column 1"},
		{"undefined tag handle", "!x!y z\n", "YAMLError: while parsing a node\nfound undefined tag handle\n  in \"<unicode string>\", line 1, column 1"},
		{"undefined tag handle anchored", "&a !x!y z\n", "YAMLError: while parsing a node\n  in \"<unicode string>\", line 1, column 1\nfound undefined tag handle\n  in \"<unicode string>\", line 1, column 4"},
		{"tag handle out of scope", "%TAG !e! a:\n--- !e!x 1\n--- !e!y 2\n", "YAMLError: while parsing a node\nfound undefined tag handle\n  in \"<unicode string>\", line 3, column 5"},
		{"block node missing content", "a: ]\n", "YAMLError: while parsing a block node\ndid not find expected node content\n  in \"<unicode string>\", line 1, column 4"},
		{"block node missing content brace", "}\n", "YAMLError: while parsing a block node\ndid not find expected node content\n  in \"<unicode string>\", line 1, column 1"},
		{"flow node missing content", "[a, }]\n", "YAMLError: while parsing a flow node\ndid not find expected node content\n  in \"<unicode string>\", line 1, column 5"},
		{"flow node missing content mapping", "{a: ]}\n", "YAMLError: while parsing a flow node\ndid not find expected node content\n  in \"<unicode string>\", line 1, column 5"},
		{"flow node missing at end", strings.Repeat("[", 3), "YAMLError: while parsing a flow node\ndid not find expected node content\n  in \"<unicode string>\", line 2, column 1"},
		{"block mapping empty key", ": v\n", "YAMLError: while parsing a block mapping\ndid not find expected key\n  in \"<unicode string>\", line 1, column 1"},
		{"flow mapping empty key", "{: b, a: }\n", "YAMLError: while parsing a flow node\ndid not find expected node content\n  in \"<unicode string>\", line 1, column 2"},
		{"document end at stream start", "...\n", "YAMLError: while parsing a block node\ndid not find expected node content\n  in \"<unicode string>\", line 1, column 1"},
		{"bom mid stream shifts column", "a: b\n\ufeffc: d\n", "YAMLError: while parsing a block mapping\n  in \"<unicode string>\", line 1, column 1\ndid not find expected key\n  in \"<unicode string>\", line 2, column 2"},
		{"block mapping quoted pair without key", "\"a\" \"b\": c\n", "YAMLError: while parsing a block mapping\n  in \"<unicode string>\", line 1, column 1\ndid not find expected key\n  in \"<unicode string>\", line 1, column 5"},
		{"block mapping missing key", "a: b\n- c\n", "YAMLError: while parsing a block mapping\n  in \"<unicode string>\", line 1, column 1\ndid not find expected key\n  in \"<unicode string>\", line 2, column 1"},
		{"block mapping missing key nested", "a:\n  b: c\n  - d\n", "YAMLError: while parsing a block mapping\n  in \"<unicode string>\", line 2, column 3\ndid not find expected key\n  in \"<unicode string>\", line 3, column 3"},
		{"flow sequence missing comma", "[a b c\n", "YAMLError: while parsing a flow sequence\n  in \"<unicode string>\", line 1, column 1\ndid not find expected ',' or ']'\n  in \"<unicode string>\", line 2, column 1"},
		{"flow sequence unterminated", "[a\n", "YAMLError: while parsing a flow sequence\n  in \"<unicode string>\", line 1, column 1\ndid not find expected ',' or ']'\n  in \"<unicode string>\", line 2, column 1"},
		{"flow sequence bad end", "[a, b}\n", "YAMLError: while parsing a flow sequence\n  in \"<unicode string>\", line 1, column 1\ndid not find expected ',' or ']'\n  in \"<unicode string>\", line 1, column 6"},
		{"flow mapping missing comma", "{a: b c: d\n", "YAMLError: while parsing a flow mapping\n  in \"<unicode string>\", line 1, column 1\ndid not find expected ',' or '}'\n  in \"<unicode string>\", line 1, column 8"},
		{"flow mapping unterminated", "{a: b\n", "YAMLError: while parsing a flow mapping\n  in \"<unicode string>\", line 1, column 1\ndid not find expected ',' or '}'\n  in \"<unicode string>\", line 2, column 1"},
		{"flow mapping bad end", "{a: b]\n", "YAMLError: while parsing a flow mapping\n  in \"<unicode string>\", line 1, column 1\ndid not find expected ',' or '}'\n  in \"<unicode string>\", line 1, column 6"},
		{"flow sequence empty key skips end", "[?]\n", "YAMLError: while parsing a flow sequence\n  in \"<unicode string>\", line 1, column 1\ndid not find expected ',' or ']'\n  in \"<unicode string>\", line 2, column 1"},
		{"flow sequence explicit empty key value", "[? : x]\n", "YAMLError: while parsing a flow sequence\n  in \"<unicode string>\", line 1, column 1\ndid not find expected ',' or ']'\n  in \"<unicode string>\", line 1, column 6"},
		{"duplicate yaml directive", "%YAML 1.1\n%YAML 1.1\n---\n", "YAMLError: found duplicate %YAML directive\n  in \"<unicode string>\", line 2, column 1"},
		{"incompatible yaml 2.0", "%YAML 2.0\n---\n", "YAMLError: found incompatible YAML document\n  in \"<unicode string>\", line 1, column 1"},
		{"incompatible yaml 1.0", "%YAML 1.0\n---\n", "YAMLError: found incompatible YAML document\n  in \"<unicode string>\", line 1, column 1"},
		{"incompatible yaml 1.3", "%YAML 1.3\n---\na\n", "YAMLError: found incompatible YAML document\n  in \"<unicode string>\", line 1, column 1"},
		{"duplicate tag directive", "%TAG !e! a\n%TAG !e! b\n---\n", "YAMLError: found duplicate %TAG directive\n  in \"<unicode string>\", line 2, column 1"},
		{"duplicate primary tag directive", "%TAG ! a\n%TAG ! b\n---\n", "YAMLError: found duplicate %TAG directive\n  in \"<unicode string>\", line 2, column 1"},
		{"two anchors", "&a &b x\n", "YAMLError: did not find expected <document start>\n  in \"<unicode string>\", line 1, column 4"},
		{"directive inside document", "a: b\n%YAML 1.1\n", "YAMLError: did not find expected <document start>\n  in \"<unicode string>\", line 3, column 1"},
	})
}

// Characters the reader rejects with byte offsets from the encoded str
func TestRuamelCParseReaderErrors(t *testing.T) {
	ruamelCParseCheck(t, ruamelCParseError, []ruamelCParseCase{
		{"nul only", "\x00", "YAMLError: unacceptable character #x0000: control characters are not allowed\n  in \"<unicode string>\", position 0"},
		{"nul after text", "a\x00", "YAMLError: unacceptable character #x0000: control characters are not allowed\n  in \"<unicode string>\", position 1"},
		{"nul in value", "a: \x00\n", "YAMLError: unacceptable character #x0000: control characters are not allowed\n  in \"<unicode string>\", position 3"},
		{"soh", "\x01", "YAMLError: unacceptable character #x0001: control characters are not allowed\n  in \"<unicode string>\", position 0"},
		{"backspace", "a\x08", "YAMLError: unacceptable character #x0008: control characters are not allowed\n  in \"<unicode string>\", position 1"},
		{"vertical tab", "a\x0b", "YAMLError: unacceptable character #x000b: control characters are not allowed\n  in \"<unicode string>\", position 1"},
		{"form feed", "a\x0c", "YAMLError: unacceptable character #x000c: control characters are not allowed\n  in \"<unicode string>\", position 1"},
		{"unit separator", "\x1f", "YAMLError: unacceptable character #x001f: control characters are not allowed\n  in \"<unicode string>\", position 0"},
		{"delete", "abc\x7f", "YAMLError: unacceptable character #x007f: control characters are not allowed\n  in \"<unicode string>\", position 3"},
		{"c1 control 80", "\u0080", "YAMLError: unacceptable character #x0080: control characters are not allowed\n  in \"<unicode string>\", position 0"},
		{"c1 control 84", "a: \u0084", "YAMLError: unacceptable character #x0084: control characters are not allowed\n  in \"<unicode string>\", position 3"},
		{"c1 control 86", "\u0086", "YAMLError: unacceptable character #x0086: control characters are not allowed\n  in \"<unicode string>\", position 0"},
		{"c1 control 9f", "\u009f", "YAMLError: unacceptable character #x009f: control characters are not allowed\n  in \"<unicode string>\", position 0"},
		{"noncharacter fffe", "\ufffe", "YAMLError: unacceptable character #xfffe: control characters are not allowed\n  in \"<unicode string>\", position 0"},
		{"noncharacter ffff", "a: \uffff", "YAMLError: unacceptable character #xffff: control characters are not allowed\n  in \"<unicode string>\", position 3"},
		{"control after multibyte", "\u00e9\u4e2d\U0001f600\x00", "YAMLError: unacceptable character #x0000: control characters are not allowed\n  in \"<unicode string>\", position 9"},
		{"control after bom", "\ufeff\x00", "YAMLError: unacceptable character #x0000: control characters are not allowed\n  in \"<unicode string>\", position 3"},
		{"control before scanner error", "a: b: c\x00", "YAMLError: unacceptable character #x0000: control characters are not allowed\n  in \"<unicode string>\", position 7"},
		{"control after quoted error", "\"\\q\" \x07", "YAMLError: unacceptable character #x0007: control characters are not allowed\n  in \"<unicode string>\", position 5"},
		{"control in comment", "# \x1b\n", "YAMLError: unacceptable character #x001b: control characters are not allowed\n  in \"<unicode string>\", position 2"},
		{"control deep in first chunk", "a: b: c\n" + strings.Repeat("#", 16000) + "\x00", "YAMLError: unacceptable character #x0000: control characters are not allowed\n  in \"<unicode string>\", position 16008"},
	})
}

// Invalid UTF-8 bytes standing for lone surrogates fail the str encode first
func TestRuamelCParseSurrogates(t *testing.T) {
	ruamelCParseCheck(t, ruamelCParseError, []ruamelCParseCase{
		{"lone surrogate", "\xff", "UnicodeEncodeError: 'utf-8' codec can't encode character '\\udcff' in position 0: surrogates not allowed"},
		{"surrogate after ascii", "abc\xff", "UnicodeEncodeError: 'utf-8' codec can't encode character '\\udcff' in position 3: surrogates not allowed"},
		{"surrogate after multibyte", "\u00e9\U0001f600\x80x", "UnicodeEncodeError: 'utf-8' codec can't encode character '\\udc80' in position 2: surrogates not allowed"},
		{"surrogate run", "ab\xff\xfe", "UnicodeEncodeError: 'utf-8' codec can't encode characters in position 2-3: surrogates not allowed"},
		{"surrogate run long", "\x80\x81\x82\x83", "UnicodeEncodeError: 'utf-8' codec can't encode characters in position 0-3: surrogates not allowed"},
		{"first run reported", "a\xff b\xfe\xfd", "UnicodeEncodeError: 'utf-8' codec can't encode character '\\udcff' in position 1: surrogates not allowed"},
		{"surrogate before nul", "\x00\xff", "UnicodeEncodeError: 'utf-8' codec can't encode character '\\udcff' in position 1: surrogates not allowed"},
		{"surrogate before scanner error", "a: b: c\xff", "UnicodeEncodeError: 'utf-8' codec can't encode character '\\udcff' in position 7: surrogates not allowed"},
		{"encoded surrogate bytes", pystr.SurrogateEscapeDecode([]byte("a\xed\xa0\x80")), "UnicodeEncodeError: 'utf-8' codec can't encode characters in position 1-3: surrogates not allowed"},
		{"lone surrogate code point", "a\xed\xa0\x80", "UnicodeEncodeError: 'utf-8' codec can't encode character '\\ud800' in position 1: surrogates not allowed"},
		{"escape range code point", "x\xed\xb2\x80y", "UnicodeEncodeError: 'utf-8' codec can't encode character '\\udc80' in position 1: surrogates not allowed"},
		{"overlong bytes", "\xc0\x80", "UnicodeEncodeError: 'utf-8' codec can't encode characters in position 0-1: surrogates not allowed"},
		{"truncated sequence at end", "key: \xe2\x82", "UnicodeEncodeError: 'utf-8' codec can't encode characters in position 5-6: surrogates not allowed"},
		{"truncated four byte", "\xf0\x9f\x98 x", "UnicodeEncodeError: 'utf-8' codec can't encode characters in position 0-2: surrogates not allowed"},
		{"above max code point", "\xf4\x90\x80\x80", "UnicodeEncodeError: 'utf-8' codec can't encode characters in position 0-3: surrogates not allowed"},
		{"lone continuation", "a: \x80b", "UnicodeEncodeError: 'utf-8' codec can't encode character '\\udc80' in position 3: surrogates not allowed"},
		{"invalid lead f8", "\xf8\x88\x80\x80\x80", "UnicodeEncodeError: 'utf-8' codec can't encode characters in position 0-4: surrogates not allowed"},
		{"separate runs", "\xff \xfe\xfd", "UnicodeEncodeError: 'utf-8' codec can't encode character '\\udcff' in position 0: surrogates not allowed"},
		{"surrogate deep in large input", strings.Repeat("x", 20000) + "\xff", "UnicodeEncodeError: 'utf-8' codec can't encode character '\\udcff' in position 20000: surrogates not allowed"},
	})
}

// Tags whose escaped octets fail strict UTF-8 decoding at event conversion
func TestRuamelCParseTagDecode(t *testing.T) {
	ruamelCParseCheck(t, ruamelCParseError, []ruamelCParseCase{
		{"verbatim encoded surrogate", "!<%ED%A0%80> x\n", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xed in position 0: invalid continuation byte"},
		{"shorthand overlong", "!%C0%80 x\n", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xc0 in position 1: invalid start byte"},
		{"verbatim overlong three byte", "!<%E0%80%80> x\n", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xe0 in position 0: invalid continuation byte"},
		{"verbatim overlong four byte", "!<%F0%80%80%80> x\n", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xf0 in position 0: invalid continuation byte"},
		{"verbatim above max", "!<%F4%90%80%80> x\n", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xf4 in position 0: invalid continuation byte"},
		{"verbatim f5 lead", "!<%F5%80%80%80> x\n", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xf5 in position 0: invalid start byte"},
		{"verbatim c1 lead", "!<%C1%BF> x\n", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xc1 in position 0: invalid start byte"},
		{"after valid prefix", "!<abc%E2%82%AC%C0%80> x\n", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xc0 in position 6: invalid start byte"},
		{"secondary handle position", "!!%C0%80 x\n", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xc0 in position 18: invalid start byte"},
		{"named handle prefix position", "%TAG !e! tag:e:\n--- !e!%ED%BF%BF x\n", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xed in position 6: invalid continuation byte"},
		{"tag directive prefix", "%TAG !e! %ED%A0%80\n--- a\n", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xed in position 0: invalid continuation byte"},
		{"second tag directive prefix", "%TAG !a! x\n%TAG !b! y%C1%81\n--- a\n", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xc1 in position 1: invalid start byte"},
		{"on flow sequence", "!<%C0%80> [a]\n", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xc0 in position 0: invalid start byte"},
		{"on flow mapping", "!<%C0%80> {a: b}\n", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xc0 in position 0: invalid start byte"},
		{"on block mapping", "!<%C0%80>\na: b\n", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xc0 in position 0: invalid start byte"},
		{"on empty node", "a: !<%C0%80>\n", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xc0 in position 0: invalid start byte"},
		{"anchor then bad tag", "&a !<%C0%80> x\n", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xc0 in position 0: invalid start byte"},
		{"tag decode before later scanner error", "!<%C0%80> x\n\"unterminated\n", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xc0 in position 0: invalid start byte"},
		{"scanner lookahead error wins", "!<%C0%80>\nx\n\"unterminated\n", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xc0 in position 0: invalid start byte"},
		{"earlier event error wins", "!<%C0%80> a\n---\n!<%ED%A0%80> b\n", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xc0 in position 0: invalid start byte"},
		{"nul before bad octets", "%TAG !e! y\n--- !e!%00%C0%80 x\n", ""},
		{"tag on later document", "ok\n--- !<%C0%80> x\n", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xc0 in position 0: invalid start byte"},
	})
}

// Reader errors surface per 16384 byte raw chunk ahead of the scanner
func TestRuamelCParseChunkBoundaries(t *testing.T) {
	ruamelCParseCheck(t, ruamelCParseError, []ruamelCParseCase{
		{"nul in second chunk", strings.Repeat("x", 20000) + "\x00", "YAMLError: unacceptable character #x0000: control characters are not allowed\n  in \"<unicode string>\", position 20000"},
		{"scanner error before second chunk nul", "a: b: c\n" + strings.Repeat("x", 20000) + "\x00", "YAMLError: mapping values are not allowed in this context\n  in \"<unicode string>\", line 1, column 5"},
		{"nul at last first chunk byte", "a: b: c\n" + strings.Repeat("#", 16375) + "\x00", "YAMLError: unacceptable character #x0000: control characters are not allowed\n  in \"<unicode string>\", position 16383"},
		{"nul at first second chunk byte", "a: b: c\n" + strings.Repeat("#", 16376) + "\x00", "YAMLError: mapping values are not allowed in this context\n  in \"<unicode string>\", line 1, column 5"},
		{"multibyte straddles first boundary then nul", "a: b: c\n" + strings.Repeat("#", 16375) + "\u00e9\x00", "YAMLError: mapping values are not allowed in this context\n  in \"<unicode string>\", line 1, column 5"},
		{"multibyte ends first chunk then nul", "a: b: c\n" + strings.Repeat("#", 16374) + "\u00e9\x00", "YAMLError: mapping values are not allowed in this context\n  in \"<unicode string>\", line 1, column 5"},
		{"four byte straddles boundary", "a: b: c\n" + strings.Repeat("#", 16374) + "\U0001f600\x00", "YAMLError: mapping values are not allowed in this context\n  in \"<unicode string>\", line 1, column 5"},
		{"straddle shifts third chunk reader", strings.Repeat("#", 16383) + "\u00e9\na: b: c\n" + strings.Repeat("#", 16372) + "\x00", "YAMLError: unacceptable character #x0000: control characters are not allowed\n  in \"<unicode string>\", position 32766"},
		{"straddle shifts third chunk scanner", strings.Repeat("#", 16383) + "\u00e9\na: b: c\n" + strings.Repeat("#", 16373) + "\x00", "YAMLError: mapping values are not allowed in this context\n  in \"<unicode string>\", line 2, column 5"},
		{"aligned third chunk reader", strings.Repeat("#", 16384) + "\na: b: c\n" + strings.Repeat("#", 16374) + "\x00", "YAMLError: unacceptable character #x0000: control characters are not allowed\n  in \"<unicode string>\", position 32767"},
		{"aligned third chunk scanner", strings.Repeat("#", 16384) + "\na: b: c\n" + strings.Repeat("#", 16375) + "\x00", "YAMLError: mapping values are not allowed in this context\n  in \"<unicode string>\", line 2, column 5"},
		{"scanner error in second chunk", strings.Repeat("#", 16390) + "\na: b: c\n", "YAMLError: mapping values are not allowed in this context\n  in \"<unicode string>\", line 2, column 5"},
		{"valid across chunks", strings.Repeat("k: v\n", 10000), ""},
		{"quoted scalar across boundary", "a: \"" + strings.Repeat("q", 20000) + "\"\n", ""},
		{"block scalar across boundary", "a: |\n" + strings.Repeat("  line\n", 5000), ""},
		{"crlf across boundary", strings.Repeat("#", 16383) + "\r\na: b\r\n", ""},
		{"nel straddles boundary", strings.Repeat("#", 16383) + "\u0085a: b: c\n", "YAMLError: mapping values are not allowed in this context\n  in \"<unicode string>\", line 2, column 5"},
	})
}

// Simple keys expire past 1024 characters or a line break
func TestRuamelCParseSimpleKeyLimit(t *testing.T) {
	ruamelCParseCheck(t, ruamelCParseError, []ruamelCParseCase{
		{"plain key 1024", strings.Repeat("k", 1024) + ": v\n", ""},
		{"plain key 1025 top level", strings.Repeat("k", 1025) + ": v\n", "YAMLError: mapping values are not allowed in this context\n  in \"<unicode string>\", line 1, column 1026"},
		{"plain key 1023 in mapping", "a: 1\n" + strings.Repeat("k", 1023) + ": v\n", ""},
		{"plain key 1024 in mapping", "a: 1\n" + strings.Repeat("k", 1024) + ": v\n", ""},
		{"quoted key 1024", "\"" + strings.Repeat("q", 1022) + "\": v\n", ""},
		{"quoted key 1025", "\"" + strings.Repeat("q", 1023) + "\": v\n", "YAMLError: mapping values are not allowed in this context\n  in \"<unicode string>\", line 1, column 1026"},
		{"flow key 1024", "{" + strings.Repeat("k", 1023) + ": v}\n", ""},
		{"flow key 1025", "{" + strings.Repeat("k", 1024) + ": v}\n", ""},
		{"flow sequence pair 1025", "[" + strings.Repeat("k", 1024) + ": v]\n", ""},
		{"flow collection key", "[" + strings.Repeat("a", 1020) + "]: v\n", ""},
		{"flow collection key too long", "[" + strings.Repeat("a", 1030) + "]: v\n", "YAMLError: mapping values are not allowed in this context\n  in \"<unicode string>\", line 1, column 1033"},
		{"key on next line", "a\n: v\n", "YAMLError: did not find expected <document start>\n  in \"<unicode string>\", line 2, column 1"},
	})
}

// Deep flow and block nesting parses without recursion
func TestRuamelCParseDeepNesting(t *testing.T) {
	ruamelCParseCheck(t, ruamelCParseError, []ruamelCParseCase{
		{"flow sequences 10000", strings.Repeat("[", 10000) + strings.Repeat("]", 10000), ""},
		{"flow mappings 5000", strings.Repeat("{a: ", 5000) + strings.Repeat("}", 5000), ""},
		{"flow sequences unclosed", strings.Repeat("[", 10000), "YAMLError: while parsing a flow node\ndid not find expected node content\n  in \"<unicode string>\", line 2, column 1"},
		{"flow mixed unclosed", strings.Repeat("[{a: ", 3000), "YAMLError: while parsing a flow node\ndid not find expected node content\n  in \"<unicode string>\", line 2, column 1"},
		{"block sequences 100000", strings.Repeat("- ", 100000) + "x", ""},
		{"block sequences error far right", strings.Repeat("- ", 100000) + "\"", "YAMLError: while scanning a quoted scalar\n  in \"<unicode string>\", line 1, column 200001\nfound unexpected end of stream\n  in \"<unicode string>\", line 1, column 200002"},
		{"explicit keys 100000", strings.Repeat("? ", 100000) + "x", ""},
		{"block sequence then flow", strings.Repeat("- ", 50000) + strings.Repeat("[", 1000) + strings.Repeat("]", 1000), ""},
		{"indented mappings", ruamelCParseStaircase(1500) + strings.Repeat(" ", 1500) + "v\n", ""},
		{"indented mappings bad dedent", ruamelCParseStaircase(1500) + strings.Repeat(" ", 700) + "- x\n", "YAMLError: while parsing a block mapping\n  in \"<unicode string>\", line 701, column 701\ndid not find expected key\n  in \"<unicode string>\", line 1501, column 701"},
	})
}

// Block nesting fails with MemoryError once the marks stack passes INT_MAX over two bytes, run at a scaled down limit
func TestRuamelCParseStackLimit(t *testing.T) {
	if libyamlStackByteLimit != math.MaxInt32/2 {
		t.Fatalf("stack byte limit = %d, want %d", libyamlStackByteLimit, math.MaxInt32/2)
	}
	if got := libyamlStackMaxLen(libyamlStackByteLimit, libyamlSizeofMark); got != 67108864 {
		t.Fatalf("marks held at the real limit = %d, want 67108864", got)
	}
	const marks = 1024
	prev := libyamlStackByteLimit
	libyamlStackByteLimit = marks * libyamlSizeofMark
	t.Cleanup(func() { libyamlStackByteLimit = prev })
	if got := libyamlStackMaxLen(libyamlStackByteLimit, libyamlSizeofMark); got != marks {
		t.Fatalf("marks held at the scaled limit = %d, want %d", got, marks)
	}
	ruamelCParseCheck(t, ruamelCParseError, []ruamelCParseCase{
		{"block depth at mark stack limit", strings.Repeat("- ", marks) + "\"", "YAMLError: while scanning a quoted scalar\n  in \"<unicode string>\", line 1, column 2049\nfound unexpected end of stream\n  in \"<unicode string>\", line 1, column 2050"},
		{"block depth over mark stack limit", strings.Repeat("- ", marks+1) + "\"", "MemoryError: "},
	})
}

// Entries a libyaml stack holds before growth past the byte limit is refused, capacity doubling from sixteen
func libyamlStackMaxLen(limit, elemSize int) int {
	capacity := libyamlInitialStackSize
	for capacity*elemSize < limit {
		capacity *= 2
	}
	return capacity
}

// Byte input through the same reader covering UTF-16 and every UTF-8 decoding error
func TestRuamelCParseByteStream(t *testing.T) {
	ruamelCParseCheck(t, ruamelCParseBytesRun, []ruamelCParseCase{
		{"utf8 bytes valid", "a: b\n", ""},
		{"utf8 bom bytes valid", "\ufeffa: b\n", ""},
		{"utf16le valid", "\xff\xfea\x00:\x00 \x00[\x00b\x00,\x00 \x00\"\x00=\xd8\x00\xde\"\x00]\x00\n\x00", ""},
		{"utf16be valid", "\xfe\xff\x00a\x00:\x00 \x00[\x00b\x00,\x00 \x00\"\xd8=\xde\x00\x00\"\x00]\x00\n", ""},
		{"utf16le scanner error", "\xff\xfe\xe9\x00:\x00 \x00b\x00:\x00 \x00c\x00\n\x00", "YAMLError: mapping values are not allowed in this context\n  in \"<byte string>\", line 1, column 5"},
		{"utf16be parser error", "\xfe\xff\x00[\x00a\x00 \x00b\x00\n", "YAMLError: while parsing a flow sequence\n  in \"<byte string>\", line 1, column 1\ndid not find expected ',' or ']'\n  in \"<byte string>\", line 2, column 1"},
		{"utf16le control character", "\xff\xfea\x00b\x00\x00\x00", "YAMLError: unacceptable character #x0000: control characters are not allowed\n  in \"<byte string>\", position 6"},
		{"utf16le incomplete character", "\xff\xfea\x00:\x00 \x00b\x00c", "YAMLError: unacceptable character #x-001: incomplete UTF-16 character\n  in \"<byte string>\", position 10"},
		{"utf16le unexpected low surrogate", "\xff\xfea\x00\x00\xdc", "YAMLError: unacceptable character #xdc00: unexpected low surrogate area\n  in \"<byte string>\", position 4"},
		{"utf16be unexpected low surrogate", "\xfe\xff\xdc\x00", "YAMLError: unacceptable character #xdc00: unexpected low surrogate area\n  in \"<byte string>\", position 2"},
		{"utf16le incomplete surrogate pair", "\xff\xfea\x00=\xd8", "YAMLError: unacceptable character #x-001: incomplete UTF-16 surrogate pair\n  in \"<byte string>\", position 4"},
		{"utf16le incomplete surrogate pair odd", "\xff\xfea\x00=\xd8\x00", "YAMLError: unacceptable character #x-001: incomplete UTF-16 surrogate pair\n  in \"<byte string>\", position 4"},
		{"utf16le expected low surrogate", "\xff\xfe=\xd8a\x00", "YAMLError: unacceptable character #x0061: expected low surrogate area\n  in \"<byte string>\", position 4"},
		{"utf16be expected low surrogate", "\xfe\xff\xd8=\xd8=", "YAMLError: unacceptable character #xd83d: expected low surrogate area\n  in \"<byte string>\", position 4"},
		{"utf16le pair straddles chunk", "\xff\xfe" + strings.Repeat("a\x00", 8190) + "=\xd8\x00\xde:\x00 \x00b\x00:\x00 \x00c\x00\n\x00", "YAMLError: mapping values are not allowed in this context\n  in \"<byte string>\", line 1, column 8192"},
		{"utf16le nul in second chunk", "\xff\xfea\x00:\x00 \x00b\x00:\x00 \x00c\x00\n\x00" + strings.Repeat("#\x00", 9000) + "\x00\x00", "YAMLError: mapping values are not allowed in this context\n  in \"<byte string>\", line 1, column 5"},
		{"invalid leading octet ff", "a: \xff", "YAMLError: unacceptable character #x00ff: invalid leading UTF-8 octet\n  in \"<byte string>\", position 3"},
		{"invalid leading octet continuation", "\x80", "YAMLError: unacceptable character #x0080: invalid leading UTF-8 octet\n  in \"<byte string>\", position 0"},
		{"invalid leading octet f8", "ab\xf8\x88\x80\x80\x80", "YAMLError: unacceptable character #x00f8: invalid leading UTF-8 octet\n  in \"<byte string>\", position 2"},
		{"incomplete sequence at end", "a: \xc3", "YAMLError: unacceptable character #x-001: incomplete UTF-8 octet sequence\n  in \"<byte string>\", position 3"},
		{"incomplete four byte at end", "\xf0\x9f\x98", "YAMLError: unacceptable character #x-001: incomplete UTF-8 octet sequence\n  in \"<byte string>\", position 0"},
		{"invalid trailing octet second", "ab\xc3(", "YAMLError: unacceptable character #x0028: invalid trailing UTF-8 octet\n  in \"<byte string>\", position 3"},
		{"invalid trailing octet third", "\xe2\x82(", "YAMLError: unacceptable character #x0028: invalid trailing UTF-8 octet\n  in \"<byte string>\", position 2"},
		{"invalid trailing octet fourth", "x\xf0\x9f\x98A", "YAMLError: unacceptable character #x0041: invalid trailing UTF-8 octet\n  in \"<byte string>\", position 4"},
		{"overlong two byte", "\xc0\x80", "YAMLError: unacceptable character #x-001: invalid length of a UTF-8 sequence\n  in \"<byte string>\", position 0"},
		{"overlong three byte", "a\xe0\x80\x80", "YAMLError: unacceptable character #x-001: invalid length of a UTF-8 sequence\n  in \"<byte string>\", position 1"},
		{"overlong four byte", "\xf0\x80\x80\x80", "YAMLError: unacceptable character #x-001: invalid length of a UTF-8 sequence\n  in \"<byte string>\", position 0"},
		{"encoded surrogate", "a: \xed\xa0\x80", "YAMLError: unacceptable character #xd800: invalid Unicode character\n  in \"<byte string>\", position 3"},
		{"above max code point", "\xf4\x90\x80\x80", "YAMLError: unacceptable character #x110000: invalid Unicode character\n  in \"<byte string>\", position 0"},
		{"max lead f7", "\xf7\xbf\xbf\xbf", "YAMLError: unacceptable character #x1fffff: invalid Unicode character\n  in \"<byte string>\", position 0"},
		{"bom then invalid", "\ufeff\xff", "YAMLError: unacceptable character #x00ff: invalid leading UTF-8 octet\n  in \"<byte string>\", position 3"},
		{"control in bytes", "a\x00", "YAMLError: unacceptable character #x0000: control characters are not allowed\n  in \"<byte string>\", position 1"},
		{"incomplete straddles chunk end", strings.Repeat("#", 16383) + "\xc3", "YAMLError: unacceptable character #x-001: incomplete UTF-8 octet sequence\n  in \"<byte string>\", position 16383"},
		{"invalid after first chunk scanner wins", "a: b: c\n" + strings.Repeat("#", 16380) + "\xff", "YAMLError: mapping values are not allowed in this context\n  in \"<byte string>\", line 1, column 5"},
		{"utf16 bom inside utf8 stream", "a: \xff\xfe", "YAMLError: unacceptable character #x00ff: invalid leading UTF-8 octet\n  in \"<byte string>\", position 3"},
	})
}

// Strict UTF-8 failures rendered exactly as the CPython decoder reports them
func TestRuamelCParseUTF8Decoder(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"abc", ""},
		{"\U0001f600", ""},
		{"\U0010ffff", ""},
		{"\x80", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0x80 in position 0: invalid start byte"},
		{"\xbf", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xbf in position 0: invalid start byte"},
		{"\xc0\x80", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xc0 in position 0: invalid start byte"},
		{"\xc1\xbf", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xc1 in position 0: invalid start byte"},
		{"\xc2", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xc2 in position 0: unexpected end of data"},
		{"\xc2A", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xc2 in position 0: invalid continuation byte"},
		{"\xdf", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xdf in position 0: unexpected end of data"},
		{"\xe0", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xe0 in position 0: unexpected end of data"},
		{"\xe0\x80\x80", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xe0 in position 0: invalid continuation byte"},
		{"\xe0\x9f", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xe0 in position 0: invalid continuation byte"},
		{"\xe0\xa0", "UnicodeDecodeError: 'utf-8' codec can't decode bytes in position 0-1: unexpected end of data"},
		{"\xe0\xa0A", "UnicodeDecodeError: 'utf-8' codec can't decode bytes in position 0-1: invalid continuation byte"},
		{"\xe0A", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xe0 in position 0: invalid continuation byte"},
		{"\xed\xa0\x80", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xed in position 0: invalid continuation byte"},
		{"\xed\xa0", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xed in position 0: invalid continuation byte"},
		{"\xed\x9f", "UnicodeDecodeError: 'utf-8' codec can't decode bytes in position 0-1: unexpected end of data"},
		{"\xed\x9fA", "UnicodeDecodeError: 'utf-8' codec can't decode bytes in position 0-1: invalid continuation byte"},
		{"\xe2\x82", "UnicodeDecodeError: 'utf-8' codec can't decode bytes in position 0-1: unexpected end of data"},
		{"\xe2", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xe2 in position 0: unexpected end of data"},
		{"\xe2A\x82", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xe2 in position 0: invalid continuation byte"},
		{"\xf0", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xf0 in position 0: unexpected end of data"},
		{"\xf0\x80\x80\x80", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xf0 in position 0: invalid continuation byte"},
		{"\xf0\x8f", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xf0 in position 0: invalid continuation byte"},
		{"\xf0\x90", "UnicodeDecodeError: 'utf-8' codec can't decode bytes in position 0-1: unexpected end of data"},
		{"\xf0\x90\x80", "UnicodeDecodeError: 'utf-8' codec can't decode bytes in position 0-2: unexpected end of data"},
		{"\xf0\x90\x80A", "UnicodeDecodeError: 'utf-8' codec can't decode bytes in position 0-2: invalid continuation byte"},
		{"\xf0\x90A", "UnicodeDecodeError: 'utf-8' codec can't decode bytes in position 0-1: invalid continuation byte"},
		{"\xf0\x90A\x80", "UnicodeDecodeError: 'utf-8' codec can't decode bytes in position 0-1: invalid continuation byte"},
		{"\xf0A", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xf0 in position 0: invalid continuation byte"},
		{"\xf4\x90\x80\x80", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xf4 in position 0: invalid continuation byte"},
		{"\xf4\x90", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xf4 in position 0: invalid continuation byte"},
		{"\xf4\x8f\x80", "UnicodeDecodeError: 'utf-8' codec can't decode bytes in position 0-2: unexpected end of data"},
		{"\xf5\x80\x80\x80", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xf5 in position 0: invalid start byte"},
		{"\xf8", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xf8 in position 0: invalid start byte"},
		{"\xff", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xff in position 0: invalid start byte"},
		{"a\u20acb\xff", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xff in position 5: invalid start byte"},
		{"abc\xe2\x82", "UnicodeDecodeError: 'utf-8' codec can't decode bytes in position 3-4: unexpected end of data"},
		{"tag:\u00e9\xed\xbf\xbf", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xed in position 6: invalid continuation byte"},
	} {
		if got := ruamelUTF8DecodeError([]byte(c.in)); got != c.want {
			t.Errorf("%q\n got  %q\n want %q", c.in, got, c.want)
		}
	}
}
