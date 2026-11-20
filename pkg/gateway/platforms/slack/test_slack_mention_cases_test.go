package slack

import (
	"strings"
	"testing"
)

const mentionBotUserID = "U_BOT_123"

func TestMentionDetectionTextIgnoresQuotedBlockkitMention(t *testing.T) {
	event := map[string]any{
		"text": "please review",
		"blocks": []any{map[string]any{"type": "rich_text", "elements": []any{
			map[string]any{"type": "rich_text_quote", "elements": []any{
				map[string]any{"type": "rich_text_section", "elements": []any{
					map[string]any{"type": "text", "text": "Contains "},
					map[string]any{"type": "user", "user_id": mentionBotUserID},
					map[string]any{"type": "text", "text": " in quoted text"},
				}},
			}},
		}}},
	}
	if strings.Contains(slackMentionDetectionText(event), "<@"+mentionBotUserID+">") {
		t.Fatal("quoted mention counted")
	}
}
