package terminal

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/tool"
	"github.com/nickheyer/icebath/internal/tool/env"
)

const envRecreatedNote = "The execution environment was recreated while running this command " +
	"(the previous container/sandbox was gone). Persistent files were restored " +
	"where the backend supports it, but background processes and any files " +
	"outside persisted paths from earlier commands may be lost — verify state " +
	"before relying on prior work."

const foregroundRetries = 3

type foreground struct {
	command      string
	env          tool.Environment
	plan         *plan
	sessionKey   string
	workdir      string
	approvalNote string
	metered      bool
}

func seconds(f float64) time.Duration {
	return time.Duration(f * float64(time.Second))
}

func (t *Terminal) runForeground(ctx context.Context, c *tool.Call, f foreground) (tool.Result, error) {
	kind, eff, timeout := f.plan.placement.Kind, f.plan.placement.Key, f.plan.timeout
	mountedHost := f.plan.placement.HostCwd
	if host, _, ok := env.HostMount(f.env); ok && host != "" {
		mountedHost = host
	}
	var res tool.ExecResult
	var commandCwd string
	for retry := 0; ; retry++ {
		commandCwd = t.resolveCommandCwd(ctx, f.workdir, f.plan.placement.Cwd, f.sessionKey, kind, mountedHost, f.env)
		opts := tool.ExecOptions{Cwd: commandCwd, Timeout: seconds(timeout), BoundedCapture: true, RewriteCompoundBackground: true,
			Yield: c.Yield, Activity: c.Activity}
		if kind == "local" {
			opts.OnYield = t.yieldHandler(ctx, c, f.command, commandCwd, eff, f.sessionKey)
		}
		r, err := f.env.Execute(ctx, f.command, opts)
		if err == nil {
			res = r
			break
		}
		if strings.Contains(strings.ToLower(err.Error()), "timeout") {
			return errorJSON(fmt.Sprintf("Command timed out after %s seconds", formatNumber(timeout)), 124, ""), nil
		}
		if retry < foregroundRetries {
			wait := time.Duration(1<<(retry+1)) * time.Second
			slog.Warn(fmt.Sprintf("Execution error, retrying in %ds (attempt %d/%d) - Command: %s - Error: %T: %v - Task: %s, Backend: %s",
				int(wait/time.Second), retry+1, foregroundRetries, commandPreview(f.command), err, err, eff, kind))
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
			}
			continue
		}
		slog.Error(fmt.Sprintf("Execution failed after %d retries - Command: %s - Error: %T: %v - Task: %s, Backend: %s",
			foregroundRetries, commandPreview(f.command), err, err, eff, kind))
		return errorJSON(t.redactError(ctx, fmt.Sprintf("Command execution failed: %T: %v", err, err)), -1, ""), nil
	}
	if res.YieldedSession != "" {
		return object{
			{"output", res.Output},
			{"exit_code", nil},
			{"error", nil},
			{"status", "yielded_to_background"},
			{"session_id", res.YieldedSession},
			{"pid", res.YieldedPID},
			{"notify_on_complete", true},
			{"note", yieldedNote},
		}.result(), nil
	}
	if f.metered {
		t.o.Metrics.TerminalOutcome(ctx, f.command, kind, res)
	}
	return t.finalize(ctx, c, f, res, commandCwd)
}

func (t *Terminal) finalize(ctx context.Context, c *tool.Call, f foreground, res tool.ExecResult, commandCwd string) (tool.Result, error) {
	kind, eff := f.plan.placement.Kind, f.plan.placement.Key
	observed := ""
	if res.CwdObserved {
		observed = res.Cwd
		if observed == "" {
			observed = f.env.Cwd()
		}
	}
	if f.workdir == "" && observed != "" {
		t.o.Envs.RecordSessionCwd(ctx, f.sessionKey, observed)
	}
	code := res.ExitCode
	output, authFailed, cacheCleared, err := t.o.Sudo.annotate(ctx, c, f.command, res.Output)
	if err != nil {
		return tool.Result{}, err
	}
	output = t.transformOutput(ctx, c, f.command, output, code, eff, kind)
	limits := tool.OutputLimits{}.Resolved()
	if cfg, err := t.o.Config.Load(ctx); err == nil {
		limits = tool.Limits(cfg).Resolved()
	} else {
		slog.Debug("tool output limits read failed, using defaults", "err", err)
	}
	output = tool.TruncateHeadTail(output, limits.MaxBytes, "")
	output = tool.StripANSI(output)
	if output != "" {
		output = t.o.Redactor.Terminal(ctx, strings.TrimSpace(output), f.command, false)
	}
	exitNote := interpretExitCode(f.command, code)
	hint := failureHint(f.command, code, output, exitNote)
	changed := ""
	if observed != "" && commandCwd != "" && hostRealpath(observed) != hostRealpath(commandCwd) {
		changed = observed
	}
	note := f.approvalNote
	if note != "" && code == 130 && strings.Contains(output, "[Command interrupted]") {
		note = strings.TrimRight(note, ".") + ", then interrupted."
	}
	o := object{{"output", output}, {"exit_code", code}, {"error", nil}}
	if changed != "" {
		o = append(o, field{"cwd", changed})
	}
	if res.Recreated {
		o = append(o, field{"environment_recreated", envRecreatedNote})
	}
	o = append(o, t.redactSpill(ctx, res.SpillPath, res.OutputTotalChars, f.command)...)
	sessionID := c.SessionID
	for _, candidate := range []string{c.TaskID, eff, "default"} {
		if sessionID == "" {
			sessionID = candidate
		}
	}
	if ev, ok := t.evidence(ctx, CommandRun{Command: f.command, Cwd: commandCwd, SessionID: sessionID, ExitCode: code, Output: output, PipelineStatus: res.PipelineStatus}); ok {
		o = append(o, field{"verification_evidence", ev})
	}
	if note != "" {
		o = append(o, field{"approval", note})
	}
	if exitNote != "" {
		o = append(o, field{"exit_code_meaning", exitNote})
	}
	if hint != "" {
		o = append(o, field{"hint", hint})
	}
	if authFailed {
		o = append(o, field{"sudo_auth_failed", true})
	}
	if cacheCleared {
		o = append(o, field{"sudo_cache_cleared", true})
	}
	r := o.result()
	r.Recreated = res.Recreated
	return r, nil
}

func (t *Terminal) evidence(ctx context.Context, r CommandRun) (object, bool) {
	ev, ok, err := t.o.Evidence.RecordTerminal(ctx, r)
	if err != nil {
		slog.Debug("verification evidence recording failed", "err", err)
		return nil, false
	}
	if !ok {
		return nil, false
	}
	return object{{"status", ev.Status}, {"kind", ev.Kind}, {"scope", ev.Scope}, {"canonical_command", ev.CanonicalCommand}}, true
}

func (t *Terminal) transformOutput(ctx context.Context, c *tool.Call, command, output string, code int, taskID, kind string) string {
	if replaced, ok := t.o.Transform.Transform(ctx, TransformRequest{Command: command, Output: output, ExitCode: code, TaskID: taskID, EnvType: kind, ToolCallID: c.ID}); ok {
		return replaced
	}
	return output
}

func (t *Terminal) redactSpill(ctx context.Context, path string, total int, command string) object {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err == nil {
		text := strings.TrimPrefix(strings.ToValidUTF8(string(data), replacementChar), byteOrderMark)
		text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
		err = tool.WriteExclusive(path, t.o.Redactor.Terminal(ctx, tool.StripANSI(text), command, false), true, true)
	}
	if err != nil {
		slog.Debug("spill redaction failed; dropping spill handle", "err", err)
		if rmErr := os.Remove(path); rmErr != nil {
			slog.Debug("spill unlink failed", "err", rmErr)
		}
		return nil
	}
	note := "Output exceeded the capture window (head+tail shown). " +
		fmt.Sprintf("Full output (%s chars) saved to %s — search it with ", groupThousands(total), path) +
		"search_files or page it with read_file instead of re-running the command."
	return object{{"output_total_chars", total}, {"full_output_path", path}, {"truncation_note", note}}
}

func groupThousands(n int) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func hostRealpath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return resolveLenient(abs)
}

func resolveLenient(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	rest := ""
	dir := abs
	for {
		parent := filepath.Dir(dir)
		rest = filepath.Join(filepath.Base(dir), rest)
		if parent == dir {
			return abs
		}
		dir = parent
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, rest)
		}
	}
}

func commandPreview(command string) string {
	return firstRunes(command, 200)
}

func firstRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func (t *Terminal) mountEnvs(e tool.Environment) []tool.Environment {
	if e != nil {
		return []tool.Environment{e}
	}
	var out []tool.Environment
	for _, sh := range t.o.Envs.Environments() {
		out = append(out, sh)
	}
	return out
}

func rewriteViaEnvMount(p string, e tool.Environment) (string, bool) {
	if e == nil || p == "" {
		return "", false
	}
	host, mount, ok := env.HostMount(e)
	if !ok || host == "" || mount == "" {
		return "", false
	}
	return env.TranslateMountedHostPath(p, host, mount)
}

func (t *Terminal) containerVisibleCwd(ctx context.Context, p, kind string, e tool.Environment) string {
	if p == "" || !t.o.Envs.Settings().IsContainerBackend(ctx, kind) {
		return p
	}
	for _, item := range t.mountEnvs(e) {
		if translated, ok := rewriteViaEnvMount(p, item); ok && translated != "" {
			return translated
		}
	}
	return p
}

func (t *Terminal) containerVisibleDefault(ctx context.Context, defaultCwd, kind string, e tool.Environment) string {
	if !t.o.Envs.Settings().IsContainerBackend(ctx, kind) {
		return defaultCwd
	}
	for _, item := range t.mountEnvs(e) {
		host, mount, _ := env.HostMount(item)
		if mount == "" || host == "" || mount == defaultCwd {
			continue
		}
		if defaultCwd == "/workspace" || env.IsUnusableContainerCwd(defaultCwd, "") {
			return mount
		}
	}
	return defaultCwd
}

type sandboxHomed interface {
	SandboxHome() (string, bool)
}

func inSandboxHome(cwd string, e tool.Environment) bool {
	sh, ok := e.(sandboxHomed)
	if !ok || e == nil {
		return false
	}
	h, ok := sh.SandboxHome()
	return ok && env.InSandboxHome(cwd, h)
}

func mountedHostCwd(cwd, mountedHost string) bool {
	if cwd == "" || mountedHost == "" {
		return false
	}
	return filepath.Clean(absPath(home.ExpandUser(cwd))) == filepath.Clean(absPath(home.ExpandUser(mountedHost)))
}

func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

func (t *Terminal) resolveCommandCwd(ctx context.Context, workdir, defaultCwd, sessionKey, kind, mountedHost string, e tool.Environment) string {
	settings := t.o.Envs.Settings()
	if workdir != "" {
		return settings.CoerceSSHRemoteCwd(ctx, t.containerVisibleCwd(ctx, workdir, kind, e), kind)
	}
	recorded, _ := t.o.Envs.SessionCwd(ctx, sessionKey)
	if recorded != "" && settings.IsContainerBackend(ctx, kind) && env.IsUnusableContainerCwd(recorded, mountedHost) && !inSandboxHome(recorded, e) {
		if visible := t.containerVisibleCwd(ctx, recorded, kind, e); visible != recorded {
			return visible
		}
		if mountedHostCwd(recorded, mountedHost) {
			slog.Info(fmt.Sprintf("Remapping recorded session cwd %q for %s backend (mounted host directory). Using '/workspace' instead.", recorded, kind))
			return "/workspace"
		}
		slog.Info(fmt.Sprintf("Ignoring recorded session cwd %q for %s backend (host/relative path won't work in sandbox). Using %q instead.", recorded, kind, defaultCwd))
		return t.containerVisibleDefault(ctx, defaultCwd, kind, e)
	}
	base := recorded
	if base == "" {
		base = t.containerVisibleDefault(ctx, defaultCwd, kind, e)
	}
	return settings.CoerceSSHRemoteCwd(ctx, base, kind)
}
