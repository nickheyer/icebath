// A helper command of the user's own as a store of a profile's secrets: run through /bin/sh, its KEY=VALUE lines taken as variables
package secrets

import (
	"context"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cast"

	commandconfig "github.com/nickheyer/icebath/internal/plugins/defaults/secrets/command/config"
	"github.com/nickheyer/icebath/internal/secrets"
)

const (
	// Longest a helper may run unless its settings say otherwise; tight on purpose, a helper is fast and asks nothing
	helperTimeout = 3 * time.Second
	// Most a helper may print
	maxOutput = 1 << 20
)

// One NAME=value line, anchored so a blob of several never matches
var line = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)

type source struct{}

func Register(registry *secrets.Registry) {
	registry.Register(secrets.Sources{source{}})
}

func (source) Spec() secrets.Spec {
	return secrets.Spec{Name: commandconfig.Name, Label: "Your own command", Short: "your helper command", Shape: secrets.Bulk,
		Hints: map[secrets.Failure]string{
			secrets.NotConfigured: "Give it a quick command that asks nothing and prints NAME=value lines, with icebath config set secrets.command.command.",
			secrets.Internal: "Run the command yourself in a shell to see its error. icebath never shows the errors it prints, so no secret can " +
				"leak that way.",
		}}
}

// What keeps the command from filling anything: no command, or no /bin/sh to run it with
func (source) Needs(_ context.Context, f secrets.Fetch) (string, string) {
	switch {
	case strings.TrimSpace(f.Config.GetString(commandconfig.Command)) == "":
		return "Needs a command", "No command is set yet."
	case runtime.GOOS == "windows":
		return "Can't run here", "Your own command needs /bin/sh, which Windows does not have."
	}
	return "", ""
}

// Value of a line without one layer of matching quotes
func unquote(raw string) string {
	t := strings.TrimSpace(raw)
	if len(t) >= 2 && t[0] == t[len(t)-1] && (t[0] == '"' || t[0] == '\'') {
		return t[1 : len(t)-1]
	}
	return t
}

// Variables of the helper's KEY=VALUE lines, blank lines, comments and every other line left out
func parse(out string) map[string]string {
	found := map[string]string{}
	for raw := range strings.Lines(strings.ReplaceAll(out, "\r\n", "\n")) {
		text := strings.TrimSpace(raw)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if m := line.FindStringSubmatch(text); m != nil {
			found[m[1]] = unquote(m[2])
		}
	}
	return found
}

func (source) Fetch(ctx context.Context, f secrets.Fetch) secrets.Result {
	command := strings.TrimSpace(f.Config.GetString(commandconfig.Command))
	if command == "" {
		return secrets.Failed("Your own command is on, but no helper command is set.", secrets.NotConfigured)
	}
	if runtime.GOOS == "windows" {
		return secrets.Failed("Your own command needs /bin/sh, which Windows does not have, so icebath skipped it.", secrets.NotConfigured)
	}
	timeout := helperTimeout
	if seconds, err := cast.ToFloat64E(f.Config.Get(commandconfig.Helper)); err == nil && seconds > 0 {
		timeout = time.Duration(seconds * float64(time.Second))
	}
	out, ok := helper(ctx, command, timeout, f.Env)
	if !ok {
		return secrets.Failed("Your helper command failed, so it set no variables. The log says how it ended.", secrets.Internal)
	}
	found := parse(out)
	if len(found) == 0 {
		return secrets.Result{Secrets: map[string]string{}, Warnings: []string{"Your helper command printed no NAME=value lines, so nothing was set."}}
	}
	return secrets.Result{Secrets: found}
}
