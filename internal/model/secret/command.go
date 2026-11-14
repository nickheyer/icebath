package secret

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/proc"
)

// Variable the requested key reaches a helper through, never spliced into its command line
const EnvSecretKey = "ICEBATH_SECRET_KEY"

// Hard limits on one helper run
const (
	commandTimeoutSeconds = 3.0
	commandMaxOutput      = 1024 * 1024
)

// One KEY=VALUE line of helper output
var envLinePattern = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)

// Helper program the user configured, printing a KEY=VALUE blob in one run
type CommandSource struct{}

// Value with one layer of matching surrounding quotes removed
func unquoteEnvValue(raw string) string {
	t := strings.TrimSpace(raw)
	if len(t) >= 2 && t[0] == t[len(t)-1] && (t[0] == '"' || t[0] == '\'') {
		return t[1 : len(t)-1]
	}
	return t
}

// Writer keeping at most limit bytes and counting the rest
type cappedBuffer struct {
	data  []byte
	limit int
	over  bool
}

// Keeps bytes up to the limit and reports every byte as written
func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - len(b.data); room >= len(p) {
		b.data = append(b.data, p...)
	} else {
		if room > 0 {
			b.data = append(b.data, p[:room]...)
		}
		b.over = true
	}
	return len(p), nil
}

// Runs the helper through sh with the key as data, false for any failure, logging structured fields only
func runHelper(ctx context.Context, command, key string, env []string, timeout time.Duration) (string, bool) {
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Env = append(append([]string(nil), env...), EnvSecretKey+"="+key)
	out := &cappedBuffer{limit: commandMaxOutput}
	cmd.Stdout, cmd.Stderr = out, io.Discard
	res, err := proc.Run(bounded, cmd)
	switch {
	case err != nil && errors.Is(bounded.Err(), context.DeadlineExceeded):
		slog.Warn("secrets command helper timed out; resolving no value", "seconds", timeout.Seconds())
		return "", false
	case err != nil:
		slog.Warn("secrets command helper failed to spawn; resolving no value", "error", errorClass(err))
		return "", false
	case !res.Exit.Success():
		code, signal := strconv.Itoa(res.Exit.Code), "none"
		if res.Exit.Signal != 0 {
			code = "?"
			if name, ok := proc.SignalName(res.Exit.Signal); ok {
				signal = name
			} else {
				signal = strconv.Itoa(int(res.Exit.Signal))
			}
		}
		slog.Warn("secrets command helper failed; resolving no value", "code", code, "signal", signal)
		return "", false
	case out.over:
		slog.Warn("secrets command helper output exceeded its cap; resolving no value", "bytes", commandMaxOutput)
		return "", false
	}
	return strings.ToValidUTF8(string(out.data), "�"), true
}

// Kind of a spawn failure without the command line that failed
func errorClass(err error) string {
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return execErr.Err.Error()
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return "spawn failure"
}

// Assignments of a KEY=VALUE blob in order, comments and other lines skipped
func parseEnvBlob(stdout string) (map[string]string, []string) {
	values := map[string]string{}
	var order []string
	for _, raw := range strings.Split(strings.ReplaceAll(stdout, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := envLinePattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if _, seen := values[m[1]]; !seen {
			order = append(order, m[1])
		}
		values[m[1]] = unquoteEnvValue(m[2])
	}
	return values, order
}

// Section key and provenance label
func (*CommandSource) Name() string { return "command" }

// Words status lines use
func (*CommandSource) Label() string { return "Command helper" }

// Whole blob injected at once
func (*CommandSource) Shape() Shape { return Bulk }

// No reference scheme
func (*CommandSource) Scheme() string { return "" }

// Reports the master switch
func (*CommandSource) Enabled(cfg config.Raw) bool { return sectionOf(cfg).truthy("enabled", false) }

// Helper values replace existing ones only when asked
func (*CommandSource) Override(cfg config.Raw) bool {
	return sectionOf(cfg).truthy("override_existing", false)
}

// No bootstrap credential to guard
func (*CommandSource) Protected(config.Raw) []string { return nil }

// Budget of the whole fetch
func (*CommandSource) Timeout(cfg config.Raw) time.Duration { return fetchTimeout(cfg) }

// Next step for a failed fetch
func (*CommandSource) Remediation(kind ErrorKind, _ config.Raw) string {
	return remediationFor(kind, map[ErrorKind]string{
		NotConfigured: "Set secrets.command.command in config.yaml to a fast, non-interactive helper that prints KEY=VALUE lines.",
		Internal: "Run the helper manually in a shell to see its real error — icebath discards helper stderr so diagnostics " +
			"can't leak secret material.",
	}, "command", "")
}

// Runs the helper once with an empty key and parses its blob
func (*CommandSource) Fetch(ctx context.Context, in FetchInput) Result {
	cfg := sectionOf(in.Config)
	var result Result
	command := strings.TrimSpace(cfg.text("command"))
	if command == "" {
		return result.fail("secrets.command.enabled is true but secrets.command.command is empty.  Set the helper command in config.yaml.", NotConfigured)
	}
	if runtime.GOOS == "windows" {
		return result.fail("the 'command' secret source is POSIX-only (needs /bin/sh); skipping on Windows", NotConfigured)
	}
	seconds := cfg.float("helper_timeout_seconds", commandTimeoutSeconds)
	stdout, ok := runHelper(ctx, command, "", in.ChildEnv, time.Duration(seconds*float64(time.Second)))
	if !ok {
		return result.fail("helper command failed (see the logged fields); no secrets applied", Internal)
	}
	secrets, order := parseEnvBlob(stdout)
	if len(secrets) == 0 {
		result.Warnings = append(result.Warnings, "helper output was not a KEY=VALUE map; nothing to apply")
		return result
	}
	result.Secrets, result.Order = secrets, order
	return result
}
