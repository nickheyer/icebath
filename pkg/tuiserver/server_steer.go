package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Bound transport and live record when that transport is attached to the session, both nil otherwise
func currentSessionSteerAuthority(ctx context.Context, sessionID string) (tuihost.Transport, *tuihost.Session) {
	transport := tuihost.CurrentTransport(ctx)
	if transport == nil || sessionID == "" {
		return nil, nil
	}
	session := tuihost.SteerAuthoritySession(sessionID, transport, currentRuntimeSessionRecord(ctx))
	if session == nil {
		return nil, nil
	}
	return transport, session
}

// Steer authority of the bound request as untyped values, both nil when the transport holds none
func SessionSteerAuthority(ctx context.Context, sessionID string) (any, any) {
	transport, session := currentSessionSteerAuthority(ctx, sessionID)
	if transport == nil || session == nil {
		return nil, nil
	}
	return transport, session
}

type runtimeSessionRecordKey struct{}

// Exact session record the current turn executes on, nil outside a turn
func currentRuntimeSessionRecord(ctx context.Context) *tuihost.Session {
	session, _ := ctx.Value(runtimeSessionRecordKey{}).(*tuihost.Session)
	return session
}

type currentRPCMethodKey struct{}
