package config

import (
	"math"
	"testing"
)

func TestNumericValues(t *testing.T) {
	check := func(t *testing.T, raw any, want int) {
		t.Helper()
		if got := ResolveTurnLimit(raw, TurnLimitUnlimited); got != want {
			t.Fatalf("%#v resolved to %d want %d", raw, got, want)
		}
	}
	t.Run("test_int_passthrough", func(t *testing.T) {
		check(t, 90, 90)
		check(t, 120, 120)
		check(t, 1, 1)
	})
	t.Run("test_float_truncated", func(t *testing.T) {
		check(t, 3.7, 3)
		check(t, 3.0, 3)
		check(t, 100.9, 100)
	})
	t.Run("test_numeric_string", func(t *testing.T) {
		check(t, "120", 120)
		check(t, "3", 3)
		check(t, "3.7", 3)
	})
	t.Run("test_negative_int_is_unlimited", func(t *testing.T) {
		check(t, -5, TurnLimitUnlimited)
	})
}

func TestUnlimitedSpellings(t *testing.T) {
	t.Run("test_string_spellings_resolve_to_sentinel", func(t *testing.T) {
		for _, spelling := range []string{"none", "None", "NONE", "unlimited", "infinite", "∞", "-1", "0"} {
			if got := ResolveTurnLimit(spelling, TurnLimitUnlimited); got != TurnLimitUnlimited {
				t.Fatalf("%q resolved to %d", spelling, got)
			}
		}
	})
	t.Run("test_whitespace_tolerant", func(t *testing.T) {
		for _, spelling := range []string{" none ", "  unlimited  ", "\tinfinite\t"} {
			if got := ResolveTurnLimit(spelling, TurnLimitUnlimited); got != TurnLimitUnlimited {
				t.Fatalf("%q resolved to %d", spelling, got)
			}
		}
	})
	t.Run("test_zero_int_is_unlimited", func(t *testing.T) {
		if ResolveTurnLimit(0, TurnLimitUnlimited) != TurnLimitUnlimited {
			t.Fatal("zero int")
		}
	})
	t.Run("test_zero_float_is_unlimited", func(t *testing.T) {
		if ResolveTurnLimit(0.0, TurnLimitUnlimited) != TurnLimitUnlimited {
			t.Fatal("zero float")
		}
	})
}

func TestAbsentAndDefault(t *testing.T) {
	t.Run("test_none_returns_default", func(t *testing.T) {
		if ResolveTurnLimit(nil, TurnLimitUnlimited) != TurnLimitUnlimited {
			t.Fatal("none")
		}
	})
	t.Run("test_none_custom_default", func(t *testing.T) {
		if got := ResolveTurnLimit(nil, 500); got != 500 {
			t.Fatalf("got %d", got)
		}
	})
	t.Run("test_empty_string_returns_default", func(t *testing.T) {
		if ResolveTurnLimit("", TurnLimitUnlimited) != TurnLimitUnlimited {
			t.Fatal("empty")
		}
	})
	t.Run("test_whitespace_only_returns_default", func(t *testing.T) {
		if ResolveTurnLimit("   ", TurnLimitUnlimited) != TurnLimitUnlimited {
			t.Fatal("whitespace")
		}
	})
}

func TestInvalidInputs(t *testing.T) {
	unlimited := func(t *testing.T, raws ...any) {
		t.Helper()
		for _, raw := range raws {
			if got := ResolveTurnLimit(raw, TurnLimitUnlimited); got != TurnLimitUnlimited {
				t.Fatalf("%#v resolved to %d", raw, got)
			}
		}
	}
	t.Run("test_bool_rejected", func(t *testing.T) { unlimited(t, true, false) })
	t.Run("test_garbage_string_returns_default", func(t *testing.T) { unlimited(t, "garbage", "not_a_number") })
	t.Run("test_list_returns_default", func(t *testing.T) { unlimited(t, []any{}, []any{90}) })
	t.Run("test_dict_returns_default", func(t *testing.T) { unlimited(t, map[string]any{}, map[string]any{"max_turns": 90}) })
}

// Numeric spellings parse, a NaN falls back and an infinity means no limit
func TestResolveTurnLimitReadsNumericSpellings(t *testing.T) {
	for raw, want := range map[any]int{"1_000": 1000, "+7": 7, "1e2": 100, "nan": 42, int64(9): 9, "99999999999999999999": TurnLimitUnlimited,
		"-inf": TurnLimitUnlimited, "1e400": TurnLimitUnlimited} {
		if got := ResolveTurnLimit(raw, 42); got != want {
			t.Fatalf("%#v resolved to %d want %d", raw, got, want)
		}
	}
	if got := ResolveTurnLimit(math.NaN(), 42); got != 42 {
		t.Fatalf("NaN resolved to %d", got)
	}
	for _, raw := range []any{math.Inf(1), math.Inf(-1), float32(math.Inf(1))} {
		if got := ResolveTurnLimit(raw, 42); got != TurnLimitUnlimited {
			t.Fatalf("%#v resolved to %d", raw, got)
		}
	}
}
