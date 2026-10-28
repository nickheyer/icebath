package app

import (
	"encoding/json"
	"testing"

	"github.com/nickheyer/icebath/pkg/ui/wm"
)

func TestTemplateFromMarksSessionsAndDropsTransientCards(t *testing.T) {
	l := wm.Layout{Theme: "light", Active: 1, Recent: []string{"quit"}, Workspaces: []wm.WorkspaceLayout{
		{ID: "w1", Mode: "dwindle", Ratio: 0.6,
			Tiles:  []wm.TileLayout{{Key: "session:a", Weight: 2}, {Key: "metrics"}, {Key: "agent:a/x"}},
			Floats: []wm.FloatLayout{{Key: "approval:a", Box: wm.Box{X: 1, Y: 2, W: 3, H: 4}}, {Key: "findings:a"}},
			Tree:   &wm.SplitLayout{Vertical: true, Ratio: 0.4, A: &wm.SplitLayout{Key: "session:a"}, B: &wm.SplitLayout{Vertical: false, Ratio: 0.5, A: &wm.SplitLayout{Key: "agent:a/x"}, B: &wm.SplitLayout{Key: "metrics"}}},
			Focus:  "agent:a/x", Zoom: "session:a",
			Cards: map[string]json.RawMessage{"config": json.RawMessage(`{"tab":"config:app"}`), "agent:a/x": json.RawMessage(`{}`)}},
		{ID: "w2", Tiles: []wm.TileLayout{{Key: "session:gone"}, {Key: "session:b"}, {Key: "mission:b:m1"}}, Focus: "session:b"},
	}}
	spec := func(id string) (templateSession, bool) {
		switch id {
		case "a":
			return templateSession{Profile: "anthropic", Model: "claude", Kind: "critic"}, true
		case "b":
			return templateSession{Profile: "ollama", Model: "llama"}, true
		}
		return templateSession{}, false
	}
	tp := templateFrom("two", l, spec)
	if tp.Name != "two" || tp.Active != 1 || len(tp.Workspaces) != 2 {
		t.Fatalf("template: %+v", tp)
	}
	if len(tp.Sessions) != 2 || tp.Sessions["@1"].Kind != "critic" || tp.Sessions["@2"].Profile != "ollama" {
		t.Fatalf("sessions: %+v", tp.Sessions)
	}
	w1 := tp.Workspaces[0]
	if w1.ID != "w1" || w1.Mode != "dwindle" || w1.Ratio != 0.6 {
		t.Fatalf("w1 settings: %+v", w1)
	}
	if len(w1.Tiles) != 2 || w1.Tiles[0].Key != "session:@1" || w1.Tiles[0].Weight != 2 || w1.Tiles[1].Key != "metrics" {
		t.Fatalf("w1 tiles: %+v", w1.Tiles)
	}
	if len(w1.Floats) != 1 || w1.Floats[0].Key != "findings:@1" {
		t.Fatalf("w1 floats: %+v", w1.Floats)
	}
	if w1.Focus != "" || w1.Zoom != "session:@1" {
		t.Fatalf("w1 focus %q zoom %q", w1.Focus, w1.Zoom)
	}
	if w1.Tree == nil || w1.Tree.A == nil || w1.Tree.A.Key != "session:@1" || w1.Tree.B == nil || w1.Tree.B.Key != "metrics" {
		t.Fatalf("w1 tree should collapse the agent leaf: %+v", w1.Tree)
	}
	if _, ok := w1.Cards["agent:a/x"]; ok || string(w1.Cards["config"]) != `{"tab":"config:app"}` {
		t.Fatalf("w1 cards: %v", w1.Cards)
	}
	w2 := tp.Workspaces[1]
	if len(w2.Tiles) != 1 || w2.Tiles[0].Key != "session:@2" || w2.Focus != "session:@2" {
		t.Fatalf("w2: %+v", w2)
	}
}

func TestTemplateFromKeepsOneMarkPerSession(t *testing.T) {
	l := wm.Layout{Workspaces: []wm.WorkspaceLayout{
		{ID: "w1", Tiles: []wm.TileLayout{{Key: "session:a"}, {Key: "findings:a"}}},
		{ID: "w2", Tiles: []wm.TileLayout{{Key: "session:a"}}},
	}}
	tp := templateFrom("one", l, func(string) (templateSession, bool) { return templateSession{}, true })
	if len(tp.Sessions) != 1 {
		t.Fatalf("one session should take one mark: %+v", tp.Sessions)
	}
	if tp.Workspaces[0].Tiles[1].Key != "findings:@1" || tp.Workspaces[1].Tiles[0].Key != "session:@1" {
		t.Fatalf("workspaces: %+v", tp.Workspaces)
	}
}

func TestTemplatesRoundTrip(t *testing.T) {
	ts := templates{Default: "two", Templates: []template{{Name: "two", Sessions: map[string]templateSession{"@1": {Profile: "p"}}}}}
	data, err := json.Marshal(ts)
	if err != nil {
		t.Fatal(err)
	}
	var back templates
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	d, ok := back.defaultTemplate()
	if !ok || d.Name != "two" || d.Sessions["@1"].Profile != "p" {
		t.Fatalf("default: %v %+v", ok, d)
	}
	back.Default = "gone"
	if _, ok := back.defaultTemplate(); ok {
		t.Fatal("a default naming no template should not resolve")
	}
}
