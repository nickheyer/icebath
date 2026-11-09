package tools

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func availabilitySchema(name string) map[string]any {
	return map[string]any{"name": name, "description": name, "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}
}

func availabilityRegistry(t *testing.T, defs ...Def) (*Registry, *Env) {
	t.Helper()
	InvalidateCheckCache()
	t.Cleanup(InvalidateCheckCache)
	r := NewRegistry()
	for _, d := range defs {
		if d.Schema == nil {
			d.Schema = availabilitySchema(d.Name)
		}
		if d.Handler == nil {
			d.Handler = constHandler(`{"ok": true}`)
		}
		r.Register(d)
	}
	return r, &Env{Home: t.TempDir()}
}

func unavailableNames(rows []UnavailableToolset) []string {
	out := []string{}
	for _, u := range rows {
		out = append(out, u.Name)
	}
	return out
}

// Definitions come back in OpenAI function format for every named tool
func TestGetDefinitionsReturnsOpenAIFormat(t *testing.T) {
	r, env := availabilityRegistry(t, Def{Name: "t1", Toolset: "s1"}, Def{Name: "t2", Toolset: "s1"})
	defs := r.Definitions(context.Background(), env, map[string]bool{"t1": true, "t2": true}, false)
	names := map[string]bool{}
	for _, d := range defs {
		if d["type"] != "function" {
			t.Fatalf("type %v", d["type"])
		}
		names[d["function"].(map[string]any)["name"].(string)] = true
	}
	if len(defs) != 2 || !names["t1"] || !names["t2"] {
		t.Fatalf("defs %v", defs)
	}
}

// A toolset without a check is available, a failing check hides it, a panicking handler returns an error
func TestNoCheckFnIsAvailable(t *testing.T) {
	r, env := availabilityRegistry(t, Def{Name: "t", Toolset: "free"})
	if !r.IsToolsetAvailable(context.Background(), env, "free") {
		t.Fatal("toolset without a check is unavailable")
	}
}

func TestCheckFnControlsAvailability(t *testing.T) {
	r, env := availabilityRegistry(t, Def{Name: "t", Toolset: "locked", Check: func(*Env) bool { return false }})
	if r.IsToolsetAvailable(context.Background(), env, "locked") {
		t.Fatal("failing check left the toolset available")
	}
}

func TestHandlerExceptionReturnsError(t *testing.T) {
	r, env := availabilityRegistry(t, Def{Name: "bad", Toolset: "s", Handler: func(context.Context, *Call) string { panic(&RuntimeError{msg: "boom"}) }})
	result := decodeModelToolsResult(t, NewDispatcher(r).RegistryDispatch(context.Background(), &Call{Name: "bad", Args: map[string]any{}, Env: env}))
	if msg, _ := result["error"].(string); !strings.Contains(msg, "RuntimeError") {
		t.Fatalf("error %v", result)
	}
}

// RuntimeError is a named error type so the dispatch text carries its type like the Python exception class
type RuntimeError struct{ msg string }

func (e *RuntimeError) Error() string { return e.msg }

func TestIsToolsetAvailableCatchesException(t *testing.T) {
	r, env := availabilityRegistry(t, Def{Name: "t", Toolset: "broken", Check: func(*Env) bool { panic("division by zero") }})
	if r.IsToolsetAvailable(context.Background(), env, "broken") {
		t.Fatal("panicking check left the toolset available")
	}
}

func TestCheckToolAvailabilitySurvivesRaisingCheck(t *testing.T) {
	r, env := availabilityRegistry(t, Def{Name: "a", Toolset: "works", Check: func(*Env) bool { return true }},
		Def{Name: "b", Toolset: "crashes", Check: func(*Env) bool { panic("division by zero") }})
	available, unavailable := r.CheckToolAvailability(context.Background(), env)
	if !reflect.DeepEqual(available, []string{"works"}) || !reflect.DeepEqual(unavailableNames(unavailable), []string{"crashes"}) {
		t.Fatalf("available %v unavailable %v", available, unavailable)
	}
}

func TestMixedToolsetAvailableWhenGeneralToolPasses(t *testing.T) {
	r, env := availabilityRegistry(t, Def{Name: "read_terminal", Toolset: "terminal", Check: func(*Env) bool { return false }},
		Def{Name: "terminal", Toolset: "terminal", Check: func(*Env) bool { return true }}, Def{Name: "process", Toolset: "terminal"})
	available, unavailable := r.CheckToolAvailability(context.Background(), env)
	if !reflect.DeepEqual(available, []string{"terminal"}) || len(unavailable) != 0 || !r.IsToolsetAvailable(context.Background(), env, "terminal") ||
		!r.AvailableToolsets(context.Background(), env)["terminal"].Available {
		t.Fatalf("available %v unavailable %v", available, unavailable)
	}
}

func TestMixedToolsetUnavailableWhenEveryToolIsGated(t *testing.T) {
	r, env := availabilityRegistry(t, Def{Name: "read_terminal", Toolset: "terminal", Check: func(*Env) bool { return false }},
		Def{Name: "terminal", Toolset: "terminal", Check: func(*Env) bool { return false }})
	available, unavailable := r.CheckToolAvailability(context.Background(), env)
	if len(available) != 0 || !reflect.DeepEqual(unavailableNames(unavailable), []string{"terminal"}) {
		t.Fatalf("available %v unavailable %v", available, unavailable)
	}
}

// Toolset requirements group tools with their unioned env vars and the toolset check
func TestToolsetRequirementsCompatRows(t *testing.T) {
	check := func(*Env) bool { return true }
	r, _ := availabilityRegistry(t, Def{Name: "web_search", Toolset: "web", RequiresEnv: []string{"EXA_API_KEY", "TAVILY_API_KEY"}, Check: check},
		Def{Name: "web_extract", Toolset: "web", RequiresEnv: []string{"TAVILY_API_KEY", "FIRECRAWL_API_KEY"}, Check: check}, Def{Name: "read_file", Toolset: "file"})
	reqs := r.ToolsetRequirements(context.Background(), "")
	web, file := reqs["web"], reqs["file"]
	if len(reqs) != 2 || web["name"] != "web" || !reflect.DeepEqual(web["env_vars"], []string{"EXA_API_KEY", "TAVILY_API_KEY", "FIRECRAWL_API_KEY"}) ||
		!reflect.DeepEqual(web["tools"], []string{"web_search", "web_extract"}) || web["setup_url"] != nil || web["check_fn"] == nil {
		t.Fatalf("web row %v", web)
	}
	if !reflect.DeepEqual(file["env_vars"], []string{}) || file["check_fn"] != nil || !reflect.DeepEqual(file["tools"], []string{"read_file"}) {
		t.Fatalf("file row %v", file)
	}
}

// Tool results encode like json.dumps with non ASCII text kept and an empty object for no data
func TestToolResultEncodesLikeJSONDumps(t *testing.T) {
	if got := ToolResult(map[string]any{"ok": true, "text": "café"}); got != `{"ok": true, "text": "café"}` {
		t.Fatalf("got %s", got)
	}
	if got := ToolResult(nil); got != "{}" {
		t.Fatalf("got %s", got)
	}
}
