package pyplatform

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLibcVer(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "binary")
	for _, c := range []struct{ data, lib, ver string }{
		{"__libc_init", "libc", ""},
		{"GLIBC_2.9", "glibc", "2.9"},
		{"libc.so.1.2.5", "libc", "1.2.5"},
		{"libc_pthread.so.1.2.5", "libc", "1.2.5_pthread"},
		{"/aports/main/musl/src/musl-1.2.5", "musl", "1.2.5"},
		{"/aports/main/musl/src/musl-12.5", "musl", "12.5"},
		{"/aports/main/musl/src/musl-1.2.5.7", "musl", "1.2.5.7"},
		{"libc.musl.so.1", "musl", "1"},
		{"libc.musl-x86_64.so.1.2.5", "musl", "1.2.5"},
		{"ld-musl.so.1", "musl", "1"},
		{"ld-musl-x86_64.so.1.2.5", "musl", "1.2.5"},
		{"", "", ""},
	} {
		if err := os.WriteFile(filename, []byte("[xxx"+c.data+"xxx]"), 0o644); err != nil {
			t.Fatal(err)
		}
		if lib, ver := libcVer(filename); lib != c.lib || ver != c.ver {
			t.Errorf("%q: got (%q, %q), want (%q, %q)", c.data, lib, ver, c.lib, c.ver)
		}
	}
	const chunkSize = 200
	for _, c := range []struct{ data, lib, ver string }{
		{"GLIBC_1.23.4\x00GLIBC_1.9\x00GLIBC_1.21\x00", "glibc", "1.23.4"},
		{"libc.so.2.4\x00libc.so.9\x00libc.so.23.1\x00", "libc", "23.1"},
		{"musl-1.4.1\x00musl-2.1.1\x00musl-2.0.1\x00", "musl", "2.1.1"},
		{"libc.musl-x86_64.so.1.4.1\x00libc.musl-x86_64.so.2.1.1\x00libc.musl-x86_64.so.2.0.1", "musl", "2.1.1"},
		{"no match here, so defaults are used", "", ""},
	} {
		if err := os.WriteFile(filename, []byte(strings.Repeat("x", chunkSize-10)+c.data), 0o644); err != nil {
			t.Fatal(err)
		}
		if lib, ver := libcVerChunked(filename, chunkSize); lib != c.lib || ver != c.ver {
			t.Errorf("%q: got (%q, %q), want (%q, %q)", c.data, lib, ver, c.lib, c.ver)
		}
	}
}

func TestPlatformJoinsTheUnameFields(t *testing.T) {
	if got := joinPlatform("Linux", "6.8.0 generic", "x86_64", "", "with", ""); got != "Linux-6.8.0_generic-x86_64-with" {
		t.Fatalf("joined %q", got)
	}
	if got := joinPlatform("A", "unknown", "b/c:d", "(e)"); got != "A-b-c-d-e" {
		t.Fatalf("joined %q", got)
	}
	label := Platform()
	if runtime.GOOS == "linux" && !strings.HasPrefix(label, "Linux-"+strings.ReplaceAll(Release(), " ", "_")+"-"+Machine()) {
		t.Fatalf("label %q", label)
	}
	if Version() == "" || Node() == "" {
		t.Fatalf("version %q node %q", Version(), Node())
	}
}
