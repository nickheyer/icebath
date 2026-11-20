package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const aliasQuickCommandsConfig = `quick_commands:
  gs:
    type: alias
    target: /status now
  hop:
    type: alias
    target: gs
  loopa:
    type: alias
    target: loopb
  loopb:
    type: alias
    target: loopa
  sh:
    type: exec
    command: echo hi
  bad:
    type: other
  blank:
    type: alias
    target: "  "
  slash:
    type: alias
    target: "/"
`

// Home whose config holds the alias fixtures, with a registry that knows only the status command
func installAliasQuickCommands(t *testing.T) context.Context {
	t.Helper()
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(aliasQuickCommandsConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	previous := ResolveRegistryCommand
	ResolveRegistryCommand = func(name string) (RegistryCommand, bool) {
		if name == "status" {
			return RegistryCommand{Name: "status"}, true
		}
		return RegistryCommand{}, false
	}
	t.Cleanup(func() { ResolveRegistryCommand = previous })
	return context.Background()
}

func TestResolveCommandAliasFollowsAliasesToARegistryCommand(t *testing.T) {
	ctx := installAliasQuickCommands(t)
	for _, tc := range []struct{ name, arg, wantName, wantArg string }{
		{"status", "x", "status", "x"},
		{"gs", "", "status", "now"},
		{"gs", "extra words", "status", "now extra words"},
		{"hop", "tail", "status", "now tail"},
		{"sh", "arg", "sh", "arg"},
		{"nothing-here", "arg", "nothing-here", "arg"},
	} {
		name, arg, err := resolveCommandAlias(ctx, tc.name, tc.arg)
		if err != nil || name != tc.wantName || arg != tc.wantArg {
			t.Fatalf("%s %q resolved to %q %q, %v", tc.name, tc.arg, name, arg, err)
		}
	}
}

func TestResolveCommandAliasRefusesBrokenAliases(t *testing.T) {
	ctx := installAliasQuickCommands(t)
	for name, reason := range map[string]string{"loopa": "invalid_params", "bad": "unsupported_command", "blank": "invalid_params"} {
		if _, _, err := resolveCommandAlias(ctx, name, ""); !isRuntimeRefusal(err, reason) {
			t.Fatalf("%s gave %v, want the %s refusal", name, err, reason)
		}
	}
	_, _, err := resolveCommandAlias(ctx, "slash", "")
	var raised *PyError
	if !errors.As(err, &raised) || raised.Kind != "IndexError" {
		t.Fatalf("a target of only a slash gave %v", err)
	}
}
