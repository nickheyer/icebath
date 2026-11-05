package tools

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// ErrCaptureTimeout reports a captured CLI child that ran past its deadline
var ErrCaptureTimeout = errors.New("command timed out")

// CaptureResult is a finished captured child process
type CaptureResult struct {
	Code   int
	Stdout string
	Stderr string
}

// Runs a CLI child with captured pipes and closed stdin, erroring on spawn failure, timeout, or check
func runCapture(ctx context.Context, argv []string, timeout time.Duration, check bool, env map[string]string) (*CaptureResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	if env != nil {
		cmd.Env = mapToEnviron(env)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = time.Second
	hideConsoleWindow(cmd)
	proc.Group(cmd)
	cmd.Cancel = func() error { return proc.Kill(cmd) }
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	err := cmd.Wait()
	if runCtx.Err() != nil {
		return nil, ErrCaptureTimeout
	}
	res := &CaptureResult{Stdout: strings.ToValidUTF8(stdout.String(), string(rune(0xFFFD))), Stderr: strings.ToValidUTF8(stderr.String(), string(rune(0xFFFD)))}
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return nil, err
		}
		res.Code = ee.ExitCode()
	}
	if check && res.Code != 0 {
		return res, &CaptureExitError{Argv: argv, Code: res.Code, Stderr: res.Stderr}
	}
	return res, nil
}

// CaptureExitError is a checked capture that exited nonzero
type CaptureExitError struct {
	Argv   []string
	Code   int
	Stderr string
}

func (e *CaptureExitError) Error() string {
	return "command " + shJoin(e.Argv) + " returned non-zero exit status " + itoa(e.Code) + ": " + strings.TrimSpace(e.Stderr)
}

// Values of the profile .env file, empty when it cannot be read
func loadIcebathEnvVars(env *Env) map[string]string {
	if env == nil || env.Secrets == nil {
		return map[string]string{}
	}
	return env.Secrets.File()
}

// Values to forward into a remote shell plus the scoped names that must be unset there
func resolvePassthroughEnv(env *Env, explicitForward []string) (map[string]string, map[string]bool, error) {
	multiplex := isMultiplexActive(env)
	forward := map[string]bool{}
	for _, k := range explicitForward {
		forward[k] = true
	}
	for k := range AllPassthrough(env) {
		if IsIcebathInternalSecret(k) || IsProviderEnvBlocklisted(env, k) {
			continue
		}
		forward[k] = true
	}
	execEnv := map[string]string{}
	unset := map[string]bool{}
	if len(forward) == 0 {
		return execEnv, unset, nil
	}
	dotenv := loadIcebathEnvVars(env)
	keys := make([]string, 0, len(forward))
	for k := range forward {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		var fallback *string
		if v, ok := os.LookupEnv(key); ok && v != "" {
			fallback = &v
		} else if v, ok := dotenv[key]; ok {
			fallback = &v
		}
		value, err := ResolvePassthroughValue(env, key, fallback)
		if err != nil {
			return nil, nil, err
		}
		if value != nil {
			execEnv[key] = *value
		} else if multiplex && !IsGlobalEnv(key) && shellEnvNameRe.MatchString(key) {
			unset[key] = true
		}
	}
	return execEnv, unset, nil
}

// Prefixes a command with unset of the profile scoped names the remote shell must not see
func prependUnset(cmd string, names map[string]bool) string {
	if len(names) == 0 {
		return cmd
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	quoted := make([]string, len(sorted))
	for i, n := range sorted {
		quoted[i] = shQuote(n)
	}
	return "unset " + strings.Join(quoted, " ") + " 2>/dev/null || true\n" + cmd
}

// Env for a docker or ssh client child carrying forwarded values, nil to inherit when empty
func clientEnvWith(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := environMap()
	for k, v := range values {
		out[k] = v
	}
	return out
}

// The bash -c argv tail every spawn per call backend uses
func bashArgv(cmd string, login bool) []string {
	if login {
		return []string{"bash", "-l", "-c", cmd}
	}
	return []string{"bash", "-c", cmd}
}

func sortedNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
