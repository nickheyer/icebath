package plugins

import (
	"context"
	"math"
	"reflect"
	"testing"
)

func TestArgFloatConvertsLikeFloatAndFallsBack(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want float64
	}{{0.5, 0.5}, {600, 600}, {"x", 15}, {nil, 15}} {
		if got := ArgFloat(tc.in, 15); got != tc.want {
			t.Fatalf("ArgFloat(%v) = %v", tc.in, got)
		}
	}
	if got := ArgFloat(math.NaN(), 15); !math.IsNaN(got) {
		t.Fatalf("nan became %v", got)
	}
}

func TestSwapHostInterpreterRestoresTheResolver(t *testing.T) {
	restore := SwapHostInterpreter(func(context.Context) (string, error) { return "/bin/probe-python", nil })
	if got, err := hostInterpreter(context.Background()); err != nil || got != "/bin/probe-python" {
		t.Fatalf("swapped %q %v", got, err)
	}
	restore()
	if reflect.ValueOf(hostInterpreter).Pointer() != reflect.ValueOf(pluginInterpreter).Pointer() {
		t.Fatal("resolver not restored")
	}
}
