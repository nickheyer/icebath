package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Turn context carrying the session's settings layer and the login choices its stored row holds at the turn start, the boundary a pin applies at
func (t *TurnRunner) sessionScope(ctx context.Context) context.Context {
	tc := t.ctx
	if tc.sessionID == "" {
		return ctx
	}
	ctx = settings.WithSession(ctx, tc.sessionID)
	db := t.runner.sessionDB(ctx)
	if db == nil {
		return ctx
	}
	cfg, err := db.DB().SessionModelConfig(ctx, tc.sessionID)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("session %s: its login choices could not be read, the profile defaults apply this turn: %v", tc.sessionID, err))
		return ctx
	}
	return provider.WithSessionLogins(ctx, provider.SessionLoginsFromModelConfig(cfg))
}

// Publishes an exhausted pinned login of this turn's session to its viewers as login.exhausted
func (t *TurnRunner) publishLoginExhausted(ev provider.LoginExhaustedEvent) {
	ev.SessionID = t.ctx.sessionID
	payload, _ := tuigateway.Plain(tuihost.LoginExhaustedWire(ev)).(map[string]any)
	t.publishExecution(tuigateway.LoginExhausted.Name(), payload)
}

// Publishes login.exhausted for a turn whose route resolution refused an exhausted pinned login before any agent ran
func (t *TurnRunner) noteResolutionLoginExhausted(ctx context.Context, err error) {
	if ev := provider.PinnedLoginExhaustion(provider.LoginEnv(ctx), err); ev != nil {
		t.publishLoginExhausted(*ev)
	}
}
