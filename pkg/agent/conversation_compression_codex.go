package agent

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// codexThreadCompactor is the slice of the codex app server session the compaction route drives
type codexThreadCompactor interface {
	CompactThread(turnTimeout, notificationPollTimeout float64) (*provider.CodexTurnResult, error)
	Close()
}

// Live codex session of the agent, a seam the tests replace like the hermes fake session
var codexCompactionSession = func(a *AIAgent) codexThreadCompactor {
	if a.codexSession == nil {
		return nil
	}
	return a.codexSession
}

// compressionCooldownSource reads the session compaction failure cooldown of an engine that keeps one
type compressionCooldownSource interface {
	GetActiveCompressionFailureCooldown(refresh bool) map[string]any
}

// compressionCooldownRecorder arms the shared compaction failure cooldown of an engine that keeps one
type compressionCooldownRecorder interface {
	recordCompressionFailureCooldown(cooldownSeconds float64, errText *string)
}

// Seconds left on the session compaction failure cooldown, zero when clear or unreadable
func codexCompactionCooldownRemaining(a *AIAgent) (remaining float64) {
	getter, ok := a.contextCompressor.(compressionCooldownSource)
	if !ok || a.contextCompressor == nil {
		return 0
	}
	var st map[string]any
	func() {
		defer func() {
			if r := recover(); r != nil {
				compressionLog().Debug("codex compaction cooldown lookup failed", "error", fmt.Sprint(r))
				st = nil
			}
		}()
		st = getter.GetActiveCompressionFailureCooldown(true)
	}()
	if len(st) == 0 {
		return 0
	}
	switch v := st["remaining_seconds"].(type) {
	case float64:
		return math.Max(0, v)
	case int:
		return math.Max(0, float64(v))
	case int64:
		return math.Max(0, float64(v))
	case bool:
		if v {
			return 1
		}
		return 0
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if v == "" || err != nil {
			return 0
		}
		return math.Max(0, f)
	}
	return 0
}

// Arms the shared failure cooldown so a failed codex compaction does not retry every turn
func recordCodexCompactionFailure(a *AIAgent, errText string) {
	recorder, ok := a.contextCompressor.(compressionCooldownRecorder)
	if !ok || a.contextCompressor == nil {
		return
	}
	swallowCompression("codex compaction cooldown persist failed", true, func() error {
		recorder.recordCompressionFailureCooldown(summaryFailureCooldownSeconds, &errText)
		return nil
	})
}

// Python class name of a raised error or panic value for the attempt failure class
func compressionErrorClass(ctx context.Context, v any) string {
	if err, ok := v.(error); ok && tools.IsKeyboardInterrupt(ctx, err) {
		return "KeyboardInterrupt"
	}
	t := reflect.TypeOf(v)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Name() == "" {
		return fmt.Sprintf("%T", v)
	}
	return t.Name()
}

// Lets codex compact its own thread and leaves the local transcript unchanged, one attempt record per exit
func compressContextViaCodexAppServer(ctx context.Context, a *AIAgent, messages []map[string]any, systemMessage *string, approxTokens *int, taskID string, force bool, startedAt time.Time, attemptSeed *pyjson.Object) (out []map[string]any, prompt string, err error) {
	var approx any
	if approxTokens != nil {
		approx = *approxTokens
	}
	record := func(commitStatus string, failureClass any, method string) {
		emitBypassedAttemptTelemetry(ctx, a, startedAt, attemptBypass{CommitStatus: commitStatus, FailureClass: failureClass,
			ApproxTokens: approx, Route: "codex_app_server", Method: method, AttemptSeed: attemptSeed})
	}
	sid := a.sessionID
	if sid == "" {
		sid = "none"
	}
	tokens := "unknown"
	if approxTokens != nil && *approxTokens != 0 {
		tokens = groupThousands(*approxTokens)
	}
	autoMode := strings.ToLower(a.codexAppServerAutoCompaction)
	if autoMode == "" {
		autoMode = "native"
	}
	if autoMode != "native" && autoMode != "hermes" && autoMode != "off" {
		autoMode = "native"
	}
	var skipReason, skipStatus, skipClass string
	if !force && autoMode != "hermes" {
		skipReason, skipStatus, skipClass = fmt.Sprintf("mode=%s force=false", autoMode), "skipped", "codex_auto_"+autoMode
	} else if !force {
		if remaining := codexCompactionCooldownRemaining(a); remaining > 0 {
			skipReason = fmt.Sprintf("failure cooldown active for %.0fs", remaining)
			skipStatus, skipClass = "blocked", "blocked:cooldown"
		}
	}
	session := codexCompactionSession(a)
	if skipReason == "" && session == nil {
		skipReason, skipStatus, skipClass = "no active codex thread", "skipped", "codex_no_thread"
	}
	if skipReason != "" {
		compressionLog().Info(fmt.Sprintf("codex app-server compaction skipped: %s (session=%s messages=%d tokens=~%s)", skipReason, sid, len(messages), tokens))
		record(skipStatus, skipClass, "none")
		prompt, err = existingSystemPrompt(ctx, a, systemMessage)
		return messages, prompt, err
	}
	compressionLog().Info(fmt.Sprintf("codex app-server compaction started: session=%s messages=%d tokens=~%s", sid, len(messages), tokens))
	func() {
		defer func() { _ = recover() }()
		a.emitStatus(CompactionStatus)
	}()
	heartbeat := newCompressionActivityHeartbeat(a, nil, true, nil).start()
	var result *provider.CodexTurnResult
	func() {
		defer func() {
			if r := recover(); r != nil {
				heartbeat.stop("context compression failed")
				record("aborted", "exception:"+compressionErrorClass(ctx, r), "none")
				panic(r)
			}
		}()
		result, err = session.CompactThread(0, 0)
	}()
	if err != nil {
		heartbeat.stop("context compression failed")
		record("aborted", "exception:"+compressionErrorClass(ctx, err), "none")
		return nil, "", err
	}
	if result == nil {
		result = &provider.CodexTurnResult{}
	}
	failed := result.Interrupted || result.Error != ""
	if failed {
		heartbeat.stop("context compression failed")
	} else {
		heartbeat.stop("context compression completed")
	}
	if result.ShouldRetire {
		func() {
			defer func() { _ = recover() }()
			session.Close()
		}()
		a.codexSession = nil
	}
	if failed {
		func() {
			defer func() { _ = recover() }()
			a.emitWarning("⚠ Codex app-server compaction failed: " + result.Error)
		}()
		reason := result.Error
		if reason == "" {
			reason = "compaction interrupted"
		}
		recordCodexCompactionFailure(a, reason)
		record("failed", "codex_compaction_failed", "none")
		prompt, err = existingSystemPrompt(ctx, a, systemMessage)
		return messages, prompt, err
	}
	swallowCompression("codex compaction bookkeeping failed", true, func() error {
		tokensForRecord := 0
		if approxTokens != nil {
			tokensForRecord = *approxTokens
		}
		provider.RecordCodexAppServerCompaction(a, result, tokensForRecord, true)
		if a.contextCompressor != nil {
			provider.RecordCodexAppServerUsage(a, result, messages)
		}
		return nil
	})
	resetReadDedupCaches(ctx, taskID, a.sessionID)
	compressionLog().Info(fmt.Sprintf("codex app-server compaction done: session=%s thread=%s turn=%s", sid, result.ThreadID, result.TurnID))
	prompt, err = existingSystemPrompt(ctx, a, systemMessage)
	if err != nil {
		return nil, "", err
	}
	record("committed", nil, "provider")
	emitCompactionDone(a)
	return messages, prompt, nil
}
