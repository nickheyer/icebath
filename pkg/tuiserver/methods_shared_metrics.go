package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Counts one user friction action of a session against the model of its last turn
func tuiModelFriction(ctx context.Context, signal string, session *tuigateway.Session, turns int) {
	opts := observability.FrictionOptions{SessionID: session.Get("session_key"), HomeDir: strOr(session.Get("profile_home")), Turns: turns}
	if a := sessionAgent(session); a != nil {
		opts.Agent = a
	}
	observability.RecordModelFriction(ctx, signal, opts)
}
