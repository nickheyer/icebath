package telegram

import (
	"context"

	"github.com/go-telegram/bot/models"
)

// Effective message of an update in the PTB priority order, nil for updates without one
func effectiveMessage(u *models.Update) *models.Message {
	if u == nil {
		return nil
	}
	switch {
	case u.Message != nil:
		return u.Message
	case u.EditedMessage != nil:
		return u.EditedMessage
	case u.CallbackQuery != nil && u.CallbackQuery.Message.Message != nil:
		return u.CallbackQuery.Message.Message
	case u.ChannelPost != nil:
		return u.ChannelPost
	case u.EditedChannelPost != nil:
		return u.EditedChannelPost
	case u.BusinessMessage != nil:
		return u.BusinessMessage
	case u.EditedBusinessMessage != nil:
		return u.EditedBusinessMessage
	}
	return nil
}

// Whether the update is one of the message kinds PTB message filters inspect
func isMessageUpdate(u *models.Update) bool {
	return u != nil && (u.ChannelPost != nil || u.Message != nil || u.EditedChannelPost != nil || u.EditedMessage != nil ||
		u.BusinessMessage != nil || u.EditedBusinessMessage != nil)
}

// Raw SDK update of a dispatched PTB update, nil for anything else
func rawUpdate(update any) *models.Update {
	u, ok := update.(*Update)
	if !ok || u == nil {
		return nil
	}
	return u.Raw
}

// Message handler gated by a filter on the effective message, the PTB MessageHandler
type messageHandler struct {
	filter    func(m *models.Message) bool
	callback  *Callback
	blockMode BlockMode
}

func (h *messageHandler) CheckUpdate(update any) any {
	raw := rawUpdate(update)
	if !isMessageUpdate(raw) {
		return false
	}
	m := effectiveMessage(raw)
	return m != nil && h.filter(m)
}

func (h *messageHandler) HandleUpdate(ctx context.Context, update any, _ *Application, _ any, cbctx *CallbackContext) error {
	return h.callback.Fn(ctx, update, cbctx)
}

func (h *messageHandler) Block() BlockMode                { return h.blockMode }
func (h *messageHandler) HandlerCallback() *Callback      { return h.callback }
func (h *messageHandler) SetHandlerCallback(cb *Callback) { h.callback = cb }

// Handler of every callback query update, the PTB CallbackQueryHandler without a pattern
type callbackQueryHandler struct {
	callback  *Callback
	blockMode BlockMode
}

func (h *callbackQueryHandler) CheckUpdate(update any) any {
	raw := rawUpdate(update)
	return raw != nil && raw.CallbackQuery != nil
}

func (h *callbackQueryHandler) HandleUpdate(ctx context.Context, update any, _ *Application, _ any, cbctx *CallbackContext) error {
	return h.callback.Fn(ctx, update, cbctx)
}

func (h *callbackQueryHandler) Block() BlockMode                { return h.blockMode }
func (h *callbackQueryHandler) HandlerCallback() *Callback      { return h.callback }
func (h *callbackQueryHandler) SetHandlerCallback(cb *Callback) { h.callback = cb }

// Handler of every inline query update, the PTB InlineQueryHandler without a pattern
type inlineQueryHandler struct {
	callback  *Callback
	blockMode BlockMode
}

func (h *inlineQueryHandler) CheckUpdate(update any) any {
	raw := rawUpdate(update)
	return raw != nil && raw.InlineQuery != nil
}

func (h *inlineQueryHandler) HandleUpdate(ctx context.Context, update any, _ *Application, _ any, cbctx *CallbackContext) error {
	return h.callback.Fn(ctx, update, cbctx)
}

func (h *inlineQueryHandler) Block() BlockMode                { return h.blockMode }
func (h *inlineQueryHandler) HandlerCallback() *Callback      { return h.callback }
func (h *inlineQueryHandler) SetHandlerCallback(cb *Callback) { h.callback = cb }

// PTB filters.COMMAND, a bot_command entity opening the text
func filterCommand(m *models.Message) bool {
	return len(m.Entities) > 0 && m.Entities[0].Type == models.MessageEntityTypeBotCommand && m.Entities[0].Offset == 0
}

// PTB filters.TEXT and not filters.COMMAND
func filterTextNotCommand(m *models.Message) bool { return m.Text != "" && !filterCommand(m) }

// PTB filters.LOCATION or filters.VENUE
func filterLocation(m *models.Message) bool { return m.Location != nil || m.Venue != nil }

// PTB photo, video, video note, audio, voice, document and sticker filters combined
func filterMedia(m *models.Message) bool {
	return len(m.Photo) > 0 || m.Video != nil || m.VideoNote != nil || m.Audio != nil || m.Voice != nil || m.Document != nil ||
		m.Sticker != nil
}

// PTB filters.StatusUpdate.FORUM_TOPIC_CREATED
func filterForumTopicCreated(m *models.Message) bool { return m.ForumTopicCreated != nil }

// Callback bound to the adapter so admission leaves acceptance to the handler itself
func (a *TelegramAdapter) boundCallback(fn func(ctx context.Context, update *Update) error) *Callback {
	return &Callback{Owner: a, Fn: func(ctx context.Context, update any, _ *CallbackContext) error {
		u, _ := update.(*Update)
		if u == nil {
			return nil
		}
		return fn(ctx, u)
	}}
}
