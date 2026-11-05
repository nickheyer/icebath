package plugins

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestRestoreFollowsDeclaredHints(t *testing.T) {
	cases := []struct {
		hint  string
		value any
		want  any
	}{
		{"Optional[str]", "x", "x"},
		{"str | None", nil, nil},
		{"Any", []any{int64(1)}, []any{int64(1)}},
		{"frozenset[str]", []any{"a", "b", "a"}, PySet{"a", "b"}},
		{"frozenset", []any{"a", "a"}, []any{"a", "a"}},
		{"set[int]", []any{int64(1), true, 1.0, int64(2)}, PySet{int64(1), int64(2)}},
		{"tuple[str, str]", []any{"a", "b"}, []any{"a", "b"}},
		{"tuple[str, str]", []any{"a"}, []any{"a"}},
		{"tuple[bool, str | None]", []any{true, nil}, []any{true, nil}},
		{"list[tuple[bool, str, str]]", []any{[]any{true, "a", "b"}}, []any{[]any{true, "a", "b"}}},
		{"dict[str, Any]", Record{"a": int64(1)}, map[string]any{"a": int64(1)}},
		{"Mapping[str, Any]", "not a dict", "not a dict"},
		{"Iterator[bytes]", []any{"x"}, []any{"x"}},
		{"Optional[RecallStatus]", Record{"provider_label": "mem", "count": int64(2)},
			&Dataclass{Type: "RecallStatus", Fields: Record{"provider_label": "mem", "count": int64(2), "glyph": "\U0001f9e0"}}},
		{"Optional[RecallStatus]", Record{"count": int64(2)}, Record{"count": int64(2)}},
		{"RecallStatus", map[string]any{"provider_label": "mem", "count": int64(2)}, map[string]any{"provider_label": "mem", "count": int64(2)}},
		{"FetchResult", Record{"secrets": Record{"K": "v"}, "error_kind": "auth_failed", "extra": int64(1)},
			&Dataclass{Type: "FetchResult", Fields: Record{"secrets": map[string]any{"K": "v"}, "applied": []any{}, "skipped": []any{},
				"warnings": []any{}, "error": nil, "error_kind": "auth_failed", "binary_path": nil}}},
	}
	for _, tc := range cases {
		got, err := restoreHinted(tc.value, parsePyHint(tc.hint))
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("restore(%#v, %s) = %#v, %v; want %#v", tc.value, tc.hint, got, err, tc.want)
		}
	}
	var builtin *HostBuiltinError
	if _, err := restoreHinted([]any{[]any{"x"}}, parsePyHint("frozenset[str]")); !errors.As(err, &builtin) || builtin.Message != "unhashable type: 'list'" {
		t.Fatalf("unhashable set element error = %v", err)
	}
}

func TestDeclaredReturnAndHostObjectBase(t *testing.T) {
	engine := HostObjectBases["register_context_engine"]
	if got := hostObjectBase(declaredReturn(engine, "clone_for_agent")); got != engine {
		t.Fatalf("clone_for_agent base = %q", got)
	}
	if declaredReturn(engine, "missing") != nil || hostObjectBase(declaredReturn(engine, "compress")) != "" {
		t.Fatal("undeclared or container returns must not name a provider base")
	}
	if got := declaredReturn(HostObjectBases["register_tts_provider"], "list_models"); got == nil || hintContainer(got) != "list" {
		t.Fatalf("inherited catalog return = %#v", got)
	}
	for method, ref := range HostObjectBases {
		if _, ok := hostDeclaredReturns[ref]; !ok {
			t.Errorf("%s base %s has no declared return table", method, ref)
		}
	}
}

func TestIsolationModeAndRefusal(t *testing.T) {
	ctx := context.Background()
	t.Setenv(HostProcessEnv, "")
	cfg := func(v any) map[string]any { return map[string]any{"plugins": map[string]any{"isolation": v}} }
	for value, want := range map[any]string{"host": IsolationHost, " Host ": IsolationHost, "in-process": IsolationInProcess, nil: IsolationInProcess, "": IsolationInProcess} {
		if got := IsolationMode(ctx, cfg(value)); got != want {
			t.Errorf("IsolationMode(%v) = %q, want %q", value, got, want)
		}
	}
	logs := captureLogs(t)
	if got := IsolationMode(ctx, cfg("sandbox")); got != IsolationInProcess || !strings.Contains(logs.String(), "plugins.isolation='sandbox' is not one of host, in_process; loading plugins in-process") {
		t.Fatalf("unknown mode = %q, log %q", got, logs.String())
	}
	if r := InProcessImportRefusal(ctx, "memory provider 'x'", "user", cfg("host")); !strings.HasPrefix(r, "memory provider 'x' runs only in-process, and plugins.isolation is 'host'") {
		t.Fatalf("refusal = %q", r)
	}
	if InProcessImportRefusal(ctx, "x", "bundled", cfg("host")) != "" || InProcessImportRefusal(ctx, "x", "user", cfg("in_process")) != "" {
		t.Fatal("bundled code and in_process mode must import")
	}
	t.Setenv(HostProcessEnv, "1")
	if IsolationMode(ctx, cfg("host")) != IsolationInProcess {
		t.Fatal("inside the host every loader imports directly")
	}
	h := t.TempDir()
	if host, err := UserPluginHost(homeCtx(h)); host != nil || err != nil {
		t.Fatalf("UserPluginHost in process = %v %v", host, err)
	}
}
