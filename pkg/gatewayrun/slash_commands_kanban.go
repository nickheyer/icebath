package gatewayrun

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/kanban"
)

// Task id of the kanban CLI create success line
var kanbanCreatedRe = pyre.MustCompile(`Created\s+(t_[0-9a-f]+)\b`)

// Kanban CLI output for the slash arguments, auto subscribing the chat to a task it created
func (r *GatewayRunner) handleKanbanCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	text := pystr.LstripChars(pystr.Strip(event.Text), "/")
	if strings.HasPrefix(text, "kanban") {
		text = pystr.Lstrip(text[len("kanban"):])
	}
	var requestedBoard *string
	action := ""
	if text != "" {
		tokens, err := pyshlex.SplitPOSIX(text, false)
		if err != nil {
			return "", err
		}
		for i := 0; i < len(tokens); i++ {
			tok := tokens[i]
			if tok == "--board" {
				if i+1 < len(tokens) {
					i++
					board := tokens[i]
					requestedBoard = &board
				}
			} else if strings.HasPrefix(tok, "--board=") {
				board := strings.SplitN(tok, "=", 2)[1]
				requestedBoard = &board
			} else {
				action = tok
				break
			}
		}
	}
	output, err := kanban.RunSlash(ctx, text)
	if err != nil {
		return i18n.T(ctx, "gateway.kanban.error_prefix", "", map[string]any{"error": pyrepr.ExceptionMessage(err)}), nil
	}
	if action == "create" && output != "" {
		if m, _ := kanbanCreatedRe.FindStringMatch(output); m != nil {
			taskID := m.GroupByNumber(1).String()
			subscribed, err := r.kanbanAutoSubscribe(ctx, event, taskID, requestedBoard)
			if err != nil {
				runLog().Warn(fmt.Sprintf("kanban create auto-subscribe failed: %s", pyrepr.ExceptionMessage(err)))
			} else if subscribed {
				output = pystr.Rstrip(output) + "\n" + i18n.T(ctx, "gateway.kanban.subscribed_suffix", "", map[string]any{"task_id": taskID})
			}
		}
	}
	if pystr.Len(output) > 3800 {
		output = pystr.Slice(output, 0, 3800) + "\n" + i18n.T(ctx, "gateway.kanban.truncated_suffix", "", nil)
	}
	if output == "" {
		return i18n.T(ctx, "gateway.kanban.no_output", "", nil), nil
	}
	return output, nil
}

// Subscribes the event chat to notify and wake events of the task, false when it has no route back
func (r *GatewayRunner) kanbanAutoSubscribe(ctx context.Context, event *gateway.MessageEvent, taskID string, requestedBoard *string) (bool, error) {
	source := event.Source
	field := func(v *string) *string {
		if v == nil || *v == "" {
			return nil
		}
		text := *v
		return &text
	}
	platformStr, chatType := "", (*string)(nil)
	var chatID *string
	if source != nil {
		platformStr = pystr.Lower(string(source.Platform))
		chatID = field(&source.ChatID)
		chatType = field(&source.ChatType)
	}
	var deliveryMetadata any
	if metadata := r.replyMetadata(event); len(metadata) > 0 {
		if chatType != nil {
			if _, ok := metadata["chat_type"]; !ok {
				metadata["chat_type"] = *chatType
			}
		}
		deliveryMetadata = metadata
	}
	if platformStr == "" || chatID == nil {
		return false, nil
	}
	notifierProfile := field(source.Profile)
	if notifierProfile == nil {
		profile := r.kanbanNotifierProfile
		if profile == "" {
			profile = r.activeProfileName(ctx)
		}
		notifierProfile = &profile
	}
	mode := "notify+wake"
	conn, err := kanbanConnect(ctx, "", requestedBoard)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	err = kanban.AddNotifySub(ctx, conn, kanban.NotifySubOptions{TaskID: taskID, Platform: platformStr, ChatID: *chatID,
		ChatType: chatType, ThreadID: field(source.ThreadID), UserID: field(source.UserID), UserIDAlt: field(source.UserIDAlt),
		NotifierProfile: notifierProfile, DeliveryMode: &mode, DeliveryMetadata: deliveryMetadata})
	if err != nil {
		return false, err
	}
	return true, nil
}
