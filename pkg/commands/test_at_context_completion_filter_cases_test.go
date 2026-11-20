package commands

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Builds the fixture tree and returns the file and folder completions of a context word
func atContextRun(t *testing.T, dir, word string) []string {
	t.Helper()
	for _, f := range []string{"readme.md", ".env"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"src", "docs"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	completer := &SlashCompleter{}
	var texts []string
	for _, c := range completer.contextCompletions(word, 30) {
		if strings.HasPrefix(c.Text, "@file:") || strings.HasPrefix(c.Text, "@folder:") {
			texts = append(texts, c.Text)
		}
	}
	return texts
}

func TestAtFolderOnlyYieldsDirectories(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	dir := t.TempDir()
	t.Chdir(dir)
	texts := atContextRun(t, dir, "@folder:")
	for _, text := range texts {
		if !strings.HasPrefix(text, "@folder:") {
			t.Fatalf("texts %v", texts)
		}
	}
	if !slices.Contains(texts, "@folder:src/") || !slices.Contains(texts, "@folder:docs/") {
		t.Fatalf("texts %v", texts)
	}
	if slices.Contains(texts, "@folder:readme.md") || slices.Contains(texts, "@folder:.env") {
		t.Fatalf("texts %v", texts)
	}
}

func TestAtFileBareWithoutColonListsFiles(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	dir := t.TempDir()
	t.Chdir(dir)
	texts := atContextRun(t, dir, "@file")
	if !slices.Contains(texts, "@file:readme.md") {
		t.Fatalf("texts %v", texts)
	}
	if slices.Contains(texts, "@file:src/") {
		t.Fatalf("texts %v", texts)
	}
}
