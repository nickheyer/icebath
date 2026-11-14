package plugins

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/skills"
)

func TestCatalogPlatformMismatchRefusesBeforeInstall(t *testing.T) {
	w := catWorld(t)
	host := hostinfo.OSFamily()
	other := "windows"
	if host == "windows" {
		other = "linux"
	}
	entry := &PluginCatalogEntry{Name: "cat-plugin", Repo: "file://" + filepath.ToSlash(w.repo), Sha: w.sha1,
		Description: "d", Maintainer: "t", Platforms: []string{other}}
	_, _, _, err := installCatalogEntry(w.ctx, entry, CatalogInstallOptions{})
	var opErr *PluginOperationError
	if !errors.As(err, &opErr) || !regexp.MustCompile("cat-plugin.*"+host+".*"+other).MatchString(err.Error()) {
		t.Fatalf("mismatch %v", err)
	}
	if _, err := os.Stat(filepath.Join(w.pluginsDir, "cat-plugin")); !os.IsNotExist(err) {
		t.Fatalf("install left a directory %v", err)
	}
}

func TestOwnerRepoHashSubdirShorthandResolvesLikeTheCatalogSpelling(t *testing.T) {
	url, subdir, err := resolveGitURL("plastic-labs/honcho#hermes-plugin-honcho")
	if err != nil || url != "https://github.com/plastic-labs/honcho.git" || subdir != "hermes-plugin-honcho" {
		t.Fatalf("subdir shorthand %q %q %v", url, subdir, err)
	}
	url, subdir, err = resolveGitURL("owner/repo")
	if err != nil || url != "https://github.com/owner/repo.git" || subdir != "" {
		t.Fatalf("owner repo %q %q %v", url, subdir, err)
	}
}

func TestInTreeSidecarCannotForgeCatalogProvenance(t *testing.T) {
	w := catWorld(t)
	evil := filepath.Join(w.root, "evil")
	c6Write(t, filepath.Join(evil, "plugin.yaml"), "name: evil-plugin\nversion: 1.0.0\ndescription: d\n")
	c6Write(t, filepath.Join(evil, "helper.py"), "def register(ctx):\n    pass\n")
	c6Write(t, filepath.Join(evil, CatalogSidecar), `{"catalog_name": "cat-plugin", "tier": "official", "sha": "`+strings.Repeat("0", 40)+`", "repo": "x"}`)
	c6Git(t, evil, "init", "-q")
	c6Commit(t, evil, "v1")
	target, _, _, err := installPluginCore(w.ctx, "file://"+filepath.ToSlash(evil), installCoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, CatalogSidecar)); err != nil {
		t.Fatalf("the shipped sidecar is gone %v", err)
	}
	if sidecar := noErr(readCatalogSidecar(w.h, target)); sidecar != nil {
		t.Fatalf("forged sidecar read %v", sidecar)
	}
	if got := noErr(catalogAnnotation(w.h, target)); got != "" {
		t.Fatalf("annotation %q", got)
	}
	if fields := noErr(CatalogRowFields(w.h, target, CatalogPins(w.ctx), nil)); len(fields) != 0 {
		t.Fatalf("row fields %v", fields)
	}
	state := noErr(InstalledCatalogState(w.ctx, map[string]map[string]any{"evil-plugin": {"dir": target, "runtime_status": nil}}, []string{"evil-plugin"}))
	entries, _ := state["entries"].([]any)
	if len(entries) == 0 || entries[0].(map[string]any)["installed"] != false {
		t.Fatalf("state %v", state)
	}
	real, _, _, err := installCatalogEntry(w.ctx, GetLiveCatalogEntry(w.ctx, "cat-plugin"), CatalogInstallOptions{})
	if err != nil || noErr(catalogAnnotation(w.h, real)) != "catalog:community@"+w.sha1[:8] {
		t.Fatalf("real catalog install %v %q", err, noErr(catalogAnnotation(w.h, real)))
	}
}

func TestRefInstallRecordsInstalledShaSoUpdateIsOffered(t *testing.T) {
	w := catWorld(t)
	target, _, _, err := installCatalogEntry(w.ctx, GetLiveCatalogEntry(w.ctx, "cat-plugin"), CatalogInstallOptions{Ref: w.sha2})
	if err != nil || catHead(t, target) != w.sha2 {
		t.Fatalf("ref install %v", err)
	}
	if sidecar := noErr(readCatalogSidecar(w.h, target)); sidecar == nil || sidecar["sha"] != w.sha2 {
		t.Fatalf("sidecar %v", sidecar)
	}
	if fields := noErr(CatalogRowFields(w.h, target, CatalogPins(w.ctx), nil)); fields["update_available"] != true {
		t.Fatalf("row fields %v", fields)
	}
	result, err := DashboardUpdateUserPlugin(w.ctx, "cat-plugin", false, false)
	if err != nil || result["unchanged"] != false || catHead(t, target) != w.sha1 {
		t.Fatalf("update %v %v", result, err)
	}
}

func TestAnnotatedTagPinKeepsReviewedTrustAndReadsAsAtPin(t *testing.T) {
	w := catWorld(t)
	c6Git(t, w.repo, "tag", "-a", "v1", w.sha1, "-m", "v1")
	tagObject := c6Git(t, w.repo, "rev-parse", "v1")
	if tagObject == w.sha1 {
		t.Fatal("the tag object is its own sha")
	}
	w.pin = tagObject
	seen := map[string]bool{}
	realScan := scanPluginTree
	t.Cleanup(func() { scanPluginTree = realScan })
	scanPluginTree = func(h, pluginDir, identifier string, opts scanOptions) (*skills.ScanResult, error) {
		seen["reviewed_pin"] = opts.ReviewedPin
		return realScan(h, pluginDir, identifier, opts)
	}
	target, _, _, err := installCatalogEntry(w.ctx, GetLiveCatalogEntry(w.ctx, "cat-plugin"), CatalogInstallOptions{})
	if err != nil || catHead(t, target) != w.sha1 || !seen["reviewed_pin"] {
		t.Fatalf("install %v %v", err, seen)
	}
	if sidecar := noErr(readCatalogSidecar(w.h, target)); sidecar == nil || sidecar["sha"] != w.sha1 || sidecar["pin"] != tagObject {
		t.Fatalf("sidecar %v", sidecar)
	}
	if fields := noErr(CatalogRowFields(w.h, target, CatalogPins(w.ctx), nil)); fields["update_available"] != false {
		t.Fatalf("row fields %v", fields)
	}
	state := noErr(InstalledCatalogState(w.ctx, map[string]map[string]any{"cat-plugin": {"dir": target, "runtime_status": nil}}, []string{"cat-plugin"}))
	if entries, _ := state["entries"].([]any); len(entries) == 0 || entries[0].(map[string]any)["update_available"] != false {
		t.Fatalf("state %v", state)
	}
	if result, err := DashboardUpdateUserPlugin(w.ctx, "cat-plugin", false, false); err != nil || result["unchanged"] != true {
		t.Fatalf("update %v %v", result, err)
	}
	w.pin = w.sha2
	if fields := noErr(CatalogRowFields(w.h, target, CatalogPins(w.ctx), nil)); fields["update_available"] != true {
		t.Fatalf("bumped row fields %v", fields)
	}
}

func TestCatalogRowsMapsResolvesTheLiveCatalogOnce(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	savedURL, savedClient, savedTree := LiveCatalogURL, liveCatalogClient, inTreeCatalogEntries
	t.Cleanup(func() { LiveCatalogURL, liveCatalogClient, inTreeCatalogEntries = savedURL, savedClient, savedTree })
	LiveCatalogURL = "http://127.0.0.1:9/nope"
	liveCatalogClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })}
	calls := 0
	inTreeCatalogEntries = func() []*PluginCatalogEntry {
		calls++
		return []*PluginCatalogEntry{
			{Name: "a", Repo: "https://github.com/o/r", Sha: strings.Repeat("a", 40), Description: "d", Maintainer: "t", Version: "1.2.3", Title: "Alpha"},
			{Name: "b", Repo: "https://github.com/o/r2", Sha: strings.Repeat("b", 40), Description: "d", Maintainer: "t"},
		}
	}
	ctx := home.WithOverride(context.Background(), os.Getenv("ICEBATH_HOME"))
	pins, versions, titles := CatalogRowsMaps(ctx)
	if !reflect.DeepEqual(pins, map[string]string{"a": strings.Repeat("a", 40), "b": strings.Repeat("b", 40)}) ||
		!reflect.DeepEqual(versions, map[string]string{"a": "1.2.3"}) || !reflect.DeepEqual(titles, map[string]string{"a": "Alpha"}) {
		t.Fatalf("maps %v %v %v", pins, versions, titles)
	}
	if calls != 1 {
		t.Fatalf("catalog resolved %d times", calls)
	}
	inTreeCatalogEntries = func() []*PluginCatalogEntry { panic(errors.New("catalog unavailable")) }
	pins, versions, titles = CatalogRowsMaps(ctx)
	if len(pins) != 0 || len(versions) != 0 || len(titles) != 0 {
		t.Fatalf("failure maps %v %v %v", pins, versions, titles)
	}
}
