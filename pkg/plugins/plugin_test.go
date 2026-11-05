package plugins

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type fakeBundled struct{ m Manifest }

func (f fakeBundled) Manifest() Manifest      { return f.m }
func (f fakeBundled) Register(*Context) error { return nil }

func TestRegisterBundledManifestsTaggedBundled(t *testing.T) {
	bundledMu.Lock()
	savedByKey, savedOrder := bundledByKey, bundledOrder
	bundledByKey, bundledOrder = map[string]Plugin{}, nil
	bundledMu.Unlock()
	t.Cleanup(func() {
		bundledMu.Lock()
		bundledByKey, bundledOrder = savedByKey, savedOrder
		bundledMu.Unlock()
	})
	RegisterBundled(fakeBundled{Manifest{Name: "alpha", Path: "/somewhere", Source: "user"}})
	RegisterBundled(fakeBundled{Manifest{Name: "beta", Key: "web/beta", Kind: "backend"}})
	got := bundledManifests()
	if len(got) != 2 || got[0].Key != "alpha" || got[0].Source != "bundled" || got[0].Path != "" || got[0].Kind != "standalone" {
		t.Fatalf("first manifest %+v", got[0])
	}
	if got[1].Key != "web/beta" || got[1].Kind != "backend" {
		t.Fatalf("second manifest %+v", got[1])
	}
	if _, ok := bundledPlugin("web/beta"); !ok {
		t.Fatal("bundled lookup by key failed")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate registration did not panic")
		}
	}()
	RegisterBundled(fakeBundled{Manifest{Name: "alpha"}})
}

func TestFindRemovedMatchesNameOrNormalizedRepo(t *testing.T) {
	dir := t.TempDir()
	doc := "removed:\n- name: evil\n  repo: https://github.com/x/evil.git\n  reason: malware\n  date: '2026-01-01'\n"
	if err := os.WriteFile(filepath.Join(dir, "removed.yaml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := loadRemovedList(dir)
	if r := matchRemoved("evil", entries); r == nil || r.Reason != "malware" {
		t.Fatalf("name match %+v", r)
	}
	if matchRemoved("https://github.com/x/EVIL/", entries) == nil {
		t.Fatal("case and slash insensitive repo match failed")
	}
	if matchRemoved("https://github.com/x/fine", entries) != nil {
		t.Fatal("unrelated repo matched")
	}
	for _, spelling := range []string{"git@github.com:x/evil.git", "ssh://git@github.com/x/evil", "http://github.com/x/evil",
		"https://www.github.com/x/evil/", "git://github.com/x/evil.git"} {
		if matchRemoved(spelling, entries) == nil {
			t.Fatalf("spelling %s not blocked", spelling)
		}
	}
	if matchRemoved("git@gitlab.com:x/evil.git", entries) != nil {
		t.Fatal("different host matched")
	}
}

func TestInTreeRemovedListEmbedded(t *testing.T) {
	if matchRemoved("or-picker", loadRemovedList("")) == nil {
		t.Fatal("embedded removed.yaml entry missing")
	}
}

func TestInstalledRemovalHonoursCachedLiveListAndAllowRemoved(t *testing.T) {
	h := t.TempDir()
	target := filepath.Join(h, "plugins", "later-killed")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON := func(path string, v any) {
		raw, _ := json.Marshal(v)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeJSON(liveCachePath(h), map[string]any{"entries": []any{}, "removed": []any{map[string]any{"name": "killed", "reason": "backdoor"}}})
	meta := filepath.Join(h, "plugins", ".install-metadata.json")
	writeJSON(meta, map[string]any{"later-killed": map[string]any{"source": "file:///x/later-killed#main", "catalog": map[string]any{"name": "killed"}}})
	m := &Manifest{Name: "killed", Source: "user", Path: target, Key: "killed"}
	gate := GateManifest(h, m, map[string]bool{}, map[string]bool{"killed": true}, true)
	if gate.Action == "load" || gate.Error == "" {
		t.Fatalf("removed plugin gated %+v", gate)
	}
	writeJSON(meta, map[string]any{"later-killed": map[string]any{"source": "file:///x/later-killed", "allow_removed": true}})
	if gate := GateManifest(h, m, map[string]bool{}, map[string]bool{"killed": true}, true); gate.Action != "load" {
		t.Fatalf("allow_removed bypass not honoured %+v", gate)
	}
	writeJSON(meta, []any{1})
	if r := InstalledPluginRemoval(h, "other", target); r != nil {
		t.Fatalf("malformed metadata produced a verdict %+v", r)
	}
	if r := InstalledPluginRemoval(h, "other", filepath.Join(h, "elsewhere", "killed")); r == nil {
		t.Fatal("directory name candidate not matched")
	}
}
