package tools

import "testing"

func TestInfReturnsOriginalString(t *testing.T) {
	if got, ok := coerceNumber("inf", false); ok || got != "inf" {
		t.Fatalf("inf coerced to %v %v", got, ok)
	}
}

func TestNanReturnsOriginalString(t *testing.T) {
	if got, ok := coerceNumber("nan", false); ok || got != "nan" {
		t.Fatalf("nan coerced to %v %v", got, ok)
	}
}

func TestNormalNumbersStillCoerce(t *testing.T) {
	for raw, want := range map[string]any{"42": int64(42), "3.14": 3.14, "1e3": int64(1000)} {
		if got, ok := coerceNumber(raw, false); !ok || got != want {
			t.Fatalf("%s coerced to %v %v", raw, got, ok)
		}
	}
}
