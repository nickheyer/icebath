package config

import (
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func countEnvParses(t *testing.T) *int {
	t.Helper()
	InvalidateEnvFileCache("")
	calls := 0
	prev := parseEnvText
	parseEnvText = func(text string) map[string]string {
		calls++
		return prev(text)
	}
	t.Cleanup(func() {
		parseEnvText = prev
		InvalidateEnvFileCache("")
	})
	return &calls
}

func TestUnchangedEnvFileIsParsedOnceAndEachCallerGetsItsOwnMap(t *testing.T) {
	env := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(env, []byte("A=1\nB=2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := countEnvParses(t)
	want := map[string]string{"A": "1", "B": "2"}
	first := LoadEnvFile(env)
	first["INJECTED"] = "must-not-persist"
	for range 5 {
		got := LoadEnvFile(env)
		if !maps.Equal(got, want) {
			t.Fatalf("got %v", got)
		}
		got["EXT"] = "vault"
	}
	if *calls != 1 {
		t.Fatalf("expected one parse, got %d", *calls)
	}
}

func TestEnvFileEditsAndDeletionAreSeen(t *testing.T) {
	env := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(env, []byte("KEY=aaa\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := countEnvParses(t)
	if got := LoadEnvFile(env); got["KEY"] != "aaa" {
		t.Fatalf("got %v", got)
	}
	info, _ := os.Stat(env)
	if err := os.WriteFile(env, []byte("KEY=bbb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bumped := info.ModTime().Add(10 * time.Second)
	_ = os.Chtimes(env, bumped, bumped)
	if got := LoadEnvFile(env); got["KEY"] != "bbb" || *calls != 2 {
		t.Fatalf("a same size rewrite is seen, got %v after %d parses", got, *calls)
	}
	if err := os.Remove(env); err != nil {
		t.Fatal(err)
	}
	if got := LoadEnvFile(env); len(got) != 0 {
		t.Fatalf("a deleted file reads empty, got %v", got)
	}
	if err := os.WriteFile(env, []byte("KEY=ccc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := LoadEnvFile(env); got["KEY"] != "ccc" || *calls != 3 {
		t.Fatalf("a recreated file is parsed again, got %v after %d parses", got, *calls)
	}
}

func TestSameSizeEnvRewriteInsideOneTimestampTickIsSeen(t *testing.T) {
	env := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(env, []byte("TUI_RELOAD_PROBE=one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := countEnvParses(t)
	info, _ := os.Stat(env)
	for range 2 {
		if got := LoadEnvFile(env); got["TUI_RELOAD_PROBE"] != "one" {
			t.Fatalf("got %v", got)
		}
	}
	if *calls != 1 {
		t.Fatalf("unchanged bytes skip the parse, got %d", *calls)
	}
	if err := os.WriteFile(env, []byte("TUI_RELOAD_PROBE=two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(env, info.ModTime(), info.ModTime())
	if got := LoadEnvFile(env); got["TUI_RELOAD_PROBE"] != "two" || *calls != 2 {
		t.Fatalf("only the bytes tell the versions apart, got %v after %d parses", got, *calls)
	}
}

func TestInvalidateEnvFileCacheDropsOnePathOrAll(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.env"), filepath.Join(dir, "b.env")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("K=v\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	calls := countEnvParses(t)
	LoadEnvFile(a)
	LoadEnvFile(b)
	InvalidateEnvFileCache(a)
	LoadEnvFile(a)
	LoadEnvFile(b)
	if *calls != 3 {
		t.Fatalf("one path is dropped, got %d parses", *calls)
	}
	InvalidateEnvFileCache("")
	LoadEnvFile(a)
	LoadEnvFile(b)
	if *calls != 5 {
		t.Fatalf("every path is dropped, got %d parses", *calls)
	}
}
