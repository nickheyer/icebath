package pm

import (
	"archive/tar"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// Writes an ar deb holding only debian-binary and one data tar, plain or zstd compressed
func dsDeb(t *testing.T, path string, headers []*tar.Header, data map[string][]byte, zstdPayload bool) {
	t.Helper()
	payload, member := makeTarHeaders(t, headers, data, false), "data.tar"
	if zstdPayload {
		enc, err := zstd.NewWriter(nil)
		if err != nil {
			t.Fatal(err)
		}
		payload, member = enc.EncodeAll(payload, nil), "data.tar.zst"
		enc.Close()
	}
	out := []byte("!<arch>\n")
	out = append(out, arMember("debian-binary", []byte("2.0\n"))...)
	out = append(out, arMember(member, payload)...)
	writeFile(t, path, out, 0o666)
}

// Regular member with tarfile's default mode
func dsFile(name string) *tar.Header {
	return &tar.Header{Name: name, Mode: 0o644, Typeflag: tar.TypeReg}
}

// Symlink member with the fixture's 0o777 mode
func dsSymlink(name, linkname string) *tar.Header {
	return &tar.Header{Name: name, Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: linkname}
}

// The fixture deb package the containment tests unpack through
func dsPackage() *DebPackage {
	return &DebPackage{BasePackage: BasePackage{Name: "evil-deb"}}
}

// Fails unless err is an install error whose text matches pattern
func dsWantInstallError(t *testing.T, err error, pattern string) {
	t.Helper()
	var installErr *InstallError
	if !errors.As(err, &installErr) {
		t.Fatalf("expected an install error, got %v", err)
	}
	if pattern != "" && !regexp.MustCompile(pattern).MatchString(err.Error()) {
		t.Fatalf("install error %q does not match %s", err.Error(), pattern)
	}
}

func TestChainedAliasesSurviveWithoutSymlinkSupport(t *testing.T) {
	saved := symlinkFile
	symlinkFile = func(string, string) error { return errors.New("symlinks unavailable") }
	t.Cleanup(func() { symlinkFile = saved })
	tmp := t.TempDir()
	deb := filepath.Join(tmp, "aliases.deb")
	lib := "data/data/com.termux/files/usr/lib"
	dsDeb(t, deb, []*tar.Header{
		dsSymlink(lib+"/libexample.so", "libexample.so.1"),
		dsSymlink(lib+"/libexample.so.1", "libexample.so.1.2"),
		dsFile(lib + "/libexample.so.1.2"),
	}, map[string][]byte{lib + "/libexample.so.1.2": []byte("library payload")}, false)
	staged := filepath.Join(tmp, "staged")
	if err := os.Mkdir(staged, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := dsPackage().Unpack(deb, staged, "linux-arm64-bionic"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"libexample.so", "libexample.so.1", "libexample.so.1.2"} {
		if got := string(readFile(t, filepath.Join(staged, filepath.FromSlash(lib), name))); got != "library payload" {
			t.Fatalf("%s = %q", name, got)
		}
	}
}

func TestRelativeMemberEscapePreservesOutside(t *testing.T) {
	tmp := t.TempDir()
	deb, staged := filepath.Join(tmp, "escape.deb"), filepath.Join(tmp, "staged")
	sentinel := filepath.Join(tmp, "sentinel")
	writeFile(t, sentinel, []byte("outside"), 0o666)
	dsDeb(t, deb, []*tar.Header{dsFile("../sentinel")}, map[string][]byte{"../sentinel": []byte("escaped")}, false)
	if err := os.Mkdir(staged, 0o777); err != nil {
		t.Fatal(err)
	}
	dsWantInstallError(t, dsPackage().Unpack(deb, staged, "linux-arm64-bionic"), "escape|unsafe|outside")
	if string(readFile(t, sentinel)) != "outside" {
		t.Fatal("sentinel was overwritten")
	}
}

func TestZstdPayloadUnpacksUnderTheSameContainment(t *testing.T) {
	tmp := t.TempDir()
	deb, staged := filepath.Join(tmp, "ok.deb"), filepath.Join(tmp, "staged")
	lib := "usr/lib/x86_64-linux-gnu/libgomp.so.1"
	dsDeb(t, deb, []*tar.Header{dsFile(lib)}, map[string][]byte{lib: []byte("omp")}, true)
	if err := os.Mkdir(staged, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := dsPackage().Unpack(deb, staged, "linux-x64"); err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, filepath.Join(staged, filepath.FromSlash(lib)))); got != "omp" {
		t.Fatalf("libgomp = %q", got)
	}
	sentinel := filepath.Join(tmp, "sentinel")
	writeFile(t, sentinel, []byte("outside"), 0o666)
	dsDeb(t, deb, []*tar.Header{dsFile("../sentinel")}, map[string][]byte{"../sentinel": []byte("escaped")}, true)
	dsWantInstallError(t, dsPackage().Unpack(deb, filepath.Join(tmp, "staged2"), "linux-x64"), "escape|unsafe|outside")
	if string(readFile(t, sentinel)) != "outside" {
		t.Fatal("sentinel was overwritten")
	}
}

func TestAbsoluteSymlinkTargetRejected(t *testing.T) {
	tmp := t.TempDir()
	deb := filepath.Join(tmp, "abs-link.deb")
	dsDeb(t, deb, []*tar.Header{dsSymlink("data/data/com.termux/files/usr/bin/tool", "/etc/shadow")}, nil, false)
	staged := filepath.Join(tmp, "staged")
	if err := os.Mkdir(staged, 0o777); err != nil {
		t.Fatal(err)
	}
	dsWantInstallError(t, dsPackage().Unpack(deb, staged, "linux-arm64-bionic"), "")
	if zsLexists(filepath.Join(staged, "data")) {
		t.Fatal("refused member created its parents")
	}
}

func TestSymlinkEscapeForbidden(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platforms posix")
	}
	for _, shape := range []string{"relative-link", "planted-ancestor"} {
		t.Run(shape, func(t *testing.T) {
			tmp := t.TempDir()
			outside := filepath.Join(tmp, "outside")
			sentinel := filepath.Join(outside, "sentinel.txt")
			writeFile(t, sentinel, []byte("do not touch"), 0o666)
			if err := os.Chmod(sentinel, 0o600); err != nil {
				t.Fatal(err)
			}
			staged := filepath.Join(tmp, "staged")
			if err := os.Mkdir(staged, 0o777); err != nil {
				t.Fatal(err)
			}
			headers := []*tar.Header{dsFile("link/evil.txt")}
			if shape == "relative-link" {
				headers = append([]*tar.Header{dsSymlink("link", "../outside")}, headers...)
			} else if err := os.Symlink(outside, filepath.Join(staged, "link")); err != nil {
				t.Fatal(err)
			}
			deb := filepath.Join(tmp, "escape-"+shape+".deb")
			dsDeb(t, deb, headers, map[string][]byte{"link/evil.txt": []byte("escaped")}, false)
			dsWantInstallError(t, dsPackage().Unpack(deb, staged, "linux-arm64-bionic"), "escape|unsafe|outside")
			if string(readFile(t, sentinel)) != "do not touch" {
				t.Fatalf("%s: a staged symlink was followed outside the tree", shape)
			}
			if st, err := os.Stat(sentinel); err != nil || st.Mode().Perm() != 0o600 {
				t.Fatalf("sentinel mode changed: %v", err)
			}
			if zsLexists(filepath.Join(outside, "evil.txt")) {
				t.Fatal("member was written outside the tree")
			}
		})
	}
}
