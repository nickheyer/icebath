package gatewayrun

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Typed reply synonyms of an approval input, English first then the active language, lower cased and unique
func approvalInputWords(ctx context.Context, inputKey string) []string {
	key := "approval.inputs." + inputKey
	seen := map[string]bool{}
	words := []string{}
	for _, raw := range []string{i18n.T(ctx, key, i18n.DefaultLanguage, nil), i18n.T(ctx, key, "", nil)} {
		if raw == key {
			continue
		}
		for _, word := range strings.Split(raw, ",") {
			word = pystr.Lower(pystr.Strip(word))
			if word != "" && !seen[word] {
				seen[word] = true
				words = append(words, word)
			}
		}
	}
	return words
}

// Remainder after slot when text starts with it as a whole colon delimited slot, false otherwise
func stripSlot(text, slot string) (string, bool) {
	if text == slot {
		return "", true
	}
	if strings.HasPrefix(text, slot+":") {
		return text[len(slot)+1:], true
	}
	return "", false
}

// True when the first slot of tail is slot
func tailHasSlot(tail, slot string) bool {
	_, ok := stripSlot(tail, slot)
	return ok
}

// Chat type and tail when key names the same chat as prefix and chat id, a workspace scope optional
func sameChatKeySlots(key, prefix, chatID string, scopeID *string) (chatType, tail string, ok bool) {
	if !strings.HasPrefix(key, prefix) {
		return "", "", false
	}
	chatType, rem, _ := strings.Cut(key[len(prefix):], ":")
	candidates := []string{chatID}
	if scopeID != nil && *scopeID != "" {
		candidates = []string{*scopeID + ":" + chatID, chatID}
	}
	for _, candidate := range candidates {
		if tail, ok := stripSlot(rem, candidate); ok {
			return chatType, tail, true
		}
	}
	return "", "", false
}
