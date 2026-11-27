package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/approvals"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/packages"
	codeconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/codeexec/config"
	"github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
)

const description = "The agent wants to run a Python script, which can start programs and change files without the checks that commands get. " +
	"Your answer counts for this run only."

type registrant struct {
	// Tools a script calls go out through, with their approvals and guards
	registry *tools.Registry
	host     *terminal.Terminal
	packages *packages.Packages
	// Checks a script against what may not run inside the server
	approvals *approvals.Approvals
	// Settings of the profile in use, which the schema quotes its limits from
	config  *config.Config
	kernels kernels
	remotes remotes
	// Closed as icebath exits, which ends the reaper
	closed chan struct{}
}

func Register(registry *tools.Registry, stores *sessions.Registry, cfg *config.Config, host *terminal.Terminal, pm *packages.Packages,
	checks *approvals.Approvals) {
	r := &registrant{registry: registry, host: host, packages: pm, approvals: checks, config: cfg, closed: make(chan struct{}),
		kernels: kernels{held: map[key]*localKernel{}}, remotes: remotes{held: map[remoteKey]*remoteKernel{}}}
	registry.Register(r)
	stores.OnRelease(func(_ context.Context, session string) { r.release(session) })
}

func (r *registrant) Tools() []tools.Def {
	return []tools.Def{{
		Name:           "execute_code",
		Toolset:        "code_execution",
		Title:          "Python",
		About:          "Run a Python script that can use the other tools",
		Group:          "Code execution",
		Icon:           tools.IconShell,
		Subject:        "code",
		Schema:         r.schema(nil),
		Dynamic:        r.schema,
		Handler:        r.execute,
		Check:          func(env *tools.Env) bool { return r.host.Check(env.Config) == nil },
		MaxResultChars: 100_000,
		Effect:         tools.Mutates,
		Fallible:       true,
		// The run's own limit stops it, which the profile sets
		Timeout: -1,
	}}
}

// Session owning the call's kernels: its own, or for a subagent's the parent's qualified by the subagent's, so each child of a parent has
// kernels of its own that the cap of kernels never evicts while it runs
func owner(call *tools.Call) string {
	if child := call.Env.Delegated; child != nil && child.Parent != "" {
		return child.Parent + childQualifier + call.SessionID
	}
	return call.SessionID
}

// What joins a parent's session to its child's in the owner of the child's kernels
const childQualifier = "::child::"

// Kills the kernels of the session as it ends, a subagent's those it owns under its parent
func (r *registrant) release(session string) {
	if session == "" {
		return
	}
	owns := func(owner string) bool { return owner == session || strings.HasSuffix(owner, childQualifier+session) }
	r.kernels.shutdown(owns)
	r.remotes.shutdown(owns)
}

// Kills every kernel as icebath exits
func (r *registrant) Close() error {
	close(r.closed)
	r.kernels.shutdown(nil)
	r.remotes.shutdown(nil)
	return nil
}

// Tools a script of the session may call: those of the sandbox set the session offers, the whole set when it offers none of them
func (r *registrant) allowed(env *tools.Env) []string {
	var out []string
	for _, def := range r.registry.Offered(env) {
		if slices.Contains(sandboxTools, def.Name) {
			out = append(out, def.Name)
		}
	}
	return out
}

// Schema listing the tools a script of the session may import, with the limits and the folder of its profile; every tool and the defaults
// with no session
func (r *registrant) schema(env *tools.Env) map[string]any {
	cfg, set := r.config, sandboxTools
	if env != nil {
		cfg, set = env.Config, r.allowed(env)
	}
	var lines []string
	for _, doc := range docs {
		if slices.Contains(set, doc.name) {
			lines = append(lines, doc.doc)
		}
	}
	var examples []string
	for _, name := range []string{"web_search", "terminal"} {
		if slices.Contains(set, name) {
			examples = append(examples, name)
		}
	}
	if len(examples) == 0 {
		sorted := slices.Sorted(slices.Values(set))
		examples = sorted[:min(2, len(sorted))]
	}
	imports := "..."
	if len(examples) > 0 {
		imports = strings.Join(examples, ", ") + ", ..."
	}
	folder := "Scripts run in the session's working directory. Interpreter: the project's activated venv/conda python when one is active " +
		"(VIRTUAL_ENV/CONDA_PREFIX — matches terminal()); otherwise Icebath's own python (the common case — stdlib plus Icebath's deps; check " +
		"`import x` before relying on project packages)."
	if mode(cfg) == codeconfig.Strict {
		folder = "Scripts run in their own temp dir, not the session's CWD — pass absolute paths for any file that lives outside the session's " +
			"working directory, or use terminal()/read_file() to reach it."
	}
	text := "Run Python that calls Icebath tools programmatically. Use when you need 3+ tool calls with logic between them: filtering/reducing " +
		"large outputs before they enter context, branching, or loops (N pages/files, retry on failure). Use normal tool calls for single calls, " +
		"results you must reason over in full, or anything needing user interaction.\n\n" +
		"Calls run in a persistent session kernel: variables, imports, and loaded data survive across execute_code calls, so build on earlier work " +
		"instead of re-loading it. A timed-out or interrupted call loses that state.\n\n" +
		"Available via `from icebath_tools import ...`:\n\n" + strings.Join(lines, "\n") + "\n\n" +
		fmt.Sprintf("Limits: %s timeout, max %d tool calls per call. ", limit(cfg.GetInt(codeconfig.Timeout)), cfg.GetInt(codeconfig.MaxToolCalls)) +
		"Stdout over 50KB shows head/tail inline; the FULL text is auto-saved to a file whose path rides in the result.\n\n" + folder + "\n\n" +
		"Helpers require imports: `from icebath_tools import json_parse, shell_quote, retry`. json_parse(text) — tolerant json.loads for terminal() " +
		"output; shell_quote(s) — shlex.quote for dynamic shell args; retry(fn, max_attempts=3, delay=2) — exponential backoff."
	return tools.Schema(text, []string{"code"}, map[string]any{
		"code": tools.Property("string", "Python code to execute. Import tools with `from icebath_tools import "+imports+"` and print your final "+
			"result to stdout."),
		"reset": tools.Property("boolean", "Discard the kernel's persistent state and start fresh before running this code."),
	})
}

// Limit of a run as the schema says it: whole minutes as a 5-minute one, else seconds
func limit(seconds int) string {
	if seconds > 0 && seconds%60 == 0 {
		return fmt.Sprintf("%d-minute", seconds/60)
	}
	return fmt.Sprintf("%d-second", seconds)
}

// Runs the call's Python in the session's kernel, here or in its sandbox, past the checks of what may run and the user's approval
func (r *registrant) execute(ctx context.Context, call *tools.Call) string {
	_, hasCode := call.Args["code"]
	if _, hasCommand := call.Args["command"]; !hasCode && hasCommand {
		slog.Warn("execute_code received 'command' instead of the required 'code' argument")
		return tools.ErrorResult(errors.New("execute_code received a 'command' parameter, but it requires Python source in 'code'. Use " +
			"terminal(command=...) for shell commands; for Python, retry as execute_code(code=...)."))
	}
	code, isText := call.Args["code"].(string)
	if raw := call.Args["code"]; raw != nil && !isText {
		return tools.ErrorResult(fmt.Errorf("execute_code received %s in 'code', but it requires Python source as a string. Retry as "+
			"execute_code(code=\"...\").", kind(raw)))
	}
	reset, _ := call.Args["reset"].(bool)
	if strings.TrimSpace(code) == "" {
		return tools.ErrorResult(errors.New("No code provided. execute_code requires a non-empty 'code' parameter containing Python source. To run " +
			"shell commands, use terminal(command=...) instead."))
	}
	cfg := call.Env.Config
	if refusal := r.approvals.Script(cfg, code); refusal != "" {
		return tools.ErrorResult(errors.New(refusal))
	}
	backend, err := r.host.Configured(cfg)
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("execute_code refused: %s (profile terminal policy unresolved; fix the profile's settings and retry)", err))
	}
	if !backend.Isolated && call.Env.Gate != nil {
		err := call.Env.Gate(ctx, tools.Gate{Key: "execute_code", Description: description, Target: "Python script:\n" + code, Code: true,
			Subject: "execute_code runs arbitrary local Python (including subprocess calls that bypass shell-string approval checks)"})
		if refusal, ok := errors.AsType[*tools.Refusal](err); ok {
			return failed(cmp.Or(refusal.Message, "execute_code blocked by approval guard."), 0, 0, refusal.Summary)
		}
		if err != nil {
			return failed(err.Error(), 0, 0, "")
		}
	}
	set := r.allowed(call.Env)
	if len(set) == 0 {
		set = slices.Clone(sandboxTools)
	}
	c := cellRun{set: set, timeout: time.Duration(max(1, cfg.GetInt(codeconfig.Timeout))) * time.Second, maxCalls: cfg.GetInt(codeconfig.MaxToolCalls),
		reset: reset}
	if backend.Remote || backend.Container {
		lease, err := r.host.Acquire(ctx, cfg, call.SessionID, call.Env.Cwd)
		if err != nil {
			return failed(terminal.RedactError(err.Error()), 0, 0, "")
		}
		defer lease.Release()
		return r.remote(ctx, call, lease, code, c)
	}
	env := r.host.SessionEnv(cfg, call.SessionID)
	if key, ok := terminal.PathKey(env); ok {
		env[key] = r.packages.Path(env[key])
	}
	if c.python, err = r.python(ctx, cfg, env); err != nil {
		return failed(err.Error(), 0, 0, "")
	}
	c.cwd = r.folder(call, "")
	return r.local(ctx, call, code, c)
}

// Kind of a JSON value as an error names it
func kind(v any) string {
	switch v.(type) {
	case bool:
		return "a boolean"
	case float64:
		return "a number"
	case []any:
		return "a list"
	case map[string]any:
		return "an object"
	}
	return fmt.Sprintf("%T", v)
}
