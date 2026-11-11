package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Tool interrupt reason of a turn whose session slot was evicted
const interruptToolReasonEvicted = "session evicted"

// Python fixed point format with no decimals, inf for an unbounded value
func zeroDecimals(value float64) string {
	text, err := pyrepr.Format(value, ".0f")
	if err != nil {
		return fmt.Sprintf("%.0f", value)
	}
	return text
}

// Evicts a leaked running slot whose agent idled past the timeout, or reports no activity and outlived the wall ttl
func (r *GatewayRunner) hmEvictIdleStaleAgent(ctx context.Context, quickKey string) {
	rawStaleTimeout := floatEnv("ICEBATH_AGENT_TIMEOUT", 1800)
	quickState := r.peekSessionState(quickKey)
	if quickState == nil || quickState.turn.agent == nil || quickState.turn.startedTS == 0 {
		return
	}
	staleAge := float64(time.Now().UnixNano())/1e9 - quickState.turn.startedTS
	staleAgent := quickState.turn.agent
	staleIdle := math.Inf(1)
	staleDetail := ""
	summaryValid := false
	if staleAgent != agentPendingSentinel {
		func() {
			defer func() { recover() }()
			summary := staleAgent.GetActivitySummary()
			now := float64(time.Now().UnixNano()) / 1e9
			if resolved := gateway.ResolveSessionIdleSecondsFromActivity(summary, &now); resolved != nil {
				staleIdle = *resolved
				summaryValid = true
				if tc := quickState.turn.ctx; tc != nil && tc.awakeIdle != nil {
					staleIdle = tc.awakeIdle.Measure(staleIdle)
				}
			}
			field := func(key string, def any) string {
				if value, ok := summary[key]; ok {
					return pyrepr.Str(value)
				}
				return pyrepr.Str(def)
			}
			staleDetail = fmt.Sprintf(" | last_activity=%s (%ss ago) | iteration=%s/%s", field("last_activity_desc", "unknown"),
				zeroDecimals(staleIdle), field("api_call_count", 0), field("max_iterations", 0))
		}()
	}
	wallTTL := math.Inf(1)
	if rawStaleTimeout > 0 {
		wallTTL = math.Max(rawStaleTimeout*10, 7200)
	}
	shouldEvict := staleAgent != agentPendingSentinel && ((summaryValid && rawStaleTimeout > 0 && staleIdle >= rawStaleTimeout) ||
		(!summaryValid && staleAge > wallTTL))
	if shouldEvict {
		runLog().Warn(fmt.Sprintf("Evicting stale _running_agents entry for %s (age: %ss, idle: %ss, timeout: %ss)%s",
			quickKey, zeroDecimals(staleAge), zeroDecimals(staleIdle), zeroDecimals(rawStaleTimeout), staleDetail))
		r.hmEvictRunningAgent(ctx, quickKey, "stale_running_agent_eviction")
	}
}

// Evicts the turn slot of a session whose durable row was ended while the gateway lived
func (r *GatewayRunner) hmEvictReapedAgent(ctx context.Context, quickKey string) {
	if r.sessionStore == nil {
		return
	}
	sid, ok := r.sessionStore.PeekSessionID(quickKey)
	if ok && sid != "" && r.sessionStore.IsSessionEndedInDB(ctx, sid, "") {
		runLog().Warn(fmt.Sprintf("Evicting stale _running_agents entry for %s — durable session %s is ended (reaped) in state.db; healing routing on next message (#99106)", quickKey, sid))
		r.hmEvictRunningAgent(ctx, quickKey, "reaped_session_eviction")
	}
}

// Interrupts the running turn as evicted and drops the slot of the generation it invalidated
func (r *GatewayRunner) hmEvictRunningAgent(ctx context.Context, quickKey, reason string) {
	generation := r.interruptRunningTurn(ctx, quickKey, interruptReasonEvicted, reason, interruptToolReasonEvicted)
	r.dropTurnSlot(ctx, quickKey, &generation)
}

// Merges the event into the source adapter's pending slot, nothing without an adapter
func (r *GatewayRunner) hmMergePendingForSource(ctx context.Context, source *gateway.SessionSource, quickKey string, event *gateway.MessageEvent, mergeText bool) error {
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return err
	}
	if adapter != nil {
		adapter.Base().MergePendingMessage(quickKey, event, mergeText)
	}
	return nil
}

// Cancels the route's queued messaging admissions then interrupts the authority's claimed turn
func (r *GatewayRunner) hmStopOwnerTurn(ctx context.Context, quickKey string) error {
	authority, err := gateway.ActiveAuthority(ctx, r)
	if err != nil {
		return err
	}
	sid := ""
	if authority != nil {
		peeked, _ := r.sessionStore.PeekSessionID(quickKey)
		if sid, err = authority.LogicalOwner(ctx, peeked); err != nil {
			return err
		}
	}
	if _, live := authorityLive(authority, sid); sid == "" || !live {
		if authority != nil {
			authority.FenceRoute(quickKey)
		}
		return nil
	}
	ref := gateway.SessionRef{ProfileID: authority.ProfileID, SessionID: sid}
	actor := gateway.Principal{Subject: "messaging-control", ProfileID: authority.ProfileID,
		Capabilities: map[string]bool{"session:submit": true, "session:control": true}, TransportID: "busy-command"}
	rows, err := authority.DB.ListSessionAdmissions(ctx, sid, false)
	if err != nil {
		return err
	}
	paused := false
	for _, row := range rows {
		if row.GetOr("status", nil) == "unknown" {
			paused = true
			break
		}
	}
	if !paused {
		authority.FenceRoute(quickKey)
		if rows, err = authority.DB.ListSessionAdmissions(ctx, sid, false); err != nil {
			return err
		}
		for _, row := range rows {
			principal, _ := row.GetOr("principal_id", "").(string)
			if row.GetOr("status", nil) == "queued" && strings.HasPrefix(principal, "messaging:") {
				admissionID, _ := row.GetOr("admission_id", "").(string)
				if _, err := authority.CancelQueued(ctx, actor, ref, admissionID); err != nil && !isRuntimeStoreError(err) {
					return err
				}
			}
		}
	}
	handle, err := authority.Handle(ctx, ref)
	if err != nil {
		return err
	}
	if handle.ExecutionState == "running" {
		if _, err := authority.Interrupt(ctx, actor, ref, handle.ExecutionGeneration); err != nil && !isRuntimeStoreError(err) {
			return err
		}
	}
	return nil
}

// Whether err is a RuntimeStoreError of the runtime store
func isRuntimeStoreError(err error) bool {
	var storeErr *state.RuntimeStoreError
	return errors.As(err, &storeErr)
}

// Live session of the authority under sid, false without an authority
func authorityLive(authority *gateway.SessionAuthority, sid string) (*gateway.LiveSession, bool) {
	if authority == nil || sid == "" {
		return nil, false
	}
	return authority.Live(sid)
}

// Queues a Telegram text follow up that lands inside the post start grace window
func (r *GatewayRunner) hmBusyTelegramGraceQueue(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey, effectiveBusyInputMode string) (bool, error) {
	rawGrace, ok := os.LookupEnv("ICEBATH_TELEGRAM_FOLLOWUP_GRACE_SECONDS")
	if !ok {
		rawGrace = "3.0"
	}
	grace, err := pyint.ParseFloat(rawGrace)
	if err != nil {
		return false, err
	}
	startedAt := 0.0
	if graceState := r.peekSessionState(quickKey); graceState != nil {
		startedAt = graceState.turn.startedTS
	}
	now := float64(time.Now().UnixNano()) / 1e9
	if !(source.Platform == gateway.PlatformTelegram && event.MessageType == gateway.MessageTypeText &&
		grace > 0 && startedAt != 0 && now-startedAt <= grace) {
		return false, nil
	}
	runLog().Debug(fmt.Sprintf("Telegram follow-up arrived %.2fs after run start for %s — queueing without interrupt", now-startedAt, quickKey))
	if effectiveBusyInputMode != "queue" {
		return true, r.hmMergePendingForSource(ctx, source, quickKey, event, true)
	}
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return true, err
	}
	if adapter != nil {
		r.enqueueFIFO(quickKey, event, adapter)
	}
	return true, nil
}
