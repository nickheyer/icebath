package goal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/proc"
	"github.com/nickheyer/icebath/internal/tool"
	"github.com/nickheyer/icebath/internal/tool/env"
)

const gateOutputTailChars = 3000

var errGateTimeout = errors.New("gate timed out")

func (m *Managers) gateWorkspace(ctx context.Context) (cwd, refusal string, err error) {
	if declared := env.ScopedSessionCwd(ctx); declared != "" {
		path := home.ExpandUser(declared)
		if fi, err := os.Stat(path); err == nil && fi.IsDir() {
			return path, "", nil
		}
		return "", "the session workspace " + declared + " is not a directory on this host, " +
			"and running gates anywhere else would check a different project", nil
	}
	if m.o.Terminal == nil {
		return "", "", &tool.UnfilledError{Name: "goal.Options.Terminal"}
	}
	resolved, err := env.ResolveAgentCwd(ctx, m.o.Terminal)
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			return "", "", nil
		}
		return "", "", err
	}
	return resolved, "", nil
}

func RunGate(ctx context.Context, g Gate, cwd string) (passed bool, exitCode int, output string, err error) {
	cmd, err := proc.ShellCommand(g.Command)
	if err != nil {
		return false, -1, "[gate could not run: " + err.Error() + "]", nil
	}
	cmd.Dir = cwd
	runCtx, cancel := context.WithTimeoutCause(ctx, time.Duration(max(1, g.TimeoutSeconds))*time.Second, errGateTimeout)
	defer cancel()
	res, runErr := proc.Run(runCtx, cmd)
	if runErr != nil {
		if errors.Is(runErr, errGateTimeout) {
			partial := decodeOutput(res.Stdout) + decodeOutput(res.Stderr)
			return false, -1, tail(partial+fmt.Sprintf("\n[gate timed out after %ds]", g.TimeoutSeconds), gateOutputTailChars), nil
		}
		if ctx.Err() != nil {
			return false, -1, "", context.Cause(ctx)
		}
		return false, -1, "[gate could not run: " + runErr.Error() + "]", nil
	}
	stdout, stderr := universalNewlines(decodeOutput(res.Stdout)), universalNewlines(decodeOutput(res.Stderr))
	combined := stdout
	if stderr != "" {
		combined += "\n" + stderr
	}
	code := res.Exit.Status()
	return code == 0, code, tail(combined, gateOutputTailChars), nil
}

func decodeOutput(b []byte) string { return strings.ToValidUTF8(string(b), "�") }

func universalNewlines(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

func (g *Manager) checkGates(ctx context.Context) (*Decision, error) {
	st := g.state
	if st == nil || len(st.Gates) == 0 {
		return nil, nil
	}
	cwd, refusal, err := g.m.gateWorkspace(ctx)
	if err != nil {
		return nil, err
	}
	if refusal != "" {
		return g.pauseDecision(ctx, "quality gates not run: "+refusal, "gate_failed", "gates not run: "+refusal,
			"⏸ Goal paused — quality gates not run: "+refusal+". Fix the workspace or "+
				"/goal gate remove the gates, then /goal resume.")
	}
	for i := range st.Gates {
		gate := &st.Gates[i]
		passed, code, out, err := RunGate(ctx, *gate, cwd)
		if err != nil {
			return nil, err
		}
		gate.LastExitCode = &code
		gate.LastOutputTail = out
		if passed {
			gate.Attempts = 0
			continue
		}
		gate.Attempts++
		if gate.Attempts > gate.MaxRetries {
			return g.pauseDecision(ctx,
				fmt.Sprintf("quality gate exhausted %d retries: $ %s", gate.Attempts-1, gate.Command),
				"gate_failed", "gate exhausted retries: $ "+gate.Command,
				fmt.Sprintf("⏸ Goal paused — quality gate still failing after %d retries: $ %s (exit %d). "+
					"Fix it manually or /goal gate remove it, then /goal resume.", gate.MaxRetries, gate.Command, code))
		}
		if err := g.save(ctx); err != nil {
			return nil, err
		}
		shown := out
		if shown == "" {
			shown = "(no output)"
		}
		prompt := fill(continuationGateFailed,
			"{goal}", st.Goal, "{command}", gate.Command, "{exit_code}", fmt.Sprint(code),
			"{attempt}", fmt.Sprint(gate.Attempts), "{max_retries}", fmt.Sprint(gate.MaxRetries), "{output}", shown)
		return &Decision{
			Status: StatusActive, Continue: true, Prompt: prompt, Verdict: "gate_failed",
			Reason: fmt.Sprintf("gate failed (exit %d): $ %s", code, gate.Command),
			Message: fmt.Sprintf("✗ Quality gate failed (%d/%d turns, attempt %d/%d): $ %s",
				st.TurnsUsed, st.MaxTurns, gate.Attempts, gate.MaxRetries, gate.Command),
		}, nil
	}
	if err := g.save(ctx); err != nil {
		return nil, err
	}
	return nil, nil
}
