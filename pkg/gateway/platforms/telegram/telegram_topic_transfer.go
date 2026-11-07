package telegram

import (
	"context"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Topic a root DM moves to, created true for a fresh topic and false for the lobby recovery fallback, ok false to decline
type TopicTransferFn = func(ctx context.Context, source *gateway.SessionSource) (threadID string, created bool, ok bool, err error)

// Adapter surface the root DM topic transfer reads
type TopicTransferHost interface {
	TopicTransferFn() TopicTransferFn
	IsSenderAuthorized(userID string, chatType, chatID *string, opts gateway.AuthorizationOpts) *bool
}

// Moves an authorized plain root DM onto the topic the transfer hook picks before session keying
func ApplyRootDMTopicTransfer(ctx context.Context, adapter TopicTransferHost, event *gateway.MessageEvent) {
	transfer := adapter.TopicTransferFn()
	source := event.Source
	if transfer == nil || source == nil || source.ChatType != "dm" {
		return
	}
	if event.AllowGatewayControl {
		gateway.CoercePlaintextGatewayCommand(event)
	}
	if cmd, _ := event.GetCommand(); event.Internal || cmd != "" {
		return
	}
	if key, ok := event.Metadata["gateway_session_key"]; ok && pystr.Strip(pyrepr.Str(truthyOrEmpty(key))) != "" {
		return
	}
	userID := ""
	if source.UserID != nil {
		userID = *source.UserID
	}
	chatType, chatID := source.ChatType, source.ChatID
	if authorized := adapter.IsSenderAuthorized(userID, &chatType, &chatID, gateway.AuthorizationOpts{}); authorized != nil && !*authorized {
		return
	}
	threadID, created, ok, err := transfer(ctx, source)
	if err != nil {
		slog.Debug("telegram topic transfer hook failed", "error", err)
		return
	}
	if !ok {
		return
	}
	if !created {
		event.Source = gateway.ReplaceSource(source, func(s *gateway.SessionSource) { s.ThreadID = &threadID })
		return
	}
	event.Source = gateway.ReplaceSource(source, func(s *gateway.SessionSource) { s.ThreadID, s.MessageID = &threadID, nil })
	empty := ""
	event.ReplyAnchorOverride = &empty
}

func truthyOrEmpty(v any) any {
	if pyrepr.Truthy(v) {
		return v
	}
	return ""
}
