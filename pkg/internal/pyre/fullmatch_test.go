package pyre

import "testing"

func TestCompileFullmatchAnchorsTheWholeSubject(t *testing.T) {
	cases := []struct {
		pattern, subject string
		want             bool
	}{
		{"terminal|file_write", "terminal", true},
		{"terminal|file_write", "file_write", true},
		{"terminal|file_write", "terminal_x", false},
		{"a|ab", "ab", true},
		{"term", "terminal", false},
		{"(?i)TERM.*", "terminal", true},
		{"x$", "x\n", false},
	}
	for _, c := range cases {
		re, err := CompileFullmatch(c.pattern)
		if err != nil {
			t.Fatalf("CompileFullmatch(%q): %v", c.pattern, err)
		}
		if got, _ := re.MatchString(c.subject); got != c.want {
			t.Fatalf("fullmatch(%q, %q) = %v, want %v", c.pattern, c.subject, got, c.want)
		}
	}
	if _, err := CompileFullmatch("("); err == nil {
		t.Fatal("unbalanced pattern compiled")
	}
}
