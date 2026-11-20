package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Headless clarify reply of a finite turn whose viewer cannot answer
func FiniteClarifyReply() *pyjson.Object { return finiteClarifyReply() }

// Context bound to an admission's finite flag, unattended only when the turn is finite too
func FiniteTurnScope(ctx context.Context, finite any, unattended bool) context.Context {
	return finiteTurnScope(ctx, finite, unattended)
}
