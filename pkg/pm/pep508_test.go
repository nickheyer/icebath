package pm

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Requirement texts and errors packaging 26.3 produces for the same inputs
func TestPep508RequirementsMatchPackaging(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "pep508_requirements.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases [][3]string
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		input, kind, want := c[0], c[1], c[2]
		req, err := ParseRequirement(input)
		var got string
		if err == nil {
			got, err = req.String()
		}
		switch kind {
		case "ok":
			if err != nil || got != want {
				t.Errorf("%q: got %q, %v want %q", input, got, err, want)
			}
		case "invalid":
			var invalid *InvalidRequirementError
			if !errors.As(err, &invalid) || invalid.Message != want {
				t.Errorf("%q: got %v want invalid %q", input, err, want)
			}
		default:
			var py *PyError
			if !errors.As(err, &py) || py.Class != kind || py.Message != want {
				t.Errorf("%q: got %v want %s %q", input, err, kind, want)
			}
		}
	}
}

// Environment requirements are normalized, deduplicated, sorted and refused with the operations message
func TestPep508NormalizedRequirements(t *testing.T) {
	got, err := normalizedRequirements([]string{"b>=1", "A==1.0", "b >= 1", "c; os_name=='nt'"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"A==1.0", "b>=1", `c; os_name == "nt"`}
	if !sameStrings(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
	if _, err := normalizedRequirements(nil); err == nil || err.Error() != "an isolated environment needs a nonempty sequence of requirements" {
		t.Fatalf("empty: %v", err)
	}
	_, err = normalizedRequirements([]string{"ok", "foo bar"})
	var py *PyError
	if !errors.As(err, &py) || py.Class != "ValueError" ||
		py.Message != "invalid environment requirement: Expected semicolon (after name with no version specifier) or end\n    foo bar\n        ^" {
		t.Fatalf("invalid: %#v", err)
	}
}

// Python string literal escapes evaluate as ast literal_eval reads them
func TestPep508StringLiterals(t *testing.T) {
	for text, want := range map[string]string{
		`'a\tb'`: "a\tb", `"\101\x42C\U00000044"`: "ABCD", `'\N{HANGUL SYLLABLE GAG}'`: "각",
		`'\N{cjk unified ideograph-4e00}'`: "一", `'\N{TANGUT IDEOGRAPH-17000}'`: "\U00017000", `'a\
b'`: "ab", `'\8'`: `\8`, `'\ud800'`: "\xed\xa0\x80",
	} {
		got, ok := pyStringLiteral(text)
		if !ok || got != want {
			t.Errorf("%s: got %q %v want %q", text, got, ok, want)
		}
	}
	for _, text := range []string{`'\N{}'`, `'\N{NO SUCH NAME}'`, `'\U00110000'`, `'\x4'`, `'a\'`, "'a\nb'", `'\N{CJK UNIFIED IDEOGRAPH-04E00}'`, `'\N{TANGUT IDEOGRAPH-18D1E}'`} {
		if got, ok := pyStringLiteral(text); ok {
			t.Errorf("%s: evaluated to %q, Python refuses it", text, got)
		}
	}
}
