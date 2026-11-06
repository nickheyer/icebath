package tools

import "testing"

// Windows spellings match across drive case, separator style and component case
func TestSamePathFoldsWindowsCase(t *testing.T) {
	cases := []struct {
		left, right string
		want        bool
	}{
		{`C:\Users\Dev\icebath\venv`, `c:/users/dev/ICEBATH/venv/`, true},
		{`C:\Users\Dev`, `D:\Users\Dev`, false},
		{`\\server\share\repo`, `//SERVER/share/Repo`, true},
		{`C:\a\b`, `C:\a\b\c`, false},
	}
	for _, c := range cases {
		if got := SamePath(c.left, c.right); got != c.want {
			t.Errorf("SamePath(%q, %q) = %v, want %v", c.left, c.right, got, c.want)
		}
	}
}
