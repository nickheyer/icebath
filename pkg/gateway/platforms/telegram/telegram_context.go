package telegram

import (
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Message fields the group addressing helpers read, a nil pointer or nil value standing for an absent attribute
type ContextMessage struct {
	Text            *string
	Caption         *string
	ReplyToMessage  *ContextMessage
	IsTopicMessage  bool
	MessageID       any
	MessageThreadID any
	FromUser        *TopicUser
	SenderChat      any
	Voice           any
	VideoNote       any
	Audio           any
}

// Message text or caption paired with the entities whose offsets index into that same text
type EntitySource struct {
	Text     string
	Entities []MessageEntity
}

// Adapter surface the group addressing helpers call for entities, bot identity, mentions and trigger cleanup
type ContextHost interface {
	EntitySources(message *ContextMessage) []EntitySource
	EntityType(entity MessageEntity) string
	EntitySpan(sourceText string, entity MessageEntity) string
	Bot() (id any, ok bool)
	CurrentBotUsername() string
	IsReplyToBot(message *ContextMessage) bool
	MessageMentionsBot(message *ContextMessage) bool
	MessageMatchesMentionPatterns(message *ContextMessage) bool
	IsGroupChat(message *ContextMessage) bool
	CleanBotTriggerText(text *string) *string
}

type sourcedEntity struct {
	text   string
	entity MessageEntity
}

// Every mention and text_mention entity of the text and caption with the text it indexes
func mentionEntities(host ContextHost, message *ContextMessage) []sourcedEntity {
	var out []sourcedEntity
	for _, src := range host.EntitySources(message) {
		for _, entity := range src.Entities {
			if t := host.EntityType(entity); t == "mention" || t == "text_mention" {
				out = append(out, sourcedEntity{src.Text, entity})
			}
		}
	}
	return out
}

func isOtherParticipant(host ContextHost, sourceText string, entity MessageEntity) bool {
	if host.EntityType(entity) == "text_mention" {
		botID, ok := host.Bot()
		if !ok {
			botID = nil
		}
		return entity.User != nil && !pyrepr.Equal(entity.User.ID, botID)
	}
	handle := pystr.Lower(pystr.LstripChars(pystr.Strip(host.EntitySpan(sourceText, entity)), "@"))
	return handle != "" && handle != host.CurrentBotUsername()
}

// Reports whether any mention or text_mention entity in text or caption names someone other than this bot
func MentionsOtherParticipants(host ContextHost, message *ContextMessage) bool {
	for _, se := range mentionEntities(host, message) {
		if isOtherParticipant(host, se.text, se.entity) {
			return true
		}
	}
	return false
}

// Reports whether the text opens with a mention entity naming someone other than this bot after whitespace
func StartsWithOtherParticipantMention(host ContextHost, message *ContextMessage) bool {
	for _, se := range mentionEntities(host, message) {
		lead := utf8.RuneCountInString(se.text) - utf8.RuneCountInString(pystr.Lstrip(se.text))
		if pyrepr.Equal(se.entity.Offset, lead) && isOtherParticipant(host, se.text, se.entity) {
			return true
		}
	}
	return false
}

// Replied message, nil when absent or when it is only the forum topic root anchor
func quotedReply(message *ContextMessage) *ContextMessage {
	replied := message.ReplyToMessage
	if replied == nil || (message.IsTopicMessage && pyrepr.Equal(replied.MessageID, message.MessageThreadID)) {
		return nil
	}
	return replied
}

func firstText(values ...*string) string {
	for _, v := range values {
		if v != nil && *v != "" {
			return *v
		}
	}
	return ""
}

// Whether an admitted message needs a reply, nil keeping the visible silence fallback for undecided group chatter
func GroupReplyExpected(host ContextHost, message *ContextMessage, chatType string) *bool {
	yes, no := true, false
	if chatType != "group" {
		if chatType == "dm" {
			return &yes
		}
		return nil
	}
	text := firstText(message.Text, message.Caption)
	if strings.HasPrefix(pystr.Lstrip(text), "/") || host.IsReplyToBot(message) || host.MessageMentionsBot(message) ||
		host.MessageMatchesMentionPatterns(message) {
		return &yes
	}
	if _, ok := host.Bot(); !ok || host.CurrentBotUsername() == "" {
		return nil
	}
	if replied := quotedReply(message); replied != nil {
		if replied.FromUser == nil || replied.SenderChat != nil {
			return nil
		}
		return &no
	}
	if StartsWithOtherParticipantMention(host, message) {
		return &no
	}
	spoken := text == "" && (message.Voice != nil || message.VideoNote != nil || message.Audio != nil)
	if strings.Contains(text, "?") || spoken {
		return nil
	}
	return &no
}

// Trigger text with our own handle stripped only when this bot is the sole addressee of the message
func GroupTriggerText(host ContextHost, message *ContextMessage, text *string) *string {
	if host.IsGroupChat(message) && MentionsOtherParticipants(host, message) {
		return text
	}
	return host.CleanBotTriggerText(text)
}

// Channel prompt extended with the session stable bot username line for group chats where the username is known
func GroupIdentityPrompt(host ContextHost, message *ContextMessage, channelPrompt *string) *string {
	if _, ok := host.Bot(); !host.IsGroupChat(message) || !ok {
		return channelPrompt
	}
	username := host.CurrentBotUsername()
	if username == "" {
		return channelPrompt
	}
	identity := "Your Telegram bot username in this group: @" + username + ". " +
		"Mentions of other bots are not requests for you to relay the message."
	if channelPrompt != nil && *channelPrompt != "" {
		joined := *channelPrompt + "\n\n" + identity
		return &joined
	}
	return &identity
}
