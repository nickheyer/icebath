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
	if !FnmatchCaseBytes([]byte{0xe9}, []byte("[\xe0-\xff]")) {
		t.Errorf("latin-1 bytes range")
	}
}

// Translate text equals CPython 3.14 fnmatch.translate
func TestTranslateMatchesCPython(t *testing.T) {
	cases := map[string]string{
		"*.txt":   `(?s:.*\.txt)\z`,
		"a*b*c":   `(?s:a(?>.*?b).*c)\z`,
		"**":      `(?s:.*)\z`,
		"[!a-c]":  `(?s:[^a-c])\z`,
		"[z-a]":   `(?s:(?!))\z`,
		"[!]":     `(?s:\[!\])\z`,
		"[]]":     `(?s:[]])\z`,
		"[a&&b]":  `(?s:[a\&\&b])\z`,
		"# x?":    `(?s:\#\ x.)\z`,
		"[^x]":    `(?s:[\^x])\z`,
		"[a-c-e]": `(?s:[a-c\-e])\z`,
	}
	for pat, want := range cases {
		if got := Translate(pat); got != want {
			t.Errorf("Translate(%q) = %s, want %s", pat, got, want)
		}
	}
}

// Filter keeps input order and filterfalse returns the complement
func TestFilter(t *testing.T) {
	names := []string{"a.py", "b.txt", "c.py"}
	if got := Filter(names, "*.py"); len(got) != 2 || got[0] != "a.py" || got[1] != "c.py" {
		t.Errorf("filter %v", got)
	}
	if got := FilterFalse(names, "*.py"); len(got) != 1 || got[0] != "b.txt" {
		t.Errorf("filterfalse %v", got)
	}
}
