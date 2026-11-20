package pluginsdk

import (
	"context"
	"fmt"
)

// Terminal backend a plugin adds, selected by terminal.backend, its environments running the commands of the terminal, code and file tools
type TerminalBackend struct {
	// Value of terminal.backend that selects the backend, lower case letters, digits and underscores
	Name        string
	DisplayName string
	// One line shown in backend pickers
	Description string
	// Where commands run, named in the system prompt when the live probe fails
	EnvDescription string
	// Whether commands run on the machine icebath runs on, a backend running them elsewhere when false
	Local bool
	// Whether commands see the files of the machine icebath runs on, a backend with a filesystem of its own when false
	HostFilesystem bool
	// Whether dangerous commands still ask for approval in a backend with a filesystem of its own, for one that can reach paths of the machine
	KeepApprovals bool
	// Directory inside the backend that holds the synced cache files, such as ~/.icebath, empty when the paths of the machine stay correct
	CachePathBase string
	// Directory commands start in when terminal.cwd is unset or unusable there, an absolute path or ~ for the home each environment resolves
	DefaultCwd string
	// Environment variables holding the credentials of the backend, removed from every process the agent starts
	StripEnvKeys []string
	// Whether a session gets an environment of its own when the backend does not persist
	SessionIsolatedWhenNonpersistent bool
	// Whether environments answer ExecPrefix, letting the bot desktop, computer use and the browser follow the terminal into the backend
	SupportsStreams bool
	// Whether the backend can run commands, a cheap check that makes no network call
	IsAvailable func(ctx context.Context) bool
	// Full requirements check with the merged terminal settings, the availability check when unset
	CheckRequirements func(ctx context.Context, config map[string]any) (bool, error)
	// Lines shown after the backend is picked in setup
	SetupInstructions func(ctx context.Context) []string
	// Rows the doctor command shows, the availability row when unset
	DoctorChecks func(ctx context.Context) []DoctorCheck
	// Creates or resumes the environment of a task
	CreateEnvironment func(ctx context.Context, opts TerminalOptions) (*TerminalEnvironment, error)
}

// One row of a backend health report
type DoctorCheck struct {
	OK     bool
	Label  string
	Detail string
}

// What icebath asks an environment to be created with
type TerminalOptions struct {
	// Directory commands start in, ~ when the backend declared that default
	Cwd string
	// Seconds a command may run when it names no timeout
	Timeout int
	// Key the environment is reused and persisted under
	TaskID string
	// Image override of one task, empty for the setting of the backend
	Image string
	// Resource and persistence settings of a backend with a filesystem of its own
	ContainerConfig map[string]any
	// Directory of the machine the session works in
	HostCwd string
	// Whether the environment is created only to check that it can be
	ProbeOnly bool
}

// One environment of a terminal backend, the place commands of a task run
type TerminalEnvironment struct {
	// Absolute directory the environment starts in, the home it resolved when it was created with ~
	Cwd string
	// Directory for scratch files inside the environment, /tmp when empty
	TempDir string
	// Whether the environment outlives its session
	Persistent bool
	// Whether the file tools may read and write the files of the environment straight on the machine icebath runs on
	UsesHostFilesystem bool
	// Directory of the machine mounted into the environment and the path it is mounted at, empty for none
	HostCwd      string
	HostCwdMount string
	// Runs one command and returns its output and exit code, stopping the command when ctx ends
	Execute func(ctx context.Context, cmd TerminalCommand) (TerminalResult, error)
	// Releases the environment, force removing what a persistent one would keep
	Cleanup func(ctx context.Context, force bool) error
	// Argv on the machine icebath runs on that runs what follows it inside the environment with its input and output open, empty when it cannot now
	ExecPrefix func(ctx context.Context, user string, interactive bool) ([]string, error)
	// Bytes of a regular file inside the environment, read through Execute when unset
	FetchFile func(ctx context.Context, path string, maxBytes int) ([]byte, error)
	// Path a file of the environment resolves to, read through Execute when unset
	FetchRealpath func(ctx context.Context, path string) (string, error)
}

// One command an environment runs
type TerminalCommand struct {
	Command string
	// Directory the command runs in, the directory the session is in
	Cwd string
	// Seconds the command may run
	TimeoutSeconds int
	// Text the command reads on its standard input, nil for none
	Stdin *string
	// Whether the output may be cut to its head and tail while it is read, set only for a command the model sees the output of
	BoundedCapture bool
}

// Outcome of one command
type TerminalResult struct {
	// Standard output and standard error merged
	Output   string
	ExitCode int
	// Directory the session is in after the command, empty to keep the one it ran in
	Cwd string
	// Whether the environment stopped the command at its timeout
	TimedOut bool
}

// Provider object of a terminal backend, its classification read as attributes and its hooks called as methods
func terminalBackendObject(b TerminalBackend) *Object {
	attrs := map[string]any{"name": b.Name, "is_remote": !b.Local, "is_container": !b.HostFilesystem,
		"skip_container_guards": !b.HostFilesystem && !b.KeepApprovals, "strip_env_keys": stringsAny(b.StripEnvKeys),
		"session_isolated_when_nonpersistent": b.SessionIsolatedWhenNonpersistent, "supports_streams": b.SupportsStreams}
	for name, value := range map[string]string{"display_name": b.DisplayName, "description": b.Description, "env_description": b.EnvDescription,
		"cache_path_base": b.CachePathBase, "default_cwd": b.DefaultCwd} {
		if value != "" {
			attrs[name] = value
		}
	}
	methods := map[string]Func{
		"is_available": func(ctx context.Context, _ []any, _ map[string]any) (any, error) { return b.IsAvailable(ctx), nil },
		"create_environment": func(ctx context.Context, _ []any, kwargs map[string]any) (any, error) {
			timeout, _ := integer(kwargs["timeout"])
			probe, _ := kwargs["probe_only"].(bool)
			env, err := b.CreateEnvironment(ctx, TerminalOptions{Cwd: text(kwargs["cwd"]), Timeout: int(timeout), TaskID: text(kwargs["task_id"]),
				Image: text(kwargs["image"]), ContainerConfig: mapOrEmpty(asMap(kwargs["container_config"])), HostCwd: text(kwargs["host_cwd"]), ProbeOnly: probe})
			if err != nil {
				return nil, err
			}
			if env == nil || env.Execute == nil || env.Cleanup == nil {
				return nil, &Error{Type: "TypeError", Message: fmt.Sprintf("terminal backend %s created an environment without Execute and Cleanup", b.Name)}
			}
			return terminalEnvironmentObject(env), nil
		},
	}
	if b.CheckRequirements != nil {
		methods["check_requirements"] = func(ctx context.Context, args []any, _ map[string]any) (any, error) {
			return b.CheckRequirements(ctx, argMap(args, 0))
		}
	}
	if b.SetupInstructions != nil {
		methods["setup_instructions"] = func(ctx context.Context, _ []any, _ map[string]any) (any, error) {
			return stringsAny(b.SetupInstructions(ctx)), nil
		}
	}
	if b.DoctorChecks != nil {
		methods["doctor_checks"] = func(ctx context.Context, _ []any, _ map[string]any) (any, error) {
			rows := []any{}
			for _, check := range b.DoctorChecks(ctx) {
				rows = append(rows, []any{check.OK, check.Label, check.Detail})
			}
			return rows, nil
		}
	}
	return &Object{Type: "TerminalEnvironmentProvider", Attrs: attrs, Methods: methods}
}

// Object icebath drives one environment through, its facts read as attributes and its operations called as methods
func terminalEnvironmentObject(e *TerminalEnvironment) *Object {
	tempDir := e.TempDir
	if tempDir == "" {
		tempDir = "/tmp"
	}
	attrs := map[string]any{"cwd": e.Cwd, "persistent": e.Persistent, "uses_host_filesystem": e.UsesHostFilesystem,
		"host_cwd": e.HostCwd, "host_cwd_mount": e.HostCwdMount}
	methods := map[string]Func{
		"execute": func(ctx context.Context, args []any, kwargs map[string]any) (any, error) {
			timeout, _ := integer(kwargs["timeout"])
			bounded, _ := kwargs["bounded_capture"].(bool)
			cmd := TerminalCommand{Command: argText(args, 0), Cwd: text(kwargs["cwd"]), TimeoutSeconds: int(timeout), BoundedCapture: bounded}
			if stdin, ok := kwargs["stdin_data"].(string); ok {
				cmd.Stdin = &stdin
			}
			result, err := e.Execute(ctx, cmd)
			if err != nil {
				return nil, err
			}
			out := map[string]any{"output": result.Output, "returncode": int64(result.ExitCode), "timed_out": result.TimedOut}
			if result.Cwd != "" {
				out["cwd"] = result.Cwd
			}
			return out, nil
		},
		"cleanup": func(ctx context.Context, _ []any, kwargs map[string]any) (any, error) {
			force, _ := kwargs["force_remove"].(bool)
			return nil, e.Cleanup(ctx, force)
		},
		"get_temp_dir": constant(tempDir),
	}
	if e.ExecPrefix != nil {
		methods["exec_prefix"] = func(ctx context.Context, _ []any, kwargs map[string]any) (any, error) {
			interactive, _ := kwargs["interactive"].(bool)
			argv, err := e.ExecPrefix(ctx, text(kwargs["user"]), interactive)
			if err != nil || len(argv) == 0 {
				return nil, err
			}
			return stringsAny(argv), nil
		}
	}
	if e.FetchFile != nil {
		methods["fetch_file"] = func(ctx context.Context, args []any, kwargs map[string]any) (any, error) {
			limit, _ := integer(kwargs["max_bytes"])
			return e.FetchFile(ctx, argText(args, 0), int(limit))
		}
	}
	if e.FetchRealpath != nil {
		methods["fetch_realpath"] = func(ctx context.Context, args []any, _ map[string]any) (any, error) {
			path, err := e.FetchRealpath(ctx, argText(args, 0))
			if err != nil || path == "" {
				return nil, err
			}
			return path, nil
		}
	}
	return &Object{Type: "TerminalEnvironment", Attrs: attrs, Methods: methods}
}

// Registers a terminal backend
func (h *host) RegisterTerminalBackend(backend TerminalBackend) (*Handle, error) {
	if backend.Name == "" || backend.IsAvailable == nil || backend.CreateEnvironment == nil {
		return nil, fmt.Errorf("a terminal backend needs a name, an availability check and a way to create its environment")
	}
	return h.RegisterProvider("terminal_environment", terminalBackendObject(backend))
}
