package tools

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	filesconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/files/config"
	filespackages "github.com/nickheyer/icebath/internal/plugins/defaults/tools/files/packages"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

var errNotRegular = errors.New("not a regular file")

// Outcome of one command a file operation ran
type run struct {
	output string
	code   int
	// The folder the command was to run in does not exist where commands run
	cwdError string
}

// What one environment told of the commands it has, kept across calls
type commands struct {
	mu    sync.Mutex
	env   terminal.Environment
	found map[string]bool
	rg    string
	// Error the rg of each path gives for an exact modification time order, nil when it can
	ordered map[string]*string
	// Home of the sandbox user, asked until it is known
	homeDir string
}

func (c *commands) home(ask func() string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.homeDir == "" {
		c.homeDir = ask()
	}
	return c.homeDir
}

// The file operations of one call, run in the session's terminal environment: straight on this machine's files when the backend uses
// them and direct access is on, through commands in the environment otherwise
type ops struct {
	r     *registrant
	ctx   context.Context
	call  *tools.Call
	cfg   *config.Config
	lease *terminal.Lease
	env   terminal.Environment
	// The backend's files are this machine's
	host bool
	// Those files are read and written directly
	native bool
	// Folder relative paths resolve against and commands run in
	base  string
	cache *commands
	track *track
	// First failure of the environment, which ends the call
	err error
}

// Runs a command in the folder, the call's folder when empty; the first failure of the environment ends every later one
func (o *ops) exec(command, cwd string, timeout time.Duration) run {
	return o.execStdin(command, cwd, timeout, nil)
}

func (o *ops) execStdin(command, cwd string, timeout time.Duration, stdin *string) run {
	if o.err != nil {
		return run{output: o.err.Error(), code: -1}
	}
	if cwd == "" {
		cwd = o.base
	}
	result, err := o.env.Execute(o.ctx, command, terminal.Exec{Cwd: cwd, Timeout: timeout, Stdin: stdin, Raw: true})
	if err != nil {
		o.err = err
		return run{output: err.Error(), code: -1}
	}
	out := run{output: result.Output, code: result.Code}
	if out.code == 126 && strings.Contains(out.output, "cd: ") {
		hint := ""
		if o.lease.Backend.Container {
			hint = "; for container backends use a path inside the container, e.g. /workspace"
		}
		out.output = fmt.Sprintf("working directory %q does not exist on the active terminal backend (check terminal.cwd or the session cwd%s). %s",
			cwd, hint, strings.TrimSpace(out.output))
		out.cwdError = out.output
	}
	if result.StdinError != "" && out.code == 0 {
		out.code = 1
	}
	return out
}

// Argument quoted for the shell, a Windows path in the form Git Bash takes
func (o *ops) path(p string) string { return quote(bashPath(p)) }

// Word quoted in single quotes for a POSIX shell
func quote(word string) string { return "'" + strings.ReplaceAll(word, "'", `'"'"'`) + "'" }

// Executable path quoted for a native Windows program as forward slashed drive form, plainly quoted elsewhere
func (o *ops) nativeArg(p string) string {
	if windows && p != "" {
		p = strings.ReplaceAll(msysToWindows(p), `\`, "/")
	}
	return quote(p)
}

// Whether a command is on the environment's PATH, rg looked for again until it is found
func (o *ops) has(cmd string) bool {
	if cmd == "rg" {
		return o.rg() != ""
	}
	o.cache.mu.Lock()
	found, ok := o.cache.found[cmd]
	o.cache.mu.Unlock()
	if ok {
		return found
	}
	result := o.exec("command -v "+cmd+" >/dev/null 2>&1 && echo 'yes'", "", 0)
	if result.cwdError != "" || result.code == -1 {
		return false
	}
	found = strings.TrimSpace(result.output) == "yes"
	o.cache.mu.Lock()
	o.cache.found[cmd] = found
	o.cache.mu.Unlock()
	return found
}

// Path of ripgrep where commands run, the well known install folders of this machine tried on Windows, empty when there is none
func (o *ops) rg() string {
	o.cache.mu.Lock()
	cached := o.cache.rg
	o.cache.mu.Unlock()
	if cached != "" {
		return cached
	}
	keep := func(found string) string {
		o.cache.mu.Lock()
		o.cache.rg = found
		o.cache.mu.Unlock()
		return found
	}
	if o.host {
		if found := o.r.managedRg(); found != "" {
			return keep(found)
		}
	}
	if result := o.exec("command -v rg 2>/dev/null", "", 0); result.code == 0 && strings.TrimSpace(result.output) != "" {
		found := strings.Split(strings.TrimSpace(result.output), "\n")[0]
		if found == "yes" {
			found = "rg"
		}
		return keep(found)
	}
	if windows && o.host {
		profile := os.Getenv("USERPROFILE")
		if profile == "" {
			profile, _ = os.UserHomeDir()
		}
		scoop := os.Getenv("SCOOP")
		if scoop == "" {
			scoop = filepath.Join(profile, "scoop")
		}
		candidates := []string{filepath.Join(profile, ".cargo", "bin", "rg.exe"), filepath.Join(scoop, "shims", "rg.exe")}
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			candidates = append(candidates, filepath.Join(local, "Microsoft", "WinGet", "Links", "rg.exe"))
		}
		for _, candidate := range candidates {
			if st, err := os.Stat(candidate); err == nil && st.Mode().IsRegular() {
				return keep(strings.ReplaceAll(candidate, `\`, "/"))
			}
		}
	}
	return ""
}

// Ripgrep the package manager installed, forward slashed for the shell, empty when it has none
func (r *registrant) managedRg() string {
	installed, err := r.packages.Installed(filespackages.Ripgrep, false)
	if err != nil || installed == nil || installed.Binary == "" {
		return ""
	}
	if st, err := os.Stat(installed.Binary); err != nil || !st.Mode().IsRegular() {
		return ""
	}
	return strings.ReplaceAll(installed.Binary, `\`, "/")
}

var bareExecutable = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// Executable quoted for the shell without this machine's path forms reaching a remote host
func (o *ops) executable(p string) string {
	if bareExecutable.MatchString(p) {
		return p
	}
	if o.host {
		return o.nativeArg(p)
	}
	return quote(p)
}

// Path of a relative path against the call's folder, as a direct access opens it
func (o *ops) full(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(o.base, p)
}

// Line of a separator a command's output is split on, random so no file content can forge it
func sentinel(prefix string) string {
	return prefix + strs.Random(16) + "__"
}

// Echoed by a probe for a path that is there but is no regular file, and for one that is not there
const (
	notRegularMark = "__icebath_not_regular__"
	missingMark    = "__icebath_missing__"
)

var (
	osc   = regexp.MustCompile("\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)")
	fence = regexp.MustCompile("'?\x07?__ICEBATH_FENCE_[A-Za-z0-9]+__\x07?'?")
)

// Output without the terminal title sequences and fence markers a shell may leak, lines that were nothing else dropped
func clean(text string) string {
	if text == "" {
		return text
	}
	var b strings.Builder
	for _, line := range strings.SplitAfter(text, "\n") {
		wrapped := strings.Contains(line, "__ICEBATH_FENCE_") || strings.Contains(line, "\x1b]")
		cleaned := strings.ReplaceAll(fence.ReplaceAllString(osc.ReplaceAllString(line, ""), ""), "\x07", "")
		if wrapped && strings.Trim(cleaned, "'\r\n\t ") == "" {
			continue
		}
		b.WriteString(cleaned)
	}
	return b.String()
}

func fields(s string) []string { return strings.FieldsFunc(s, unicode.IsSpace) }

// Whole number of a text, ASCII digits after an optional sign
func integer(s string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	return n, err == nil
}

var base64Text = regexp.MustCompile(`^[A-Za-z0-9+/]+={0,2}$`)

// Bytes of base64 a command printed, whitespace joined; false when it is not clean base64
func unbase64(text string) ([]byte, bool) {
	encoded := strings.Join(fields(clean(text)), "")
	if encoded == "" {
		return []byte{}, true
	}
	if !base64Text.MatchString(encoded) {
		return nil, false
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	return data, err == nil
}

// Segments of the body's output and of each further command between sentinel lines, the body's exit status, and the reply; fenced is
// false when the reply did not come back in its segments
func (o *ops) fenced(body string, more ...string) (segments []string, status int, statusOK bool, reply run, fenced bool) {
	mark := sentinel("__ICEBATH_RB_")
	echo := "echo " + mark
	var rest strings.Builder
	for _, cmd := range more {
		rest.WriteString(echo + "; " + cmd + "; ")
	}
	reply = o.exec("{ set +x; } 2>/dev/null; "+echo+"; "+body+"; __hb=$?; "+rest.String()+echo+"; echo $__hb", "", 0)
	parts := strings.Split(reply.output, mark+"\n")
	if len(parts) != len(more)+3 {
		return nil, 0, false, reply, false
	}
	tail := fields(clean(parts[len(parts)-1]))
	if len(tail) == 0 {
		return parts[1 : len(parts)-1], 0, false, reply, true
	}
	status, statusOK = integer(tail[0])
	return parts[1 : len(parts)-1], status, statusOK, reply, true
}

// Whether the data is as long as the one number the segment holds
func sized(data []byte, segment string) bool {
	tokens := fields(clean(segment))
	if len(tokens) != 1 {
		return false
	}
	n, err := strconv.Atoi(tokens[0])
	return err == nil && n == len(data)
}

// First bytes of a file through base64, false when no clean base64 came back
func (o *ops) sample(path string, n int) ([]byte, bool) {
	segments, status, ok, _, fenced := o.fenced("head -c " + strconv.Itoa(n) + " " + o.path(path) + " 2>/dev/null | base64")
	if !fenced || !ok || status != 0 {
		return nil, false
	}
	return unbase64(segments[0])
}

// Exact bytes of a file: directly on this machine, else base64 through the environment checked against wc, od where base64 is missing;
// the failure as a run when there are none
func (o *ops) exactBytes(path string) ([]byte, *run) {
	if o.native {
		if data, ok := nativeRead(o.full(path)); ok {
			return data, nil
		}
	}
	arg := o.path(path)
	garbled := &run{output: path + ": the backend returned a garbled byte-exact read", code: 1}
	segments, status, ok, reply, fenced := o.fenced("base64 < "+arg, "wc -c < "+arg)
	switch {
	case !fenced && reply.code != 0:
		return nil, &reply
	case !fenced || !ok:
		return nil, garbled
	case status == 127:
		return o.exactBytesOd(path)
	case status != 0:
		return nil, readFailure(path, segments[0], status)
	}
	data, decoded := unbase64(segments[0])
	if !decoded || !sized(data, segments[1]) {
		return nil, garbled
	}
	return data, nil
}

// Exact bytes through od for an environment with no base64
func (o *ops) exactBytesOd(path string) ([]byte, *run) {
	arg := o.path(path)
	unavailable := &run{output: path + ": this backend has neither base64 nor od, so a byte-exact read is unavailable", code: 1}
	segments, status, ok, reply, fenced := o.fenced("od -An -v -tx1 < "+arg, "wc -c < "+arg)
	switch {
	case !fenced && reply.code != 0:
		return nil, &reply
	case !fenced || !ok || status == 127:
		return nil, unavailable
	case status != 0:
		return nil, readFailure(path, segments[0], status)
	}
	data, err := hex.DecodeString(strings.Join(fields(clean(segments[0])), ""))
	if err != nil || !sized(data, segments[1]) {
		return nil, &run{output: path + ": the backend returned a garbled byte-exact read", code: 1}
	}
	return data, nil
}

// What the environment said of a read that failed, else its exit status
func readFailure(path, payload string, status int) *run {
	message := strings.TrimSpace(clean(payload))
	if message == "" {
		message = path + ": exit " + strconv.Itoa(status)
	}
	return &run{output: message, code: status}
}

// Whole regular file read directly, false for anything else
func nativeRead(path string) ([]byte, bool) {
	f, err := openRegular(path)
	if err != nil || f == nil {
		return nil, false
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	return data, err == nil
}

// Size of a regular file and how the probe went: ok, missing, not_regular, bad_size, env_unavailable or the folder error
func (o *ops) probe(path string) (int64, string) {
	if o.native {
		st, err := os.Stat(o.full(path))
		switch {
		case err == nil && st.Mode().IsRegular():
			return st.Size(), "ok"
		case err == nil:
			return 0, "not_regular"
		case os.IsNotExist(err):
			if _, lerr := os.Lstat(o.full(path)); lerr == nil {
				return 0, "not_regular"
			}
			return 0, "missing"
		}
	}
	arg := o.path(path)
	result := o.exec("if [ -f "+arg+" ]; then wc -c < "+arg+" 2>/dev/null; elif [ -e "+arg+" ] || [ -L "+arg+" ]; then echo "+notRegularMark+
		"; else echo "+missingMark+"; fi", "", 0)
	out := strings.TrimSpace(clean(result.output))
	switch {
	case out == missingMark:
		return 0, "missing"
	case out == notRegularMark:
		return 0, "not_regular"
	case result.code != 0:
		if result.cwdError != "" {
			return 0, result.cwdError
		}
		return 0, "env_unavailable"
	}
	n, ok := integer(out)
	if !ok {
		return 0, "bad_size"
	}
	return int64(n), "ok"
}

// Read error of a probe that failed, the folder error as it is
func unavailable(path, status string) *readResult {
	if status != "env_unavailable" {
		return &readResult{Error: status}
	}
	return &readResult{Error: "Terminal environment unavailable: could not stat " + path + " (the sandbox may still be starting or was removed). Retry shortly."}
}

// Error of a path that is there but would block a read
func notRegular(path string) *readResult {
	return &readResult{Error: "Cannot read '" + path + "': not a regular file (directory, dangling symlink, FIFO, socket, or device). Reading it could " +
		"block indefinitely."}
}

// Most characters a line keeps
func (o *ops) lineLimit() int { return max(o.cfg.GetInt(toolsconfig.OutputMaxLineLength), 1) }

// Lines of a page numbered from start, each cut to the line limit
func (o *ops) number(content string, start int) string {
	limit := o.lineLimit()
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(strconv.Itoa(start + i))
		b.WriteByte('|')
		if utf8.RuneCountInString(line) <= limit {
			b.WriteString(line)
		} else {
			b.WriteString(strs.Head(line, limit) + "... [truncated]")
		}
	}
	return b.String()
}

// Whether any line of the page is past the line limit
func (o *ops) longLines(content string) bool {
	limit := o.lineLimit()
	for line := range strings.SplitSeq(content, "\n") {
		if utf8.RuneCountInString(line) > limit {
			return true
		}
	}
	return false
}

// Python the environment runs snippets with: this machine's own on its files, python3 elsewhere
func (o *ops) python() string {
	if o.host {
		if py := hostPython(); py != "" {
			return o.path(py)
		}
	}
	return "python3"
}

var (
	pythonOnce sync.Once
	pythonPath string
)

// First python3 or python on this machine's PATH that is not a Windows Store alias
func hostPython() string {
	pythonOnce.Do(func() {
		for _, name := range []string{"python3", "python"} {
			if p, err := exec.LookPath(name); err == nil && !(windows && strings.Contains(strings.ToLower(p), `\microsoft\windowsapps\`)) {
				pythonPath = p
				return
			}
		}
	})
	return pythonPath
}

// Runs a Python snippet base64 wrapped so it survives every layer of quoting, with python when python3 is missing
func (o *ops) snippet(code string) run {
	encoded := base64.StdEncoding.EncodeToString([]byte(code))
	command := func(py string) string {
		return py + ` -c "import base64; exec(base64.b64decode('` + encoded + `').decode())"`
	}
	result := o.exec(command(o.python()), "", 0)
	if result.code != 0 && strings.Contains(result.output, "python3") {
		result = o.exec(command("python"), "", 0)
	}
	return result
}

// Python string literal of a text, every character past ASCII escaped
func pyString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r >= ' ' && r <= '~':
			b.WriteRune(r)
		case r > 0xffff:
			fmt.Fprintf(&b, `\U%08x`, r)
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Hex sha256 of the bytes
func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Bytes as text, each invalid byte a replacement character
func decode(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}
	return strings.ToValidUTF8(string(data), "�")
}

// Setting read as alpha's switches are: on unless 0, false, no or off
func enabled(cfg *config.Config, key string) bool {
	switch strings.ToLower(strings.TrimSpace(cfg.GetString(key))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// Whether the stale write guard and the cross session file state are off: by the setting, or by the variable that turns them off for a run
func (o *ops) unguarded() bool {
	return !o.cfg.GetBool(filesconfig.StateGuard) || config.Yes(o.cfg.Getenv(filesconfig.NoStateGuardEnv))
}
