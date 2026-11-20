package main

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/app"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Console engine over the process wiring with an isolated home
func consoleEngineForTest(t *testing.T) *app.ConsoleEngine {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	installProcessWiring()
	return app.NewConsoleEngine(context.Background(), 20000)
}

func TestExecuteHandlerStringExitReturnsErrorNotCrash(t *testing.T) {
	engine := consoleEngineForTest(t)
	result, err := engine.Execute("auth remove openrouter __no_such_credential__", true)
	if err != nil {
		t.Fatalf("Execute escaped with %v", err)
	}
	if result.Status != app.ConsoleError || result.Output == "" {
		t.Fatalf("result %+v", result)
	}
}

func TestConsoleCheckpointsPruneDoesNotRepromptForOrphans(t *testing.T) {
	engine := consoleEngineForTest(t)
	var pruneCalls []tools.CheckpointPruneOptions
	status := func(*tools.Env, string) (tools.CheckpointStatus, error) {
		return tools.CheckpointStatus{Projects: []tools.CheckpointProjectStatus{
			{Hash: "abc123", Workdir: "/gone/v2-project", Exists: false, Commits: 4}}}, nil
	}
	prune := func(_ *tools.Env, o tools.CheckpointPruneOptions) (tools.CheckpointPruneResult, error) {
		pruneCalls = append(pruneCalls, o)
		return tools.CheckpointPruneResult{Scanned: 1, DeletedOrphan: 1}, nil
	}
	input := func(string) (string, error) {
		t.Fatal("input() must not be called: the console already confirmed `checkpoints prune`")
		return "", nil
	}
	t.Cleanup(tools.SwapCheckpointsCommandFns(status, prune, input))
	result, err := engine.Execute("checkpoints prune", true)
	if err != nil {
		t.Fatalf("Execute escaped with %v", err)
	}
	if result.Status != app.ConsoleOK {
		t.Fatalf("result %+v", result)
	}
	if len(pruneCalls) != 1 || !pruneCalls[0].DeleteOrphans || pruneCalls[0].OrphanAllowlist != nil {
		t.Fatalf("prune calls %+v", pruneCalls)
	}
}
