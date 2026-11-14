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

type namedValue struct{ text string }

func (v namedValue) PyStr() string    { return v.text }
func (namedValue) PyTypeName() string { return "datetime" }

func TestDumpsRefusesAValueThatOnlyHasAStrForm(t *testing.T) {
	_, err := Dumps(map[string]any{"created": namedValue{"2026-08-22 00:00:00"}})
	if err == nil || err.Error() != "Object of type datetime is not JSON serializable" {
		t.Fatalf("err = %v", err)
	}
	got, err := Dumps(namedValue{"x"}, Default(func(v any) (any, error) { return "via default", nil }))
	if err != nil || got != `"via default"` {
		t.Fatalf("dumps %q %v", got, err)
	}
}
