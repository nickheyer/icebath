package tools

import (
	"math"
	"math/big"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestTruthyStringIsTheHermesSet(t *testing.T) {
	for s, want := range map[string]bool{
		"1": true, "true": true, "yes": true, "on": true, " TRUE ": true, "On\n": true, " yes ": true, "\x1cyes\x1f": true,
		"enabled": false, "approve": false, "y": false, "0": false, "": false, "false": false, "ye s": false,
	} {
		if truthyString(s) != want {
			t.Fatalf("%q: want %v", s, want)
		}
	}
	for _, c := range []struct {
		v        any
		fallback bool
		want     bool
	}{
		{nil, true, true}, {nil, false, false}, {"enabled", true, false}, {false, true, false},
		{0, true, false}, {1.5, false, true}, {map[string]any{}, true, false}, {map[string]any{"a": 1}, false, true},
		{[]any{}, true, false}, {[]any{0}, false, true},
	} {
		if truthyValue(c.v, c.fallback) != c.want {
			t.Fatalf("%#v %v: want %v", c.v, c.fallback, c.want)
		}
	}
}

func TestKeylessEnabledReadsTheSettingWithBool(t *testing.T) {
	for _, c := range []struct {
		web  any
		want bool
	}{
		{map[string]any{"keyless_fallback": "false"}, true},
		{map[string]any{"keyless_fallback": false}, false},
		{map[string]any{"keyless_fallback": nil}, false},
		{map[string]any{"keyless_fallback": 0}, false},
		{map[string]any{"keyless_fallback": "enabled"}, true},
		{map[string]any{}, true},
		{nil, true},
		{"x", true},
		{[]any{1}, true},
	} {
		env := &Env{Config: config.Tree{"web": c.web}}
		if keylessEnabled(env) != c.want {
			t.Fatalf("%#v: want %v", c.web, c.want)
		}
	}
	if !keylessEnabled(nil) || !keylessEnabled(&Env{Config: config.Tree{}}) {
		t.Fatal("unset setting enables the keyless tier")
	}
}

func TestPyTruthyEmptyContainersAreFalse(t *testing.T) {
	for _, v := range []any{map[string]any{}, []string{}, []any{}, pyjson.NewObject(), "", 0, int32(0), 0.0, nil, false, new(big.Int)} {
		if pyTruthy(v) {
			t.Errorf("pyTruthy(%#v) true", v)
		}
	}
	for _, v := range []any{map[string]any{"a": 1}, []string{"x"}, "0", math.NaN(), big.NewInt(3), true} {
		if !pyTruthy(v) {
			t.Errorf("pyTruthy(%#v) false", v)
		}
	}
}

func TestBoundErrorTextCountsCodePoints(t *testing.T) {
	text := strings.Repeat("a", maxToolErrorChars-1) + "éétail"
	got := boundErrorText(text)
	if !utf8.ValidString(got) || got != strings.Repeat("a", maxToolErrorChars-1)+"é"+toolErrorTruncationMarker {
		t.Fatalf("bounded %q", got[len(got)-40:])
	}
	short := strings.Repeat("é", maxToolErrorChars)
	if boundErrorText(short) != short {
		t.Fatal("text at the budget in code points was cut")
	}
}
