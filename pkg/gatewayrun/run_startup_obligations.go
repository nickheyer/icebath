package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Flood redelivery workers keyed by platform and bot identity, one per identity
type floodRedelivery struct {
	mu    sync.Mutex
	tasks map[[2]string]*gatewayTask
	wakes map[[2]string]*pythread.Event
}

// Clears resume flags of claimed rows and returns the rows safe to redeliver, strict for runtime reconnects
func (r *GatewayRunner) clearResumePendingForClaimedObligations(ctx context.Context, claimed []gateway.ClaimedObligation, requireSuccess bool) []gateway.ClaimedObligation {
	var sendable []gateway.ClaimedObligation
	for _, row := range claimed {
		if row.SessionKey == "" {
			sendable = append(sendable, row)
			continue
		}
		if _, err := r.sessionStore.ClearResumePending(ctx, row.SessionKey); err != nil {
			runLog().Debug(fmt.Sprintf("clear_resume_pending failed for %s", row.SessionKey), "exc_info", err)
			if requireSuccess {
				continue
			}
		}
		sendable = append(sendable, row)
	}
	return sendable
}

// Claims recoverable ledger rows whose exact adapter is connected and clears their resume flags before any send
func (r *GatewayRunner) claimPendingObligations(ctx context.Context) []gateway.ClaimedObligation {
	if !gateway.LedgerEnabled(ctx, nil) {
		return nil
	}
	targets := map[gateway.LedgerTarget]bool{}
	for _, platform := range r.adapters.Keys() {
		targets[gateway.LedgerTarget{Platform: string(platform), Profile: "default"}] = true
	}
	if r.profileAdapters == nil || r.profileAdapters.Len() == 0 {
		for _, platform := range r.adapters.Keys() {
			targets[gateway.LedgerTarget{Platform: string(platform), NullProfile: true}] = true
		}
	}
	if r.profileAdapters != nil {
		for _, profile := range r.profileAdapters.Keys() {
			adapters, _ := r.profileAdapters.Get(profile)
			if adapters == nil {
				continue
			}
			for _, platform := range adapters.Keys() {
				targets[gateway.LedgerTarget{Platform: string(platform), Profile: profile}] = true
			}
		}
	}
	platforms := map[string]bool{}
	for target := range targets {
		platforms[target.Platform] = true
	}
	claimed, err := gateway.SweepRecoverable(ctx, nil, platforms, targets)
	if err != nil {
		runLog().Debug("delivery ledger sweep failed", "exc_info", err)
		return nil
	}
	if len(claimed) == 0 {
		return nil
	}
	r.clearResumePendingForClaimedObligations(ctx, claimed, false)
	return claimed
}

// Releases an unsent runtime ledger claim back to failed with its error, logging a failure only
func releaseRuntimeClaimQuiet(ctx context.Context, obligationID, logFmt, errText string) {
	if errText == "" {
		errText = "send_path_degraded"
	}
	if _, err := gateway.ReleaseRuntimeClaim(ctx, obligationID, errText); err != nil {
		runLog().Debug(fmt.Sprintf(logFmt, obligationID), "exc_info", err)
	}
}

// Wakes one deadline driven ledger worker per bot identity, never sleeping in a send
func (r *GatewayRunner) scheduleFloodRedelivery(ctx context.Context, platform gateway.Platform, profile *string) {
	key := [2]string{string(platform), "default"}
	if profile != nil && *profile != "" {
		key[1] = *profile
	}
	flood := &r.floodRedelivery
	flood.mu.Lock()
	if flood.tasks == nil {
		flood.tasks = map[[2]string]*gatewayTask{}
		flood.wakes = map[[2]string]*pythread.Event{}
	}
	if _, running := flood.tasks[key]; running {
		flood.wakes[key].Set()
		flood.mu.Unlock()
		return
	}
	wake := &pythread.Event{}
	flood.wakes[key] = wake
	task := spawnGatewayTask(context.WithoutCancel(ctx), fmt.Sprintf("flood-redelivery:%s:%s", key[0], key[1]), func(ctx context.Context) (any, error) {
		defer func() {
			flood.mu.Lock()
			delete(flood.tasks, key)
			delete(flood.wakes, key)
			flood.mu.Unlock()
		}()
		for r.running {
			wake.Clear()
			waiting, err := gateway.PendingRetries(ctx, nil)
			if err != nil {
				return nil, err
			}
			var earliest *float64
			for _, row := range waiting {
				if row.Platform == key[0] && row.Profile == key[1] && (earliest == nil || row.NotBefore < *earliest) {
					notBefore := row.NotBefore
					earliest = &notBefore
				}
			}
			if earliest == nil {
				if wake.IsSet() {
					continue
				}
				return nil, nil
			}
			delay := gateway.FloodRetryDelay(*earliest - float64(time.Now().UnixNano())/1e9)
			if wake.Wait(delay) {
				continue
			}
			if r.running {
				r.redeliverFailedObligationsForPlatform(ctx, platform, profile)
			}
		}
		return nil, nil
	})
	flood.tasks[key] = task
	flood.mu.Unlock()
	if r.backgroundTasks != nil {
		r.trackTaskIn(r.backgroundTasks, task)
	}
}

// Arms a flood timer for every row waiting on a flood penalty or retry backoff
func (r *GatewayRunner) armFloodTimersForWaitingRows(ctx context.Context) error {
	waiting, err := gateway.PendingRetries(ctx, nil)
	if err != nil {
		return err
	}
	for _, row := range waiting {
		profile := row.Profile
		r.scheduleFloodRedelivery(ctx, gateway.Platform(row.Platform), &profile)
	}
	return nil
}

// Redelivers the final responses of claimed rows and arms timers for rows still waiting, returning the count
func (r *GatewayRunner) redeliverClaimedObligations(ctx context.Context, claimed []gateway.ClaimedObligation) int {
	redelivered := 0
	for _, row := range claimed {
		if row.Adopted {
			continue
		}
		adapter := r.obligationAdapter(ctx, row)
		if adapter == nil {
			continue
		}
		content := row.Content
		if row.NeedsMarker {
			marker := row.Marker
			if marker == "" {
				marker = gateway.RecoveredMarker
			}
			content = marker + content
		}
		var metadata map[string]any
		if row.ThreadID != nil && *row.ThreadID != "" {
			metadata = map[string]any{"thread_id": *row.ThreadID}
		}
		result, err := adapter.Send(ctx, row.ChatID, content, "", metadata)
		if err != nil {
			runLog().Warn(fmt.Sprintf("obligation %s: redelivery send raised: %s", row.ObligationID, err))
		}
		if err == nil && result.Success {
			if err := gateway.MarkDelivered(ctx, row.ObligationID); err != nil {
				logSuppressedExc(slog.LevelDebug, err, "delivery ledger update failed")
				continue
			}
			redelivered++
			runLog().Info(fmt.Sprintf("Redelivered recovered final response to %s:%s (obligation %s, attempt %d)", row.Platform, row.ChatID, row.ObligationID, row.Attempts))
		} else {
			errText := "send failed"
			if err == nil && pyrepr.Truthy(result.Error) {
				errText = pyrepr.Str(result.Error)
			}
			logSuppressedExc(slog.LevelDebug, gateway.MarkFailed(ctx, row.ObligationID, errText), "delivery ledger update failed")
		}
	}
	logSuppressedExc(slog.LevelDebug, r.armFloodTimersForWaitingRows(ctx), "arming flood redelivery timers failed")
	return redelivered
}

// Adapter of a claimed ledger row, releasing the claim unspent when that adapter is gone
func (r *GatewayRunner) obligationAdapter(ctx context.Context, row gateway.ClaimedObligation) gateway.PlatformAdapter {
	platform, err := gateway.ParsePlatform(ctx, row.Platform)
	if err != nil {
		runLog().Debug(fmt.Sprintf("obligation %s: unknown platform '%s'", row.ObligationID, row.Platform))
		return nil
	}
	profile := row.Profile
	adapter := r.authorizationAdapter(ctx, platform, &profile)
	if adapter == nil {
		errText := "send_path_degraded"
		if gateway.IsFloodError(row.LastError) {
			errText = row.LastError
		}
		releaseRuntimeClaimQuiet(ctx, row.ObligationID, "failed to release undispatched runtime obligation %s", errText)
	}
	return adapter
}

// Claims and redelivers in one call
func (r *GatewayRunner) redeliverPendingObligations(ctx context.Context) int {
	return r.redeliverClaimedObligations(ctx, r.claimPendingObligations(ctx))
}

// Replays one adapter identity's transient delivery failures after it reconnected
func (r *GatewayRunner) redeliverFailedObligationsForPlatform(ctx context.Context, platform gateway.Platform, profile *string) int {
	if !gateway.LedgerEnabled(ctx, nil) {
		return 0
	}
	claimed, err := gateway.SweepFailedForRuntime(ctx, string(platform), nil, profile)
	if err != nil {
		runLog().Debug(fmt.Sprintf("runtime delivery ledger sweep failed after %s reconnect", platform), "exc_info", err)
		return 0
	}
	if len(claimed) == 0 {
		return 0
	}
	sendable := r.clearResumePendingForClaimedObligations(ctx, claimed, true)
	ids := map[string]bool{}
	for _, row := range sendable {
		ids[row.ObligationID] = true
	}
	for _, row := range claimed {
		if !ids[row.ObligationID] {
			releaseRuntimeClaimQuiet(ctx, row.ObligationID, "failed to release runtime delivery claim %s", row.LastError)
		}
	}
	return r.redeliverClaimedObligations(ctx, sendable)
}
