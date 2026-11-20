package plugins

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func p9Sidecar(t *testing.T, root, rows string) string {
	t.Helper()
	plugins := filepath.Join(root, "plugins")
	if err := os.MkdirAll(plugins, 0o755); err != nil {
		t.Fatal(err)
	}
	if rows != "" {
		if err := os.WriteFile(filepath.Join(plugins, ".install-metadata.json"), []byte(rows), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return plugins
}

func p9GitDir(t *testing.T, dir, origin string) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("[remote \"origin\"]\n\turl = "+origin+"\n"), 0o644)
}

func p9Manual(t *testing.T, dir string) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte("name: x\n"), 0o644)
}

func p9NoGit(t *testing.T) {
	backup := resolveGitExecutable
	resolveGitExecutable = func() string { return "" }
	t.Cleanup(func() { resolveGitExecutable = backup })
}

func TestProvenanceTwoByTwo(t *testing.T) {
	p9NoGit(t)
	ctx := context.Background()
	a40, b40, c40 := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	t.Run("git_install_row_and_git", func(t *testing.T) {
		plugins := p9Sidecar(t, t.TempDir(), `{"plug": {"pinned": false, "revision": "`+a40+`", "source": "https://example/o/r"}}`)
		p9GitDir(t, filepath.Join(plugins, "plug"), "https://example/o/r")
		prov := PluginsProvenance(ctx, plugins)
		if len(prov) != 1 || prov[0].Class != ProvenanceGit || prov[0].Row == nil || prov[0].Row["revision"] != a40 {
			t.Fatalf("%+v", prov)
		}
	})
	t.Run("manual_drop_neither", func(t *testing.T) {
		plugins := p9Sidecar(t, t.TempDir(), `{}`)
		p9Manual(t, filepath.Join(plugins, "handmade"))
		if prov := PluginsProvenance(ctx, plugins); prov[0].Class != ProvenanceManual {
			t.Fatalf("%+v", prov)
		}
	})
	t.Run("self_cloned_git_without_row", func(t *testing.T) {
		plugins := p9Sidecar(t, t.TempDir(), `{}`)
		p9GitDir(t, filepath.Join(plugins, "selfcloned"), "https://example/m/r")
		if prov := PluginsProvenance(ctx, plugins); prov[0].Class != ProvenanceSelfCloned || prov[0].OriginURL != "https://example/m/r" {
			t.Fatalf("%+v", prov)
		}
	})
	t.Run("drift_row_without_git", func(t *testing.T) {
		plugins := p9Sidecar(t, t.TempDir(), `{"ghost": {"pinned": false, "revision": "`+b40+`", "source": "https://example/g/r"}}`)
		p9Manual(t, filepath.Join(plugins, "ghost"))
		if prov := PluginsProvenance(ctx, plugins); prov[0].Class != ProvenanceDrift || prov[0].Row["source"] != "https://example/g/r" {
			t.Fatalf("%+v", prov)
		}
	})
	t.Run("missing_sidecar_is_all_manual_or_selfcloned", func(t *testing.T) {
		plugins := p9Sidecar(t, t.TempDir(), "")
		p9GitDir(t, filepath.Join(plugins, "cloned"), "https://example/o/r")
		p9Manual(t, filepath.Join(plugins, "dropped"))
		classes := map[string]ProvenanceClass{}
		for _, p := range PluginsProvenance(ctx, plugins) {
			classes[p.Name] = p.Class
		}
		if classes["cloned"] != ProvenanceSelfCloned || classes["dropped"] != ProvenanceManual {
			t.Fatalf("%v", classes)
		}
	})
	t.Run("update_url_tag_round_trips", func(t *testing.T) {
		plugins := p9Sidecar(t, t.TempDir(), `{"plug": {"pinned": false, "revision": "`+c40+`", "source": "https://example/o/r", "update_url": "https://example/feed.yml"}}`)
		p9GitDir(t, filepath.Join(plugins, "plug"), "https://example/o/r")
		if prov := PluginsProvenance(ctx, plugins); prov[0].SavedUpdateURL != "https://example/feed.yml" {
			t.Fatalf("%+v", prov)
		}
	})
}

func TestOriginURLUsesTheResolvedGitAndSurvivesNoGitAtAll(t *testing.T) {
	plugin := filepath.Join(t.TempDir(), "plugins", "p")
	p9GitDir(t, plugin, "https://example/from-config")
	backup := resolveGitExecutable
	t.Cleanup(func() { resolveGitExecutable = backup })
	resolveGitExecutable = func() string { return "" }
	if got := gitOriginURL(context.Background(), plugin); got != "https://example/from-config" {
		t.Fatalf("no git %q", got)
	}
	resolveGitExecutable = func() string { return "/nonexistent/resolved-git" }
	if got := gitOriginURL(context.Background(), plugin); got != "https://example/from-config" {
		t.Fatalf("missing resolved git %q", got)
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "spawned")
	script := filepath.Join(bin, "resolved-git")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$0\" >> "+log+"\nexit 1\n"), 0o755)
	resolveGitExecutable = func() string { return script }
	if got := gitOriginURL(context.Background(), plugin); got != "https://example/from-config" {
		t.Fatalf("failing resolved git %q", got)
	}
	if raw, _ := os.ReadFile(log); strings.TrimSpace(string(raw)) != script {
		t.Fatalf("spawned %q", raw)
	}
}
