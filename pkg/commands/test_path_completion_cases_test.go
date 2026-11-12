package commands

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func completionDisplays(found []Completion) []string {
	out := make([]string, 0, len(found))
	for _, c := range found {
		out = append(out, c.Display)
	}
	return out
}

func touchFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExtractPathWordRelativePath(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if got, ok := extractPathWord("look at ./src/main.py"); !ok || got != "./src/main.py" {
		t.Fatalf("got %q %v", got, ok)
	}
}

func TestPathCompletionsListsCurrentDirectory(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	dir := t.TempDir()
	touchFiles(t, dir, "file_a.py", "file_b.txt")
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	names := completionDisplays(pathCompletions("./", 30))
	for _, want := range []string{"file_a.py", "file_b.txt", "subdir/"} {
		if !slices.Contains(names, want) {
			t.Fatalf("names %v missing %s", names, want)
		}
	}
}

func TestPathCompletionsDirectoriesHaveTrailingSlash(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "mydir"), 0o755); err != nil {
		t.Fatal(err)
	}
	touchFiles(t, dir, "myfile.txt")
	found := pathCompletions(dir+"/", 30)
	names := completionDisplays(found)
	idx := slices.Index(names, "mydir/")
	if idx < 0 {
		t.Fatalf("names %v", names)
	}
	if found[idx].Meta != "dir" {
		t.Fatalf("meta %q", found[idx].Meta)
	}
}

func TestIntegrationSlashCommandsStillWork(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	names := completionDisplays(NewCompleter(CompleterSources{}).Complete("/hel"))
	if !slices.Contains(names, "/help") {
		t.Fatalf("names %v", names)
	}
}

func TestIntegrationPathCompletionTriggersOnDotSlash(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	dir := t.TempDir()
	touchFiles(t, dir, "test.py")
	t.Chdir(dir)
	names := completionDisplays(NewCompleter(CompleterSources{}).Complete("edit ./te"))
	if !slices.Contains(names, "test.py") {
		t.Fatalf("names %v", names)
	}
}

func TestIntegrationUrlDoesNotTouchFilesystem(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	old := listDir
	t.Cleanup(func() { listDir = old })
	listDir = func(string) ([]os.DirEntry, error) {
		t.Fatal("os.listdir must not run for a URL token")
		return nil, nil
	}
	if got := NewCompleter(CompleterSources{}).Complete("open https://paste.rs/abc"); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestFileSizeLabelNonexistent(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if got := fileSizeLabel("/nonexistent_xyz"); got != "" {
		t.Fatalf("got %q", got)
	}
}
