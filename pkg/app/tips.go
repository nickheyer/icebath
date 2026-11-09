package app

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"

	"github.com/nickheyer/icebath/pkg/i18n"
)

const (
	TipKeyPrefix          = "tips.t"
	ComposerHintKeyPrefix = "tips.placeholder.p"
	tipKeyWidth           = 3
	composerHintKeyWidth  = 2
	maxTipKeys            = 10000
)

var (
	tipKeysMu    sync.Mutex
	tipKeysCache = map[string][]string{}
)

// Contiguous prefixed keys present in the English catalog, cached per prefix for the process lifetime
func catalogKeys(ctx context.Context, prefix string, width int) []string {
	tipKeysMu.Lock()
	cached, ok := tipKeysCache[prefix]
	tipKeysMu.Unlock()
	if ok {
		return cached
	}
	var keys []string
	for index := 1; index < maxTipKeys; index++ {
		key := fmt.Sprintf("%s%0*d", prefix, width, index)
		if i18n.T(ctx, key, i18n.DefaultLanguage, nil) == key {
			break
		}
		keys = append(keys, key)
	}
	tipKeysMu.Lock()
	tipKeysCache[prefix] = keys
	tipKeysMu.Unlock()
	return keys
}

// Forgets the probed key lists so a swapped locales dir is probed again
func ResetTipsCache() {
	tipKeysMu.Lock()
	tipKeysCache = map[string][]string{}
	tipKeysMu.Unlock()
}

// Catalog keys of every startup tip in catalog order
func tipKeys(ctx context.Context) []string { return catalogKeys(ctx, TipKeyPrefix, tipKeyWidth) }

// Catalog keys of every empty composer example prompt in catalog order
func composerHintKeys(ctx context.Context) []string {
	return catalogKeys(ctx, ComposerHintKeyPrefix, composerHintKeyWidth)
}

// Random tip in the active language, empty when the catalog has none
func getRandomTip(ctx context.Context) string {
	keys := tipKeys(ctx)
	if len(keys) == 0 {
		return ""
	}
	return i18n.T(ctx, keys[rand.IntN(len(keys))], "", nil)
}

// Rotating task oriented prompt for the empty composer in the active language, empty when the catalog has none
func getRandomComposerHint(ctx context.Context) string {
	keys := composerHintKeys(ctx)
	if len(keys) == 0 {
		return ""
	}
	return i18n.T(ctx, keys[rand.IntN(len(keys))], "", nil)
}
