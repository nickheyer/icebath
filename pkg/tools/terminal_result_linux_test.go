//go:build linux

package tools

import (
	"context"
	"strings"
	"testing"
)

func TestLargeProcessOutputIsBoundedBeforeSudoAndPluginHooks(t *testing.T) {
	dir := t.TempDir()
	env := newTerminalTestEnv(t, dir)
	writeHomeConfig(t, env, "tool_output:\n  max_bytes: 10000\n")
	var hookInputs []string
	TerminalOutputTransformHook = func(_ context.Context, payload map[string]any) []any {
		hookInputs = append(hookInputs, payload["output"].(string))
		return nil
	}
	local := NewLocalEnvironment(context.Background(), env, dir, 10, nil, true)
	defer local.Cleanup(false)
	cacheFakeEnv("default", local)
	command := "python3 -c \"import sys; sys.stdout.write('HEAD-SENTINEL\\n' + 'x' * 2000000 + '\\nTAIL-SENTINEL')\""
	r := runTerminal(t, env, TerminalRequest{Command: command})
	if len(hookInputs) != 1 || len(hookInputs[0]) > 10000 || !strings.HasPrefix(hookInputs[0], "HEAD-SENTINEL") || !strings.HasSuffix(hookInputs[0], "TAIL-SENTINEL") || !strings.Contains(hookInputs[0], "[OUTPUT TRUNCATED") {
		t.Fatalf("hook input %d %q", len(hookInputs), truncateForLog(hookInputs))
	}
	if len(resultString(r, "output")) > 10000 {
		t.Fatal("result output over the limit")
	}
}
