package tarzst

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
)

// Small tree with nested, hidden, long named, executable and shebang files
func sampleTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	long := strings.Repeat("d", 120)
	for rel, body := range map[string]string{
		"a.txt":                         "alpha",
		"skills/x/SKILL.md":             "# x\n",
		"skills/x/scripts/run.sh":       "#!/bin/sh\necho hi\n",
		"skills/x/scripts/tool.py":      "#!/usr/bin/env python3\nprint(1)\n",
		"skills/.hidden/_private/f":     "",
		long + "/" + long + "/leaf.txt": "deep",
	} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(root, "skills", "x", "scripts", "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestEncodeIsDeterministicAndRoundTrips(t *testing.T) {
	root := sampleTree(t)
	files, err := Collect(root, false)
	if err != nil {
		t.Fatal(err)
	}
	first, err := Encode(files)
	if err != nil {
		t.Fatal(err)
	}
	reversed := append([]File(nil), files...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	second, err := Encode(reversed)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("encoding depends on input order: %v", err)
	}
	decoded, err := Decode(first)
	if err != nil || !reflect.DeepEqual(decoded, files) {
		t.Fatalf("round trip differs: %v", err)
	}
	modes := map[string]fs.FileMode{}
	for _, f := range decoded {
		modes[f.Path] = f.Mode
	}
	if runtime.GOOS != "windows" && (modes["skills/x/scripts/run.sh"] != 0o755 || modes["skills/x/scripts/tool.py"] != 0o644) {
		t.Fatalf("modes %v", modes)
	}
	shebang, err := Collect(root, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range shebang {
		if f.Path == "skills/x/scripts/tool.py" && f.Mode != 0o755 {
			t.Fatalf("a shebang script packs as %v", f.Mode)
		}
	}
}

func TestManifestParsesBack(t *testing.T) {
	files, err := Collect(sampleTree(t), false)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseManifest(Manifest(files))
	if err != nil || len(parsed) != len(files) {
		t.Fatalf("parsed %d of %d: %v", len(parsed), len(files), err)
	}
	for i := range files {
		if parsed[i].Path != files[i].Path || parsed[i].Sum != files[i].Sum || parsed[i].Mode != files[i].Mode {
			t.Fatalf("row %d %+v != %+v", i, parsed[i], files[i])
		}
	}
}

func TestFSBehavesLikeEmbedFS(t *testing.T) {
	files, err := Collect(sampleTree(t), false)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := Encode(files)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Path)
	}
	if err := fstest.TestFS(NewFS(archive, ""), names...); err != nil {
		t.Fatal(err)
	}
	rooted := NewFS(archive, "payload")
	var prefixed []string
	for _, n := range names {
		prefixed = append(prefixed, "payload/"+n)
	}
	if err := fstest.TestFS(rooted, prefixed...); err != nil {
		t.Fatal(err)
	}
	st, err := fs.Stat(rooted, "payload/skills/x/scripts/run.sh")
	if err != nil || st.Mode() != 0o444 || !st.ModTime().IsZero() {
		t.Fatalf("file info %v %v", st, err)
	}
	if st, err := fs.Stat(rooted, "payload/skills"); err != nil || st.Mode() != fs.ModeDir|0o555 {
		t.Fatalf("dir info %v %v", st, err)
	}
	if _, err := rooted.Open("payload/missing"); !errorsIs(err, fs.ErrNotExist) {
		t.Fatalf("missing path gave %v", err)
	}
	if _, err := rooted.Open("/payload"); !errorsIs(err, fs.ErrInvalid) {
		t.Fatalf("invalid path gave %v", err)
	}
	if _, err := NewFS([]byte("not zstd"), "").Open("."); err == nil {
		t.Fatal("a corrupt archive must fail to open")
	}
}

func TestExtractWritesModes(t *testing.T) {
	files, err := Collect(sampleTree(t), false)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := Encode(files)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := Extract(archive, dir); err != nil {
		t.Fatal(err)
	}
	again, err := Collect(dir, false)
	if err != nil || !reflect.DeepEqual(again, files) {
		t.Fatalf("extracted tree differs: %v", err)
	}
}

func errorsIs(err, target error) bool {
	pe, ok := err.(*fs.PathError)
	return ok && pe.Err == target
}
