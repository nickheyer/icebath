package pm

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Exit status a pm command ends the process with, the error form of a Python SystemExit
type SystemExit struct{ Code int }

func (e *SystemExit) Error() string { return strconv.Itoa(e.Code) }

// Receipt call the self update slice installs, nil when the receipt module offers no such call
type UpdateReceiptHooks struct {
	RecordStopReason func(string)
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

// Installs the receipt call an incomplete handoff records its stop reason with
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

// Runs one takeover child over a request and result file pair, returning its status once its acknowledgement decodes
func runChild(ctx context.Context, request *OrderedObject) (int, error) {
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
		return 0, err
	}
	defer os.RemoveAll(directory)
	contextPath := filepath.Join(directory, "request.json")
	resultPath := filepath.Join(directory, "result.json")
	encoded, err := dumpsJSON(request, false)
	if err != nil {
		return 0, &ClassError{Class: "TypeError", Message: err.Error()}
	}
	if err := os.WriteFile(contextPath, encoded, 0o644); err != nil {
		return 0, err
	}
	argv, err := takeoverArgv()
	if err != nil {
		return 0, err
	}
	argv = append(argv, contextPath, resultPath)
	code, err := runInCustody(ctx, argv, root, env)
	if err != nil {
		return 0, err
	}
	if info, statErr := os.Stat(resultPath); statErr == nil && info.Mode().IsRegular() {
		data, err := os.ReadFile(resultPath)
		if err != nil {
			return 0, err
		}
		data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
		if err := strictUTF8(data); err != nil {
			return 0, &ClassError{Class: "UnicodeDecodeError", Message: "invalid update takeover acknowledgement: " + err.Error()}
		}
		decoded, err := jsonutil.Decode(data)
		if err != nil {
			return 0, &ClassError{Class: "ValueError", Message: "invalid update takeover acknowledgement: " + err.Error()}
		}
		if _, ok := decoded.(*jsonutil.Object); !ok {
			return 0, &ClassError{Class: "ValueError", Message: "invalid update takeover acknowledgement"}
		}
	}
	if code < 0 {
		code = 1
	}
	return code, nil
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
