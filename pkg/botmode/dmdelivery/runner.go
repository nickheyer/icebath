package dmdelivery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/botmode"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Parsed run delivery argv, the author JSON when given, transport mode, DM file and transport argv
type runnerArgs struct {
	author *string
	mode   string
	dmFile string
	argv   []string
}

// Splits run delivery with an optional author into its parts, false when malformed
func runnerArgv(args []string) (runnerArgs, bool) {
	if len(args) == 0 || args[0] != "--run-delivery" {
		return runnerArgs{}, false
	}
	rest := args[1:]
	var author *string
	if len(rest) > 0 && rest[0] == "--author" {
		if len(rest) < 2 {
			return runnerArgs{}, false
		}
		value := rest[1]
		author, rest = &value, rest[2:]
	}
	if len(rest) < 2 || (rest[0] != "stdin" && rest[0] != "query-file") {
		return runnerArgs{}, false
	}
	return runnerArgs{author: author, mode: rest[0], dmFile: rest[1], argv: rest[2:]}, true
}

// Basename of the transport executable split on both separators, empty for an empty argv
func cliName(argv []string) string {
	first := ""
	if len(argv) > 0 {
		first = argv[0]
	}
	first = first[strings.LastIndex(first, "\\")+1:]
	return first[strings.LastIndex(first, "/")+1:]
}

// Whether argv runs the icebath CLI pinned to a profile with -p
func profileCLIArgv(argv []string) bool {
	cli := cliName(argv)
	return len(argv) >= 3 && (cli == "icebath" || cli == "icebath.exe") && argv[1] == "-p"
}

// Roster home of the profile a local CLI argv pins, empty when argv names none
func localDeliveryHome(argv []string) string {
	if !profileCLIArgv(argv) {
		return ""
	}
	dir := ""
	for _, p := range botmode.Roster(botmode.InstallRoot(home.Process())) {
		if p.Name == argv[2] {
			dir = p.Dir
		}
	}
	return dir
}

// Per profile turn lock for a local teammate delivery, a no op for peer stdin transports
func deliveryLock(ctx context.Context, argv []string, stdinFile bool) (func(), error) {
	if stdinFile || !profileCLIArgv(argv) {
		return func() {}, nil
	}
	return tools.AcquireTurnLock(ctx, botmode.InstallRoot(home.Process()), argv[2], nil)
}

// Admits a local DM only through the profile authority, a peer stdin transport run with the DM file as input
func runDelivery(ctx context.Context, argv []string, dmFile string, stdinFile bool, profileHome string, author *pyjson.Object, stdout, stderr io.Writer) (int, error) {
	if !stdinFile {
		target := profileHome
		if target == "" {
			target = localDeliveryHome(argv)
		}
		if target != "" || osPathExists(LiveIntentFile(dmFile)) {
			record, err := AdmitLiveDM(ctx, target, dmFile, author)
			if err != nil {
				var held *LiveTargetHeldError
				var refusal LiveTargetError
				var text string
				switch {
				case errors.As(err, &held):
					text, err = SettledNoSend(dmFile, held)
				case errors.As(err, &refusal):
					text, err = UnavailablePayload(dmFile, refusal)
				default:
					text, err = LiveOutcomeUnknown(dmFile, excText(err))
				}
				if err != nil {
					return 0, err
				}
				fmt.Fprintln(stdout, text)
				return 1, nil
			}
			if record != nil {
				return waitAdmitted(ctx, stdout, record, dmFile)
			}
		}
		text, err := pyjson.Dumps(pyjson.ObjectOf("reason", "runtime_unavailable", "error", "No canonical Bot Chat authority; payload retained."))
		if err != nil {
			return 0, err
		}
		fmt.Fprintln(stdout, text)
		return 1, nil
	}
	defer UnlinkDMFile(dmFile)
	env, err := provider.DeliveryEnv(ctx, nil, author, "")
	if err != nil {
		return 0, err
	}
	release, err := deliveryLock(ctx, argv, stdinFile)
	if err != nil {
		return 0, err
	}
	defer release()
	stream, err := os.Open(dmFile)
	if err != nil {
		return 0, err
	}
	defer stream.Close()
	raw, err := io.ReadAll(stream)
	if err != nil {
		return 0, err
	}
	text, err := pystr.DecodeUTF8SigFile(raw)
	if err != nil {
		return 0, err
	}
	return runTransport(argv, []byte(pystr.TranslateNewlines(text)), env, stdout, stderr)
}

// Whether a path exists as os.path.exists reports it, any stat error reading as absent
func osPathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Waits on the receipt an admission answered, its profile home and delivery id read off the record
func waitAdmitted(ctx context.Context, stdout io.Writer, record map[string]any, dmFile string) (int, error) {
	homeVal, err := subscript(record, "profile_home")
	if err != nil {
		return 0, err
	}
	idVal, err := subscript(record, "delivery_id")
	if err != nil {
		return 0, err
	}
	id, _ := idVal.(string)
	target, err := pathArg(homeVal)
	if err != nil {
		if idErr := gateway.CheckDeliveryID(id); idErr != nil {
			return 0, idErr
		}
		return 0, err
	}
	return WaitLiveDM(ctx, stdout, target, id, dmFile)
}

// Executable subprocess run finds for argv0, searched on the child env PATH where POSIX execvpe looks
func transportExecutable(name string, env map[string]string) (string, error) {
	if runtime.GOOS == "windows" || strings.Contains(name, "/") {
		return name, nil
	}
	search, ok := env["PATH"]
	if !ok {
		search = "/bin:/usr/bin"
	}
	if found, ok := pyshutil.Which(name, &search); ok {
		return found, nil
	}
	return "", &fs.PathError{Op: "exec", Path: name, Err: syscall.ENOENT}
}

// Runs the transport with input on stdin and the runner's own stdout and stderr, its return code
func runTransport(argv []string, input []byte, env map[string]string, stdout, stderr io.Writer) (int, error) {
	if len(argv) == 0 {
		return 0, &pyError{"IndexError", "list index out of range"}
	}
	path, err := transportExecutable(argv[0], env)
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(path, argv[1:]...)
	cmd.Args[0] = argv[0]
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	sort.Strings(cmd.Env)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(input), stdout, stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return pysubprocess.Returncode(exitErr), nil
	}
	if err != nil {
		return 0, err
	}
	return 0, nil
}

// Seconds since the epoch as time.time reads them
func epochSeconds() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

// Relay reply waiter printing the reply file as the sender's completion notification, exit 1 on failure or budget
func WaitReplyMain(replyPath, label, budgetSeconds string, stdout io.Writer) (int, error) {
	budget, err := pyint.ParseFloat(budgetSeconds)
	if err != nil {
		return 2, nil
	}
	deadline := epochSeconds() + budget
	for epochSeconds() < deadline {
		if _, err := os.Stat(replyPath); err == nil {
			decoded, err := readJSON(replyPath)
			if err != nil {
				return 0, err
			}
			d, ok := pyjson.Dict(decoded)
			if !ok {
				return 0, &pyError{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(decoded))}
			}
			if failure := d["error"]; pyrepr.Truthy(failure) {
				reason := d["reason"]
				if !pyrepr.Truthy(reason) {
					reason = ""
				}
				tag := ""
				if code := pystr.Strip(pyrepr.Str(reason)); code != "" {
					tag = " [reason: " + code + "]"
				}
				fmt.Fprintf(stdout, "Delivery to %s failed%s: %s\n", label, tag, pyrepr.Str(failure))
				return 1, nil
			}
			fmt.Fprintf(stdout, "Reply from %s:\n", label)
			reply := d["reply"]
			if !pyrepr.Truthy(reply) {
				reply = "(empty reply)"
			}
			fmt.Fprintln(stdout, pyrepr.Str(reply))
			return 0, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	fmt.Fprintf(stdout, "No reply from %s within %ss. The message may still be delivered when the Desktop reconnects; do not resend blindly.\n", label, budgetSeconds)
	return 1, nil
}

// Exit status of the runner for the argv the delivery command and the relay waiter build, malformed argv 2
func DeliveryMain(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "--wait-reply" {
		if len(args) != 4 {
			return 2
		}
		code, err := WaitReplyMain(args[1], args[2], args[3], stdout)
		if err != nil {
			fmt.Fprintf(stderr, "Traceback (most recent call last):\n%s: %s\n", pyrepr.ExceptionName(err), excText(err))
			return 1
		}
		return code
	}
	parsed, ok := runnerArgv(args)
	if !ok {
		return 2
	}
	var author *pyjson.Object
	if parsed.author != nil {
		if author = provider.ParseTurnAuthor(*parsed.author); author == nil {
			return 2
		}
	}
	argv, profileHome := parsed.argv, ""
	if len(argv) >= 2 && argv[0] == "--profile-home" {
		profileHome, argv = pypath.PathStr(argv[1]), argv[2:]
	}
	code, err := runDelivery(ctx, argv, parsed.dmFile, parsed.mode == "stdin", profileHome, author, stdout, stderr)
	if err == nil {
		return code
	}
	reasonErr := err
	switch err.(type) {
	case *fs.PathError, *os.LinkError:
		reasonErr = errors.New(excText(err))
	}
	payload, dumpErr := pyjson.Dumps(pyjson.ObjectOf("error", excText(err), "reason", tools.DeliveryFailureReason(reasonErr)))
	if dumpErr != nil {
		fmt.Fprintf(stderr, "Traceback (most recent call last):\n%s: %s\n", pyrepr.ExceptionName(dumpErr), excText(dumpErr))
		return 1
	}
	fmt.Fprintln(stdout, payload)
	return 1
}

// Runs the DM runner and exits when argv carries its entry word, the script run of tools/bot_mode_dm.py
func DispatchEntry(ctx context.Context, exit func(int)) {
	if len(os.Args) < 2 || os.Args[1] != botmode.DMRunnerEntry {
		return
	}
	exit(DeliveryMain(ctx, os.Args[2:], os.Stdout, os.Stderr))
}
