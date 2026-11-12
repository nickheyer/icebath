package observability

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// Mark is one process level fact, its mark name and bounded data
type Mark struct {
	Name string
	Data map[string]string
}

// Records one process mark when shared metrics are on, a builder returning nil or panicking recording nothing
func emit(ctx context.Context, mark string, build func() map[string]string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("Shared-metrics %s not recorded", mark), "error", r)
		}
	}()
	if !relayEnabled(ctx) {
		return
	}
	if data := build(); data != nil {
		recordProcessMark(ctx, mark, data)
	}
}

// Records facts recovered from a file and returns how many rows are settled, blocking on the store
func EmitSaved(ctx context.Context, marks []Mark) (settled int) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Shared-metrics recovered rows not recorded", "error", r)
			settled = 0
		}
	}()
	if len(marks) == 0 {
		return 0
	}
	return recordProcessMarksSaved(ctx, marks)
}

// Counts a finished setup on its surface with the provider it configured, nil provider reading None
func RecordSetupCompleted(ctx context.Context, surface string, provider any) {
	emit(ctx, SetupCompletedMark, func() map[string]string { return SetupCompletedFields(surface, provider) })
}

// Counts one slash command typed on a surface
func RecordSlashCommand(ctx context.Context, command, surface string) {
	emit(ctx, SlashCommandMark, func() map[string]string { return SlashCommandFields(ctx, command, surface) })
}

// Counts one tool search call by scope, hit count and outcome
func RecordToolSearch(ctx context.Context, scope string, hits int, outcome string) {
	emit(ctx, ToolSearchMark, func() map[string]string { return ToolSearchFields(scope, hits, outcome) })
}

// Counts one compression attempt, nil token counts and failure class reading None
func RecordCompression(ctx context.Context, trigger, outcome string, tokensBefore, contextLength, failureClass any) {
	emit(ctx, CompressionMark, func() map[string]string {
		return CompressionFields(trigger, outcome, tokensBefore, contextLength, failureClass)
	})
}

// pendingCompression is one begun compression attempt awaiting its outcome
type pendingCompression struct {
	trigger      string
	tokensBefore any
}

// Pending attempt per owner, the owner being the worker that begins and finishes one attempt
var compressionAttempts sync.Map

// Stores the owner's pending compression attempt, replacing any earlier one
func BeginCompressionAttempt(owner any, trigger string, tokensBefore any) {
	compressionAttempts.Store(owner, pendingCompression{trigger: trigger, tokensBefore: tokensBefore})
}

// Clears the owner's pending attempt without recording a metric
func DiscardCompressionAttempt(owner any) {
	compressionAttempts.Delete(owner)
}

// Counts the owner's pending attempt once, a committed one also counting a compression cache break
func FinishCompressionAttempt(ctx context.Context, owner any, commitStatus string, failureClass, contextLength any, agent MetricsAgent) {
	value, ok := compressionAttempts.LoadAndDelete(owner)
	if !ok {
		return
	}
	pending := value.(pendingCompression)
	committed := commitStatus == "committed"
	stated := "failed"
	if committed {
		stated = "success"
	}
	reason := CompressionFailureClass(stated, failureClass)
	outcome := "failed"
	switch {
	case committed:
		outcome = "success"
	case CompressionSkipClasses.Has(reason):
		outcome = "skipped"
	}
	RecordCompression(ctx, pending.trigger, outcome, pending.tokensBefore, contextLength, reason)
	if outcome == "success" && agent != nil {
		RecordCacheBreak(ctx, agent, "compression")
	}
}

// Counts a user typed gateway slash command once, the event's counted flag surviving a busy path requeue
func RecordGatewaySlashCommand(ctx context.Context, command string, counted *bool) {
	if command == "" || (counted != nil && *counted) {
		return
	}
	if counted != nil {
		*counted = true
	}
	RecordSlashCommand(ctx, command, "gateway")
}

// Counts a model switch, friction against the left model and the turns it served before the switch
func RecordModelSwitch(ctx context.Context, fromProvider, toProvider any, surface string, fromModel any, sessionID any) {
	emit(ctx, ModelSwitchMark, func() map[string]string { return ModelSwitchFields(ctx, fromProvider, toProvider, surface) })
	if pyText(fromModel) != "" {
		RecordModelFriction(ctx, "switch_away", FrictionOptions{Provider: fromProvider, Model: fromModel})
	}
	if id := pyText(sessionID); id != "" {
		recordModelSwitchAfter(ctx, id)
	}
}

// Counts a provider fallback with the failover reason that caused it
func RecordFallback(ctx context.Context, fromProvider, toProvider, reason any) {
	emit(ctx, FallbackMark, func() map[string]string { return FallbackFields(fromProvider, toProvider, reason) })
}

// Counts one extension install, a raised error classified by its tagged class or type inside the metrics guard
func RecordExtensionInstall(ctx context.Context, kind, source string, name any, outcome string, failureClass, registry any, err error) {
	emit(ctx, ExtensionInstallMark, func() map[string]string {
		return ExtensionInstallFields(kind, source, name, outcome, failureClass, registry, err)
	})
}
