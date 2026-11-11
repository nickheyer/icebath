package gatewayrun

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Pushes a credits or out of band notice from the agent goroutine to the chat as one line
func (t *TurnRunner) noticeCallbackSync(notice provider.AgentNotice) {
	tc := t.ctx
	if tc.muteNotificationReply || !t.statusLive() {
		return
	}
	key := notice.Key
	if key == "credits.window_limit" || strings.HasPrefix(key, "credits.window.") {
		return
	}
	diagnostic := config.IsDiagnosticNotice(notice.Level, notice.Key)
	present := func() error {
		line := renderNoticeLine(notice)
		if strings.HasPrefix(key, "credits.") {
			line = provider.PortalWording(line)
		}
		if line != "" {
			source := tc.source
			t.schedule(func(ctx context.Context) (any, error) {
				return nil, t.runner.deliverPlatformNotice(ctx, source, line)
			}, "notice_callback delivery scheduling error")
		}
		return nil
	}
	if _, err := config.RenderNotification(t.turnCtx, present, string(tc.source.Platform), diagnostic, tc.userConfig); err != nil {
		runLog().Debug(fmt.Sprintf("render_notice_line failed: %s", err))
	}
}

// Generation the session's event stream is stamped with, nil when no execution is stamped
func executionGenerationOf(live *gateway.LiveSession) any {
	live.EventStream.Lock.Lock()
	defer live.EventStream.Lock.Unlock()
	if live.EventStream.Execution == nil {
		return nil
	}
	value, _ := live.EventStream.Execution.Get("execution_generation")
	return value
}

// Send and release callbacks holding background review messages until the main response is delivered
func (t *TurnRunner) makeBgReviewCallbacks() (func(string), gateway.PostDeliveryCallback) {
	tc := t.ctx
	var mu sync.Mutex
	released := false
	var pending []string
	owner := t.approvalOwner
	localViewer := owner != nil && tc.source != nil && tc.source.Platform == gateway.PlatformLocal
	deliver := func(message string) {
		if localViewer {
			if live, ok := owner.authority.Live(owner.sessionID); ok {
				live.EventStream.Publish(owner.sessionID, map[string]any{"text": message}, "review.summary", nil)
			}
			return
		}
		if t.statusLive() {
			t.sendStatusText(message, interimMetadata(nonConversationalMetadata(tc.statusThreadMetadata, tc.source.Platform)),
				"background_review_callback scheduling error")
		}
	}
	release := func() {
		mu.Lock()
		released = true
		queued := pending
		pending = nil
		mu.Unlock()
		for _, message := range queued {
			deliver(message)
		}
	}
	send := func(message string) {
		if localViewer {
			live, ok := owner.authority.Live(owner.sessionID)
			deadline := time.Now().Add(120 * time.Second)
			for ok && time.Now().Before(deadline) && pyrepr.Equal(executionGenerationOf(live), owner.generation) {
				time.Sleep(50 * time.Millisecond)
			}
			deliver(message)
			return
		}
		if !t.statusLive() {
			return
		}
		mu.Lock()
		if !released {
			pending = append(pending, message)
			mu.Unlock()
			return
		}
		mu.Unlock()
		deliver(message)
	}
	return send, func(context.Context) error {
		release()
		return nil
	}
}

// Layers this turn's routing overrides over the agent's init overrides, dropping only the previous turn's
func mergeTurnRequestOverrides(a *agent.AIAgent, route turnRoute) {
	overrides := maps.Clone(a.RequestOverrides())
	if overrides == nil {
		overrides = map[string]any{}
	}
	for key, value := range a.GatewayTurnRequestOverrides() {
		if current, ok := overrides[key]; ok && pyrepr.Equal(current, value) {
			delete(overrides, key)
		}
	}
	turnOverrides := maps.Clone(route.requestOverrides)
	if turnOverrides == nil {
		turnOverrides = map[string]any{}
	}
	maps.Copy(overrides, turnOverrides)
	a.SetRequestOverrides(overrides)
	a.SetGatewayTurnRequestOverrides(turnOverrides)
}

// Finalizes a native stream before an interrupting prompt so later output lands below it, false when it failed
func (t *TurnRunner) closeNativeStreamBoundary(reason string, boundaryText string, reopen bool) bool {
	sc := t.ctx.streamConsumer()
	if sc == nil || !sc.AcceptsToolProgress() {
		return true
	}
	boundary := sc.CloseForApprovalPrompt(boundaryText, reason, reopen)
	if boundary == nil {
		return true
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ok, err := boundary.Wait(waitCtx)
	if err != nil {
		runLog().Warn(fmt.Sprintf("%s boundary timed out or failed: %s", reason, err))
		return false
	}
	if !ok {
		runLog().Warn(fmt.Sprintf("%s boundary failed to close stream properly — prompt may still appear in typing bubble", reason))
	}
	return ok
}
