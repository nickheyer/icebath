package gateway

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

// Homes whose local auto prune notice was shown, with when, guarded by its mutex
var localAutoPruneNotified = struct {
	sync.Mutex
	shown map[string]float64
}{shown: map[string]float64{}}

// One sticky auto prune notice per profile per snooze window for a desktop or TUI local session
func publishLocalAutoPruneNotice(ctx context.Context, a *SessionAuthority, ref SessionRef) {
	live, ok := a.live(ref.SessionID)
	if !ok || live.Source == nil || live.Source.Platform != PlatformLocal {
		return
	}
	policy, err := policyForSource(ctx, a.Runner, live.Source)
	if err != nil {
		if runtimeStoreErrorOf(err) == nil {
			slog.Error("local auto-prune notice failed", "error", err)
		}
		return
	}
	if policy == nil || (policy.Source != "tui" && policy.Source != "gui") {
		return
	}
	home := filepath.Dir(a.DB.Path())
	localAutoPruneNotified.Lock()
	if !state.AutoPruneNoticeDue(ctx, home, localAutoPruneNotified.shown) {
		localAutoPruneNotified.Unlock()
		return
	}
	localAutoPruneNotified.shown[home] = float64(time.Now().UnixNano()) / 1e9
	localAutoPruneNotified.Unlock()
	advice, err := state.AutoPruneAdvise(ctx, home)
	if err != nil {
		slog.Error("local auto-prune notice failed", "error", err)
	}
	delivered := err == nil && advice != nil && state.AutoPruneShouldAsk(ctx, home) != nil &&
		live.EventStream.Publish(ref.SessionID, state.AutoPruneNoticePayload(*advice), "notification.show", nil)
	if !delivered {
		localAutoPruneNotified.Lock()
		delete(localAutoPruneNotified.shown, home)
		localAutoPruneNotified.Unlock()
	}
}

// Low disk auto prune question for this viewer, off the resume reply's path
func (c *AuthorityConnection) spawnNotice(ctx context.Context, ref SessionRef) {
	scoped := context.WithoutCancel(ctx)
	go publishLocalAutoPruneNotice(scoped, c.authority, ref)
}
