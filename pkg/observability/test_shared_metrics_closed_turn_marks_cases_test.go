package observability

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/tools"
)

// The relay_home fixture with the core command registry wired as pkg/commands wires it, holding turns in its context
func closedTurnMarksHome(t *testing.T) context.Context {
	t.Helper()
	_, ctx := directRuntime(t)
	catalogMu.Lock()
	savedSource, hadSource := catalogSources["slash_command_names"]
	catalogMu.Unlock()
	savedResolve := ResolveCommandHook
	SetCatalogSource("slash_command_names", func() ([]string, error) { return []string{"help"}, nil })
	ResolveCommandHook = func(name string) (string, bool) { return name, name == "help" }
	t.Cleanup(func() {
		ResolveCommandHook = savedResolve
		if hadSource {
			SetCatalogSource("slash_command_names", savedSource)
			return
		}
		catalogMu.Lock()
		delete(catalogSources, "slash_command_names")
		delete(catalogCache, "slash_command_names")
		catalogMu.Unlock()
	})
	return pyctx.With(ctx, pyctx.New())
}

// A slash command and a title generation usage event, the process level marks of background work
func emitClosedTurnProcessMarks(ctx context.Context) {
	RecordSlashCommand(ctx, "help", "cli")
	ObserveLifecycle(ctx, nil, "post_auxiliary_call", map[string]any{
		"aux_task": "title_generation", "model": "google/gemini-2.5-flash", "provider": "openrouter",
		"usage": map[string]any{"input_tokens": 1000, "output_tokens": 10},
	})
}

// Help slash command count and title generation input tokens persisted once the runtimes shut down
func closedTurnRecorded(t *testing.T, ctx context.Context) map[string]int64 {
	t.Helper()
	ShutdownRuntimes(ctx)
	found := map[string]int64{"slash": 0, "aux_input": 0}
	for _, row := range storedRows(t, ctx) {
		dims := dimsOf(row.Dimensions)
		if row.MetricName == SlashCommandMetric && dims["command"] == "help" {
			found["slash"] += row.Value
		} else if row.MetricName == ModelTokensMetric && dims["token_type"] == "input" {
			found["aux_input"] += row.Value
		}
	}
	return found
}

// Emits the marks on a goroutine holding spawned, only after beforeEmit ran on the test goroutine
func closedTurnRunAfter(t *testing.T, spawned context.Context, beforeEmit func()) {
	t.Helper()
	gate, released, finished := make(chan struct{}), make(chan bool, 1), make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-gate:
			released <- true
		case <-time.After(30 * time.Second):
			released <- false
		}
		emitClosedTurnProcessMarks(spawned)
	}()
	beforeEmit()
	close(gate)
	select {
	case <-finished:
	case <-time.After(40 * time.Second):
		t.Fatal("the mark goroutine is still alive")
	}
	if !<-released {
		t.Fatal("the marks must be emitted only after before_emit ran")
	}
}

func TestMarksFromAThreadThatOutlivesItsTurnAreRecorded(t *testing.T) {
	ctx := closedTurnMarksHome(t)
	coordinator := tools.RelayCoordinator
	lease := coordinator.AcquireConversation(ctx, tools.RelayConversation{ProfileKey: tools.CurrentRelayProfileKey(ctx), SessionID: "s1", Platform: "cli"})
	turn, err := coordinator.BeginTurn(ctx, lease, "turn-1", "t1", nil)
	if err != nil {
		t.Fatal(err)
	}
	StartTaskRun(ctx, nil, "s1", "t1", "cli", "")
	spawnedInTurn := pyctx.With(ctx, pyctx.CopyContext(ctx))
	closedTurnRunAfter(t, spawnedInTurn, func() {
		FinishTaskRun(ctx, "s1", "t1", "cli", map[string]any{"completed": true}, nil)
		coordinator.EndTurn(ctx, turn, "success")
		coordinator.ReleaseConversation(lease)
	})
	if tools.CurrentRelayTurn(spawnedInTurn) != turn || !turn.Closed() {
		t.Fatalf("spawned context holds %v, closed %v", tools.CurrentRelayTurn(spawnedInTurn), turn.Closed())
	}
	if got := closedTurnRecorded(t, ctx); !reflect.DeepEqual(got, map[string]int64{"slash": 1, "aux_input": 1000}) {
		t.Fatalf("recorded %v", got)
	}
}

func TestATurnRelaySkippedStillGatesMarks(t *testing.T) {
	ctx := closedTurnMarksHome(t)
	coordinator := tools.RelayCoordinator
	lease := coordinator.AcquireConversation(ctx, tools.RelayConversation{ProfileKey: tools.CurrentRelayProfileKey(ctx), SessionID: "s1", Platform: "cli"})
	first, err := coordinator.BeginTurn(ctx, lease, "turn-1", "t1", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := coordinator.BeginTurn(ctx, lease, "turn-2", "t2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.RelayEnabled() {
		t.Fatal("a concurrent turn on the session must be skipped by Relay")
	}
	insideSkipped := pyctx.With(ctx, pyctx.CopyContext(ctx))
	closedTurnRunAfter(t, insideSkipped, func() {
		coordinator.EndTurn(ctx, second, "success")
		coordinator.EndTurn(ctx, first, "success")
		coordinator.ReleaseConversation(lease)
	})
	if got := closedTurnRecorded(t, ctx); !reflect.DeepEqual(got, map[string]int64{"slash": 0, "aux_input": 0}) {
		t.Fatalf("recorded %v", got)
	}
}
