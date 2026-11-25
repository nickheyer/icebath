package plugins

import (
	"errors"
	"reflect"
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
		{"frozenset[str]", []any{"a", "b", "a"}, SetOf{"a", "b"}},
		{"frozenset", []any{"a", "a"}, []any{"a", "a"}},
		{"set[int]", []any{int64(1), true, 1.0, int64(2)}, SetOf{int64(1), int64(2)}},
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
	if _, err := restoreHinted([]any{[]any{"x"}}, parsePyHint("frozenset[str]")); !errors.As(err, &builtin) || builtin.Message != "a set cannot hold a list" {
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
