package pypath

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPosixAndNtMatchCPython(t *testing.T) {
	str := map[string]string{
		PosixNormpath("//a/../b/./c/"): "//b/c", PosixNormpath("///a/.."): "/", PosixNormpath("../x/.."): "..",
		PosixNormpath(""): ".", PosixJoin("a", "/b", "c"): "/b/c", PosixJoin("", "x"): "x", PosixDirname("a//b"): "a",
		PosixBasename("a/b/"): "", NtNormpath("C:/a/../../b"): `C:\b`, NtNormpath(`\\srv\sh\..\x`): `\\srv\sh\x`,
		NtJoin("C:a", "D:b"): "D:b", NtJoin(`C:\`, "x"): `C:\x`, NtJoin("c:x", "C:y"): `C:x\y`, NtNormpath(`..\a`): `..\a`,
		NtBasename(`C:\a\b`): "b", NtDirname(`C:\a\b\`): `C:\a\b`,
	}
	for got, want := range str {
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
	pairs := [][4]string{}
	add := func(a, b, wa, wb string) { pairs = append(pairs, [4]string{a, b, wa, wb}) }
	h, tl := PosixSplit("//x")
	add(h, tl, "//", "x")
	h, tl = PosixSplit("a//b//c")
	add(h, tl, "a//b", "c")
	h, tl = PosixSplitext("a/.b.c")
	add(h, tl, "a/.b", ".c")
	h, tl = PosixSplitext("..x")
	add(h, tl, "..x", "")
	h, tl = PosixSplitext("a.b/c")
	add(h, tl, "a.b/c", "")
	h, tl = NtSplit(`C:\a\b\`)
	add(h, tl, `C:\a\b`, "")
	h, tl = NtSplitext(`c:\x.y\z`)
	add(h, tl, `c:\x.y\z`, "")
	for _, p := range pairs {
		if p[0] != p[2] || p[1] != p[3] {
			t.Errorf("got %q %q want %q %q", p[0], p[1], p[2], p[3])
		}
	}
	if d, r, rest := NtSplitRoot("//?/UNC/s/h/x"); d != "//?/UNC/s/h" || r != "/" || rest != "x" {
		t.Errorf("splitroot %q %q %q", d, r, rest)
	}
	if NtIsAbs("C:foo") || NtIsAbs("/x") || !NtIsAbs("//srv") || !NtIsAbs("C:/x") {
		t.Error("ntpath isabs")
	}
}

func TestRealpathMatchesCPython(t *testing.T) {
	dir := t.TempDir()
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Mkdir(filepath.Join(root, "d"), 0o755))
	must(os.WriteFile(filepath.Join(root, "d", "f"), nil, 0o644))
	must(os.Symlink("d", filepath.Join(root, "a")))
	must(os.Symlink("loop2", filepath.Join(root, "loop1")))
	must(os.Symlink("loop1", filepath.Join(root, "loop2")))
	must(os.Symlink("../missing", filepath.Join(root, "d", "m")))
	must(os.Symlink(filepath.Join(root, "d"), filepath.Join(root, "abs")))
	t.Chdir(root)
	cases := map[string][3]string{
		"a/f":      {"R/d/f", "R/d/f", "R/d/f"},
		"a/../x":   {"R/x", "FileNotFoundError:[Errno 2] No such file or directory: 'R/x'", "R/x"},
		"loop1/x":  {"R/loop1/x", "OSError:[Errno 40] Too many levels of symbolic links: 'R/loop1'", "OSError:[Errno 40] Too many levels of symbolic links: 'R/loop1'"},
		"d/m/z":    {"R/missing/z", "FileNotFoundError:[Errno 2] No such file or directory: 'R/missing'", "R/missing/z"},
		"abs/f/..": {"R/d", "NotADirectoryError:[Errno 20] Not a directory: 'R/d/f'", "NotADirectoryError:[Errno 20] Not a directory: 'R/d/f'"},
		"a/f/g":    {"R/d/f/g", "NotADirectoryError:[Errno 20] Not a directory: 'R/d/f'", "NotADirectoryError:[Errno 20] Not a directory: 'R/d/f'"},
	}
	for in, want := range cases {
		for i, mode := range []Strict{Lax, StrictAll, AllowMissing} {
			got, err := Realpath(in, mode)
			if err != nil {
				got = OSErrorClass(err) + ":" + OSErrorText(err)
			}
			if got = strings.ReplaceAll(got, root, "R"); got != want[i] {
				t.Errorf("realpath(%q, %d) = %q want %q", in, mode, got, want[i])
			}
		}
	}
	if got, _ := Abspath("x/../y"); got != root+"/y" {
		t.Errorf("abspath %q", got)
	}
	gone := filepath.Join(root, "gone")
	must(os.Mkdir(gone, 0o755))
	t.Chdir(gone)
	must(os.Remove(gone))
	for name, call := range map[string]func() (string, error){
		"abspath":  func() (string, error) { return Abspath("x") },
		"realpath": func() (string, error) { return Realpath("x", Lax) },
		"getcwd":   Getcwd,
	} {
		_, err := call()
		if got := OSErrorClass(err) + ":" + OSErrorText(err); err == nil || got != "FileNotFoundError:[Errno 2] No such file or directory" {
			t.Errorf("%s in a removed cwd = %v", name, err)
		}
	}
	if got, err := Abspath("/a/./b"); err != nil || got != "/a/b" {
		t.Errorf("absolute abspath in a removed cwd = %q, %v", got, err)
	}
}

func TestPathlibAndExpanduser(t *testing.T) {
	str := map[string]string{
		PathStr("//a/./b//"): "//a/b", PathStr("///a"): "/a", PathStr(""): ".", PathName("/"): "", PathName("a/b.c"): "b.c",
		PathSuffix("foo."): ".", PathSuffix("a.tar.gz"): ".gz", PathSuffix(".bashrc"): "", PathSuffix(".."): "",
		PathStem("a.tar.gz"): "a.tar", PathParent("a/b"): "a", PathParent("/"): "/", PathParent("a"): ".",
		PathJoin("/x", "y/", "./z"): "/x/y/z", PathJoin("x", "/y"): "/y",
	}
	for got, want := range str {
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
	if got := PathParents("/a/b"); !reflect.DeepEqual(got, []string{"/a", "/"}) {
		t.Errorf("parents %v", got)
	}
	if got := PathParents("a/b"); !reflect.DeepEqual(got, []string{"a", "."}) {
		t.Errorf("parents %v", got)
	}
	t.Setenv("HOME", "/home/u/")
	for in, want := range map[string]string{"~": "/home/u", "~/x": "/home/u/x", "x~": "x~", "~nosuchuser_zz/x": "~nosuchuser_zz/x"} {
		if got := PosixExpanduser(in); got != want {
			t.Errorf("expanduser(%q) = %q want %q", in, got, want)
		}
	}
	t.Setenv("HOME", "/")
	if got := PosixExpanduser("~"); got != "/" {
		t.Errorf("expanduser root %q", got)
	}
	t.Setenv("USERPROFILE", `C:\Users\me`)
	t.Setenv("USERNAME", "me")
	for in, want := range map[string]string{"~": `C:\Users\me`, `~\x`: `C:\Users\me\x`, `~you\x`: `C:\Users\you\x`} {
		if got := NtExpanduser(in); got != want {
			t.Errorf("nt expanduser(%q) = %q want %q", in, got, want)
		}
	}
	_, err := os.Open("/nonexistent/it's")
	if got := OSErrorText(err); got != `[Errno 2] No such file or directory: "/nonexistent/it's"` || OSErrorClass(err) != "FileNotFoundError" {
		t.Errorf("oserror %q %s", got, OSErrorClass(err))
	}
	_, err = exec.LookPath("icebath-no-such-binary")
	if got := OSErrorText(err); got != "[Errno 2] No such file or directory: 'icebath-no-such-binary'" || OSErrorClass(err) != "FileNotFoundError" {
		t.Errorf("missing executable %q %s", got, OSErrorClass(err))
	}
}
