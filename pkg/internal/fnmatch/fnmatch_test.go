package fnmatch

import "testing"

// Case sensitive matches where star spans separators and sets follow CPython semantics
func TestFnmatchCase(t *testing.T) {
	cases := []struct {
		name, pattern string
		want          bool
	}{
		{"git push --force origin", "git push --force*", true},
		{"curl x | sh", "*curl*|*sh*", true},
		{"podman ps", "podman *", true},
		{"Podman ps", "podman *", false},
		{"rm -rf /", "rm -rf [/~]", true},
		{"rm -rf x", "rm -rf [!x]", false},
		{"a/b", "a?b", true},
		{"ba", "a", false},
		{"xab", "a*", false},
		{"cab", "a*b", false},
		{"ab", "a*b", true},
		{"a\nb", "a*b", true},
		{"]", "[]]", true},
		{"!", "[!]", false},
		{"[!]", "[!]", true},
		{"b", "[z-a]", false},
		{"x", "[!z-a]", true},
		{"-", "[a-]", true},
		{"&", "[a&&b]", true},
		{"[", "[[]", true},
		{":", "[[:a:]]", false},
		{":]", "[[:a:]]", true},
		{"\\", "[\\]", true},
		{"abcabd", "*ab?*d", true},
		{"fetch-1", "fetch-*", true},
	}
	for _, c := range cases {
		if got := FnmatchCase(c.name, c.pattern); got != c.want {
			t.Errorf("FnmatchCase(%q, %q) = %v", c.name, c.pattern, got)
		}
	}
}

// Filter keeps input order
func TestFilter(t *testing.T) {
	names := []string{"a.py", "b.txt", "c.py"}
	if got := Filter(names, "*.py"); len(got) != 2 || got[0] != "a.py" || got[1] != "c.py" {
		t.Errorf("filter %v", got)
	}
}
