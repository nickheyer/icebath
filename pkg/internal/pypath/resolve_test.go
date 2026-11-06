package pypath

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

func TestResolveMatchesPathlib(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix realpath encoding")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ in, want, wantErr string }{
		{in: dir + "/link//x/./", want: dir + "/real/x"},
		{in: dir + "/link/\xed\xb3\xbf", want: dir + "/real/\xff"},
		{in: dir + "/link/a\x00b/c", wantErr: "embedded null byte"},
		{in: "/x\xed\xa0\x80", wantErr: "'utf-8' codec can't encode character '\\ud800' in position 2: surrogates not allowed"},
		{in: dir + "/link/\xed\xa0\x80/y", wantErr: "'utf-8' codec can't encode character '\\ud800' in position " + strconv.Itoa(len(dir)+6) + ": surrogates not allowed"},
	}
	for _, tc := range cases {
		got, err := Resolve(tc.in)
		if tc.wantErr != "" {
			if err == nil || err.Error() != tc.wantErr {
				t.Errorf("Resolve(%q) error = %v, want %q", tc.in, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("Resolve(%q) = %q, %v, want %q", tc.in, got, err, tc.want)
		}
	}
}
