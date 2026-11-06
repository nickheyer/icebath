package telegram

import (
	"strings"
	"testing"
)

type contextHostFake struct {
	username string
	hasBot   bool
	group    bool
	entities map[*ContextMessage][2][]MessageEntity
}

func (h *contextHostFake) EntitySources(m *ContextMessage) []EntitySource {
	e := h.entities[m]
	return []EntitySource{{firstText(m.Text), e[0]}, {firstText(m.Caption), e[1]}}
}

func (h *contextHostFake) EntityType(e MessageEntity) string {
	s, _ := e.Type.(string)
	return s
}

func (h *contextHostFake) EntitySpan(text string, e MessageEntity) string {
	r := []rune(text)
	off, _ := e.Offset.(int)
	n, _ := e.Length.(int)
	return string(r[off : off+n])
}

func (h *contextHostFake) Bot() (any, bool)           { return 999, h.hasBot }
func (h *contextHostFake) CurrentBotUsername() string { return h.username }
func (h *contextHostFake) IsGroupChat(*ContextMessage) bool {
	return h.group
}

func (h *contextHostFake) IsReplyToBot(m *ContextMessage) bool {
	r := m.ReplyToMessage
	return r != nil && r.FromUser != nil && r.FromUser.ID == 999
}

func (h *contextHostFake) MessageMentionsBot(m *ContextMessage) bool {
	for _, se := range mentionEntities(h, m) {
		if h.EntityType(se.entity) == "mention" && strings.EqualFold(h.EntitySpan(se.text, se.entity), "@"+h.username) {
			return true
		}
	}
	return false
}

func (h *contextHostFake) MessageMatchesMentionPatterns(m *ContextMessage) bool {
	return strings.HasPrefix(strings.ToLower(firstText(m.Text, m.Caption)), "hermes")
}

func (h *contextHostFake) CleanBotTriggerText(text *string) *string {
	if text == nil {
		return nil
	}
	out := strings.TrimSpace(strings.ReplaceAll(*text, "@"+h.username, ""))
	return &out
}

func sp(s string) *string { return &s }

func mentionOf(text, mention string) MessageEntity {
	return MessageEntity{Type: "mention", Offset: len([]rune(text[:strings.Index(text, mention)])), Length: len([]rune(mention))}
}

func TestGroupReplyExpectedFollowsWhoTheGroupMessageAddresses(t *testing.T) {
	host := &contextHostFake{username: "hermes_bot", hasBot: true, entities: map[*ContextMessage][2][]MessageEntity{}}
	group := func(text *string, mentions ...string) *ContextMessage {
		m := &ContextMessage{Text: text, MessageID: 42, FromUser: &TopicUser{ID: 111}}
		var ents []MessageEntity
		for _, mention := range mentions {
			ents = append(ents, mentionOf(*text, mention))
		}
		host.entities[m] = [2][]MessageEntity{ents, nil}
		return m
	}
	replying := func(m *ContextMessage, userID any, messageID any, senderChat any) *ContextMessage {
		r := &ContextMessage{Text: sp("earlier"), MessageID: messageID, SenderChat: senderChat}
		if userID != nil {
			r.FromUser = &TopicUser{ID: userID}
		}
		m.ReplyToMessage = r
		return m
	}
	toBot := group(sp("sure, saturday"))
	toBot.ReplyToMessage = &ContextMessage{FromUser: &TopicUser{ID: 999}, MessageID: 10, Text: sp("previous bot reply")}
	caption := group(sp(""))
	caption.Caption = sp("/status")
	topic := replying(group(sp("anyone up for dinner?")), 222, 5, nil)
	topic.MessageThreadID, topic.IsTopicMessage = 5, true
	voice, audio, note, photo := group(nil), group(nil), group(nil), group(nil)
	voice.Voice, audio.Audio, note.VideoNote = struct{}{}, struct{}{}, struct{}{}
	captioned := group(nil)
	captioned.Caption, captioned.Voice = sp("the fridge arrives on saturday"), struct{}{}
	T, F := true, false
	cases := []struct {
		id       string
		msg      *ContextMessage
		chatType string
		want     *bool
	}{
		{"mention", group(sp("@hermes_bot when is rent due?"), "@hermes_bot"), "group", &T},
		{"reply-to-bot", toBot, "group", &T},
		{"wake-word", group(sp("hermes, add milk to the list")), "group", &T},
		{"caption-command", caption, "group", &T},
		{"indented-command", group(sp(" /status")), "group", &T},
		{"dm", group(sp("hello")), "dm", &T},
		{"reply-to-other-member", replying(group(sp("yes, saturday")), 222, 10, nil), "group", &F},
		{"opens-with-other-mention", group(sp("@alice are we buying the fridge?"), "@alice"), "group", &F},
		{"chatter", group(sp("the fridge arrives on saturday")), "group", &F},
		{"question", group(sp("are we buying the fridge on saturday?")), "group", nil},
		{"mentions-other-mid-question", group(sp("what did @alice say?"), "@alice"), "group", nil},
		{"forum-topic-root", topic, "group", nil},
		{"reply-to-unknown-author", replying(group(sp("agreed?")), nil, 10, nil), "group", nil},
		{"reply-to-unknown-author-statement", replying(group(sp("agreed")), nil, 10, nil), "group", nil},
		{"reply-to-channel-autoforward", replying(group(sp("when does it start?")), 777000, 10, -1009), "group", nil},
		{"reply-to-anonymous-admin", replying(group(sp("agreed")), 1087968824, 10, -100), "group", nil},
		{"voice-note", voice, "group", nil},
		{"audio-file", audio, "group", nil},
		{"video-note", note, "group", nil},
		{"captioned-voice-chatter", captioned, "group", &F},
		{"photo-without-caption", photo, "group", &F},
		{"channel", group(sp("hello")), "channel", nil},
	}
	for _, c := range cases {
		got := GroupReplyExpected(host, c.msg, c.chatType)
		if (got == nil) != (c.want == nil) || got != nil && *got != *c.want {
			t.Fatalf("%s: got %v want %v", c.id, got, c.want)
		}
	}
	host.hasBot = false
	if got := GroupReplyExpected(host, cases[8].msg, "group"); got != nil {
		t.Fatalf("botless chatter = %v", *got)
	}
}

func TestGroupTriggerTextAndIdentityPrompt(t *testing.T) {
	host := &contextHostFake{username: "research_bot", hasBot: true, group: true, entities: map[*ContextMessage][2][]MessageEntity{}}
	text := "@research_bot @ops_bot hi"
	multi := &ContextMessage{Text: sp(text)}
	host.entities[multi] = [2][]MessageEntity{{mentionOf(text, "@research_bot"), mentionOf(text, "@ops_bot")}, nil}
	if got := GroupTriggerText(host, multi, sp(text)); *got != text {
		t.Fatalf("multi bot trigger = %q", *got)
	}
	sole := &ContextMessage{Text: sp("@research_bot hi")}
	host.entities[sole] = [2][]MessageEntity{{mentionOf("@research_bot hi", "@research_bot")}, nil}
	if got := GroupTriggerText(host, sole, sole.Text); *got != "hi" {
		t.Fatalf("sole trigger = %q", *got)
	}
	host.username = "hermes_bot"
	textMention := &ContextMessage{Text: sp("Hermes hello")}
	host.entities[textMention] = [2][]MessageEntity{{{Type: "text_mention", Offset: 0, Length: 6, User: &TopicUser{ID: 999}}}, nil}
	if MentionsOtherParticipants(host, textMention) || StartsWithOtherParticipantMention(host, textMention) {
		t.Fatal("text_mention of this bot counted as another participant")
	}
	host.entities[textMention] = [2][]MessageEntity{{{Type: "text_mention", Offset: 0, Length: 6, User: &TopicUser{ID: 5}}}, nil}
	if !StartsWithOtherParticipantMention(host, textMention) {
		t.Fatal("text_mention of another user missed")
	}
	want := "Keep answers concise.\n\nYour Telegram bot username in this group: @hermes_bot. Mentions of other bots are not requests for you to relay the message."
	if got := GroupIdentityPrompt(host, multi, sp("Keep answers concise.")); got == nil || *got != want {
		t.Fatalf("prompt = %v", got)
	}
	if got := GroupIdentityPrompt(host, multi, nil); got == nil || !strings.HasPrefix(*got, "Your Telegram bot username") {
		t.Fatalf("bare prompt = %v", got)
	}
	host.group = false
	if got := GroupIdentityPrompt(host, multi, nil); got != nil {
		t.Fatalf("dm prompt = %q", *got)
	}
	host.group, host.username = true, ""
	if got := GroupIdentityPrompt(host, multi, sp("p")); *got != "p" {
		t.Fatalf("nameless prompt = %q", *got)
	}
}
