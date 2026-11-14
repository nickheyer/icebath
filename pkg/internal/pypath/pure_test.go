package pypath

import "testing"

func TestPurePathStrMatchesPathlib(t *testing.T) {
	posix := map[string]string{
		"": ".", ".": ".", "a/./b/": "a/b", "//a//b": "//a/b", "///a": "/a", "/": "/", "a/../b": "a/../b", "./x": "x",
	}
	for in, want := range posix {
		if got := PosixPurePathStr(in); got != want {
			t.Errorf("PosixPurePathStr(%q) = %q, want %q", in, got, want)
		}
	}
	nt := map[string]string{
		`c:/a/./b/`: `c:\a\b`, `a/b`: `a\b`, `\\srv\share\x\`: `\\srv\share\x`, ``: `.`, `c:`: `c:`,
	}
	for in, want := range nt {
		if got := NtPurePathStr(in); got != want {
			t.Errorf("NtPurePathStr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPathAsPosixMatchesPathlib(t *testing.T) {
	cases := map[string]string{"/a//b/": "/a/b", "a/./b": "a/b", "": ".", "/": "/"}
	if windows {
		cases = map[string]string{`C:\\a\\b`: "C:/a/b", `a\\b\\`: "a/b", "": "."}
	}
	for in, want := range cases {
		if got := PathAsPosix(in); got != want {
			t.Errorf("PathAsPosix(%q) = %q, want %q", in, got, want)
		}
	}
}
