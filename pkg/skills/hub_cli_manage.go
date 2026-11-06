package skills

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Value of a lock entry key, false when absent as a KeyError would report
func objGet(o *pyjson.Object, key string) (any, bool) {
	if o == nil {
		return nil, false
	}
	return o.Get(key)
}

func entryStr(o *pyjson.Object, key, def string) string {
	if v, ok := objGet(o, key); ok {
		return pyrepr.Str(v)
	}
	return def
}

func keyError(key string) error { return fmt.Errorf("KeyError: %s", pyrepr.Repr(key)) }

// Lists installed skills as hub, builtin or local with their enabled state in the active profile
func (x *HubCLI) DoList(ctx context.Context, sourceFilter string, enabledOnly bool, c *pyrich.Console) error {
	c = x.console(c)
	if err := x.Hub.EnsureHubDirs(); err != nil {
		return err
	}
	entries, err := x.Hub.LockFile("").ListInstalled()
	if err != nil {
		return err
	}
	hubInstalled := map[string]*pyjson.Object{}
	for _, e := range entries {
		name, ok := objGet(e, "name")
		if !ok {
			return keyError("name")
		}
		hubInstalled[pyrepr.Str(name)] = e
	}
	manifest, err := x.S.readManifest()
	if err != nil {
		return err
	}
	var all []catalogEntry
	for _, s := range x.S.skillCatalog(true, false) {
		if s.LoadName != "" {
			s.Name = s.LoadName
			all = append(all, s)
		}
	}
	rootLabels := map[int]string{TierProject: "project", TierCreateDir: "create_dir", TierExternal: "external"}
	disabled := x.S.DisabledSkillNames("")
	title := "Installed Skills"
	if enabledOnly {
		title += " (enabled only)"
	}
	table := &pyrich.Table{Title: title, Columns: []pyrich.Column{{Header: "Name", Style: "bold cyan"}, dimCol("Category"),
		dimCol("Source"), dimCol("Trust"), dimCol("Status")}}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Category != all[j].Category {
			return all[i].Category < all[j].Category
		}
		return all[i].Name < all[j].Name
	})
	counts := map[string]int{"hub": 0, "builtin": 0, "local": 0}
	enabledCount, disabledCount := 0, 0
	for _, skill := range all {
		name := skill.Name
		var sourceType, sourceDisplay, trust string
		if entry, ok := hubInstalled[name]; ok {
			sourceType, sourceDisplay = "hub", entryStr(entry, "source", "hub")
			trust = entryStr(entry, "trust_level", "community")
		} else {
			sourceType = "local"
			if _, ok := manifest[name]; ok {
				sourceType = "builtin"
			}
			sourceDisplay, trust = sourceType, sourceType
			if label, ok := rootLabels[skill.Tier]; ok {
				sourceDisplay = label
			}
		}
		isEnabled := !disabled[name]
		if (sourceFilter != "all" && sourceFilter != sourceType) || (enabledOnly && !isEnabled) {
			continue
		}
		counts[sourceType]++
		status := "[dim red]disabled[/]"
		if isEnabled {
			enabledCount++
			status = "[bold green]enabled[/]"
		} else {
			disabledCount++
		}
		if note, ok := SkillCommandCollisionNote(name); ok && note != "" {
			status += "\n[yellow]" + note + "[/]"
		}
		table.AddRow(name, skill.Category, sourceDisplay, trustCell(trust, sourceDisplay, "official"), status)
	}
	c.PrintTable(table)
	tail := fmt.Sprintf("%d enabled, %d disabled", enabledCount, disabledCount)
	if enabledOnly {
		tail = fmt.Sprintf("%d enabled shown", enabledCount)
	}
	c.Print(fmt.Sprintf("[dim]%d hub-installed, %d builtin, %d local — %s[/]\n", counts["hub"], counts["builtin"], counts["local"], tail))
	return nil
}

func (x *HubCLI) checkUpdates(ctx context.Context, name string) ([]SkillUpdateCheck, error) {
	if x.seams.checkUpdates != nil {
		return x.seams.checkUpdates(ctx, name)
	}
	return x.Hub.CheckForSkillUpdates(ctx, name, nil, nil, nil)
}

// Checks hub installed skills for upstream updates
func (x *HubCLI) DoCheck(ctx context.Context, name string, c *pyrich.Console) error {
	c = x.console(c)
	results, err := x.checkUpdates(ctx, name)
	if err != nil {
		return err
	}
	if len(results) == 0 {
		c.Print("[dim]No hub-installed skills to check.[/]\n")
		return nil
	}
	table := &pyrich.Table{Title: "Skill Updates", Columns: []pyrich.Column{{Header: "Name", Style: "bold cyan"}, dimCol("Source"), dimCol("Status")}}
	updates := 0
	var orphaned []string
	for _, e := range results {
		table.AddRow(e.Name, e.Source, e.Status)
		if e.Status == "update_available" {
			updates++
		}
		if e.Status == "orphaned" {
			orphaned = append(orphaned, e.Name)
		}
	}
	c.PrintTable(table)
	c.Print(fmt.Sprintf("[dim]%d update(s) available across %d checked skill(s)[/]\n", updates, len(results)))
	if len(orphaned) > 0 {
		c.Print(fmt.Sprintf("[yellow]Orphaned:[/] %s — lock-file entries whose local directory is missing or replaced by a non-directory. "+
			"For missing directories, remove the stale entry with: icebath skills uninstall <name>\n", strings.Join(orphaned, ", ")))
	}
	return nil
}

// Whether the installed copy no longer matches the install time content hash
func (x *HubCLI) hasLocalEdits(installed *pyjson.Object) bool {
	recorded := entryStr(installed, "content_hash", "")
	path := filepath.Join(x.S.SkillsDir(), entryStr(installed, "install_path", ""))
	info, err := os.Stat(path)
	return recorded != "" && err == nil && info.IsDir() && ContentHash(path) != recorded
}

// Category of a lock install path, its parent with the current directory meaning none
func installCategory(installPath string) string {
	parent := filepath.Dir(filepath.Clean(installPath))
	if parent == "." {
		return ""
	}
	return parent
}

// Updates hub installed skills, skipping locally edited ones unless forced
func (x *HubCLI) DoUpdate(ctx context.Context, name string, c *pyrich.Console, force bool) (HubOutcome, error) {
	c = x.console(c)
	checks, err := x.checkUpdates(ctx, name)
	if err != nil {
		return HubNoop, err
	}
	var updates []SkillUpdateCheck
	for _, e := range checks {
		if e.Status == "update_available" {
			updates = append(updates, e)
		}
	}
	if len(updates) == 0 {
		c.Print("[dim]No updates available.[/]\n")
		return HubNoop, nil
	}
	var skipped, failed []string
	for _, e := range updates {
		installed, err := x.lockInstalled(e.Name)
		if err != nil {
			return HubNoop, err
		}
		category := ""
		if installed != nil {
			category = installCategory(entryStr(installed, "install_path", ""))
			if !force && x.hasLocalEdits(installed) {
				skipped = append(skipped, e.Name)
				c.Print(fmt.Sprintf("[yellow]Skipping:[/] %s — you have local edits (update would overwrite them).", e.Name))
				continue
			}
		}
		c.Print(fmt.Sprintf("[bold]Updating:[/] %s", e.Name))
		out, err := x.DoInstall(ctx, e.Identifier, HubInstallOptions{Category: category, Force: true, Console: c, SourceID: e.Source})
		if err != nil {
			return HubNoop, err
		}
		if out == HubFailed {
			failed = append(failed, e.Name)
		}
	}
	if updated := len(updates) - len(skipped) - len(failed); updated > 0 {
		c.Print(fmt.Sprintf("[bold green]Updated %d skill(s).[/]\n", updated))
	}
	if len(skipped) > 0 {
		sort.Strings(skipped)
		c.Print(fmt.Sprintf("[dim]%d skill(s) kept your local edits: %s.[/]", len(skipped), strings.Join(skipped, ", ")))
		c.Print("[dim]Overwrite with: icebath skills update <name> --force[/]\n")
	}
	if len(failed) > 0 {
		sort.Strings(failed)
		c.Print(fmt.Sprintf("[bold red]Not updated:[/] %s (see the messages above).\n", strings.Join(failed, ", ")))
		return HubFailed, nil
	}
	return HubNoop, nil
}

// Re-scans installed hub skills, deep adding the AST diagnostic
func (x *HubCLI) DoAudit(ctx context.Context, name string, c *pyrich.Console, deep bool) error {
	c = x.console(c)
	installed, err := x.Hub.LockFile("").ListInstalled()
	if err != nil {
		return err
	}
	if len(installed) == 0 {
		c.Print("[dim]No hub-installed skills to audit.[/]\n")
		return nil
	}
	targets := installed
	if name != "" {
		targets = nil
		for _, e := range installed {
			n, ok := objGet(e, "name")
			if !ok {
				return keyError("name")
			}
			if pyrepr.Str(n) == name {
				targets = append(targets, e)
			}
		}
	}
	if len(targets) == 0 {
		printHubError(c, fmt.Sprintf("'%s' is not a hub-installed skill.", name))
		return nil
	}
	c.Print(fmt.Sprintf("\n[bold]Auditing %d skill(s)...[/]\n", len(targets)))
	for _, e := range targets {
		ip, ok := objGet(e, "install_path")
		if !ok {
			return keyError("install_path")
		}
		skillPath := filepath.Join(x.S.SkillsDir(), pyrepr.Str(ip))
		entryName := entryStr(e, "name", "")
		if _, err := os.Stat(skillPath); err != nil {
			c.Print(fmt.Sprintf("[yellow]Warning:[/] %s — path missing: %s", entryName, pyrepr.Str(ip)))
			continue
		}
		source, ok := objGet(e, "identifier")
		if !ok {
			if source, ok = objGet(e, "source"); !ok {
				return keyError("source")
			}
		}
		c.Print(FormatScanReport(ScanSkill(skillPath, pyrepr.Str(source))))
		if deep {
			c.Print(FormatASTReport(ASTScanPath(skillPath), entryName))
		}
		c.Print()
	}
	return nil
}

// Removes a hub installed skill after confirmation
func (x *HubCLI) DoUninstall(ctx context.Context, name string, c *pyrich.Console, skipConfirm, deferCache bool) (HubOutcome, error) {
	c = x.console(c)
	if !skipConfirm && !x.confirmOrCancel(c, cancelledLine, fmt.Sprintf("\n[bold]Uninstall '%s'?[/]", name)) {
		return HubNoop, nil
	}
	ok, msg, err := x.Hub.UninstallSkill(name)
	if err != nil {
		return HubNoop, err
	}
	if !reportPair(c, ok, msg) {
		return HubFailed, nil
	}
	x.finishDefault(c, !deferCache)
	return HubOK, nil
}

func stringList(v any) []string {
	var out []string
	switch l := v.(type) {
	case []string:
		out = l
	case []any:
		for _, s := range l {
			out = append(out, pyrepr.Str(s))
		}
	}
	return out
}

// Resets a bundled skill manifest entry, restore also re-copying the bundled version
func (x *HubCLI) DoReset(ctx context.Context, name string, restore bool, c *pyrich.Console, skipConfirm, deferCache bool) error {
	c = x.console(c)
	if !skipConfirm && restore && !x.confirmOrCancel(c, cancelledLine, fmt.Sprintf("\n[bold]Restore '%s' from bundled source?[/]", name),
		"[dim]This will DELETE your current copy and re-copy the bundled version.[/]") {
		return nil
	}
	result, err := x.S.ResetBundledSkill(ctx, name, restore)
	if err != nil {
		return err
	}
	if !reportOK(c, result, "") {
		return nil
	}
	synced, _ := result["synced"].(map[string]any)
	printListed(c, "Copied", stringList(synced["copied"]))
	printListed(c, "Updated", stringList(synced["updated"]))
	c.Print()
	x.finishDefault(c, !deferCache)
	return nil
}

// Lists the bundled skills the user has edited
func (x *HubCLI) DoListModified(ctx context.Context, c *pyrich.Console, asJSON bool) error {
	c = x.console(c)
	modified, err := x.S.ListUserModifiedBundledSkills()
	if err != nil {
		return err
	}
	if asJSON {
		names := make([]any, len(modified))
		for i, m := range modified {
			names[i] = m["name"]
		}
		text, err := pyjson.Dumps(names)
		if err != nil {
			return err
		}
		c.Print(text)
		return nil
	}
	if len(modified) == 0 {
		c.Print("[dim]No user-modified bundled skills — everything tracks upstream.[/]\n")
		return nil
	}
	c.Print(fmt.Sprintf("\n[bold]%d user-modified bundled skill(s)[/] [dim](kept as-is by `icebath update`):[/]", len(modified)))
	for _, m := range modified {
		c.Print("  [yellow]~[/] " + pyrepr.Str(m["name"]))
	}
	c.Print()
	c.Print("[dim]See changes:   icebath skills diff <name>[/]")
	c.Print("[dim]Resume updates: icebath skills reset <name>          (keep your copy, re-baseline)[/]")
	c.Print("[dim]Revert to stock: icebath skills reset <name> --restore[/]\n")
	return nil
}

// Unified diff line with light coloring, file headers left plain
func printDiffLine(c *pyrich.Console, line string) {
	for _, ps := range [][2]string{{"+", "green"}, {"-", "red"}, {"@@", "cyan"}} {
		if strings.HasPrefix(line, ps[0]) && !strings.HasPrefix(line, strings.Repeat(ps[0], 3)) {
			c.Print("[" + ps[1] + "]" + line + "[/]")
			return
		}
	}
	c.Print(line)
}

var diffStatusLine = map[string]string{"added": "[green]+ only in your copy:[/] %s", "removed": "[red]- only in stock:[/] %s",
	"binary": "[yellow]~ %s:[/] binary file differs"}

// Shows how the user copy of a bundled skill differs from the stock version
func (x *HubCLI) DoDiff(ctx context.Context, name string, c *pyrich.Console) error {
	c = x.console(c)
	result, err := x.S.DiffBundledSkill(name)
	if err != nil {
		return err
	}
	if !pyrepr.Truthy(result["ok"]) {
		printHubError(c, pyrepr.Str(result["message"]))
		return nil
	}
	if !pyrepr.Truthy(result["modified"]) {
		c.Print(fmt.Sprintf("[green]%s[/]\n", pyrepr.Str(result["message"])))
		return nil
	}
	c.Print(fmt.Sprintf("\n[bold]%s[/]\n", pyrepr.Str(result["message"])))
	diffs, _ := result["diffs"].([]map[string]any)
	for _, e := range diffs {
		if e["status"] == "modified" {
			for _, line := range pystr.Splitlines(pyrepr.Str(e["diff"]), false) {
				printDiffLine(c, line)
			}
			continue
		}
		format, ok := diffStatusLine[pyrepr.Str(e["status"])]
		if !ok {
			format = diffStatusLine["binary"]
		}
		c.Print(fmt.Sprintf(format, pyrepr.Str(e["path"])))
	}
	c.Print()
	c.Print(fmt.Sprintf("[dim]Revert with: icebath skills reset %s --restore[/]\n", name))
	return nil
}

// Writes the bundled skills opt out marker, remove also deleting pristine bundled skills
func (x *HubCLI) DoOptOut(ctx context.Context, remove bool, c *pyrich.Console, skipConfirm, deferCache bool) error {
	c = x.console(c)
	res := x.S.SetBundledSkillsOptOut(true)
	if !reportOK(c, res, "") {
		return nil
	}
	c.Print(fmt.Sprintf("[dim]Marker: %s[/]", pyrepr.Str(res["marker"])))
	if !remove {
		c.Print("[dim]Existing skills on disk were left in place. Re-run with --remove to also delete unmodified bundled skills.[/]\n")
		return nil
	}
	preview, err := x.S.RemovePristineBundledSkills(true)
	if err != nil {
		return err
	}
	candidates := stringList(preview["removed"])
	if len(candidates) == 0 {
		c.Print("[dim]No pristine bundled skills to remove (nothing tracked, or all are user-modified/local).[/]\n")
		return nil
	}
	c.Print(fmt.Sprintf("\n[bold]Will remove %d unmodified bundled skill(s):[/]", len(candidates)))
	c.Print(fmt.Sprintf("[dim]%s[/]", strings.Join(candidates, ", ")))
	if skipped := stringList(preview["skipped"]); len(skipped) > 0 {
		c.Print(fmt.Sprintf("[dim]Keeping %d (user-modified or non-bundled).[/]", len(skipped)))
	}
	if !skipConfirm && !x.confirmOrCancel(c, "[dim]Marker kept; no skills deleted.[/]\n",
		"[dim]This deletes the on-disk copies. User-edited and hub/local skills are NOT touched.[/]") {
		return nil
	}
	result, err := x.S.RemovePristineBundledSkills(false)
	if err != nil {
		return err
	}
	c.Print(fmt.Sprintf("[bold green]%s[/]", pyrepr.Str(result["message"])))
	printListed(c, "Removed", stringList(result["removed"]))
	c.Print()
	x.finishChange(c, !deferCache, "Change will take effect", "apply", false)
	return nil
}

// Removes the opt out marker, sync re-seeding bundled skills now
func (x *HubCLI) DoOptIn(ctx context.Context, sync bool, c *pyrich.Console, deferCache bool) error {
	c = x.console(c)
	if !reportOK(c, x.S.SetBundledSkillsOptOut(false), "") {
		return nil
	}
	if sync {
		res, err := x.S.SyncSkills(ctx, true)
		if err != nil {
			return err
		}
		c.Print(fmt.Sprintf("[dim]Re-seeded %d bundled skill(s).[/]", len(stringList(res["copied"]))))
		x.finishChange(c, !deferCache, "Change will take effect", "apply", false)
	}
	c.Print()
	return nil
}

// Backfills or restores official optional skills from repo source
func (x *HubCLI) DoRepairOfficial(ctx context.Context, name string, restore bool, c *pyrich.Console, skipConfirm, deferCache bool) error {
	c = x.console(c)
	if restore && !skipConfirm && !x.confirmOrCancel(c, cancelledLine, fmt.Sprintf("\n[bold]Restore official optional skill '%s' from repo source?[/]", name),
		"[dim]Existing matching active copies will be moved to a restore backup before copying the official source.[/]") {
		return nil
	}
	result, err := x.S.RestoreOfficialOptionalSkill(name, restore)
	if err != nil {
		return err
	}
	if !reportOK(c, result, "Repair failed") {
		return nil
	}
	printListed(c, "Restored", stringList(result["restored"]))
	printListed(c, "Backfilled provenance", stringList(result["backfilled"]))
	if backed := stringList(result["backed_up"]); len(backed) > 0 {
		c.Print(fmt.Sprintf("[dim]Backed up: %s[/]", strings.Join(backed, ", ")))
		c.Print(fmt.Sprintf("[dim]Backup dir: %s[/]", pyrepr.Str(result["backup_dir"])))
	}
	c.Print()
	x.finishChange(c, !deferCache, "Change will take effect", "apply", false)
	return nil
}

// Manages taps, the custom GitHub repo sources
func (x *HubCLI) DoTap(ctx context.Context, action, repo string, c *pyrich.Console) error {
	c = x.console(c)
	mgr := x.Hub.Taps("")
	switch action {
	case "list":
		taps, err := mgr.ListTaps()
		if err != nil {
			return err
		}
		if len(taps) == 0 {
			c.Print("[dim]No custom taps configured. Using default sources only.[/]\n")
			return nil
		}
		table := &pyrich.Table{Title: "Configured Taps", Columns: []pyrich.Column{{Header: "Repo", Style: "bold cyan"}, dimCol("Path")}}
		for _, t := range taps {
			obj, ok := t.(*pyjson.Object)
			if !ok {
				return fmt.Errorf("AttributeError: %s object has no attribute 'get'", pyrepr.Repr(fmt.Sprintf("%T", t)))
			}
			label := any(nil)
			for _, k := range []string{"repo", "name"} {
				if v, _ := obj.Get(k); pyrepr.Truthy(v) {
					label = v
					break
				}
			}
			if label == nil {
				label = entryStr(obj, "path", "unknown")
			}
			table.AddRow(pyrepr.Str(label), entryStr(obj, "path", "skills/"))
		}
		c.PrintTable(table)
		c.Print()
	case "add", "remove":
		if repo == "" {
			printHubError(c, fmt.Sprintf("Repo required. Usage: icebath skills tap %s owner/repo", action))
			return nil
		}
		var ok bool
		var err error
		okLine, failLine := "[bold green]Added tap:[/] %s\n", "[yellow]Tap already exists:[/] %s\n"
		if action == "add" {
			ok, err = mgr.Add(repo, "skills/")
		} else {
			okLine, failLine = "[bold green]Removed tap:[/] %s\n", "[bold red]Error:[/] Tap not found: %s\n"
			ok, err = mgr.Remove(repo)
		}
		if err != nil {
			return err
		}
		if ok {
			c.Print(fmt.Sprintf(okLine, repo))
		} else {
			c.Print(fmt.Sprintf(failLine, repo))
		}
	default:
		c.Print(fmt.Sprintf("[bold red]Unknown tap action:[/] %s. Use: list, add, remove\n", action))
	}
	return nil
}

const pyWhitespace = `\t\n\x0b\x0c\r\x1c-\x1f \x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

var publishFrontmatterEndRe = regexp.MustCompile(`\n---[` + pyWhitespace + `]*\n`)

// YAML frontmatter of a SKILL.md body, empty when absent or invalid
func readPublishFrontmatter(skillMD string) (map[string]any, error) {
	if !strings.HasPrefix(skillMD, "---") {
		return map[string]any{}, nil
	}
	loc := publishFrontmatterEndRe.FindStringIndex(skillMD[3:])
	if loc == nil {
		return map[string]any{}, nil
	}
	data, err := yamlLoad(skillMD[3 : loc[0]+3])
	if err != nil || !pyrepr.Truthy(data) {
		return map[string]any{}, nil
	}
	m, ok := data.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("AttributeError: '%T' object has no attribute 'get'", data)
	}
	return m, nil
}

// Publishes a local skill to a registry after a security scan
func (x *HubCLI) DoPublish(ctx context.Context, skillPath, target, repo string, c *pyrich.Console) error {
	c = x.console(c)
	path := skillPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(x.S.SkillsDir(), path)
	}
	raw, err := os.ReadFile(filepath.Join(path, "SKILL.md"))
	if errors.Is(err, os.ErrNotExist) {
		printHubError(c, fmt.Sprintf("No SKILL.md found at %s", path))
		return nil
	}
	if err != nil {
		return err
	}
	if !utf8.Valid(raw) {
		return errors.New("UnicodeDecodeError: 'utf-8' codec can't decode SKILL.md")
	}
	skillMD := strings.TrimLeft(strings.TrimPrefix(string(raw), "\uFEFF"), "\uFEFF")
	fm, err := readPublishFrontmatter(skillMD)
	if err != nil {
		return err
	}
	name := pyrepr.Str(fmGet(fm, "name", filepath.Base(path)))
	if !pyrepr.Truthy(fmGet(fm, "description", "")) {
		printHubError(c, "SKILL.md must have a 'description' in frontmatter.")
		return nil
	}
	c.Print(fmt.Sprintf("[bold]Scanning '%s' before publish...[/]", name))
	result := ScanSkill(path, "self")
	c.Print(FormatScanReport(result))
	if result.Verdict == "dangerous" {
		c.Print("[bold red]Cannot publish a skill with DANGEROUS verdict.[/]\n")
		return nil
	}
	switch target {
	case "github":
		if repo == "" {
			printHubError(c, "--repo required for GitHub publish.\nUsage: icebath skills publish <path> --to github --repo owner/repo")
			return nil
		}
		auth := NewGitHubAuth(x.S)
		if !auth.IsAuthenticated() {
			printHubError(c, fmt.Sprintf("GitHub authentication required.\nSet GITHUB_TOKEN in %s/.env or run 'gh auth login'.", home.Display(x.S.Home)))
			return nil
		}
		c.Print(fmt.Sprintf("[bold]Publishing '%s' to %s...[/]", name, repo))
		ok, msg, err := x.githubPublish(ctx, path, name, repo, auth)
		if err != nil {
			return err
		}
		reportPair(c, ok, msg)
	case "clawhub":
		c.Print("[yellow]ClawHub publishing is not yet supported. Submit manually at https://clawhub.ai/submit[/]\n")
	default:
		c.Print(fmt.Sprintf("[bold red]Unknown target:[/] %s. Use 'github' or 'clawhub'.\n", target))
	}
	return nil
}

type ghResp struct {
	status int
	body   []byte
}

func (r ghResp) json() (any, error) { return pyjson.Loads(string(r.body)) }

// Forks, branches, uploads every file and opens a PR, transport failures becoming messages
func (x *HubCLI) githubPublish(ctx context.Context, skillPath, skillName, targetRepo string, auth *GitHubAuth) (bool, string, error) {
	headers := auth.GetHeaders()
	call := func(method, path string, timeout time.Duration, payload any) (ghResp, error) {
		var body io.Reader
		if payload != nil {
			b, err := json.Marshal(payload)
			if err != nil {
				return ghResp{}, err
			}
			body = bytes.NewReader(b)
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(cctx, method, x.GitHubAPI+"/"+path, body)
		if err != nil {
			return ghResp{}, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := x.HTTPClient.Do(req)
		if err != nil {
			return ghResp{}, err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		return ghResp{resp.StatusCode, data}, err
	}
	resp, err := call("POST", targetRepo+"/forks", 30*time.Second, nil)
	if err != nil {
		return false, "Network error forking repo: " + err.Error(), nil
	}
	var forkRepo string
	switch resp.status {
	case 200, 202:
		data, err := resp.json()
		if err != nil {
			return false, "", err
		}
		obj, _ := data.(*pyjson.Object)
		v, ok := objGet(obj, "full_name")
		if !ok {
			return false, "", keyError("full_name")
		}
		forkRepo = pyrepr.Str(v)
	case 403:
		return false, "GitHub token lacks permission to fork repos", nil
	default:
		return false, fmt.Sprintf("Failed to fork %s: %d", targetRepo, resp.status), nil
	}
	defaultBranch := "main"
	if r, err := call("GET", targetRepo, 15*time.Second, nil); err == nil {
		if data, err := r.json(); err == nil {
			if obj, ok := data.(*pyjson.Object); ok {
				defaultBranch = entryStr(obj, "default_branch", "main")
			}
		}
	}
	r, err := call("GET", forkRepo+"/git/refs/heads/"+defaultBranch, 15*time.Second, nil)
	baseSHA, berr := "", err
	if err == nil {
		baseSHA, berr = refSHA(r)
	}
	if berr != nil {
		return false, "Failed to get base branch: " + berr.Error(), nil
	}
	branch := "add-skill-" + skillName
	if _, err := call("POST", forkRepo+"/git/refs", 15*time.Second, map[string]string{"ref": "refs/heads/" + branch, "sha": baseSHA}); err != nil {
		return false, "Failed to create branch: " + err.Error(), nil
	}
	for _, f := range rglobFiles(skillPath) {
		rel, _ := filepath.Rel(skillPath, f)
		data, err := os.ReadFile(f)
		if err == nil {
			_, err = call("PUT", fmt.Sprintf("%s/contents/skills/%s/%s", forkRepo, skillName, rel), 15*time.Second, map[string]string{
				"message": fmt.Sprintf("Add %s skill: %s", skillName, rel), "content": base64.StdEncoding.EncodeToString(data), "branch": branch})
		}
		if err != nil {
			return false, fmt.Sprintf("Failed to upload %s: %s", rel, err.Error()), nil
		}
	}
	owner, _, _ := strings.Cut(forkRepo, "/")
	pr, err := call("POST", targetRepo+"/pulls", 15*time.Second, map[string]string{
		"title": "Add skill: " + skillName,
		"body":  fmt.Sprintf("Submitting the `%s` skill via Icebath Skills Hub.\n\nThis skill was scanned by the Icebath Skills Guard before submission.", skillName),
		"head":  owner + ":" + branch, "base": defaultBranch})
	if err != nil {
		return false, "Network error creating PR: " + err.Error(), nil
	}
	if pr.status == 201 {
		data, err := pr.json()
		if err != nil {
			return false, "", err
		}
		obj, _ := data.(*pyjson.Object)
		return true, "PR created: " + entryStr(obj, "html_url", ""), nil
	}
	text := []rune(pystr.DecodeUTF8Replace(pr.body))
	return false, fmt.Sprintf("Failed to create PR: %d %s", pr.status, string(text[:min(200, len(text))])), nil
}

// Commit SHA of a git ref response, the error naming the missing key
func refSHA(r ghResp) (string, error) {
	data, err := r.json()
	if err != nil {
		return "", err
	}
	obj, _ := data.(*pyjson.Object)
	o, ok := objGet(obj, "object")
	if !ok {
		return "", keyError("object")
	}
	sha, ok := objGet(func() *pyjson.Object { v, _ := o.(*pyjson.Object); return v }(), "sha")
	if !ok {
		return "", keyError("sha")
	}
	return pyrepr.Str(sha), nil
}

// Exports the hub skill configuration to a portable JSON file, a dash writing it to stdout
func (x *HubCLI) DoSnapshotExport(ctx context.Context, outputPath string, c *pyrich.Console) error {
	c = x.console(c)
	installed, err := x.Hub.LockFile("").ListInstalled()
	if err != nil {
		return err
	}
	taps, err := x.Hub.Taps("").ListTaps()
	if err != nil {
		return err
	}
	skills := make([]any, len(installed))
	for i, e := range installed {
		name, ok := objGet(e, "name")
		if !ok {
			return keyError("name")
		}
		ip, ok := objGet(e, "install_path")
		if !ok {
			return keyError("install_path")
		}
		category := ""
		if strings.Contains(entryStr(e, "install_path", ""), "/") {
			category = filepath.Dir(filepath.Clean(pyrepr.Str(ip)))
		}
		skills[i] = objOf("name", name, "source", fmGetObj(e, "source", ""), "identifier", fmGetObj(e, "identifier", ""), "category", category)
	}
	if taps == nil {
		taps = []any{}
	}
	snapshot := objOf("icebath_version", "0.1.0", "exported_at", pydatetime.ISOFormatUTC(x.Now().UTC()), "skills", skills, "taps", taps)
	payload, err := pyjson.Dumps(snapshot, pyjson.Indent(2), pyjson.EnsureASCII(false))
	if err != nil {
		return err
	}
	payload += "\n"
	if outputPath == "-" {
		_, err := io.WriteString(x.Stdout, payload)
		return err
	}
	if err := os.WriteFile(outputPath, []byte(payload), 0o666); err != nil {
		return err
	}
	c.Print(fmt.Sprintf("[bold green]Snapshot exported:[/] %s", filepath.Clean(outputPath)))
	c.Print(fmt.Sprintf("[dim]%d skill(s), %d tap(s)[/]\n", len(installed), len(taps)))
	return nil
}

func fmGetObj(o *pyjson.Object, key string, def any) any {
	if v, ok := objGet(o, key); ok {
		return v
	}
	return def
}

func asObject(v any) (*pyjson.Object, error) {
	o, ok := v.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("AttributeError: '%T' object has no attribute 'get'", v)
	}
	return o, nil
}

// Re-installs skills from a snapshot file, failing when it is unreadable or any entry failed
func (x *HubCLI) DoSnapshotImport(ctx context.Context, inputPath string, force bool, c *pyrich.Console) (HubOutcome, error) {
	c = x.console(c)
	inp := filepath.Clean(inputPath)
	raw, err := os.ReadFile(inp)
	if errors.Is(err, os.ErrNotExist) {
		printHubError(c, fmt.Sprintf("File not found: %s", inp))
		return HubFailed, nil
	}
	if err != nil {
		return HubNoop, err
	}
	if !utf8.Valid(raw) {
		return HubNoop, errors.New("UnicodeDecodeError: 'utf-8' codec can't decode the snapshot")
	}
	parsed, err := pyjson.Loads(strings.TrimPrefix(string(raw), "\uFEFF"))
	if err != nil {
		printHubError(c, fmt.Sprintf("Invalid JSON in %s", inp))
		return HubFailed, nil
	}
	snapshot, err := asObject(parsed)
	if err != nil {
		return HubNoop, err
	}
	taps, _ := fmGetObj(snapshot, "taps", []any{}).([]any)
	if len(taps) > 0 {
		mgr := x.Hub.Taps("")
		for _, t := range taps {
			tap, err := asObject(t)
			if err != nil {
				return HubNoop, err
			}
			if repo := entryStr(tap, "repo", ""); repo != "" {
				if _, err := mgr.Add(repo, entryStr(tap, "path", "skills/")); err != nil {
					return HubNoop, err
				}
			}
		}
		c.Print(fmt.Sprintf("[dim]Restored %d tap(s)[/]", len(taps)))
	}
	skills, _ := fmGetObj(snapshot, "skills", []any{}).([]any)
	if len(skills) == 0 {
		c.Print("[dim]No skills in snapshot to install.[/]\n")
		return HubNoop, nil
	}
	c.Print(fmt.Sprintf("[bold]Importing %d skill(s) from snapshot...[/]\n", len(skills)))
	var failed []string
	for _, s := range skills {
		entry, err := asObject(s)
		if err != nil {
			return HubNoop, err
		}
		identifier := entryStr(entry, "identifier", "")
		if identifier == "" {
			c.Print(fmt.Sprintf("[yellow]Skipping entry with no identifier: %s[/]", entryStr(entry, "name", "?")))
			continue
		}
		label := entryStr(entry, "name", identifier)
		c.Print(fmt.Sprintf("[bold]--- %s ---[/]", label))
		out, err := x.DoInstall(ctx, identifier, HubInstallOptions{Category: entryStr(entry, "category", ""), Force: force, Console: c})
		if err != nil {
			return HubNoop, err
		}
		if out == HubFailed {
			failed = append(failed, label)
		}
	}
	if len(failed) > 0 {
		c.Print(fmt.Sprintf("[bold red]Snapshot import incomplete.[/] Not installed: %s (see the messages above).\n", strings.Join(failed, ", ")))
		return HubFailed, nil
	}
	c.Print("[bold green]Snapshot import complete.[/]\n")
	return HubNoop, nil
}

// Parsed icebath skills command line, the argparse namespace skills_command reads
type SkillsArgs struct {
	Action                                                  string
	Page, Size, Limit                                       int
	Source, Query, Identifier, Category, Name               string
	Force, Yes, JSON, EnabledOnly, Deep, Restore, Remove    bool
	Sync                                                    bool
	SkillPath, To, Repo, SnapshotAction, Output, Input, Tap string
}

// Routes icebath skills subcommands and returns the exit code, 1 when a handler reported failure
func (x *HubCLI) SkillsCommand(ctx context.Context, a SkillsArgs) (int, error) {
	out, err := x.dispatchCLI(ctx, a)
	if err != nil {
		return 0, err
	}
	if out == HubFailed {
		return 1, nil
	}
	return 0, nil
}

func noop(err error) (HubOutcome, error) { return HubNoop, err }

func (x *HubCLI) dispatchCLI(ctx context.Context, a SkillsArgs) (HubOutcome, error) {
	switch a.Action {
	case "browse":
		x.DoBrowse(ctx, a.Page, a.Size, a.Source, nil)
		return HubNoop, nil
	case "search":
		return noop(x.DoSearch(ctx, a.Query, a.Source, a.Limit, nil, a.JSON))
	case "install":
		return x.DoInstall(ctx, a.Identifier, HubInstallOptions{Category: a.Category, Force: a.Force, SkipConfirm: a.Yes, NameOverride: a.Name})
	case "inspect":
		x.DoInspect(ctx, a.Identifier, nil)
		return HubNoop, nil
	case "list":
		return noop(x.DoList(ctx, a.Source, a.EnabledOnly, nil))
	case "check":
		return noop(x.DoCheck(ctx, a.Name, nil))
	case "update":
		return x.DoUpdate(ctx, a.Name, nil, a.Force)
	case "audit":
		return noop(x.DoAudit(ctx, a.Name, nil, a.Deep))
	case "uninstall":
		return x.DoUninstall(ctx, a.Name, nil, a.Yes, false)
	case "reset":
		return noop(x.DoReset(ctx, a.Name, a.Restore, nil, a.Yes, false))
	case "list-modified":
		return noop(x.DoListModified(ctx, nil, a.JSON))
	case "diff":
		return noop(x.DoDiff(ctx, a.Name, nil))
	case "opt-out":
		return noop(x.DoOptOut(ctx, a.Remove, nil, a.Yes, false))
	case "opt-in":
		return noop(x.DoOptIn(ctx, a.Sync, nil, false))
	case "repair-official":
		return noop(x.DoRepairOfficial(ctx, a.Name, a.Restore, nil, a.Yes, false))
	case "publish":
		to := a.To
		if to == "" {
			to = "github"
		}
		return noop(x.DoPublish(ctx, a.SkillPath, to, a.Repo, nil))
	case "snapshot":
		switch a.SnapshotAction {
		case "export":
			return noop(x.DoSnapshotExport(ctx, a.Output, nil))
		case "import":
			return x.DoSnapshotImport(ctx, a.Input, a.Force, nil)
		}
		x.C.Print("Usage: icebath skills snapshot [export|import]\n")
		return HubNoop, nil
	case "tap":
		if a.Tap == "" {
			x.C.Print("Usage: icebath skills tap [list|add|remove]\n")
			return HubNoop, nil
		}
		repo := a.Repo
		if repo == "" {
			repo = a.Name
		}
		return noop(x.DoTap(ctx, a.Tap, repo, nil))
	}
	x.C.Print("Usage: icebath skills [browse|search|install|inspect|list|list-modified|diff|check|update|audit|uninstall|reset|opt-out|opt-in|publish|snapshot|tap]\n")
	x.C.Print("Run 'icebath skills <command> --help' for details.\n")
	return HubNoop, nil
}

// Value after flag, the first occurrence or with last the final one, def when absent or trailing
func optValue(args []string, flag, def string, last bool) string {
	var hits []string
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			hits = append(hits, args[i+1])
		}
	}
	if len(hits) == 0 {
		return def
	}
	if last {
		return hits[len(hits)-1]
	}
	return hits[0]
}

// Python int() of text, def when it is no integer literal
func intOr(text string, def int) int {
	s := pystr.Strip(text)
	if strings.Contains(s, "_") {
		if strings.HasPrefix(s, "_") || strings.HasSuffix(s, "_") || strings.Contains(s, "__") {
			return def
		}
		s = strings.ReplaceAll(s, "_", "")
	}
	n, err := strconv.Atoi(strings.TrimPrefix(s, "+"))
	if err != nil || s == "" || strings.HasPrefix(s, "+-") || strings.HasPrefix(s, "+") && strings.HasPrefix(s[1:], "+") {
		return def
	}
	return n
}

func optInt(args []string, flag string, def int) int {
	return intOr(optValue(args, flag, strconv.Itoa(def), true), def)
}

func contains(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}

func (x *HubCLI) slashSearch(ctx context.Context, args []string, c *pyrich.Console) error {
	source, limit, asJSON := "all", 25, false
	var query []string
	for i := 0; i < len(args); {
		flag := args[i]
		hasValue := i+1 < len(args)
		takes := (flag == "--source" || flag == "--limit") && hasValue
		switch {
		case flag == "--source" && takes:
			source = args[i+1]
		case flag == "--limit" && takes:
			limit = intOr(args[i+1], limit)
		case flag == "--json":
			asJSON = true
		default:
			query = append(query, flag)
		}
		if takes {
			i += 2
		} else {
			i++
		}
	}
	return x.DoSearch(ctx, strings.Join(query, " "), source, limit, c, asJSON)
}

func (x *HubCLI) slashSnapshot(ctx context.Context, args []string, c *pyrich.Console) error {
	switch {
	case len(args) > 1 && args[0] == "export":
		return x.DoSnapshotExport(ctx, args[1], c)
	case len(args) > 1 && args[0] == "import":
		_, err := x.DoSnapshotImport(ctx, args[1], contains(args, "--force"), c)
		return err
	}
	c.Print("[bold red]Usage:[/] /skills snapshot export <file> | /skills snapshot import <file>\n")
	return nil
}

func firstPositional(args []string) string {
	if len(args) > 0 && !strings.HasPrefix(args[0], "--") {
		return args[0]
	}
	return ""
}

var slashUsage = map[string][]string{
	"search":    {"[bold red]Usage:[/] /skills search <query> [--source skills-sh|github|official|nvidia|openai|anthropic|huggingface] [--limit N] [--json]\n"},
	"install":   {"[bold red]Usage:[/] /skills install <identifier-or-url> [--name <name>] [--category <cat>] [--force] [--now]\n"},
	"inspect":   {"[bold red]Usage:[/] /skills inspect <identifier>\n"},
	"uninstall": {"[bold red]Usage:[/] /skills uninstall <name> [--now]\n"},
	"reset": {"[bold red]Usage:[/] /skills reset <name> [--restore] [--now]\n",
		"[dim]Clears the bundled-skills manifest entry so future updates stop marking it as user-modified.[/]",
		"[dim]Pass --restore to also replace the current copy with the bundled version.[/]\n"},
	"diff":    {"[bold red]Usage:[/] /skills diff <name>\n"},
	"publish": {"[bold red]Usage:[/] /skills publish <skill-path> [--to github] [--repo owner/repo]\n"},
}

var slashActions = map[string]bool{"browse": true, "search": true, "install": true, "inspect": true, "list": true, "check": true,
	"update": true, "audit": true, "uninstall": true, "reset": true, "list-modified": true, "modified": true, "diff": true,
	"publish": true, "snapshot": true, "tap": true, "help": true, "--help": true, "-h": true}

// Parses and runs a /skills slash command from chat, where every confirmation is skipped
func (x *HubCLI) HandleSkillsSlash(ctx context.Context, cmd string, c *pyrich.Console) error {
	c = x.console(c)
	parts := strings.Fields(cmd)
	if len(parts) > 0 && pystr.Lower(parts[0]) == "/skills" {
		parts = parts[1:]
	}
	if len(parts) == 0 {
		printSkillsHelp(c)
		return nil
	}
	action, args := pystr.Lower(parts[0]), parts[1:]
	if !slashActions[action] {
		c.Print(fmt.Sprintf("[bold red]Unknown action:[/] %s", action))
		printSkillsHelp(c)
		return nil
	}
	if usage, ok := slashUsage[action]; ok && len(args) == 0 {
		for _, l := range usage {
			c.Print(l)
		}
		return nil
	}
	now := contains(args, "--now")
	switch action {
	case "browse":
		x.DoBrowse(ctx, optInt(args, "--page", 1), optInt(args, "--size", 20), optValue(args, "--source", "all", true), c)
	case "search":
		return x.slashSearch(ctx, args, c)
	case "install":
		_, err := x.DoInstall(ctx, args[0], HubInstallOptions{Category: optValue(args, "--category", "", true), Force: contains(args, "--force"),
			SkipConfirm: true, DeferCache: !now, NameOverride: optValue(args, "--name", "", true), Console: c})
		return err
	case "inspect":
		x.DoInspect(ctx, args[0], c)
	case "list":
		return x.DoList(ctx, optValue(args, "--source", "all", false), contains(args, "--enabled-only") || contains(args, "--enabled"), c)
	case "check":
		name := ""
		if len(args) > 0 {
			name = args[0]
		}
		return x.DoCheck(ctx, name, c)
	case "update":
		name := ""
		for _, a := range args {
			if !strings.HasPrefix(a, "--") {
				name = a
				break
			}
		}
		_, err := x.DoUpdate(ctx, name, c, contains(args, "--force"))
		return err
	case "audit":
		return x.DoAudit(ctx, firstPositional(args), c, contains(args, "--deep"))
	case "uninstall":
		_, err := x.DoUninstall(ctx, args[0], c, true, !now)
		return err
	case "reset":
		return x.DoReset(ctx, args[0], contains(args, "--restore"), c, true, !now)
	case "list-modified", "modified":
		return x.DoListModified(ctx, c, contains(args, "--json"))
	case "diff":
		return x.DoDiff(ctx, args[0], c)
	case "publish":
		return x.DoPublish(ctx, args[0], optValue(args, "--to", "github", true), optValue(args, "--repo", "", true), c)
	case "snapshot":
		return x.slashSnapshot(ctx, args, c)
	case "tap":
		if len(args) == 0 {
			return x.DoTap(ctx, "list", "", c)
		}
		repo := ""
		if len(args) > 1 {
			repo = args[1]
		}
		return x.DoTap(ctx, args[0], repo, c)
	default:
		printSkillsHelp(c)
	}
	return nil
}

// Prints the /skills slash command help panel
func printSkillsHelp(c *pyrich.Console) {
	c.PrintPanel(pyrich.Panel{Title: "/skills", Body: "[bold]Skills Hub Commands:[/]\n\n" +
		"  [cyan]browse[/] [--source official]   Browse all available skills (paginated)\n" +
		"  [cyan]search[/] <query>              Search registries for skills\n" +
		"  [cyan]install[/] <identifier>        Install a skill (with security scan)\n" +
		"  [cyan]inspect[/] <identifier>        Preview a skill without installing\n" +
		"  [cyan]list[/] [--source hub|builtin|local] [--enabled-only]\n" +
		"       List installed skills; --enabled-only filters to the active profile's live set\n" +
		"  [cyan]check[/] [name]                Check hub skills for upstream updates\n" +
		"  [cyan]update[/] [name]               Update hub skills with upstream changes\n" +
		"  [cyan]audit[/] [name]                Re-scan hub skills for security\n" +
		"  [cyan]uninstall[/] <name>            Remove a hub-installed skill\n" +
		"  [cyan]list-modified[/]               List bundled skills you've edited (kept by update)\n" +
		"  [cyan]diff[/] <name>                 Diff your copy of a bundled skill vs the stock version\n" +
		"  [cyan]reset[/] <name> [--restore]    Reset bundled-skill tracking (fix 'user-modified' flag)\n" +
		"  [cyan]publish[/] <path> --repo <r>   Publish a skill to GitHub via PR\n" +
		"  [cyan]snapshot[/] export|import      Export/import skill configurations\n" +
		"  [cyan]tap[/] list|add|remove         Manage skill sources\n"})
}
