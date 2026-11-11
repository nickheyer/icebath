package gatewayrun

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
)

// Wait for the scheduled auto prune post to report delivery
const autoPrunePostWait = 60 * time.Second

// Served profile and its home, a nil profile standing for the launch profile
type autoPruneHome struct {
	profile *string
	home    string
}

// Delivery outcome of one scheduled auto prune post
type autoPrunePostResult struct {
	delivered bool
	err       error
}

// Same profile, nil matching only nil
func sameProfile(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// One housekeeping tick posting the low disk auto prune question to each reachable undecided profile, snoozing on delivery
func maybePostAutoPrunePrompt(ctx context.Context, r *GatewayRunner, loop gateway.EventLoop) error {
	if r == nil || loop == nil {
		return nil
	}
	targets := r.servedHomeChannelTransports(ctx)
	reachable := func(profile *string) bool {
		for _, t := range targets {
			if sameProfile(t.profile, profile) {
				return true
			}
		}
		return false
	}
	homes := []autoPruneHome{{home: home.RoutingProcess()}}
	if r.servedProfileHomes != nil {
		for name, profileHome := range r.servedProfileHomes.All() {
			homes = append(homes, autoPruneHome{profile: &name, home: profileHome})
		}
	}
	for _, entry := range homes {
		if !reachable(entry.profile) || state.AutoPruneShouldAsk(ctx, entry.home) == nil {
			continue
		}
		advice, err := state.AutoPruneAdvise(ctx, entry.home)
		if err != nil {
			return err
		}
		if advice == nil {
			continue
		}
		outcome := make(chan autoPrunePostResult, 1)
		base := context.WithoutCancel(ctx)
		profile, profileHome, posted := entry.profile, entry.home, *advice
		if err := loop.CallSoonThreadsafe(func() {
			spawnGatewayTask(base, "auto-prune-prompt", func(c context.Context) (any, error) {
				return postAutoPrunePrompt(c, r, profile, profileHome, posted)
			}).addDoneCallback(func(done *gatewayFuture) {
				value, err := done.wait(context.Background())
				delivered, _ := value.(bool)
				outcome <- autoPrunePostResult{delivered: delivered, err: err}
			})
		}); err != nil {
			housekeepingDiskLog().Debug(fmt.Sprintf("Auto-prune prompt scheduling error: %s", err))
			continue
		}
		select {
		case res := <-outcome:
			if res.err != nil {
				return res.err
			}
			if res.delivered {
				if err := state.AutoPruneSnooze(ctx, entry.home, state.AutoPruneSnoozeSeconds); err != nil {
					return err
				}
			}
		case <-time.After(autoPrunePostWait):
			return &gateway.PyError{Kind: "TimeoutError", Msg: ""}
		}
	}
	return nil
}

// Sends the auto prune question to the profile's home channels, true when at least one send was delivered
func postAutoPrunePrompt(ctx context.Context, r *GatewayRunner, profile *string, profileHome string, advice state.PruneAdvice) (bool, error) {
	scoped := ctx
	if profile != nil {
		scoped = gateway.ProfileRuntimeScope(ctx, profileHome, true)
	}
	delivered := false
	for _, target := range r.servedHomeChannelTransports(scoped) {
		if !sameProfile(target.profile, profile) {
			continue
		}
		prefix := "/"
		if adapter, ok := target.transport.Adapter.(gateway.PlatformAdapter); ok && adapter != nil && adapter.Base() != nil {
			prefix = adapter.Base().Traits.TypedCommandPrefix
		}
		message := "⚠ " + strings.Join(state.AutoPruneAdviceLines(advice, prefix), "\n")
		var sent []bool
		platform, homeChannel, transport := target.platform, target.home, target.transport
		if _, err := config.PresentNotification(scoped, func(c context.Context) error {
			sent = append(sent, r.sendHomeChannelMessage(c, platform, homeChannel, transport, message, "Auto-prune prompt failed for %s:%s: %s"))
			return nil
		}, string(platform), true, nil); err != nil {
			return delivered, err
		}
		for _, ok := range sent {
			delivered = delivered || ok
		}
	}
	return delivered, nil
}
