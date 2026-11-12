package pm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var uvLabels = map[string]string{
	"sync":           "Installing Python dependencies",
	"lock":           "Resolving Python dependencies",
	"pip\x00install": "Installing Python packages",
	"cache":          "Pruning the uv cache",
}

var (
	resolverMarkers   = []string{"no solution found", "conflicting requirements", "conflicting urls", "because only the following versions", "and your pyproject depends on"}
	buildMarkers      = []string{"the build backend returned an error"}
	undatedFile       = []string{"is missing an upload date", "was filtered by `exclude-newer"}
	streamTailMarkers = append(append(append([]string{}, resolverMarkers...), bytecodeCompileMarker), buildMarkers...)
)

const (
	undatedPin            = "has no publish time"
	bytecodeCompileMarker = "failed to bytecode-compile"
	cacheRecoveryRounds   = 3
	streamTailChars       = 2000
	defaultUVTimeout      = 1800 * time.Second
)

// UndatedIndexHint is the remedy when the index omits upload times the release age check needs
const UndatedIndexHint = "the package index did not report upload times, which locking icebath's " +
	"dependencies needs for its 3-day release-age check. Use https://pypi.org/simple " +
	"or a mirror that serves upload times (pip.conf index-url / UV_INDEX_URL), then retry."

// ResolutionConflict is a uv resolver verdict that the requested set has no solution
type ResolutionConflict struct{ *InstallError }

func (e *ResolutionConflict) Unwrap() error { return e.InstallError }

// BuildFailure is a package build backend that ran and failed
type BuildFailure struct{ *InstallError }

func (e *BuildFailure) Unwrap() error { return e.InstallError }

// uvResult is a finished uv child, its exit status and decoded output
type uvResult struct {
	Code   int
	Stdout string
	Stderr string
}

// uvTimeoutError is a uv child that outlived its deadline, carrying the output tail and raw stderr seen so far
type uvTimeoutError struct {
	Argv    []string
	Timeout time.Duration
	Tail    string
	Stderr  []byte
}

func (e *uvTimeoutError) Error() string {
	return fmt.Sprintf("Command '%s' timed out after %s seconds", pyrepr.Repr(e.Argv), pySeconds(e.Timeout))
}

// Seconds of a duration the way Python prints an int or float timeout
func pySeconds(d time.Duration) string {
	if d%time.Second == 0 {
		return strconv.FormatInt(int64(d/time.Second), 10)
	}
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64)
}

// stderr or stdout, whichever Python's or picks
func (r uvResult) diagnostic() string {
	if r.Stderr != "" {
		return r.Stderr
	}
	return r.Stdout
}

// Reports a uv failure in the optional bytecode compile step rather than the install
func isBytecodeCompileFailure(r uvResult) bool {
	if r.Code == 0 {
		return false
	}
	return strings.Contains(pystr.Lower(r.Stderr+"\n"+r.Stdout), bytecodeCompileMarker)
}

// Classified error of a failed uv stage, a resolver conflict, a build failure or a plain install error
func ClassifyUVFailure(stage string, code int, output string) error {
	cause := fmt.Sprintf("uv %s exited %d: %s", stage, code, lastChars(pystr.Strip(output), 600))
	lowered := pystr.Lower(output)
	undated := true
	for _, marker := range undatedFile {
		if !strings.Contains(lowered, marker) {
			undated = false
		}
	}
	switch {
	case strings.Contains(lowered, undatedPin) || undated:
		return NewInstallError("venv", cause, UndatedIndexHint)
	case containsAny(lowered, resolverMarkers):
		return &ResolutionConflict{NewInstallError("venv", cause, "")}
	case containsAny(lowered, buildMarkers):
		return &BuildFailure{NewInstallError("venv", cause, MissingCompilerRemedy(output))}
	}
	return NewInstallError("venv", cause, "")
}

// Project name of a source tree's pyproject
func projectName(source string) (string, error) {
	data, err := os.ReadFile(filepath.Join(source, "pyproject.toml"))
	if err != nil {
		return "", err
	}
	data = trimBOM(data)
	if err := utf8Strict(data); err != nil {
		return "", err
	}
	doc, err := tomlLoads(string(data))
	if err != nil {
		return "", err
	}
	project, err := tomlIndex(doc, "project")
	if err != nil {
		return "", err
	}
	name, err := tomlIndex(project, "name")
	if err != nil {
		return "", err
	}
	s, ok := name.(string)
	if !ok {
		return "", &PyError{Class: "TypeError", Message: fmt.Sprintf("expected str, bytes or os.PathLike object, not %s", tomlTypeName(name))}
	}
	return s, nil
}

// Drops the pth files that must never run inside a shipped payload, keeping every other one
func PruneSitePth(venv string) {
	var sites []string
	if isDirectory(filepath.Join(venv, "Scripts")) {
		sites = []string{filepath.Join(venv, "Lib", "site-packages")}
	} else if lib := filepath.Join(venv, "lib"); isDirectory(lib) {
		sites = globUnder(lib, filepath.Join("python*", "site-packages"))
	}
	for _, site := range sites {
		if !isDirectory(site) {
			continue
		}
		for _, pth := range globUnder(site, "*.pth") {
			name := filepath.Base(pth)
			if name != "_virtualenv.pth" && !strings.HasPrefix(name, "__editable__") {
				continue
			}
			if st, err := os.Lstat(pth); err == nil && !st.IsDir() {
				_ = os.Remove(pth)
			}
		}
	}
}

// Process environment the way Python os environ maps it, keys uppercased on Windows
func pyEnviron() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			if runtime.GOOS == "windows" {
				k = strings.ToUpper(k)
			}
			env[k] = v
		}
	}
	return env
}

// Environment without ambient interpreter, project and cache selection, index and transport settings kept
func BaseEnvironment(env map[string]string) (map[string]string, error) {
	source := env
	if env == nil {
		source = pyEnviron()
	}
	base := map[string]string{}
	for k, v := range source {
		if !strings.HasPrefix(k, "PYTHON") && k != "VIRTUAL_ENV" && (!strings.HasPrefix(k, "UV_") || IsForwarded(k)) {
			base[k] = v
		}
	}
	if env == nil {
		bridged, err := BridgedIndexSettings(pyEnviron(), "")
		if err != nil {
			return nil, err
		}
		for k, v := range bridged {
			base[k] = v
		}
	}
	return base, nil
}

// PythonEnvironment is one uv driven environment, its tools, destination, cache, child env and output policy
type PythonEnvironment struct {
	UV          string
	Python      string
	Destination string
	Cache       string
	Env         map[string]string
	Offline     bool
	Output      io.Writer
	NoConfig    bool
}

// ManagedEnvironmentOptions select the interpreter, cache, env, network and output of a managed environment
type ManagedEnvironmentOptions struct {
	Python    string
	Cache     string
	Env       map[string]string
	Offline   bool
	Explicit  bool
	Output    io.Writer
	NoRealize bool
}

// Environment over PM's pinned toolchain, the pinned interpreter getting compiler defaults this host can run
func ManagedEnvironment(ctx context.Context, destination string, opts ManagedEnvironmentOptions) (*PythonEnvironment, error) {
	uv, pinned, found, err := uvToolchain(ctx, !opts.NoRealize, opts.Explicit)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, NewInstallError("venv", "PM's pinned toolchain is unavailable", "")
	}
	python := pinned
	if opts.Python != "" {
		python = absoluteUnresolved(opts.Python)
	}
	buildEnv, err := BaseEnvironment(opts.Env)
	if err != nil {
		return nil, err
	}
	if resolvePath(python) == resolvePath(pinned) {
		defaults, err := PinnedBuildDefaults(python, buildEnv)
		if err != nil {
			return nil, err
		}
		for k, v := range defaults {
			if _, set := buildEnv[k]; !set {
				buildEnv[k] = v
			}
		}
	}
	cache := opts.Cache
	if cache == "" {
		if cache, err = UvCacheDir(ctx); err != nil {
			return nil, err
		}
	} else {
		cache = absoluteUnresolved(cache)
	}
	return &PythonEnvironment{
		UV: uv, Python: python, Destination: absoluteUnresolved(destination), Cache: cache,
		Env: buildEnv, Offline: opts.Offline, Output: opts.Output,
	}, nil
}

// Builds into a destination this call creates, removing it on any failure and checking it on success
func freshBuild(ctx context.Context, e *PythonEnvironment, sealed bool, lock string, build func() error) error {
	out := e.Destination
	if err := os.MkdirAll(filepath.Dir(out), 0o777); err != nil {
		return err
	}
	if err := os.Mkdir(out, 0o777); err != nil {
		return err
	}
	err := func() error {
		if err := e.Create(ctx); err != nil {
			return err
		}
		if err := build(); err != nil {
			return err
		}
		if err := e.Check(ctx, lock); err != nil {
			return err
		}
		if sealed {
			PruneSitePth(out)
		}
		return nil
	}()
	if err != nil {
		removeTreeQuiet(out)
	}
	return err
}

// Interpreter inside the destination venv
func (e *PythonEnvironment) Executable() string { return VenvPython(e.Destination) }

// Copy of the environment that captures output instead of reporting it
func (e *PythonEnvironment) quiet() *PythonEnvironment {
	c := *e
	c.Output = nil
	return &c
}

// Process seams of the uv engine, replaceable by tests the way Python patches _run, _run_streaming and subprocess run
var (
	uvRunHook       = (*PythonEnvironment).runUV
	uvStreamRunner  = runStreaming
	uvCaptureRunner = runCaptured
)

// Runs one uv command through the replaceable run seam
func (e *PythonEnvironment) run(ctx context.Context, args []string, cwd string, timeout time.Duration) (uvResult, error) {
	return uvRunHook(e, ctx, args, cwd, timeout)
}

// Runs one uv command under the operation's isolation, streaming or capturing per the output policy
func (e *PythonEnvironment) runUV(ctx context.Context, args []string, cwd string, timeout time.Duration) (uvResult, error) {
	env, err := BaseEnvironment(e.Env)
	if err != nil {
		return uvResult{}, err
	}
	if e.NoConfig {
		for k := range env {
			if IsIndexRedirect(k) {
				delete(env, k)
			}
		}
	}
	env["UV_PYTHON"] = Native(e.Python)
	env["UV_PROJECT_ENVIRONMENT"] = Native(e.Destination)
	env["UV_CACHE_DIR"] = Native(e.Cache)
	env["UV_PYTHON_DOWNLOADS"] = "never"
	if _, set := env["CMAKE_POLICY_VERSION_MINIMUM"]; !set {
		env["CMAKE_POLICY_VERSION_MINIMUM"] = "3.5"
	}
	config, err := os.MkdirTemp("", "pm-uv-config-")
	if err != nil {
		return uvResult{}, err
	}
	defer os.RemoveAll(config)
	env["XDG_CONFIG_HOME"] = config
	env["XDG_CONFIG_DIRS"] = config
	command := []string{Native(e.UV)}
	for _, a := range args {
		command = append(command, Native(a))
	}
	if e.NoConfig && !containsString(command, "--no-config") {
		command = append(command, "--no-config")
	}
	if e.Offline {
		command = append(command, "--offline")
	}
	result, err := e.dispatch(ctx, args, command, cwd, env, timeout)
	var timedOut *uvTimeoutError
	if errors.As(err, &timedOut) {
		return uvResult{}, NewInstallError("venv", fmt.Sprintf("uv %s timed out after %ss", args[0], pySeconds(timeout)), TimeoutHint)
	}
	return result, err
}

// Picks verbose streaming, a live status line or a captured run for one uv command
func (e *PythonEnvironment) dispatch(ctx context.Context, args, command []string, cwd string, env map[string]string, timeout time.Duration) (uvResult, error) {
	if e.Output != nil && VerboseOutput(nil) {
		command = append(command, "--verbose")
		if _, set := env["RUST_LOG"]; !set {
			env["RUST_LOG"] = "uv_build_frontend=debug"
		}
		return uvStreamRunner(ctx, command, cwd, env, timeout, func(text string) { writeFlush(e.Output, text) })
	}
	if e.Output != nil {
		var label *string
		key := args[0]
		if len(args) >= 2 {
			if l, ok := uvLabels[args[0]+"\x00"+args[1]]; ok {
				label = &l
			}
		}
		if l, ok := uvLabels[key]; label == nil && ok {
			label = &l
		}
		tail := NewLiveTail(label, e.Output, nil, "  ")
		result, err := uvStreamRunner(ctx, command, cwd, env, timeout, tail.WriteString)
		if err != nil {
			tail.Close(false)
			return result, err
		}
		if containsString(args, "--compile-bytecode") && isBytecodeCompileFailure(result) {
			tail.Discard()
		} else {
			tail.Close(result.Code == 0)
		}
		return result, nil
	}
	return uvCaptureRunner(ctx, command, cwd, env, timeout)
}

// Writes text to a stream and flushes it when the stream buffers
func writeFlush(w io.Writer, text string) {
	io.WriteString(w, text)
	if f, ok := w.(interface{ Flush() error }); ok {
		_ = f.Flush()
	}
}

// Reports whether a string list holds a value
func containsString(list []string, value string) bool {
	for _, s := range list {
		if s == value {
			return true
		}
	}
	return false
}

// newlineDecoder decodes UTF-8 with replacement and translates CRLF and CR to LF across chunk boundaries in reused buffers
type newlineDecoder struct {
	pending   []byte
	pendingCR bool
	joined    []byte
	out       []byte
}

// Decodes one chunk, final flushing any partial character and a held carriage return
func (d *newlineDecoder) decode(data []byte, final bool) string {
	source := data
	if len(d.pending) > 0 {
		d.joined = append(append(d.joined[:0], d.pending...), data...)
		source = d.joined
	}
	out, used := appendUTF8Replace(append(d.out[:0], '\r'), source, final)
	d.pending = append(d.pending[:0], source[used:]...)
	d.out = out
	start := 1
	if d.pendingCR && (len(out) > 1 || final) {
		start = 0
		d.pendingCR = false
	}
	if !final && len(out) > start && out[len(out)-1] == '\r' {
		out = out[:len(out)-1]
		d.pendingCR = true
	}
	w := start
	for r := start; r < len(out); r++ {
		c := out[r]
		if c == '\r' {
			if r+1 < len(out) && out[r+1] == '\n' {
				continue
			}
			c = '\n'
		}
		out[w] = c
		w++
	}
	return string(out[start:w])
}

// streamTail keeps the last characters of a stream in a fixed ring of code points
type streamTail struct {
	ring  []rune
	first int
	size  int
}

// Appends text, evicting the oldest characters past the ring's capacity
func (t *streamTail) push(text string) {
	start, count := len(text), 0
	for start > 0 && count < len(t.ring) {
		_, width := utf8.DecodeLastRuneInString(text[:start])
		start -= width
		count++
	}
	for _, r := range text[start:] {
		if t.size < len(t.ring) {
			t.ring[(t.first+t.size)%len(t.ring)] = r
			t.size++
			continue
		}
		t.ring[t.first] = r
		t.first = (t.first + 1) % len(t.ring)
	}
}

// Retained characters in stream order
func (t *streamTail) String() string {
	var b strings.Builder
	b.Grow(t.size)
	for i := 0; i < t.size; i++ {
		b.WriteRune(t.ring[(t.first+i)%len(t.ring)])
	}
	return b.String()
}

// markerScan matches markers in lowercased text across chunk boundaries one byte at a time without copying it
type markerScan struct {
	markers  []string
	prefix   [][]int
	state    []int
	found    []bool
	alphabet [256]bool
}

// Builds the prefix tables of every marker for an incremental substring search
func newMarkerScan(markers []string) *markerScan {
	s := &markerScan{markers: markers, prefix: make([][]int, len(markers)), state: make([]int, len(markers)), found: make([]bool, len(markers))}
	for i, m := range markers {
		pi := make([]int, len(m)+1)
		k := 0
		for q := 1; q < len(m); q++ {
			for k > 0 && m[k] != m[q] {
				k = pi[k]
			}
			if m[k] == m[q] {
				k++
			}
			pi[q+1] = k
		}
		s.prefix[i] = pi
		for j := 0; j < len(m); j++ {
			s.alphabet[m[j]] = true
		}
	}
	return s
}

// Advances every marker over one lowercased byte, recording the markers it completes
func (s *markerScan) feed(c byte) {
	if !s.alphabet[c] {
		clear(s.state)
		return
	}
	for i, m := range s.markers {
		q := s.state[i]
		for q > 0 && m[q] != c {
			q = s.prefix[i][q]
		}
		if m[q] == c {
			q++
		}
		if q == len(m) {
			s.found[i] = true
			q = s.prefix[i][q]
		}
		s.state[i] = q
	}
}

// First marker in list order occurring in the text lowercased like Python str lower, empty when none does
func (s *markerScan) scan(text string) string {
	var lowered [2 * utf8.UTFMax]byte
	for i := 0; i < len(text); {
		c := text[i]
		if c < utf8.RuneSelf {
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			s.feed(c)
			i++
			continue
		}
		r, width := utf8.DecodeRuneInString(text[i:])
		i += width
		mapped := lowered[:0]
		if r == 'İ' {
			mapped = append(mapped, "i̇"...)
		} else {
			mapped = utf8.AppendRune(mapped, unicode.ToLower(r))
		}
		for _, b := range mapped {
			s.feed(b)
		}
	}
	for i, hit := range s.found {
		if hit {
			return s.markers[i]
		}
	}
	return ""
}

// Starts a child with the given argv, directory and environment
func uvCommand(command []string, cwd string, env map[string]string) *exec.Cmd {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = Native(cwd)
	cmd.Env = EnvList(env)
	return cmd
}

// Waits for a started child up to a deadline, ok false when the deadline passed first
func waitUntil(done <-chan error, deadline time.Time) (error, bool) {
	remaining := time.Until(deadline)
	if remaining < 0 {
		remaining = 0
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case err := <-done:
		return err, true
	case <-timer.C:
		select {
		case err := <-done:
			return err, true
		default:
		}
		return nil, false
	}
}

// Exit status of a waited child as Python reports it
func exitStatus(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return pyReturnCode(exitErr), nil
	}
	return 0, err
}

// Runs a child with combined output streamed live, a bounded diagnostic tail and a wall clock deadline
func runStreaming(ctx context.Context, command []string, cwd string, env map[string]string, timeout time.Duration, write func(string)) (uvResult, error) {
	deadline := time.Now().Add(timeout)
	reader, writer, err := os.Pipe()
	if err != nil {
		return uvResult{}, err
	}
	defer reader.Close()
	cmd := uvCommand(command, cwd, env)
	cmd.Stdout, cmd.Stderr = writer, writer
	if err := cmd.Start(); err != nil {
		writer.Close()
		return uvResult{}, err
	}
	writer.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	abort := func(err error) (uvResult, error) {
		_ = cmd.Process.Kill()
		waitUntil(done, time.Now().Add(5*time.Second))
		return uvResult{}, err
	}
	free := make(chan []byte, 2)
	free <- make([]byte, 65536)
	free <- make([]byte, 65536)
	chunks := make(chan []byte)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		defer close(chunks)
		for {
			var buf []byte
			select {
			case buf = <-free:
			case <-stop:
				return
			}
			n, err := reader.Read(buf)
			if n > 0 {
				select {
				case chunks <- buf[:n]:
				case <-stop:
					return
				}
			} else {
				free <- buf
			}
			if err != nil {
				return
			}
		}
	}()
	var decoder newlineDecoder
	tail := streamTail{ring: make([]rune, streamTailChars)}
	scan := newMarkerScan(streamTailMarkers)
	conflict := ""
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return abort(&uvTimeoutError{Argv: command, Timeout: timeout, Tail: tail.String()})
		}
		timer.Reset(remaining)
		var data []byte
		var open bool
		select {
		case data, open = <-chunks:
		case <-timer.C:
			return abort(&uvTimeoutError{Argv: command, Timeout: timeout, Tail: tail.String()})
		case <-ctx.Done():
			return abort(ctx.Err())
		}
		text := decoder.decode(data, !open)
		if open {
			free <- data[:cap(data)]
		}
		if text != "" {
			if conflict == "" {
				conflict = scan.scan(text)
			}
			tail.push(text)
			write(text)
		}
		if !open {
			break
		}
	}
	waited, ok := waitUntil(done, deadline)
	if !ok {
		return abort(&uvTimeoutError{Argv: command, Timeout: timeout, Tail: tail.String()})
	}
	code, err := exitStatus(waited)
	if err != nil {
		return uvResult{}, err
	}
	retained := tail.String()
	if conflict != "" && !strings.Contains(pystr.Lower(retained), conflict) {
		keep := streamTailChars - len([]rune(conflict)) - 1
		retained = conflict + "\n" + lastChars(retained, max(keep, 0))
	}
	return uvResult{Code: code, Stderr: retained}, nil
}

// Runs a child capturing stdout and stderr as text, killing it at the deadline like subprocess run
func runCaptured(ctx context.Context, command []string, cwd string, env map[string]string, timeout time.Duration) (uvResult, error) {
	cmd := uvCommand(command, cwd, env)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return uvResult{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return uvResult{}, err
	}
	if err := cmd.Start(); err != nil {
		return uvResult{}, err
	}
	deadline := time.Now().Add(timeout)
	outCh, errCh := make(chan []byte, 1), make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(stdout); outCh <- b }()
	go func() { b, _ := io.ReadAll(stderr); errCh <- b }()
	var outData, errData []byte
	gotOut, gotErr := false, false
	for !gotOut || !gotErr {
		timer := time.NewTimer(max(time.Until(deadline), 0))
		select {
		case outData = <-outCh:
			gotOut = true
		case errData = <-errCh:
			gotErr = true
		case <-timer.C:
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			if !gotErr {
				errData = <-errCh
			}
			return uvResult{}, &uvTimeoutError{Argv: command, Timeout: timeout, Stderr: errData}
		case <-ctx.Done():
			timer.Stop()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return uvResult{}, ctx.Err()
		}
		timer.Stop()
	}
	code, err := exitStatus(cmd.Wait())
	if err != nil {
		return uvResult{}, err
	}
	out, _ := decodeUTF8Replace(outData, true)
	errText, _ := decodeUTF8Replace(errData, true)
	return uvResult{Code: code, Stdout: universalNewlines(out), Stderr: universalNewlines(errText)}, nil
}

// Effective timeout of an operation, zero meaning the default half hour
func uvTimeout(timeout time.Duration) time.Duration {
	if timeout == 0 {
		return defaultUVTimeout
	}
	return timeout
}

// Creates the venv at its final destination, which callers must never move
func (e *PythonEnvironment) Create(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(e.Destination), 0o777); err != nil {
		return err
	}
	result, err := e.run(ctx, []string{"venv", "--relocatable", "--no-project", "--no-config", "--python", e.Python, e.Destination},
		filepath.Dir(e.Destination), 120*time.Second)
	if err != nil {
		return err
	}
	if result.Code != 0 {
		return NewInstallError("venv", "uv venv failed: "+lastChars(result.Stderr, 600), "")
	}
	return nil
}

// Resolves a project's lock, optionally upgrading every pin
func (e *PythonEnvironment) Lock(ctx context.Context, source string, upgrade bool, timeout time.Duration) error {
	command := []string{"lock", "--python", e.Python}
	if upgrade {
		command = append(command, "--upgrade")
	}
	result, err := e.run(ctx, command, source, uvTimeout(timeout))
	if err != nil {
		return err
	}
	if result.Code != 0 {
		return ClassifyUVFailure("lock", result.Code, result.diagnostic())
	}
	return nil
}

// Refuses a missing or stale lock without rewriting it
func (e *PythonEnvironment) CheckLock(ctx context.Context, source string) error {
	result, err := e.run(ctx, []string{"lock", "--check", "--python", e.Python}, source, defaultUVTimeout)
	if err != nil {
		return err
	}
	if result.Code != 0 {
		return ClassifyUVFailure("lock", result.Code, result.diagnostic())
	}
	return nil
}

// SyncOptions select what a sync installs, Resolve locking first where Python passes frozen False
type SyncOptions struct {
	Extras           []string
	Groups           []string
	Timeout          time.Duration
	Resolve          bool
	AllExtras        bool
	NoInstallProject bool
	Locked           bool
	NoDefaultGroups  bool
}

// Sorted distinct values of a list
func sortedSet(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// Installs the root and every workspace member, resolving only when asked
func (e *PythonEnvironment) Sync(ctx context.Context, source string, opts SyncOptions) error {
	timeout := uvTimeout(opts.Timeout)
	if opts.Resolve {
		if err := e.Lock(ctx, source, false, timeout); err != nil {
			return err
		}
	}
	mode := "--frozen"
	if opts.Locked {
		mode = "--locked"
	}
	command := []string{"sync", mode, "--all-packages", "--python", e.Python, "--compile-bytecode"}
	if opts.NoDefaultGroups {
		command = append(command, "--no-default-groups")
	}
	if opts.AllExtras {
		command = append(command, "--all-extras")
		optIn, err := optInExtras(source)
		if err != nil {
			return err
		}
		for _, extra := range optIn {
			command = append(command, "--no-extra", extra)
		}
	}
	if opts.NoInstallProject {
		name, err := projectName(source)
		if err != nil {
			return err
		}
		command = append(command, "--no-install-project", "--no-install-package", name)
	}
	for _, extra := range sortedSet(opts.Extras) {
		command = append(command, "--extra", extra)
	}
	for _, group := range sortedSet(opts.Groups) {
		command = append(command, "--group", group)
	}
	result, err := e.withCacheRecovery(ctx, "sync", func() (uvResult, error) { return e.syncAttempt(ctx, command, source, timeout) })
	if err != nil {
		return err
	}
	if result.Code != 0 {
		return ClassifyUVFailure("sync", result.Code, result.diagnostic())
	}
	return nil
}

// One sync, retried once without bytecode compilation when only that optional step failed
func (e *PythonEnvironment) syncAttempt(ctx context.Context, command []string, cwd string, timeout time.Duration) (uvResult, error) {
	result, err := e.run(ctx, command, cwd, timeout)
	if err != nil || !isBytecodeCompileFailure(result) {
		return result, err
	}
	if e.Output != nil {
		io.WriteString(e.Output, "Bytecode compile failed; retrying the install without it. Modules will compile on first import.\n")
	}
	var fallback []string
	for _, arg := range command {
		if arg != "--compile-bytecode" {
			fallback = append(fallback, arg)
		}
	}
	return e.run(ctx, fallback, cwd, timeout)
}

// Sorted names of a set
func sortedNames(set map[string]bool) []string {
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Reports whether every member of a is in b
func setWithin(a, b map[string]bool) bool {
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// Runs an attempt and retries after purging cached packages a corrupt install step names
func (e *PythonEnvironment) withCacheRecovery(ctx context.Context, stage string, attempt func() (uvResult, error)) (uvResult, error) {
	purged := map[string]bool{}
	result, err := attempt()
	if err != nil {
		return result, err
	}
	for round := 0; round < cacheRecoveryRounds; round++ {
		if result.Code == 0 {
			return result, nil
		}
		failed := FailedCachedPackages(result.Stderr + "\n" + result.Stdout)
		if len(failed) == 0 || setWithin(failed, purged) {
			break
		}
		union := map[string]bool{}
		for n := range failed {
			union[n] = true
		}
		for n := range CorruptCachedPackages(e.Cache) {
			union[n] = true
		}
		for n := range purged {
			delete(union, n)
		}
		names := sortedNames(union)
		if e.Output != nil {
			io.WriteString(e.Output, fmt.Sprintf("The cached copy of %s is corrupt; removing it from the uv cache and retrying the install.\n", strings.Join(names, ", ")))
		}
		if err := e.PurgeCachedPackages(ctx, names); err != nil {
			var ie *InstallError
			if errors.As(err, &ie) {
				original := lastChars(pystr.Strip(result.Stderr+"\n"+result.Stdout), 600)
				return result, NewInstallError(ie.Package, fmt.Sprintf("%s; uv %s had failed with: %s", ie.Cause, stage, original), ie.Remedy)
			}
			return result, err
		}
		for _, n := range names {
			purged[n] = true
		}
		if result, err = attempt(); err != nil {
			return result, err
		}
	}
	if result.Code == 0 || len(purged) == 0 {
		return result, nil
	}
	text := result.Stderr + "\n" + result.Stdout
	cache := Native(e.Cache)
	if still := FailedCachedPackages(text); len(still) > 0 {
		remedy := "run `icebath pm doctor --repair`, then retry"
		if setWithin(still, purged) {
			remedy = "the package source is serving a broken wheel; retry later or pin another version"
		}
		return result, NewInstallError("venv", fmt.Sprintf("uv %s exited %d: the cached copy of %s is still invalid after removing %s from the uv cache at %s: %s",
			stage, result.Code, strings.Join(sortedNames(still), ", "), strings.Join(sortedNames(purged), ", "), cache, lastChars(pystr.Strip(text), 600)), remedy)
	}
	if e.Offline {
		return result, NewInstallError("venv", fmt.Sprintf("uv %s exited %d: removed the corrupt cached copy of %s from %s, and offline mode cannot fetch it again: %s",
			stage, result.Code, strings.Join(sortedNames(purged), ", "), cache, lastChars(pystr.Strip(text), 600)), "retry with network access")
	}
	return result, nil
}

// Removes packages from the uv cache through uv, then deletes leftover archives that still fail validation
func (e *PythonEnvironment) PurgeCachedPackages(ctx context.Context, names []string) error {
	cache := Native(e.Cache)
	result, err := e.quiet().run(ctx, append([]string{"cache", "clean"}, names...), filepath.Dir(e.Cache), 900*time.Second)
	if err != nil {
		var ie *InstallError
		if errors.As(err, &ie) {
			return NewInstallError("uv", fmt.Sprintf("could not clean the uv cache at %s: %s", cache, ie.Cause), "stop other icebath installs and retry")
		}
		return err
	}
	if result.Code != 0 {
		return NewInstallError("uv", fmt.Sprintf("could not remove %s from the uv cache at %s: %s", strings.Join(names, ", "), cache, lastChars(result.diagnostic(), 600)), "")
	}
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	return RemoveCorruptEntries(e.Cache, set)
}

// Exports locked runtime requirements to a requirements file
func (e *PythonEnvironment) ExportRequirements(ctx context.Context, source, out string, extras []string, timeout time.Duration) error {
	command := []string{"export", "--frozen", "--python", e.Python, "--no-default-groups", "--no-emit-project", "--no-hashes",
		"--no-annotate", "--no-header", "--format", "requirements-txt", "--output-file", out}
	for _, extra := range sortedSet(extras) {
		command = append(command, "--extra", extra)
	}
	result, err := e.run(ctx, command, source, uvTimeout(timeout))
	if err != nil {
		return err
	}
	if result.Code != 0 {
		return ClassifyUVFailure("export", result.Code, result.diagnostic())
	}
	return nil
}

// Installs requirement lines through a requirements file, a lock's overrides applied
func (e *PythonEnvironment) InstallRequirements(ctx context.Context, requirements []string, wheelhouse, lock string) error {
	if len(requirements) == 0 {
		return nil
	}
	temporary, err := os.MkdirTemp("", "pm-requirements-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	file := filepath.Join(temporary, "requirements.txt")
	if err := os.WriteFile(file, []byte(strings.Join(requirements, "\n")+"\n"), 0o666); err != nil {
		return err
	}
	return e.installRequirementsFile(ctx, file, wheelhouse, lock, defaultUVTimeout)
}

// Installs a requirements file, restoring the lock's manifest overrides the export would otherwise lose
func (e *PythonEnvironment) installRequirementsFile(ctx context.Context, requirements, wheelhouse, lock string, timeout time.Duration) error {
	command := []string{"pip", "install", "--no-config", "--python", e.Executable(), "--requirements", requirements}
	if lock != "" {
		overrides, err := OverrideRequirements(lock)
		if err != nil {
			return err
		}
		if len(overrides) > 0 {
			file := filepath.Join(filepath.Dir(requirements), "overrides.txt")
			if err := os.WriteFile(file, []byte(strings.Join(overrides, "\n")+"\n"), 0o666); err != nil {
				return err
			}
			command = append(command, "--overrides", file)
		}
	}
	if wheelhouse != "" {
		command = append(command, "--no-index", "--only-binary", ":all:", "--find-links", absoluteUnresolved(wheelhouse))
	}
	result, err := e.withCacheRecovery(ctx, "pip", func() (uvResult, error) {
		return e.run(ctx, command, filepath.Dir(requirements), timeout)
	})
	if err != nil {
		return err
	}
	if result.Code != 0 {
		return ClassifyUVFailure("pip", result.Code, result.diagnostic())
	}
	return nil
}

// Installs rebuilt wheels a bundle manifest owns, then checks the result against the lock
func (e *PythonEnvironment) InstallWheelhouse(ctx context.Context, source, wheelhouse string, timeout time.Duration) error {
	timeout = uvTimeout(timeout)
	requirements := filepath.Join(source, "requirements.txt")
	if err := e.ExportRequirements(ctx, source, requirements, nil, timeout); err != nil {
		return err
	}
	lock := filepath.Join(source, "uv.lock")
	if err := e.installRequirementsFile(ctx, requirements, wheelhouse, lock, timeout); err != nil {
		return err
	}
	return e.Check(ctx, lock)
}

// Prunes unused cache entries, CI mode also discarding downloaded wheels
func (e *PythonEnvironment) PruneCache(ctx context.Context, ci bool) error {
	command := []string{"cache", "prune", "--no-config"}
	if ci {
		command = append(command, "--ci", "--force")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	result, err := e.run(ctx, command, cwd, defaultUVTimeout)
	if err != nil {
		return err
	}
	if result.Code != 0 {
		return NewInstallError("uv", "cache pruning failed: "+lastChars(result.Stderr, 600), "")
	}
	return nil
}

// Validates installed dependencies with uv pip check, the lock's deliberate override mismatches excused
func (e *PythonEnvironment) Check(ctx context.Context, lock string) error {
	result, err := e.quiet().run(ctx, []string{"pip", "check", "--no-config", "--color", "never", "--python", e.Executable()},
		filepath.Dir(e.Destination), 60*time.Second)
	if err != nil {
		return err
	}
	if result.Code == 0 {
		return nil
	}
	unexpected, err := UnexpectedIncompatibilities(result.Stdout+"\n"+result.Stderr, lock)
	if err != nil {
		return err
	}
	if unexpected == nil {
		return NewInstallError("venv", "dependency validation failed: "+lastChars(result.Stderr, 600), "")
	}
	if len(unexpected) > 0 {
		return NewInstallError("venv", "dependency validation failed: "+lastChars(strings.Join(unexpected, "\n"), 600), "")
	}
	return nil
}
