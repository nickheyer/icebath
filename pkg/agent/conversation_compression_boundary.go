package agent

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Forgets the computer use screenshot dedup of a session, assigned by the top level wiring
var ResetScreenshotDedup func(sessionID string)

// Starts a fresh read and skill view dedup generation after a compaction boundary
func resetReadDedupCaches(taskID, sessionID string) {
	func() {
		defer func() { _ = recover() }()
		tools.ResetFileDedup(taskID)
	}()
	if sessionID != "" && ResetScreenshotDedup != nil {
		func() {
			defer func() { _ = recover() }()
			ResetScreenshotDedup(sessionID)
		}()
	}
	func() {
		defer func() { _ = recover() }()
		skills.ResetSkillViewDedup(taskID)
	}()
}

// Keeps the seeded system prompt across compaction boundaries, set by the gateway for seeded sessions
func (a *AIAgent) SetRetainSeededSystemPrompt(retain bool) { a.retainSeededSystemPrompt = retain }

// Warns once per distinct failed summary, or once per broken auxiliary model the main model covered for
func warnSummaryOrAuxFallback(a *AIAgent) {
	cc, _ := a.contextCompressor.(*ContextCompressor)
	if cc != nil && cc.lastSummaryError != nil && *cc.lastSummaryError != "" {
		summaryError := *cc.lastSummaryError
		if a.lastCompressionSummaryWarning == nil || *a.lastCompressionSummaryWarning != summaryError {
			a.lastCompressionSummaryWarning = &summaryError
			a.emitWarning(fmt.Sprintf("⚠ Compression summary failed: %s. Inserted a fallback context marker.", summaryError))
		}
		return
	}
	if cc == nil || cc.LastAuxModelFailureModel == "" {
		return
	}
	key := [2]string{cc.LastAuxModelFailureModel, cc.LastAuxModelFailureError}
	if a.lastAuxFallbackWarningKey != nil && *a.lastAuxFallbackWarningKey == key {
		return
	}
	a.lastAuxFallbackWarningKey = &key
	reason := key[1]
	if reason == "" {
		reason = "unknown error"
	}
	compressionLog().Warn(fmt.Sprintf("Configured compression model %s failed (%s); recovered using the main model.", pyrepr.Repr(key[0]), reason))
	a.emitWarning(fmt.Sprintf("ℹ Configured compression model '%s' failed, so Icebath summarised with your main model instead. Check auxiliary.compression.model in your config.", key[0]))
}
