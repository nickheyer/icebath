package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	xterm "github.com/charmbracelet/x/term"
	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyast"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Seconds a network git may run before the updater reports it as a failed run
const networkGitTimeoutSeconds = 300

// Startup files compiled before and after every tree move
var updateCriticalFiles = []string{
	"icebath_bootstrap.py", "icebath_cli/_parser.py", "icebath_cli/_launchers.py",
	"icebath_cli/_early_recovery.py", "icebath_cli/update_lock.py", "icebath_cli/update_custody.py",
	"icebath_cli/update_handoff.py", "icebath_cli/venv_sync.py", "pm/__init__.py", "pm/environments.py",
	"icebath_cli/main.py", "icebath_cli/config.py", "icebath_cli/__init__.py",
	"icebath_cli/web_server.py", "cli.py", "run_agent.py", "model_tools.py", "toolsets.py",
	"icebath_constants.py"}

var (
	updateLog = slog.Default().With("logger", "hermes_cli.update_cmd")
	// Clock and sleep of the gateway prompt poll
	updateMonotonic = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	updateSleep     = time.Sleep
	// Terminal probes of the interactive restore prompt
	updateStdinIsTTY  = func() bool { return fileIsTTY(os.Stdin) }
	updateStdoutIsTTY = func(w io.Writer) bool { return writerIsTTY(w) }
)

// Whether an open file is a terminal the way isatty reports one
func fileIsTTY(f *os.File) bool { return xterm.IsTerminal(f.Fd()) }

// Mirroring update writer that reports whether its original stream is still a terminal
type ttyReporter interface {
	IsATTY() bool
}

// Mirroring update writer exposing the update log it copies every write into
type updateLogMirror interface {
	Log() io.Writer
}

// Whether a writer is a terminal, the original stream of a mirroring update writer included
func writerIsTTY(w io.Writer) bool {
	if f, ok := w.(*os.File); ok {
		return fileIsTTY(f)
	}
	if s, ok := w.(ttyReporter); ok {
		return s.IsATTY()
	}
	return false
}

// Retired entry of an old updater, hands the run to the takeover child without inventing a path
func getDefaultIcebathRoot(ctx context.Context) error { return pm.StopForRelaunch(ctx) }

// Retired entry of an old updater, hands the run to the takeover child without bootstrapping uv
func ensureUvForTermux(ctx context.Context, pipCmd []string) error { return pm.StopForRelaunch(ctx) }

// Retired entry of an old updater, hands the run to the takeover child without bootstrapping pip
func ensureVenvPip(ctx context.Context, pipCmd []string, pythonExe string) error {
	return pm.StopForRelaunch(ctx)
}

// Retired entry of an old updater, hands the run to the takeover child without forming an install
func pipInstallPrefix(ctx context.Context, uvBin string) error { return pm.StopForRelaunch(ctx) }

// Retired entry of an old updater, hands the run to the takeover child and writes no markers
func refuseUpdateForContendedShims(ctx context.Context, exc error) error {
	return pm.StopForRelaunch(ctx)
}

// Failure class no real error ever is, what an old updater's quarantine clause is given to catch
type neverRaised struct{}

func (*neverRaised) Error() string { return "" }

// Matcher of the retired quarantine clause, false for every real failure
func shimQuarantineErrorType() func(error) bool {
	return func(err error) bool {
		var never *neverRaised
		return errors.As(err, &never)
	}
}

// Retired entry of an old updater, hands the run to the takeover child
func writeUpdateIncompleteMarker(ctx context.Context) error { return pm.StopForRelaunch(ctx) }

// Retired entry of an old updater, hands the run to the takeover child
func writeLazyRefreshIncompleteMarker(ctx context.Context) error { return pm.StopForRelaunch(ctx) }

// Retired entry of an old updater, hands the run to the takeover child
func filterNonGatewayConcurrentInstances(ctx context.Context, matches []pidCommand) ([]pidCommand, error) {
	return nil, pm.StopForRelaunch(ctx)
}

// Process id and command line of one concurrent instance match
type pidCommand struct {
	PID     int
	Command string
}

// The updates section of the config, empty when absent or not a mapping
func updatesConfig(ctx context.Context) (map[string]any, error) {
	cfg, err := config.LoadContext(ctx, home.Dir(ctx), nil)
	if err != nil {
		return nil, err
	}
	switch section := cfg["updates"].(type) {
	case map[string]any:
		return section, nil
	case config.Tree:
		return section, nil
	}
	return map[string]any{}, nil
}

// Points git at the CA bundle SSL_CERT_FILE names unless git already has one configured
func mapSSLCertFileForGit(ctx context.Context, gitCmd []string) error {
	bundle := os.Getenv("SSL_CERT_FILE")
	if bundle == "" || os.Getenv("GIT_SSL_CAINFO") != "" {
		return nil
	}
	configured, err := updateGitRun(ctx, gitCmd, []string{"config", "--get", "http.sslCAInfo"}, "", false, false)
	if err != nil {
		return err
	}
	if configured.ReturnCode == 0 && pystr.Strip(configured.Stdout) != "" {
		return nil
	}
	return os.Setenv("GIT_SSL_CAINFO", bundle)
}

// Records one step on the open receipt, never failing the update
func recordUpdateStep(ctx context.Context, step string, ok bool, detail string) {
	defer func() { _ = recover() }()
	updaterecord.RecordStep(ctx, step, ok, detail)
}

// Records one skipped step on the open receipt, never failing the update
func recordUpdateSkip(ctx context.Context, step, reason string) {
	defer func() { _ = recover() }()
	updaterecord.RecordSkip(ctx, step, reason)
}

// Records the backup as a step when a snapshot exists, a skip when disabled, else a failed step
func recordPreUpdateBackupOutcome(ctx context.Context, args map[string]any, snapshotID *string) {
	if snapshotID != nil && *snapshotID != "" {
		recordUpdateStep(ctx, "pre_update_backup", true, "snapshot="+*snapshotID)
		return
	}
	if resolvePreUpdateBackupMode(ctx, args) == "off" {
		reason := "disabled by updates.pre_update_backup (mode: off)"
		if pyrepr.Truthy(args["no_backup"]) {
			reason = "disabled by --no-backup"
		}
		recordUpdateSkip(ctx, "pre_update_backup", reason)
		return
	}
	recordUpdateStep(ctx, "pre_update_backup", false, "no snapshot captured")
}

// Records the snapshot stage as success, skipped when the backup is off, else failed
func recordSnapshotStage(ctx context.Context, args map[string]any, snapshotID *string) {
	taken := snapshotID != nil && *snapshotID != ""
	outcome := "failed"
	switch {
	case taken:
		outcome = "success"
	case resolvePreUpdateBackupMode(ctx, args) == "off":
		outcome = "skipped"
	}
	updaterecord.RecordStage(ctx, "snapshot", outcome, nil)
}

// Spawn keywords of the updater's network git calls, the terminal prompt disabled
func noPromptGitKwargs() ChildOptions {
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=Never")
	return ChildOptions{Stdin: DevNull, Env: env, Attr: tools.WindowsHideAttr()}
}

// The updater's git runner, the function hermes tests replace to script git
var updateGitRunner = runUpdateGit

// Runs git through the updater's runner, the checkout as the working directory when cwd is empty
func updateGitRun(ctx context.Context, gitCmd, args []string, cwd string, check, network bool) (*CompletedProcess, error) {
	return updateGitRunner(ctx, gitCmd, args, cwd, check, network)
}

// Runs git capturing text in the checkout, a network call bounded and promptless, a timeout reported as exit 124
func runUpdateGit(ctx context.Context, gitCmd, args []string, cwd string, check, network bool) (*CompletedProcess, error) {
	opts := ChildOptions{Attr: tools.WindowsHideAttr()}
	if network {
		opts = noPromptGitKwargs()
		opts.Timeout = networkGitTimeoutSeconds * time.Second
	}
	opts.Dir, opts.Capture, opts.Text, opts.Check = cwd, true, true, check
	if cwd == "" {
		opts.Dir = fleetProjectRoot()
	}
	result, err := RunGit(ctx, gitCmd, args, opts)
	var timeout *updaterecord.GitTimeout
	if errors.As(err, &timeout) {
		stderr := fmt.Sprintf("git %s timed out after %ds (a stalled remote, or a transfer too large for the limit)", args[0], networkGitTimeoutSeconds)
		if check {
			return nil, &CalledProcessError{ReturnCode: 124, Cmd: timeout.Args, Stderr: stderr}
		}
		return &CompletedProcess{Args: timeout.Args, ReturnCode: 124, Stderr: stderr}, nil
	}
	return result, err
}

// Whether a failure is an OSError or a subprocess failure, the pair the updater's git guards catch
func isOSOrSubprocessError(err error) bool {
	return err != nil && (isUpdateOSError(err) || isSubprocessError(err))
}

// Captured stderr of a failed or timed out subprocess, empty for any other failure
func subprocessStderr(err error) string {
	if called, ok := asCalledProcessError(err); ok {
		return called.Stderr
	}
	var expired *pysubprocess.TimeoutExpired
	if errors.As(err, &expired) {
		return pystr.DecodeUTF8Replace(expired.Stderr)
	}
	return ""
}

// Fetches the history a shallow checkout lacks before the bounded fetch, a failure only warned
func healStaleShallowCheckout(ctx context.Context, w io.Writer, repoRoot, branch string) error {
	kw := noPromptGitKwargs()
	healed, err := tools.HealShallowHistory(ctx, repoRoot, branch, pysubprocess.Options{Env: kw.Env, Attr: kw.Attr})
	if err != nil {
		if !isOSOrSubprocessError(err) {
			return err
		}
		text := subprocessStderr(err)
		if text == "" {
			text = launchErrorText(err)
		}
		detail := launchErrorClass(err)
		if lines := pystr.Splitlines(pystr.Strip(text), false); len(lines) > 0 {
			detail = lines[len(lines)-1]
		}
		fmt.Fprintf(w, "  ⚠ Could not fetch the missing commit history (%s); a very stale install may time out\n", detail)
		return nil
	}
	if healed {
		fmt.Fprintln(w, "  ✓ Fetched the commit history this shallow checkout was missing")
	}
	return nil
}

// Traceback lines Python prints for a SyntaxError alone, the file line, source, carets and message
func formatSyntaxErrorOnly(se *pyast.SyntaxError, filename string) string {
	var b strings.Builder
	suffix := ""
	if se.HasLineno {
		name := filename
		if name == "" {
			name = "<string>"
		}
		fmt.Fprintf(&b, "  File \"%s\", line %d\n", name, se.Lineno)
	} else if filename != "" {
		suffix = " (" + filename + ")"
	}
	if se.HasText {
		textLen := utf8.RuneCountInString(se.Text)
		rtext := strings.TrimRight(se.Text, "\n")
		ltext := []rune(strings.TrimLeft(rtext, " \n\f"))
		rlen := utf8.RuneCountInString(rtext)
		spaces := rlen - len(ltext)
		offset, endOffset := se.Offset, rlen+1
		if se.HasEnd && se.Lineno == se.EndLineno {
			endOffset = offset
			if se.EndOffset != 0 {
				endOffset = se.EndOffset
			}
		}
		if textLen > 0 && offset > textLen {
			offset = textLen + 1
		}
		if textLen > 0 && endOffset > textLen {
			endOffset = textLen + 1
		}
		if offset >= endOffset || endOffset < 0 {
			endOffset = offset + 1
		}
		colno, endColno := offset-1-spaces, endOffset-1-spaces
		fmt.Fprintf(&b, "    %s\n", string(ltext))
		if colno >= 0 {
			b.WriteString("    ")
			for i := 0; i < colno && i < len(ltext); i++ {
				if unicode.IsSpace(ltext[i]) {
					b.WriteRune(ltext[i])
				} else {
					b.WriteByte(' ')
				}
			}
			b.WriteString(strings.Repeat("^", max(endColno-colno, 0)))
			b.WriteByte('\n')
		}
	}
	msg := se.Msg
	if msg == "" {
		msg = "<no detail available>"
	}
	fmt.Fprintf(&b, "SyntaxError: %s%s\n", msg, suffix)
	return b.String()
}

// Message py_compile raises for a source that does not compile
func pyCompileErrorMessage(err error, file string) string {
	var se *pyast.SyntaxError
	if errors.As(err, &se) {
		if se.Class == "SyntaxError" || se.Class == "" {
			return formatSyntaxErrorOnly(se, file)
		}
		named := *se
		named.Filename = filepath.Base(file)
		return fmt.Sprintf("Sorry: %s: %s", se.Class, named.Error())
	}
	var limit *pyast.LimitError
	if errors.As(err, &limit) {
		return fmt.Sprintf("Sorry: %s: %s", limit.Class, limit.Msg)
	}
	return fmt.Sprintf("Sorry: %s: %s", pm.ErrorClass(err), err.Error())
}

// Compiles each existing file under root, reporting the first that fails with its path and error
func validatePythonFilesSyntax(root string, relpaths []string) (bool, string, string) {
	for _, relpath := range relpaths {
		file := filepath.Join(root, filepath.FromSlash(relpath))
		if !existsFollow(file) {
			continue
		}
		source, err := os.ReadFile(file)
		if err != nil {
			return false, file, "could not read: " + launchErrorText(err)
		}
		if err := pyast.Compile(source, file); err != nil {
			return false, file, pyCompileErrorMessage(err, file)
		}
	}
	return true, "", ""
}

// Compiles every startup file under root to catch a pull that would brick the launcher
func validateCriticalFilesSyntax(root string) (bool, string, string) {
	return validatePythonFilesSyntax(root, updateCriticalFiles)
}

// The gateway prompt the config migration and stash restore ask through, replaced by hermes tests
var updateGatewayPrompt = gatewayPrompt

// Asks the gateway to relay a prompt through marker files and polls for the answer, the default on timeout
func gatewayPrompt(ctx context.Context, w io.Writer, promptText, def string, timeout float64) (string, error) {
	if gatewaysPaused() {
		shown := def
		if shown == "" {
			shown = "(empty)"
		}
		fmt.Fprintf(w, "  → %s → %s (gateways are paused for the update)\n", promptText, shown)
		return def, nil
	}
	h := home.Dir(ctx)
	promptPath := filepath.Join(h, ".update_prompt.json")
	responsePath := filepath.Join(h, ".update_response")
	if err := unlinkMissingOK(responsePath); err != nil {
		return "", err
	}
	payload, err := pyjson.Dumps(pyjson.ObjectOf("prompt", promptText, "default", def, "id", uuid.NewString()))
	if err != nil {
		return "", err
	}
	tmp := filepath.Join(h, ".update_prompt.tmp")
	if err := os.WriteFile(tmp, []byte(payload), 0o666); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, promptPath); err != nil {
		return "", err
	}
	deadline := updateMonotonic() + timeout
	for updateMonotonic() < deadline {
		if existsFollow(responsePath) {
			if answer, ok := readGatewayResponse(responsePath, promptPath); ok {
				if answer == "" {
					return def, nil
				}
				return answer, nil
			}
		}
		updateSleep(500 * time.Millisecond)
	}
	if err := unlinkMissingOK(promptPath); err != nil {
		return "", err
	}
	if err := unlinkMissingOK(responsePath); err != nil {
		return "", err
	}
	fmt.Fprintf(w, "  (no response after %ds, using default: %s)\n", int(timeout), pyrepr.Repr(def))
	return def, nil
}

// Reads and consumes the gateway's answer file, false when it cannot be read or removed yet
func readGatewayResponse(responsePath, promptPath string) (string, bool) {
	raw, err := os.ReadFile(responsePath)
	if err != nil {
		return "", false
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return "", false
	}
	if unlinkMissingOK(responsePath) != nil || unlinkMissingOK(promptPath) != nil {
		return "", false
	}
	return pystr.Strip(text), true
}

// Removes a file, a missing one counting as removed
func unlinkMissingOK(file string) error {
	if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Argv, status and captured streams of a failed checked subprocess of any runner
type calledProcess struct {
	Cmd            []string
	ReturnCode     int
	Stdout, Stderr string
}

// Reads a checked subprocess failure of the custody, pm or plain runner as one shape
func asCalledProcessError(err error) (*calledProcess, bool) {
	var custody *CalledProcessError
	if errors.As(err, &custody) {
		return &calledProcess{Cmd: custody.Cmd, ReturnCode: custody.ReturnCode, Stdout: custody.Output, Stderr: custody.Stderr}, true
	}
	var plain *pysubprocess.CalledProcessError
	if errors.As(err, &plain) {
		return &calledProcess{Cmd: plain.Cmd, ReturnCode: plain.Returncode, Stdout: pystr.DecodeUTF8Replace(plain.Stdout),
			Stderr: pystr.DecodeUTF8Replace(plain.Stderr)}, true
	}
	var worker *pm.CalledProcessError
	if errors.As(err, &worker) {
		return &calledProcess{Cmd: worker.Argv, ReturnCode: worker.Code, Stdout: worker.Output}, true
	}
	return nil, false
}

// Argv tokens of the command a failed subprocess ran
func calledProcessErrorCmdParts(exc error) []string {
	called, ok := asCalledProcessError(exc)
	if !ok || called.Cmd == nil {
		return []string{}
	}
	return append([]string{}, called.Cmd...)
}

// Whether the failed subprocess was git itself
func calledProcessErrorIsGit(exc error) bool {
	parts := calledProcessErrorCmdParts(exc)
	if len(parts) == 0 {
		return false
	}
	name := strings.ToLower(path.Base(strings.ReplaceAll(parts[0], "\\", "/")))
	return name == "git" || name == "git.exe"
}

// Whether the failed subprocess was a uv, pip or ensurepip install
func calledProcessErrorIsPythonDepInstall(exc error) bool {
	parts := calledProcessErrorCmdParts(exc)
	if len(parts) == 0 {
		return false
	}
	has := map[string]bool{}
	for i, part := range parts {
		parts[i] = strings.ToLower(part)
		has[parts[i]] = true
	}
	exe := pyBasename(strings.ReplaceAll(parts[0], "\\", "/"))
	if has["ensurepip"] {
		return true
	}
	installers := map[string]bool{"pip": true, "pip.exe": true, "pip3": true, "pip3.exe": true, "uv": true, "uv.exe": true}
	return has["install"] && (has["pip"] || installers[exe])
}

// Final component of a slash separated path, empty after a trailing slash
func pyBasename(p string) string {
	return p[strings.LastIndex(p, "/")+1:]
}

// Names the update stage whose subprocess failed
func formatUpdateFailureStage(exc error) string {
	if calledProcessErrorIsPythonDepInstall(exc) {
		return "Python dependency install failed"
	}
	if calledProcessErrorIsGit(exc) {
		return "Git update failed"
	}
	return "Update step failed"
}

// Whether a failure is a Windows git breakage the ZIP download can repair on an untouched checkout
func shouldZipFallbackOnUpdateError(ctx context.Context, exc error) (bool, error) {
	if _, called := asCalledProcessError(exc); !called || !installRepairIsWindows() || !calledProcessErrorIsGit(exc) {
		return false, nil
	}
	return runCheckoutUntouched(ctx, fleetProjectRoot())
}

// Prints the last lines of the output a failed subprocess captured
func printCalledProcessErrorTail(w io.Writer, exc error, limit int) {
	called, ok := asCalledProcessError(exc)
	if !ok {
		return
	}
	blob := called.Stderr
	if blob == "" {
		blob = called.Stdout
	}
	var lines []string
	for _, line := range pystr.Splitlines(blob, false) {
		if pystr.Strip(line) != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return
	}
	fmt.Fprintln(w, "  Last output:")
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	for _, line := range lines {
		fmt.Fprintf(w, "    %s\n", line)
	}
}

// Appends text to the update log only, a failed write swallowed and only an interrupt raised by the log returned
func logOnlyWrite(w io.Writer, text string) error {
	if text == "" {
		return nil
	}
	if stream, ok := w.(updateLogMirror); ok && stream.Log() != nil {
		log := stream.Log()
		if _, err := io.WriteString(log, text); err != nil {
			if isKeyboardInterrupt(err) {
				return err
			}
			return nil
		}
		if f, ok := log.(interface{ Sync() error }); ok {
			_ = f.Sync()
		}
		return nil
	}
	logPath := filepath.Join(home.Root(), "logs", "update.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o777); err != nil {
		return nil
	}
	fallback, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return nil
	}
	defer fallback.Close()
	_, _ = io.WriteString(fallback, text)
	return nil
}

// Decodes chunks as UTF-8 with replacement and universal newlines across chunk boundaries
type newlineDecoder struct {
	pending   []byte
	pendingCR bool
}

// Text of one chunk, an incomplete rune or a trailing carriage return held until the next call
func (d *newlineDecoder) decode(chunk []byte, final bool) string {
	data := append(d.pending, chunk...)
	d.pending = nil
	if !final {
		for cut := len(data); cut > 0 && len(data)-cut < utf8.UTFMax; cut-- {
			if utf8.RuneStart(data[cut-1]) {
				if !utf8.FullRune(data[cut-1:]) {
					d.pending = append([]byte{}, data[cut-1:]...)
					data = data[:cut-1]
				}
				break
			}
		}
	}
	text := pystr.DecodeUTF8Replace(data)
	if d.pendingCR {
		text = "\r" + text
		d.pendingCR = false
	}
	if !final && strings.HasSuffix(text, "\r") {
		text = text[:len(text)-1]
		d.pendingCR = true
	}
	return pystr.TranslateNewlines(text)
}

// Runs a build step with its combined output streamed to the update log and kept for failure reports
func runLoggedSubprocess(ctx context.Context, w io.Writer, cmd []string, cwd string, env []string) (*CompletedProcess, error) {
	if env == nil {
		env = os.Environ()
	}
	unbuffered := false
	for _, entry := range env {
		if strings.HasPrefix(entry, "PYTHONUNBUFFERED=") {
			unbuffered = true
		}
	}
	if !unbuffered {
		env = append(append([]string{}, env...), "PYTHONUNBUFFERED=1")
	}
	child := exec.Command(cmd[0], cmd[1:]...)
	child.Dir, child.Env = cwd, env
	if runtime.GOOS == "windows" {
		child.SysProcAttr = tools.WindowsHideAttr()
	} else {
		proc.Group(child)
	}
	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer pipeR.Close()
	child.Stdout, child.Stderr = pipeW, pipeW
	if err := child.Start(); err != nil {
		pipeW.Close()
		return nil, err
	}
	pipeW.Close()
	stop := context.AfterFunc(ctx, func() { proc.KillTree(child.Process.Pid) })
	defer stop()
	abandon := func() {
		proc.KillTree(child.Process.Pid)
		done := make(chan struct{})
		go func() { _ = child.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	var decoder newlineDecoder
	var output strings.Builder
	buf := make([]byte, 8192)
	for {
		n, readErr := pipeR.Read(buf)
		text := decoder.decode(buf[:n], n == 0)
		output.WriteString(text)
		if err := logOnlyWrite(w, text); err != nil {
			abandon()
			return nil, err
		}
		if n == 0 {
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				abandon()
				return nil, readErr
			}
			break
		}
	}
	if ctx.Err() != nil {
		abandon()
		return nil, ctx.Err()
	}
	code := 0
	if err := child.Wait(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return nil, err
		}
		code = pysubprocess.Returncode(exit)
	}
	return &CompletedProcess{Args: cmd, ReturnCode: code, Stdout: output.String()}, nil
}

// Whether a failure is an exit or interrupt, which the swallowing guards of bookkeeping steps let through
func isUpdateExit(err error) bool {
	var exit *pm.SystemExit
	return errors.As(err, &exit) || errors.Is(err, cliui.ErrInterrupt) || errors.Is(err, context.Canceled)
}
