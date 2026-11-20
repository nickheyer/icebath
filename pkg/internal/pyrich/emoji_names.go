package pyrich

import (
	"strings"
	"sync"
)

var (
	emojiNamesOnce sync.Once
	emojiNames     map[string]string
)

// Shortest code name of an emoji, the gemoji style name chat platforms take for reactions
func EmojiShortName(emoji string) (string, bool) {
	emojiNamesOnce.Do(func() {
		emojiNames = make(map[string]string, len(emojiCodes))
		for name, value := range emojiCodes {
			if strings.ContainsAny(name, "().,'&") {
				continue
			}
			key := strings.ReplaceAll(value, "️", "")
			if cur, ok := emojiNames[key]; !ok || len(name) < len(cur) || (len(name) == len(cur) && name < cur) {
				emojiNames[key] = name
			}
		}
	})
	name, ok := emojiNames[strings.ReplaceAll(strings.TrimSpace(emoji), "️", "")]
	return name, ok
}
