package config

import (
	"errors"
	"os"
	"path/filepath"
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
	lines := strings.Split(strings.TrimSuffix(string(got), "\n"), "\n")
	if len(lines) != 2 || len(lines[0]) != 82 || lines[1] != "  word word word word word word" {
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
	if err := AtomicYAMLWrite(path, map[string]any{"a": "b"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	restore := SwapSafeDump(func(any) ([]byte, error) { return nil, errors.New("simulated interruption mid-write") })
	err := AtomicYAMLWrite(path, map[string]any{"a": "c"}, "")
	restore()
	if err == nil {
		t.Fatal("write succeeded")
	}
	if data, _ := os.ReadFile(path); string(data) != "a: b\n" {
		t.Fatalf("changed %q", data)
	}
	if err := AtomicYAMLWrite(path, map[string]any{"a": "c"}, "# tail\n"); err != nil {
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
