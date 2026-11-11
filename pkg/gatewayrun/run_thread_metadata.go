package gatewayrun

import (
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Lower cased trimmed platform value of an enum or raw string
func gatewayPlatformValue(platform gateway.Platform) string {
	return pystr.Lower(pystr.Strip(string(platform)))
}

// Marks Discord lifecycle and status sends as non conversational, other platforms unchanged
func nonConversationalMetadata(metadata map[string]any, platform gateway.Platform) map[string]any {
	if gatewayPlatformValue(platform) != "discord" {
		return metadata
	}
	merged := make(map[string]any, len(metadata)+1)
	for k, v := range metadata {
		merged[k] = v
	}
	merged["non_conversational"] = true
	return merged
}

// Adapter able to describe a Telegram private DM topic lane
type dmTopicInfoAdapter interface {
	GetDMTopicInfo(chatID, threadID string) (any, error)
}

// True when the target is a Telegram private DM topic lane
func isTelegramDMTopicTarget(platform gateway.Platform, chatID string, threadID *string, chatType string, adapter any) bool {
	if platform != gateway.PlatformTelegram || threadID == nil {
		return false
	}
	if chatType == "dm" {
		return true
	}
	if lookup, ok := adapter.(dmTopicInfoAdapter); ok && adapter != nil && chatID != "" {
		info, err := lookup.GetDMTopicInfo(chatID, *threadID)
		if err != nil {
			inboundLog().Debug("Failed to inspect Telegram DM topic metadata", "exc_info", err)
			return false
		}
		_, isDict := info.(map[string]any)
		return isDict
	}
	return false
}

// Thread metadata for a synthetic send that only has routing state, nil without a thread
func (r *GatewayRunner) threadMetadataForTarget(platform gateway.Platform, chatID string, threadID *string, chatType string, replyToMessageID *string, adapter any) map[string]any {
	if threadID == nil {
		return nil
	}
	metadata := map[string]any{"thread_id": *threadID}
	if isTelegramDMTopicTarget(platform, chatID, threadID, chatType, adapter) {
		metadata["telegram_dm_topic_reply_fallback"] = true
		if tid := *threadID; tid != "" && tid != "1" {
			metadata["direct_messages_topic_id"] = tid
		}
		if replyToMessageID != nil {
			metadata["telegram_reply_to_message_id"] = *replyToMessageID
		}
	}
	if platform == gateway.PlatformSlack && replyToMessageID != nil {
		metadata["message_id"] = *replyToMessageID
	}
	return metadata
}

// Inbound route anchors kept on durable delivery metadata after the source is gone
func sourceRouteMetadata(source *gateway.SessionSource, metadata map[string]any) map[string]any {
	anchors := map[string]any{}
	if source.ScopeID != nil && *source.ScopeID != "" {
		anchors["scope_id"] = *source.ScopeID
	}
	if source.ParentChatID != nil && *source.ParentChatID != "" {
		anchors["parent_chat_id"] = *source.ParentChatID
	}
	if len(anchors) == 0 {
		return metadata
	}
	merged := make(map[string]any, len(metadata)+len(anchors))
	for k, v := range metadata {
		merged[k] = v
	}
	for k, v := range anchors {
		merged[k] = v
	}
	return merged
}

// Metadata platforms need for a thread aware reply to the source, with Slack egress identity and routed profile
func (r *GatewayRunner) threadMetadataForSource(source *gateway.SessionSource, replyToMessageID *string) map[string]any {
	if source == nil {
		return nil
	}
	anchor := replyToMessageID
	if anchor == nil || *anchor == "" {
		anchor = source.MessageID
	}
	metadata := r.threadMetadataForTarget(source.Platform, source.ChatID, source.ThreadID, source.ChatType, anchor, nil)
	if source.Platform == gateway.PlatformSlack {
		teamID := source.ScopeID != nil && *source.ScopeID != ""
		userID := source.UserID != nil && *source.UserID != ""
		if teamID || userID {
			merged := map[string]any{}
			for k, v := range metadata {
				merged[k] = v
			}
			if teamID {
				merged["slack_team_id"] = *source.ScopeID
				if _, ok := merged["scope_id"]; !ok {
					merged["scope_id"] = *source.ScopeID
				}
			}
			if userID {
				if _, ok := merged["user_id"]; !ok {
					merged["user_id"] = *source.UserID
				}
			}
			metadata = merged
		}
	}
	metadata = sourceRouteMetadata(source, metadata)
	profile := ""
	if source.Profile != nil {
		profile = pystr.Strip(*source.Profile)
	}
	if profile != "" && metadata != nil {
		merged := make(map[string]any, len(metadata)+1)
		for k, v := range metadata {
			merged[k] = v
		}
		merged["icebath_profile"] = profile
		metadata = merged
	}
	return metadata
}
