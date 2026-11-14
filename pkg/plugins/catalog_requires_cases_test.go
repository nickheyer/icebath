package plugins

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallProceedsWhenHermesSatisfiesTheSpecOrItsVersionIsUnknown(t *testing.T) {
	for _, tc := range []struct{ running, spec string }{{"0.30.0", ">=0.30"}, {"unknown", ">=99.0"}} {
		t.Run(tc.running, func(t *testing.T) {
			w := catWorld(t)
			RunningVersion, w.requires = tc.running, tc.spec
			if problem := RequiresVersionError(tc.spec); problem != "" {
				t.Fatalf("spec refused %q", problem)
			}
			result, err := DashboardInstallPlugin(w.ctx, DashboardInstallOptions{CatalogName: "cat-plugin"})
			if err != nil || result["ok"] != true {
				t.Fatalf("install %v %v", result, err)
			}
			if catHead(t, filepath.Join(w.pluginsDir, "cat-plugin")) != w.sha1 {
				t.Fatal("installed at another commit")
			}
		})
	}
}

func TestPortableCatalogEntryGetsItsHermesFloorFromTheCatalog(t *testing.T) {
	w := catWorld(t)
	RunningVersion = "0.30.0"
	const refusal = "cat-plugin needs Icebath >=0.31; this is 0.30.0."
	repo := filepath.Join(w.root, "portable-repo")
	shas := []string{}
	for i, version := range []string{"1.0.0", "2.0.0"} {
		manifest, _ := json.Marshal(map[string]any{"$schema": PluginSchemaV1, "name": "cat-plugin", "version": version})
		c6Write(t, filepath.Join(repo, "plugin.json"), string(manifest))
		if i == 0 {
			c6Git(t, repo, "init", "-q")
		}
		shas = append(shas, c6Commit(t, repo, version))
	}
	w.repo, w.pin, w.requires = repo, shas[0], ">=0.31"
	target := filepath.Join(w.pluginsDir, "cat-plugin")
	refused, err := DashboardInstallPlugin(w.ctx, DashboardInstallOptions{CatalogName: "cat-plugin"})
	if err != nil || refused["ok"] != false || !strings.Contains(refused["error"].(string), refusal) || pathExists(target) {
		t.Fatalf("install below the catalog floor %v %v", refused, err)
	}
	w.requires = ""
	if installed, err := DashboardInstallPlugin(w.ctx, DashboardInstallOptions{CatalogName: "cat-plugin"}); err != nil || installed["ok"] != true {
		t.Fatalf("install %v %v", installed, err)
	}
	w.pin, w.requires = shas[1], ">=0.31"
	held, err := DashboardUpdateUserPlugin(w.ctx, "cat-plugin", false, false)
	if err != nil || held["ok"] != false || !strings.Contains(held["error"].(string), refusal) {
		t.Fatalf("update below the catalog floor %v %v", held, err)
	}
	if catHead(t, target) != shas[0] {
		t.Fatal("the installed pin moved")
	}
}

func TestInstallSummaryShowsKnownIssues(t *testing.T) {
	w := catWorld(t)
	const issue = "Local embedded mode is not supported on PM-managed Icebath."
	repo := w.repo
	inTreeCatalogEntries = func() []*PluginCatalogEntry {
		return []*PluginCatalogEntry{{Name: "cat-plugin", Repo: "file://" + filepath.ToSlash(repo), Sha: w.sha1, Description: "d",
			Maintainer: "t", Tier: "community", Category: "memory", KnownIssues: []string{issue}}}
	}
	result, err := DashboardInstallPlugin(w.ctx, DashboardInstallOptions{CatalogName: "cat-plugin"})
	if err != nil || result["ok"] != true {
		t.Fatalf("install %v %v", result, err)
	}
	if issues, _ := result["known_issues"].([]any); len(issues) != 1 || issues[0] != issue {
		t.Fatalf("known_issues %v", result["known_issues"])
	}
	warned := false
	for _, warning := range result["warnings"].([]any) {
		warned = warned || strings.HasPrefix(warning.(string), "Known issue:")
	}
	if !warned {
		t.Fatalf("warnings %v", result["warnings"])
	}
	entry := inTreeCatalogEntries()[0]
	if summary := EntryCapabilitySummary(entry); !strings.Contains(summary, "Known issues:") || !strings.Contains(summary, issue) {
		t.Fatalf("summary %q", summary)
	}
}
