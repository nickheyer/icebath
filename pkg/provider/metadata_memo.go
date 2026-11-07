package provider

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Entry count bound of the message token memo
var msgTokensCacheMax = 4096

// Retained bytes budget of the message token memo
var msgTokensCacheMaxBytes = 16 * 1024 * 1024

// Fingerprint structure cost per pinned string leaf
const leafOverheadBytes = 120

// Fixed cost of one string object beside its characters
const stringHeaderBytes = 49

// Entries looked up this recently are never evicted
const activeWindow = 60 * time.Second

// MemoPins holds an entry's pinned strings stamped with its last lookup
type MemoPins struct {
	Strings []string
	Touched time.Time
}

// Memo entry of text tokens and image count for one message fingerprint
type messageTokenEntry struct {
	pins   *MemoPins
	text   int
	images int
}

type msgTokensMemo struct {
	mu       sync.Mutex
	order    *list.List
	index    map[string]*list.Element
	retained int
	clock    func() time.Time
}

type msgTokensItem struct {
	key   string
	entry messageTokenEntry
}

var msgTokensCache = &msgTokensMemo{order: list.New(), index: map[string]*list.Element{}, clock: time.Now}

func entryBytes(e messageTokenEntry) int {
	total := 0
	for _, s := range e.pins.Strings {
		total += len(s) + stringHeaderBytes + leafOverheadBytes
	}
	return total
}

// Entry for the key marked most recently used
func lookupMessageTokens(key string) (messageTokenEntry, bool) {
	c := msgTokensCache
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.index[key]
	if !ok {
		return messageTokenEntry{}, false
	}
	item := el.Value.(*msgTokensItem)
	item.entry.pins.Touched = c.clock()
	c.order.MoveToBack(el)
	return item.entry, true
}

// Inserts the entry as most recent and evicts idle least recently used entries past either bound
func rememberMessageTokens(key string, entry messageTokenEntry) {
	size := entryBytes(entry)
	if size > msgTokensCacheMaxBytes {
		return
	}
	c := msgTokensCache
	now := c.clock()
	entry.pins.Touched = now
	idleBefore := now.Add(-activeWindow)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.order.Len() == 0 {
		c.retained = 0
	}
	if el, ok := c.index[key]; ok {
		c.retained -= entryBytes(el.Value.(*msgTokensItem).entry)
		c.order.Remove(el)
		delete(c.index, key)
	}
	c.index[key] = c.order.PushBack(&msgTokensItem{key: key, entry: entry})
	c.retained += size
	for c.order.Len() > 1 && (c.order.Len() > msgTokensCacheMax || c.retained > msgTokensCacheMaxBytes) {
		front := c.order.Front()
		item := front.Value.(*msgTokensItem)
		if !item.entry.pins.Touched.Before(idleBefore) {
			break
		}
		c.retained -= entryBytes(item.entry)
		c.order.Remove(front)
		delete(c.index, item.key)
	}
}

// RetainedBytes is the current retained bytes estimate of the message token memo
func RetainedBytes() int {
	c := msgTokensCache
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.retained
}

// Resets the message token memo
func clearMessageTokenMemo() {
	c := msgTokensCache
	c.mu.Lock()
	defer c.mu.Unlock()
	c.order.Init()
	c.index = map[string]*list.Element{}
	c.retained = 0
}

func longStrings(value any, out map[string]bool) {
	stack := []any{value}
	for len(stack) > 0 {
		v := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		switch t := v.(type) {
		case map[string]any:
			for _, item := range t {
				stack = append(stack, item)
			}
		case []any:
			stack = append(stack, t...)
		case []map[string]any:
			for _, item := range t {
				stack = append(stack, item)
			}
		case string:
			if len(t) > 64 {
				out[t] = true
			}
		}
	}
}

// ForgetMessageTokenMemo evicts entries pinning any long string of the messages that keep does not hold
func ForgetMessageTokenMemo(messages any, keep any) int {
	owned := map[string]bool{}
	longStrings(messages, owned)
	if len(owned) > 0 && keep != nil {
		kept := map[string]bool{}
		longStrings(keep, kept)
		for s := range kept {
			delete(owned, s)
		}
	}
	if len(owned) == 0 {
		return 0
	}
	c := msgTokensCache
	c.mu.Lock()
	defer c.mu.Unlock()
	var stale []*list.Element
	for el := c.order.Front(); el != nil; el = el.Next() {
		item := el.Value.(*msgTokensItem)
		for _, pin := range item.entry.pins.Strings {
			if owned[pin] {
				stale = append(stale, el)
				break
			}
		}
	}
	for _, el := range stale {
		item := el.Value.(*msgTokensItem)
		c.retained -= entryBytes(item.entry)
		c.order.Remove(el)
		delete(c.index, item.key)
	}
	return len(stale)
}

// Structural fingerprint of a message value, pinning every string it holds
func msgFingerprint(value any, pins *MemoPins) (string, bool) {
	var walk func(v any) (any, bool)
	walk = func(v any) (any, bool) {
		switch t := v.(type) {
		case nil, bool:
			return t, true
		case string:
			pins.Strings = append(pins.Strings, t)
			return []any{"s", t}, true
		case int:
			return []any{"n", "int", t}, true
		case int64:
			return []any{"n", "int", t}, true
		case *big.Int:
			return []any{"n", "int", t.String()}, true
		case float64:
			return []any{"n", "float", pyrepr.Float(t)}, true
		case json.Number:
			return []any{"n", "number", t.String()}, true
		case map[string]any:
			out := make([]any, 0, len(t)+1)
			out = append(out, "d")
			for _, k := range sortedStringKeys(t) {
				fv, ok := walk(t[k])
				if !ok {
					return nil, false
				}
				out = append(out, []any{k, fv})
			}
			return out, true
		case []any:
			out := make([]any, 0, len(t)+1)
			out = append(out, "l")
			for _, item := range t {
				fv, ok := walk(item)
				if !ok {
					return nil, false
				}
				out = append(out, fv)
			}
			return out, true
		case pyrepr.Tuple:
			out := make([]any, 0, len(t)+1)
			out = append(out, "t")
			for _, item := range t {
				fv, ok := walk(item)
				if !ok {
					return nil, false
				}
				out = append(out, fv)
			}
			return out, true
		case []map[string]any:
			out := make([]any, 0, len(t)+1)
			out = append(out, "l")
			for _, item := range t {
				fv, ok := walk(item)
				if !ok {
					return nil, false
				}
				out = append(out, fv)
			}
			return out, true
		}
		return nil, false
	}
	tree, ok := walk(value)
	if !ok {
		return "", false
	}
	raw, err := json.Marshal(tree)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), true
}

// Message row minus the persistence only fields that never reach the estimate
func withoutPersistenceFields(row map[string]any) map[string]any {
	out := make(map[string]any, len(row))
	for k, v := range row {
		if !persistenceOnlyFields[k] {
			out[k] = v
		}
	}
	return out
}

// Text token estimate the memo computes on a miss
var messageTextTokens = estimateMessageTokensWithoutImages

// Text tokens and image count of a row memoized on its fingerprint so a repriced image cost reuses the row
func estimateMessageTokensCached(msg any, imageCost int) int {
	keyed := msg
	if row, ok := msg.(map[string]any); ok {
		keyed = withoutPersistenceFields(row)
	}
	pins := &MemoPins{}
	key, ok := msgFingerprint(keyed, pins)
	if !ok {
		return messageTextTokens(msg) + countImageParts(msg)*imageCost
	}
	if cached, hit := lookupMessageTokens(key); hit {
		return cached.text + cached.images*imageCost
	}
	text, images := messageTextTokens(msg), countImageParts(msg)
	rememberMessageTokens(key, messageTokenEntry{pins: pins, text: text, images: images})
	return text + images*imageCost
}

// Memoized rough token weight of one message row at the given per image cost
func EstimateMessageTokensRough(msg any, imageCost int) int {
	return estimateMessageTokensCached(msg, imageCost)
}
