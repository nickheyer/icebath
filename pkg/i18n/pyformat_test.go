package i18n

import (
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

func TestPyFormatMatchesPython(t *testing.T) {
	kw := map[string]any{
		"name": "ab", "n": 1234, "neg": -42, "f": 1.5, "big": 1e16, "tiny": 0.00001, "one": 1.0,
		"b": true, "none": nil, "list": []any{"x", 2}, "m": map[string]any{"k": "v"}, "w": 6, "q": "it's",
	}
	cases := map[string]string{
		"{name}":       "ab",
		"{{x}} {name}": "{x} ab",
		"{name:>5}":    "   ab",
		"{name:^6}":    "  ab  ",
		"{name:*<5}":   "ab***",
		"{name:05}":    "ab000",
		"{name:.1}":    "a",
		"{name!r}":     "'ab'",
		"{q!r}":        `"it's"`,
		"{n}":          "1234",
		"{n:,}":        "1,234",
		"{n:_}":        "1_234",
		"{n:8}":        "    1234",
		"{n:<8}|":      "1234    |",
		"{n:08}":       "00001234",
		"{n:010,}":     "00,001,234",
		"{neg:+}":      "-42",
		"{n:+}":        "+1234",
		"{neg:=8}":     "-     42",
		"{n:x}":        "4d2",
		"{n:#X}":       "0X4D2",
		"{n:#b}":       "0b10011010010",
		"{n:.2f}":      "1234.00",
		"{f}":          "1.5",
		"{one}":        "1.0",
		"{big}":        "1e+16",
		"{tiny}":       "1e-05",
		"{f:.0f}":      "2",
		"{f:.3}":       "1.5",
		"{one:.3}":     "1.0",
		"{f:.2%}":      "150.00%",
		"{f:e}":        "1.500000e+00",
		"{f:g}":        "1.5",
		"{big:g}":      "1e+16",
		"{n:.3g}":      "1.23e+03",
		"{b}":          "True",
		"{b:d}":        "1",
		"{none}":       "None",
		"{list}":       "['x', 2]",
		"{list[0]}":    "x",
		"{m[k]}":       "v",
		"{name:>{w}}":  "    ab",
		"{verb:9}|":    "",
		"{name!s:3}|":  "ab |",
		"{list[1]:03}": "002",
		"{n:c}":        "Ӓ",
		"{m}":          "{'k': 'v'}",
		"{name:s}":     "ab",
		"{neg:,}":      "-42",
		"{tiny:.2e}":   "1.00e-05",
		"{big:,.0f}":   "10,000,000,000,000,000",
		"{one:#.3g}":   "1.00",
		"{f:10.2f}|":   "      1.50|",
		"{neg: }":      "-42",
		"{n: }":        " 1234",
		"{name!a}":     "'ab'",
		"{f:+.1f}":     "+1.5",
		"{tiny:%}":     "0.001000%",
		"{big:.2}":     "1e+16",
		"{one:g}":      "1",
		"{n:n}":        "1234",
		"{neg:x}":      "-2a",
		"{f:<6}|":      "1.5   |",
		"{name:<}":     "ab",
		"{list[1]}":    "2",
		"{one:.0%}":    "100%",
		"{w:^5}|":      "  6  |",
		"{f:=+7.2f}":   "+  1.50",
		"{n:_x}":       "4d2",
		"{neg:08.1f}":  "-00042.0",
		"{tiny:.1}":    "1e-05",
		"{n:e}":        "1.234000e+03",
		"{b:>5}":       "    1",
		"{name:·^6}":   "··ab··",
	}
	kw["verb"] = "run"
	cases["{verb:9}|"] = "run      |"
	for tmpl, want := range cases {
		got, err := PyFormat(tmpl, kw)
		if err != nil || got != want {
			t.Errorf("%q got %q err %v want %q", tmpl, got, err, want)
		}
	}
}

func TestPyFormatErrorClasses(t *testing.T) {
	kw := map[string]any{"s": "x", "n": 3, "none": nil, "list": []any{1}}
	cases := map[string]string{
		"{missing}":  "KeyError",
		"{}":         "IndexError",
		"{0}":        "IndexError",
		"}":          "ValueError",
		"{s":         "ValueError",
		"{s:d}":      "ValueError",
		"{s!x}":      "ValueError",
		"{n:.2d}":    "ValueError",
		"{s:+}":      "ValueError",
		"{none:5}":   "TypeError",
		"{n[0]}":     "TypeError",
		"{n.attr}":   "AttributeError",
		"{list[5]}":  "IndexError",
		"{list[a]}":  "TypeError",
		"{s:,}":      "ValueError",
		"{n:,_}":     "ValueError",
		"{s:=5}":     "ValueError",
		"{s!r:>{x}}": "KeyError",
	}
	for tmpl, class := range cases {
		_, err := PyFormat(tmpl, kw)
		var fe *FormatError
		if !errors.As(err, &fe) || fe.Class != class {
			t.Errorf("%q err %v want %s", tmpl, err, class)
		}
	}
}

func TestPyFloatRepr(t *testing.T) {
	cases := map[float64]string{0.1: "0.1", 1e22: "1e+22", 123456789012345.0: "123456789012345.0", 1234567890123456.0: "1234567890123456.0",
		12345678901234567.0: "1.2345678901234568e+16", 0.0001: "0.0001", math.Inf(1): "inf", 2.5e-7: "2.5e-07"}
	for f, want := range cases {
		if got := pyrepr.Float(f); got != want {
			t.Errorf("%v got %q want %q", f, got, want)
		}
	}
}

func TestYAML11Resolution(t *testing.T) {
	flat, err := ParseLocaleYAML("x", []byte("on: a\noff: [1]\n2: b\n0x10: c\nnull: d\nplain: e\nflag: off\nquoted: 'off'\n2001-12-14: f\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"True": "a", "2": "b", "16": "c", "None": "d", "plain": "e", "quoted": "off", "2001-12-14": "f"}
	if !reflect.DeepEqual(flat, want) {
		t.Fatalf("got %v", flat)
	}
	doc, _ := loadYAML11([]byte("a: 1\nb: [x, true]\n"))
	if got := NonTextLeaves(doc, ""); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("non text leaves %v", got)
	}
	if got := NonTextLeaves([]any{1}, ""); !reflect.DeepEqual(got, []string{"<root>"}) {
		t.Fatalf("root leaf %v", got)
	}
	if _, err := ParseLocaleYAML("x", []byte("a: 1\na: 2\n")); err == nil {
		t.Fatal("duplicate key accepted")
	}
	if _, err := ParseLocaleYAML("x", []byte("- a\n- b\n")); err == nil {
		t.Fatal("sequence document accepted")
	}
	if got, err := ParseLocaleYAML("x", []byte("\xef\xbb\xbf# only a comment\n")); err != nil || len(got) != 0 {
		t.Fatalf("empty doc %v %v", got, err)
	}
	merged, err := ParseLocaleYAML("x", []byte("base: &b\n  x: one\n  q: two\nchild:\n  <<: *b\n  q: own\n"))
	if err != nil || merged["child.x"] != "one" || merged["child.q"] != "own" || pyrepr.Float(math.Copysign(0, -1)) != "-0.0" {
		t.Fatalf("merge %v %v", merged, err)
	}
	stamps := map[string]string{
		"2001-12-14t21:59:43.10-05:00": "datetime.datetime(2001, 12, 14, 21, 59, 43, 100000, tzinfo=datetime.timezone(datetime.timedelta(days=-1, seconds=68400)))",
		"2001-12-14 21:00:00 -5":       "datetime.datetime(2001, 12, 14, 21, 0, tzinfo=datetime.timezone(datetime.timedelta(days=-1, seconds=68400)))",
		"2001-12-14 21:00:05+05:30":    "datetime.datetime(2001, 12, 14, 21, 0, 5, tzinfo=datetime.timezone(datetime.timedelta(seconds=19800)))",
		"2001-12-14 21:00:05":          "datetime.datetime(2001, 12, 14, 21, 0, 5)",
		"2001-12-14":                   "datetime.date(2001, 12, 14)",
	}
	for text, want := range stamps {
		v, err := parseYAML11Timestamp(text)
		if got := pyrepr.Repr(pyMap{{Key: "k", Value: v}}); err != nil || got != "{'k': "+want+"}" {
			t.Errorf("timestamp %q repr %s %v", text, got, err)
		}
	}
}

func TestLanguageIDHelpers(t *testing.T) {
	if NormalizeLanguageID("  PT_BR ") != "pt-br" || NormalizeLanguageID(5) != "" {
		t.Fatal("normalize")
	}
	for id, ok := range map[string]bool{"en": true, "zh-hant": true, "sr-latn-rs": true, "not a lang": false, "e": false} {
		if IsLanguageID(id) != ok {
			t.Fatalf("%s", id)
		}
	}
	dir := t.TempDir()
	for _, name := range []string{"pl.yaml", "pl.tui.yaml", "pl.tiu.yaml", "README.md", "de.desktop.yml"} {
		writeFile(t, dir+"/"+name, "a: b\n")
	}
	var got [][2]string
	for _, f := range ScanLocaleDir(dir) {
		got = append(got, [2]string{f.Lang, f.Surface})
	}
	if !reflect.DeepEqual(got, [][2]string{{"de", "desktop"}, {"pl", "tui"}, {"pl", "core"}}) {
		t.Fatalf("scan %v", got)
	}
	if len(ScanLocaleDir(dir+"/missing")) != 0 {
		t.Fatal("missing dir")
	}
}
