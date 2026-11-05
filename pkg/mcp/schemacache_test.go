package mcp

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestConfigFingerprint(t *testing.T) {
	base := map[string]any{"command": "npx", "args": []any{"-y", "@playwright/mcp"}}
	fp := ConfigFingerprint(base)
	if len(fp) != 16 || fp != ConfigFingerprint(map[string]any{"command": "npx", "args": []any{"-y", "@playwright/mcp"}}) {
		t.Fatalf("unstable fingerprint %q", fp)
	}
	for _, changed := range []map[string]any{
		{"command": "npx", "args": []any{"-y", "@playwright/mcp", "--headless"}},
		{"command": "uvx", "args": []any{"-y", "@playwright/mcp"}},
		{"command": "npx", "args": []any{"-y", "@playwright/mcp"}, "tools": map[string]any{"include": []any{"a"}}},
	} {
		if ConfigFingerprint(changed) == fp {
			t.Fatalf("fingerprint ignored %v", changed)
		}
	}
	empty := map[string]any{"command": "npx", "args": []any{}}
	if ConfigFingerprint(empty) != ConfigFingerprint(map[string]any{"command": "npx", "args": []any{}, "timeout": 5, "enabled": true, "lazy": true}) {
		t.Fatal("non connection keys changed the fingerprint")
	}
	if got := pyJSONCompactASCII(map[string]any{"b": []any{1, 2.0, "\u00e9"}, "a": nil}); got != `{"a":null,"b":[1,2.0,"\u00e9"]}` {
		t.Fatalf("python json %s", got)
	}
}

func TestSchemaCacheRoundTrip(t *testing.T) {
	h := isolateHome(t)
	ctx := context.Background()
	tools := []any{map[string]any{"name": "t1", "description": "d", "inputSchema": map[string]any{"type": "object"}}}
	if err := WriteCacheEntry(ctx, "srv", "fp1", tools, []any{}, nil, ""); err != nil {
		t.Fatal(err)
	}
	entry := GetCachedEntry(ctx, "srv", "fp1")
	if entry == nil || !reflect.DeepEqual(ToolsFromCacheEntry(entry), tools) || len(UtilityToolsFromCacheEntry(entry)) != 0 {
		t.Fatalf("entry %v", entry)
	}
	if GetCachedEntry(ctx, "srv", "OTHER") != nil || GetCachedEntry(ctx, "nope", "fp") != nil {
		t.Fatal("mismatch or missing must miss")
	}
	path := filepath.Join(h, "cache", "mcp_schema_cache.json")
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("cache file %v %v", st, err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if GetCachedEntry(ctx, "srv", "fp1") != nil {
		t.Fatal("corrupt file must miss")
	}
	if err := WriteCacheEntry(ctx, "srv", "fp", nil, nil, nil, ""); err != nil || GetCachedEntry(ctx, "srv", "fp") == nil {
		t.Fatalf("write did not recover the file: %v", err)
	}
	if len(ToolsFromCacheEntry(map[string]any{"tools": "nope"})) != 0 || len(UtilityToolsFromCacheEntry(map[string]any{})) != 0 {
		t.Fatal("malformed shapes must read empty")
	}
}

func TestSchemaCacheTTL(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	now := 1000.0
	prev := schemaCacheNow
	schemaCacheNow = func() float64 { return now }
	t.Cleanup(func() { schemaCacheNow = prev })
	tools := []any{map[string]any{"name": "t"}}
	_ = WriteCacheEntry(ctx, "srv", "fp", tools, nil, nil, "")
	now += 1e9
	if GetCachedEntry(ctx, "srv", "fp") == nil {
		t.Fatal("entry without a TTL expired")
	}
	_ = WriteCacheEntry(ctx, "srv", "fp", tools, nil, fptr(60000), "private")
	entry := GetCachedEntry(ctx, "srv", "fp")
	if entry == nil || entry["ttl_ms"] != 60000.0 || entry["written_at"] == nil || entry["cache_scope"] != "private" {
		t.Fatalf("ttl entry %v", entry)
	}
	first := entry["written_at"].(float64)
	now += 0.01
	_ = WriteCacheEntry(ctx, "srv", "fp", tools, nil, fptr(60000), "private")
	if second := GetCachedEntry(ctx, "srv", "fp")["written_at"].(float64); second <= first {
		t.Fatalf("written_at did not advance %v %v", first, second)
	}
	_ = WriteCacheEntry(ctx, "srv", "fp", tools, nil, fptr(1000), "")
	now += 2
	if GetCachedEntry(ctx, "srv", "fp") != nil {
		t.Fatal("entry past its TTL was served")
	}
}
