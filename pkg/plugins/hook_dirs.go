package plugins

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Argument vector of the command line a hook directory names, a program that sits inside the directory resolved to its full path
func ResolveDirectoryHookCommand(hookDir, command string) ([]string, error) {
	argv, err := pyshlex.SplitCommandLine(pypath.Expanduser(command))
	if err != nil || len(argv) == 0 {
		return nil, fmt.Errorf("command %s cannot be parsed", errchain.Show(command))
	}
	if !filepath.IsAbs(argv[0]) {
		if local := filepath.Join(hookDir, filepath.FromSlash(argv[0])); isFile(local) {
			argv[0] = local
		}
	}
	return argv, nil
}

// Seconds the command of a hook directory may run, the shell hook default for an unset or unusable value and its cap above that
func DirectoryHookTimeout(raw any) int {
	seconds := 0
	switch v := raw.(type) {
	case int:
		seconds = v
	case int64:
		seconds = int(v)
	case float64:
		seconds = int(v)
	}
	if seconds <= 0 {
		return ShellHookDefaultTimeoutSeconds
	}
	return min(seconds, ShellHookMaxTimeoutSeconds)
}

// Runs the command of a hook directory inside that directory for one gateway event with the event name and its context as JSON on standard input and answers the JSON object it printed, nil when it printed none
func RunDirectoryHook(ctx context.Context, eventType, hookDir string, argv []string, timeout int, hookContext map[string]any) (any, error) {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}
	if hookContext == nil {
		hookContext = map[string]any{}
	}
	payload, err := pyjson.Dumps(pyjson.ObjectOf("hook_event_name", eventType, "context", hookContext, "cwd", cwd,
		"profile", profiles.GetActiveProfileName(ctx)), pyjson.EnsureASCII(false),
		pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil }))
	if err != nil {
		return nil, err
	}
	r, err := spawnShellHookArgv(ctx, argv, hookDir, timeout, payload)
	if err != nil {
		return nil, err
	}
	switch {
	case r.Error != "":
		return nil, fmt.Errorf("%s", r.Error)
	case r.TimedOut:
		return nil, fmt.Errorf("hook command timed out after %ds", timeout)
	case r.ReturnCode != nil && *r.ReturnCode != 0:
		return nil, fmt.Errorf("hook command exited with status %d: %s", *r.ReturnCode,
			pystr.Slice(pystr.Strip(r.Stderr), 0, shellHookStderrLimit))
	}
	stdout := pystr.Strip(r.Stdout)
	if stdout == "" {
		return nil, nil
	}
	v, err := pyjson.Loads(stdout)
	if err != nil {
		return nil, nil
	}
	obj, ok := v.(*pyjson.Object)
	if !ok {
		return nil, nil
	}
	return pyjson.Plain(obj), nil
}
