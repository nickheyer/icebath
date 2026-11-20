package pypath

import "testing"

func TestPosixPathMatchesPurePosixPath(t *testing.T) {
	cases := map[string]string{"": ".", ".": ".", "a//b/": "a/b", "./a/./b": "a/b", "/": "/", "//x": "//x",
		"///x//": "/x", "a/../b": "a/../b", "/tmp/sessions/": "/tmp/sessions"}
	for in, want := range cases {
		if got := PosixPath(in); got != want {
			t.Fatalf("PosixPath(%q) = %q want %q", in, got, want)
		}
	}
}

// Root splits match posixpath.splitroot of CPython 3.14.8
func TestPosixSplitRootMatchesCPython(t *testing.T) {
	cases := []struct{ in, root, rest string }{
		{"", "", ""}, {"a", "", "a"}, {"/a", "/", "a"}, {"//a", "//", "a"}, {"///a", "/", "//a"}, {"////a", "/", "///a"},
		{"a/b", "", "a/b"}, {"/", "/", ""}, {"//", "//", ""}, {"///", "/", "//"},
	}
	for _, c := range cases {
		drive, root, rest := PosixSplitRoot(c.in)
		if drive != "" || root != c.root || rest != c.rest {
			t.Fatalf("PosixSplitRoot(%q) = %q %q %q", c.in, drive, root, rest)
		}
	}
}
