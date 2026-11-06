package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

func catalogEntry(name string, platforms []string, requiresEnv ...string) *plugins.PluginCatalogEntry {
	return &plugins.PluginCatalogEntry{Name: name, Repo: "https://github.com/example/" + name, Sha: strings.Repeat("a", 40),
		Tier: "official", Description: "Drives " + name + ". More text.", Platforms: platforms,
		Capabilities: plugins.CatalogCapabilities{RequiresEnv: requiresEnv}}
}

// Catalog, platform refusal and installer of a row, recording every install with its home
type catalogFake struct {
	mu           sync.Mutex
	entries      map[string]*plugins.PluginCatalogEntry
	refusal      map[string]string
	installError string
	installs     []map[string]any
	clawhub      bool
}

func newCatalogFake(entries ...*plugins.PluginCatalogEntry) *catalogFake {
	f := &catalogFake{entries: map[string]*plugins.PluginCatalogEntry{}, refusal: map[string]string{}}
	for _, e := range entries {
		f.entries[e.Name] = e
	}
	return f
}

func (f *catalogFake) PluginEntry(_ context.Context, name string) (*plugins.PluginCatalogEntry, error) {
	return f.entries[name], nil
}

func (f *catalogFake) Refuse(_ context.Context, entry *plugins.PluginCatalogEntry) error {
	if msg, ok := f.refusal[entry.Name]; ok {
		return errors.New(msg)
	}
	return nil
}

func (f *catalogFake) InstallPlugin(ctx context.Context, name string, force, enable bool, ref string, _ func(plugins.InstallPhase)) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var refValue any
	if ref != "" {
		refValue = ref
	}
	f.installs = append(f.installs, map[string]any{"name": name, "force": force, "enable": enable, "ref": refValue, "home": home.Dir(ctx)})
	if f.installError != "" {
		return map[string]any{"ok": false, "error": f.installError}, nil
	}
	toolNames := []any{"mcp__" + name + "__status", "mcp__" + name + "__launch"}
	return map[string]any{"ok": true, "plugin_name": name, "missing_env": []any{}, "activation": map[string]any{
		"live_now": map[string]any{"mcp_servers": []any{map[string]any{"name": name, "connected": true, "tools": toolNames}},
			"skills": []any{}}}}, nil
}

func (f *catalogFake) SkillMeta(_ context.Context, identifier string) (*CatalogSkillMeta, error) {
	if f.clawhub {
		return &CatalogSkillMeta{Name: "Agent Git Oracle", Description: "Repository analysis.", Source: "clawhub", Identifier: identifier}, nil
	}
	return nil, nil
}

func (f *catalogFake) InstallSkill(_ context.Context, identifier string, _ bool, source string) (map[string]any, error) {
	if !f.clawhub {
		panic("no skill in these tests")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installs = append(f.installs, map[string]any{"identifier": identifier, "source": source})
	return map[string]any{"name": identifier, "already_installed": false}, nil
}

func (f *catalogFake) installList() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.installs...)
}

func (f *catalogFake) setInstallError(msg string) {
	f.mu.Lock()
	f.installError = msg
	f.mu.Unlock()
}

// Isolated launch home with the tui gateway profile hooks bound the way server.py resolves profiles
func catalogProfile(t *testing.T, name string) string {
	t.Helper()
	dir, err := profiles.GetProfileDir(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

type catalogCardRec struct {
	mu   sync.Mutex
	seen []map[string]any
}

func (c *catalogCardRec) payloads() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.seen...)
}

// Desktop card that answers the live operation a moment after it is drawn, looked up under profileHome
func catalogCard(ctx context.Context, profileHome string, answer func(payload map[string]any) map[string]any) (*catalogCardRec, tools.ConnCallback) {
	card := &catalogCardRec{}
	return card, func(payload *pyjson.Object) string {
		plain := plainJSON(payload)
		card.mu.Lock()
		card.seen = append(card.seen, plain)
		card.mu.Unlock()
		time.AfterFunc(10*time.Millisecond, func() {
			if op := tools.LiveGet("s1", plain["op_id"].(string), profileHome); op != nil {
				raw, _ := json.Marshal(answer(plain))
				_ = ApplyConnAnswer(ctx, op, string(raw))
			}
		})
		return ""
	}
}

func catalogApprove(env map[string]any) func(map[string]any) map[string]any {
	return func(payload map[string]any) map[string]any {
		var rows []any
		for _, raw := range payload["targets"].([]any) {
			row := raw.(map[string]any)
			if row["state"] == "pending" {
				rows = append(rows, map[string]any{"name": row["name"], "status": "approved", "env": env})
			}
		}
		return map[string]any{"targets": rows}
	}
}

func catalogItems(pairs ...string) []any {
	var items []any
	for i := 0; i+1 < len(pairs); i += 2 {
		items = append(items, map[string]any{"kind": pairs[i], "id": pairs[i+1]})
	}
	return items
}

func catalogRun(t *testing.T, ctx context.Context, args map[string]any, cb tools.ConnCallback, surface bool, installer CatalogInstaller) map[string]any {
	t.Helper()
	out, err := ManageCatalog(ctx, nil, args, CatalogOptions{SessionID: "s1", Callback: cb, CardSurface: surface, Installer: installer})
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("%v in %s", err, out)
	}
	return parsed
}

func catalogInstallRun(t *testing.T, ctx context.Context, items []any, installer CatalogInstaller, cb tools.ConnCallback) map[string]any {
	return catalogRun(t, ctx, map[string]any{"action": "install", "items": items}, cb, true, installer)
}

func catalogRows(out map[string]any) []map[string]any {
	var rows []map[string]any
	for _, r := range out["targets"].([]any) {
		rows = append(rows, r.(map[string]any))
	}
	return rows
}

func TestCatalogSourceVersionOrProfileFromTheModelIsRefusedBeforeAnythingRuns(t *testing.T) {
	ctx, _ := catalogInstallFixture(t)
	for _, args := range []map[string]any{
		{"action": "install", "items": catalogItems("plugin", "x"), "profile": "work"},
		{"action": "install", "items": []any{map[string]any{"kind": "plugin", "id": "x", "sha": strings.Repeat("b", 40)}}},
		{"action": "install", "items": []any{map[string]any{"kind": "plugin", "id": "x", "url": "https://evil.example/x"}}},
	} {
		installer := newCatalogFake(catalogEntry("x", nil))
		card, cb := catalogCard(ctx, "", catalogApprove(nil))
		out := catalogRun(t, ctx, args, cb, true, installer)
		if _, ok := out["error"]; !ok || len(card.payloads()) != 0 || len(installer.installList()) != 0 {
			t.Fatalf("%v -> %v", args, out)
		}
	}
}

func TestCatalogOffTheDesktopPointsAtTheCLIAndOpensNoCard(t *testing.T) {
	ctx, _ := catalogInstallFixture(t)
	installer := newCatalogFake(catalogEntry("x", nil))
	card, cb := catalogCard(ctx, "", catalogApprove(nil))
	for _, c := range []struct {
		cb      tools.ConnCallback
		surface bool
	}{{nil, true}, {cb, false}} {
		out := catalogRun(t, ctx, map[string]any{"action": "install", "items": catalogItems("plugin", "x")}, c.cb, c.surface, installer)
		if out["error"] != CatalogNotHere {
			t.Fatalf("out %v", out)
		}
	}
	if len(card.payloads()) != 0 || len(installer.installList()) != 0 || tools.LiveCurrent("s1", "") != nil {
		t.Fatal("a card or install ran off the desktop")
	}
}

func TestCatalogUnknownAndUnsupportedIDsFailAtOnceWithTheReason(t *testing.T) {
	ctx, _ := catalogInstallFixture(t)
	installer := newCatalogFake(catalogEntry("nvidia-app", []string{"windows"}))
	installer.refusal["nvidia-app"] = "Plugin 'nvidia-app' is unavailable on darwin; supported platforms: windows."
	card, cb := catalogCard(ctx, "", func(map[string]any) map[string]any { return map[string]any{"settled_by": "continue"} })
	out := catalogInstallRun(t, ctx, catalogItems("plugin", "nope", "plugin", "nvidia-app"), installer, cb)
	if len(card.payloads()) != 0 {
		t.Fatal("a card was drawn for rows that are already done")
	}
	rows := map[string]map[string]any{}
	for _, r := range catalogRows(out) {
		rows[r["name"].(string)] = r
		if r["state"] != "failed" {
			t.Fatalf("row %v", r)
		}
	}
	nv := rows["nvidia-app"]
	if !strings.Contains(rows["nope"]["detail"].(string), "catalog") || !strings.Contains(nv["detail"].(string), "unavailable on darwin") ||
		nv["display"] != "Nvidia App" || !reflect.DeepEqual(nv["platforms"], []any{"windows"}) || len(installer.installList()) != 0 {
		t.Fatalf("rows %v", rows)
	}
}

func TestCatalogApprovedRowInstallsIntoTheChatsProfileAndListsTheLiveTools(t *testing.T) {
	ctx, _ := catalogInstallFixture(t)
	bHome := catalogProfile(t, "b")
	installer := newCatalogFake(catalogEntry("blender", nil))
	card, cb := catalogCard(ctx, bHome, catalogApprove(nil))
	out := catalogInstallRun(t, home.WithOverride(ctx, bHome), catalogItems("plugin", "blender"), installer, cb)
	rows := catalogRows(out)
	if len(rows) != 1 || card.payloads()[0]["targets"].([]any)[0].(map[string]any)["state"] != "pending" {
		t.Fatalf("rows %v card %v", rows, card.payloads())
	}
	row := rows[0]
	if row["state"] != "connected" || row["target_profile"] != "b" ||
		!reflect.DeepEqual(row["tools"], []any{"mcp__blender__status", "mcp__blender__launch"}) {
		t.Fatalf("row %v", row)
	}
	calls := installer.installList()
	if len(calls) != 1 || calls[0]["force"] != false || calls[0]["enable"] != true || calls[0]["ref"] != nil ||
		!catalogSameHome(calls[0]["home"].(string), bHome) {
		t.Fatalf("installs %v", calls)
	}
}

func TestCatalogAdvancedValuesPickTheProfileForceAndPin(t *testing.T) {
	ctx, _ := catalogInstallFixture(t)
	work := catalogProfile(t, "work")
	installer := newCatalogFake(catalogEntry("blender", nil))
	pin := strings.Repeat("c", 40)
	_, cb := catalogCard(ctx, "", catalogApprove(map[string]any{"target_profile": "work", "force": "1", "enable": "0", "ref": pin}))
	rows := catalogRows(catalogInstallRun(t, ctx, catalogItems("plugin", "blender"), installer, cb))
	calls := installer.installList()
	if len(rows) != 1 || len(calls) != 1 || calls[0]["force"] != true || calls[0]["enable"] != false || calls[0]["ref"] != pin {
		t.Fatalf("rows %v installs %v", rows, calls)
	}
	row := rows[0]
	if !catalogSameHome(calls[0]["home"].(string), work) || row["target_profile"] != "work" ||
		!reflect.DeepEqual(row["approved"], map[string]any{"force": true, "enable": false, "ref": pin}) ||
		row["enabled"] != false || !strings.Contains(row["detail"].(string), "not enabled") {
		t.Fatalf("row %v installs %v", row, calls)
	}
}

func catalogWaitFor(t *testing.T, state tools.TargetState) *tools.ConnectionOperation {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if op := tools.LiveCurrent("s1", ""); op != nil && op.StateOf(op.Targets[0]) == state {
			return op
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("blender never reached %s", state)
	return nil
}

func TestCatalogFailedRowKeepsItsReasonAndTryAgainWorksWhileTheCardIsOpen(t *testing.T) {
	ctx, _ := catalogInstallFixture(t)
	installer := newCatalogFake(catalogEntry("blender", nil), catalogEntry("krita", nil))
	installer.installError = "clone failed: network down"
	_, cb := catalogCard(ctx, "", func(map[string]any) map[string]any {
		return map[string]any{"targets": []any{map[string]any{"name": "blender", "status": "approved", "env": nil}}}
	})
	done := make(chan map[string]any, 1)
	go func() {
		out, _ := ManageCatalog(ctx, nil, map[string]any{"action": "install", "items": catalogItems("plugin", "blender", "plugin", "krita")},
			CatalogOptions{SessionID: "s1", Callback: cb, CardSurface: true, Installer: installer})
		var parsed map[string]any
		_ = json.Unmarshal([]byte(out), &parsed)
		done <- parsed
	}()
	var result map[string]any
	func() {
		defer func() {
			deadline := time.Now().Add(10 * time.Second)
			for result == nil && time.Now().Before(deadline) {
				if op := tools.LiveCurrent("s1", ""); op != nil {
					op.Settle(tools.SettleContinue)
					op.Wake.Set()
				}
				select {
				case result = <-done:
				case <-time.After(10 * time.Millisecond):
				}
			}
		}()
		op := catalogWaitFor(t, tools.TargetFailed)
		var detail string
		op.UpdateTarget(op.Targets[0], func(tg *tools.ConnTarget) { detail = tg.Detail })
		if detail != "clone failed: network down" || op.Settled() {
			t.Fatalf("detail %q settled %v", detail, op.Settled())
		}
		installer.setInstallError("")
		if err := ApplyConnAnswer(ctx, op, `{"targets": [{"name": "blender", "status": "approved"}]}`); err != nil {
			t.Fatal(err)
		}
		catalogWaitFor(t, tools.TargetConnected)
		if err := ApplyConnAnswer(ctx, op, `{"targets": [{"name": "krita", "status": "skipped"}]}`); err != nil {
			t.Fatal(err)
		}
		select {
		case result = <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the operation did not settle once krita was skipped")
		}
	}()
	rows := catalogRows(result)
	if rows[0]["state"] != "connected" || rows[1]["state"] != "skipped" || len(installer.installList()) != 2 {
		t.Fatalf("rows %v installs %v", rows, installer.installList())
	}
	installer.setInstallError("clone failed: network down")
	_, cb = catalogCard(ctx, "", catalogApprove(nil))
	out := catalogInstallRun(t, ctx, catalogItems("plugin", "blender"), installer, cb)
	rows = catalogRows(out)
	if out["settled_by"] != "all_resolved" || len(rows) != 1 || rows[0]["state"] != "failed" || rows[0]["detail"] != "clone failed: network down" {
		t.Fatalf("out %v", out)
	}
}

func TestCatalogSkillRowInstallsWithTheSourceThatResolvedIt(t *testing.T) {
	ctx, _ := catalogInstallFixture(t)
	installer := newCatalogFake()
	installer.clawhub = true
	_, cb := catalogCard(ctx, "", catalogApprove(nil))
	out := catalogInstallRun(t, ctx, catalogItems("skill", "agent-git-oracle"), installer, cb)
	if catalogRows(out)[0]["state"] != "connected" ||
		!reflect.DeepEqual(installer.installList(), []map[string]any{{"identifier": "agent-git-oracle", "source": "clawhub"}}) {
		t.Fatalf("out %v installs %v", out, installer.installList())
	}
}

func TestCatalogSearchRowsMarkAnInstalledClawHubHitButNotAnotherSourcesBareID(t *testing.T) {
	ctx, root := catalogInstallFixture(t)
	lock := skills.NewHub(skills.NewScope(root, nil)).LockFile("")
	if err := lock.Save(pyjson.ObjectOf("version", int64(1), "installed", pyjson.ObjectOf("agent-git-oracle", pyjson.ObjectOf(
		"source", "clawhub", "identifier", "@tmstudio667-commits/agent-git-oracle", "install_path", "agent-git-oracle")))); err != nil {
		t.Fatal(err)
	}
	hits := []skills.SkillMeta{{Name: "Agent Git Oracle", Source: "clawhub", Identifier: "agent-git-oracle", TrustLevel: "community"},
		{Name: "agent-git-oracle", Source: "skills-sh", Identifier: "agent-git-oracle", TrustLevel: "community"}}
	savedSources, savedSearch := catalogSkillSources, catalogUnifiedSearch
	catalogSkillSources = func(context.Context, *skills.HubCLI) []skills.SkillSource { return nil }
	catalogUnifiedSearch = func(context.Context, *skills.HubCLI, string, []skills.SkillSource, int) []skills.SkillMeta {
		return hits
	}
	t.Cleanup(func() { catalogSkillSources, catalogUnifiedSearch = savedSources, savedSearch })
	rows, err := catalogSkillRows(ctx, "agent git oracle")
	if err != nil {
		t.Fatal(err)
	}
	var installed []bool
	for _, r := range rows {
		v, _ := r.(*pyjson.Object).Get("installed")
		installed = append(installed, v.(bool))
	}
	if !reflect.DeepEqual(installed, []bool{true, false}) {
		t.Fatalf("installed %v", installed)
	}
}
