package gateway

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Vision prompt that describes a sticker once for the description cache
const StickerVisionPrompt = "Describe this sticker in 1-2 sentences. Focus on what it depicts -- " +
	"character, action, emotion. Be concise and objective."

var stickerCacheMu sync.Mutex

// Sticker cache file of the active profile home at call time
func stickerCachePath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), "sticker_cache.json")
}

// Sticker cache read BOM tolerant, empty on a missing, unreadable or malformed file
func loadStickerCache(ctx context.Context) *jsonutil.Object {
	raw, err := os.ReadFile(stickerCachePath(ctx))
	if err != nil {
		return jsonutil.NewObject()
	}
	text := strings.TrimPrefix(string(raw), "\uFEFF")
	obj, err := jsonutil.DecodeObject(text)
	if err != nil || obj == nil {
		return jsonutil.NewObject()
	}
	return obj
}

// Cached description entry of a sticker unique id, nil when never described
func GetCachedStickerDescription(ctx context.Context, fileUniqueID string) map[string]any {
	value, ok := loadStickerCache(ctx).Get(fileUniqueID)
	if !ok {
		return nil
	}
	switch entry := value.(type) {
	case *jsonutil.Object:
		out := map[string]any{}
		for _, k := range entry.Keys() {
			out[k] = entry.Value(k)
		}
		return out
	case map[string]any:
		return entry
	}
	return nil
}

// Stores a vision description under the stable sticker id through an atomic JSON write
func CacheStickerDescription(ctx context.Context, fileUniqueID, description, emoji, setName string) error {
	entry := jsonutil.ObjectOf("description", description, "emoji", emoji, "set_name", setName,
		"cached_at", float64(time.Now().UnixNano())/1e9)
	stickerCacheMu.Lock()
	defer stickerCacheMu.Unlock()
	cache := loadStickerCache(ctx)
	cache.Set(fileUniqueID, entry)
	return config.AtomicJSONWrite(ctx, stickerCachePath(ctx), cache)
}

// Injection text naming the sticker emoji and pack with its description
func BuildStickerInjection(description, emoji, setName string) string {
	context := ""
	if emoji != "" {
		context = " " + emoji
	}
	if setName != "" && emoji != "" {
		context += fmt.Sprintf(" from \"%s\"", setName)
	}
	return fmt.Sprintf("[The user sent a sticker%s. It shows: \"%s\"]", context, description)
}

// Injection text for an animated or video sticker vision cannot read
func BuildAnimatedStickerInjection(emoji string) string {
	if emoji != "" {
		return fmt.Sprintf("[The user sent an animated sticker %s. I can't see animated ones yet, but the emoji suggests: %s]", emoji, emoji)
	}
	return "[The user sent an animated sticker. I can't see animated ones yet]"
}
