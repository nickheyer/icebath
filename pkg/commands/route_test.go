package commands

import (
	"errors"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/i18n"
)

func quickRouter(quick any) *Router {
	return &Router{Config: config.Tree{"quick_commands": quick}}
}

func TestRouterQuickCommandRoutingReplies(t *testing.T) {
	ctx := quickTestContext(t)
	rt := quickRouter(map[string]any{
		"notarget": map[string]any{"type": "alias"},
		"blank":    map[string]any{"type": "alias", "target": "   "},
		"odd":      map[string]any{"type": "shell", "command": "echo"},
		"untyped":  map[string]any{"command": "echo"},
		"empty":    map[string]any{"type": "exec"},
		"emptystr": map[string]any{"type": "exec", "command": ""},
		"zero":     map[string]any{"type": "exec", "command": 0},
		"no":       map[string]any{"type": "exec", "command": false},
		"dn":       map[string]any{"type": "exec", "command": "echo daily-note"},
		"listed":   map[string]any{"type": "exec", "command": []any{"echo", "hi"}},
	})
	cases := []struct {
		line string
		kind RouteKind
		msg  string
		exec any
	}{
		{"/notarget", RouteQuickError, "Quick command '/notarget' has no target defined", nil},
		{"/BLANK arg", RouteQuickError, "Quick command '/blank' has no target defined", nil},
		{"/odd", RouteQuickError, "Quick command '/odd' has unsupported type (supported: 'exec', 'alias')", nil},
		{"/untyped", RouteQuickError, "Quick command '/untyped' has unsupported type (supported: 'exec', 'alias')", nil},
		{"/empty", RouteQuickError, "Quick command '/empty' has no command defined", nil},
		{"/emptystr", RouteQuickError, "Quick command '/emptystr' has no command defined", nil},
		{"/zero", RouteQuickError, "Quick command '/zero' has no command defined", nil},
		{"/no", RouteQuickError, "Quick command '/no' has no command defined", nil},
		{"/dn", RouteQuickExec, "", "echo daily-note"},
		{"/listed", RouteQuickExec, "", []any{"echo", "hi"}},
	}
	for _, c := range cases {
		r := rt.Route(ctx, c.line)
		if r.Kind != c.kind || r.Message != c.msg || !reflect.DeepEqual(r.Exec, c.exec) {
			t.Fatalf("%s routed %+v", c.line, r)
		}
	}
}

func TestRouterQuickAliasRedispatchesTheTarget(t *testing.T) {
	ctx := quickTestContext(t)
	rt := quickRouter(map[string]any{
		"st":    map[string]any{"type": "alias", "target": " status "},
		"a":     map[string]any{"type": "alias", "target": "/b one"},
		"b":     map[string]any{"type": "exec", "command": "echo b"},
		"loop1": map[string]any{"type": "alias", "target": "/loop2"},
		"loop2": map[string]any{"type": "alias", "target": "loop1 more"},
		"Kq":    map[string]any{"type": "alias", "target": "/status"},
		"kq":    map[string]any{"type": "alias", "target": "/status"},
	})
	r := rt.Route(ctx, "/st  Foo Bar ")
	if r.Kind != RouteBuiltin || r.Inv.Name != "status" || r.Inv.Args != "Foo Bar" || r.Expanded != "/status Foo Bar" {
		t.Fatalf("alias to builtin %+v %+v", r, r.Inv)
	}
	r = rt.Route(ctx, "/a two")
	if r.Kind != RouteQuickExec || r.Exec != "echo b" || r.Expanded != "/b one two" {
		t.Fatalf("alias chain %+v", r)
	}
	r = rt.Route(ctx, "/loop1 x")
	var raised *RaisedError
	if r.Kind != RouteRaised || !errors.As(r.Err, &raised) || raised.Class != "RecursionError" || raised.Msg != "maximum recursion depth exceeded" {
		t.Fatalf("alias cycle %+v", r)
	}
	r = rt.Route(ctx, "/KQ x")
	if r.Kind != RouteBuiltin || r.Inv.Name != "status" || r.Inv.Args != "x" || r.Expanded != "/status x" {
		t.Fatalf("code point slicing of user args %+v %+v", r, r.Inv)
	}
}

func TestRouterQuickCommandBeforeSkill(t *testing.T) {
	ctx := quickTestContext(t)
	rt := quickRouter(map[string]any{"mygif": map[string]any{"type": "exec", "command": "echo overridden"}})
	rt.Skills = func() map[string]SkillInfo { return map[string]SkillInfo{"/mygif": {Name: "gif-search"}} }
	rt.Bundles = func() map[string]BundleInfo { return map[string]BundleInfo{"/mygif": {Name: "gifs"}} }
	rt.Plugins = func() []string { return []string{"mygif"} }
	if r := rt.Route(ctx, "/mygif"); r.Kind != RouteQuickExec || r.Exec != "echo overridden" {
		t.Fatalf("quick command lost to %+v", r)
	}
}

func TestRouterMalformedQuickCommandsRaise(t *testing.T) {
	ctx := quickTestContext(t)
	cases := []struct {
		quick any
		line  string
		class string
		msg   string
	}{
		{nil, "/zzz", "TypeError", "argument of type 'NoneType' is not a container or iterable"},
		{5, "/zzz", "TypeError", "argument of type 'int' is not a container or iterable"},
		{2.5, "/zzz", "TypeError", "argument of type 'float' is not a container or iterable"},
		{true, "/zzz", "TypeError", "argument of type 'bool' is not a container or iterable"},
		{[]any{"zzz"}, "/zzz", "TypeError", "list indices must be integers or slices, not str"},
		{"xzzzx", "/zzz", "TypeError", "string indices must be integers, not 'str'"},
		{map[string]any{"zzz": "echo"}, "/zzz", "AttributeError", "'str' object has no attribute 'get'"},
		{map[string]any{"zzz": nil}, "/zzz", "AttributeError", "'NoneType' object has no attribute 'get'"},
		{map[string]any{"zzz": []any{1}}, "/zzz", "AttributeError", "'list' object has no attribute 'get'"},
		{map[string]any{"zzz": map[string]any{"type": "alias", "target": nil}}, "/zzz", "AttributeError", "'NoneType' object has no attribute 'strip'"},
		{map[string]any{"zzz": map[string]any{"type": "alias", "target": 5}}, "/zzz", "AttributeError", "'int' object has no attribute 'strip'"},
		{map[string]any{"zzz": map[string]any{"type": "alias", "target": []any{"/status"}}}, "/zzz", "AttributeError", "'list' object has no attribute 'strip'"},
	}
	for _, c := range cases {
		r := quickRouter(c.quick).Route(ctx, c.line)
		var raised *RaisedError
		if r.Kind != RouteRaised || !errors.As(r.Err, &raised) || raised.Class != c.class || raised.Msg != c.msg || r.Err.Error() != c.msg {
			t.Fatalf("%v routed %+v", c.quick, r)
		}
	}
	for _, quick := range []any{[]any{"other", 7}, "abc"} {
		if r := quickRouter(quick).Route(ctx, "/zzz"); r.Kind != RouteUnknown {
			t.Fatalf("%v routed %+v", quick, r)
		}
	}
	if r := (&Router{Config: config.Tree{}}).Route(ctx, "/zzz"); r.Kind != RouteUnknown {
		t.Fatalf("absent quick_commands routed %+v", r)
	}
}

func TestRouterPrefixExpansionLinesAreLocalized(t *testing.T) {
	ctx := quickTestContext(t)
	rt := &Router{Config: config.Tree{}, Skills: func() map[string]SkillInfo {
		return map[string]SkillInfo{"/zzalpha": {Name: "a"}, "/zzbravo": {Name: "b"}, "/qqlong-one": {Name: "c"}, "/qqx": {Name: "d"}}
	}}
	r := rt.Route(ctx, "/ZZ some args")
	if r.Kind != RouteAmbiguous || !reflect.DeepEqual(r.Candidates, []string{"/zzalpha", "/zzbravo"}) ||
		r.Lines != [2]string{"Ambiguous command: /zz some args", "Did you mean: /zzalpha, /zzbravo?"} {
		t.Fatalf("ambiguous %+v", r)
	}
	if r = rt.Route(ctx, "/qq Keep Case"); r.Kind != RouteSkill || !reflect.DeepEqual(r.SkillKeys, []string{"/qqx"}) || r.Instruction != "Keep Case" || r.Expanded != "/qqx Keep Case" {
		t.Fatalf("unique shortest %+v", r)
	}
	t.Setenv("ICEBATH_LANGUAGE", "de")
	de := i18n.LoadBundled("de")
	want, _ := i18n.PyFormat(de["cli.command.ambiguous"], map[string]any{"command": "/zz"})
	if r = rt.Route(ctx, "/zz"); r.Lines[0] != want || want == "" {
		t.Fatalf("german ambiguous %q want %q", r.Lines[0], want)
	}
}
