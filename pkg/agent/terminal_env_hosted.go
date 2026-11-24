package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
	shellutil "github.com/nickheyer/icebath/pkg/utils/shell"
)

const (
	// Time past its timeout a command of a plugin environment may take to answer before the wait ends
	hostedExecuteGrace = 2 * time.Second
	// Pause between two looks at the interrupt of the worker a plugin environment runs a command for
	hostedInterruptPoll = 200 * time.Millisecond
	// Seconds a file read through the commands of a plugin environment may take
	hostedFetchTimeoutSeconds = 300
)

var (
	errHostedCommandTimeout     = errors.New("the command timed out")
	errHostedCommandInterrupted = errors.New("the command was interrupted")
)

// Environment of a plugin terminal backend, living in the guest of its plugin and driven over the plugin wire
type hostedTerminalEnv struct {
	obj          *plugins.HostedObject
	backend      string
	timeout      int
	tempDir      string
	hostCwd      string
	hostCwdMount string
	persistent   bool
	hostFiles    bool
	mu           sync.Mutex
	cwd          string
	sandboxHome  string
}

var (
	_ tools.Environment        = (*hostedTerminalEnv)(nil)
	_ tools.StreamExecPrefixer = (*hostedTerminalEnv)(nil)
)

// Wraps the environment object a plugin backend created, its facts read once and its starting directory the one it resolved
func newHostedTerminalEnv(ctx context.Context, obj *plugins.HostedObject, backend string, opts tools.CreateOptions) (*hostedTerminalEnv, error) {
	if !obj.HasMethod("execute") || !obj.HasMethod("cleanup") {
		return nil, fmt.Errorf("terminal backend %s created a %s environment without execute and cleanup", backend, obj.TypeName)
	}
	e := &hostedTerminalEnv{obj: obj, backend: backend, timeout: opts.Timeout, tempDir: "/tmp", cwd: opts.Cwd}
	attr := func(name string) any {
		v, err := obj.Get(ctx, name)
		if err != nil {
			return nil
		}
		return v
	}
	if cwd, ok := attr("cwd").(string); ok && cwd != "" {
		e.cwd = cwd
	}
	e.persistent = config.PyTruthy(attr("persistent"))
	e.hostFiles = attr("uses_host_filesystem") == true
	e.hostCwd, _ = attr("host_cwd").(string)
	e.hostCwdMount, _ = attr("host_cwd_mount").(string)
	if obj.HasMethod("get_temp_dir") {
		v, err := obj.Call(ctx, "get_temp_dir", nil, nil)
		if err != nil {
			return nil, err
		}
		if dir, ok := v.(string); ok && dir != "" {
			e.tempDir = dir
		}
	}
	return e, nil
}

// Runs one command in the plugin environment, the wait ending at the timeout, at a user interrupt or when ctx ends
func (e *hostedTerminalEnv) Execute(ctx context.Context, command string, opts tools.ExecOptions) (*tools.ExecResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !opts.NoCompoundRewrite {
		command = tools.RewriteCompoundBackground(command)
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = e.timeout
	}
	cwd := opts.Cwd
	if cwd == "" {
		cwd = e.Cwd()
	}
	kwargs := map[string]any{"cwd": cwd, "timeout": int64(timeout), "stdin_data": nil, "bounded_capture": opts.BoundedCapture}
	if opts.Stdin != nil {
		kwargs["stdin_data"] = *opts.Stdin
	}
	callCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if timeout > 0 {
		timer := time.AfterFunc(time.Duration(timeout)*time.Second+hostedExecuteGrace, func() { cancel(errHostedCommandTimeout) })
		defer timer.Stop()
	}
	worker := tools.WorkerOf(ctx)
	watched := make(chan struct{})
	defer close(watched)
	go func() {
		tick := time.NewTicker(hostedInterruptPoll)
		defer tick.Stop()
		for {
			select {
			case <-watched:
				return
			case <-tick.C:
				if tools.IsInterrupted(ctx) || tools.IsWorkerInterrupted(worker) {
					cancel(errHostedCommandInterrupted)
					return
				}
				// The process runs inside the plugin's environment where icebath holds nothing to hand to the background, so a yield request ends here and the command runs on
				if opts.YieldHandler != nil {
					tools.ConsumeYield(worker)
				}
			}
		}
	}()
	raw, err := e.obj.Call(callCtx, "execute", []any{command}, kwargs)
	if err != nil {
		cause := context.Cause(callCtx)
		switch {
		case errors.Is(cause, errHostedCommandTimeout):
			return &tools.ExecResult{Output: fmt.Sprintf("[Command timed out after %ds]", timeout), ReturnCode: 124, TimedOut: true}, nil
		case cause != nil:
			return &tools.ExecResult{Output: "[Command interrupted]", ReturnCode: 130, Interrupted: true}, nil
		}
		return nil, err
	}
	fields := plugins.FieldsOf(raw)
	if fields == nil {
		return nil, fmt.Errorf("terminal backend %s execute returned %s where a mapping was expected", e.backend, errchain.Kind(raw))
	}
	result := &tools.ExecResult{TimedOut: config.PyTruthy(fields["timed_out"])}
	result.Output, _ = fields["output"].(string)
	code, coded := fields["returncode"]
	if !coded {
		code = fields["exit_code"]
	}
	switch n := code.(type) {
	case int64:
		result.ReturnCode = int(n)
	case float64:
		result.ReturnCode = int(n)
	default:
		return nil, fmt.Errorf("terminal backend %s execute returned no whole number exit code", e.backend)
	}
	if dir, ok := fields["cwd"].(string); ok && dir != "" {
		e.SetCwd(dir)
		result.CwdObserved, result.Cwd = true, dir
	}
	return result, nil
}

// Releases the environment in the plugin
func (e *hostedTerminalEnv) Cleanup(forceRemove bool) error {
	_, err := e.obj.Call(context.Background(), "cleanup", nil, map[string]any{"force_remove": forceRemove})
	return err
}

func (e *hostedTerminalEnv) EnvType() string { return e.backend }

// Directory the session is in
func (e *hostedTerminalEnv) Cwd() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cwd
}

func (e *hostedTerminalEnv) SetCwd(cwd string) {
	e.mu.Lock()
	e.cwd = cwd
	e.mu.Unlock()
}

func (e *hostedTerminalEnv) TempDir() string      { return e.tempDir }
func (e *hostedTerminalEnv) HostCwd() string      { return e.hostCwd }
func (e *hostedTerminalEnv) HostCwdMount() string { return e.hostCwdMount }

// Home the provider declared for the environment, recorded when it was created
func (e *hostedTerminalEnv) SandboxHome() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sandboxHome
}

// Records the home the provider declared for this environment
func (e *hostedTerminalEnv) StampSandboxHome(home string) {
	e.mu.Lock()
	e.sandboxHome = home
	e.mu.Unlock()
}

func (e *hostedTerminalEnv) IsPersistent() bool { return e.persistent }

// A plugin environment is keyed by its task, never by the session scope of the docker backend
func (e *hostedTerminalEnv) IsSessionScoped() bool { return false }

func (e *hostedTerminalEnv) UsesHostFilesystem() bool { return e.hostFiles }

// Argv on this machine that runs its remainder inside the environment, none for a plugin that offers no exec_prefix
func (e *hostedTerminalEnv) ExecPrefix(user string, interactive bool) ([]string, error) {
	if !e.obj.HasMethod("exec_prefix") {
		return nil, nil
	}
	kwargs := map[string]any{"user": nil, "interactive": interactive}
	if user != "" {
		kwargs["user"] = user
	}
	v, err := e.obj.Call(context.Background(), "exec_prefix", nil, kwargs)
	if err != nil || v == nil {
		return nil, err
	}
	words, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s.exec_prefix() returned %s, not a list of argv words", e.backend, errchain.Kind(v))
	}
	argv := make([]string, 0, len(words))
	for _, word := range words {
		s, isText := word.(string)
		if !isText {
			return nil, fmt.Errorf("%s.exec_prefix() returned %s, not a list of argv words", e.backend, errchain.Kind(v))
		}
		argv = append(argv, s)
	}
	return argv, nil
}

// Copies a regular file out of the environment, through the plugin's own fetch when it offers one and over base64 through its commands otherwise
func (e *hostedTerminalEnv) FetchFile(ctx context.Context, remotePath, localDest string, maxBytes int) error {
	var data []byte
	if e.obj.HasMethod("fetch_file") {
		v, err := e.obj.Call(ctx, "fetch_file", []any{remotePath}, map[string]any{"max_bytes": int64(maxBytes)})
		if err != nil {
			return &tools.FileFetchError{Msg: fmt.Sprintf("could not read %q in the sandbox: %v", remotePath, err)}
		}
		fetched, ok := v.([]byte)
		if !ok {
			return &tools.FileFetchError{Msg: fmt.Sprintf("could not read %q in the sandbox (missing, not a regular file, or unreadable)", remotePath)}
		}
		data = fetched
	} else {
		marker := "__ICEBATH_FETCH_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12] + "__"
		quoted := shellutil.Quote(remotePath)
		cmd := fmt.Sprintf("[ -f %s ] && echo %s && head -c %d < %s | base64 && echo %s", quoted, marker, maxBytes+1, quoted, marker)
		result, err := e.Execute(ctx, cmd, tools.ExecOptions{Timeout: hostedFetchTimeoutSeconds, NoCompoundRewrite: true})
		if err != nil {
			return err
		}
		first, last := strings.Index(result.Output, marker), strings.LastIndex(result.Output, marker)
		if result.ReturnCode != 0 || first == -1 || last <= first {
			return &tools.FileFetchError{Msg: fmt.Sprintf("could not read %q in the sandbox (missing, not a regular file, or unreadable)", remotePath)}
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(result.Output[first+len(marker):last]), ""))
		if err != nil {
			return &tools.FileFetchError{Msg: fmt.Sprintf("transfer of %q was corrupted in transit: %v", remotePath, err)}
		}
		data = decoded
	}
	if len(data) > maxBytes {
		return &tools.FileFetchError{Msg: fmt.Sprintf("%q exceeds the %d MB delivery limit", remotePath, maxBytes/(1024*1024))}
	}
	return os.WriteFile(localDest, data, 0o600)
}

// Path a file resolves to inside the environment, empty when it cannot be resolved
func (e *hostedTerminalEnv) FetchRealpath(ctx context.Context, remotePath string) string {
	if e.obj.HasMethod("fetch_realpath") {
		v, err := e.obj.Call(ctx, "fetch_realpath", []any{remotePath}, nil)
		if err != nil {
			return ""
		}
		path, _ := v.(string)
		return path
	}
	result, err := e.Execute(ctx, "readlink -f "+shellutil.Quote(remotePath)+" 2>/dev/null", tools.ExecOptions{NoCompoundRewrite: true})
	if err != nil || result.ReturnCode != 0 {
		return ""
	}
	lines := strings.Split(result.Output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); strings.HasPrefix(line, "/") {
			return line
		}
	}
	return ""
}
