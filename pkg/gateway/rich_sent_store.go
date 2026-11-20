package gateway

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	richSentMaxEntries   = 1000
	richSentMaxTextChars = 2000
)

// Serialises the load, merge and save of the index across goroutines
var richSentLock sync.Mutex

// Index file under the active profile state directory
func richSentStorePath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), "state", "rich_sent_index.json")
}

// Index contents, empty for a missing, unparseable or non object file
func richSentLoad(path string) *pyjson.Object {
	raw, err := os.ReadFile(path)
	if err != nil {
		return pyjson.NewObject()
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return pyjson.NewObject()
	}
	data, err := pyjson.Loads(text)
	if obj, ok := data.(*pyjson.Object); err == nil && ok {
		return obj
	}
	return pyjson.NewObject()
}

// Merges fields into the chat and message entry with a fresh timestamp, trimming the oldest past the cap
func richSentUpdate(ctx context.Context, chatID, messageID any, fields map[string]any) {
	path := richSentStorePath(ctx)
	richSentLock.Lock()
	defer richSentLock.Unlock()
	if os.MkdirAll(filepath.Dir(path), 0o777) != nil {
		return
	}
	data := richSentLoad(path)
	key := fmt.Sprintf("%v:%v", chatID, messageID)
	merged := pyjson.NewObject()
	if prior, ok := data.GetOr(key, nil).(*pyjson.Object); ok {
		for _, k := range prior.Keys {
			merged.Set(k, prior.Vals[k])
		}
	}
	for _, k := range []string{"t", "m"} {
		if v, ok := fields[k]; ok {
			merged.Set(k, v)
		}
	}
	merged.Set("ts", time.Now().Unix())
	data.Set(key, merged)
	if data.Len() > richSentMaxEntries {
		keys := append([]string(nil), data.Keys...)
		sort.SliceStable(keys, func(i, j int) bool { return richSentTS(data.Vals[keys[i]]) < richSentTS(data.Vals[keys[j]]) })
		for _, k := range keys[:data.Len()-richSentMaxEntries] {
			data.Delete(k)
		}
	}
	_ = config.AtomicJSONWriteWith(ctx, path, data)
}

// Timestamp of one entry, zero when it has none
func richSentTS(entry any) float64 {
	obj, ok := entry.(*pyjson.Object)
	if !ok {
		return 0
	}
	switch v := obj.GetOr("ts", 0).(type) {
	case int64:
		return float64(v)
	case int:
		return float64(v)
	case float64:
		return v
	}
	return 0
}

// Persists text sent or received for the chat and message, nothing for empty text or a missing key
func RichSentRecord(ctx context.Context, chatID, messageID any, text string) {
	if text == "" || messageID == nil || chatID == nil {
		return
	}
	runes := []rune(text)
	if len(runes) > richSentMaxTextChars {
		runes = runes[:richSentMaxTextChars]
	}
	richSentUpdate(ctx, chatID, messageID, map[string]any{"t": string(runes)})
}

// Persists local attachment path and mime pairs for the chat and message
func RichSentRecordMedia(ctx context.Context, chatID, messageID any, media [][2]string) {
	if len(media) == 0 || messageID == nil || chatID == nil {
		return
	}
	pairs := []any{}
	for _, m := range media {
		if m[0] != "" {
			pairs = append(pairs, []any{m[0], m[1]})
		}
	}
	richSentUpdate(ctx, chatID, messageID, map[string]any{"m": pairs})
}

// Stored entry of the chat and message, empty when absent
func richSentEntry(ctx context.Context, chatID, messageID any) *pyjson.Object {
	if messageID == nil || chatID == nil {
		return pyjson.NewObject()
	}
	if obj, ok := richSentLoad(richSentStorePath(ctx)).GetOr(fmt.Sprintf("%v:%v", chatID, messageID), nil).(*pyjson.Object); ok {
		return obj
	}
	return pyjson.NewObject()
}

// Stored text of the chat and message, false when none was recorded
func RichSentLookup(ctx context.Context, chatID, messageID any) (string, bool) {
	text, ok := richSentEntry(ctx, chatID, messageID).GetOr("t", nil).(string)
	return text, ok && text != ""
}

// Stored path and mime pairs of the chat and message whose file still exists
func RichSentLookupMedia(ctx context.Context, chatID, messageID any) [][2]string {
	out := [][2]string{}
	pairs, _ := richSentEntry(ctx, chatID, messageID).GetOr("m", nil).([]any)
	for _, raw := range pairs {
		pair, ok := raw.([]any)
		if !ok || len(pair) != 2 {
			continue
		}
		p, okP := pair[0].(string)
		mt, _ := pair[1].(string)
		if fi, err := os.Stat(p); okP && err == nil && fi.Mode().IsRegular() {
			out = append(out, [2]string{p, mt})
		}
	}
	return out
}
