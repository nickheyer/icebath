package telegram

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Telegram message entity as the bot API delivers it, fields untyped so malformed values are skipped like Python
type MessageEntity struct {
	Type   any
	Offset any
	Length any
	URL    any
	User   *TopicUser
}

// Text and caption of a Telegram message with their entities
type EntityMessage struct {
	Text            string
	Entities        []MessageEntity
	Caption         string
	CaptionEntities []MessageEntity
}

// UTF-16 code unit count of the text
func utf16Length(text string) int {
	n := 0
	for _, r := range text {
		n += utf16Units(r)
	}
	return n
}

// Code units of one rune in UTF-16
func utf16Units(r rune) int {
	if r >= 0x10000 && r <= utf8.MaxRune {
		return 2
	}
	return 1
}

// Code point index of a UTF-16 offset, false when it splits a surrogate pair
func codePointIndex(text string, offset int) (int, bool) {
	units, idx := 0, 0
	for _, r := range text {
		if units == offset {
			return idx, true
		}
		units += utf16Units(r)
		if units > offset {
			return 0, false
		}
		idx++
	}
	return idx, true
}

// Integer of an entity field, false where Python int raises
func entityInt(v any) (int, bool) {
	n, err := pyint.Of(v)
	if err != nil || !n.IsInt64() {
		return 0, false
	}
	return int(n.Int64()), true
}

type textLink struct {
	start, end int
	url        string
}

// Message text or caption with every hidden text link URL inlined after its anchor
func expandLinkEntities(message EntityMessage) string {
	text, entities := message.Text, message.Entities
	if text == "" {
		text, entities = message.Caption, message.CaptionEntities
	}
	if text == "" || len(entities) == 0 {
		return text
	}
	total := utf16Length(text)
	runes := []rune(text)
	var links []textLink
	for _, entity := range entities {
		typeName := ""
		if entity.Type != nil {
			typeName = pyrepr.Str(entity.Type)
		}
		parts := strings.Split(typeName, ".")
		rawURL, _ := entity.URL.(string)
		url := pystr.Strip(rawURL)
		if pystr.Lower(parts[len(parts)-1]) != "text_link" || url == "" {
			continue
		}
		offsetValue, lengthValue := entity.Offset, entity.Length
		if offsetValue == nil {
			offsetValue = -1
		}
		if lengthValue == nil {
			lengthValue = 0
		}
		offset, ok1 := entityInt(offsetValue)
		length, ok2 := entityInt(lengthValue)
		if !ok1 || !ok2 || offset < 0 || length <= 0 || offset+length > total {
			continue
		}
		start, okStart := codePointIndex(text, offset)
		end, okEnd := codePointIndex(text, offset+length)
		if !okStart || !okEnd || end <= start {
			continue
		}
		if pystr.Strip(string(runes[start:end])) == url {
			continue
		}
		links = append(links, textLink{start, end, url})
	}
	sort.Slice(links, func(i, j int) bool {
		a, b := links[i], links[j]
		if a.start != b.start {
			return a.start > b.start
		}
		if a.end != b.end {
			return a.end > b.end
		}
		return a.url > b.url
	})
	expanded := runes
	for _, l := range links {
		inline := []rune(" (" + l.url + ")")
		if strings.HasPrefix(string(expanded[l.end:]), string(inline)) {
			continue
		}
		expanded = append(append(append([]rune{}, expanded[:l.end]...), inline...), expanded[l.end:]...)
	}
	return string(expanded)
}
