package pyjson

import "testing"

type strSubclass struct{ text, extra string }

func (s *strSubclass) PyStr() string { return s.text }

func TestDumpsEncodesStrSubclassAsItsText(t *testing.T) {
	got, err := Dumps(map[string]any{"summary": &strSubclass{text: "visible", extra: "forced"}})
	if err != nil || got != `{"summary": "visible"}` {
		t.Fatalf("dumps %q %v", got, err)
	}
}
