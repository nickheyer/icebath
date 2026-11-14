package pyfs

import (
	"archive/tar"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func writeTarMembers(t *testing.T, path string, headers []*tar.Header, bodies map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := tar.NewWriter(f)
	for _, h := range headers {
		body := bodies[h.Name]
		h.Size = int64(len(body))
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestExtractTarDataKeepsSafeMembersAndFiltersModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes and symlinks")
	}
	dir := t.TempDir()
	archive := filepath.Join(dir, "a.tar")
	mtime := time.Unix(1700000000, 0)
	writeTarMembers(t, archive, []*tar.Header{
		{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": "abc"}},
		{Typeflag: tar.TypeDir, Name: "pkg/", Mode: 0o775, ModTime: mtime},
		{Typeflag: tar.TypeReg, Name: "pkg/run.sh", Mode: 0o4777, ModTime: mtime},
		{Typeflag: tar.TypeReg, Name: "/pkg/data.txt", Mode: 0o444, ModTime: mtime},
		{Typeflag: tar.TypeSymlink, Name: "pkg/link", Linkname: "data.txt", ModTime: mtime},
	}, map[string]string{"pkg/run.sh": "#!/bin/sh\n", "/pkg/data.txt": "data"})
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := ExtractTarData(archive, out); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(out, "pkg", "run.sh"))
	if err != nil || info.Mode().Perm() != 0o755 || info.Mode()&os.ModeSetuid != 0 || !info.ModTime().Equal(mtime) {
		t.Fatalf("run.sh %v %v", info, err)
	}
	if info, err := os.Stat(filepath.Join(out, "pkg", "data.txt")); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("data.txt %v %v", info, err)
	}
	if target, err := os.Readlink(filepath.Join(out, "pkg", "link")); err != nil || target != "data.txt" {
		t.Fatalf("link %q %v", target, err)
	}
	if info, err := os.Stat(filepath.Join(out, "pkg")); err != nil || !info.ModTime().Equal(mtime) {
		t.Fatalf("dir mtime %v %v", info, err)
	}
}

func TestExtractTarDataRefusesUnsafeMembers(t *testing.T) {
	for _, tc := range []struct {
		header *tar.Header
		class  string
	}{
		{&tar.Header{Typeflag: tar.TypeReg, Name: "../escape", Mode: 0o644}, "OutsideDestinationError"},
		{&tar.Header{Typeflag: tar.TypeSymlink, Name: "abs", Linkname: "/etc/passwd"}, "AbsoluteLinkError"},
		{&tar.Header{Typeflag: tar.TypeSymlink, Name: "up", Linkname: "../../x"}, "LinkOutsideDestinationError"},
		{&tar.Header{Typeflag: tar.TypeFifo, Name: "fifo", Mode: 0o644}, "SpecialFileError"},
	} {
		dir := t.TempDir()
		archive := filepath.Join(dir, "a.tar")
		writeTarMembers(t, archive, []*tar.Header{tc.header}, nil)
		out := filepath.Join(dir, "out")
		os.Mkdir(out, 0o777)
		var refused *TarFilterError
		if err := ExtractTarData(archive, out); !errors.As(err, &refused) || refused.Class != tc.class {
			t.Fatalf("%s: %v", tc.header.Name, err)
		}
	}
}
