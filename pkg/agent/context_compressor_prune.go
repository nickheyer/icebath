package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Store that commits a full archive and compact rewrite
type archiveCompactor interface {
	ArchiveAndCompact(ctx context.Context, sessionID string, compacted []map[string]any, o state.ArchiveCompactOptions) (int, error)
}

// Store that rewrites only the pruned rows in place
type prunedRowRewriter interface {
	RewritePrunedRows(ctx context.Context, sessionID string, changes []state.PruneChange, o state.PruneRewriteOptions) (int, error)
}

// Tool result scan the prune and the window pass run, swapped by tests as hermes patches _prune_old_tool_results
var compressorPruneScan = (*ContextCompressor).pruneOldToolResults

// Whether a provider billed reading puts the session at or over the threshold
func (c *ContextCompressor) billedBasisOverThreshold(currentTokens *int) bool {
	threshold := c.thresholdTokensValue()
	return currentTokens != nil && threshold > 0 && *currentTokens >= threshold
}

// Warns once per reason and rearm mark when an over threshold session declines every reclamation path
func (c *ContextCompressor) warnReclamationNoOp(reason string, currentTokens, before *int) {
	if currentTokens == nil || !c.billedBasisOverThreshold(currentTokens) {
		c.lastReclaimBlockWarn = nil
		return
	}
	key := reclaimBlockWarnKey{reason: reason, rearm: c.proactivePruneRearmTokens}
	if c.lastReclaimBlockWarn != nil && *c.lastReclaimBlockWarn == key {
		return
	}
	c.lastReclaimBlockWarn = &key
	beforeText := "n/a"
	if before != nil {
		beforeText = groupedInt(*before)
	}
	slog.Warn(fmt.Sprintf("Context is over the compression threshold (~%s of %s tokens) but reclamation did not run: %s (message-token estimate %s, prune rearm mark %s). The session may keep growing until the provider rejects the request — /compact to compress history now.",
		groupedInt(*currentTokens), groupedInt(c.thresholdTokensValue()), reason, beforeText, groupedInt(c.proactivePruneRearmTokens)))
}

func (c *ContextCompressor) messageBudgetTokens(messages []map[string]any) int {
	total := 0
	for _, m := range messages {
		total += estimateMsgBudgetTokens(c.ctx, m, true)
	}
	return total
}

// Names the bound session row without reloading its durable state, the hermes _session_id assignment
func (c *ContextCompressor) SetSessionID(sessionID string) { c.sessionID = sessionID }

// Deterministic tool result prune gated on the proactive budget, the input returned unchanged on any no op
func (c *ContextCompressor) pruneToolResultsOnly(messages []map[string]any, currentTokens *int) ([]map[string]any, int) {
	if c.proactivePruneTokens <= 0 || (currentTokens != nil && *currentTokens < c.proactivePruneTokens) {
		return messages, 0
	}
	if len(messages) <= c.ProtectLastN+c.protectHeadSize(messages)+1 {
		c.warnReclamationNoOp("prune:tail_only", currentTokens, nil)
		return messages, 0
	}
	before := c.messageBudgetTokens(messages)
	if before < c.proactivePruneRearmTokens && !c.billedBasisOverThreshold(currentTokens) {
		return messages, 0
	}
	sessionDB, sessionID := c.sessionDB, c.sessionID
	if sessionDB != nil && sessionID != "" {
		if _, ok := sessionDB.(archiveCompactor); !ok {
			c.warnReclamationNoOp("prune:store_cannot_persist", currentTokens, nil)
			return messages, 0
		}
	}
	prunedMsgs, prunedCount := compressorPruneScan(c, messages, c.ProtectLastN, nil, c.proactivePruneMinResultChars)
	prunedMsgs = WithPrunedRowSkillSafetyRule(messages, prunedMsgs)
	if prunedCount == 0 {
		c.warnReclamationNoOp("prune:nothing_eligible", currentTokens, nil)
		return messages, 0
	}
	after := c.messageBudgetTokens(prunedMsgs)
	reclaimed := max(0, before-after)
	if reclaimed < c.proactivePruneMinReclaimTokens {
		c.warnReclamationNoOp("prune:reclaim_below_minimum", currentTokens, &before)
		return messages, 0
	}
	nextRearmTokens := after + max(reclaimed, c.proactivePruneTokens, c.proactivePruneMinReclaimTokens)
	if sessionDB != nil && sessionID != "" {
		patch := pyjson.ObjectOf(ProactivePruneRearmModelConfigKey, nextRearmTokens)
		rewritten, err := c.commitPrune(sessionDB, sessionID, messages, prunedMsgs, patch)
		var stale *StaleHeldHistory
		var heldRewritten *state.HeldRowRewrittenError
		switch {
		case errors.As(err, &stale):
			slog.Info("Proactive tool-result prune skipped: another compaction already committed this session")
			c.warnReclamationNoOp("prune:stale_generation", currentTokens, &before)
			return messages, 0
		case errors.As(err, &heldRewritten):
			slog.Info("Proactive tool-result prune skipped: a held row was rewritten in place before the commit")
			c.warnReclamationNoOp("prune:stale_generation", currentTokens, &before)
			return messages, 0
		case err != nil:
			slog.Warn("Proactive tool-result prune DB commit failed; keeping the original transcript: " + err.Error())
			return messages, 0
		}
		if rewritten == nil {
			StampDBPersistedMarkers(prunedMsgs)
		} else {
			var persisted []map[string]any
			for i, original := range messages {
				if pyrepr.Truthy(original[dbPersistedMarker]) {
					persisted = append(persisted, prunedMsgs[i])
				}
			}
			StampDBPersistedMarkers(persisted)
		}
	}
	c.proactivePruneRearmTokens = nextRearmTokens
	c.lastReclaimBlockWarn = nil
	return prunedMsgs, prunedCount
}

// Commits the prune through an in place row rewrite or the full archive and compact, nil count for the full rewrite
func (c *ContextCompressor) commitPrune(sessionDB any, sessionID string, messages, prunedMsgs []map[string]any, patch *pyjson.Object) (*int, error) {
	watermark, err := archiveWatermarkFor(c.ctx, sessionDB, sessionID, messages, nil)
	if err != nil {
		return nil, err
	}
	rewritten, err := c.rewritePrunedRowsInPlace(sessionDB, sessionID, messages, prunedMsgs, patch)
	if err != nil || rewritten != nil {
		return rewritten, err
	}
	o := state.ArchiveCompactOptions{ModelConfigPatch: patch, Watermark: watermark, HeldForCAS: messages}
	covered, unresolved, ok, err := state.CoverageForCommit(sessionDB, sessionID, messages, nil)
	if err != nil {
		return nil, err
	}
	if ok {
		o.CoveredIDs, o.UnresolvedHeld = covered, unresolved
	}
	_, err = sessionDB.(archiveCompactor).ArchiveAndCompact(c.ctx, sessionID, prunedMsgs, o)
	return nil, err
}

// Rewrites only the changed durable rows, nil without error when the full rewrite must run instead
func (c *ContextCompressor) rewritePrunedRowsInPlace(sessionDB any, sessionID string, messages, prunedMsgs []map[string]any, patch *pyjson.Object) (*int, error) {
	rewriter, ok := sessionDB.(prunedRowRewriter)
	if !ok || len(messages) != len(prunedMsgs) {
		return nil, nil
	}
	ignored := map[string]bool{dbPersistedMarker: true, "_row_id": true}
	var changes []state.PruneChange
	unwrittenSeen := false
	for i, original := range messages {
		replacement := prunedMsgs[i]
		if original == nil || replacement == nil {
			return nil, nil
		}
		if !pyrepr.Truthy(original[dbPersistedMarker]) {
			unwrittenSeen = true
			continue
		}
		if unwrittenSeen {
			return nil, nil
		}
		if !pyrepr.Equal(withoutKeys(original, ignored), withoutKeys(replacement, ignored)) {
			changes = append(changes, state.PruneChange{Original: original, Replacement: replacement})
		}
	}
	n, err := rewriter.RewritePrunedRows(c.ctx, sessionID, changes, state.PruneRewriteOptions{ModelConfigPatch: patch})
	var staleRow *state.PruneRowStaleError
	var unresolved *state.PruneRowUnresolvedError
	switch {
	case errors.As(err, &staleRow):
		return nil, &StaleHeldHistory{Msg: staleRow.Error()}
	case errors.As(err, &unresolved):
		slog.Info(fmt.Sprintf("Proactive prune: in-place rewrite refused (%s); using the full rewrite", unresolved.Error()))
		return nil, nil
	case err != nil:
		return nil, err
	}
	return &n, nil
}

// Shallow copy of the row without the named keys
func withoutKeys(m map[string]any, drop map[string]bool) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if !drop[k] {
			out[k] = v
		}
	}
	return out
}
