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

func TestBindHostCallExportsThePythonKeywordBinding(t *testing.T) {
	params := []HostParam{HostReq("name"), HostKwo("session_key", ""), HostKwo("apply_now", false)}
	bound, _, err := BindHostCall("DelegationBackends", "set_global_backend", params, []any{"local"}, map[string]any{"apply_now": true})
	if err != nil || bound["name"] != "local" || bound["session_key"] != "" || bound["apply_now"] != true {
		t.Fatalf("bound %v %v", bound, err)
	}
	if _, _, err := BindHostCall("DelegationBackends", "set_global_backend", params, nil, nil); err == nil {
		t.Fatal("a missing required argument is a TypeError")
	}
	if _, _, err := BindHostCall("DelegationBackends", "set_global_backend", params, []any{"local", "extra"}, nil); err == nil {
		t.Fatal("a keyword only parameter refuses a positional value")
	}
}

func TestInstalledServicesExportsTheProcessSet(t *testing.T) {
	previous := InstalledServices()
	t.Cleanup(func() { InstallServices(previous) })
	s := &Services{}
	InstallServices(s)
	if InstalledServices() != s {
		t.Fatal("the exported accessor must read the installed set")
	}
}
