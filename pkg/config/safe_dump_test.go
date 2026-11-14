package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSafeDumpBlockLayout(t *testing.T) {
	nested := map[string]any{"a": 1, "list": []any{"x", "y"}}
	SetMappingKeys(nested, []string{"a", "list"})
	item := map[string]any{"a": 1, "b": 2}
	SetMappingKeys(item, []string{"a", "b"})
	doc := map[string]any{
		"name": "coder", "enabled": true, "ratio": 0.5, "big": 1e20, "nothing": nil, "yes_str": "yes",
		"empty_str": "", "quote": "it's", "squote": "'lead", "tab": "a\tb", "multi": "a\nb", "emoji": "\U0001F600 hi",
		"url": "http://h:1/x", "colon": "a: b", "nested": nested, "empty_map": map[string]any{},
		"empty_list": []any{}, "items": []any{item}, "num_str": "0123", "dash": "- x",
	}
	SetMappingKeys(doc, []string{"name", "enabled", "ratio", "big", "nothing", "yes_str", "empty_str", "quote", "tab", "squote",
		"multi", "emoji", "url", "colon", "nested", "empty_map", "empty_list", "items", "num_str", "dash"})
	got, err := SafeDump(doc)
	if err != nil {
		t.Fatal(err)
	}
	want := "name: coder\nenabled: true\nratio: 0.5\nbig: 1e+20\nnothing: null\nyes_str: 'yes'\nempty_str: ''\n" +
		"quote: it's\ntab: \"a\\tb\"\nsquote: \"'lead\"\nmulti: \"a\\nb\"\nemoji: \U0001F600 hi\nurl: http://h:1/x\ncolon: 'a: b'\n" +
		"nested:\n  a: 1\n  list:\n    - x\n    - 'y'\nempty_map: {}\nempty_list: []\nitems:\n  - a: 1\n    b: 2\n" +
		"num_str: '0123'\ndash: '- x'\n"
	if string(got) != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestSafeDumpFoldsPlainAtWidth80(t *testing.T) {
	got, err := SafeDump(map[string]any{"description": strings.TrimSpace(strings.Repeat("word ", 20))})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "description: word word word word word word word word word word word word word \n  word word word word word word word\n" {
		t.Fatalf("folded %q", got)
	}
	long := "https://example.invalid/" + strings.Repeat("x", 90)
	got, _ = SafeDump(map[string]any{"u": long})
	if string(got) != "u: \n  "+long+"\n" {
		t.Fatalf("long word %q", got)
	}
}

func TestSafeDumpRootShapes(t *testing.T) {
	cases := map[string]any{"{}\n": map[string]any{}, "k: v\n": map[string]string{"k": "v"}, "t: 1\n": Tree{"t": 1}, "foo\n...\n": "foo", "  - a\n  - 1\n": []any{"a", 1}}
	for want, v := range cases {
		if got, err := SafeDump(v); err != nil || string(got) != want {
			t.Fatalf("%v -> %q %v", v, got, err)
		}
	}
	if _, err := SafeDump(map[string]any{"c": make(chan int)}); err == nil {
		t.Fatal("unrepresentable value accepted")
	}
}

func TestAtomicYAMLWriteFailedDumpLeavesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.yaml")
	if err := AtomicYAMLWrite(context.Background(), path, map[string]any{"a": "b"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	restore := SwapSafeDump(func(any) ([]byte, error) { return nil, errors.New("simulated interruption mid-write") })
	err := AtomicYAMLWrite(context.Background(), path, map[string]any{"a": "c"}, "")
	restore()
	if err == nil {
		t.Fatal("write succeeded")
	}
	if data, _ := os.ReadFile(path); string(data) != "a: b\n" {
		t.Fatalf("changed %q", data)
	}
	if err := AtomicYAMLWrite(context.Background(), path, map[string]any{"a": "c"}, "# tail\n"); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	data, _ := os.ReadFile(path)
	if string(data) != "a: c\n# tail\n" || fi.Mode().Perm() != 0o600 {
		t.Fatalf("rewrite %q %v", data, fi.Mode())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temp files left %v", entries)
	}
}

// Temp files a failed or finished atomic write left beside the target
func leftTempFiles(t *testing.T, dir, pattern string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func TestAtomicYamlWriteWritesValidYaml(t *testing.T) {
	target := filepath.Join(t.TempDir(), "data.yaml")
	data := map[string]any{"key": "value", "nested": map[string]any{"a": 1}}
	if err := AtomicYAMLWrite(context.Background(), target, data, ""); err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(target)
	if loaded, err := SafeLoad(text); err != nil || !reflect.DeepEqual(loaded, data) {
		t.Fatalf("reload = %#v %v", loaded, err)
	}
}

func TestAtomicYamlWriteCleansUpTempFileOnBaseexception(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "data.yaml")
	original := map[string]any{"preserved": true}
	text, err := SafeDumpWith(original, DefaultSafeDumpOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, text, 0o644); err != nil {
		t.Fatal(err)
	}
	abort := errors.New("SimulatedAbort")
	restore := SwapSafeDump(func(any) ([]byte, error) { return nil, abort })
	err = AtomicYAMLWrite(context.Background(), target, map[string]any{"new": true}, "")
	restore()
	if !errors.Is(err, abort) {
		t.Fatalf("err = %v", err)
	}
	if tmp := leftTempFiles(t, dir, "*.tmp*"); len(tmp) != 0 {
		t.Fatalf("temp files %v", tmp)
	}
	after, _ := os.ReadFile(target)
	if loaded, err := SafeLoad(after); err != nil || !reflect.DeepEqual(loaded, original) {
		t.Fatalf("reload = %#v %v", loaded, err)
	}
}

func TestAtomicYamlWriteWritesUnicodeUnescapedAndRoundTrips(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config.yaml")
	data := yamlDict(
		"personalities", yamlDict(
			"kawaii", "kawaii desu~! (◕‿◕) ★ ♪ ヽ(>∀<☆)ノ",
			"catgirl", "nya~! (=^･ω･^=) ฅ^•ﻌ•^ฅ",
			"surfer", "Cowabunga! 🤙 totally rad bro",
			"hype", "LET'S GOOOO!!! 🔥 LEGENDARY!",
		),
		"display", yamlDict("cursor", " ▉"),
	)
	if err := AtomicYAMLWrite(context.Background(), target, data, ""); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(target)
	text := string(raw)
	if strings.Contains(text, `\U`) || strings.Contains(text, `\u`) {
		t.Fatalf("escapes in %q", text)
	}
	if !strings.Contains(text, "🔥") || !strings.Contains(text, "(=^･ω･^=)") {
		t.Fatalf("glyphs missing in %q", text)
	}
	if loaded, err := SafeLoad(text); err != nil || !reflect.DeepEqual(loaded, data) {
		t.Fatalf("reload = %#v %v", loaded, err)
	}
}

func TestAtomicWriteThenKeyUpdateKeepsLayoutAndValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := yamlDict("custom_providers", []any{yamlDict("name", "Tëst 🦀", "base_url", "https://example.com")})
	if err := AtomicYAMLWrite(context.Background(), path, data, ""); err != nil {
		t.Fatal(err)
	}
	initial, _ := os.ReadFile(path)
	if err := atomicRoundtripYAMLUpdate(context.Background(), path, "approvals.mode", "off"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	content := string(raw)
	want := map[string]any{"custom_providers": data["custom_providers"], "approvals": map[string]any{"mode": "off"}}
	if !strings.Contains(content, "Tëst 🦀") {
		t.Fatalf("content %q", content)
	}
	if tmp := leftTempFiles(t, dir, ".config_*.tmp"); len(tmp) != 0 {
		t.Fatalf("temp files %v", tmp)
	}
	doc, err := NewRoundtripYAML().Load(content)
	if err != nil {
		t.Fatal(err)
	}
	if loaded, err := doc.Value(); err != nil || !reflect.DeepEqual(loaded, want) {
		t.Fatalf("roundtrip = %#v %v", loaded, err)
	}
	if !strings.HasPrefix(content, string(initial)) || !strings.Contains(content, "\n  - ") {
		t.Fatalf("layout %q after %q", content, initial)
	}
	if loaded, err := SafeLoad(content); err != nil || !reflect.DeepEqual(loaded, want) {
		t.Fatalf("safe_load = %#v %v", loaded, err)
	}
}

func TestFailedAtomicYamlWriteKeepsOriginal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := "# keep original\nkey: value\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AtomicYAMLWrite(context.Background(), path, map[string]any{"object": struct{}{}}, ""); !IsYAMLError(err) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != original {
		t.Fatalf("changed %q", got)
	}
	if tmp := leftTempFiles(t, dir, ".config_*.tmp"); len(tmp) != 0 {
		t.Fatalf("temp files %v", tmp)
	}
}
