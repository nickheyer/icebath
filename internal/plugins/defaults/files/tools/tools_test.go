package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/internal/config"
	filesconfig "github.com/nickheyer/icebath/internal/plugins/defaults/files/config"
	"github.com/nickheyer/icebath/internal/tools"
)

type bench struct {
	t     *testing.T
	host  *tools.Tools
	env   *tools.Env
	asked []string
	deny  error
}

func newBench(t *testing.T) *bench {
	t.Helper()
	cfg := config.New(t.TempDir())
	b := &bench{t: t, host: tools.New(cfg)}
	filesconfig.Register(cfg)
	Register(&b.host.Registry)
	b.env = &tools.Env{Cwd: t.TempDir(), Config: cfg, Approve: func(_ context.Context, action, reason string) error {
		b.asked = append(b.asked, action+" | "+reason)
		return b.deny
	}}
	return b
}

func (b *bench) run(name, arguments string) string {
	b.t.Helper()
	return b.host.Dispatch(b.t.Context(), &tools.Call{Name: name, RawArgs: arguments, Env: b.env})
}

func TestToolsAreOffered(t *testing.T) {
	b := newBench(t)
	var names []string
	for _, def := range b.host.Tools() {
		names = append(names, def.Name+"@"+def.Toolset)
	}
	if got, want := strings.Join(names, " "), "read_file@file write_file@file patch@file search_files@file"; got != want {
		t.Fatalf("Tools() = %s, want %s", got, want)
	}
	if got := len(b.host.Definitions(b.env)); got != 4 {
		t.Fatalf("Definitions() offered %d tools, want 4", got)
	}
}

func TestWriteReadPatch(t *testing.T) {
	b := newBench(t)
	if got, want := b.run("write_file", `{"path":"sub/notes.txt","content":"one\ntwo\nthree\ntwo\n"}`), "wrote 18 bytes to "+filepath.Join(b.env.Cwd, "sub/notes.txt"); got != want {
		t.Fatalf("write_file = %q, want %q", got, want)
	}
	if got, want := b.run("read_file", `{"path":"sub/notes.txt"}`), "1|one\n2|two\n3|three\n4|two\n"; got != want {
		t.Fatalf("read_file = %q, want %q", got, want)
	}
	if got, want := b.run("read_file", `{"path":"sub/notes.txt","offset":2,"limit":2}`), "2|two\n3|three\n[more lines: continue with offset 4]\n"; got != want {
		t.Fatalf("read_file page = %q, want %q", got, want)
	}
	for arguments, want := range map[string]string{
		`{"path":"sub/notes.txt","old_string":"two","new_string":"2"}`:   `{"error":"old_string matches 2 times: add lines around it or set replace_all"}`,
		`{"path":"sub/notes.txt","old_string":"four","new_string":"4"}`:  `{"error":"old_string was not found in the file"}`,
		`{"path":"sub/notes.txt","old_string":"","new_string":"x"}`:      `{"error":"old_string must not be empty or equal to new_string"}`,
		`{"path":"sub/notes.txt","old_string":"one","new_string":"one"}`: `{"error":"old_string must not be empty or equal to new_string"}`,
		`{"path":"sub/notes.txt","old_string":"three","new_string":"3"}`: "replaced 1 match(es) in " + filepath.Join(b.env.Cwd, "sub/notes.txt"),
		`{"path":"sub/notes.txt","old_string":"one"}`:                    `{"error":"missing argument new_string"}`,
	} {
		if got := b.run("patch", arguments); got != want {
			t.Errorf("patch %s = %s, want %s", arguments, got, want)
		}
	}
	if got := b.run("patch", `{"path":"sub/notes.txt","old_string":"two","new_string":"2","replace_all":true}`); !strings.HasPrefix(got, "replaced 2 match(es)") {
		t.Fatalf("patch replace_all = %q", got)
	}
	if content, _ := os.ReadFile(filepath.Join(b.env.Cwd, "sub/notes.txt")); string(content) != "one\n2\n3\n2\n" {
		t.Fatalf("file content = %q", content)
	}
	if got := b.run("read_file", `{"path":"missing.txt"}`); !tools.Failed(got) || !strings.Contains(got, "no such file") {
		t.Fatalf("read_file of a missing file = %q", got)
	}
	os.WriteFile(filepath.Join(b.env.Cwd, "blob.bin"), []byte("a\x00b\n"), 0o644)
	if got, want := b.run("read_file", `{"path":"blob.bin"}`), `{"error":"binary file"}`; got != want {
		t.Fatalf("read_file of a binary file = %q, want %q", got, want)
	}
	if len(b.asked) != 0 {
		t.Fatalf("writes inside the working directory asked for approval: %v", b.asked)
	}
}

func TestReadStopsAtTheBudget(t *testing.T) {
	b := newBench(t)
	if err := b.env.Config.Set(filesconfig.ReadMaxChars, 20); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	os.WriteFile(filepath.Join(b.env.Cwd, "long.txt"), []byte("aaaaaaaa\nbbbbbbbb\ncccccccc\n"), 0o644)
	if got, want := b.run("read_file", `{"path":"long.txt"}`), "1|aaaaaaaa\n[more lines: continue with offset 2]\n"; got != want {
		t.Fatalf("read_file over the budget = %q, want %q", got, want)
	}
}

func TestWritesOutsideTheWorkingDirectoryAsk(t *testing.T) {
	b := newBench(t)
	outside := filepath.Join(t.TempDir(), "out.txt")
	b.deny = errors.New("denied by the user")
	if got, want := b.run("write_file", `{"path":"`+outside+`","content":"x"}`), `{"error":"denied by the user"}`; got != want {
		t.Fatalf("write_file outside = %q, want %q", got, want)
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a denied write created the file: %v", err)
	}
	b.deny = nil
	if got := b.run("write_file", `{"path":"`+outside+`","content":"x"}`); tools.Failed(got) {
		t.Fatalf("an approved write failed: %q", got)
	}
	if got := b.run("patch", `{"path":"../escape.txt","old_string":"a","new_string":"b"}`); !tools.Failed(got) {
		t.Fatalf("patch of a missing file outside = %q", got)
	}
	want := []string{"write " + outside + " | writes outside the working directory", "write " + outside + " | writes outside the working directory", "write " + filepath.Join(filepath.Dir(b.env.Cwd), "escape.txt") + " | writes outside the working directory"}
	if strings.Join(b.asked, "\n") != strings.Join(want, "\n") {
		t.Fatalf("asked = %v, want %v", b.asked, want)
	}
}

func TestSearch(t *testing.T) {
	b := newBench(t)
	for name, content := range map[string]string{
		"a.go":          "package a\n\nfunc Find() {}\n",
		"nested/b.go":   "package b\n\n// Find is here too\n",
		"nested/c.txt":  "Find in text\n",
		".git/config":   "Find in git\n",
		"nested/d.blob": "Find\x00binary\n",
	} {
		path := filepath.Join(b.env.Cwd, name)
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte(content), 0o644)
	}
	if got, want := b.run("search_files", `{"pattern":"Find"}`), "a.go:3:func Find() {}\nnested/b.go:3:// Find is here too\nnested/c.txt:1:Find in text\n"; got != want {
		t.Fatalf("search_files = %q, want %q", got, want)
	}
	if got, want := b.run("search_files", `{"pattern":"Find","file_glob":"*.go","path":"nested"}`), "nested/b.go:3:// Find is here too\n"; got != want {
		t.Fatalf("search_files with a glob and a path = %q, want %q", got, want)
	}
	if got, want := b.run("search_files", `{"pattern":"*.go","target":"files"}`), "a.go\nnested/b.go\n"; got != want {
		t.Fatalf("search_files for files = %q, want %q", got, want)
	}
	if got, want := b.run("search_files", `{"pattern":"Find","limit":1}`), "a.go:3:func Find() {}\n[more than 1 results: narrow the search or raise limit]\n"; got != want {
		t.Fatalf("search_files over the limit = %q, want %q", got, want)
	}
	if got := b.run("search_files", `{"pattern":"nothing matches this"}`); got != "no matches" {
		t.Fatalf("search_files without a match = %q", got)
	}
	if got := b.run("search_files", `{"pattern":"("}`); !tools.Failed(got) {
		t.Fatalf("search_files with a bad expression = %q", got)
	}
}
