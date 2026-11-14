//go:build unix

package pypath

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestGlobMatchesCPython(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "gfix")
	for _, d := range []string{"sub/deep", "LICENSES"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"a.py", "b.txt", ".hidden.py", "LICENSE", "sub/c.py", "sub/deep/d.py", "LICENSES/MIT.txt"} {
		if err := os.WriteFile(filepath.Join(root, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("sub", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		pattern string
		want    []string
	}{
		{"*.py", []string{".hidden.py", "a.py"}},
		{"**/*.py", []string{".hidden.py", "a.py", "sub/c.py", "sub/deep/d.py"}},
		{"**", []string{".", ".hidden.py", "LICENSE", "LICENSES", "LICENSES/MIT.txt", "a.py", "b.txt", "link", "sub", "sub/c.py", "sub/deep", "sub/deep/d.py"}},
		{"sub/", []string{"sub"}},
		{"LICENSE*", []string{"LICENSE", "LICENSES"}},
		{"sub/**", []string{"sub", "sub/c.py", "sub/deep", "sub/deep/d.py"}},
		{"*/", []string{"LICENSES", "link", "sub"}},
		{"**/", []string{".", "LICENSES", "sub", "sub/deep"}},
		{"nope/x", []string{}},
		{"sub/../a.py", []string{"sub/../a.py"}},
		{"[ab].*", []string{"a.py", "b.txt"}},
		{"LICENSES/*.txt", []string{"LICENSES/MIT.txt"}},
		{"link/*.py", []string{"link/c.py"}},
		{"**/deep", []string{"sub/deep"}},
		{"./a.py", []string{"a.py"}},
	}
	rel := func(paths []string, prefix string) []string {
		out := []string{}
		for _, p := range paths {
			if p == strings.TrimSuffix(prefix, "/") {
				out = append(out, ".")
			} else {
				out = append(out, strings.TrimPrefix(p, prefix))
			}
		}
		slices.Sort(out)
		return out
	}
	for _, c := range cases {
		got, err := Glob(root, c.pattern)
		if err != nil {
			t.Fatalf("glob %q: %v", c.pattern, err)
		}
		if r := rel(got, root+"/"); !slices.Equal(r, c.want) {
			t.Errorf("glob %q = %q want %q", c.pattern, r, c.want)
		}
	}
	for pattern, want := range map[string]string{
		"/abs": "NotImplementedError:Non-relative patterns are unsupported",
		"":     "ValueError:Unacceptable pattern: ''",
		".":    "ValueError:Unacceptable pattern: '.'",
	} {
		_, err := Glob(root, pattern)
		pe, ok := err.(*PatternError)
		if !ok || pe.Class+":"+pe.Msg != want {
			t.Errorf("glob %q error %v want %s", pattern, err, want)
		}
	}
	t.Chdir(root)
	for pattern, want := range map[string][]string{
		"**/*.py": {".hidden.py", "a.py", "sub/c.py", "sub/deep/d.py"},
		"**":      {".", ".hidden.py", "LICENSE", "LICENSES", "LICENSES/MIT.txt", "a.py", "b.txt", "link", "sub", "sub/c.py", "sub/deep", "sub/deep/d.py"},
	} {
		got, err := Glob(".", pattern)
		slices.Sort(got)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("glob of dot %q = %q, %v want %q", pattern, got, err, want)
		}
	}
}
