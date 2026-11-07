package media

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Python ValueError raised for a bad provider configuration
type ttsValueError struct{ msg string }

func (e *ttsValueError) Error() string { return e.msg }

// Python RuntimeError raised for a provider run that failed
type ttsRuntimeError struct {
	msg   string
	cause error
}

func (e *ttsRuntimeError) Error() string { return e.msg }
func (e *ttsRuntimeError) Unwrap() error { return e.cause }

// Command provider outlived its idle timeout, carrying the output read before the kill
type commandTimeoutError struct {
	Command        string
	Timeout        float64
	Stdout, Stderr string
	cause          *commandTimeoutError
}

func (e *commandTimeoutError) Error() string {
	return fmt.Sprintf("Command '%s' timed out after %s seconds", e.Command, pyrepr.Str(e.Timeout))
}

// The timeout the outer one is raised from, as Python chains TimeoutExpired from TimeoutExpired
func (e *commandTimeoutError) Unwrap() error {
	if e.cause == nil {
		return nil
	}
	return e.cause
}

// Command provider exited nonzero
type commandExitError struct {
	Returncode     int
	Command        string
	Stdout, Stderr string
}

func (e *commandExitError) Error() string {
	return fmt.Sprintf("Command '%s' returned non-zero exit status %d.", e.Command, e.Returncode)
}

// Finished command provider run
type commandCompleted struct {
	Command        string
	Returncode     int
	Stdout, Stderr string
}

// Platform name hermes keys its spawn branches on
func hostOSName() string {
	if runtime.GOOS == "windows" {
		return "nt"
	}
	return "posix"
}

// Shell quote character active right before the position, empty outside quotes
func shellQuoteContext(commandTemplate string, position int) string {
	quote := ""
	escaped := false
	for i := 0; i < position && i < len(commandTemplate); i++ {
		char := commandTemplate[i]
		switch {
		case quote == "'":
			if char == '\'' {
				quote = ""
			}
		case quote == `"`:
			if escaped {
				escaped = false
			} else if char == '\\' {
				escaped = true
			} else if char == '"' {
				quote = ""
			}
		case char == '\'' || char == '"':
			quote = string(char)
		case char == '\\':
			i++
		}
	}
	return quote
}

// Template value quoted for its position in a shell command template
func quoteCommandPlaceholder(value, quoteContext string) string {
	switch quoteContext {
	case "'":
		return strings.ReplaceAll(value, "'", `'\''`)
	case `"`:
		return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace(value)
	}
	if hostOSName() == "nt" {
		return pysubprocess.List2cmdline([]string{value})
	}
	return pyshlex.Quote(value)
}

// Template with every single or doubled braced name substituted and quoted for its context, literal doubled braces kept
func renderCommandTemplate(commandTemplate string, placeholders map[string]string) string {
	names := make([]string, 0, len(placeholders))
	for n := range placeholders {
		names = append(names, regexp.QuoteMeta(n))
	}
	sort.Strings(names)
	alt := strings.Join(names, "|")
	pattern, err := pyre.Compile(`(?<!\$)(?:\{\{(?P<double>` + alt + `)\}\}|\{(?P<single>` + alt + `)\})`)
	if err != nil {
		panic(err)
	}
	type replacement struct{ token, value string }
	var replacements []replacement
	var out strings.Builder
	runes := []rune(commandTemplate)
	last := 0
	m, _ := pattern.FindStringMatch(commandTemplate)
	for m != nil {
		name := ""
		for _, grp := range m.Groups()[1:] {
			if len(grp.Captures) > 0 {
				name = grp.String()
				break
			}
		}
		token := fmt.Sprintf("__ICEBATH_CMD_PLACEHOLDER_%d__", len(replacements))
		bytePos := len(string(runes[:m.RuneIndex]))
		replacements = append(replacements, replacement{token, quoteCommandPlaceholder(placeholders[name], shellQuoteContext(commandTemplate, bytePos))})
		out.WriteString(string(runes[last:m.RuneIndex]))
		out.WriteString(token)
		last = m.RuneIndex + m.RuneLength
		m, _ = pattern.FindNextMatch(m)
	}
	out.WriteString(string(runes[last:]))
	rendered := strings.ReplaceAll(strings.ReplaceAll(out.String(), "{{", "{"), "}}", "}")
	for _, r := range replacements {
		rendered = strings.ReplaceAll(rendered, r.token, r.value)
	}
	return rendered
}

// Running command provider child with a reaper so poll and wait never block the output readers
type commandProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func startCommandWaiter(cmd *exec.Cmd) *commandProcess {
	p := &commandProcess{cmd: cmd, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p
}

func (p *commandProcess) poll() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *commandProcess) wait(d time.Duration) bool {
	if d <= 0 {
		return p.poll()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-p.done:
		return true
	case <-t.C:
		return false
	}
}

func (p *commandProcess) returncode() int {
	var ee *exec.ExitError
	if errors.As(p.err, &ee) {
		return pysubprocess.Returncode(ee)
	}
	if p.err != nil {
		return -1
	}
	return 0
}

// Every descendant of pid, gathered before any signal so reparented grandchildren are still found
func processDescendants(pid int) []int {
	var out []int
	queue := []int{pid}
	seen := map[int]bool{pid: true}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range tools.ProcessChildren(cur) {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
				queue = append(queue, c)
			}
		}
	}
	return out
}

func processGone(err error) bool {
	return errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone)
}

// Terminates or kills the child and all its descendants, the child alone when the tree cannot be read
func signalProcessTree(p *commandProcess, method string) {
	send := proc.Terminate
	if method == "kill" {
		send = proc.KillPID
	}
	pid := p.cmd.Process.Pid
	for _, child := range processDescendants(pid) {
		_ = send(child)
	}
	if err := send(pid); err != nil {
		if processGone(err) {
			return
		}
		if method == "kill" {
			_ = p.cmd.Process.Kill()
		} else {
			_ = p.cmd.Process.Signal(syscall.SIGTERM)
		}
	}
}

// Runs taskkill hidden with a five second limit, an error only when it cannot start or overruns
var runTaskkill = func(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	waiter := startCommandWaiter(cmd)
	if !waiter.wait(5 * time.Second) {
		_ = cmd.Process.Kill()
		return errors.New("taskkill timed out")
	}
	return nil
}

// Stops a shell child and every one of its children, terminate first and kill after two seconds
func terminateCommandProcessTree(p *commandProcess) {
	if p.poll() {
		return
	}
	if hostOSName() == "nt" {
		kill := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(p.cmd.Process.Pid))
		hideConsoleWindow(kill)
		if err := runTaskkill(kill); err != nil {
			_ = p.cmd.Process.Kill()
		}
		return
	}
	signalProcessTree(p, "terminate")
	if !p.wait(2 * time.Second) {
		signalProcessTree(p, "kill")
	}
}

// Parent environment variables copied back into the secret scrubbed child environment
func commandEnvPassthrough(cfg map[string]any) []string {
	raw, ok := cfg["env_passthrough"].([]any)
	out := []string{}
	if !ok {
		return out
	}
	for _, x := range raw {
		if s := pystr.Strip(pyrepr.Str(x)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Stderr then stdout of a failed command provider, or a no output marker
func commandFailureDetail(e *commandExitError) string {
	var parts []string
	for _, s := range [][2]string{{"stderr", e.Stderr}, {"stdout", e.Stdout}} {
		if s[1] != "" {
			parts = append(parts, s[0]+": "+pystr.Strip(s[1]))
		}
	}
	if len(parts) == 0 {
		return "no command output"
	}
	return strings.Join(parts, "; ")
}

const (
	windowsCreateNewProcessGroup = 0x00000200
	windowsCreateNoWindow        = 0x08000000
)

// Process group spawn options per platform, the hidden console detach bundle on Windows and nothing on POSIX
func providerPopenGroupKwargs(osName string) map[string]any {
	if osName == "nt" {
		return map[string]any{"creationflags": uint32(windowsCreateNewProcessGroup | windowsCreateNoWindow)}
	}
	return map[string]any{}
}

// Starts the command provider child as a process group that dies with this process
var spawnCommandProvider = func(cmd *exec.Cmd, purpose string) error { return tools.SpawnOwned(cmd, purpose) }

type commandChunk struct {
	stream string
	data   *string
}

// Runs a command provider under an idle timeout reset by output, killing the tree when it stalls
func runCommandProvider(env *tools.Env, command string, timeout float64, envPassthrough []string) (*commandCompleted, error) {
	scrubbed := tools.IcebathSubprocessEnv(env, false, nil)
	for _, key := range envPassthrough {
		var fallback *string
		if v, ok := os.LookupEnv(key); ok {
			fallback = &v
		}
		value, err := tools.ResolvePassthroughValue(env, key, fallback)
		if err != nil {
			return nil, err
		}
		if value != nil {
			scrubbed[key] = *value
		}
	}
	childEnv := tools.DelegatedChildSubprocessEnv(env, scrubbed)
	cmd, err := pysubprocess.ShellCommand(command)
	if err != nil {
		return nil, err
	}
	if flags, ok := providerPopenGroupKwargs(hostOSName())["creationflags"].(uint32); ok {
		applyCreationFlags(cmd, flags)
	}
	keys := make([]string, 0, len(childEnv))
	for k := range childEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	cmd.Env = make([]string, 0, len(keys))
	for _, k := range keys {
		cmd.Env = append(cmd.Env, k+"="+childEnv[k])
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr, cmd.Stdin = outW, errW, nil
	err = spawnCommandProvider(cmd, "tts-command")
	outW.Close()
	errW.Close()
	if err != nil {
		outR.Close()
		errR.Close()
		return nil, err
	}
	p := startCommandWaiter(cmd)
	queue := make(chan commandChunk, 256)
	readerDone := map[string]chan struct{}{"stdout": make(chan struct{}), "stderr": make(chan struct{})}
	read := func(name string, f *os.File) {
		defer close(readerDone[name])
		defer f.Close()
		buf := make([]byte, 65536)
		for {
			n, rerr := f.Read(buf)
			if n > 0 {
				s := pystr.DecodeUTF8Replace(buf[:n])
				queue <- commandChunk{name, &s}
			}
			if rerr != nil {
				break
			}
		}
		queue <- commandChunk{name, nil}
	}
	go read("stdout", outR)
	go read("stderr", errR)
	chunks := map[string]*strings.Builder{"stdout": {}, "stderr": {}}
	open := map[string]bool{"stdout": true, "stderr": true}
	idle := time.Duration(timeout * float64(time.Second))
	deadline := time.Now().Add(idle)
	timedOut := false
	for len(open) > 0 {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			timedOut = true
			break
		}
		t := time.NewTimer(min(50*time.Millisecond, remaining))
		select {
		case c := <-queue:
			t.Stop()
			if c.data == nil {
				delete(open, c.stream)
				continue
			}
			chunks[c.stream].WriteString(*c.data)
			deadline = time.Now().Add(idle)
		case <-t.C:
		}
	}
	if !timedOut && !p.wait(max(0, time.Until(deadline))) {
		timedOut = true
	}
	if timedOut {
		terminateCommandProcessTree(p)
		for _, name := range []string{"stdout", "stderr"} {
			select {
			case <-readerDone[name]:
			case <-time.After(500 * time.Millisecond):
			}
		}
	drain:
		for {
			select {
			case c := <-queue:
				if c.data != nil {
					chunks[c.stream].WriteString(*c.data)
				}
			default:
				break drain
			}
		}
	}
	if p.poll() {
		tools.ReleaseProcessGroupOnceEmpty(cmd.Process.Pid, func() { p.poll() }, time.Second)
	}
	stdout, stderr := chunks["stdout"].String(), chunks["stderr"].String()
	if timedOut {
		return nil, &commandTimeoutError{Command: command, Timeout: timeout, Stdout: stdout, Stderr: stderr,
			cause: &commandTimeoutError{Command: command, Timeout: timeout}}
	}
	if rc := p.returncode(); rc != 0 {
		return nil, &commandExitError{Returncode: rc, Command: command, Stdout: stdout, Stderr: stderr}
	}
	return &commandCompleted{Command: command, Stdout: stdout, Stderr: stderr}, nil
}

// Mapping under the name, empty when the config or the value is not a mapping
func getProviderSection(cfg any, name string) map[string]any {
	m, ok := asStrMap(cfg)
	if !ok {
		return map[string]any{}
	}
	section, ok := asStrMap(m[name])
	if !ok {
		return map[string]any{}
	}
	return section
}

// String keyed mapping view of a decoded config value
func asStrMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case config.Tree:
		return m, true
	}
	return nil, false
}

// Section providers.name, else the legacy section name block for names that are not built in
func namedProviderConfig(cfg any, name string, builtins map[string]bool) map[string]any {
	if section, ok := asStrMap(getProviderSection(cfg, "providers")[name]); ok {
		return section
	}
	if builtins[pystr.Lower(name)] {
		return map[string]any{}
	}
	return getProviderSection(cfg, name)
}

// Whether the config declares a command type provider with a non empty command
func isCommandProviderConfig(cfg any) bool {
	m, ok := asStrMap(cfg)
	if !ok {
		return false
	}
	ptype := ""
	if pyTruthy(m["type"]) {
		ptype = pystr.Lower(pystr.Strip(pyrepr.Str(m["type"])))
	}
	command, isStr := m["command"].(string)
	return (ptype == "" || ptype == "command") && isStr && pystr.Strip(command) != ""
}

// Config of a user declared command provider, nil for reserved, unknown or non command names
func resolveCommandConfig(providerName string, cfg any, reserved map[string]bool) map[string]any {
	key := pystr.Strip(pystr.Lower(providerName))
	if key == "" || reserved[key] {
		return nil
	}
	named := namedProviderConfig(cfg, key, reserved)
	if isCommandProviderConfig(named) {
		return named
	}
	return nil
}

// Timeout seconds from timeout then timeout_seconds, the default for invalid or non positive values
func commandTimeout(cfg map[string]any, def float64) float64 {
	raw, ok := cfg["timeout"]
	if !ok {
		raw, ok = cfg["timeout_seconds"]
		if !ok {
			return def
		}
	}
	value, err := pyint.Float(raw)
	if err != nil {
		return def
	}
	if value > 0 {
		return value
	}
	return def
}

// Validated format or output_format, else the default
func commandOutputFormat(cfg map[string]any, formats map[string]bool, def string) string {
	raw := any(def)
	if pyTruthy(cfg["format"]) {
		raw = cfg["format"]
	} else if pyTruthy(cfg["output_format"]) {
		raw = cfg["output_format"]
	}
	f := pystr.LstripChars(pystr.Strip(pystr.Lower(pyrepr.Str(raw))), ".")
	if formats[f] {
		return f
	}
	return def
}

var builtinTTSProviders = map[string]bool{
	"edge": true, "elevenlabs": true, "openai": true, "minimax": true, "xai": true, "mistral": true, "gemini": true,
	"neutts": true, "kittentts": true, "piper": true, "deepinfra": true,
}

const (
	defaultCommandTTSTimeoutSeconds = 120
	defaultCommandTTSOutputFormat   = "mp3"
	defaultCommandTTSMaxTextLength  = 5000
)

var commandTTSOutputFormats = map[string]bool{"mp3": true, "wav": true, "ogg": true, "flac": true, "m4a": true, "aac": true, "amr": true, "opus": true}

func getNamedTTSProviderConfig(cfg any, name string) map[string]any {
	return namedProviderConfig(cfg, name, builtinTTSProviders)
}

func resolveCommandTTSProviderConfig(providerName string, cfg any) map[string]any {
	return resolveCommandConfig(providerName, cfg, builtinTTSProviders)
}

func getCommandTTSTimeout(cfg map[string]any) float64 {
	return commandTimeout(cfg, defaultCommandTTSTimeoutSeconds)
}

// Name and config of every declared command type provider, sorted by name
func iterCommandTTSProviders(ttsConfig any) [][2]any {
	providers := getProviderSection(ttsConfig, "providers")
	names := make([]string, 0, len(providers))
	for n := range providers {
		names = append(names, n)
	}
	sort.Strings(names)
	var out [][2]any
	for _, n := range names {
		if !builtinTTSProviders[pystr.Lower(n)] && isCommandProviderConfig(providers[n]) {
			out = append(out, [2]any{n, providers[n]})
		}
	}
	return out
}

// Output format from the output path suffix when valid, then format or output_format
func getCommandTTSOutputFormat(cfg map[string]any, outputPath string) string {
	suffix := ""
	if outputPath != "" {
		suffix = pystr.LstripChars(pystr.Strip(pystr.Lower(pypath.Suffix(outputPath))), ".")
	}
	if commandTTSOutputFormats[suffix] {
		return suffix
	}
	return commandOutputFormat(cfg, commandTTSOutputFormats, defaultCommandTTSOutputFormat)
}

// Whether the user explicitly opted the command provider into voice delivery
func isCommandTTSVoiceCompatible(cfg map[string]any) bool {
	return isTruthyValue(cfg["voice_compatible"], false)
}

// Bool coercion with the shared truthy string set for strings and the default for None
func isTruthyValue(v any, def bool) bool {
	if v == nil {
		return def
	}
	if s, ok := v.(string); ok {
		switch pystr.Lower(pystr.Strip(s)) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	}
	return pyTruthy(v)
}

// Path whose extension matches the provider output format
func configuredCommandTTSOutputPath(path string, cfg map[string]any) (string, error) {
	return pypath.WithSuffix(path, "."+getCommandTTSOutputFormat(cfg, ""))
}

// Speech written by a user configured shell command, the audio path it produced
func generateCommandTTS(env *tools.Env, text, outputPath, providerName string, cfg, ttsConfig map[string]any) (string, error) {
	commandTemplate := ""
	if pyTruthy(cfg["command"]) {
		commandTemplate = pystr.Strip(pyrepr.Str(cfg["command"]))
	}
	if commandTemplate == "" {
		return "", &ttsValueError{fmt.Sprintf("tts.providers.%s.command is not configured", providerName)}
	}
	output := pypath.Expanduser(outputPath)
	if err := os.MkdirAll(filepath.Dir(output), 0o777); err != nil {
		return "", err
	}
	if _, err := os.Stat(output); err == nil {
		if err := os.Remove(output); err != nil {
			return "", err
		}
	}
	timeout := getCommandTTSTimeout(cfg)
	tmpdir, err := os.MkdirTemp("", "tmp")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpdir)
	textPath := filepath.Join(tmpdir, "input.txt")
	if err := os.WriteFile(textPath, []byte(text), 0o666); err != nil {
		return "", err
	}
	str := func(m map[string]any, k string, def any) string {
		if v, ok := m[k]; ok {
			return pyrepr.Str(v)
		}
		return pyrepr.Str(def)
	}
	speedDefault := any("")
	if v, ok := ttsConfig["speed"]; ok {
		speedDefault = v
	}
	placeholders := map[string]string{
		"input_path": textPath, "text_path": textPath, "output_path": output,
		"format": getCommandTTSOutputFormat(cfg, output),
		"voice":  str(cfg, "voice", ""), "model": str(cfg, "model", ""), "speed": str(cfg, "speed", speedDefault),
	}
	command := renderCommandTemplate(commandTemplate, placeholders)
	if _, err := runCommandProvider(env, command, timeout, commandEnvPassthrough(cfg)); err != nil {
		var te *commandTimeoutError
		var ee *commandExitError
		switch {
		case errors.As(err, &te):
			return "", &ttsRuntimeError{fmt.Sprintf("TTS provider '%s' timed out after %ss", providerName, strconv.FormatFloat(timeout, 'g', 6, 64)), err}
		case errors.As(err, &ee):
			return "", &ttsRuntimeError{fmt.Sprintf("TTS provider '%s' exited with code %d: %s", providerName, ee.Returncode, commandFailureDetail(ee)), err}
		}
		return "", err
	}
	st, err := os.Stat(output)
	if err != nil || st.Size() <= 0 {
		return "", &ttsRuntimeError{msg: fmt.Sprintf("TTS provider '%s' produced no output at %s", providerName, output)}
	}
	return output, nil
}
