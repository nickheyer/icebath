package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// File under the home cache dir holding every server's cached tool manifest
const schemaCacheFilename = "mcp_schema_cache.json"

var schemaCacheMu sync.Mutex

// Clock the schema cache stamps and ages entries with, swapped by tests
var schemaCacheNow = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Path of the schema cache in the active home
func schemaCachePath(ctx context.Context) string {
	return filepath.Join(home.CacheDir(home.Dir(ctx)), schemaCacheFilename)
}

// Python json.dumps of a value with sorted keys, compact separators and ASCII escapes
func pyJSONCompactASCII(v any) string {
	var b strings.Builder
	var enc func(v any)
	enc = func(v any) {
		switch x := v.(type) {
		case nil:
			b.WriteString("null")
		case bool:
			if x {
				b.WriteString("true")
			} else {
				b.WriteString("false")
			}
		case string:
			b.WriteByte('"')
			for _, r := range x {
				switch {
				case r == '"':
					b.WriteString(`\"`)
				case r == '\\':
					b.WriteString(`\\`)
				case r == '\n':
					b.WriteString(`\n`)
				case r == '\r':
					b.WriteString(`\r`)
				case r == '\t':
					b.WriteString(`\t`)
				case r == '\b':
					b.WriteString(`\b`)
				case r == '\f':
					b.WriteString(`\f`)
				case r < 0x20 || (r > 0x7e && r <= 0xffff):
					fmt.Fprintf(&b, `\u%04x`, r)
				case r > 0xffff:
					hi, lo := utf16.EncodeRune(r)
					fmt.Fprintf(&b, `\u%04x\u%04x`, hi, lo)
				default:
					b.WriteRune(r)
				}
			}
			b.WriteByte('"')
		case int:
			b.WriteString(strconv.Itoa(x))
		case int64:
			b.WriteString(strconv.FormatInt(x, 10))
		case uint64:
			b.WriteString(strconv.FormatUint(x, 10))
		case float64:
			switch {
			case math.IsNaN(x):
				b.WriteString("NaN")
			case math.IsInf(x, 1):
				b.WriteString("Infinity")
			case math.IsInf(x, -1):
				b.WriteString("-Infinity")
			default:
				b.WriteString(pyFloatRepr(x))
			}
		case []any:
			b.WriteByte('[')
			for i, item := range x {
				if i > 0 {
					b.WriteByte(',')
				}
				enc(item)
			}
			b.WriteByte(']')
		case []string:
			items := make([]any, len(x))
			for i, s := range x {
				items[i] = s
			}
			enc(items)
		case map[string]any:
			b.WriteByte('{')
			for i, k := range orderedKeys(x) {
				if i > 0 {
					b.WriteByte(',')
				}
				enc(k)
				b.WriteByte(':')
				enc(x[k])
			}
			b.WriteByte('}')
		default:
			enc(pyStr(v))
		}
	}
	enc(v)
	return b.String()
}

// Sorted elements of a filter value the way sorted() iterates it, a string yielding its characters
func sortedFilter(v any) []any {
	var items []string
	switch x := v.(type) {
	case nil:
		return []any{}
	case string:
		for _, r := range x {
			items = append(items, string(r))
		}
	case []any:
		for _, item := range x {
			items = append(items, pyStr(item))
		}
	case []string:
		items = append(items, x...)
	default:
		if !pyTruthy(v) {
			return []any{}
		}
		items = append(items, pyStr(v))
	}
	sort.Strings(items)
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

// Stable sixteen hex digit hash of the connection defining parts of a server entry
func ConfigFingerprint(cfg map[string]any) string {
	toolsFilter, _ := cfg["tools"].(map[string]any)
	args := cfg["args"]
	if !pyTruthy(args) {
		args = []any{}
	}
	include, exclude := toolsFilter["include"], toolsFilter["exclude"]
	if !pyTruthy(include) {
		include = nil
	}
	if !pyTruthy(exclude) {
		exclude = nil
	}
	payload := map[string]any{
		"command":       cfg["command"],
		"args":          args,
		"url":           cfg["url"],
		"transport":     cfg["transport"],
		"tools_include": sortedFilter(include),
		"tools_exclude": sortedFilter(exclude),
	}
	sum := sha256.Sum256([]byte(pyJSONCompactASCII(payload)))
	return hex.EncodeToString(sum[:])[:16]
}

// Every cached entry, empty when the file is missing, unreadable or not an object
func loadSchemaCache(ctx context.Context) map[string]any {
	p := schemaCachePath(ctx)
	raw, err := os.ReadFile(p)
	if err != nil {
		if !os.IsNotExist(err) {
			mcpLogger().Debug(fmt.Sprintf("Could not read MCP schema cache %s: %v", p, err))
		}
		return map[string]any{}
	}
	var data any
	if err := json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &data); err != nil {
		mcpLogger().Debug(fmt.Sprintf("Could not read MCP schema cache %s: %v", p, err))
		return map[string]any{}
	}
	m, ok := data.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return m
}

// Writes every entry atomically with owner only permissions
func saveSchemaCache(ctx context.Context, data map[string]any) error {
	p := schemaCachePath(ctx)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(data); err != nil {
		return err
	}
	return config.AtomicWrite(p, bytes.TrimRight(buf.Bytes(), "\n"), 0o600)
}

// Cached entry of a server when its fingerprint matches and its recorded TTL still holds
func GetCachedEntry(ctx context.Context, serverName, fingerprint string) map[string]any {
	schemaCacheMu.Lock()
	entry, _ := loadSchemaCache(ctx)[serverName].(map[string]any)
	schemaCacheMu.Unlock()
	if entry == nil || entry["fingerprint"] != fingerprint {
		return nil
	}
	ttl, ttlOK := jsonNumber(entry["ttl_ms"])
	written, writtenOK := jsonNumber(entry["written_at"])
	if ttlOK && writtenOK && (schemaCacheNow()-written)*1000.0 >= ttl {
		return nil
	}
	return entry
}

// Number of a decoded JSON value, booleans counting as the Python ints they subclass
func jsonNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// Persists a server's tool schemas after a live connect, the TTL hints anchored at the write time
func WriteCacheEntry(ctx context.Context, serverName, fingerprint string, tools, utilityTools []any, ttlMs *float64, cacheScope string) error {
	if utilityTools == nil {
		utilityTools = []any{}
	}
	if tools == nil {
		tools = []any{}
	}
	entry := map[string]any{"fingerprint": fingerprint, "tools": tools, "utility_tools": utilityTools}
	if ttlMs != nil {
		entry["ttl_ms"] = *ttlMs
		entry["written_at"] = schemaCacheNow()
	}
	if cacheScope != "" {
		entry["cache_scope"] = cacheScope
	}
	normalized, err := jsonRoundTrip(entry)
	if err != nil {
		return err
	}
	schemaCacheMu.Lock()
	defer schemaCacheMu.Unlock()
	data := loadSchemaCache(ctx)
	if _, stamped := entry["written_at"]; !stamped && reflect.DeepEqual(data[serverName], normalized) {
		return nil
	}
	data[serverName] = normalized
	return saveSchemaCache(ctx, data)
}

// Value as it reads back after a JSON write, numbers as float64 and structs as maps
func jsonRoundTrip(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// List field of an entry copied, empty when absent or not a list
func listField(entry map[string]any, key string) []any {
	if l, ok := entry[key].([]any); ok {
		return append([]any(nil), l...)
	}
	return []any{}
}

// Cached tool dicts with name, description and inputSchema
func ToolsFromCacheEntry(entry map[string]any) []any { return listField(entry, "tools") }

// Cached schema and handler_key utility rows
func UtilityToolsFromCacheEntry(entry map[string]any) []any { return listField(entry, "utility_tools") }
