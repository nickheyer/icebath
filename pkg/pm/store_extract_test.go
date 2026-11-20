package pm

import (
	"archive/tar"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/hostinfo"
)

// One tar member, a regular file holding x when link is empty and a symlink otherwise
type sxEntry struct {
	name, link string
}

// Writes a gzipped tar of regular files and symlinks shaped like tarfile's TarInfo defaults
func sxTar(t *testing.T, dir string, entries []sxEntry) string {
	t.Helper()
	var headers []*tar.Header
	data := map[string][]byte{}
	for _, e := range entries {
		if e.link == "" {
			headers = append(headers, &tar.Header{Name: e.name, Mode: 0o644, Typeflag: tar.TypeReg})
			data[e.name] = []byte("x")
			continue
		}
		headers = append(headers, &tar.Header{Name: e.name, Mode: 0o644, Typeflag: tar.TypeSymlink, Linkname: e.link})
	}
	archive := filepath.Join(dir, "pkg.tar.gz")
	writeFile(t, archive, makeTarHeaders(t, headers, data, true), 0o666)
	return archive
}

func TestRelativeSymlinksResolveFromTheirOwnDirectory(t *testing.T) {
	tmp := t.TempDir()
	archive := sxTar(t, tmp, []sxEntry{
		{"python/share/terminfo/a/adm1178", ""}, {"python/share/terminfo/1/1178", "../a/adm1178"},
	})
	dest := filepath.Join(tmp, "out")
	if err := Extract(archive, dest); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dest, "python", "share", "terminfo", "1", "1178")
	if target, err := os.Readlink(link); err != nil || target != "../a/adm1178" {
		t.Fatalf("readlink = %q %v", target, err)
	}
	if realpath(link) != realpath(filepath.Join(dest, "python", "share", "terminfo", "a", "adm1178")) {
		t.Fatal("link does not resolve to its sibling directory")
	}
}

func TestSymlinksEscapingTheDestinationAreRejected(t *testing.T) {
	for _, linkname := range []string{"../../../etc/passwd", "/etc/passwd"} {
		t.Run(linkname, func(t *testing.T) {
			tmp := t.TempDir()
			archive := sxTar(t, tmp, []sxEntry{{"python/bin/evil", linkname}})
			err := Extract(archive, filepath.Join(tmp, "out"))
			var filterErr *TarFilterError
			if !errors.As(err, &filterErr) {
				t.Fatalf("expected a filter error, got %v", err)
			}
			if st, err := os.Lstat(filepath.Join(tmp, "out", "python", "bin", "evil")); err == nil && st.Mode()&os.ModeSymlink != 0 {
				t.Fatal("escaping symlink was created")
			}
		})
	}
}

func TestTargetUsesSharedNativeArch(t *testing.T) {
	if got, want := nativeMachine(), hostinfo.NativeArch(); got != want {
		t.Fatalf("native machine %q, shared native arch %q", got, want)
	}
}
