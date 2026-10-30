package vsc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/protocol"
)

func call(name string, args any) protocol.ToolCall {
	raw, _ := json.Marshal(args)
	return protocol.ToolCall{ID: "c1", Name: name, Args: raw}
}

func TestFileChangeWriteCreate(t *testing.T) {
	dir := t.TempDir()
	fc, ok := FileChangeOf(call("Write", map[string]string{"file_path": "new.txt", "content": "hello"}), dir)
	if !ok {
		t.Fatal("write not recognized")
	}
	if fc.OldText != nil {
		t.Fatalf("old text should be nil for a new file, got %q", *fc.OldText)
	}
	if fc.NewText != "hello" || fc.Path != filepath.Join(dir, "new.txt") {
		t.Fatalf("change %+v", fc)
	}
}

func TestFileChangeWriteOverwrite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	os.WriteFile(p, []byte("old"), 0o644)
	fc, ok := FileChangeOf(call("Write", map[string]string{"file_path": "f.txt", "content": "new"}), dir)
	if !ok || fc.OldText == nil || *fc.OldText != "old" || fc.NewText != "new" {
		t.Fatalf("overwrite %+v ok=%v", fc, ok)
	}
}

func TestFileChangeSingleEdit(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("alpha beta gamma"), 0o644)
	fc, ok := FileChangeOf(call("Edit", map[string]any{"file_path": "f.txt", "old_string": "beta", "new_string": "DELTA"}), dir)
	if !ok || fc.OldText == nil || *fc.OldText != "alpha beta gamma" || fc.NewText != "alpha DELTA gamma" {
		t.Fatalf("edit %+v ok=%v", fc, ok)
	}
}

func TestFileChangeReplaceAll(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("a a a"), 0o644)
	fc, ok := FileChangeOf(call("Edit", map[string]any{"file_path": "f.txt", "old_string": "a", "new_string": "b", "replace_all": true}), dir)
	if !ok || fc.NewText != "b b b" {
		t.Fatalf("replace all %+v ok=%v", fc, ok)
	}
}

func TestFileChangeMultiEdit(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("one two three"), 0o644)
	args := map[string]any{"file_path": "f.txt", "edits": []map[string]any{
		{"old_string": "one", "new_string": "1"},
		{"old_string": "three", "new_string": "3"},
	}}
	fc, ok := FileChangeOf(call("Edit", args), dir)
	if !ok || fc.NewText != "1 two 3" {
		t.Fatalf("multi edit %+v ok=%v", fc, ok)
	}
}

func TestFileChangeMissingOldString(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("content"), 0o644)
	if _, ok := FileChangeOf(call("Edit", map[string]any{"file_path": "f.txt", "old_string": "absent", "new_string": "x"}), dir); ok {
		t.Fatal("missing old_string should return false")
	}
}

func TestFileChangeAmbiguousOldString(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x x"), 0o644)
	if _, ok := FileChangeOf(call("Edit", map[string]any{"file_path": "f.txt", "old_string": "x", "new_string": "y"}), dir); ok {
		t.Fatal("ambiguous old_string should return false without replace_all")
	}
}

func TestFileChangeNonFileTool(t *testing.T) {
	if _, ok := FileChangeOf(call("Shell", map[string]string{"command": "ls"}), t.TempDir()); ok {
		t.Fatal("shell is not a file change")
	}
}
