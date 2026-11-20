package gateway

import (
	"context"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Existing text and the new text joined by a newline when both are set, else the set one
func appendText(existing, newText string) string {
	if existing != "" {
		return existing + "\n" + newText
	}
	return newText
}

// Exact DM phrases asking for a gateway restart that become the restart slash command
var plaintextGatewayRestartPatterns = func() []*regexp.Regexp {
	ws := pyWhitespace
	s := "[" + ws + "]"
	return []*regexp.Regexp{
		regexp.MustCompile(`(?i)^(?:please` + s + `+)?restart` + s + `+(?:the` + s + `+)?gateway[.!?` + ws + `]*$`),
		regexp.MustCompile(`(?i)^(?:please` + s + `+)?restart` + s + `+(?:the` + s + `+)?icebath` + s + `+gateway[.!?` + ws + `]*$`),
		regexp.MustCompile(`(?i)^(?:please` + s + `+)?restart` + s + `+icebath[.!?` + ws + `]*$`),
	}
}()

// Rewrites an exact plaintext restart request in a DM into the restart command so it never reaches the agent
func CoercePlaintextGatewayCommand(event *MessageEvent) {
	if event == nil || event.MessageType != MessageTypeText {
		return
	}
	text := pystr.Strip(event.Text)
	if text == "" || strings.HasPrefix(text, "/") {
		return
	}
	if event.Source == nil || event.Source.ChatType != "dm" {
		return
	}
	for _, pattern := range plaintextGatewayRestartPatterns {
		if pattern.MatchString(text) {
			event.Text = "/restart"
			return
		}
	}
}

// One rendered approval button row with the label, the approval choice and the button style
type ExecApprovalAction struct {
	Label  string
	Choice string
	Style  string
}

// One exec approval prompt ready for a platform to render natively
type ExecApprovalPrompt struct {
	ChatID      string
	SessionKey  string
	Text        string
	Actions     []ExecApprovalAction
	Command     string
	Description string
	SmartDenied bool
	Metadata    map[string]any
	RequestID   *string
}

// Approval choices of the action rows in order
func (p *ExecApprovalPrompt) Choices() []string {
	out := make([]string, 0, len(p.Actions))
	for _, a := range p.Actions {
		out = append(out, a.Choice)
	}
	return out
}

// System notice reply that auto deletes after the TTL on platforms that delete messages, nil TTL uses the display default
type EphemeralReply struct {
	text       string
	TTLSeconds *int
}

// Ephemeral reply over the text with the optional TTL in seconds
func NewEphemeralReply(text string, ttlSeconds *int) *EphemeralReply {
	return &EphemeralReply{text: text, TTLSeconds: ttlSeconds}
}

// Underlying reply text
func (r *EphemeralReply) Text() string { return r.text }

// Reply text so the reply reads as plain text everywhere a handler result is treated as text
func (r *EphemeralReply) String() string { return r.text }

// Handler result is nil when already delivered, a string reply or an EphemeralReply
type MessageHandler func(ctx context.Context, event *MessageEvent) (any, error)

// New caption appended after a blank line unless an identical stripped caption is already there
func mergeCaption(existingText, newText string) string {
	if existingText == "" {
		return newText
	}
	stripped := pystr.Strip(newText)
	for _, c := range strings.Split(existingText, "\n\n") {
		if pystr.Strip(c) == stripped {
			return existingText
		}
	}
	return pystr.Strip(existingText + "\n\n" + newText)
}

// Inline flags padded with unset entries up to the attachment count
func paddedInlineFlags(msg *MessageEvent) []*bool {
	flags := append([]*bool(nil), msg.MediaTextInlined...)
	for len(flags) < len(msg.MediaURLs) {
		flags = append(flags, nil)
	}
	return flags
}

// Stores the pending event or merges it into the queued one, photo bursts and media always absorb and text appends when asked
func MergePendingMessageEvent(pending map[string]*MessageEvent, sessionKey string, event *MessageEvent, mergeText bool) {
	existing := pending[sessionKey]
	if existing != nil {
		existingType := existing.MessageType
		existingIsPhoto := existingType == MessageTypePhoto
		incomingIsPhoto := event.MessageType == MessageTypePhoto
		bothPhoto := existingIsPhoto && incomingIsPhoto
		incomingHasMedia := len(event.MediaURLs) > 0
		var incomingInlineFlags []*bool
		if incomingHasMedia {
			existing.MediaTextInlined = paddedInlineFlags(existing)
			incomingInlineFlags = paddedInlineFlags(event)
		}
		if bothPhoto || len(existing.MediaURLs) > 0 || incomingHasMedia {
			if bothPhoto || incomingHasMedia {
				existing.ExtendMedia(event)
				existing.MediaTextInlined = append(existing.MediaTextInlined, incomingInlineFlags...)
			}
			if event.Text != "" {
				existing.Text = mergeCaption(existing.Text, event.Text)
			}
			existing.AbsorbReplyExpected(event)
			if existingIsPhoto || incomingIsPhoto {
				existing.MessageType = MessageTypePhoto
			} else if existingType == MessageTypeText && event.MessageType != MessageTypeText {
				existing.MessageType = event.MessageType
			}
			existing.clearPendingSTT()
			return
		}
		bothText := existingType == MessageTypeText && event.MessageType == MessageTypeText
		if mergeText && bothText {
			if event.Text != "" {
				existing.Text = appendText(existing.Text, event.Text)
			}
			existing.AbsorbReplyExpected(event)
			return
		}
	}
	pending[sessionKey] = event
}

// Reports whether the error text looks like a transient network failure
func isRetryableError(errText string) bool {
	lowered := pystr.Lower(errText)
	for _, pat := range retryableErrorPatterns {
		if strings.Contains(lowered, pat) {
			return true
		}
	}
	return false
}

// Reports whether the error text classifies as a rate limit or flood cap
func isRateLimitedError(errText string) bool {
	return ClassifySendError(nil, errText) == "rate_limited"
}

// Reports read and write timeouts, which are neither retried nor sent again as plain text
func isTimeoutError(errText string) bool {
	lowered := pystr.Lower(errText)
	for _, pat := range []string{"timed out", "readtimeout", "writetimeout"} {
		if strings.Contains(lowered, pat) {
			return true
		}
	}
	return false
}

// Channel prompt for the channel id, then the parent id, with blank prompts counting as absent
func ResolveChannelPrompt(configExtra map[string]any, channelID string, parentID string) (string, bool) {
	raw := configExtra["channel_prompts"]
	if !pyrepr.Truthy(raw) {
		return "", false
	}
	prompts, ok := raw.(map[string]any)
	if !ok {
		return "", false
	}
	for _, key := range []string{channelID, parentID} {
		if key == "" {
			continue
		}
		prompt, present := prompts[key]
		if !present || prompt == nil {
			continue
		}
		if text := pystr.Strip(pyrepr.Str(prompt)); text != "" {
			return text, true
		}
	}
	return "", false
}

// Auto loaded skills bound to the channel id, then the parent id, deduplicated in order, ok false when none
func ResolveChannelSkills(configExtra map[string]any, channelID string, parentID string) ([]string, bool) {
	bindings, ok := configExtra["channel_skill_bindings"].([]any)
	if !ok || len(bindings) == 0 {
		return nil, false
	}
	for _, wanted := range []string{channelID, parentID} {
		if wanted == "" {
			continue
		}
		for _, raw := range bindings {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			id, present := entry["id"]
			if !present {
				id = ""
			}
			if pyrepr.Str(id) != wanted {
				continue
			}
			skills := entry["skills"]
			if !pyrepr.Truthy(skills) {
				skills = entry["skill"]
			}
			switch v := skills.(type) {
			case string:
				if s := pystr.Strip(v); s != "" {
					return []string{s}, true
				}
				return nil, false
			case []any:
				if len(v) == 0 {
					continue
				}
				seen := map[string]bool{}
				var out []string
				for _, name := range v {
					s, isStr := name.(string)
					if !isStr {
						continue
					}
					if nm := pystr.Strip(s); nm != "" && !seen[nm] {
						seen[nm] = true
						out = append(out, nm)
					}
				}
				return out, len(out) > 0
			}
		}
	}
	return nil, false
}
