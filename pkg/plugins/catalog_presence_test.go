package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const presenceSHA = "0000000000000000000000000000000000000000"

func presenceNeedsApp(location string, extra map[string]any) map[string]any {
	app := map[string]any{}
	for _, osf := range []string{"darwin", "linux"} {
		app[osf] = map[string]any{"presence": "executable", "location": location}
	}
	app["win32"] = map[string]any{"presence": "executable", "location": "C:/nonexistent/fx-app.exe"}
	srv := map[string]any{"app": app, "requires": map[string]any{"app": true}}
	for k, v := range extra {
		srv[k] = v
	}
	return map[string]any{"extensions": map[string]any{"com.nousresearch.hermes": map[string]any{"servers": map[string]any{"srv": srv}}}}
}

func presenceEntry(t *testing.T, name string, onboarding bool, platforms []string, title, description string) *PluginCatalogEntry {
	t.Helper()
	if description == "" {
		description = name + " does things. More."
	}
	plats := []any{}
	for _, p := range platforms {
		plats = append(plats, p)
	}
	e := EntryFromMapping(map[string]any{"name": name, "repo": "https://github.com/fx/" + name, "sha": presenceSHA,
		"description": description, "maintainer": "fx", "tier": "official", "category": "tools", "platforms": plats,
		"onboarding": onboarding, "title": title}, name)
	if e == nil {
		t.Fatalf("entry %s rejected", name)
	}
	return e
}

func presenceHereOther() (string, string) {
	here := map[string]string{"darwin": "macos", "win32": "windows"}[hostinfo.OSFamily()]
	if here == "" {
		here = "linux"
	}
	other := "windows"
	if here == "windows" {
		other = "macos"
	}
	return here, other
}

// Serves pinned manifests by repo name, a nil value dropping the connection and an absent name answering 404
func presenceWorld(t *testing.T, entries []*PluginCatalogEntry, manifests map[string]any, gate func()) *atomic.Int64 {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if gate != nil {
			gate()
		}
		parts := strings.Split(r.URL.Path, "/")
		m, ok := manifests[parts[2]]
		switch {
		case !ok:
			http.NotFound(w, r)
		case m == nil:
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		default:
			_ = json.NewEncoder(w).Encode(m)
		}
	}))
	t.Cleanup(srv.Close)
	prevBase, prevCatalog := presenceRawBase, presenceCatalog
	presenceRawBase = srv.URL
	presenceCatalog = func(context.Context) []*PluginCatalogEntry { return entries }
	presenceCache.Lock()
	presenceCache.manifests = map[[3]string]*pyjson.Object{}
	presenceCache.Unlock()
	t.Cleanup(func() { presenceRawBase, presenceCatalog = prevBase, prevCatalog })
	return &hits
}

func onboardingFixture(t *testing.T) {
	here, other := presenceHereOther()
	entries := []*PluginCatalogEntry{presenceEntry(t, "everywhere", true, nil, "Everywhere App", ""),
		presenceEntry(t, "not-curated", false, nil, "", ""), presenceEntry(t, "here-only", true, []string{here}, "", ""),
		presenceEntry(t, "elsewhere", true, []string{other}, "", "")}
	presenceWorld(t, entries, map[string]any{"everywhere": presenceNeedsApp("/nonexistent/fx-app", nil),
		"here-only": map[string]any{"name": "here-only"}}, nil)
}

func TestOnboardingRowsAreCuratedEntriesThisOSRuns(t *testing.T) {
	onboardingFixture(t)
	rows, err := OnboardingEntries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]OnboardingEntry{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	if len(byName) != 2 || byName["everywhere"].Title != "Everywhere App" || byName["here-only"].Title != "here-only" {
		t.Fatalf("rows %+v", rows)
	}
	if names := OnboardingNames(context.Background()); strings.Join(names, ",") != "everywhere,here-only" {
		t.Fatalf("names %v", names)
	}
	if byName["everywhere"].Description != "everywhere does things." {
		t.Fatalf("description %q", byName["everywhere"].Description)
	}
}

func TestAppStateComesFromThePinnedDeclaration(t *testing.T) {
	onboardingFixture(t)
	rows, err := OnboardingEntries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]OnboardingEntry{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	if r := byName["everywhere"]; r.AppState != "missing_app" || !strings.Contains(r.Sentence, "Everywhere App") {
		t.Fatalf("everywhere %+v", r)
	}
	if r := byName["here-only"]; r.AppState != "unknown" || r.Sentence != "" {
		t.Fatalf("here-only %+v", r)
	}
}

// Every fetch waits for the other two so a serial reader breaks the barrier and reads nothing
func presenceByNameFixture(t *testing.T) *atomic.Int64 {
	here, other := presenceHereOther()
	entries := []*PluginCatalogEntry{
		presenceEntry(t, "app", false, []string{here}, "Fx App", "Drives Fx. Disclosure: the tools change Fx settings; nothing leaves loopback. Needs Fx 2."),
		presenceEntry(t, "dashed", false, nil, "", "Pane. Disclosure — reads the cron file under ICEBATH_HOME."),
		presenceEntry(t, "broken", false, nil, "Broken", ""),
		presenceEntry(t, "elsewhere", false, []string{other}, "", ""),
	}
	var mu sync.Mutex
	arrived, release := 0, make(chan struct{})
	gate := func() {
		mu.Lock()
		arrived++
		if arrived == 3 {
			close(release)
		}
		mu.Unlock()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
	}
	return presenceWorld(t, entries, map[string]any{"app": presenceNeedsApp("/nonexistent/fx-app", nil),
		"dashed": map[string]any{"name": "dashed"}, "broken": nil}, gate)
}

func TestRowsInTheOrderAskedWithoutRemovedOrOtherOSNames(t *testing.T) {
	presenceByNameFixture(t)
	rows, err := PresenceFor(context.Background(), []string{"broken", "removed-from-catalog", "elsewhere", "app", "dashed", "app"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range rows {
		names = append(names, r.Name)
	}
	if strings.Join(names, ",") != "broken,app,dashed" {
		t.Fatalf("names %v", names)
	}
}

func TestRowShapeStateAndDisclosure(t *testing.T) {
	presenceByNameFixture(t)
	rows, err := PresenceFor(context.Background(), []string{"app", "dashed", "broken"})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]NamedPresence{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	want := NamedPresence{Name: "app", Title: "Fx App", State: "missing_app", Sentence: "needs Fx App",
		Disclosure: "the tools change Fx settings; nothing leaves loopback."}
	if byName["app"] != want {
		t.Fatalf("app %+v", byName["app"])
	}
	if d := byName["dashed"]; d.Title != "dashed" || d.Disclosure != "reads the cron file under ICEBATH_HOME." || d.State != "unknown" {
		t.Fatalf("dashed %+v", d)
	}
	if b := byName["broken"]; b.State != "unknown" || b.Sentence != "" || b.Disclosure != "" {
		t.Fatalf("broken %+v", b)
	}
	raw, _ := json.Marshal(byName["app"])
	if string(raw) != `{"name":"app","title":"Fx App","state":"missing_app","sentence":"needs Fx App","disclosure":"the tools change Fx settings; nothing leaves loopback."}` {
		t.Fatalf("wire %s", raw)
	}
}

func TestPinnedManifestRemembersAnswersButNotTransientFailures(t *testing.T) {
	ctx := context.Background()
	big := map[string]any{"pad": strings.Repeat("x", presenceMaxBytes)}
	hits := presenceWorld(t, nil, map[string]any{"ok": map[string]any{"name": "ok"}, "drop": nil, "big": big}, nil)
	if pinnedManifest(ctx, "https://github.com/fx/drop", presenceSHA, "") != nil {
		t.Fatal("a dropped connection reads no manifest")
	}
	before := hits.Load()
	if pinnedManifest(ctx, "https://github.com/fx/drop", presenceSHA, "") != nil || hits.Load() == before {
		t.Fatal("a dropped connection must not be remembered")
	}
	for _, tc := range []struct {
		repo    string
		present bool
		hits    int64
	}{{"ok", true, 1}, {"missing", false, 1}, {"big", false, 2}} {
		before := hits.Load()
		for range 2 {
			got := pinnedManifest(ctx, "https://github.com/fx/"+tc.repo+".git", presenceSHA, "")
			if (got != nil) != tc.present {
				t.Fatalf("%s present %v", tc.repo, got != nil)
			}
		}
		if n := hits.Load() - before; n != tc.hits {
			t.Fatalf("%s fetched %d times", tc.repo, n)
		}
	}
	before = hits.Load()
	if pinnedManifest(ctx, "https://gitlab.com/fx/ok", presenceSHA, "") != nil || hits.Load() != before {
		t.Fatal("a repo off github must not be fetched")
	}
	if u := rawManifestURL(" https://github.com/fx/repo.git/ ", "abc", "/sub/dir/"); u != presenceRawBase+"/fx/repo/abc/sub/dir/plugin.json" {
		t.Fatalf("url %s", u)
	}
}

func presenceLivenessParser(raw any) (ServerLiveness, error) {
	m := raw.(map[string]any)
	switch m["kind"] {
	case "static", "interactive_session":
		return ServerLiveness{Kind: m["kind"].(string)}, nil
	case "server_json":
		return ServerLiveness{Kind: "server_json", Path: m["path"].(string), URLField: "http", TokenField: "token", PIDField: "pid"}, nil
	}
	return ServerLiveness{}, errors.New("unknown liveness kind")
}

func TestServerPresenceFollowsTheDeclaredLiveness(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "fx-app")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	session := Presence{"app_not_running", "Fx needs an interactive desktop session"}
	if hostinfo.InteractiveSession() {
		session = Presence{State: "present"}
	}
	cases := map[string]struct {
		liveness map[string]any
		want     Presence
	}{
		"none":        {nil, Presence{State: "present"}},
		"static":      {map[string]any{"kind": "static"}, Presence{State: "present"}},
		"interactive": {map[string]any{"kind": "interactive_session"}, session},
		"server_json": {map[string]any{"kind": "server_json", "path": filepath.Join(t.TempDir(), "server.json")}, Presence{"app_not_running", "Fx is not running"}},
		"invalid":     {map[string]any{"kind": "bogus"}, Presence{State: "present"}},
	}
	manifests := map[string]any{}
	var entries []*PluginCatalogEntry
	for name, tc := range cases {
		extra := map[string]any{}
		if tc.liveness != nil {
			extra["liveness"] = tc.liveness
		}
		manifests[name] = presenceNeedsApp(exe, extra)
		entries = append(entries, presenceEntry(t, name, true, nil, "Fx", ""))
	}
	manifests["fields"] = presenceNeedsApp(exe, map[string]any{"liveness": map[string]any{"kind": "server_json", "path": "x", "fields": []any{1}}})
	presenceWorld(t, entries, manifests, nil)
	prev := installedServices()
	t.Cleanup(func() { InstallServices(prev) })
	InstallServices(&Services{ParseServerLiveness: presenceLivenessParser})
	for i, entry := range entries {
		got, err := CatalogPresence(context.Background(), entry)
		if err != nil || got != cases[entry.Name].want {
			t.Fatalf("%d %s got %+v %v", i, entry.Name, got, err)
		}
	}
	fields := presenceEntry(t, "fields", true, nil, "Fx", "")
	if _, err := CatalogPresence(context.Background(), fields); err == nil || err.Error() != "'list' object is not a mapping" {
		t.Fatalf("fields error %v", err)
	}
	InstallServices(&Services{})
	var missing *ServiceMissingError
	if _, err := CatalogPresence(context.Background(), presenceEntry(t, "static", true, nil, "Fx", "")); !errors.As(err, &missing) {
		t.Fatalf("missing service %v", err)
	}
}

func TestDisclosureAndFirstSentence(t *testing.T) {
	for in, want := range map[string]string{"A b.  C d. E": "A b.", "no stop": "no stop", "x\n y.\tz": "x y."} {
		if got := firstSentence(in); got != want {
			t.Fatalf("first sentence %q -> %q", in, got)
		}
	}
	if got := disclosureOf("Thing. Disclosure:   writes\nfiles. More."); got != "writes files." {
		t.Fatalf("disclosure %q", got)
	}
	if got := disclosureOf("NoDisclosure: hidden"); got != "" {
		t.Fatalf("boundary %q", got)
	}
}
