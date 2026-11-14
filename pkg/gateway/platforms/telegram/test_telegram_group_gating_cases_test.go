package telegram

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Adapter of the hermes _make_adapter helper, bot id 999 with the given handle, empty allow lists unless set
func gatingAdapter(t *testing.T, extra map[string]any, botUsername string) (*TelegramAdapter, *scriptedBot) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cfg := gateway.NewPlatformConfig()
	cfg.Enabled, cfg.Token = true, "999:test-token"
	for _, key := range []string{"allowed_topics", "allowed_chats", "group_allowed_chats"} {
		cfg.Extra[key] = []any{}
	}
	for k, v := range extra {
		cfg.Extra[k] = v
	}
	if botUsername == "" {
		botUsername = "hermes_bot"
	}
	a := NewTelegramAdapter(context.Background(), cfg)
	fake := &scriptedBot{}
	attachScriptedBot(t, a, fake)
	a.app.mu.Lock()
	a.app.botUser = &models.User{ID: 999, IsBot: true, FirstName: "Hermes", Username: botUsername}
	a.app.mu.Unlock()
	return a, fake
}

// Event sink standing in for the hermes AsyncMock message handler
type eventSink struct {
	mu     sync.Mutex
	events []*gateway.MessageEvent
	got    chan struct{}
}

func newEventSink(a *TelegramAdapter) *eventSink {
	s := &eventSink{got: make(chan struct{}, 16)}
	a.SetMessageHandler(func(_ context.Context, event *gateway.MessageEvent) (any, error) {
		s.mu.Lock()
		s.events = append(s.events, event)
		s.mu.Unlock()
		s.got <- struct{}{}
		return nil, nil
	})
	return s
}

// First event the handler received, failing when none arrives
func (s *eventSink) first(t *testing.T) *gateway.MessageEvent {
	t.Helper()
	select {
	case <-s.got:
	case <-time.After(5 * time.Second):
		t.Fatal("message handler never awaited")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.events[0]
}

func (s *eventSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

// Session store double of the hermes _FakeSessionStore
type observeRecorder struct {
	sources  []*gateway.SessionSource
	messages []observedLine
}

type observedLine struct {
	sessionID string
	message   map[string]any
	skipDB    bool
}

func (s *observeRecorder) GetOrCreateSession(_ context.Context, source *gateway.SessionSource, _, _ bool) (*gateway.SessionEntry, error) {
	s.sources = append(s.sources, source)
	return &gateway.SessionEntry{SessionID: "telegram-group-session"}, nil
}

func (s *observeRecorder) AppendToTranscript(_ context.Context, sessionID string, message map[string]any, skipDB bool) error {
	s.messages = append(s.messages, observedLine{sessionID, message, skipDB})
	return nil
}

// Options of the hermes _group_message helper
type groupMsgOpts struct {
	chatID      int64
	fromID      int64
	fromName    string
	threadID    int
	replyToBot  bool
	entities    []models.MessageEntity
	caption     string
	captionEnts []models.MessageEntity
}

// Group message of the hermes _group_message helper
func groupMessage(text string, o groupMsgOpts) *models.Message {
	if o.chatID == 0 {
		o.chatID = -100
	}
	if o.fromID == 0 {
		o.fromID = 111
	}
	if o.fromName == "" {
		o.fromName = "Alice Example"
	}
	first, last, _ := strings.Cut(o.fromName, " ")
	m := &models.Message{ID: 42, Text: text, Caption: o.caption, Entities: o.entities, CaptionEntities: o.captionEnts,
		MessageThreadID: o.threadID, IsTopicMessage: o.threadID != 0,
		Chat: models.Chat{ID: o.chatID, Type: "group", Title: "Test Group", IsForum: o.threadID != 0},
		From: &models.User{ID: o.fromID, FirstName: first, LastName: last}}
	if o.replyToBot {
		m.ReplyToMessage = &models.Message{ID: 10, Text: "previous bot reply", From: &models.User{ID: 999}}
	}
	return m
}

// Private message of the hermes _dm_message helper
func dmMessage(text string) *models.Message {
	return &models.Message{ID: 43, Text: text, Chat: models.Chat{ID: 111, Type: "private", FirstName: "Alice", LastName: "Example"},
		From: &models.User{ID: 111, FirstName: "Alice", LastName: "Example"}}
}

func mentionEntity(text, mention string) models.MessageEntity {
	return models.MessageEntity{Type: "mention", Offset: strings.Index(text, mention), Length: len(mention)}
}

func mentionEnts(text string, mentions ...string) []models.MessageEntity {
	out := make([]models.MessageEntity, 0, len(mentions))
	for _, m := range mentions {
		out = append(out, mentionEntity(text, m))
	}
	return out
}

// Message with independently controlled topic and forum flags
func forumMsg(chatID int64, threadID int, isTopic, isForum bool, chatType string) *models.Message {
	return &models.Message{ID: 42, Text: "hello", MessageThreadID: threadID, IsTopicMessage: isTopic,
		Chat: models.Chat{ID: chatID, Type: models.ChatType(chatType), Title: "T", IsForum: isForum},
		From: &models.User{ID: 111, FirstName: "Alice"}}
}

// Group message sent by another bot account
func botSenderMessage(text string, replyToBot bool, entities []models.MessageEntity) *models.Message {
	m := groupMessage(text, groupMsgOpts{fromID: 555, replyToBot: replyToBot, entities: entities})
	m.From.IsBot = true
	return m
}

func updateOf(id int64, m *models.Message) *Update {
	return &Update{UpdateID: id, Raw: &models.Update{ID: id, Message: m}}
}

func TestUnmentionedGroupMessagesCanBeObservedWithoutDispatching(t *testing.T) {
	ctx := context.Background()
	a, _ := gatingAdapter(t, map[string]any{"require_mention": true, "allowed_chats": []any{"-100"},
		"group_allowed_chats": []any{"-100"}, "observe_unmentioned_group_messages": true}, "")
	sink := newEventSink(a)
	store := &observeRecorder{}
	a.SetSessionStore(store)
	if err := a.handleTextMessage(ctx, updateOf(1001, groupMessage("side chatter", groupMsgOpts{}))); err != nil {
		t.Fatal(err)
	}
	if sink.count() != 0 {
		t.Fatal("observed chatter dispatched")
	}
	if len(store.messages) != 1 {
		t.Fatalf("messages %d", len(store.messages))
	}
	line := store.messages[0]
	if line.sessionID != "telegram-group-session" || line.skipDB {
		t.Fatalf("line %+v", line)
	}
	msg := line.message
	if msg["role"] != "user" || msg["content"] != "[Alice Example|111]\nside chatter" || msg["observed"] != true || msg["message_id"] != "42" {
		t.Fatalf("message %v", msg)
	}
	src := store.sources[0]
	if src.ChatID != "-100" || src.ChatType != "group" || src.UserID != nil || src.UserName != nil {
		t.Fatalf("source %+v", src)
	}
}

func TestObservedGroupContextUsesSharedSourceAndPromptForLaterMentions(t *testing.T) {
	ctx := context.Background()
	a, _ := gatingAdapter(t, map[string]any{"require_mention": true, "allowed_chats": []any{"-100"},
		"group_allowed_chats": []any{"-100"}, "observe_unmentioned_group_messages": true}, "")
	a.SetSessionStore(&observeRecorder{})
	text := "@hermes_bot what did Alice say?"
	msg := groupMessage(text, groupMsgOpts{fromID: 222, fromName: "Bob Example", entities: mentionEnts(text, "@hermes_bot")})
	id := int64(1003)
	event := a.buildMessageEvent(ctx, msg, gateway.MessageTypeText, &id)
	event.Text = *a.cleanBotTriggerText(&event.Text)
	prompt := "Existing topic prompt"
	event.ChannelPrompt = &prompt
	event = a.applyGroupObserveAttribution(ctx, event)
	if event.Source.ChatID != "-100" || event.Source.ChatType != "group" || event.Source.UserID != nil || event.Source.UserName != nil {
		t.Fatalf("source %+v", event.Source)
	}
	if event.Text != "[Bob Example|222]\nwhat did Alice say?" {
		t.Fatalf("text %q", event.Text)
	}
	for _, want := range []string{"Existing topic prompt", "observed Telegram group context", "current new message"} {
		if !strings.Contains(*event.ChannelPrompt, want) {
			t.Fatalf("prompt %q lacks %q", *event.ChannelPrompt, want)
		}
	}
}

func TestObservedGroupContextPreservesSlashCommandTextForDispatch(t *testing.T) {
	a, _ := gatingAdapter(t, map[string]any{"require_mention": true, "allowed_chats": []any{"-100"},
		"group_allowed_chats": []any{"-100"}, "observe_unmentioned_group_messages": true}, "")
	user, name, thread := "111", "Alice", "7"
	event := gateway.NewMessageEvent("/new@hermes_bot")
	event.MessageType = gateway.MessageTypeCommand
	event.Source = &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "-100", UserID: &user, UserName: &name, ChatType: "group", ThreadID: &thread}
	event.RawMessage = groupMessage("/new@hermes_bot", groupMsgOpts{entities: []models.MessageEntity{{Type: "bot_command", Offset: 0, Length: len("/new@hermes_bot")}}})
	attributed := a.applyGroupObserveAttribution(context.Background(), event)
	if attributed.Text != "/new@hermes_bot" {
		t.Fatalf("text %q", attributed.Text)
	}
	if cmd, ok := attributed.GetCommand(); !ok || cmd != "new" {
		t.Fatalf("command %q", cmd)
	}
	if attributed.Source.UserID == nil || *attributed.Source.UserID != "111" || attributed.Source.UserName == nil || *attributed.Source.UserName != "Alice" {
		t.Fatalf("source %+v", attributed.Source)
	}
	if attributed.ChannelPrompt == nil || !strings.Contains(*attributed.ChannelPrompt, "observed Telegram group context") {
		t.Fatal("observe prompt missing")
	}
}

// Asserts the gate verdict of a message
func expectGate(t *testing.T, a *TelegramAdapter, m *models.Message, want bool) {
	t.Helper()
	if got := a.shouldProcessMessage(context.Background(), m); got != want {
		t.Fatalf("should process %q = %v, want %v", m.Text, got, want)
	}
}

func expectObserve(t *testing.T, a *TelegramAdapter, m *models.Message, want bool) {
	t.Helper()
	if got := a.shouldObserveUnmentionedGroupMessage(context.Background(), m); got != want {
		t.Fatalf("should observe %q = %v, want %v", m.Text, got, want)
	}
}

func TestExplicitMultiBotMentionsRouteOnlyToNamedBots(t *testing.T) {
	text := "@research_bot @ops_bot hi"
	ents := mentionEnts(text, "@research_bot", "@ops_bot")
	def, _ := gatingAdapter(t, map[string]any{"require_mention": true}, "default_bot")
	research, _ := gatingAdapter(t, map[string]any{"require_mention": true}, "research_bot")
	ops, _ := gatingAdapter(t, map[string]any{"require_mention": true}, "ops_bot")
	expectGate(t, def, groupMessage(text, groupMsgOpts{replyToBot: true, entities: ents}), false)
	expectGate(t, research, groupMessage(text, groupMsgOpts{entities: ents}), true)
	expectGate(t, ops, groupMessage(text, groupMsgOpts{entities: ents}), true)
}

func TestEntitylessMultiBotMentionsStillRouteExclusively(t *testing.T) {
	text := "@research_bot @ops_bot hi"
	def, _ := gatingAdapter(t, map[string]any{"require_mention": true}, "default_bot")
	research, _ := gatingAdapter(t, map[string]any{"require_mention": true}, "research_bot")
	ops, _ := gatingAdapter(t, map[string]any{"require_mention": true}, "ops_bot")
	expectGate(t, def, groupMessage(text, groupMsgOpts{replyToBot: true}), false)
	expectGate(t, research, groupMessage(text, groupMsgOpts{}), true)
	expectGate(t, ops, groupMessage(text, groupMsgOpts{}), true)
}

func TestInternBotsIgnoreMessagesAddressedToOtherInternBot(t *testing.T) {
	text := "@Interntestnumber1bot you're not supposed to do the blog"
	test2, _ := gatingAdapter(t, map[string]any{"require_mention": false}, "Interntestnumber2bot")
	test1, _ := gatingAdapter(t, map[string]any{"require_mention": false}, "Interntestnumber1bot")
	expectGate(t, test2, groupMessage(text, groupMsgOpts{replyToBot: true}), false)
	expectGate(t, test1, groupMessage(text, groupMsgOpts{}), true)
}

func TestRawBotMentionFallbackDoesNotMatchEmailOrSubstring(t *testing.T) {
	a, _ := gatingAdapter(t, map[string]any{"require_mention": true}, "hermes_bot")
	expectGate(t, a, groupMessage("email ops@hermes_bot.example", groupMsgOpts{}), false)
	expectGate(t, a, groupMessage("prefix@hermes_bot hi", groupMsgOpts{}), false)
	expectGate(t, a, groupMessage("hi @hermes_bot", groupMsgOpts{}), true)
}

func TestExclusiveBotMentionsCanBeDisabledForLegacyGroups(t *testing.T) {
	a, _ := gatingAdapter(t, map[string]any{"require_mention": true, "exclusive_bot_mentions": false}, "default_bot")
	expectGate(t, a, groupMessage("@research_bot hi", groupMsgOpts{replyToBot: true}), true)
}

func TestFreeResponseTopicMessagesAreDispatchedNotObserved(t *testing.T) {
	a, _ := gatingAdapter(t, map[string]any{"require_mention": true, "allowed_chats": []any{"-200"}, "group_allowed_chats": []any{"-200"},
		"observe_unmentioned_group_messages": true, "free_response_topics": []any{"-200:31"}}, "")
	inTopic := groupMessage("hello everyone", groupMsgOpts{chatID: -200, threadID: 31})
	expectGate(t, a, inTopic, true)
	expectObserve(t, a, inTopic, false)
	other := groupMessage("side chatter", groupMsgOpts{chatID: -200, threadID: 32})
	expectGate(t, a, other, false)
	expectObserve(t, a, other, true)
}

func TestGuestModeAllowsOnlyDirectMentionsOutsideAllowedChats(t *testing.T) {
	a, _ := gatingAdapter(t, map[string]any{"require_mention": true, "allowed_chats": []any{"-200"}, "guest_mode": true,
		"mention_patterns": []any{`^\s*chompy\b`}}, "")
	expectGate(t, a, groupMessage("hi @hermes_bot", groupMsgOpts{chatID: -201, entities: mentionEnts("hi @hermes_bot", "@hermes_bot")}), true)
	expectGate(t, a, groupMessage("reply", groupMsgOpts{chatID: -201, replyToBot: true}), false)
	expectGate(t, a, groupMessage("chompy status", groupMsgOpts{chatID: -201}), false)
	expectGate(t, a, groupMessage("hello", groupMsgOpts{chatID: -201}), false)
}

func TestAllowedTopicsDropOtherForumTopicsBeforeOtherGates(t *testing.T) {
	a, _ := gatingAdapter(t, map[string]any{"require_mention": false, "allowed_chats": []any{"-100"}, "allowed_topics": []any{"8"}}, "")
	expectGate(t, a, groupMessage("hello", groupMsgOpts{chatID: -100, threadID: 8}), true)
	expectGate(t, a, groupMessage("hello", groupMsgOpts{chatID: -100, threadID: 11}), false)
	expectGate(t, a, groupMessage("hi @hermes_bot", groupMsgOpts{chatID: -100, threadID: 11, entities: mentionEnts("hi @hermes_bot", "@hermes_bot")}), false)
}

func TestGatingIgnoresNonForumReplyAnchorThreadId(t *testing.T) {
	a, _ := gatingAdapter(t, map[string]any{"require_mention": false, "free_response_chats": []any{"-200"}, "ignored_threads": []any{55}}, "")
	anchor := forumMsg(-200, 55, false, false, "group")
	expectGate(t, a, anchor, true)
	a2, _ := gatingAdapter(t, map[string]any{"require_mention": false, "allowed_chats": []any{"-200"}, "allowed_topics": []any{"1"}}, "")
	expectGate(t, a2, anchor, true)
}

func TestGatingForumGeneralTopicNormalizesToOne(t *testing.T) {
	general := forumMsg(-100, 0, false, true, "supergroup")
	a, _ := gatingAdapter(t, map[string]any{"require_mention": false, "allowed_chats": []any{"-100"}, "allowed_topics": []any{"1"}}, "")
	expectGate(t, a, general, true)
	a2, _ := gatingAdapter(t, map[string]any{"require_mention": false, "allowed_chats": []any{"-100"}, "allowed_topics": []any{"8"}}, "")
	expectGate(t, a2, general, false)
}

func TestBotSelfMessagesAreIgnoredInDmAndGroup(t *testing.T) {
	a, _ := gatingAdapter(t, map[string]any{"require_mention": false}, "")
	expectGate(t, a, groupMessage("hi", groupMsgOpts{chatID: -100}), true)
	selfDM := groupMessage("[SYSTEM: Background process matched watch pattern ...]", groupMsgOpts{chatID: 555, fromID: 999})
	selfDM.Chat.Type = "private"
	expectGate(t, a, selfDM, false)
	expectGate(t, a, groupMessage("status tick", groupMsgOpts{chatID: -100, fromID: 999}), false)
}

func TestTriggeredLocationMessageUsesSharedSessionInObserveMode(t *testing.T) {
	a, _ := gatingAdapter(t, map[string]any{"require_mention": false, "group_allowed_chats": []any{"-100"}, "observe_unmentioned_group_messages": true}, "")
	sink := newEventSink(a)
	m := &models.Message{ID: 50, Chat: models.Chat{ID: -100, Type: "group", Title: "Test Group"},
		From: &models.User{ID: 111, FirstName: "Alice", LastName: "Example"}, Location: &models.Location{Latitude: 37.7749, Longitude: -122.4194}}
	if err := a.handleLocationMessage(context.Background(), updateOf(2002, m)); err != nil {
		t.Fatal(err)
	}
	event := sink.first(t)
	if sink.count() != 1 || event.Source.UserID != nil || !strings.Contains(event.Text, "[Alice Example|111]") {
		t.Fatalf("event %+v %q", event.Source, event.Text)
	}
}

func TestStaleUsernameSchedulesBackgroundIdentityRecheck(t *testing.T) {
	a, fake := gatingAdapter(t, map[string]any{"require_mention": true, "exclusive_bot_mentions": true}, "old_helper_bot")
	fake.respond = func(method string, _ map[string]string) (int, string) {
		if method == "getMe" {
			return 200, `{"ok":true,"result":{"id":999,"is_bot":true,"first_name":"Hermes","username":"new_helper_bot"}}`
		}
		return 0, ""
	}
	text := "@new_helper_bot what's the weather"
	message := groupMessage(text, groupMsgOpts{entities: mentionEnts(text, "@new_helper_bot")})
	expectGate(t, a, message, false)
	a.mu.Lock()
	task := a.identityRefreshTask
	a.mu.Unlock()
	if task == nil {
		t.Fatal("no identity recheck scheduled")
	}
	if err := task.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.callsOf("getMe")); n != 1 {
		t.Fatalf("getMe calls %d", n)
	}
	if got := a.currentBotUsername(); got != "new_helper_bot" {
		t.Fatalf("username %q", got)
	}
	expectGate(t, a, message, true)
}

func TestBotNeverAdoptsAnotherAccountsUsername(t *testing.T) {
	a, _ := gatingAdapter(t, map[string]any{"require_mention": true}, "hermes_bot")
	m := groupMessage("hello", groupMsgOpts{})
	m.From = &models.User{ID: 555, Username: "impostor_bot", FirstName: "Impostor"}
	a.observeBotIdentityFromMessage(m)
	if got := a.currentBotUsername(); got != "hermes_bot" {
		t.Fatalf("username %q", got)
	}
}

func TestCollectibleUsernameNotSuppressedByOtherBotMention(t *testing.T) {
	a, _ := gatingAdapter(t, map[string]any{"require_mention": true, "exclusive_bot_mentions": true}, "jarvis")
	text := "@jarvis ask @other_helper_bot for the log"
	m := groupMessage(text, groupMsgOpts{entities: mentionEnts(text, "@jarvis", "@other_helper_bot")})
	if a.explicitBotMentionsExcludeSelf(context.Background(), m) {
		t.Fatal("own collectible handle counted as another bot")
	}
	expectGate(t, a, m, true)
}

func TestIdentityFreshnessDoesNotDependOnHostUptime(t *testing.T) {
	a, _ := gatingAdapter(t, map[string]any{"require_mention": true}, "old_helper_bot")
	saved := monotonicNow
	monotonicNow = func() float64 { return 12.0 }
	t.Cleanup(func() { monotonicNow = saved })
	if a.botIdentityIsFresh() {
		t.Fatal("never checked identity reads fresh")
	}
	a.noteBotUsername("new_helper_bot")
	if !a.botIdentityIsFresh() {
		t.Fatal("identity stale right after a check")
	}
}

func TestBotQuoteReplyLoopIsBrokenByBotsRequireMention(t *testing.T) {
	loop, _ := gatingAdapter(t, map[string]any{"require_mention": true}, "")
	expectGate(t, loop, botSenderMessage("auto-reply", true, nil), true)
	gated, _ := gatingAdapter(t, map[string]any{"require_mention": true, "bots_require_mention": true}, "")
	expectGate(t, gated, botSenderMessage("auto-reply", true, nil), false)
	text := "@hermes_bot ping"
	expectGate(t, gated, botSenderMessage(text, false, mentionEnts(text, "@hermes_bot")), true)
}

func TestHumanReplyUnaffectedByBotsRequireMention(t *testing.T) {
	gated, _ := gatingAdapter(t, map[string]any{"require_mention": true, "bots_require_mention": true}, "")
	expectGate(t, gated, groupMessage("replying", groupMsgOpts{replyToBot: true}), true)
}

// Extra of the sibling bot cases, wake word plus observe mode in an allowed group
func siblingExtra() map[string]any {
	return map[string]any{"require_mention": true, "bots_require_mention": true, "mention_patterns": []any{"hermes"},
		"allowed_chats": []any{"-100"}, "group_allowed_chats": []any{"-100"}, "observe_unmentioned_group_messages": true}
}

func TestSiblingBotWakeWordMessageIsObservedNotDropped(t *testing.T) {
	a, _ := gatingAdapter(t, siblingExtra(), "")
	sink := newEventSink(a)
	store := &observeRecorder{}
	a.SetSessionStore(store)
	msg := botSenderMessage("hermes, can you take this one?", false, nil)
	expectGate(t, a, msg, false)
	if err := a.handleTextMessage(context.Background(), updateOf(2001, msg)); err != nil {
		t.Fatal(err)
	}
	if sink.count() != 0 || len(store.messages) != 1 {
		t.Fatalf("dispatched %d observed %d", sink.count(), len(store.messages))
	}
	line := store.messages[0].message
	content, _ := line["content"].(string)
	if line["observed"] != true || !strings.HasSuffix(content, "hermes, can you take this one?") {
		t.Fatalf("line %v", line)
	}
}

func TestSiblingBotExplicitMentionStillDispatchesAndIsNotObserved(t *testing.T) {
	a, _ := gatingAdapter(t, siblingExtra(), "")
	text := "@hermes_bot ping"
	msg := botSenderMessage(text, false, mentionEnts(text, "@hermes_bot"))
	expectGate(t, a, msg, true)
	expectObserve(t, a, msg, false)
	quoted := botSenderMessage("auto-reply", true, nil)
	expectGate(t, a, quoted, false)
	expectObserve(t, a, quoted, true)
	human := groupMessage("hermes, hello", groupMsgOpts{})
	expectGate(t, a, human, true)
	expectObserve(t, a, human, false)
}

// Message of the hermes _replying_to helper
func replyingTo(m *models.Message, userID int64, messageID int, senderChat int64) *models.Message {
	reply := &models.Message{ID: messageID, Text: "earlier"}
	if userID != 0 {
		reply.From = &models.User{ID: userID}
	}
	if senderChat != 0 {
		reply.SenderChat = &models.Chat{ID: senderChat, Type: "channel"}
	}
	m.ReplyToMessage = reply
	return m
}

// Message of the hermes _with_media helper
func withMedia(m *models.Message, kind string) *models.Message {
	switch kind {
	case "voice":
		m.Voice = &models.Voice{FileID: "f"}
	case "audio":
		m.Audio = &models.Audio{FileID: "f"}
	case "video_note":
		m.VideoNote = &models.VideoNote{FileID: "f"}
	case "photo":
		m.Photo = []models.PhotoSize{{FileID: "f"}}
	}
	return m
}

func TestReplyExpectedFollowsWhoTheGroupMessageAddresses(t *testing.T) {
	yes, no := true, false
	ent := func(text, mention string) []models.MessageEntity { return mentionEnts(text, mention) }
	cases := []struct {
		id       string
		message  *models.Message
		expected *bool
	}{
		{"mention", groupMessage("@hermes_bot when is rent due?", groupMsgOpts{entities: ent("@hermes_bot when is rent due?", "@hermes_bot")}), &yes},
		{"reply-to-bot", groupMessage("sure, saturday", groupMsgOpts{replyToBot: true}), &yes},
		{"wake-word", groupMessage("hermes, add milk to the list", groupMsgOpts{}), &yes},
		{"caption-command", groupMessage("", groupMsgOpts{caption: "/status"}), &yes},
		{"indented-command", groupMessage(" /status", groupMsgOpts{}), &yes},
		{"dm", dmMessage("hello"), &yes},
		{"reply-to-other-member", replyingTo(groupMessage("yes, saturday", groupMsgOpts{}), 222, 10, 0), &no},
		{"opens-with-other-mention", groupMessage("@alice are we buying the fridge?", groupMsgOpts{entities: ent("@alice are we buying the fridge?", "@alice")}), &no},
		{"chatter", groupMessage("the fridge arrives on saturday", groupMsgOpts{}), &no},
		{"question", groupMessage("are we buying the fridge on saturday?", groupMsgOpts{}), nil},
		{"mentions-other-mid-question", groupMessage("what did @alice say?", groupMsgOpts{entities: ent("what did @alice say?", "@alice")}), nil},
		{"forum-topic-root", replyingTo(groupMessage("anyone up for dinner?", groupMsgOpts{threadID: 5}), 222, 5, 0), nil},
		{"reply-to-unknown-author", replyingTo(groupMessage("agreed?", groupMsgOpts{}), 0, 10, 0), nil},
		{"reply-to-unknown-author-statement", replyingTo(groupMessage("agreed", groupMsgOpts{}), 0, 10, 0), nil},
		{"reply-to-channel-autoforward", replyingTo(groupMessage("when does it start?", groupMsgOpts{}), 777000, 10, -1009), nil},
		{"reply-to-anonymous-admin", replyingTo(groupMessage("agreed", groupMsgOpts{}), 1087968824, 10, -100), nil},
		{"voice-note", withMedia(groupMessage("", groupMsgOpts{}), "voice"), nil},
		{"audio-file", withMedia(groupMessage("", groupMsgOpts{}), "audio"), nil},
		{"video-note", withMedia(groupMessage("", groupMsgOpts{}), "video_note"), nil},
		{"captioned-voice-chatter", withMedia(groupMessage("", groupMsgOpts{caption: "the fridge arrives on saturday"}), "voice"), &no},
		{"photo-without-caption", withMedia(groupMessage("", groupMsgOpts{}), "photo"), &no},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			a, _ := gatingAdapter(t, map[string]any{"require_mention": false, "mention_patterns": []any{"hermes"}}, "")
			got := a.buildMessageEvent(context.Background(), tc.message, gateway.MessageTypeText, nil).ReplyExpected
			if (got == nil) != (tc.expected == nil) || (got != nil && *got != *tc.expected) {
				t.Fatalf("reply expected %v, want %v", got, tc.expected)
			}
		})
	}
}
