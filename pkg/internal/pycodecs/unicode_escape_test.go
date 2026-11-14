package pycodecs

import "testing"

func TestUnicodeEscapeMatchesCPython(t *testing.T) {
	for in, want := range map[string]string{
		"plain":            "plain",
		"tab\tnew\nret\r":  `tab\tnew\nret\r`,
		`back\slash`:       `back\\slash`,
		"café":             `caf\xe9`,
		"\u0001\u007f":     `\x01\x7f`,
		"snow ☃":           `snow \u2603`,
		"emoji \U0001f600": `emoji \U0001f600`,
		`quotes ' and "`:   `quotes ' and "`,
	} {
		if got := UnicodeEscape(in); got != want {
			t.Fatalf("%q escaped %q want %q", in, got, want)
		}
	}
}
