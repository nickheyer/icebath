package pm

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/nickheyer/icebath/pkg/home"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

// Hand off file and update flags of the exact fresh checkout command shipped updaters emit
func LegacyPostSwapInvocation(argv []string) (string, []string, bool) {
	if len(argv) == 0 || argv[0] != "update" {
		return "", nil, false
	}
	marker := slices.Index(argv[1:], "--post-swap")
	if marker < 0 {
		return "", nil, false
	}
	marker++
	if marker+2 != len(argv) {
		return "", nil, false
	}
	return argv[marker+1], argv[1:marker], true
}

// Post swap payload of a hand off file, a ValueError when it holds no JSON object
func readHandoff(path string) (*pyjson.Object, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if err := strictUTF8(data); err != nil {
		return nil, &PyError{Class: "UnicodeDecodeError", Message: err.Error()}
	}
	decoded, err := pyjson.Loads(string(data))
	if err != nil {
		return nil, err
	}
	payload, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil, &PyError{Class: "ValueError", Message: fmt.Sprintf("post-swap hand-off %s is not a JSON object", pypath.PurePathStr(path))}
	}
	return payload, nil
}

// Takeover request of a post swap payload, the older desktop, resume and assume yes names mapped onto the new ones
func takeoverRequest(payload *pyjson.Object, argvTail []string) *OrderedObject {
	request := NewOrderedObject()
	for _, key := range payload.Keys {
		request.Set(key, payload.Vals[key])
	}
	setDefault := func(key string, value any) {
		if _, ok := request.Get(key); !ok {
			request.Set(key, value)
		}
	}
	setDefault("desktop", pyrepr.Truthy(payload.GetOr("had_desktop_app_before_update", false)))
	setDefault("windows_resume", payload.GetOr("windows_gateway_resume", nil))
	setDefault("assume_yes", slices.Contains(argvTail, "--yes"))
	setDefault("restart_update", false)
	return request
}

// Finishes a shipped updater's post swap hand off in a takeover child and returns the child's status
func ContinueLegacyPostSwap(ctx context.Context, handoffPath string, argvTail []string) (int, error) {
	payload, err := readHandoff(handoffPath)
	if err != nil {
		return 0, err
	}
	_ = pypath.Unlink(handoffPath)
	code, _, err := runChild(ctx, takeoverRequest(payload, argvTail))
	return code, err
}

// Env var set on the post swap child, its receipt header reads continued and the lock is the parent's
const PostSwapEnv = "ICEBATH_UPDATE_POST_SWAP"

// True inside the process that finishes an update after the checkout swap
func IsPostSwapChild() bool { return os.Getenv(PostSwapEnv) == "1" }

// Persists the post swap payload beside the receipts of the root home and returns its path
func WriteHandoff(ctx context.Context, payload *pyjson.Object) (string, error) {
	directory := filepath.Join(home.LogsDir(home.RootOf(home.Dir(ctx))), "update_receipts")
	if err := os.MkdirAll(directory, 0o777); err != nil {
		return "", err
	}
	path := filepath.Join(directory, fmt.Sprintf("post_swap_%d.json", os.Getpid()))
	ordered := NewOrderedObject()
	for _, key := range payload.Keys {
		ordered.Set(key, payload.Vals[key])
	}
	encoded, err := dumpsJSON(ordered, true)
	if err != nil {
		return "", &PyError{Class: "TypeError", Message: err.Error()}
	}
	if err := os.WriteFile(path, encoded, 0o666); err != nil {
		return "", err
	}
	return path, nil
}

// Executable the post swap child runs, this binary since no console shim holds it open
func PostSwapPython() (string, error) { return os.Executable() }

// Historical continuation command of the child, the update flags then the hand off file
func PostSwapCommand(handoffPath string, argvTail []string) ([]string, error) {
	exe, err := PostSwapPython()
	if err != nil {
		return nil, err
	}
	cmd := append([]string{exe, "update"}, argvTail...)
	return append(cmd, "--post-swap", handoffPath), nil
}

// Child environment marked post swap and off the shim, the hand off pid claimed only when nobody upstream named one
func PostSwapChildEnv() []string {
	env := append(os.Environ(), PostSwapEnv+"=1", "ICEBATH_UPDATE_REEXEC=1")
	if _, claimed := os.LookupEnv("ICEBATH_UPDATE_HANDOFF_PID"); !claimed {
		env = append(env, fmt.Sprintf("ICEBATH_UPDATE_HANDOFF_PID=%d", os.Getpid()))
	}
	return env
}

// Runs the post swap tail in a takeover child, started false when no child could be started
func ContinueUpdateInFreshInterpreter(ctx context.Context, payload *pyjson.Object, argvTail []string) (code int, started bool, err error) {
	handoffPath, err := WriteHandoff(ctx, payload)
	if err != nil {
		return 0, false, err
	}
	request := takeoverRequest(payload, argvTail)
	cmd, err := PostSwapCommand(handoffPath, argvTail)
	if err != nil {
		return 0, false, err
	}
	out := SyncOutput(ctx)
	fmt.Fprintf(out, "→ Post-swap hand-off: completing the update in a fresh interpreter (%s)\n", handoffPath)
	code, _, err = runChild(ctx, request)
	if err != nil {
		if !pypath.IsOSError(err) {
			return 0, false, err
		}
		fmt.Fprintf(out, "  ⚠ Could not start the post-update interpreter: %s\n", pypath.OSErrorText(err))
		fmt.Fprintln(out, "  The code update is applied. Finish it with:")
		fmt.Fprintf(out, "    %s\n", pysubprocess.List2cmdline(cmd))
		return 0, false, nil
	}
	return code, true, nil
}
