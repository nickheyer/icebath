package pydatetime

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestZoneInfoLoadsKeysAndRejectsUnnormalized(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX TZPATH layout")
	}
	dir := t.TempDir()
	data, err := os.ReadFile("/usr/share/zoneinfo/Asia/Kolkata")
	if err != nil {
		t.Skip("system zoneinfo unavailable")
	}
	if err := os.MkdirAll(filepath.Join(dir, "Asia"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Asia", "Kolkata"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "zone.tab"), []byte("# not tzif\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PYTHONTZPATH", "relative/dir"+string(os.PathListSeparator)+dir)
	loc, err := ZoneInfo("Asia/Kolkata")
	if err != nil || loc.String() != "Asia/Kolkata" {
		t.Fatalf("ZoneInfo = %v, %v", loc, err)
	}
	if _, off := time.Date(2026, 7, 14, 0, 0, 0, 0, loc).Zone(); off != 19800 {
		t.Fatalf("offset = %d", off)
	}
	cases := map[string]string{
		"/etc/localtime": "ZoneInfo keys may not be absolute paths, got: /etc/localtime",
		"./Asia/Kolkata": "ZoneInfo keys must be normalized relative paths, got: ./Asia/Kolkata",
		"Asia/Kolkata/":  "ZoneInfo keys must be normalized relative paths, got: Asia/Kolkata/",
		"..":             "ZoneInfo keys must refer to subdirectories of TZPATH, got: ..",
		"Local":          "'No time zone found with key Local'",
		"Asia":           "'No time zone found with key Asia'",
		"Mars/Olympus":   "'No time zone found with key Mars/Olympus'",
		"zone.tab":       "Invalid TZif file: magic not found",
		"Bad'Quote/Zone": `"No time zone found with key Bad'Quote/Zone"`,
		"":               "ZoneInfo keys must be normalized relative paths, got: ",
	}
	for key, want := range cases {
		if _, err := ZoneInfo(key); err == nil || err.Error() != want {
			t.Errorf("ZoneInfo(%q) error = %v, want %q", key, err, want)
		}
	}
	var nf *ZoneInfoNotFoundError
	if _, err := ZoneInfo("Mars/Olympus"); !errors.As(err, &nf) || nf.Key != "Mars/Olympus" {
		t.Fatalf("not found error = %#v", err)
	}
	t.Setenv("PYTHONTZPATH", "")
	if _, err := ZoneInfo("Asia/Kolkata"); !errors.As(err, &nf) {
		t.Fatalf("empty PYTHONTZPATH error = %v", err)
	}
}
