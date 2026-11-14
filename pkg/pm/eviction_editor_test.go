package pm

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Applies an edit to the plugins mapping of a round tripped config, returning the dumped text and its parse
func edEdit(t *testing.T, config string, edit func(plugins *yaml.Node) error) (string, map[string]any) {
	t.Helper()
	doc, err := loadRoundtrip([]byte(config))
	if err != nil {
		t.Fatal(err)
	}
	if err := edit(rtGet(rtRoot(doc), "plugins")); err != nil {
		t.Fatal(err)
	}
	data, err := dumpRoundtrip(doc)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	plugins, _ := parsed["plugins"].(map[string]any)
	return string(data), plugins
}

// Spelling set of a plugin under its configured names
func edSpellings(names ...string) map[string]bool {
	out := map[string]bool{}
	for _, name := range names {
		out[name] = true
	}
	return out
}

// Spelling lookup a rejoin consults only once the record proves it is due
func edSpell(names ...string) func() (map[string]bool, error) {
	return func() (map[string]bool, error) { return edSpellings(names...), nil }
}

func TestEditorDisableParksThePluginAndRecordsWhatToUndo(t *testing.T) {
	config := "# operator note\nplugins:\n  enabled: [keep, group/slow, slow]  # chosen by hand\n  disabled: [other]\n"
	reason := "  conflict\n\twith   " + strings.Repeat("x", 400)
	text, plugins := edEdit(t, config, func(p *yaml.Node) error {
		return disablePlugin(p, "slow", edSpellings("slow", "group/slow"), reason, "1.2.3")
	})
	if !strings.Contains(text, "# operator note") || !strings.Contains(text, "# chosen by hand") {
		t.Fatalf("comments lost %s", text)
	}
	if !reflect.DeepEqual(svList(plugins["enabled"]), []string{"keep"}) || !reflect.DeepEqual(svList(plugins["disabled"]), []string{"other", "slow"}) {
		t.Fatalf("lists %v", plugins)
	}
	record, _ := svAt(plugins, DisabledByUpdateRecord, "slow").(map[string]any)
	want := ("conflict with " + strings.Repeat("x", 400))[:300]
	if record["enabled"] != true || record["reason"] != want || record["tried"] != "1.2.3" {
		t.Fatalf("record %v", record)
	}

	_, plugins = edEdit(t, text, func(p *yaml.Node) error {
		return disablePlugin(p, "slow", edSpellings("slow"), "still failing", "")
	})
	record, _ = svAt(plugins, DisabledByUpdateRecord, "slow").(map[string]any)
	if _, tried := record["tried"]; record["enabled"] != true || record["reason"] != "still failing" || tried ||
		!reflect.DeepEqual(svList(plugins["disabled"]), []string{"other", "slow"}) {
		t.Fatalf("retry record %v %v", record, plugins)
	}

	_, plugins = edEdit(t, "plugins:\n  disabled:\n", func(p *yaml.Node) error {
		return disablePlugin(p, "fresh", edSpellings("fresh"), "too new", "")
	})
	if record, _ := svAt(plugins, DisabledByUpdateRecord, "fresh").(map[string]any); record["enabled"] != false ||
		!reflect.DeepEqual(svList(plugins["disabled"]), []string{"fresh"}) {
		t.Fatalf("never enabled plugin %v", plugins)
	}
}

func TestEditorRejoinUndoesOnlyARecordedDisable(t *testing.T) {
	config := "plugins:\n  enabled: [keep]\n  disabled: [slow, group/slow, mine]\n  disabled_by_update:\n    slow: {enabled: true, reason: r}\n    parked: {enabled: false, reason: r}\n"
	_, plugins := edEdit(t, config, func(p *yaml.Node) error {
		for _, name := range []string{"slow", "mine"} {
			if err := rejoinPlugin(p, name, edSpell(name, "group/"+name)); err != nil {
				return err
			}
		}
		return nil
	})
	if !reflect.DeepEqual(svList(plugins["enabled"]), []string{"keep", "slow"}) || !reflect.DeepEqual(svList(plugins["disabled"]), []string{"mine"}) {
		t.Fatalf("rejoined %v", plugins)
	}
	_, plugins = edEdit(t, "plugins:\n  disabled: [parked]\n  disabled_by_update:\n    parked: {enabled: false, reason: r}\n", func(p *yaml.Node) error {
		return rejoinPlugin(p, "parked", edSpell("parked"))
	})
	if _, has := plugins["enabled"]; has || len(svList(plugins["disabled"])) != 0 {
		t.Fatalf("a plugin disabled before the update came back on %v", plugins)
	}
}

func TestEditorSettleAndPruneForgetWhatTheUserDecided(t *testing.T) {
	config := "plugins:\n  disabled: [a, b, c]\n  disabled_by_update:\n    a: {enabled: true}\n    b: {enabled: true}\n    gone: {enabled: true}\n"
	_, plugins := edEdit(t, config, func(p *yaml.Node) error { return settleRecord(p, []any{"a", "missing"}) })
	record, _ := plugins[DisabledByUpdateRecord].(map[string]any)
	if _, kept := record["b"]; len(record) != 1 || !kept {
		t.Fatalf("record %v", record)
	}
	_, plugins = edEdit(t, config, func(p *yaml.Node) error { return settleRecord(p, []any{"a", "b"}) })
	if _, has := plugins[DisabledByUpdateRecord]; has {
		t.Fatalf("empty record kept %v", plugins)
	}
	_, plugins = edEdit(t, "plugins:\n  disabled: [a]\n  disabled_by_update: [a]\n", func(p *yaml.Node) error { return pruneRecord(p) })
	if _, has := plugins[DisabledByUpdateRecord]; has {
		t.Fatalf("malformed record kept %v", plugins)
	}
	_, plugins = edEdit(t, "plugins:\n  disabled: {a: 1}\n  disabled_by_update:\n    a: {enabled: true}\n    b: {enabled: true}\n", func(p *yaml.Node) error { return pruneRecord(p) })
	if record, _ := plugins[DisabledByUpdateRecord].(map[string]any); len(record) != 1 || record["a"] == nil {
		t.Fatalf("mapping disabled pruned %v", plugins)
	}
	_, plugins = edEdit(t, "plugins:\n  disabled: ab\n  disabled_by_update:\n    a: {enabled: true}\n    ab: {enabled: true}\n", func(p *yaml.Node) error { return pruneRecord(p) })
	if record, _ := plugins[DisabledByUpdateRecord].(map[string]any); len(record) != 1 || record["a"] == nil {
		t.Fatalf("string disabled pruned %v", plugins)
	}
	doc, _ := loadRoundtrip([]byte("plugins:\n  disabled: 5\n  disabled_by_update:\n    a: {enabled: true}\n"))
	if err := pruneRecord(rtGet(rtRoot(doc), "plugins")); err == nil || !strings.Contains(err.Error(), "'int' object is not iterable") {
		t.Fatalf("non iterable disabled %v", err)
	}
	if err := settleRecord(rtMapping(), []any{map[string]any{}}); err != nil {
		t.Fatalf("settling without a record raised %v", err)
	}
	doc, _ = loadRoundtrip([]byte(config))
	if err := settleRecord(rtGet(rtRoot(doc), "plugins"), []any{[]any{"a"}}); err == nil || !strings.Contains(err.Error(), "unhashable type: 'list'") {
		t.Fatalf("unhashable settled name %v", err)
	}
}

func TestEditorQuotesNamesYAMLWouldReadAsOtherTypes(t *testing.T) {
	for _, name := range []string{"on", "yes", "No", "null", "~", "1.0", "12", "2001-12-14", "<<", "="} {
		text, plugins := edEdit(t, "plugins:\n  enabled: []\n", func(p *yaml.Node) error {
			p.Content[1].Content = append(p.Content[1].Content, rtString(name))
			return nil
		})
		if got := svList(plugins["enabled"]); !reflect.DeepEqual(got, []string{name}) || !strings.Contains(text, "'"+name+"'") {
			t.Fatalf("%q dumped as %s", name, text)
		}
	}
	text, _ := edEdit(t, "plugins:\n  enabled: []\n", func(p *yaml.Node) error {
		p.Content[1].Content = append(p.Content[1].Content, rtString("plain-name"))
		return nil
	})
	if strings.Contains(text, "'plain-name'") || strings.Contains(text, "\"plain-name\"") {
		t.Fatalf("plain name quoted %s", text)
	}
}

func TestEditorOutcomeNoticesNameEveryOutcome(t *testing.T) {
	root := t.TempDir()
	entry := func(home, name string) PluginEntry {
		return PluginEntry{PluginsDir: filepath.Join(root, home, "plugins"), Name: name, PluginDir: filepath.Join(root, home, "plugins", name)}
	}
	evicted, waiting, joined, still, unknown := entry("a", "evicted"), entry("a", "waiting"), entry("b", "joined"), entry("b", "still"), entry("b", "unknown")
	notices := outcomeNotices([]PluginEntry{evicted, waiting, entry("a", "quiet")},
		[]recordedEviction{{Entry: joined}, {Entry: still, Record: map[string]any{"reason": "old reason"}}, {Entry: unknown, Record: map[string]any{"reason": ""}}},
		map[string]string{resolvePath(evicted.PluginDir): "conflicts"}, map[string]string{resolvePath(waiting.PluginDir): "requires icebath >=9"}, []PluginEntry{joined})
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	want := []string{
		"Disabled plugin 'evicted' in " + a + ": conflicts; a later update retries it and turns it back on once it fits, or run `icebath plugins enable evicted` sooner",
		"Left plugin 'waiting' in " + a + " out of this update: requires icebath >=9; it stays enabled and rejoins once icebath reports a version it accepts",
		"Turned plugin 'joined' in " + b + " back on: it fits this icebath again",
		"Plugin 'still' in " + b + " is still disabled: old reason; a later update retries it",
		"Plugin 'unknown' in " + b + " is still disabled: unknown; a later update retries it",
	}
	if !reflect.DeepEqual(notices, want) {
		t.Fatalf("notices\n%s", strings.Join(notices, "\n"))
	}
}

func TestEditorDisabledValuesFollowPythonMembership(t *testing.T) {
	_, plugins := edEdit(t, "plugins:\n  enabled: [slow]\n  disabled: slow-and-more\n", func(p *yaml.Node) error {
		return disablePlugin(p, "slow", edSpellings("slow"), "r", "")
	})
	if plugins["disabled"] != "slow-and-more" || len(svList(plugins["enabled"])) != 0 || svAt(plugins, DisabledByUpdateRecord, "slow", "enabled") != true {
		t.Fatalf("string disabled %v", plugins)
	}
	doc, _ := loadRoundtrip([]byte("plugins:\n  disabled: other\n"))
	if err := disablePlugin(rtGet(rtRoot(doc), "plugins"), "slow", edSpellings("slow"), "r", ""); err == nil || !strings.Contains(err.Error(), "'str' object has no attribute 'append'") {
		t.Fatalf("string without the name %v", err)
	}
	doc, _ = loadRoundtrip([]byte("plugins:\n  disabled: 5\n"))
	if err := disablePlugin(rtGet(rtRoot(doc), "plugins"), "slow", edSpellings("slow"), "r", ""); err == nil || !strings.Contains(err.Error(), "argument of type 'int' is not iterable") {
		t.Fatalf("int disabled %v", err)
	}
	_, plugins = edEdit(t, "plugins:\n  enabled: keep-slow\n  disabled: slow\n  disabled_by_update:\n    slow: {enabled: true}\n", func(p *yaml.Node) error {
		return rejoinPlugin(p, "slow", edSpell("slow"))
	})
	if plugins["disabled"] != "slow" || plugins["enabled"] != "keep-slow" {
		t.Fatalf("string lists on rejoin %v", plugins)
	}
	doc, _ = loadRoundtrip([]byte("plugins:\n  disabled: s\n  disabled_by_update:\n    s: {enabled: true}\n"))
	if err := rejoinPlugin(rtGet(rtRoot(doc), "plugins"), "s", edSpell("s")); err == nil || !strings.Contains(err.Error(), "doesn't support item deletion") {
		t.Fatalf("single letter string disabled %v", err)
	}
	text, _ := edEdit(t, "plugins:\n  disabled: {0: slow, 1: keep}\n  disabled_by_update:\n    slow: {enabled: false}\n", func(p *yaml.Node) error {
		return rejoinPlugin(p, "slow", edSpell("slow"))
	})
	if !strings.Contains(text, "disabled: {1: keep}") {
		t.Fatalf("index keyed mapping %s", text)
	}
	doc, _ = loadRoundtrip([]byte("plugins:\n  enabled: [keep, [nested]]\n  disabled: []\n"))
	if err := disablePlugin(rtGet(rtRoot(doc), "plugins"), "slow", edSpellings("slow"), "r", ""); !edPyError(err, "TypeError", "unhashable type: 'seq'") {
		t.Fatalf("unhashable enabled item %v", err)
	}
	doc, _ = loadRoundtrip([]byte("plugins:\n  disabled: {a: slow}\n  disabled_by_update:\n    slow: {enabled: false}\n"))
	if err := rejoinPlugin(rtGet(rtRoot(doc), "plugins"), "slow", edSpell("slow")); !edPyError(err, "KeyError", "0") {
		t.Fatalf("mapping without index keys %v", err)
	}
}

// Reports whether err carries the Python exception class and message
func edPyError(err error, class, message string) bool {
	var py *PyError
	return errors.As(err, &py) && py.Class == class && py.Message == message
}
