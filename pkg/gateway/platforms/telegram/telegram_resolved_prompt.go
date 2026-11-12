package telegram

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/go-telegram/bot/models"

	"github.com/nickheyer/icebath/pkg/internal/pyhtml"
)

// Appends the decision to the prompt and drops its buttons, the decision only edit running when the body is unusable or too long
func editKeepingPrompt(ctx context.Context, a *TelegramAdapter, q callbackQuery, decision string) error {
	if m := q.message(); m != nil && m.Text != "" {
		body := strings.ReplaceAll(messageTextHTML(m), a.EADeadlineLine(ctx), "")
		err := q.editMessageText(ctx, body+"\n\n— "+pyhtml.Escape(decision, true), models.ParseModeHTML, nil)
		if err == nil {
			return nil
		}
		switch {
		case isPTBError(err, "BadRequest"):
		case isPTBError(err, "NetworkError"):
			slog.Debug("Telegram resolved-prompt edit failed: " + redactTelegramErrorText(err))
			return nil
		default:
			return err
		}
	}
	a.editMDQuiet(ctx, q, decision)
	return nil
}

// HTML rendering of a message text rebuilt from its entities, the PTB Message.text_html
func messageTextHTML(m *models.Message) string {
	units := utf16.Encode([]rune(m.Text))
	return parseEntitiesHTML(units, m.Entities, 0)
}

// Text of a UTF-16 unit span decoded back to a string
func utf16Span(units []uint16, from, to int) string {
	from, to = max(0, min(from, len(units))), max(0, min(to, len(units)))
	if from >= to {
		return ""
	}
	return string(utf16.Decode(units[from:to]))
}

// Nested entity HTML of a UTF-16 text whose entities start at the given offset, the PTB _parse_html with urled false
func parseEntitiesHTML(units []uint16, entities []models.MessageEntity, offset int) string {
	sorted := append([]models.MessageEntity(nil), entities...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Offset < sorted[j].Offset })
	var out strings.Builder
	lastOffset := 0
	parsed := map[int]bool{}
	for i, entity := range sorted {
		if parsed[i] {
			continue
		}
		var nested []models.MessageEntity
		for j, e := range sorted {
			if j != i && !sameEntity(e, entity) && e.Offset >= entity.Offset && e.Offset+e.Length <= entity.Offset+entity.Length {
				nested = append(nested, e)
				parsed[j] = true
			}
		}
		text := utf16Span(units, entity.Offset-offset, entity.Offset-offset+entity.Length)
		escaped := pyhtml.Escape(text, true)
		if len(nested) > 0 {
			inner := utf16.Encode([]rune(text))
			escaped = parseEntitiesHTML(inner, nested, entity.Offset)
		}
		var insert string
		switch entity.Type {
		case models.MessageEntityTypeTextLink:
			insert = `<a href="` + entity.URL + `">` + escaped + `</a>`
		case models.MessageEntityTypeTextMention:
			if entity.User != nil {
				insert = `<a href="tg://user?id=` + strconv.FormatInt(entity.User.ID, 10) + `">` + escaped + `</a>`
			} else {
				insert = escaped
			}
		case models.MessageEntityTypeBlockquote:
			insert = "<blockquote>" + escaped + "</blockquote>"
		case models.MessageEntityTypeExpandableBlockquote:
			insert = "<blockquote expandable>" + escaped + "</blockquote>"
		case models.MessageEntityTypeBold:
			insert = "<b>" + escaped + "</b>"
		case models.MessageEntityTypeItalic:
			insert = "<i>" + escaped + "</i>"
		case models.MessageEntityTypeCode:
			insert = "<code>" + escaped + "</code>"
		case models.MessageEntityTypePre:
			if entity.Language != "" {
				insert = `<pre><code class="language-` + entity.Language + `">` + escaped + `</code></pre>`
			} else {
				insert = "<pre>" + escaped + "</pre>"
			}
		case models.MessageEntityTypeUnderline:
			insert = "<u>" + escaped + "</u>"
		case models.MessageEntityTypeStrikethrough:
			insert = "<s>" + escaped + "</s>"
		case models.MessageEntityTypeSpoiler:
			insert = `<span class="tg-spoiler">` + escaped + `</span>`
		case models.MessageEntityTypeCustomEmoji:
			insert = `<tg-emoji emoji-id="` + entity.CustomEmojiID + `">` + escaped + `</tg-emoji>`
		default:
			insert = escaped
		}
		out.WriteString(pyhtml.Escape(utf16Span(units, lastOffset, entity.Offset-offset), true))
		out.WriteString(insert)
		lastOffset = entity.Offset - offset + entity.Length
	}
	out.WriteString(pyhtml.Escape(utf16Span(units, lastOffset, len(units)), true))
	return out.String()
}

// Whether two entities are equal the way PTB compares them, by type, offset and length
func sameEntity(a, b models.MessageEntity) bool {
	return a.Type == b.Type && a.Offset == b.Offset && a.Length == b.Length
}
