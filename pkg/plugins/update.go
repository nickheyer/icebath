package plugins

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
)

// Install record of a plugin, the AttributeError a non mapping row raises on get
func installRecordOf(metadata map[string]any, name string) (map[string]any, error) {
	raw, ok := metadata[name]
	if !ok {
		return map[string]any{}, nil
	}
	record, isMap := raw.(map[string]any)
	if !isMap {
		return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(raw))
	}
	return record, nil
}

func recordGet(record map[string]any, key string, def any) any {
	if v, ok := record[key]; ok {
		return v
	}
	return def
}

// Refuses pinned and kill listed installs, then pulls a git checkout or re clones a recorded source
func pullPluginUpdate(ctx context.Context, h, target string, pinnedMsg func(map[string]any) string, notGitMsg func() string,
	beforePull func(), interactive, force bool, scanDecision func(scan any) bool) (string, error) {
	metadata, err := readInstallMetadata(h)
	if err != nil {
		return "", err
	}
	name := filepath.Base(target)
	record, err := installRecordOf(metadata, name)
	if err != nil {
		return "", err
	}
	if record["pinned"] == true {
		return "", &PluginOperationError{Msg: pinnedMsg(record)}
	}
	if err := refuseIfInstalledRemoved(h, name, target); err != nil {
		return "", err
	}
	if _, statErr := os.Stat(filepath.Join(target, ".git")); statErr != nil {
		source, _ := record["source"].(string)
		if source == "" {
			return "", &PluginOperationError{Msg: notGitMsg()}
		}
		if beforePull != nil {
			beforePull()
		}
		return recloneUpdate(ctx, h, target, source, record["revision"], force, scanDecision)
	}
	if beforePull != nil {
		beforePull()
	}
	return updatePlugin(ctx, target, updatePluginOptions{Interactive: interactive, Force: force, ScanDecision: scanDecision})
}

// Re installs a subdirectory install from its source, carrying user files into the staged tree
func recloneUpdate(ctx context.Context, h, target, source string, previousRevision any, force bool, scanDecision func(scan any) bool) (string, error) {
	acceptCaution := force
	newTarget, _, _, err := installPluginCore(ctx, source, installCoreOptions{Force: true, AcceptCaution: &acceptCaution, ScanDecision: scanDecision,
		BeforeSwap: func(_ map[string]any, tree string) ([]string, error) { return carryUserFiles(target, tree, nil, false) }})
	if err != nil {
		return "", err
	}
	metadata, err := readInstallMetadata(h)
	if err != nil {
		return "", err
	}
	record, err := installRecordOf(metadata, filepath.Base(newTarget))
	if err != nil {
		return "", err
	}
	revision := ""
	if v := record["revision"]; jsonTruthy(v) {
		revision = pyrepr.Str(v)
	}
	previous, _ := previousRevision.(string)
	if revision != "" && revision == previous {
		return "Already up to date.", nil
	}
	return fmt.Sprintf("Re-installed from %s: %s..%s", source, previous[:min(8, len(previous))], revision[:min(8, len(revision))]), nil
}

// Updates one installed plugin with progress and consent prompts, true when it moved
func updateOne(ctx context.Context, name, target string, console *Console, interactive, force bool) (bool, error) {
	h := home.Dir(ctx)
	if sidecar := catalogInstallRecord(h, target); len(sidecar) > 0 {
		return cmdUpdateCatalog(ctx, name, target, sidecar, console, interactive)
	}
	var decision func(scan any) bool
	if interactive {
		decision = InteractiveScanDecision(console, "Update")
	}
	output, err := pullPluginUpdate(ctx, h, target,
		func(rec map[string]any) string {
			return fmt.Sprintf("Plugin '%s' is pinned to %s. To move it, run `icebath plugins install %s --force --ref <40-character commit SHA>`.",
				name, pyrepr.Str(rec["revision"]), pyrich.Escape(pyrepr.Str(recordGet(rec, "source", "<source>"))))
		},
		func() string {
			return fmt.Sprintf("Plugin '%s' was not installed from git (no .git directory). Cannot update.", name)
		},
		func() { console.Print(fmt.Sprintf("[dim]Updating %s...[/dim]", name)) }, interactive, force, decision)
	if err != nil {
		return false, err
	}
	postPullHousekeeping(target, console)
	updated, err := readManifest(target)
	if err != nil {
		return false, err
	}
	pluginID := filepath.Base(target)
	if v := updated["name"]; jsonTruthy(v) {
		pluginID = pyrepr.Str(v)
	}
	if declared := declaredCapabilitiesFromManifest(updated, pluginID); len(declared) > 0 {
		cfg := loadConfigOrEmpty(h)
		if len(PendingCapabilities(cfg, pluginID, declared)) > 0 || DeclaredSetChanged(cfg, pluginID, declared) {
			if interactive {
				if err := runCapabilityConsent(ctx, console, pluginID, declared, "update"); err != nil {
					return false, err
				}
			} else {
				console.Print(fmt.Sprintf("[yellow]Plugin %s has new capabilities; review them with `icebath plugins capabilities %s`.[/yellow]", pluginID, pluginID))
			}
		}
	}
	out := strings.TrimSpace(output)
	if strings.Contains(out, "Already up to date") {
		console.Print(fmt.Sprintf("[green]✓[/green] Plugin [bold]%s[/bold] is already up to date.", name))
		return false, nil
	}
	console.Print(fmt.Sprintf("[green]✓[/green] Plugin [bold]%s[/bold] updated.", name))
	console.Print(fmt.Sprintf("[dim]%s[/dim]", out))
	return true, nil
}

// Updates one named plugin, exiting 1 on a consent, scan or operation refusal
func CmdUpdate(ctx context.Context, name string, interactive, force bool, out io.Writer) error {
	console := NewConsole(out)
	dir, err := pluginsDir(home.Dir(ctx))
	if err != nil {
		return err
	}
	target, err := requireInstalledPlugin(ctx, name, dir, console)
	if err != nil {
		return err
	}
	_, err = updateOne(ctx, name, target, console, interactive, force)
	return updateRefusal(console, name, err)
}

// Exit 1 with the refusal message for consent, scan and operation errors, other errors unchanged
func updateRefusal(console *Console, name string, err error) error {
	var consent *RepinConsentRequired
	var blocked *PluginScanBlocked
	var opErr *PluginOperationError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &consent):
		return fail(console, fmt.Sprintf("[yellow]Update of %s not applied:[/yellow] %s", name, consent.Error()))
	case errors.As(err, &blocked):
		return fail(console, "[red]Blocked:[/red] "+blocked.Error())
	case errors.As(err, &opErr):
		return fail(console, "[red]Error:[/red] "+opErr.Error())
	}
	return err
}

// Drops stale bytecode and copies new example files after publication
func postPullHousekeeping(target string, console *Console) {
	clearPluginBytecode(target)
	copyExampleFiles(target, console)
}

// Records the git origin of a self cloned plugin dir as its provenance row
func CmdAdopt(ctx context.Context, name string, out io.Writer) error {
	console := NewConsole(out)
	h := home.Dir(ctx)
	dir, err := pluginsDir(h)
	if err != nil {
		return err
	}
	target, err := requireInstalledPlugin(ctx, name, dir, console)
	if err != nil {
		return err
	}
	var prov *Provenance
	for _, p := range PluginsProvenance(ctx, dir) {
		if p.Name == filepath.Base(target) {
			prov = &p
			break
		}
	}
	switch {
	case prov == nil:
		return fail(console, fmt.Sprintf("[red]Error:[/red] Plugin '%s' not classifiable.", name))
	case prov.Class != ProvenanceSelfCloned:
		return fail(console, fmt.Sprintf("[red]Error:[/red] Plugin '%s' is %s, not self-cloned — there is nothing to adopt.", name, prov.Class))
	case prov.OriginURL == "":
		return fail(console, fmt.Sprintf("[red]Error:[/red] Plugin '%s' has no readable git origin remote. Add one (git remote add origin <url>) and retry.", name))
	}
	if _, _, err := resolveGitURL(prov.OriginURL); err != nil {
		return fail(console, "[red]Error:[/red] The dir's origin url is not installable: "+err.Error())
	}
	metadata, err := readInstallMetadata(h)
	if err != nil {
		return err
	}
	if _, ok := metadata[filepath.Base(target)]; ok {
		return fail(console, fmt.Sprintf("[red]Error:[/red] Plugin '%s' already has a provenance row.", name))
	}
	revision := ""
	if gitExe := resolveGitExecutable(); gitExe != "" {
		if revision, err = gitHeadRevision(ctx, target, gitExe); err != nil {
			return err
		}
	}
	source, err := canonicalSource(prov.OriginURL, "")
	if err != nil {
		return err
	}
	metadata[filepath.Base(target)] = map[string]any{"pinned": false, "revision": revision, "source": source}
	if err := writeInstallMetadata(ctx, h, metadata); err != nil {
		return err
	}
	shown := revision[:min(12, len(revision))]
	if shown == "" {
		shown = "unknown"
	}
	console.Print(fmt.Sprintf("[green]✓[/green] Adopted [bold]%s[/bold] (source: %s, revision: %s). It is now a tracked git install.", name, prov.OriginURL, shown))
	return nil
}

// Moves the saved update url tag to the manifest update url after a needs fixing mismatch
func CmdTrustUpdateURL(ctx context.Context, name string, out io.Writer) error {
	console := NewConsole(out)
	h := home.Dir(ctx)
	dir, err := pluginsDir(h)
	if err != nil {
		return err
	}
	target, err := requireInstalledPlugin(ctx, name, dir, console)
	if err != nil {
		return err
	}
	rows := ReadSidecarRows(dir)
	row, ok := rows[filepath.Base(target)].(map[string]any)
	if !ok {
		return fail(console, fmt.Sprintf("[red]Error:[/red] Plugin '%s' has no provenance row — nothing to trust. Reinstall it instead.", name))
	}
	var saved any
	if v := row["update_url"]; jsonTruthy(v) {
		saved = v
	}
	manifest, err := readManifest(target)
	if err != nil {
		return err
	}
	var claimed any
	if v := manifest["update_url"]; jsonTruthy(v) {
		claimed = v
	}
	if pyEqual(claimed, saved) {
		console.Print(fmt.Sprintf("[yellow]Nothing to trust:[/yellow] '%s' has no update_url mismatch.", name))
		return nil
	}
	if claimed != nil {
		url, err := httpsUpdateURL(claimed)
		if err != nil {
			return fail(console, fmt.Sprintf("[red]Error:[/red] Plugin '%s' %s. Not trusted.", name, err.Error()))
		}
		claimed = url
	}
	row["update_url"] = claimed
	rows[filepath.Base(target)] = row
	if err := writeInstallMetadata(ctx, h, rows); err != nil {
		return err
	}
	shownOld, shownNew := "(none)", "(none)"
	if saved != nil {
		shownOld = pyrepr.Str(saved)
	}
	if jsonTruthy(claimed) {
		shownNew = pyrepr.Str(claimed)
	}
	console.Print(fmt.Sprintf("[green]✓[/green] Trusted [bold]%s[/bold] update_url:\n  old: %s\n  new: %s", name, shownOld, shownNew))
	return nil
}

func pyEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return pyrepr.Repr(a) == pyrepr.Repr(b)
}

// Ordered check result mapping in the hermes to_json key order
func checkResultObject(r *CheckResult) *pyjson.Object {
	m := r.ToJSON()
	o := pyjson.NewObject()
	for _, k := range []string{"name", "class", "current", "latest", "update_available", "needs_fixing", "min_icebath", "reason"} {
		o.Set(k, m[k])
	}
	return o
}

// Reports which installed plugins are outdated without changing anything
func CmdCheckUpdates(ctx context.Context, asJSON bool, out io.Writer) error {
	console := NewConsole(out)
	dir, err := pluginsDir(home.Dir(ctx))
	if err != nil {
		return err
	}
	checked := snapshotInstalls(dir)
	results := RunChecks(ctx, dir, RunChecksOptions{})
	recordCheckResults(ctx, results, checked)
	if asJSON {
		list := make([]any, 0, len(results))
		for _, r := range results {
			list = append(list, checkResultObject(r))
		}
		text, err := pyjson.Dumps(list, pyjson.Indent(2))
		if err != nil {
			return err
		}
		fmt.Fprintln(out, text)
		return nil
	}
	table := newPluginTable("Plugin updates", [][2]string{{"Name", "bold"}, {"Class", "dim"}, {"Current", ""}, {"Latest", ""}, {"Status", ""}})
	for _, r := range results {
		held := ""
		if r.UpdateAvailable != nil && *r.UpdateAvailable {
			held = heldForNewerIcebath(r)
		}
		var status string
		switch {
		case r.NeedsFixing != "":
			status = fmt.Sprintf("[red]needs fixing[/red]\n[dim]%s[/dim]", r.NeedsFixing)
		case held != "":
			status = fmt.Sprintf("[yellow]update available after updating Icebath[/yellow]\n[dim]%s[/dim]", held)
		case r.UpdateAvailable != nil && *r.UpdateAvailable:
			status = "[green]update available[/green]"
		case r.UpdateAvailable != nil:
			status = "[dim]up to date[/dim]"
		default:
			status = fmt.Sprintf("[yellow]unknown[/yellow]\n[dim]%s[/dim]", r.Reason)
		}
		current := []rune(r.Current)
		if len(current) == 0 {
			current = []rune("-")
		}
		latest := r.Latest
		if latest == "" {
			latest = "-"
		}
		table.AddRow(r.Name, r.Class, string(current[:min(12, len(current))]), latest, status)
	}
	console.Print("")
	console.printTable(table)
	console.Print("")
	console.Print("[dim]Check-only. Apply with: icebath plugins update <name>, or icebath plugins update to update them all[/dim]")
	return nil
}

// Non interactive update for the dashboard, a catalog re pin or a git pull, refusals as answers
func DashboardUpdateUserPlugin(ctx context.Context, name string, acceptCapabilities, force bool) (map[string]any, error) {
	h := home.Dir(ctx)
	target, err := userInstalledPluginDir(ctx, name)
	if err != nil {
		return nil, err
	}
	if target == "" {
		dir, err := pluginsDir(h)
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": false, "error": fmt.Sprintf("Plugin '%s' was not found under %s.", name, dir)}, nil
	}
	answer, msg, err := dashboardUpdateStep(ctx, h, name, target, acceptCapabilities, force)
	var consent *RepinConsentRequired
	var blocked *PluginScanBlocked
	var opErr *PluginOperationError
	switch {
	case errors.As(err, &consent):
		return map[string]any{"ok": false, "consent_required": true, "error": consent.Error(), "name": consent.Name, "sha": consent.Sha,
			"delta": consent.Delta, "delta_lines": surfaceDeltaLines(consent.Delta)}, nil
	case errors.As(err, &blocked):
		return mergeResult(ScanBlockedResult(blocked), map[string]any{"name": name}), nil
	case errors.As(err, &opErr):
		return map[string]any{"ok": false, "error": opErr.Error()}, nil
	case err != nil:
		return nil, err
	case answer != nil:
		return answer, nil
	}
	postPullHousekeeping(target, NewConsole(os.Stdout))
	unchanged := strings.Contains(msg, "Already up to date")
	activated := map[string]any{}
	if !unchanged {
		if activated, err = activatePluginNow(ctx, filepath.Base(target)); err != nil {
			return nil, err
		}
	}
	return mergeResult(map[string]any{"ok": true, "name": name, "output": msg, "unchanged": unchanged}, activated), nil
}

// Catalog re pin answer or the git pull output of one dashboard update
func dashboardUpdateStep(ctx context.Context, h, name, target string, acceptCapabilities, force bool) (map[string]any, string, error) {
	if sidecar := catalogInstallRecord(h, target); len(sidecar) > 0 {
		var consent func(map[string][]string) bool
		if acceptCapabilities {
			consent = func(map[string][]string) bool { return true }
		}
		result, err := repinCatalogPlugin(ctx, target, sidecar, false, consent)
		if err != nil {
			return nil, "", err
		}
		warnings := append([]string{}, result.Warnings...)
		deps := []string{}
		activated := map[string]any{}
		if result.Changed {
			deps = pythonDependencySummary(filepath.Join(filepath.Dir(target), result.InstalledName), &warnings)
			if activated, err = activatePluginNow(ctx, result.InstalledName); err != nil {
				return nil, "", err
			}
		}
		return mergeResult(map[string]any{"ok": true, "name": result.InstalledName, "sha": result.Sha, "unchanged": !result.Changed,
			"python_dependencies": deps, "warnings": warnings}, activated), "", nil
	}
	msg, err := pullPluginUpdate(ctx, h, target,
		func(rec map[string]any) string {
			return fmt.Sprintf("Plugin '%s' is pinned to %s; run `icebath plugins install %s --force --ref <40-character commit SHA>` to move it.",
				name, pyrepr.Str(rec["revision"]), pyrepr.Str(recordGet(rec, "source", "<source>")))
		},
		func() string { return fmt.Sprintf("Plugin '%s' is not a git checkout; cannot pull updates.", name) },
		nil, false, force, nil)
	return nil, msg, err
}

// Removes every pycache dir under an updated checkout, counting the ones that are gone
func clearPluginBytecode(target string) int {
	removed := 0
	_ = filepath.WalkDir(target, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == "__pycache__" {
			_ = os.RemoveAll(path)
			if _, statErr := os.Lstat(path); statErr != nil {
				removed++
			}
			return filepath.SkipDir
		}
		return nil
	})
	return removed
}
