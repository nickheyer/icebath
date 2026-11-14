package selfupdate

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

func TestUpdateAndUpstreamNetworkCallsDisableTerminalPrompts(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	t.Setenv("GCM_INTERACTIVE", "Always")
	t.Setenv("GIT_ASKPASS", "fixture-askpass")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "credential.helper")
	t.Setenv("GIT_CONFIG_VALUE_0", "fixture-helper")
	savedRemote, savedCount, savedRun := upstreamRemotePresent, upstreamCommitCount, runInCustody
	t.Cleanup(func() { upstreamRemotePresent, upstreamCommitCount, runInCustody = savedRemote, savedCount, savedRun })
	upstreamRemotePresent = func(context.Context, []string, string) bool { return true }
	upstream := strings.Repeat("c", 40)
	upstreamCommitCount = func(_ context.Context, _ []string, _, _, head string) int {
		if head == upstream {
			return 2
		}
		return 0
	}
	type call struct {
		args []string
		opts ChildOptions
	}
	var calls []call
	runInCustody = func(_ context.Context, argv []string, _ bool, opts ChildOptions) (*CompletedProcess, error) {
		calls = append(calls, call{args: argv[1:], opts: opts})
		stdout := ""
		if slices.Contains(argv, "rev-parse") {
			stdout = upstream
		}
		return &CompletedProcess{Args: argv, Stdout: stdout}, nil
	}
	if err := os.Mkdir(filepath.Join(tmp, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := updateGitRun(ctx, []string{"git"}, []string{"fetch", "origin", "main"}, tmp, true, true); err != nil {
		t.Fatal(err)
	}
	checked, err := syncWithUpstreamIfNeeded(ctx, io.Discard, []string{"git"}, tmp, true, nil, nil)
	if err != nil || !checked {
		t.Fatalf("sync = %v, %v", checked, err)
	}
	syncForkWithUpstream(ctx, []string{"git"}, tmp)
	var network []call
	var subcommands []string
	for _, c := range calls {
		switch sub := updaterecord.GitSubcommand(c.args); sub {
		case "fetch", "pull", "push", "merge":
			network = append(network, c)
			subcommands = append(subcommands, sub)
		}
	}
	if want := []string{"fetch", "fetch", "merge", "push"}; !reflect.DeepEqual(subcommands, want) {
		t.Fatalf("network subcommands = %v, want %v", subcommands, want)
	}
	if mergeArgs := network[2].args; mergeArgs[len(mergeArgs)-1] != upstream {
		t.Fatalf("merge target = %v", mergeArgs)
	}
	for _, c := range network {
		if c.opts.Stdin != io.Reader(DevNull) {
			t.Fatalf("%v stdin is not DEVNULL", c.args)
		}
		env := map[string]string{}
		for _, entry := range c.opts.Env {
			if key, value, ok := strings.Cut(entry, "="); ok {
				env[key] = value
			}
		}
		for key, want := range map[string]string{"GIT_TERMINAL_PROMPT": "0", "GCM_INTERACTIVE": "Never", "GIT_ASKPASS": "fixture-askpass",
			"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_VALUE_0": "fixture-helper"} {
			if env[key] != want {
				t.Fatalf("%v env %s = %q, want %q", c.args, key, env[key], want)
			}
		}
	}
}
