package tui

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Env lookup over a map, absent keys unset
func lookupOf(kv map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := kv[k]
		return v, ok
	}
}

// Executable script named name in dir
func exe(t *testing.T, dir, name string) string {
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveEditor(t *testing.T) {
	eq := func(t *testing.T, got []string, want ...string) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %q want %q", got, want)
		}
	}
	t.Run("honors $VISUAL above all else", func(t *testing.T) {
		eq(t, resolveEditor(lookupOf(map[string]string{"EDITOR": "vim", "PATH": t.TempDir(), "VISUAL": "helix"}), "linux"), "helix")
	})
	t.Run("falls back to $EDITOR when $VISUAL is unset", func(t *testing.T) {
		eq(t, resolveEditor(lookupOf(map[string]string{"EDITOR": "nvim", "PATH": t.TempDir()}), "linux"), "nvim")
	})
	t.Run("shell-tokenizes editors with arguments", func(t *testing.T) {
		dir := t.TempDir()
		eq(t, resolveEditor(lookupOf(map[string]string{"EDITOR": "code --wait", "PATH": dir}), "linux"), "code", "--wait")
		eq(t, resolveEditor(lookupOf(map[string]string{"PATH": dir, "VISUAL": "emacsclient -t"}), "linux"), "emacsclient", "-t")
	})
	t.Run("ignores whitespace-only env vars", func(t *testing.T) {
		dir := t.TempDir()
		expected := exe(t, dir, "editor")
		eq(t, resolveEditor(lookupOf(map[string]string{"EDITOR": "   ", "PATH": dir, "VISUAL": ""}), "linux"), expected)
	})
	t.Run("prefers `editor` over nano over vi on $PATH", func(t *testing.T) {
		dir := t.TempDir()
		exe(t, dir, "nano")
		exe(t, dir, "vi")
		expected := exe(t, dir, "editor")
		eq(t, resolveEditor(lookupOf(map[string]string{"PATH": dir}), "linux"), expected)
	})
	t.Run("falls back to nano before vi when both exist", func(t *testing.T) {
		dir := t.TempDir()
		exe(t, dir, "vi")
		expected := exe(t, dir, "nano")
		eq(t, resolveEditor(lookupOf(map[string]string{"PATH": dir}), "linux"), expected)
	})
	t.Run(`returns ["vi"] when $PATH is empty`, func(t *testing.T) {
		eq(t, resolveEditor(lookupOf(map[string]string{"PATH": ""}), "linux"), "vi")
	})
	t.Run("walks multi-entry $PATH", func(t *testing.T) {
		a, b := t.TempDir(), t.TempDir()
		expected := exe(t, b, "editor")
		eq(t, resolveEditor(lookupOf(map[string]string{"PATH": a + string(os.PathListSeparator) + b}), "linux"), expected)
	})
	t.Run("uses notepad.exe on Windows when no env override", func(t *testing.T) {
		eq(t, resolveEditor(lookupOf(map[string]string{"PATH": t.TempDir()}), "windows"), "notepad.exe")
	})
}
