package tools

import (
	"errors"
	"testing"
)

func TestEscapeDriftGolden(t *testing.T) {
	g := loadFileEditGolden(t)
	for _, c := range g.Drift {
		o, n, note, err := maybeCorrectEscapeDrift(c.Old, c.New, c.Content)
		errText := ""
		if err != nil {
			errText = err.Error()
		}
		if o != c.ROld || n != c.RNew || note != c.Note || errText != c.Error {
			t.Errorf("maybeCorrectEscapeDrift(%q, %q, %q):\n got  (%q, %q, %q, %q)\n want (%q, %q, %q, %q)", c.Old, c.New, c.Content, o, n, note, errText, c.ROld, c.RNew, c.Note, c.Error)
		}
	}
}

func TestEscapeDriftQuoteCorrection(t *testing.T) {
	o, n, note, err := maybeCorrectEscapeDrift(`it\'s`, `it\'s fine`, "it's here")
	if err != nil || o != "it's" || n != "it's fine" || note == "" {
		t.Fatalf("got %q %q %q %v", o, n, note, err)
	}
}

func TestEscapeDriftBackslashAmbiguous(t *testing.T) {
	_, _, _, err := maybeCorrectEscapeDrift(`a\\b`, `x\\y`, `a\b`)
	var amb *ambiguousEscapeDriftError
	if !errors.As(err, &amb) {
		t.Fatalf("expected ambiguous drift error, got %v", err)
	}
}

func TestEscapeDriftHelpers(t *testing.T) {
	if got := halveBackslashRuns(`a\\\\b\\c`); got != `a\\b\c` {
		t.Fatalf("halveBackslashRuns = %q", got)
	}
	if !allBackslashRunsEven(`a\\b\\\\`) || allBackslashRunsEven(`a\\\b`) {
		t.Fatal("allBackslashRunsEven misclassified")
	}
	if !quoteEscapesAllSpurious(`it\'s`, `don\'t`) || quoteEscapesAllSpurious(`it\'s`, `don't`) || quoteEscapesAllSpurious(`it\\'s`) {
		t.Fatal("quoteEscapesAllSpurious misclassified")
	}
	runs := backslashRuns(`\a\\b\\\`)
	if len(runs) != 3 || runs[0] != 1 || runs[1] != 2 || runs[2] != 3 {
		t.Fatalf("backslashRuns = %v", runs)
	}
}
