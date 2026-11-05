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
