package gatewayrun

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Window between two BotFather Threads Settings screenshots for one profile and chat
const telegramCapabilityHintCooldownS = 300.0

// File name of the bundled BotFather Threads Settings screenshot
const telegramTopicSetupImageName = "telegram-botfather-threads-settings.jpg"

//go:embed assets/telegram-botfather-threads-settings.jpg
var telegramTopicSetupImage []byte

// Telegram bot client reading its own bot user
type telegramMeReader interface {
	GetMe(ctx context.Context) (any, error)
}

// Telegram bot client pinning a chat message
type telegramMessagePinner interface {
	PinChatMessage(ctx context.Context, chatID, messageID int64, disableNotification bool) error
}

// Bot API object exposing its attributes by name
type botAttrReader interface {
	Attr(name string) (any, bool)
}

// Python str of an optional identity field, None when unset
func strOrNone(p *string) string {
	if p == nil {
		return "None"
	}
	return *p
}

// The two restore instruction lines of the topic status replies
func topicRestoreSteps(ctx context.Context) []string {
	return []string{i18n.T(ctx, "gateway.topic.restore_step_1", "", nil), i18n.T(ctx, "gateway.topic.restore_step_2", "", nil)}
}

// Help text of the topic command
func telegramTopicHelpText(ctx context.Context) string {
	return i18n.T(ctx, "gateway.topic.help", "", nil)
}

// Rate limits the BotFather Threads Settings screenshot to one per cooldown window
func (r *GatewayRunner) shouldSendTelegramCapabilityHint(source *gateway.SessionSource) bool {
	return telegramCooldownElapsed(&r.telegramCapabilityHintTS, source, telegramCapabilityHintCooldownS)
}

// Bot client of the source's delivery adapter, nil when the adapter exposes none
func (r *GatewayRunner) telegramBotFor(ctx context.Context, source *gateway.SessionSource) (gateway.PlatformAdapter, any) {
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil || adapter == nil {
		return adapter, nil
	}
	if holder, ok := adapter.(telegramBotAdapter); ok {
		return adapter, holder.TelegramBot()
	}
	return adapter, nil
}

// Attribute of a getMe result, then its api kwargs entry, then its dict key
func botMeField(me any, name string) any {
	if reader, ok := me.(botAttrReader); ok {
		if v, has := reader.Attr(name); has {
			return v
		}
		if raw, has := reader.Attr("api_kwargs"); has {
			if kwargs, isDict := raw.(map[string]any); isDict {
				if v, present := kwargs[name]; present {
					return v
				}
			}
		}
	}
	if dict, ok := me.(map[string]any); ok {
		return dict[name]
	}
	return nil
}

// Telegram private topic capability flags read through the Bot API getMe, unchecked when unavailable
func (r *GatewayRunner) getTelegramTopicCapabilities(ctx context.Context, source *gateway.SessionSource) map[string]any {
	_, bot := r.telegramBotFor(ctx, source)
	reader, ok := bot.(telegramMeReader)
	if bot == nil || !ok {
		return map[string]any{"checked": false}
	}
	me, err := reader.GetMe(ctx)
	if err != nil {
		runLog().Debug("Failed to fetch Telegram getMe topic capabilities", "exc_info", err)
		return map[string]any{"checked": false}
	}
	return map[string]any{"checked": true, "has_topics_enabled": botMeField(me, "has_topics_enabled"),
		"allows_users_to_create_topics": botMeField(me, "allows_users_to_create_topics")}
}

// Creates the managed System topic, posts its intro and pins it when the adapter allows each step
func (r *GatewayRunner) ensureTelegramSystemTopic(ctx context.Context, source *gateway.SessionSource) {
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil || adapter == nil || source.ChatID == "" {
		return
	}
	creator, ok := adapter.(dmTopicCreator)
	if !ok {
		return
	}
	threadID, err := func() (*int64, error) {
		chatID, ok := pystr.IntText(source.ChatID)
		if !ok || !chatID.IsInt64() {
			return nil, fmt.Errorf("ValueError: invalid literal for int() with base 10: %s", pyrepr.StrRepr(source.ChatID))
		}
		return creator.CreateDMTopic(ctx, chatID.Int64(), i18n.T(ctx, "gateway.topic.system_topic_name", "", nil), nil, nil)
	}()
	if err != nil {
		runLog().Debug("Failed to create Telegram System topic", "exc_info", err)
		return
	}
	if threadID == nil || *threadID == 0 {
		return
	}
	sent, err := adapter.Send(ctx, source.ChatID, i18n.T(ctx, "gateway.topic.system_topic_intro", "", nil), "",
		map[string]any{"thread_id": fmt.Sprint(*threadID)})
	if err != nil {
		runLog().Debug("Failed to send Telegram System topic intro", "exc_info", err)
		return
	}
	pinner, ok := func() (telegramMessagePinner, bool) {
		holder, has := adapter.(telegramBotAdapter)
		if !has {
			return nil, false
		}
		p, can := holder.TelegramBot().(telegramMessagePinner)
		return p, can
	}()
	if !pyrepr.Truthy(sent.MessageID) || !ok {
		return
	}
	err = func() error {
		chatID, err := pyint.Of(source.ChatID)
		if err != nil {
			return err
		}
		messageID, err := pyint.Of(sent.MessageID)
		if err != nil {
			return err
		}
		return pinner.PinChatMessage(ctx, chatID.Int64(), messageID.Int64(), true)
	}()
	if err != nil {
		runLog().Debug("Failed to pin Telegram System topic intro", "exc_info", err)
	}
}

// Bundled BotFather screenshot written once under the profile cache, empty when it cannot be written
func telegramTopicSetupImagePath(ctx context.Context) string {
	path := filepath.Join(home.CacheDir(home.Dir(ctx)), "gateway_assets", telegramTopicSetupImageName)
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, telegramTopicSetupImage) {
		return path
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return ""
	}
	if err := os.WriteFile(path, telegramTopicSetupImage, 0o644); err != nil {
		return ""
	}
	return path
}

// Sends the bundled BotFather Threads Settings screenshot into the source chat
func (r *GatewayRunner) sendTelegramTopicSetupImage(ctx context.Context, source *gateway.SessionSource) {
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil || adapter == nil || source.ChatID == "" {
		return
	}
	imagePath := telegramTopicSetupImagePath(ctx)
	if imagePath == "" {
		return
	}
	var metadata map[string]any
	if sattr(source.ThreadID) != "" {
		metadata = map[string]any{"thread_id": *source.ThreadID}
	}
	if _, err := adapter.SendImageFile(ctx, source.ChatID, imagePath, i18n.T(ctx, "gateway.topic.setup_image_caption", "", nil), "", metadata, nil); err != nil {
		runLog().Debug("Failed to send Telegram topic setup image", "exc_info", err)
	}
}

// Disables topic mode for the chat and clears its per profile cooldown stamps
func (r *GatewayRunner) disableTelegramTopicModeForChat(ctx context.Context, source *gateway.SessionSource) string {
	asyncDB := r.sessionDB(ctx)
	if asyncDB == nil {
		return sessionDBUnavailableReply(ctx)
	}
	db := asyncDB.DB()
	chatID := source.ChatID
	if chatID == "" {
		return i18n.T(ctx, "gateway.topic.no_chat_id", "", nil)
	}
	profileName := telegramTopicProfileName(source)
	enabled, err := state.AsyncCall(ctx, asyncDB, func(d *state.SessionDB) (bool, error) {
		return d.IsTelegramTopicModeEnabled(ctx, chatID, sattr(source.UserID), profileName)
	})
	if err != nil || !enabled {
		return i18n.T(ctx, "gateway.topic.mode_not_enabled", "", nil)
	}
	if _, err := state.AsyncCall(ctx, asyncDB, func(d *state.SessionDB) (struct{}, error) {
		return struct{}{}, db.DisableTelegramTopicMode(ctx, chatID, profileName, true)
	}); err != nil {
		runLog().Error("Failed to disable Telegram topic mode", "exc_info", err)
		return i18n.T(ctx, "gateway.topic.disable_failed", "", map[string]any{"error": pyrepr.ExceptionMessage(err)})
	}
	if key := telegramTopicCooldownKey(source); key != "" {
		delete(r.telegramLobbyReminderTS, key)
		delete(r.telegramCapabilityHintTS, key)
	}
	return i18n.T(ctx, "gateway.topic.mode_disabled", "", nil)
}

// Topic mode status of the root DM with the user's sessions not yet bound to a topic
func (r *GatewayRunner) telegramTopicRootStatusMessage(ctx context.Context, source *gateway.SessionSource) string {
	lines := []string{i18n.T(ctx, "gateway.topic.status_enabled", "", nil), "", i18n.T(ctx, "gateway.topic.status_how_to", "", nil), ""}
	var sessions []*pyjson.Object
	err := errors.New("'NoneType' object has no attribute 'list_unlinked_telegram_sessions_for_user'")
	if asyncDB := r.sessionDB(ctx); asyncDB != nil {
		sessions, err = state.AsyncCall(ctx, asyncDB, func(d *state.SessionDB) ([]*pyjson.Object, error) {
			return d.ListUnlinkedTelegramSessionsForUser(ctx, strOrNone(source.UserID), telegramTopicProfileName(source), 10)
		})
	}
	if err != nil {
		runLog().Debug("Failed to list unlinked Telegram sessions", "exc_info", err)
		sessions = nil
	}
	if len(sessions) == 0 {
		lines = append(lines, i18n.T(ctx, "gateway.topic.no_unlinked", "", nil), "", i18n.T(ctx, "gateway.topic.restore_later", "", nil))
		lines = append(lines, topicRestoreSteps(ctx)...)
		return strings.Join(lines, "\n")
	}
	lines = append(lines, i18n.T(ctx, "gateway.topic.previous_unlinked", "", nil))
	for _, session := range sessions {
		preview := pystr.Strip(rowStr(session, "preview"))
		title := rowStr(session, "title")
		if title == "" {
			title = i18n.T(ctx, "gateway.topic.untitled_session", "", nil)
		}
		line := "- " + title + " — `" + rowStr(session, "id") + "`"
		if preview != "" {
			line += " — " + preview
		}
		lines = append(lines, line)
	}
	firstID, _ := sessions[0].Get("id")
	lines = append(lines, "", i18n.T(ctx, "gateway.topic.restore_one", "", nil))
	lines = append(lines, topicRestoreSteps(ctx)...)
	lines = append(lines, i18n.T(ctx, "gateway.topic.restore_example", "", map[string]any{"session_id": firstID}))
	return strings.Join(lines, "\n")
}

// Restores an existing Telegram owned session into this topic, echoing its last assistant message
func (r *GatewayRunner) restoreTelegramTopicSession(ctx context.Context, event *gateway.MessageEvent, rawSessionID string) (string, error) {
	source := event.Source
	asyncDB := r.sessionDB(ctx)
	if asyncDB == nil {
		return "", errors.New("AttributeError: 'NoneType' object has no attribute 'resolve_session_id'")
	}
	db := asyncDB.DB()
	wanted := pystr.Strip(rawSessionID)
	sessionID, err := db.ResolveSessionID(wanted)
	if err != nil {
		return "", err
	}
	var session *pyjson.Object
	if sessionID != "" {
		if session, err = db.GetSession(ctx, sessionID); err != nil {
			return "", err
		}
	}
	if session == nil || session.Len() == 0 {
		return i18n.T(ctx, "gateway.topic.session_not_found", "", map[string]any{"session_id": wanted}), nil
	}
	if rowStr(session, "source") != "telegram" {
		return i18n.T(ctx, "gateway.topic.not_telegram_session", "", nil), nil
	}
	if rowStr(session, "user_id") != strOrNone(source.UserID) {
		return i18n.T(ctx, "gateway.topic.not_your_session", "", nil), nil
	}
	linked, err := db.IsTelegramSessionLinkedToTopic(ctx, sessionID)
	if err != nil {
		return "", err
	}
	topicProfile := telegramTopicProfileName(source)
	currentBinding, err := db.GetTelegramTopicBinding(ctx, source.ChatID, strOrNone(source.ThreadID), topicProfile)
	if err != nil {
		return "", err
	}
	alreadyLinked := i18n.T(ctx, "gateway.topic.already_linked", "", nil)
	if linked {
		var bound any
		if currentBinding != nil {
			bound, _ = currentBinding.Get("session_id")
		}
		if currentBinding == nil || currentBinding.Len() == 0 || bound != sessionID {
			return alreadyLinked, nil
		}
	}
	sessionKey, err := r.sessionKeyForSource(ctx, source)
	if err != nil {
		return "", err
	}
	if err := db.BindTelegramTopic(ctx, state.TelegramTopicBinding{ChatID: source.ChatID, ThreadID: strOrNone(source.ThreadID),
		UserID: strOrNone(source.UserID), SessionKey: sessionKey, SessionID: sessionID, ManagedMode: "restored", ProfileName: topicProfile}); err != nil {
		if isValueError(err) && strings.Contains(pyrepr.ExceptionMessage(err), "already linked") {
			return alreadyLinked, nil
		}
		return "", err
	}
	title, _, err := db.GetSessionTitle(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if title == "" {
		title = sessionID
	}
	lastAssistant := ""
	if messages, err := db.GetMessages(ctx, sessionID, state.GetMessagesOptions{}); err == nil {
		for i := len(messages) - 1; i >= 0; i-- {
			if role, _ := messages[i].Get("role"); role != "assistant" {
				continue
			}
			projected := state.ProjectCompactionMessageForDisplay(messages[i])
			if projected != nil && pyrepr.Truthy(projected["content"]) {
				lastAssistant = pyrepr.Str(projected["content"])
				break
			}
		}
	}
	response := i18n.T(ctx, "gateway.topic.restored", "", map[string]any{"title": title})
	if lastAssistant != "" {
		response += i18n.T(ctx, "gateway.topic.restored_last_message", "", map[string]any{"text": lastAssistant})
	}
	return response, nil
}
