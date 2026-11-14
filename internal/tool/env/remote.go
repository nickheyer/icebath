package env

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/proc"
	"github.com/nickheyer/icebath/internal/shell"
)

func (h *Host) resolvePassthroughEnv(ctx context.Context, explicitForward []string) (map[string]string, []string, error) {
	passthrough, err := h.passthrough.All(ctx)
	if err != nil {
		slog.Debug("passthrough registry unavailable for the remote shell", "err", err)
		passthrough = nil
	}
	multiplex := h.secrets.Multiplexed(ctx)
	forward := map[string]bool{}
	for _, k := range explicitForward {
		forward[k] = true
	}
	var registered map[string]bool
	for _, k := range passthrough {
		if IsInternalSecret(k) {
			continue
		}
		if registered == nil {
			if registered, err = h.policy.RegisteredAdapterSecrets(ctx); err != nil {
				return nil, nil, err
			}
		}
		blocked, err := h.policy.ProviderBlocklisted(ctx, k, registered)
		if err != nil {
			return nil, nil, err
		}
		if !blocked {
			forward[k] = true
		}
	}
	fileEnv := map[string]string{}
	if len(forward) > 0 {
		if loaded, err := h.cfg.Env().File(ctx); err == nil {
			fileEnv = loaded
		}
	}
	keys := make([]string, 0, len(forward))
	for k := range forward {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	values := map[string]string{}
	var unset []string
	for _, key := range keys {
		var value *string
		if v := os.Getenv(key); v != "" {
			value = &v
		} else if v, ok := fileEnv[key]; ok {
			value = &v
		}
		resolved, err := h.passthrough.Resolve(ctx, key, value)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case resolved != nil:
			values[key] = *resolved
		case multiplex && !config.IsGlobalEnv(key) && ValidShellEnvName(key):
			unset = append(unset, key)
		}
	}
	return values, unset, nil
}

func prependUnset(command string, names []string) string {
	if len(names) == 0 {
		return command
	}
	sorted := append([]string{}, names...)
	sort.Strings(sorted)
	quoted := make([]string, len(sorted))
	for i, n := range sorted {
		quoted[i] = shell.Quote(n)
	}
	return "unset " + strings.Join(quoted, " ") + " 2>/dev/null || true\n" + command
}

func clientEnvWith(values map[string]string) []string {
	if len(values) == 0 {
		return nil
	}
	merged := Environ()
	for k, v := range values {
		merged[k] = v
	}
	return EnvList(merged)
}

type captured struct {
	code   int
	stdout string
	stderr string
}

func runCapture(ctx context.Context, argv []string, timeout time.Duration, env []string) (captured, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(rctx, argv[0], argv[1:]...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = streamWaitDelay
	proc.Hide(cmd)
	err := cmd.Run()
	if rctx.Err() != nil {
		if ctx.Err() != nil {
			return captured{}, ctx.Err()
		}
		return captured{}, &RunTimeoutError{Argv: argv, Timeout: timeout}
	}
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return captured{}, err
		}
	}
	return captured{code: proc.ExitOf(cmd.ProcessState).Status(), stdout: decodeLossy(stdout.Bytes()), stderr: decodeLossy(stderr.Bytes())}, nil
}

func bashArgv(command string, login bool) []string {
	if login {
		return []string{"bash", "-l", "-c", command}
	}
	return []string{"bash", "-c", command}
}
