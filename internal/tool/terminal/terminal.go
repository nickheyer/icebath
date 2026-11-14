// Runs model shell commands in the task environment with pre-exec guards, approvals, sudo and background handoff
package terminal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/tool"
	"github.com/nickheyer/icebath/internal/tool/approval"
	"github.com/nickheyer/icebath/internal/tool/env"
	"github.com/nickheyer/icebath/internal/tool/process"
	"github.com/nickheyer/icebath/internal/tool/redact"
)

const (
	replacementChar        = string(rune(0xFFFD))
	byteOrderMark          = string(rune(0xFEFF))
	defaultForegroundMax   = 600
	PreExecGuardMinTimeout = 30 * time.Second
	maxResultChars         = 100_000
)

const description = `Run shell commands on the OS/shell/backend named in your environment section. The working directory, files and exported variables persist between calls: activate a virtualenv or export once, not before every command.

Use read_file, search_files, patch and write_file instead of cat/head/tail, grep/find/ls, sed/awk and echo/heredoc. Output is auto-truncated with the full text saved to a file; never pipe through head/tail to shorten it.

Foreground (default) returns as soon as the command finishes. To wait a fixed time, run the wait in the foreground; never background a sleep or polling loop. Background is only for commands that must keep running after this call returns; health-check a server in a separate call and manage jobs with process_manage. When a command changes directory, trust the result's "cwd" instead of prefixing later commands with cd.
`

const ptyDisabledReason = "PTY disabled for this command because it expects piped stdin/EOF " +
	"(for example gh auth login --with-token). For local background " +
	"processes, call process(action='close') after writing so it receives " +
	"EOF."

type Transformer interface {
	Transform(ctx context.Context, r TransformRequest) (string, bool)
}

type TransformRequest struct {
	Command    string
	Output     string
	ExitCode   int
	TaskID     string
	EnvType    string
	ToolCallID string
}

type Verification interface {
	RecordTerminal(ctx context.Context, r CommandRun) (Evidence, bool, error)
}

type CommandRun struct {
	Command        string
	Cwd            string
	SessionID      string
	ExitCode       int
	Output         string
	PipelineStatus []int
}

type Evidence struct {
	Status           string
	Kind             string
	Scope            string
	CanonicalCommand string
}

type Metrics interface {
	TerminalOutcome(ctx context.Context, command, backend string, r tool.ExecResult)
	ExecutionBackend(ctx context.Context, toolName, backend, result, errorClass string)
}

type Watchers interface {
	ArmProcessWatcher(ctx context.Context, w process.Watcher) bool
}

type SkillLocks interface {
	CommandRefusal(ctx context.Context, command, cwd string) (string, bool, error)
}

type LiveDatabases interface {
	HasLiveConnection(path string) bool
	OfflineAccess(path, what string, fn func() error) error
}

type Options struct {
	Config     *config.Store
	Homes      *home.Resolver
	Envs       *env.Manager
	Processes  *process.Registry
	Gate       *approval.Gate
	Redactor   *redact.Redactor
	Sudo       *Sudo
	Role       process.Role
	Transform  Transformer
	Evidence   Verification
	Metrics    Metrics
	Watchers   Watchers
	SkillLocks SkillLocks
	Databases  LiveDatabases
}

type Terminal struct {
	o             Options
	foregroundMax int
	schema        json.RawMessage
	slots         sync.Map
}

func New(o Options) (*Terminal, error) {
	switch {
	case o.Config == nil:
		return nil, &tool.UnfilledError{Name: "terminal.Options.Config"}
	case o.Homes == nil:
		return nil, &tool.UnfilledError{Name: "terminal.Options.Homes"}
	case o.Envs == nil:
		return nil, &tool.UnfilledError{Name: "terminal.Options.Envs"}
	case o.Processes == nil:
		return nil, &tool.UnfilledError{Name: "terminal.Options.Processes"}
	case o.Gate == nil:
		return nil, &tool.UnfilledError{Name: "terminal.Options.Gate"}
	case o.Redactor == nil:
		return nil, &tool.UnfilledError{Name: "terminal.Options.Redactor"}
	case o.Sudo == nil:
		return nil, &tool.UnfilledError{Name: "terminal.Sudo"}
	case o.Role == nil:
		return nil, &tool.UnfilledError{Name: "process.Role"}
	case o.Transform == nil:
		return nil, &tool.UnfilledError{Name: "terminal.Transformer"}
	case o.Evidence == nil:
		return nil, &tool.UnfilledError{Name: "terminal.Verification"}
	case o.Metrics == nil:
		return nil, &tool.UnfilledError{Name: "terminal.Metrics"}
	case o.Watchers == nil:
		return nil, &tool.UnfilledError{Name: "terminal.Watchers"}
	case o.SkillLocks == nil:
		return nil, &tool.UnfilledError{Name: "terminal.SkillLocks"}
	case o.Databases == nil:
		return nil, &tool.UnfilledError{Name: "terminal.LiveDatabases"}
	}
	t := &Terminal{o: o, foregroundMax: foregroundMaxFromEnv()}
	t.schema = json.RawMessage(schemaText(t.foregroundMax))
	return t, nil
}

func foregroundMaxFromEnv() int {
	raw, ok := os.LookupEnv("TERMINAL_MAX_FOREGROUND_TIMEOUT")
	if !ok || raw == "" {
		return defaultForegroundMax
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		slog.Warn(fmt.Sprintf("Invalid value for %s: %q (expected %s). Falling back to %d.", "TERMINAL_MAX_FOREGROUND_TIMEOUT", raw, "integer", defaultForegroundMax))
		return defaultForegroundMax
	}
	return n
}

func schemaText(max int) string {
	timeoutText := fmt.Sprintf("Max seconds (default 180, foreground max %d). Finishing early returns at once, so set it generously. Above %ds the command runs as a tracked background process with notify_on_complete=true (the result says so; do not re-run it).", max, max)
	quoted, _ := json.Marshal(timeoutText)
	return `{"type":"object","properties":{` +
		`"command":{"type":"string","description":"The shell command to execute"},` +
		`"background":{"type":"boolean","description":"Keep running after this call returns; returns a session_id.","default":false},` +
		`"timeout":{"type":"integer","description":` + string(quoted) + `,"minimum":1},` +
		`"workdir":{"type":"string","description":"Absolute working directory for this command only."},` +
		`"pty":{"type":"boolean","description":"With background=true: pseudo-terminal for interactive CLIs (REPLs, coding agents); drive it with process_manage write/submit. Local backend only.","default":false},` +
		`"notify":{"description":"With background=true. true: one notification on exit; use it for anything that ends (builds, tests, deploys), or the job runs silently. ['pattern', ...]: notify when a line matches, only for one-shot readiness on processes that never exit; rate-limited. Omit for servers/daemons.","anyOf":[{"type":"boolean"},{"type":"array","items":{"type":"string"}}]},` +
		`"heartbeat":{"type":"integer","minimum":0,"default":0,"description":"With background=true: also notify every N seconds (min 60) with new output; quiet ticks are skipped. For bounded jobs you must react to mid-run, never servers. Implies notify=true; 0 = off."},` +
		`"persist_on_release":{"type":"boolean","default":false,"description":"With background=true: survive session end, /new, compression and other cleanup (not host exit). Only for jobs the user explicitly wants to outlive the conversation."}` +
		`},"required":["command"]}`
}

func (t *Terminal) Tool() tool.Tool {
	return tool.Tool{
		Name:           "terminal",
		Toolset:        "terminal",
		Description:    description,
		Parameters:     t.schema,
		Handler:        t.handle,
		Check:          &tool.Check{Name: "terminal", Probe: t.requirements},
		Emoji:          "\U0001F4BB",
		MaxResultChars: maxResultChars,
		Effects:        tool.Mutating,
		Prepare:        t.prepare,
	}
}

func (t *Terminal) requirements(ctx context.Context) (bool, error) {
	cfg, err := t.o.Envs.Settings().EnvConfig(ctx)
	if err != nil {
		slog.Error(fmt.Sprintf("Terminal requirements check failed: %v", err))
		t.o.Envs.RecordUnavailableReason(fmt.Sprintf("the requirements check failed: %v", err), true)
		return false, nil
	}
	return t.o.Envs.CheckRequirements(ctx, cfg), nil
}

type Request struct {
	Command               string
	Background            bool
	Timeout               int
	Workdir               string
	PTY                   bool
	Notify                bool
	WatchPatterns         []string
	Heartbeat             int
	PersistOnRelease      bool
	HostLocal             bool
	CompletionOutputChars int
}

func (t *Terminal) Run(ctx context.Context, c *tool.Call, r Request) (tool.Result, error) {
	in := invocation{command: r.Command, background: r.Background, workdir: r.Workdir, pty: r.PTY, notify: r.Notify,
		watch: r.WatchPatterns, heartbeat: r.Heartbeat, persist: r.PersistOnRelease, hostLocal: r.HostLocal,
		completionChars: r.CompletionOutputChars}
	if r.Timeout != 0 {
		v := float64(r.Timeout)
		in.timeout = &v
	}
	return t.execute(ctx, c, in)
}

type invocation struct {
	command         any
	background      bool
	timeout         *float64
	workdir         any
	pty             bool
	notify          bool
	watch           []string
	heartbeat       int
	persist         bool
	hostLocal       bool
	completionChars int
}

func (t *Terminal) handle(ctx context.Context, c *tool.Call, args tool.Args) (tool.Result, error) {
	if err := t.validatePrepared(ctx, c, args); err != nil {
		return tool.Result{}, err
	}
	_, hasCommand := args["command"]
	if _, hasCode := args["code"]; !hasCommand && hasCode {
		return tool.Failure("terminal received a 'code' parameter, but it requires a shell " +
			"command in 'command'. Use execute_code(code=...) for Python; " +
			"for shell, retry as terminal(command=...)."), nil
	}
	notify := args["notify"]
	notifyOnComplete := truthy(args["notify_on_complete"])
	watch := args["watch_patterns"]
	heartbeat := 0
	if hb := args["heartbeat"]; truthy(hb) {
		n, ok := wholeNumber(hb)
		if !ok || n < 0 {
			return tool.Failure("heartbeat must be a whole number of seconds (0 disables; positive values are clamped to min 60)."), nil
		}
		heartbeat = n
	}
	persist := truthy(args["persist_on_release"])
	background := truthy(args["background"])
	if !background {
		if truthy(notify) || truthy(watch) || notifyOnComplete {
			return tool.Failure("notify only applies to background commands (foreground " +
				"results return directly). Either drop it, or run as " +
				"terminal(command=..., background=true, notify=...)."), nil
		}
		heartbeat = 0
		if truthy(args["pty"]) {
			return tool.Failure("pty requires background=true (a PTY session is interacted " +
				"with via process(action='write'/'submit'), which needs a " +
				"tracked background process). Retry as terminal(command=..., " +
				"background=true, pty=true)."), nil
		}
		if persist {
			return tool.Failure("persist_on_release only applies to background commands (a foreground " +
				"process is awaited inline and has nothing to persist). Retry as " +
				"terminal(command=..., background=true, persist_on_release=true)."), nil
		}
	}
	if notify != nil {
		switch v := notify.(type) {
		case bool:
			notifyOnComplete = v
			watch = nil
		case []any:
			watch = v
			notifyOnComplete = false
		case []string:
			watch = v
			notifyOnComplete = false
		default:
			return tool.Failure("notify must be true/false (notify on exit) or a list of " +
				"strings (notify on output pattern match)."), nil
		}
	}
	if heartbeat != 0 {
		notifyOnComplete = true
	}
	in := invocation{command: args["command"], background: background, workdir: args["workdir"], pty: truthy(args["pty"]),
		notify: notifyOnComplete, watch: patternList(watch), heartbeat: heartbeat, persist: persist}
	if raw, ok := args["timeout"]; ok && raw != nil {
		n, ok := number(raw)
		if !ok {
			return t.fatal(ctx, nil, false, fmt.Errorf("timeout must be a number, got %s", typeName(raw)))
		}
		in.timeout = &n
	}
	return t.execute(ctx, c, in)
}

type plan struct {
	placement env.Placement
	timeout   float64
	promoted  *float64
	autoBg    *string
}

type rejected struct{ result tool.Result }

func (r *rejected) Error() string { return r.result.Text }

func (t *Terminal) plan(ctx context.Context, c *tool.Call, in invocation) (*plan, error) {
	command, ok := in.command.(string)
	if !ok {
		slog.Warn(fmt.Sprintf("Rejected invalid terminal command value: %s", typeName(in.command)))
		return nil, &rejected{errorJSON(fmt.Sprintf("Invalid command: expected string, got %s", typeName(in.command)), -1, "error")}
	}
	placement, err := t.o.Envs.Place(ctx, c.TaskID, in.hostLocal)
	if err != nil {
		return nil, err
	}
	if in.timeout != nil && *in.timeout <= 0 {
		return nil, &rejected{tool.Failure(fmt.Sprintf("timeout must be a positive number of seconds (got %s).", formatNumber(*in.timeout)))}
	}
	p := &plan{placement: placement, timeout: float64(placement.Config.Timeout)}
	if in.timeout != nil {
		p.timeout = *in.timeout
	}
	if !in.background {
		autoBg, err := t.trailingAmpBackground(ctx, c, command)
		if err != nil {
			return nil, err
		}
		if autoBg == nil {
			if guidance, ok := foregroundGuidance(command); ok {
				return nil, &rejected{errorJSON(guidance, -1, "error")}
			}
			if in.timeout != nil && *in.timeout > float64(t.foregroundMax) {
				v := *in.timeout
				p.promoted = &v
			}
		}
		p.autoBg = autoBg
	}
	return p, nil
}

func (t *Terminal) execute(ctx context.Context, c *tool.Call, in invocation) (tool.Result, error) {
	p, err := t.plan(ctx, c, in)
	if err != nil {
		var rej *rejected
		if errors.As(err, &rej) {
			return rej.result, nil
		}
		return t.failure(ctx, c, nil, in.hostLocal, err)
	}
	command := in.command.(string)
	background := in.background
	if p.autoBg != nil {
		command, background = *p.autoBg, true
	}
	shell, err := t.o.Envs.AcquirePlaced(ctx, c.TaskID, p.placement, int(math.Ceil(p.timeout)))
	if err != nil {
		return t.failure(ctx, c, p, in.hostLocal, err)
	}
	release := t.o.Envs.CallInFlight(p.placement.Key, c.TaskID)
	defer release()
	var e tool.Environment = shell
	kind, cwd, eff := p.placement.Kind, p.placement.Cwd, p.placement.Key
	sessionKey := c.SessionKey
	if sessionKey == "" {
		sessionKey = c.TaskID
	}
	ctx = withCall(ctx, c)
	workdir, ok := in.workdir.(string)
	if !ok && in.workdir != nil {
		return t.failure(ctx, c, p, in.hostLocal, fmt.Errorf("workdir must be a string, got %s", typeName(in.workdir)))
	}
	guardTimeout := max(time.Duration(p.timeout*float64(time.Second)), PreExecGuardMinTimeout)
	blocked, err := t.boundedPreExec(ctx, c, guardTimeout, command, e, kind, cwd, workdir, sessionKey)
	if err != nil {
		var rej *rejected
		if errors.As(err, &rej) {
			return rej.result, nil
		}
		return t.failure(ctx, c, p, in.hostLocal, err)
	}
	if blocked != nil {
		return *blocked, nil
	}
	note, err := t.approvalGuards(ctx, c, command, kind, p.placement.Config.DockerHostAccess())
	if err != nil {
		var rej *rejected
		if errors.As(err, &rej) {
			return rej.result, nil
		}
		return t.failure(ctx, c, p, in.hostLocal, err)
	}
	ptyDisabled := in.pty && requiresPipeStdin(command)
	notify, watch := in.notify, in.watch
	if p.promoted != nil {
		background, notify, watch = true, true, nil
	}
	mountedHost := p.placement.HostCwd
	if host, _, ok := env.HostMount(e); ok && host != "" {
		mountedHost = host
	}
	if background {
		bg := backgroundSpawn{command: command, env: e, kind: kind, eff: eff, taskID: c.TaskID, sessionKey: sessionKey,
			workdir: workdir, cwd: cwd, mountedHost: mountedHost, pty: in.pty && !ptyDisabled, notify: notify, watch: watch,
			approvalNote: note, completionChars: in.completionChars, heartbeat: in.heartbeat, persist: in.persist,
			autoBackgrounded: p.autoBg != nil && p.promoted == nil}
		if ptyDisabled {
			bg.ptyNote = ptyDisabledReason
		}
		o := t.spawnBackground(ctx, c, bg)
		if p.promoted != nil {
			o = t.withPromotedNote(o, *p.promoted)
		}
		return t.metered(ctx, p, in.hostLocal, o.result(), ""), nil
	}
	result, err := t.runForeground(ctx, c, foreground{command: command, env: e, plan: p, sessionKey: sessionKey,
		workdir: workdir, approvalNote: note, metered: !in.hostLocal})
	if err != nil {
		return t.failure(ctx, c, p, in.hostLocal, err)
	}
	return t.metered(ctx, p, in.hostLocal, result, ""), nil
}

func (t *Terminal) metered(ctx context.Context, p *plan, hostLocal bool, r tool.Result, errorClass string) tool.Result {
	if p == nil || hostLocal {
		return r
	}
	t.o.Metrics.ExecutionBackend(ctx, "terminal", p.placement.Kind, r.Text, errorClass)
	return r
}

func (t *Terminal) failure(ctx context.Context, c *tool.Call, p *plan, hostLocal bool, err error) (tool.Result, error) {
	var conn *env.ConnectionError
	if errors.As(err, &conn) {
		res, derr := t.degraded(ctx, c, conn)
		if derr != nil {
			return tool.Result{}, derr
		}
		return t.metered(ctx, p, hostLocal, res, "tool_error"), nil
	}
	return t.fatal(ctx, p, hostLocal, err)
}

func (t *Terminal) fatal(ctx context.Context, p *plan, hostLocal bool, err error) (tool.Result, error) {
	var chain []string
	for e := err; e != nil; e = errors.Unwrap(e) {
		chain = append(chain, fmt.Sprintf("%T: %v", e, e))
	}
	trace := strings.Join(chain, "\n")
	slog.Error("terminal_tool exception:\n" + trace)
	res := object{
		{"output", ""},
		{"exit_code", -1},
		{"error", t.redactError(ctx, fmt.Sprintf("Failed to execute command: %v", err))},
		{"traceback", t.redactError(ctx, trace)},
		{"status", "error"},
	}.result()
	return t.metered(ctx, p, hostLocal, res, "exception"), nil
}

func (t *Terminal) degraded(ctx context.Context, c *tool.Call, e *env.ConnectionError) (tool.Result, error) {
	mode, _, err := t.o.Envs.Settings().Lookup(ctx, "TERMINAL_DEGRADED_MODE")
	if err != nil {
		return tool.Result{}, err
	}
	if mode == "" {
		mode = "warn"
	}
	if strings.ToLower(strings.TrimSpace(mode)) == "fail" {
		return t.fatal(ctx, nil, true, e)
	}
	slog.Warn(fmt.Sprintf("terminal backend degraded: %s", e.Reason))
	if err := t.o.Envs.Evict(ctx, c.TaskID); err != nil {
		slog.Debug("degraded-env eviction failed", "err", err)
	}
	return object{
		{"output", ""},
		{"exit_code", -1},
		{"status", "degraded"},
		{"reason", e.Reason},
		{"retry_hint", e.RetryHint},
		{"error", "Terminal backend degraded: " + e.Reason},
	}.result(), nil
}

func (t *Terminal) redactError(ctx context.Context, text string) string {
	return t.o.Redactor.Text(ctx, text, redact.Mode{Force: true})
}

func (t *Terminal) approvalGuards(ctx context.Context, c *tool.Call, command, kind string, hostAccess bool) (string, error) {
	if c.Approver == nil {
		return "", &tool.UnfilledError{Name: "tool.Approver"}
	}
	gctx := ctx
	if slot := t.slotFor(ctx, c); slot != nil {
		if err := slot.prep.Err(); err != nil {
			return "", err
		}
		gctx = t.withSlot(ctx, c, slot)
	}
	d := c.Approver.Command(gctx, c, tool.CommandCheck{Command: command, EnvKind: kind, HostAccess: hostAccess, SudoGuard: true})
	f := fieldMap(d)
	if !d.Approved {
		if s, _ := f["status"].(string); s == "pending_approval" {
			allow := any(true)
			if v, ok := f["allow_permanent"]; ok {
				allow = v
			}
			smartDenied := any(false)
			if v, ok := f["smart_denied"]; ok {
				smartDenied = v
			}
			return "", &rejected{object{
				{"output", ""},
				{"exit_code", -1},
				{"error", ""},
				{"status", "pending_approval"},
				{"approval_pending", true},
				{"command", getOr(f, "command", command)},
				{"description", getOr(f, "description", "command flagged")},
				{"pattern_key", getOr(f, "pattern_key", "")},
				{"smart_denied", smartDenied},
				{"allow_permanent", allow},
			}.result()}
		}
		desc := getOr(f, "description", "command flagged")
		message := getOr(f, "message", fmt.Sprintf("Command denied: %v. Use the approval prompt to allow it, or rephrase the command.", desc))
		body := object{{"output", ""}, {"exit_code", -1}, {"error", message}, {"status", "blocked"}}
		if s, ok := f["user_summary"]; ok && truthy(s) {
			body = append(body, field{"user_summary", s})
		}
		return "", &rejected{body.result()}
	}
	desc := getOr(f, "description", "flagged as dangerous")
	switch {
	case d.UserApproved:
		return fmt.Sprintf("Command required approval (%v) and was approved by the user.", desc), nil
	case d.SmartApproved:
		return fmt.Sprintf("Command was flagged (%v) and auto-approved by smart approval.", desc), nil
	}
	return "", nil
}

func fieldMap(d tool.Decision) map[string]any {
	out := map[string]any{}
	for _, f := range d.Fields() {
		out[f.Key] = f.Value
	}
	return out
}

func getOr(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

func errorJSON(msg string, exitCode int, status string, extra ...field) tool.Result {
	o := object{{"output", ""}, {"exit_code", exitCode}, {"error", msg}}
	if status != "" {
		o = append(o, field{"status", status})
	}
	return append(o, extra...).result()
}

func blockedJSON(msg, status string) tool.Result {
	return object{{"output", ""}, {"exit_code", 1}, {"error", msg}, {"status", status}}.result()
}

type field struct {
	key   string
	value any
}

type object []field

func (o object) set(key string, value any) object {
	for i := range o {
		if o[i].key == key {
			o[i].value = value
			return o
		}
	}
	return append(o, field{key, value})
}

func (o object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := fileutil.EncodeJSON(strings.ToValidUTF8(f.key, replacementChar), fileutil.JSONStyle{Tight: true})
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		v, err := fileutil.EncodeJSON(validUTF8(f.value), fileutil.JSONStyle{Tight: true})
		if err != nil {
			return nil, fmt.Errorf("terminal result field %q: %w", f.key, err)
		}
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func validUTF8(v any) any {
	switch x := v.(type) {
	case string:
		return strings.ToValidUTF8(x, replacementChar)
	case []string:
		out := make([]string, len(x))
		for i, s := range x {
			out[i] = strings.ToValidUTF8(s, replacementChar)
		}
		return out
	}
	return v
}

func (o object) result() tool.Result {
	data, err := fileutil.EncodeJSON(o, fileutil.JSONStyle{})
	if err != nil {
		return tool.Failure("Failed to encode the terminal result: " + err.Error())
	}
	return tool.Result{Text: string(data)}
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case int:
		return x != 0
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	case []any:
		return len(x) > 0
	case []string:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

func wholeNumber(v any) (int, bool) {
	f, ok := number(v)
	if !ok || f != math.Trunc(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return int(f), true
}

func formatNumber(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func typeName(v any) string {
	switch x := v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case float64:
		if x == math.Trunc(x) {
			return "int"
		}
		return "float"
	case int, json.Number:
		return "int"
	case []any, []string:
		return "list"
	case map[string]any:
		return "dict"
	}
	return fmt.Sprintf("%T", v)
}

func patternList(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			} else {
				out = append(out, fmt.Sprint(e))
			}
		}
		return out
	case string:
		out := make([]string, 0, len(x))
		for _, r := range x {
			out = append(out, string(r))
		}
		return out
	}
	return []string{fmt.Sprint(v)}
}

func requiresPipeStdin(command string) bool {
	normalized := strings.Join(strings.Fields(strings.ToLower(command)), " ")
	return strings.HasPrefix(normalized, "gh auth login") && strings.Contains(normalized, "--with-token")
}

type callKey struct{}

func withCall(ctx context.Context, c *tool.Call) context.Context {
	return context.WithValue(ctx, callKey{}, c)
}

func callFrom(ctx context.Context) *tool.Call {
	c, _ := ctx.Value(callKey{}).(*tool.Call)
	return c
}
