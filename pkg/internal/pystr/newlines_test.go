package pystr

import "testing"

func TestTranslateNewlines(t *testing.T) {
	if got := TranslateNewlines("a\r\nb\rc\n\r\n"); got != "a\nb\nc\n\n" {
		t.Fatalf("%q", got)
	}
}
