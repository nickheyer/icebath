package pyfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadTextCharsMatchesTextIOWrapper(t *testing.T) {
	files := map[string]string{
		"crlf":      "\xef\xbb\xbfab\r\ncd\re\r",
		"late_bad":  strings.Repeat("\xc3\xa9", 100) + strings.Repeat("a", 8092) + strings.Repeat("b", 100) + "\xff",
		"early_bad": "#!/bin/sh\nexec icebath -p x\n\xff",
		"tail_ok":   strings.Repeat("a", 8191) + "\xe2\x82\xac" + "z",
		"split_cr":  strings.Repeat("a", 8191) + "\r\nb",
		"truncated": "ab\xe2\x82",
		"badcont":   "ab\xe2\x82a",
		"nosig":     "\xef\xbb\xbfx",
		"bomstart":  "\xef\xbb",
	}
	dir := t.TempDir()
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	type want struct {
		n    int
		tail string
		err  string
	}
	cases := []struct {
		name string
		sig  bool
		size int
		want want
	}{
		{"crlf", true, 5, want{5, "ab\ncd", ""}},
		{"crlf", true, 8192, want{8, "\ncd\ne\n", ""}},
		{"crlf", false, 5, want{5, "\ufeffab\nc", ""}},
		{"crlf", false, 8192, want{9, "\ncd\ne\n", ""}},
		{"late_bad", true, 5, want{5, "ééééé", ""}},
		{"late_bad", true, 8192, want{0, "", "'utf-8' codec can't decode byte 0xff in position 200: invalid start byte"}},
		{"late_bad", false, 8192, want{0, "", "'utf-8' codec can't decode byte 0xff in position 200: invalid start byte"}},
		{"early_bad", true, 5, want{0, "", "'utf-8' codec can't decode byte 0xff in position 28: invalid start byte"}},
		{"early_bad", false, 8192, want{0, "", "'utf-8' codec can't decode byte 0xff in position 28: invalid start byte"}},
		{"tail_ok", true, 5, want{5, "aaaaa", ""}},
		{"tail_ok", true, 8192, want{8192, "aaaaa€", ""}},
		{"split_cr", false, 8192, want{8192, "aaaaa\n", ""}},
		{"truncated", true, 5, want{0, "", "'utf-8' codec can't decode bytes in position 0-1: unexpected end of data"}},
		{"truncated", false, 8192, want{0, "", "'utf-8' codec can't decode bytes in position 0-1: unexpected end of data"}},
		{"badcont", true, 5, want{0, "", "'utf-8' codec can't decode bytes in position 2-3: invalid continuation byte"}},
		{"nosig", true, 5, want{1, "x", ""}},
		{"bomstart", true, 5, want{0, "", ""}},
		{"nosig", false, 8192, want{2, "\ufeffx", ""}},
	}
	for _, c := range cases {
		got, err := ReadTextChars(filepath.Join(dir, c.name), c.size, c.sig)
		if c.want.err != "" {
			if _, ok := err.(*UnicodeDecodeError); !ok || err.Error() != c.want.err {
				t.Errorf("%s sig=%v n=%d: err %v, want %q", c.name, c.sig, c.size, err, c.want.err)
			}
			continue
		}
		runes := []rune(got)
		tail := string(runes[max(0, len(runes)-6):])
		if err != nil || len(runes) != c.want.n || tail != c.want.tail {
			t.Errorf("%s sig=%v n=%d: %d %q %v, want %d %q", c.name, c.sig, c.size, len(runes), tail, err, c.want.n, c.want.tail)
		}
	}
	if _, err := ReadTextChars(filepath.Join(dir, "missing"), 5, true); !os.IsNotExist(err) {
		t.Errorf("missing file %v", err)
	}
}
