package tuiserver

import (
	"context"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

var (
	autoPruneNotifiedMu sync.Mutex
	autoPruneNotified   = map[string]float64{}
)

// Fresh durable session key in the timestamp plus hex shape the store mints
func newSessionKey() string { return state.NewSessionID() }

// Deferred agent build scheduler, the delayed timer below
var scheduleAgentBuild func(ctx context.Context, sid string, delay float64)

func init() { scheduleAgentBuild = scheduleAgentBuildTimer }

// Starts the agent build of a deferred session after a short delay, off the response path
func scheduleAgentBuildTimer(ctx context.Context, sid string, delay float64) {
	ctx = context.WithoutCancel(ctx)
	time.AfterFunc(time.Duration(delay*float64(time.Second)), func() {
		defer func() {
			if failure := recover(); failure != nil {
				logger().Error("Exception in thread agent-build-timer", "exc_info", failureText(failure))
			}
		}()
		if session := tuigateway.SessionRecord(sid); session != nil {
			startAgentBuild(ctx, sid, session)
			maybeNotifyAutoPrune(ctx, sid, strOr(session.Get("profile_home")))
		}
	})
}

// Raises the sticky low disk auto prune notice once per home per snooze window, estimating off thread
func maybeNotifyAutoPrune(ctx context.Context, sid, profileHome string) {
	h := profileHome
	if h == "" {
		h = home.Dir(ctx)
	}
	autoPruneNotifiedMu.Lock()
	shown := make(map[string]float64, len(autoPruneNotified))
	for key, at := range autoPruneNotified {
		shown[key] = at
	}
	autoPruneNotifiedMu.Unlock()
	if !state.AutoPruneNoticeDue(ctx, h, shown) {
		return
	}
	autoPruneNotifiedMu.Lock()
	autoPruneNotified[h] = nowSeconds()
	autoPruneNotifiedMu.Unlock()
	memory.SpawnContextThread(ctx, "auto-prune-advice", func(ctx context.Context) {
		defer threadExceptHook("auto-prune-advice")
		advice, err := state.AutoPruneAdvise(ctx, h)
		if err != nil {
			logger().Error("Exception in thread auto-prune-advice", "exc_info", err.Error())
			return
		}
		if advice != nil && state.AutoPruneShouldAsk(ctx, h) != nil {
			tuigateway.Emit(ctx, "notification.show", sid, objectMap(state.AutoPruneNoticePayload(*advice)))
			return
		}
		autoPruneNotifiedMu.Lock()
		delete(autoPruneNotified, h)
		autoPruneNotifiedMu.Unlock()
	}).Start()
}
