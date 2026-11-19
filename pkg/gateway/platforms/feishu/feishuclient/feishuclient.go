// Package feishuclient carries the lark client of a feishu comment agent turn to the feishu doc and drive tools
package feishuclient

import (
	"context"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
)

// Open Platform request the feishu doc and drive tools send, the lark SDK client of the feishu adapter in production
type API interface {
	Do(ctx context.Context, req *larkcore.ApiReq) (*larkcore.ApiResp, error)
}

type clientKey struct{}

// Context carrying the lark client the feishu doc and drive tools use for the comment agent turn
func With(ctx context.Context, client API) context.Context {
	return context.WithValue(ctx, clientKey{}, client)
}

// Lark client of the comment agent turn, nil outside one
func From(ctx context.Context) API {
	client, _ := ctx.Value(clientKey{}).(API)
	return client
}
