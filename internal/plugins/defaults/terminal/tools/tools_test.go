//go:build unix

package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/internal/config"
	terminalconfig "github.com/nickheyer/icebath/internal/plugins/defaults/terminal/config"
	"github.com/nickheyer/icebath/internal/tools"
)

func TestTerminal(t *testing.T) {
	cfg := config.New(t.TempDir())
	host := tools.New(cfg)
	terminalconfig.Register(cfg)
	Register(&host.Registry)
	var asked []string
	var deny error
	env := &tools.Env{Cwd: t.TempDir(), Config: cfg, Approve: func(_ context.Context, action, reason string) error {
		asked = append(asked, action+" | "+reason)
		return deny
	}}
	run := func(arguments string) string {
		return host.Dispatch(t.Context(), &tools.Call{Name: "terminal", RawArgs: arguments, Env: env})
	}
	t.Setenv("ICEBATH_TEST_API_KEY", "secret")
	t.Setenv("ICEBATH_TEST_AUTH_TOKEN", "secret")
	t.Setenv("ICEBATH_TEST_PLAIN", "visible")

	if got, want := run(`{"command":"pwd; echo err >&2"}`), env.Cwd+"\nerr\n"; got != want {
		t.Fatalf("terminal = %q, want %q", got, want)
	}
	if got, want := run(`{"command":"echo out; exit 3"}`), "out\n\n[exit code 3]"; got != want {
		t.Fatalf("terminal with a failing command = %q, want %q", got, want)
	}
	if got, want := run(`{"command":"echo \"[$ICEBATH_TEST_API_KEY][$ICEBATH_TEST_AUTH_TOKEN][$ICEBATH_TEST_PLAIN]\""}`), "[][][visible]\n"; got != want {
		t.Fatalf("terminal environment = %q, want credentials held back: %q", got, want)
	}
	if err := cfg.Set(terminalconfig.EnvPassthrough, []string{"ICEBATH_TEST_API_KEY"}); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if got, want := run(`{"command":"echo \"[$ICEBATH_TEST_API_KEY][$ICEBATH_TEST_AUTH_TOKEN]\""}`), "[secret][]\n"; got != want {
		t.Fatalf("terminal environment with a passthrough = %q, want %q", got, want)
	}
	if got, want := run(`{"command":"sleep 5","timeout":1}`), "\n[timed out after 1s]"; got != want {
		t.Fatalf("terminal past its timeout = %q, want %q", got, want)
	}
	if got, want := run(`{"command":"rm -rf /"}`), `{"error":"blocked: deletes a system or home directory"}`; got != want {
		t.Fatalf("terminal with a blocked command = %q, want %q", got, want)
	}
	if len(asked) != 0 {
		t.Fatalf("asked = %v before any risky command", asked)
	}
	deny = errors.New("denied by the user")
	if got, want := run(`{"command":"git reset --hard"}`), `{"error":"denied by the user"}`; got != want {
		t.Fatalf("terminal with a denied command = %q, want %q", got, want)
	}
	deny = nil
	if got := run(`{"command":"echo sudo is only a word here"}`); got != "sudo is only a word here\n" {
		t.Fatalf("terminal with an approved command = %q", got)
	}
	if got, want := strings.Join(asked, "\n"), "git reset --hard | git reset --hard discards uncommitted changes\necho sudo is only a word here | runs as root"; got != want {
		t.Fatalf("asked = %q, want %q", got, want)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if got, want := host.Dispatch(cancelled, &tools.Call{Name: "terminal", RawArgs: `{"command":"sleep 5"}`, Env: env}), "\n[interrupted]"; got != want {
		t.Fatalf("terminal under a cancelled context = %q, want %q", got, want)
	}
}
