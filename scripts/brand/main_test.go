package main

import (
	"bytes"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func sourceRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := findRoot(wd)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// Glyphs of every row without markup
func plainLines(rows [][]cell) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		var b strings.Builder
		for _, c := range row {
			b.WriteRune(c.glyph)
		}
		out[i] = b.String()
	}
	return out
}

// Source art of the checkout the tests run in
func testArt(t *testing.T) *art {
	t.Helper()
	a, err := buildArt(sourceRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestWriteStatusIncludesEveryTarget(t *testing.T) {
	good := []byte("package app\n")
	textShape := shape{format: "TEXT"}
	for _, failure := range []string{"", "render", "directory", "verify"} {
		out := t.TempDir()
		first := "first.go"
		if failure == "directory" {
			first = "blocked/first.go"
			os.WriteFile(filepath.Join(out, "blocked"), []byte("not a directory"), 0o644)
		}
		targets := []target{
			{first, func(*art) ([]byte, error) {
				switch failure {
				case "render":
					return nil, io.ErrUnexpectedEOF
				case "verify":
					return []byte{}, nil
				}
				return good, nil
			}, textShape},
			{"last.go", func(*art) ([]byte, error) { return good, nil }, textShape},
		}
		var log bytes.Buffer
		code := cmdWrite(&log, targets, nil, out)
		if (code != 0) != (failure != "") {
			t.Errorf("%q: exit %d", failure, code)
		}
		if data, _ := os.ReadFile(filepath.Join(out, "last.go")); !bytes.Equal(data, good) {
			t.Errorf("%q: the target after a failure was not written", failure)
		}
		if !strings.Contains(log.String(), "last.go: 12 bytes TEXT") || strings.Contains(log.String(), "FAILED") != (failure != "") {
			t.Errorf("%q: report\n%s", failure, log.String())
		}
	}
}

func TestCheckAcceptsWrittenOutputsAndReportsMissingOnes(t *testing.T) {
	a := testArt(t)
	targets := allTargets()
	out := t.TempDir()
	var log bytes.Buffer
	if code := cmdWrite(&log, targets, a, out); code != 0 {
		t.Fatalf("write failed:\n%s", log.String())
	}
	log.Reset()
	if code := cmdCheck(&log, targets, a, out); code != 0 {
		t.Fatalf("check failed:\n%s", log.String())
	}
	banner := filepath.Join(out, "pkg/app/banner_art.go")
	os.WriteFile(banner, []byte("package app\n"), 0o644)
	log.Reset()
	if code := cmdCheck(&log, targets, a, out); code == 0 || !strings.Contains(log.String(), "banner_art.go: STALE") {
		t.Fatalf("check passed a stale file:\n%s", log.String())
	}
	os.Remove(banner)
	log.Reset()
	if code := cmdCheck(&log, targets, a, out); code == 0 || !strings.Contains(log.String(), "banner_art.go: MISSING") {
		t.Fatalf("check passed a missing file:\n%s", log.String())
	}
}

func TestBannerArtKeepsTheShapeTheBannersExpect(t *testing.T) {
	src, err := loadSources(sourceRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	hero, logo := plainLines(src.heroCells()), plainLines(src.logoCells())
	if len(hero) != heroRows || len(logo) != logoRows {
		t.Fatalf("hero %d rows, logo %d rows", len(hero), len(logo))
	}
	for _, line := range hero {
		if utf8.RuneCountInString(line) != heroCols {
			t.Fatalf("hero line %q is not %d cells", line, heroCols)
		}
		for _, r := range line {
			if r < 0x2800 || r > 0x28ff {
				t.Fatalf("hero line %q holds a non braille cell", line)
			}
		}
	}
	width := utf8.RuneCountInString(logo[0])
	if width >= 95 {
		t.Fatalf("logo is %d cells, the banner prints it from 95 columns", width)
	}
	for _, line := range logo {
		if utf8.RuneCountInString(line) != width {
			t.Fatalf("ragged logo line %q", line)
		}
	}
	tags := regexp.MustCompile(`\[#[0-9a-f]{6}\]|\[/\]`)
	for i, line := range src.markupLines(src.heroCells()) {
		if tags.ReplaceAllString(line, "") != hero[i] {
			t.Fatalf("hero markup line %d does not strip to its art", i)
		}
	}
	if !strings.Contains(strings.Join(src.markupLines(src.logoCells()), ""), "["+hexOf(src.red)+"]") {
		t.Fatal("the logo lost its red accent")
	}
	code := src.bannerGo()
	if formatted, err := format.Source(code); err != nil || !bytes.Equal(formatted, code) {
		t.Fatalf("banner Go source is not gofmt clean: %v", err)
	}
}
