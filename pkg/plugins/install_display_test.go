package plugins

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestDisplayAfterInstall(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	console := &Console{Out: &out, Width: 40}
	if err := displayAfterInstall(console, dir, "owner/repo", true); err != nil || out.Len() != 0 {
		t.Fatalf("brief without after-install.md prints nothing %q %v", out.String(), err)
	}
	if err := displayAfterInstall(console, "/h/plugins/repo", "owner/repo", false); err != nil {
		t.Fatal(err)
	}
	want := "\n╭──────── ✓ Installed ─────────╮\n│ Plugin installed: owner/repo │\n│ Location: /h/plugins/repo    │\n╰──────────────────────────────╯\n\n"
	if out.String() != want {
		t.Fatalf("%q", out.String())
	}
	out.Reset()
	os.WriteFile(filepath.Join(dir, "after-install.md"), []byte("\xef\xbb\xbf# Done\r\n\r\nRun `x`\r\n"), 0o644)
	if err := displayAfterInstall(console, dir, "owner/repo", true); err != nil {
		t.Fatal(err)
	}
	want = "\n╭──────────────────────────────────────╮\n│                 Done                 │\n│                                      │\n│ Run x                                │\n╰──────────────────────────────────────╯\n\n"
	if out.String() != want {
		t.Fatalf("%q", out.String())
	}
	os.WriteFile(filepath.Join(dir, "after-install.md"), []byte("bad \xff"), 0o644)
	if err := displayAfterInstall(console, dir, "owner/repo", false); err == nil {
		t.Fatal("invalid utf-8 raises")
	}
}
