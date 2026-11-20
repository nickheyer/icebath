package pyfs

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestTarAdderRecursesSortedFiltersAndKeepsHardLinks(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "skill", ".git"), 0o755)
	os.WriteFile(filepath.Join(src, "skill", "b.md"), []byte("b"), 0o644)
	os.WriteFile(filepath.Join(src, "skill", "a.md"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(src, "skill", ".git", "HEAD"), []byte("x"), 0o644)
	if err := os.Link(filepath.Join(src, "skill", "a.md"), filepath.Join(src, "skill", "c.md")); err != nil {
		t.Skip(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	adder := &TarAdder{W: tw}
	err := adder.Add(filepath.Join(src, "skill"), "skill", true, func(h *tar.Header) *tar.Header {
		if filepath.Base(filepath.Clean(h.Name)) == ".git" {
			return nil
		}
		return h
	})
	if err != nil {
		t.Fatal(err)
	}
	tw.Close()
	names, err := TarMemberNames(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"skill", "skill/a.md", "skill/b.md", "skill/c.md"}
	if len(names) != len(want) {
		t.Fatalf("members %v", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("members %v", names)
		}
	}
	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))
	for {
		h, err := tr.Next()
		if err != nil {
			t.Fatal("c.md missing")
		}
		if h.Name == "skill/c.md" {
			if h.Typeflag != tar.TypeLink || h.Linkname != "skill/a.md" {
				t.Fatalf("hard link stored as %v %q", h.Typeflag, h.Linkname)
			}
			break
		}
	}
	dest := t.TempDir()
	if err := ExtractTarDataReader(bytes.NewReader(buf.Bytes()), dest); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "skill", "c.md")); string(b) != "a" {
		t.Fatalf("extracted link %q", b)
	}
}
