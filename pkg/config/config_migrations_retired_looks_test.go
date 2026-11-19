package config

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func m67Run(t *testing.T, doc map[string]any) (map[string]any, *MigrationResults, string) {
	t.Helper()
	var out bytes.Buffer
	var persisted map[string]any
	res := &MigrationResults{}
	r := &migrationRun{h: t.TempDir(), res: res, src: MigrationSources{
		Out:     &out,
		ReadRaw: func(string) (map[string]any, error) { return doc, nil },
		Persist: func(_ string, cfg map[string]any) error { persisted = cfg; return nil },
	}}
	if err := migrateTo67(r); err != nil {
		t.Fatal(err)
	}
	return persisted, res, out.String()
}

func TestMigration67MovesRetiredSkinsAndTheKaomojiIndicator(t *testing.T) {
	for retired, want := range retiredSkins {
		if _, ok := builtinSkin(want); !ok {
			t.Fatalf("%s is moved to %s, which is not a built in skin", retired, want)
		}
		if _, still := builtinSkin(retired); still {
			t.Fatalf("%s is retired and still built in", retired)
		}
		persisted, res, _ := m67Run(t, map[string]any{"display": map[string]any{"skin": " " + strings.ToUpper(retired) + " ", "compact": true}})
		if !reflect.DeepEqual(persisted, map[string]any{"display": map[string]any{"skin": want, "compact": true}}) || len(res.ConfigAdded) != 1 {
			t.Fatalf("%s migrated to %v, added %v", retired, persisted, res.ConfigAdded)
		}
	}
	persisted, res, _ := m67Run(t, map[string]any{"display": map[string]any{"skin": "daylight", "tui_status_indicator": "Kaomoji"}})
	if !reflect.DeepEqual(persisted, map[string]any{"display": map[string]any{"skin": "light", "tui_status_indicator": "unicode"}}) {
		t.Fatalf("both moved to %v", persisted)
	}
	if len(res.ConfigAdded) != 1 || res.ConfigAdded[0] != "display.skin=light, display.tui_status_indicator=unicode" {
		t.Fatalf("added %v", res.ConfigAdded)
	}
}

func TestMigration67LeavesCurrentAndUserLooksAlone(t *testing.T) {
	for _, doc := range []map[string]any{
		{"display": map[string]any{"skin": "taro", "tui_status_indicator": "emoji"}},
		{"display": map[string]any{"skin": "default"}},
		{"display": map[string]any{"skin": "my-own-skin"}},
		{"display": map[string]any{"skin": 3, "tui_status_indicator": nil}},
		{"display": "not a mapping"},
		{"model": map[string]any{"default": "x"}},
	} {
		if persisted, res, out := m67Run(t, doc); persisted != nil || len(res.ConfigAdded) != 0 || out != "" {
			t.Fatalf("%v was rewritten to %v (%v, %q)", doc, persisted, res.ConfigAdded, out)
		}
	}
}
