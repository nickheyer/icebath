package tui

import (
	"testing"
)

func TestVirtualHeightEstimates(t *testing.T) {
	t.Run("uses stable content keys across resumed message objects", func(t *testing.T) {
		m := msg{Role: "assistant", Text: "same text", Tools: []toolRow{{Call: "Search Files [long message]"}}}
		copied := m
		if messageHeightKey(m) != messageHeightKey(copied) {
			t.Fatal("keys differ")
		}
	})
}

func TestTermuxTUIMode(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	t.Run("defaults on inside Termux and honours the opt out", func(t *testing.T) {
		if !isTermuxTUIMode(env("TERMUX_VERSION", "0.118")) || !isTermuxTUIMode(env("PREFIX", termuxPrefix)) {
			t.Fatal("termux default")
		}
		if isTermuxTUIMode(env("TERMUX_VERSION", "0.118", "ICEBATH_TUI_TERMUX_MODE", "0")) || !isTermuxTUIMode(env("TERMUX_VERSION", "1", "ICEBATH_TUI_TERMUX_MODE", " Yes ")) {
			t.Fatal("override")
		}
		if isTermuxTUIMode(env("ICEBATH_TUI_TERMUX_MODE", "1")) {
			t.Fatal("outside termux")
		}
	})
	t.Run("hashes like the JavaScript djb2 xor over UTF-16 units", func(t *testing.T) {
		if got := hashText(""); got != "45h" {
			t.Fatalf("empty %q", got)
		}
		if hashText("a") == hashText("b") || hashText("😀") == hashText("x") {
			t.Fatal("collisions")
		}
	})
}
