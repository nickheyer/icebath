package tui

import (
	"testing"
)

func TestParseDetailsMode(t *testing.T) {
	t.Run("accepts the canonical modes case-insensitively", func(t *testing.T) {
		if parseDetailsMode("hidden") != "hidden" || parseDetailsMode(" COLLAPSED ") != "collapsed" || parseDetailsMode("Expanded") != "expanded" {
			t.Fatal("modes")
		}
	})
	t.Run("rejects junk", func(t *testing.T) {
		for _, v := range []string{"truncated", "", "42"} {
			if parseDetailsMode(v) != "" {
				t.Fatalf("%v accepted", v)
			}
		}
	})
}

func TestIsSectionName(t *testing.T) {
	t.Run("only lets the four canonical sections through", func(t *testing.T) {
		for v, want := range map[string]bool{"thinking": true, "tools": true, "subagents": true, "activity": true, "Thinking": false, "bogus": false, "": false} {
			if isSectionName(v) != want {
				t.Fatalf("%v", v)
			}
		}
	})
}

func TestSectionMode(t *testing.T) {
	type c struct {
		name, global string
		sections     map[string]string
		override     bool
		want         string
	}
	for _, group := range []struct {
		title string
		cases []c
	}{
		{"falls back to the global mode for sections without a built-in default", []c{{"subagents", "collapsed", map[string]string{}, false, "collapsed"}, {"subagents", "expanded", nil, false, "expanded"}, {"subagents", "hidden", map[string]string{}, false, "hidden"}}},
		{"streams thinking + tools expanded by default for persisted config values", []c{{"thinking", "collapsed", map[string]string{}, false, "expanded"}, {"thinking", "hidden", nil, false, "expanded"}, {"tools", "collapsed", map[string]string{}, false, "expanded"}, {"tools", "hidden", nil, false, "expanded"}}},
		{"hides the activity panel by default for persisted config values", []c{{"activity", "collapsed", map[string]string{}, false, "hidden"}, {"activity", "expanded", nil, false, "hidden"}, {"activity", "hidden", map[string]string{}, false, "hidden"}}},
		{"applies in-session /details mode globally over built-in defaults", []c{{"thinking", "collapsed", map[string]string{}, true, "collapsed"}, {"tools", "hidden", map[string]string{}, true, "hidden"}, {"activity", "expanded", nil, true, "expanded"}}},
		{"honours per-section overrides over both the section default and global mode", []c{{"thinking", "collapsed", map[string]string{"thinking": "collapsed"}, false, "collapsed"}, {"tools", "collapsed", map[string]string{"tools": "hidden"}, false, "hidden"}, {"activity", "collapsed", map[string]string{"activity": "expanded"}, false, "expanded"}, {"activity", "expanded", map[string]string{"activity": "collapsed"}, false, "collapsed"}}},
		{"lets per-section overrides escape the global hidden mode", []c{{"subagents", "hidden", map[string]string{"subagents": "expanded"}, false, "expanded"}, {"thinking", "hidden", map[string]string{"thinking": "collapsed"}, false, "collapsed"}, {"activity", "hidden", map[string]string{"activity": "expanded"}, false, "expanded"}}},
	} {
		t.Run(group.title, func(t *testing.T) {
			for _, k := range group.cases {
				if got := sectionMode(k.name, k.global, k.sections, k.override); got != k.want {
					t.Fatalf("%+v got %s", k, got)
				}
			}
		})
	}
}
