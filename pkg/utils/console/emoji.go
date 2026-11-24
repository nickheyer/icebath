package consoleutil

import (
	"strings"
	"sync"
)

// Shortest GitHub name of every emoji, parsed from the table on first use
var emojiNames = sync.OnceValue(func() map[string]string {
	names := make(map[string]string, strings.Count(emojiTable, "\n"))
	for line := range strings.Lines(emojiTable) {
		if emoji, name, ok := strings.Cut(strings.TrimSuffix(line, "\n"), " "); ok {
			names[emoji] = name
		}
	}
	return names
})

// Emoji without variation selectors or, when stripTones is set, skin tone modifiers
func emojiKey(emoji string, stripTones bool) string {
	return strings.Map(func(r rune) rune {
		if r == '️' || r == '︎' || stripTones && r >= 0x1f3fb && r <= 0x1f3ff {
			return -1
		}
		return r
	}, strings.TrimSpace(emoji))
}

// Shortest GitHub style name of an emoji, the name chat platforms take for reactions, a skin toned emoji falling back to the name of its plain form
func EmojiShortName(emoji string) (string, bool) {
	names := emojiNames()
	if name, ok := names[emojiKey(emoji, false)]; ok {
		return name, true
	}
	name, ok := names[emojiKey(emoji, true)]
	return name, ok
}
