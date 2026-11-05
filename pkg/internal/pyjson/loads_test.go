package pyjson

import (
	"strings"
	"testing"
)

func TestLoadsLenientAcceptsControlCharacters(t *testing.T) {
	text := "{\"a\": \"x\ty\nz\"}"
	if _, err := Loads(text); err == nil || !strings.Contains(err.Error(), "Invalid control character at") {
		t.Fatalf("strict decode accepted control characters: %v", err)
	}
	v, err := LoadsLenient(text)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := v.(*Object).Get("a"); got != "x\ty\nz" {
		t.Fatalf("got %q", got)
	}
}
