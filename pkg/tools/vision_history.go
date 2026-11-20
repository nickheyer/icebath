package tools

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Embed byte budget bounds, 256 KB keeps a 1568px screenshot cheap enough to ride the session
const (
	defaultEmbedTargetBytes = 256 * 1024
	minEmbedTargetBytes     = 64 * 1024
	maxEmbedTargetBytes     = 4 * 1024 * 1024
)

// Default per image embed cap inside a delegated subagent, the main agent stays unlimited
const subagentRepeatCap = 3

// Upper bound on remembered session and image pairs before the oldest are evicted
const repeatCountsMaxKeys = 4096

// Env.Values key holding the image keys attached natively to the running user turn
const nativeTurnImagesKey = "vision.native_turn_images"

// One session and image identity counted against the per image cap
type repeatKey struct {
	session string
	image   string
}

// Insertion ordered embed counts so eviction drops the oldest pair first
var repeatCounts = struct {
	sync.Mutex
	order *list.List
	index map[repeatKey]*list.Element
	count map[repeatKey]int
}{order: list.New(), index: map[repeatKey]*list.Element{}, count: map[repeatKey]int{}}

// Value under the vision config section, nil when unset
func cfgVision(env *Env, key string) any {
	return cfgTree(env).Sub("vision")[key]
}

// Embed byte budget from vision.embed_target_bytes clamped to 64 KiB through 4 MiB, the default on a bad value
func resolveEmbedTargetBytes(env *Env) int {
	raw, present := cfgTree(env).Sub("vision")["embed_target_bytes"]
	if !present || raw == nil {
		return defaultEmbedTargetBytes
	}
	if _, isBool := raw.(bool); isBool {
		return defaultEmbedTargetBytes
	}
	target, ok := pyInt(raw)
	if !ok {
		return defaultEmbedTargetBytes
	}
	return min(max(target, minEmbedTargetBytes), maxEmbedTargetBytes)
}

// ResolveEmbedTargetBytes exposes the history embed byte budget to other tool packages
func ResolveEmbedTargetBytes(env *Env) int { return resolveEmbedTargetBytes(env) }

// Integer conversion with Python int semantics for ints, truncated floats, and numeric strings
func pyInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case uint64:
		return int(t), true
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return 0, false
		}
		return int(t), true
	case string:
		n, err := strconv.Atoi(strings.ReplaceAll(strings.TrimSpace(t), "_", ""))
		return n, err == nil
	}
	return 0, false
}

// Per image embed cap for this session, zero meaning unlimited
func resolveRepeatCap(env *Env) int {
	raw := cfgVision(env, "max_calls_per_image")
	if raw != nil && raw != "" {
		if _, isBool := raw.(bool); !isBool {
			if n, ok := pyInt(raw); ok {
				return max(n, 0)
			}
		}
	}
	if isDelegatedChildProcessContext(env) {
		return subagentRepeatCap
	}
	return 0
}

// Stable identity for an image source, local paths resolved, URL fragments dropped, data URLs hashed
func visionImageKey(env *Env, imageURL string) string {
	if strings.HasPrefix(imageURL, "data:") {
		sum := sha256.Sum256([]byte(strings.ToValidUTF8(imageURL, "")))
		return "data:" + hex.EncodeToString(sum[:])[:32]
	}
	stripped := strings.TrimPrefix(strings.SplitN(imageURL, "#", 2)[0], "file://")
	if strings.Contains(stripped, "://") {
		return "url:" + stripped
	}
	return "file:" + resolveAgainstCwd(env, pyExpandUser(stripped))
}

// Session and image pair the cap counts against
func visionCountKey(env *Env, sessionID, imageURL string) repeatKey {
	session := sessionContextValue(env, "ICEBATH_SESSION_ID")
	if session == "" {
		session = sessionID
	}
	return repeatKey{session: session, image: visionImageKey(env, imageURL)}
}

// Increments one pair and evicts the oldest pairs past the bound, caller holds the lock
func recordEmbedLocked(key repeatKey) {
	if _, ok := repeatCounts.index[key]; !ok {
		repeatCounts.index[key] = repeatCounts.order.PushBack(key)
	}
	repeatCounts.count[key]++
	for len(repeatCounts.count) > repeatCountsMaxKeys {
		oldest := repeatCounts.order.Front()
		k := oldest.Value.(repeatKey)
		repeatCounts.order.Remove(oldest)
		delete(repeatCounts.index, k)
		delete(repeatCounts.count, k)
	}
}

// Reserves one native embed for the session, a tool error once the per session cap is spent
func repeatRefusal(env *Env, sessionID, imageURL string) string {
	limit := resolveRepeatCap(env)
	if limit <= 0 {
		return ""
	}
	key := visionCountKey(env, sessionID, imageURL)
	repeatCounts.Lock()
	count := repeatCounts.count[key]
	if count < limit {
		recordEmbedLocked(key)
		repeatCounts.Unlock()
		return ""
	}
	repeatCounts.Unlock()
	return toolErrorJSON(fmt.Sprintf("vision_analyze refused: this image has already been loaded into context %d time(s) "+
		"in this session (region crops of the same file count too), and every native load re-sends "+
		"the full image on each later API call. Answer from what you can already see, or ask the "+
		"user. (vision.max_calls_per_image = %d; 0 = unlimited)", count, limit), Dict("success", false))
}

// Refusal text for an image embedded past its repeat cap
func RepeatRefusal(env *Env, sessionID, imageURL string) string {
	return repeatRefusal(env, sessionID, imageURL)
}

// Counts one successful native embed for uncapped sessions
func recordEmbed(env *Env, sessionID, imageURL string) {
	key := visionCountKey(env, sessionID, imageURL)
	repeatCounts.Lock()
	defer repeatCounts.Unlock()
	recordEmbedLocked(key)
}

// Gives back a reserved slot when the embed did not happen
func releaseEmbed(env *Env, sessionID, imageURL string) {
	key := visionCountKey(env, sessionID, imageURL)
	repeatCounts.Lock()
	defer repeatCounts.Unlock()
	if count := repeatCounts.count[key]; count > 1 {
		repeatCounts.count[key] = count - 1
		return
	}
	if el, ok := repeatCounts.index[key]; ok {
		repeatCounts.order.Remove(el)
	}
	delete(repeatCounts.index, key)
	delete(repeatCounts.count, key)
}

// Handle line every native attachment writes into the user text part
var nativeHandleRe = regexp.MustCompile(`(?m)^\[Image attached(?: at)?: (.+?)\][ \t\r\f\v]*$`)

// Content parts of a user message as maps whatever the slice type
func contentParts(userMessage any) []map[string]any {
	switch v := userMessage.(type) {
	case []map[string]any:
		return v
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, p := range v {
			if m, ok := p.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// Image keys of the handles written into a user message that carries native image parts
func nativeTurnKeys(env *Env, userMessage any) map[string]bool {
	parts := contentParts(userMessage)
	hasImage := false
	var texts []string
	for _, p := range parts {
		switch p["type"] {
		case "image_url":
			hasImage = true
		case "text":
			s, _ := p["text"].(string)
			texts = append(texts, s)
		}
	}
	keys := map[string]bool{}
	if !hasImage {
		return keys
	}
	for _, m := range nativeHandleRe.FindAllStringSubmatch(strings.Join(texts, "\n"), -1) {
		if handle := strings.TrimSpace(m[1]); handle != "" {
			keys[visionImageKey(env, handle)] = true
		}
	}
	return keys
}

// NativeTurnImages scopes the images natively attached to a user message to the running turn and returns the restore func
func NativeTurnImages(env *Env, userMessage any) func() {
	if env == nil || env.Values == nil {
		return func() {}
	}
	prev, had := env.Values.Load(nativeTurnImagesKey)
	env.Values.Store(nativeTurnImagesKey, nativeTurnKeys(env, userMessage))
	return func() {
		if had {
			env.Values.Store(nativeTurnImagesKey, prev)
		} else {
			env.Values.Delete(nativeTurnImagesKey)
		}
	}
}

// Ordered result telling the model the image already rides the user turn
type alreadyInContextResult struct {
	Success          bool   `json:"success"`
	AlreadyInContext bool   `json:"already_in_context"`
	Message          string `json:"message"`
}

// Text result when the image already rides the active user turn natively, empty otherwise
func nativeTurnDuplicate(env *Env, imageURL string, region any) string {
	if region != nil || env == nil || env.Values == nil {
		return ""
	}
	v, ok := env.Values.Load(nativeTurnImagesKey)
	if !ok {
		return ""
	}
	keys, _ := v.(map[string]bool)
	if !keys[visionImageKey(env, imageURL)] {
		return ""
	}
	return JSONASCII(alreadyInContextResult{Success: true, AlreadyInContext: true,
		Message: "This image is already attached natively to the current user message — you can see it " +
			"now. Answer with your built-in vision; pass a `region` to zoom into part of it."})
}
