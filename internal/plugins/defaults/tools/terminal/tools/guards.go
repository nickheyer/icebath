package tools

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/approvals"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
)

// Least time the guards before a command get, whatever the command's own timeout
const guardFloor = 30 * time.Second

// Largest script the lifecycle guard reads through the backend
const scriptBytes = 1024 * 1024

// Result refusing the command by a guard that runs before it, empty when it may run: the server's lifecycle, the workdir, and where the
// command touches this machine's files, the checkout icebath runs from and the skills the user locked. The guards get the command's
// timeout, at least half a minute, and a guard that runs past it refuses the command
func (r *registrant) guard(ctx context.Context, call *tools.Call, lease *terminal.Lease, req request, command string, timeout time.Duration) string {
	limit := max(timeout, guardFloor)
	done := make(chan string, 1)
	go func() { done <- r.guards(ctx, call, lease, req, command) }()
	select {
	case refusal := <-done:
		return refusal
	case <-time.After(limit):
		slog.Warn(fmt.Sprintf("[deadline] %q timed out after %.1fs; worker abandoned", "terminal.pre-exec-guard", limit.Seconds()))
		return failure(fmt.Sprintf("Terminal pre-execution guard did not finish within %ds (process-identity probe wedged); the command was not "+
			"run. Retry the call.", int(limit.Seconds())), -1, "error")
	}
}

func (r *registrant) guards(ctx context.Context, call *tools.Call, lease *terminal.Lease, req request, command string) string {
	cfg := call.Env.Config
	cwd := lease.Folder(req.workdir)
	if refusal := r.approvals.Lifecycle(cfg, command, cwd, script(ctx, lease, cwd)); refusal != "" {
		return failure(refusal, 1, "error")
	}
	if req.workdir != "" {
		if refusal := validateWorkdir(req.workdir); refusal != "" {
			slog.Warn("Blocked dangerous workdir", "workdir", preview(req.workdir), "command", preview(command))
			return failure(refusal, -1, "blocked")
		}
	}
	if lease.Backend.Remote || lease.Backend.Container {
		return ""
	}
	if refusal := approvals.SelfRepo(cfg, command, cwd); refusal != "" {
		slog.Warn("Blocked self-repo git mutation", "command", preview(command))
		return failure(refusal, 1, "blocked")
	}
	if refusal := approvals.SkillLock(r.skills.Scope(cfg, call.Env.Cwd), command, cwd); refusal != "" {
		slog.Warn("Blocked write to a locked skill", "command", preview(command))
		return failure("Blocked: "+refusal, 1, "blocked")
	}
	return ""
}

// Reads a script the command runs through the session's backend, when it is not on this machine: its text, false when it can't be read
// or holds a NUL
func script(ctx context.Context, lease *terminal.Lease, cwd string) func(path string) (string, bool) {
	return func(path string) (string, bool) {
		res, err := lease.Env.Execute(ctx, fmt.Sprintf("head -c %d < %s", scriptBytes+1, terminal.Quote(path)), terminal.Exec{Cwd: cwd})
		if err != nil || res == nil || res.Code != 0 || strings.Contains(res.Output, "\x00") {
			return "", false
		}
		return res.Output, true
	}
}

// Result refusing the command when the approvals won't let it run, and the note of how it was let through after it was flagged
func (r *registrant) approve(ctx context.Context, call *tools.Call, lease *terminal.Lease, command string) (string, string) {
	if call.Env.Command == nil {
		return failure("Command not run: no approval system can check it in this session.", -1, "blocked"), ""
	}
	verdict := call.Env.Command(ctx, command, lease.Backend.Isolated)
	if refusal := verdict.Refusal; refusal != nil {
		return encode(result{ExitCode: ptr(-1), Error: &refusal.Message, Status: "blocked", UserSummary: refusal.Summary}), ""
	}
	// A message the user sent while the command waited for their approval is no reason to put the command aside
	if verdict.User && call.Env.Yield != nil {
		call.Env.Yield()
	}
	return "", verdict.Note
}

// Text cut short for the log
func preview(text string) string {
	if len(text) > 200 {
		return text[:200]
	}
	return text
}
