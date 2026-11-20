package pypath

import "testing"

func TestPathRelativeTo(t *testing.T) {
	cases := []struct {
		p, root, want string
		ok            bool
	}{
		{"/a/b/c", "/a", "b/c", true},
		{"/a/b", "/a/b", ".", true},
		{"/a//b/./c", "/a/b/", "c", true},
		{"/ab/c", "/a", "", false},
		{"a/b", "/a", "", false},
		{"/a", "/a/b", "", false},
		{"/a/../b", "/a", "../b", true},
	}
	for _, c := range cases {
		got, ok := PathRelativeTo(c.p, c.root)
		if got != c.want || ok != c.ok {
			t.Errorf("PathRelativeTo(%q, %q) = %q, %v want %q, %v", c.p, c.root, got, ok, c.want, c.ok)
		}
	}
}
