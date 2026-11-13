package pm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Exit status a pm command ends the process with, the error form of a Python SystemExit
type SystemExit struct{ Code int }

func (e *SystemExit) Error() string { return strconv.Itoa(e.Code) }

// Receipt calls the self update slice installs, each nil when the receipt module offers no such call
type UpdateReceiptHooks struct {
	RecordStopReason func(string)
	Current          func() map[string]any
	Clear            func()
}

// Checkout lock calls the self update slice installs, each nil when the lock module offers no such call
type UpdateLockHooks struct {
	BindChild       func(*exec.Cmd) (string, bool)
	ResumeSuspended func(*exec.Cmd) error
	Held            func() bool
	CheckoutLockFDs func(root string) ([]int, error)
}

var updateHooks struct {
	sync.Mutex
	receipt UpdateReceiptHooks
	lock    UpdateLockHooks
}

// Installs the receipt calls a takeover reads, records its stop reason with, and clears after a handled receipt
func SetUpdateReceiptHooks(hooks UpdateReceiptHooks) {
	updateHooks.Lock()
	defer updateHooks.Unlock()
	updateHooks.receipt = hooks
}

// Installs the checkout lock calls a takeover child is bound, resumed and handed lock descriptors through
func SetUpdateLockHooks(hooks UpdateLockHooks) {
	updateHooks.Lock()
	defer updateHooks.Unlock()
	updateHooks.lock = hooks
}

// Currently installed receipt and lock calls
func currentUpdateHooks() (UpdateReceiptHooks, UpdateLockHooks) {
	updateHooks.Lock()
	defer updateHooks.Unlock()
	return updateHooks.receipt, updateHooks.lock
}

// Status of the one takeover this process runs, later stops returning it instead of starting again
var takeoverMemo struct {
	sync.Mutex
	set  bool
	code int
}

// Argv after the executable reaching the hidden update takeover entry of this binary
func updateTakeoverArgs() []string { return []string{"update-takeover"} }

// Argv starting this binary's update takeover, swapped by tests for a re-executed test binary
var takeoverArgv = func() ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return append([]string{exe}, updateTakeoverArgs()...), nil
}

// False always since a compiled binary carries no historical updater frames to hand control from
func InHistoricalUpdate() bool { return false }

// Takeover request fields an updater without historical frames carries, flags from argv and plan from the open receipt
func historicalContext() *OrderedObject {
	hooks, _ := currentUpdateHooks()
	var receipt map[string]any
	if hooks.Current != nil {
		receipt = hooks.Current()
	}
	var plan any
	if receipt != nil {
		plan = receipt["plan"]
	}
	request := NewOrderedObject()
	request.Set("restart_update", false)
	request.Set("desktop", nil)
	request.Set("pre_update_snapshot_id", nil)
	request.Set("pre_update_version", nil)
	request.Set("gateway_mode", containsString(os.Args, "--gateway"))
	request.Set("assume_yes", containsString(os.Args, "--yes"))
	request.Set("windows_resume", nil)
	request.Set("plan", plan)
	if receipt != nil {
		request.Set("receipt", receipt)
	} else {
		request.Set("receipt", nil)
	}
	return request
}

// Child environment without Python and uv variables, carrying the home and the handing off pid
func takeoverEnv(homeValue any) []string {
	var keys []string
	values := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(key)
		}
		if strings.HasPrefix(key, "PYTHON") || strings.HasPrefix(key, "UV_") || key == "VIRTUAL_ENV" {
			continue
		}
		if _, seen := values[key]; !seen {
			keys = append(keys, key)
		}
		values[key] = value
	}
	set := func(key, value string, replace bool) {
		if _, seen := values[key]; !seen {
			keys = append(keys, key)
		} else if !replace {
			return
		}
		values[key] = value
	}
	if text, ok := homeValue.(string); ok && text != "" {
		set("ICEBATH_HOME", text, true)
	}
	set("ICEBATH_UPDATE_HANDOFF_PID", strconv.Itoa(os.Getpid()), false)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env
}

// Runs one takeover child over a request and result file pair, returning its status and decoded acknowledgement
func runChild(ctx context.Context, request *OrderedObject) (int, *pyjson.Object, error) {
	root := RepoRoot()
	var homeValue any
	if value, ok := os.LookupEnv("ICEBATH_HOME"); ok {
		homeValue = value
	}
	if override := home.Override(ctx); override != "" {
		homeValue = override
	}
	request.Set("root", root)
	request.Set("home", homeValue)
	request.Set("argv", append([]string{}, os.Args...))
	env := takeoverEnv(homeValue)
	fmt.Fprintln(SyncOutput(ctx), "-> Handing off to the new updater (package manager) for the rest of this update...")
	directory, err := os.MkdirTemp("", "icebath-update-takeover-")
	if err != nil {
		return 0, nil, err
	}
	defer os.RemoveAll(directory)
	contextPath := filepath.Join(directory, "request.json")
	resultPath := filepath.Join(directory, "result.json")
	encoded, err := dumpsJSON(request, false)
	if err != nil {
		return 0, nil, &PyError{Class: "TypeError", Message: err.Error()}
	}
	if err := os.WriteFile(contextPath, encoded, 0o644); err != nil {
		return 0, nil, err
	}
	argv, err := takeoverArgv()
	if err != nil {
		return 0, nil, err
	}
	argv = append(argv, contextPath, resultPath)
	code, err := runInCustody(ctx, argv, root, env)
	if err != nil {
		return 0, nil, err
	}
	completed := &pyjson.Object{Vals: map[string]any{}}
	if info, statErr := os.Stat(resultPath); statErr == nil && info.Mode().IsRegular() {
		data, err := os.ReadFile(resultPath)
		if err != nil {
			return 0, nil, err
		}
		data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
		if err := strictUTF8(data); err != nil {
			return 0, nil, &PyError{Class: "UnicodeDecodeError", Message: err.Error()}
		}
		decoded, err := pyjson.Loads(string(data))
		if err != nil {
			return 0, nil, err
		}
		object, ok := decoded.(*pyjson.Object)
		if !ok {
			return 0, nil, &PyError{Class: "ValueError", Message: "invalid update takeover acknowledgement"}
		}
		completed = object
	}
	if code < 0 {
		code = 1
	}
	return code, completed, nil
}

// Runs the takeover holding this updater's checkout custody, inside its kill on close job on Windows
func runInCustody(ctx context.Context, argv []string, root string, env []string) (int, error) {
	_, lock := currentUpdateHooks()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = root, env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if runtime.GOOS != "windows" || lock.BindChild == nil || lock.ResumeSuspended == nil || lock.Held == nil || !lock.Held() {
		files, err := checkoutCustody(root, lock)
		if err != nil {
			return 0, err
		}
		cmd.ExtraFiles = files
		err = cmd.Start()
		for _, file := range files {
			if file != nil {
				file.Close()
			}
		}
		if err != nil {
			return 0, err
		}
		return waitCustody(ctx, cmd)
	}
	cmd.SysProcAttr = suspendedProcAttr()
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	if refusal, refused := lock.BindChild(cmd); refused {
		fmt.Fprintf(SyncOutput(ctx), "  ! The update's job would not take the takeover (%s); it runs outside the job, holding its own checkout lease.\n", refusal)
	}
	if err := lock.ResumeSuspended(cmd); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return 0, err
	}
	return waitCustody(ctx, cmd)
}

// Waits for the takeover without bound, killing and reaping it when ctx ends before it exits
func waitCustody(ctx context.Context, cmd *exec.Cmd) (int, error) {
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		return exitStatus(err)
	case <-ctx.Done():
		select {
		case err := <-exited:
			return exitStatus(err)
		default:
		}
		cmd.Process.Kill()
		<-exited
		return 0, ctx.Err()
	}
}

// Python truthiness of a value decoded by pyJSONLoads
func pyJSONTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case int64:
		return t != 0
	case *big.Int:
		return t.Sign() != 0
	case float64:
		return t != 0
	case []any:
		return len(t) > 0
	case *pyjson.Object:
		return len(t.Keys) > 0
	}
	return true
}

// Whether a takeover failure is one of the OSError, ValueError or TypeError classes a stop reports
func takeoverCaught(err error) bool {
	var decode *pyjson.DecodeError
	if errors.As(err, &decode) {
		return true
	}
	switch errorClass(err) {
	case "OSError", "FileNotFoundError", "FileExistsError", "PermissionError", "ValueError", "TypeError", "UnicodeDecodeError":
		return true
	}
	return false
}

// Python str of a caught takeover failure
func takeoverErrorText(err error) string {
	var py *PyError
	if errors.As(err, &py) {
		return py.Message
	}
	return pypath.OSErrorText(err)
}

// Finishes the update in a fresh takeover child once and returns its status as a SystemExit
func stopForRelaunch(ctx context.Context, incomplete bool) error {
	receipt, _ := currentUpdateHooks()
	if incomplete {
		if receipt.RecordStopReason != nil {
			receipt.RecordStopReason("old_version_handoff")
		}
		fmt.Fprintln(cliStderr(ctx), "You're updating from an older version of icebath. To complete this update, run `icebath update` again.")
		return &SystemExit{Code: 1}
	}
	takeoverMemo.Lock()
	if takeoverMemo.set {
		code := takeoverMemo.code
		takeoverMemo.Unlock()
		return &SystemExit{Code: code}
	}
	takeoverMemo.set, takeoverMemo.code = true, 1
	takeoverMemo.Unlock()
	code, completed, err := runChild(ctx, historicalContext())
	if err != nil {
		if !takeoverCaught(err) {
			return err
		}
		code = 1
		fmt.Fprintf(cliStderr(ctx), "Update takeover failed: %s\n", takeoverErrorText(err))
	} else if handled, _ := completed.Get("receipt_handled"); pyJSONTruthy(handled) && receipt.Clear != nil {
		receipt.Clear()
	}
	takeoverMemo.Lock()
	takeoverMemo.code = code
	takeoverMemo.Unlock()
	return &SystemExit{Code: code}
}

// Relaunches the serves a historical atexit token left stopped, clearing pending once the child handled them
func RelaunchStoppedServes(ctx context.Context, token *OrderedObject) error {
	if pending, _ := token.Get("pending"); !pyrepr.Truthy(pending) {
		return nil
	}
	request := NewOrderedObject()
	request.Set("stopped_serves", token)
	code, completed, err := runChild(ctx, request)
	if err != nil {
		if !takeoverCaught(err) {
			return err
		}
		code = 1
		fmt.Fprintf(cliStderr(ctx), "Stopped serve recovery failed: %s\n", takeoverErrorText(err))
	} else if handled, _ := completed.Get("serves_handled"); handled == true {
		token.Set("pending", false)
	}
	if pending, _ := token.Get("pending"); code != 0 || pyrepr.Truthy(pending) {
		fmt.Fprintln(cliStderr(ctx), "Stopped serve recovery did not complete; restart the affected serve/dashboard backends manually.")
	}
	return nil
}

// Hands the update to a fresh takeover child the way stop_for_relaunch() does from tools_config
func StopForRelaunch(ctx context.Context) error { return stopForRelaunch(ctx, false) }
