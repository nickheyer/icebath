package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Bound transport and live record when that transport is attached to the session, both nil otherwise
func currentSessionSteerAuthority(ctx context.Context, sessionID string) (tuigateway.Transport, *tuigateway.Session) {
	transport := tuigateway.CurrentTransport(ctx)
	if transport == nil || sessionID == "" {
		return nil, nil
	}
	session := tuigateway.SteerAuthoritySession(sessionID, transport, currentRuntimeSessionRecord(ctx))
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

// Context whose turn executes on the given in memory session record
func withRuntimeSessionRecord(ctx context.Context, session *tuigateway.Session) context.Context {
	return context.WithValue(ctx, runtimeSessionRecordKey{}, session)
}

// Exact session record the current turn executes on, nil outside a turn
func currentRuntimeSessionRecord(ctx context.Context) *tuigateway.Session {
	session, _ := ctx.Value(runtimeSessionRecordKey{}).(*tuigateway.Session)
	return session
}

type currentRPCMethodKey struct{}

// Method being dispatched on this context, empty outside a dispatch
func currentRPCMethod(ctx context.Context) string {
	method, _ := ctx.Value(currentRPCMethodKey{}).(string)
	return method
}
