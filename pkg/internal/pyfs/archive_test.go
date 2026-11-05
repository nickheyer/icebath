package pyfs

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNormalizeArchivePartsRejectsEscapes(t *testing.T) {
	ok := map[string][]string{"a/b": {"a", "b"}, "./a//b/": {"a", "b"}, `a\b`: {"a", "b"}}
	for in, want := range ok {
		got, err := NormalizeArchiveParts(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "/etc/x", "../x", "a/../../x", `C:\x`, "C:x", `\\srv\share\x`, "//srv/x", ".", "./"} {
		var ae *ArchiveError
		if _, err := NormalizeArchiveParts(bad); !errors.As(err, &ae) || ae.Msg != "Unsafe archive member path: "+bad {
			t.Fatalf("%q accepted: %v", bad, err)
		}
	}
}

func TestMakeTargzRoundTripsThroughSafeExtract(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "coder")
	os.MkdirAll(filepath.Join(src, "skills", "x"), 0o755)
	os.WriteFile(filepath.Join(src, "skills", "x", "SKILL.md"), []byte("hi"), 0o640)
	os.WriteFile(filepath.Join(src, "config.yaml"), []byte("a: 1\n"), 0o600)
	out := filepath.Join(t.TempDir(), "coder")
	path, err := MakeTargz(out, root, "coder")
	if err != nil || path != out+".tar.gz" {
		t.Fatalf("%s %v", path, err)
	}
	roots, err := ArchiveRootDirs(path)
	if err != nil || !reflect.DeepEqual(roots, map[string]bool{"coder": true}) {
		t.Fatalf("%v %v", roots, err)
	}
	dest := t.TempDir()
	if err := SafeExtractTargz(path, dest); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dest, "coder", "skills", "x", "SKILL.md"))
	st, _ := os.Stat(filepath.Join(dest, "coder", "config.yaml"))
	if string(data) != "hi" || st.Mode().Perm() != 0o600 {
		t.Fatalf("%q %v", data, st.Mode())
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(out), ".archive_*")); len(leftovers) != 0 {
		t.Fatalf("temp left %v", leftovers)
	}
}

func TestSafeExtractRefusesLinks(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "evil.tar.gz")
	f, _ := os.Create(archive)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "p/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "p/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
	tw.Close()
	gz.Close()
	f.Close()
	var ae *ArchiveError
	if err := SafeExtractTargz(archive, t.TempDir()); !errors.As(err, &ae) || ae.Msg != "Unsupported archive member type: p/link" {
		t.Fatalf("got %v", err)
	}
}

func TestRmTreeRoutesFailuresAndRefusesLinkRoot(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "t")
	os.MkdirAll(filepath.Join(tree, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(tree, "a", "b", "f"), []byte("x"), 0o644)
	link := filepath.Join(root, "link")
	if err := os.Symlink(tree, link); err == nil {
		if err := RmTree(link, nil); !errors.Is(err, ErrRmTreeSymlink) {
			t.Fatalf("link root: %v", err)
		}
	}
	var calls []string
	err := RmTree(filepath.Join(root, "missing"), func(_ RmTreeOp, p string, err error) error {
		calls = append(calls, filepath.Base(p))
		return nil
	})
	if err != nil || !reflect.DeepEqual(calls, []string{"missing", "missing"}) {
		t.Fatalf("missing: %v %v", calls, err)
	}
	if err := RmTree(tree, nil); err != nil || func() bool { _, e := os.Stat(tree); return e == nil }() {
		t.Fatalf("tree survives: %v", err)
	}
}
