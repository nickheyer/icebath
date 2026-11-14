package scenario

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

const (
	tagEnv      = "ICEBATH_SCENARIO_TAG"
	stepTimeout = 5 * time.Minute
	waitLimit   = 3 * time.Minute
	stopGrace   = 20 * time.Second
)

var scenarioSeq atomic.Int64

// Setup seeds one scenario, every text expanded with the ${NAME} variables of the scenario
type Setup struct {
	Config       string
	Extra        string
	Bare         bool
	Home         map[string]string
	User         map[string]string
	Work         map[string]string
	Env          map[string]string
	Model        []any
	ModelOptions fakellm.Options
}

// S is one running scenario with its private home, working directory, scripted model and processes
type S struct {
	t        *testing.T
	Root     string
	Home     string
	UserHome string
	Work     string
	Model    *fakellm.Server
	env      map[string]string
	tag      string
	finding  string
	mu       sync.Mutex
	vars     map[string]string
	procs    []*Proc
	closers  []func()
}

// Step is one run of the binary, Pipe keeping standard input open for a background process to be written to
type Step struct {
	Args    []string
	Stdin   string
	Pipe    bool
	Env     map[string]string
	Dir     string
	Timeout time.Duration
}

// Result is what one finished run of the binary showed its user
type Result struct {
	s      *S
	Args   []string
	Code   int
	Stdout string
	Stderr string
}

// Creates the home, the working directory and the scripted model of a scenario and registers its teardown
func Start(t *testing.T, setup Setup) *S {
	t.Helper()
	t.Parallel()
	root, err := os.MkdirTemp("", "sc")
	if err != nil {
		t.Fatal(err)
	}
	s := &S{t: t, Root: root, Home: filepath.Join(root, "h"), UserHome: filepath.Join(root, "u"), Work: filepath.Join(root, "w"),
		env: setup.Env, tag: suite.runTag + "." + strconv.FormatInt(scenarioSeq.Add(1), 10), vars: map[string]string{}}
	t.Cleanup(s.finish)
	for _, dir := range []string{s.Home, s.UserHome, s.Work, filepath.Join(root, "t")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if s.Model, err = fakellm.New(setup.Model, setup.ModelOptions); err != nil {
		t.Fatal(err)
	}
	s.vars["MODEL_URL"], s.vars["HOME"], s.vars["ICEBATH_HOME"] = s.Model.BaseURL(), s.UserHome, s.Home
	s.vars["WORK"], s.vars["TMP"], s.vars["BIN"] = s.Work, filepath.Join(root, "t"), suite.bin
	home := map[string]string{}
	if !setup.Bare {
		home["config.yaml"] = "model:\n  provider: custom\n  base_url: ${MODEL_URL}\n  default: " + fakellm.ModelID +
			"\n  context_length: 128000\nagent:\n  api_max_retries: 1\n" + setup.Extra
		if setup.Config != "" {
			home["config.yaml"] = setup.Config
		}
		home[".env"] = "OPENAI_API_KEY=sk-fake-e2e\n"
	}
	for rel, content := range setup.Home {
		home[rel] = content
	}
	for base, files := range map[string]map[string]string{s.Home: home, s.UserHome: setup.User, s.Work: setup.Work} {
		for rel, content := range files {
			s.Write(filepath.Join(base, rel), content)
		}
	}
	return s
}

// Starts one more scripted model endpoint, stopped with the scenario, its base address defined as ${MODEL_name}
func (s *S) AddModel(name string, script []any, opts fakellm.Options) *fakellm.Server {
	s.t.Helper()
	m, err := fakellm.New(script, opts)
	if err != nil {
		s.t.Fatal(err)
	}
	s.closers = append(s.closers, m.Stop)
	s.Set("MODEL_"+name, m.BaseURL())
	return m
}

// Marks the scenario as reproducing an open entry of SCENARIO_FINDINGS.md until the product matches hermes
func (s *S) Finding(id string) { s.finding = id }

// Fails the scenario, reported as the open finding when one is marked
func (s *S) Failf(format string, args ...any) {
	s.t.Helper()
	if s.finding != "" {
		s.t.Skipf("OPEN FINDING %s still reproduces: %s", s.finding, fmt.Sprintf(format, args...))
	}
	s.t.Fatalf(format, args...)
}

// Defines a ${NAME} variable for later texts
func (s *S) Set(name, value string) {
	s.mu.Lock()
	s.vars[name] = value
	s.mu.Unlock()
}

// Substitutes every ${NAME} variable of the scenario into the text, variables inside variable values included
func (s *S) Expand(text string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for pass := 0; pass < 4 && strings.Contains(text, "${"); pass++ {
		for k, v := range s.vars {
			text = strings.ReplaceAll(text, "${"+k+"}", v)
		}
	}
	return text
}

// Reserves a free loopback port and defines it as ${PORT_name}
func (s *S) Port(name string) int {
	s.t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		s.t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	s.Set("PORT_"+name, strconv.Itoa(port))
	return port
}

// Resolves a path relative to the product home, absolute paths kept
func (s *S) Path(rel string) string {
	rel = s.Expand(rel)
	if filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(s.Home, rel)
}

// Writes an expanded text file, relative paths under the product home
func (s *S) Write(rel, content string) {
	s.t.Helper()
	path := s.Path(rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s.Expand(content)), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

// Returns the content of a file, relative paths under the product home
func (s *S) Read(rel string) string {
	s.t.Helper()
	raw, err := os.ReadFile(s.Path(rel))
	if err != nil {
		s.Failf("reading %s: %v", rel, err)
	}
	return string(raw)
}

// Parses a YAML file into plain values, relative paths under the product home
func (s *S) YAML(rel string) any {
	s.t.Helper()
	var v any
	if err := yaml.Unmarshal([]byte(s.Read(rel)), &v); err != nil {
		s.Failf("%s is not YAML: %v", rel, err)
	}
	return v
}

// Reports whether a file or directory exists, relative paths under the product home
func (s *S) Exists(rel string) bool {
	_, err := os.Stat(s.Path(rel))
	return err == nil
}

// Builds the closed environment of one product process, nothing inherited from the caller of the suite
func (s *S) environ(extra map[string]string, tty bool) []string {
	name := "icebath"
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	env := map[string]string{
		"PATH": filepath.Dir(suite.bin) + ":/usr/local/bin:/usr/bin:/bin", "HOME": s.UserHome, "ICEBATH_HOME": s.Home,
		"USER": name, "LOGNAME": name, "SHELL": "/bin/bash", "LANG": "C.UTF-8", "TZ": "UTC",
		"TMPDIR": filepath.Join(s.Root, "t"), "COLUMNS": "120", "LINES": "40", "TERM": "dumb", "NO_COLOR": "1",
		"GOCOVERDIR": suite.coverDir, tagEnv: s.tag, runTagEnv: suite.runTag,
		"DBUS_SESSION_BUS_ADDRESS": "unix:path=" + filepath.Join(s.Root, "t", "no-session-bus"),
	}
	if tty {
		env["TERM"], env["COLORTERM"] = "xterm-256color", "truecolor"
		delete(env, "NO_COLOR")
	}
	for _, layer := range []map[string]string{s.env, extra} {
		for k, v := range layer {
			env[k] = s.Expand(v)
		}
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// Prepares one process of the binary in its own process group with expanded arguments
func (s *S) command(ctx context.Context, st Step, tty bool) *exec.Cmd {
	args := make([]string, len(st.Args))
	for i, a := range st.Args {
		args[i] = s.Expand(a)
	}
	cmd := exec.CommandContext(ctx, suite.bin, args...)
	cmd.Dir = s.Work
	if st.Dir != "" {
		cmd.Dir = s.Path(st.Dir)
	}
	cmd.Env = s.environ(st.Env, tty)
	if !tty {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// Runs the binary with the arguments to its end
func (s *S) Run(args ...string) *Result {
	s.t.Helper()
	return s.RunStep(Step{Args: args})
}

// Runs one step of the binary to its end, a timeout failing the scenario
func (s *S) RunStep(st Step) *Result {
	s.t.Helper()
	if st.Timeout == 0 {
		st.Timeout = stepTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), st.Timeout)
	defer cancel()
	cmd := s.command(ctx, st, false)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if st.Stdin != "" {
		cmd.Stdin = strings.NewReader(s.Expand(st.Stdin))
	}
	began := time.Now()
	err := cmd.Run()
	r := &Result{s: s, Args: st.Args, Stdout: stdout.String(), Stderr: stderr.String()}
	s.t.Logf("icebath %s took %s", strings.Join(st.Args, " "), time.Since(began).Round(time.Millisecond))
	if ctx.Err() != nil {
		s.Failf("icebath %s did not end within %s\n%s", strings.Join(st.Args, " "), st.Timeout, r.dump())
	}
	r.Code = exitCode(s, err)
	return r
}

// Converts a wait error into the exit code a shell would report
func exitCode(s *S, err error) int {
	s.t.Helper()
	if err == nil {
		return 0
	}
	exit, ok := err.(*exec.ExitError)
	if !ok {
		s.Failf("running icebath: %v", err)
	}
	if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return exit.ExitCode()
}

func (r *Result) dump() string {
	return "exit " + strconv.Itoa(r.Code) + "\n--- stdout\n" + clip(r.Stdout) + "\n--- stderr\n" + clip(r.Stderr)
}

// Keeps the head and the tail of a long text for a failure message
func clip(text string) string {
	if len(text) <= 6000 {
		return text
	}
	return text[:3000] + "\n[...]\n" + text[len(text)-3000:]
}

// Requires the exit code
func (r *Result) Exit(code int) *Result {
	r.s.t.Helper()
	if r.Code != code {
		r.s.Failf("icebath %s exited %d, want %d\n%s", strings.Join(r.Args, " "), r.Code, code, r.dump())
	}
	return r
}

// Returns everything the user read, standard output then standard error, without terminal escapes
func (r *Result) Text() string { return ansi.Strip(r.Stdout) + ansi.Strip(r.Stderr) }

// Requires every part in what the user read
func (r *Result) Has(parts ...string) *Result {
	r.s.t.Helper()
	text := r.Text()
	for _, part := range parts {
		if !strings.Contains(text, r.s.Expand(part)) {
			r.s.Failf("icebath %s did not show %q\n%s", strings.Join(r.Args, " "), part, r.dump())
		}
	}
	return r
}

// Requires that no part appears in what the user read
func (r *Result) Lacks(parts ...string) *Result {
	r.s.t.Helper()
	text := r.Text()
	for _, part := range parts {
		if strings.Contains(text, r.s.Expand(part)) {
			r.s.Failf("icebath %s showed %q\n%s", strings.Join(r.Args, " "), part, r.dump())
		}
	}
	return r
}

// Requires the pattern in what the user read and returns its submatches
func (r *Result) Match(pattern string) []string {
	r.s.t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(r.Text())
	if m == nil {
		r.s.Failf("icebath %s showed nothing matching %s\n%s", strings.Join(r.Args, " "), pattern, r.dump())
	}
	return m
}

// Parses standard output as one JSON value
func (r *Result) JSON() any {
	r.s.t.Helper()
	var v any
	if err := json.Unmarshal([]byte(r.Stdout), &v); err != nil {
		r.s.Failf("icebath %s did not print JSON: %v\n%s", strings.Join(r.Args, " "), err, r.dump())
	}
	return v
}

// Polls until the check holds, failing the scenario with the description at the deadline
func (s *S) Eventually(what string, limit time.Duration, check func() bool) {
	s.t.Helper()
	for deadline := time.Now().Add(limit); ; time.Sleep(100 * time.Millisecond) {
		if check() {
			return
		}
		if time.Now().After(deadline) {
			s.Failf("%s did not happen within %s", what, limit)
		}
	}
}

// Waits for a file, relative paths under the product home, and returns its path
func (s *S) WaitFile(rel string) string {
	s.t.Helper()
	s.Eventually("file "+rel, waitLimit, func() bool { return s.Exists(rel) })
	return s.Path(rel)
}

// Waits until a loopback port accepts connections
func (s *S) WaitPort(port int) {
	s.t.Helper()
	s.Eventually("port "+strconv.Itoa(port), waitLimit, func() bool {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	})
}

// Runs a query against a SQLite file opened read only and returns each row keyed by column
func (s *S) Rows(db, query string, args ...any) []map[string]any {
	s.t.Helper()
	pool, err := sql.Open("sqlite", "file:"+s.Path(db)+"?mode=ro")
	if err != nil {
		s.Failf("opening %s: %v", db, err)
	}
	defer pool.Close()
	rows, err := pool.Query(query, args...)
	if err != nil {
		s.Failf("%s: %s: %v", db, query, err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	out := []map[string]any{}
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			s.Failf("%s: %s: %v", db, query, err)
		}
		row := map[string]any{}
		for i, c := range cols {
			if b, ok := cells[i].([]byte); ok {
				cells[i] = string(b)
			}
			row[c] = cells[i]
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		s.Failf("%s: %s: %v", db, query, err)
	}
	return out
}

// Turns JSON text or any Go value into plain JSON values for comparison
func (s *S) value(v any) any {
	s.t.Helper()
	var raw []byte
	switch x := v.(type) {
	case string:
		raw = []byte(x)
	case []byte:
		raw = x
	default:
		var err error
		if raw, err = json.Marshal(v); err != nil {
			s.Failf("not a JSON value: %v", err)
		}
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		s.Failf("not JSON: %v\n%s", err, clip(string(raw)))
	}
	return out
}

// Expands scenario variables when the wanted document is given as text
func (s *S) expanded(want any) any {
	if text, ok := want.(string); ok {
		return s.Expand(text)
	}
	return want
}

// Requires two JSON documents to hold equal values whatever their key order and spacing
func (s *S) SameJSON(got, want any) {
	s.t.Helper()
	g, w := s.value(got), s.value(s.expanded(want))
	if !reflect.DeepEqual(g, w) {
		gj, _ := json.MarshalIndent(g, "", " ")
		wj, _ := json.MarshalIndent(w, "", " ")
		s.Failf("JSON values differ\n--- got\n%s\n--- want\n%s", clip(string(gj)), clip(string(wj)))
	}
}

// Requires every key and element of want in got with equal values, extra keys of got allowed
func (s *S) HasJSON(got, want any) {
	s.t.Helper()
	g, w := s.value(got), s.value(s.expanded(want))
	if where, ok := covers(g, w, "$"); !ok {
		gj, _ := json.MarshalIndent(g, "", " ")
		wj, _ := json.MarshalIndent(w, "", " ")
		s.Failf("JSON value lacks the wanted content at %s\n--- got\n%s\n--- want\n%s", where, clip(string(gj)), clip(string(wj)))
	}
}

func covers(got, want any, at string) (string, bool) {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return at, false
		}
		for k, wv := range w {
			gv, present := g[k]
			if !present {
				return at + "." + k, false
			}
			if where, ok := covers(gv, wv, at+"."+k); !ok {
				return where, false
			}
		}
		return "", true
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return at, false
		}
		for i := range w {
			if where, ok := covers(g[i], w[i], at+"."+strconv.Itoa(i)); !ok {
				return where, false
			}
		}
		return "", true
	}
	return at, reflect.DeepEqual(got, want)
}

// Walks maps by string key and lists by int index, nil when the path leaves the value
func Dig(v any, path ...any) any {
	for _, step := range path {
		switch key := step.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[key]
		case int:
			l, ok := v.([]any)
			if !ok {
				return nil
			}
			if key < 0 {
				key += len(l)
			}
			if key < 0 || key >= len(l) {
				return nil
			}
			v = l[key]
		default:
			return nil
		}
	}
	return v
}

// Returns the messages of the newest request the scripted model received with tools
func (s *S) LastMessages() []any {
	s.t.Helper()
	bodies := s.Model.MainRequests()
	if len(bodies) == 0 {
		s.Failf("the scripted model received no request with tools")
	}
	msgs, _ := bodies[len(bodies)-1]["messages"].([]any)
	return msgs
}

// Returns what the named tool answered, read from the tool message the product sent back to the model
func (s *S) ToolResult(tool string) string {
	s.t.Helper()
	names := map[string]string{}
	result, found := "", false
	for _, body := range s.Model.MainRequests() {
		msgs, _ := body["messages"].([]any)
		for _, m := range msgs {
			if calls, ok := Dig(m, "tool_calls").([]any); ok {
				for _, c := range calls {
					id, _ := Dig(c, "id").(string)
					names[id], _ = Dig(c, "function", "name").(string)
				}
			}
			if Dig(m, "role") == "tool" {
				if id, _ := Dig(m, "tool_call_id").(string); names[id] == tool {
					found = true
					if text, ok := Dig(m, "content").(string); ok {
						result = text
					} else {
						raw, _ := json.Marshal(Dig(m, "content"))
						result = string(raw)
					}
				}
			}
		}
	}
	if !found {
		s.Failf("the model never received a result of tool %s", tool)
	}
	return result
}

// Ends every process of the scenario, reports an open finding that stopped reproducing and removes the root
func (s *S) finish() {
	for i := len(s.closers) - 1; i >= 0; i-- {
		s.closers[i]()
	}
	for _, p := range s.procs {
		p.end()
	}
	if killed := killTagged(tagEnv+"="+s.tag, 15*time.Second); killed > 0 {
		s.t.Logf("%d process(es) of the scenario ignored the termination request and were killed", killed)
	}
	if s.Model != nil {
		s.Model.Stop()
	}
	if s.t.Failed() {
		s.t.Logf("product logs\n%s", s.logTail())
	}
	if leaked, _ := os.ReadDir(filepath.Join(s.UserHome, ".icebath")); len(leaked) > 0 {
		s.t.Errorf("the product wrote %d entries into ~/.icebath although ICEBATH_HOME names another directory, first %s", len(leaked), leaked[0].Name())
	}
	if s.finding != "" && !s.t.Failed() && !s.t.Skipped() {
		s.t.Errorf("finding %s no longer reproduces, remove its Finding mark and close its entry in SCENARIO_FINDINGS.md", s.finding)
	}
	os.RemoveAll(s.Root)
}

// Returns the last lines of the agent, error and gateway logs for the report of a failed scenario
func (s *S) logTail() string {
	var b strings.Builder
	for _, name := range []string{"agent.log", "errors.log", "gateway.log"} {
		raw, err := os.ReadFile(filepath.Join(s.Home, "logs", name))
		if err != nil {
			continue
		}
		lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
		if len(lines) > 15 {
			lines = lines[len(lines)-15:]
		}
		b.WriteString("== " + name + "\n")
		for _, line := range lines {
			if len(line) > 300 {
				line = line[:300]
			}
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}
