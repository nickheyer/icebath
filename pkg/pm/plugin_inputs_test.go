package pm

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

// The worker receives the same plugin input the caller built
func TestPluginInputSurvivesTheWorkerWire(t *testing.T) {
	root := t.TempDir()
	cases := map[string]PluginInput{
		"none":       nil,
		"empty":      &Members{Dirs: []MemberDir{}},
		"paths":      &Members{Dirs: MemberPaths([]string{filepath.Join(root, "plugins/a"), filepath.Join(root, "plugins/b")})},
		"sources":    &Members{Dirs: []MemberDir{{Identity: filepath.Join(root, "home/plugins/a"), Source: filepath.Join(root, "staged/a")}}, Mapped: true},
		"candidates": &Candidates{Dirs: []string{filepath.Join(root, "plugins/candidate")}},
		"selection": &Selection{Data: map[string]any{"home": filepath.Join(root, "home"), "enabled": []any{"a"}, "disabled": []any{},
			"expected_config": "missing"}},
		"staged": &StagedUpdate{Data: map[string]any{"target": filepath.Join(root, "home/plugins/a"), "staged": filepath.Join(root, "staged/a"),
			"target_digest": nil, "old_metadata": map[string]any{}, "new_metadata": map[string]any{"a": map[string]any{"revision": "new"}}}},
	}
	for name, plugins := range cases {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal(EncodePluginInput(plugins))
			if err != nil {
				t.Fatal(err)
			}
			wire, err := loadJSON(encoded)
			if err != nil {
				t.Fatal(err)
			}
			object, _ := wire.(map[string]any)
			decoded, err := DecodePluginInput(object)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded, plugins) {
				t.Fatalf("decoded %#v, want %#v", decoded, plugins)
			}
		})
	}
}

// An unknown kind is refused with the kind's repr
func TestPluginInputUnknownKindIsRefused(t *testing.T) {
	_, err := DecodePluginInput(map[string]any{"kind": "mystery"})
	if err == nil || err.Error() != "unknown plugin input kind: 'mystery'" || errorClass(err) != "ValueError" {
		t.Fatalf("got %v", err)
	}
}
