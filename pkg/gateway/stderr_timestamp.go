package gateway

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysys"
)

// Hidden first argument that runs the stderr timestamping wrapper instead of the CLI
const StderrTimestampEntry = "__icebath_stderr_timestamp__"

const (
	launchdLabelEnv            = "ICEBATH_LAUNCHD_LABEL"
	gatewayFatalConfigExitCode = 78
	stdoutDrainTimeout         = 5 * time.Second
	stderrTimestampUsage       = "usage: icebath [-h] --error-log ERROR_LOG ...\n"
)

var (
	timestampPrefixRe                = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2},\d{3}(?:\s|$)`)
	stderrTimestampNow               = time.Now
	stderrTimestampStdout  io.Writer = os.Stdout
	stderrTimestampStderr  io.Writer = os.Stderr
	stderrTimestampEnviron           = os.Environ
	stderrTimestampSignals           = forwardedSignals
	stderrTimestampExit              = os.Exit
	stderrTimestampArgs              = func() []string { return os.Args }
)

// Local time in the logging asctime shape with milliseconds after a comma
func stderrTimestamp() string {
	now := stderrTimestampNow()
	return now.Format("2006-01-02 15:04:05") + fmt.Sprintf(",%03d", now.Nanosecond()/1e6)
}

// Line with a leading timestamp unless it already has one, ending in exactly one newline
func StampLine(line string) string {
	rendered := strings.TrimRight(line, "\r\n")
	prefix := ""
	if !timestampPrefixRe.MatchString(rendered) {
		prefix = stderrTimestamp() + " "
	}
	return prefix + rendered + "\n"
}

// Opens the error log for appending after creating its parent directory
func openStderrLog(logPath string) (*os.File, error) {
	if err := pyfs.Makedirs(filepath.Dir(logPath), 0o777, true); err != nil {
		return nil, err
	}
	return os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
}

// Writes the stamped line and flushes, a Windows text stream ending it in CRLF
func writeTimestampedLine(w io.Writer, line string) error {
	text := StampLine(line)
	if runtime.GOOS == "windows" {
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	if _, err := io.WriteString(w, text); err != nil {
		return streamIOError(err)
	}
	if f, ok := pysys.Attr[interface{ Flush() error }](w); ok {
		return streamIOError(f.Flush())
	}
	return nil
}

// Copies every line of src to dst stamped, decoding invalid UTF-8 bytes as replacement characters
func copyLinesWithTimestamps(src io.Reader, dst io.Writer) error {
	r := bufio.NewReader(src)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if werr := writeTimestampedLine(dst, pystr.DecodeUTF8Replace(line)); werr != nil {
				return werr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return streamIOError(err)
		}
	}
}

// Appends every stderr line of the child to the error log stamped until the pipe ends
func copyStderrWithTimestamps(stderr io.Reader, logPath string) error {
	f, err := openStderrLog(logPath)
	if err != nil {
		return err
	}
	err = copyLinesWithTimestamps(stderr, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Copies every stdout line of the child to the wrapper stdout stamped, keeping its destination
func copyStdoutWithTimestamps(stdout io.Reader, out io.Writer) error {
	return copyLinesWithTimestamps(stdout, out)
}

// Errno error of a stream read or write, which Python raises without a filename
func streamIOError(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// Last traceback line of the Python exception a Go error stands for
func stderrTimestampExceptionLine(err error) string {
	var fe *pyrepr.FormatError
	if errors.As(err, &fe) {
		return fe.Class + ": " + fe.Msg
	}
	return pypath.OSErrorClass(err) + ": " + pypath.OSErrorText(err)
}

// Prints the traceback an uncaught exception leaves on stderr and returns the interpreter exit status
func stderrTimestampUncaught(err error) int {
	fmt.Fprintf(stderrTimestampStderr, "Traceback (most recent call last):\n%s\n", stderrTimestampExceptionLine(err))
	return 1
}

// True for an icebath gateway run argv the wrapper may mark as externally supervised
func isGatewayRunArgv(command []string) bool {
	return LooksLikeGatewayCommandLine(strings.Join(command, " "))
}

// Env carrying this wrapper's icebath launchd job label to the grandchild, empty for other labels
func childLaunchdLabelEnv(environ map[string]string) map[string]string {
	for _, variable := range []string{"XPC_SERVICE_NAME", launchdLabelEnv} {
		if label := pystr.Strip(environ[variable]); strings.HasPrefix(label, "ai.icebath") {
			return map[string]string{launchdLabelEnv: label}
		}
	}
	return map[string]string{}
}

// Argv to exec, adding the external supervisor flag to a launchd wrapped gateway run that lacks it
func prepareChildCommand(command []string, environ map[string]string) []string {
	argv := append([]string(nil), command...)
	xpc := pystr.Strip(environ["XPC_SERVICE_NAME"])
	hasFlag := false
	for _, a := range argv {
		hasFlag = hasFlag || a == externalSupervisorFlag
	}
	if !hasFlag && xpc != "" && xpc != "0" && isGatewayRunArgv(argv) {
		argv = append(argv, externalSupervisorFlag)
	}
	return argv
}

// Exit status reported to launchd, signal deaths as 128 plus N and gateway config exit 78 as 0
func childReturncodeForSupervisor(command []string, returncode int) int {
	if returncode < 0 {
		return 128 - returncode
	}
	if returncode == gatewayFatalConfigExitCode && isGatewayRunArgv(command) {
		return 0
	}
	return returncode
}

// Error log and child command of the wrapper argv, exit status 2 with usage on a bad invocation
func parseStderrTimestampArgs(argv []string) (string, []string, int) {
	fail := func(msg string) (string, []string, int) {
		fmt.Fprint(stderrTimestampStderr, stderrTimestampUsage+"icebath: error: "+msg+"\n")
		return "", nil, 2
	}
	errorLog, have := "", false
	i := 0
	for ; i < len(argv); i++ {
		arg := argv[i]
		if arg == "-h" || arg == "--help" {
			fmt.Fprint(stderrTimestampStdout, stderrTimestampUsage+"\nRun a command, timestamp each stderr line into a log file and each stdout line to stdout.\n\npositional arguments:\n  command\n\noptions:\n  -h, --help            show this help message and exit\n  --error-log ERROR_LOG\n")
			return "", nil, 0
		}
		name, value, inline := strings.Cut(arg, "=")
		if len(name) < 3 || !strings.HasPrefix(name, "--") || !strings.HasPrefix("--error-log", name) {
			break
		}
		if !inline {
			if i+1 >= len(argv) || strings.HasPrefix(argv[i+1], "-") && argv[i+1] != "-" {
				return fail("argument --error-log: expected one argument")
			}
			i++
			value = argv[i]
		}
		errorLog, have = value, true
	}
	if !have {
		return fail("the following arguments are required: --error-log")
	}
	command := argv[i:]
	if len(command) > 0 && command[0] == "--" {
		command = command[1:]
	}
	if len(command) == 0 {
		return fail("missing command after --")
	}
	return errorLog, command, -1
}

// Signals the wrapper forwards to its child
func forwardedSignals() []os.Signal {
	return append([]os.Signal{syscall.SIGTERM, os.Interrupt}, extraForwardedSignals...)
}

// Forwards the wrapper signals to the child until the returned restore runs, a failed forward raised to the main flow
func installSignalForwarders(p *os.Process, raise func(error)) func() {
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, stderrTimestampSignals()...)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				if err := forwardSignal(p, s); err != nil {
					raise(err)
				}
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(sigs)
		close(done)
	}
}

// Runs the child with stamped stdout and stamped stderr appended to the error log, returning the supervisor exit status
func StderrTimestampMain(argv []string) int {
	logPath, command, code := parseStderrTimestampArgs(argv)
	if code >= 0 {
		return code
	}
	environ := map[string]string{}
	base := stderrTimestampEnviron()
	for _, kv := range base {
		if k, v, ok := strings.Cut(kv, "="); ok {
			environ[k] = v
		}
	}
	argvChild := prepareChildCommand(command, environ)
	cmd := exec.Command(argvChild[0], argvChild[1:]...)
	cmd.Env = base
	for k, v := range childLaunchdLabelEnv(environ) {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stderr io.ReadCloser
	stdout, stdoutW, err := os.Pipe()
	if err == nil {
		cmd.Stdout = stdoutW
		stderr, err = cmd.StderrPipe()
		if err == nil {
			err = cmd.Start()
		}
		stdoutW.Close()
		if err != nil {
			stdout.Close()
		}
	}
	if err != nil {
		f, openErr := openStderrLog(logPath)
		if openErr != nil {
			return stderrTimestampUncaught(openErr)
		}
		err = writeTimestampedLine(f, "failed to start stderr-timestamped command: "+startErrorText(argvChild[0], err))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return stderrTimestampUncaught(err)
		}
		return 127
	}
	pumped := make(chan struct{})
	wrapperStdout, wrapperStderr := stderrTimestampStdout, stderrTimestampStderr
	go func() {
		defer close(pumped)
		defer stdout.Close()
		if err := copyStdoutWithTimestamps(stdout, wrapperStdout); err != nil {
			fmt.Fprintf(wrapperStderr, "Exception in thread Thread-1 (_copy_stdout_with_timestamps):\nTraceback (most recent call last):\n%s\n", stderrTimestampExceptionLine(err))
		}
	}()
	raised := make(chan error, 1)
	restore := installSignalForwarders(cmd.Process, func(err error) {
		select {
		case raised <- err:
			stderr.Close()
		default:
		}
	})
	crash := func(err error) int {
		stderr.Close()
		restore()
		return stderrTimestampUncaught(err)
	}
	copyErr := copyStderrWithTimestamps(stderr, logPath)
	select {
	case err := <-raised:
		return crash(err)
	default:
	}
	if copyErr != nil {
		return crash(copyErr)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waited:
	case err := <-raised:
		return crash(err)
	}
	select {
	case <-pumped:
	case <-time.After(stdoutDrainTimeout):
	case err := <-raised:
		return crash(err)
	}
	stderr.Close()
	restore()
	return childReturncodeForSupervisor(command, exitStatusOf(cmd, waitErr))
}

// Python style returncode, negative signal number for a signalled child
func exitStatusOf(cmd *exec.Cmd, waitErr error) int {
	if cmd.ProcessState == nil {
		return 1
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -int(ws.Signal())
	}
	return cmd.ProcessState.ExitCode()
}

// OSError text of a failed spawn the way subprocess reports a missing executable
func startErrorText(program string, err error) string {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return fmt.Sprintf("[Errno 2] No such file or directory: '%s'", program)
	}
	if errors.Is(err, os.ErrPermission) {
		return fmt.Sprintf("[Errno 13] Permission denied: '%s'", program)
	}
	return pythonExceptionText(err)
}

// Runs the wrapper and exits when the process was started with the hidden entry argument
func DispatchStderrTimestampEntry() {
	args := stderrTimestampArgs()
	if len(args) > 1 && args[1] == StderrTimestampEntry {
		stderrTimestampExit(StderrTimestampMain(args[2:]))
	}
}
