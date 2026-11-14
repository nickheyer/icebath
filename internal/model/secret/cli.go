package secret

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/proc"
)

// Output and exit code of one finished helper program
type CLIResult struct {
	Stdout string
	Stderr string
	Code   int
}

// Variables every helper program receives when the process has them
var baseCLIEnv = []string{"PATH", "HOME", "USERPROFILE", "SYSTEMROOT", "TMPDIR", "TEMP", "LANG", "LC_ALL", "XDG_CONFIG_HOME", "XDG_DATA_HOME"}

// Environment map as the sorted assignment list a child process takes
func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for name, value := range env {
		out = append(out, name+"="+value)
	}
	sort.Strings(out)
	return out
}

// Assignment list as a map, a later assignment of one name winning
func envMap(list []string) map[string]string {
	out := make(map[string]string, len(list))
	for _, kv := range list {
		if name, value, ok := strings.Cut(kv, "="); ok && name != "" {
			out[name] = value
		}
	}
	return out
}

// Runs argv directly with env, the output decoded as text, a deadline or a failed start returned as an error
func runCLI(ctx context.Context, argv, env []string, timeout time.Duration, label, timeoutMessage string, stdin *os.File) (CLIResult, error) {
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	if env == nil {
		cmd.Env = []string{}
	}
	if stdin != nil {
		cmd.Stdin = stdin
	}
	res, err := proc.Run(bounded, cmd)
	switch {
	case err != nil && ctx.Err() != nil:
		return CLIResult{}, ctx.Err()
	case err != nil && errors.Is(bounded.Err(), context.DeadlineExceeded):
		return CLIResult{}, errors.New(timeoutMessage)
	case err != nil:
		return CLIResult{}, fmt.Errorf("failed to invoke %s: %v", label, err)
	}
	return CLIResult{
		Stdout: strings.ToValidUTF8(string(res.Stdout), "�"),
		Stderr: strings.ToValidUTF8(string(res.Stderr), "�"),
		Code:   res.Exit.Status(),
	}, nil
}

// Runs a secret manager helper with basic, allowed and extra variables only, its stderr scrubbed of escapes
func RunCLI(ctx context.Context, argv, allowEnv []string, extraEnv map[string]string, timeout time.Duration) (CLIResult, error) {
	env := map[string]string{}
	for _, name := range append(append([]string(nil), baseCLIEnv...), allowEnv...) {
		if v, ok := os.LookupEnv(name); ok {
			env[name] = v
		}
	}
	for name, value := range extraEnv {
		env[name] = value
	}
	if _, ok := env["NO_COLOR"]; !ok {
		env["NO_COLOR"] = "1"
	}
	if timeout <= 0 {
		timeout = DefaultCLITimeout
	}
	name := filepath.Base(argv[0])
	res, err := runCLI(ctx, argv, envList(env), timeout, name, fmt.Sprintf("%s timed out after %.0fs", name, timeout.Seconds()), nil)
	if err != nil {
		return CLIResult{}, err
	}
	res.Stderr = ScrubANSI(res.Stderr)
	return res, nil
}
