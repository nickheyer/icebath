package gateway

import (
	"context"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Headless clarify reply of a finite turn whose viewer cannot answer
func FiniteClarifyReply() *jsonutil.Object { return finiteClarifyReply() }

// Context bound to an admission's finite flag, unattended only when the turn is finite too
func FiniteTurnScope(ctx context.Context, finite any, unattended bool) context.Context {
	return finiteTurnScope(ctx, finite, unattended)
}
