package tuiserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Lines a console prints, kept as plain text for a screen to show
type capturedLines struct {
	mu    sync.Mutex
	lines []string
}

// Console that keeps what it prints instead of writing it to a terminal
func (c *capturedLines) console() *pyrich.Console {
	return &pyrich.Console{Width: 100, Capture: func(raw string) {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, line := range strings.Split(strings.TrimRight(pyrich.Plain(raw), "\n"), "\n") {
			c.lines = append(c.lines, strings.TrimRight(line, " "))
		}
	}}
}

// Writer of plain text lines, for commands that print to a writer
func (c *capturedLines) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(pyrich.Plain(string(p)), "\n"), "\n") {
		c.lines = append(c.lines, strings.TrimRight(line, " "))
	}
	return len(p), nil
}

// Lines printed so far, blank lines at either end dropped
func (c *capturedLines) text() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	start, end := 0, len(c.lines)
	for start < end && strings.TrimSpace(c.lines[start]) == "" {
		start++
	}
	for end > start && strings.TrimSpace(c.lines[end-1]) == "" {
		end--
	}
	return append([]string{}, c.lines[start:end]...)
}

// Installed skills with their source, trust and enabled state, the extra sources, the bundles and the trusted projects of the profile
func skillsHubList(ctx context.Context, rid any, _ map[string]any, _ *tuihost.Session) (map[string]any, error) {
	x := skillsHubCLI(ctx)
	if err := x.Hub.EnsureHubDirs(); err != nil {
		return nil, err
	}
	hubRows, err := x.Hub.LockFile("").ListInstalled()
	if err != nil {
		return nil, err
	}
	fromHub := map[string]*pyjson.Object{}
	for _, row := range hubRows {
		if name := pyrepr.Str(row.GetOr("name", "")); name != "" {
			fromHub[name] = row
		}
	}
	bundled, err := x.S.ReadBundledNames()
	if err != nil {
		return nil, err
	}
	disabled := x.S.DisabledSkillNames("")
	text := func(o *pyjson.Object, key string) string {
		if o == nil {
			return ""
		}
		if v := o.GetOr(key, nil); pyrepr.Truthy(v) {
			return pyrepr.Str(v)
		}
		return ""
	}
	out := tuigateway.SkillsHubResult{Skills: []tuigateway.SkillHubRow{}, Taps: []tuigateway.SkillTapRow{}, Bundles: []tuigateway.SkillBundleRow{}, Trusted: []string{}}
	// The catalog keeps disabled skills in when told to skip the disabled filter
	for _, s := range skills.SortSkills(x.S.FindAllSkills(true)) {
		name := pyrepr.Str(s["name"])
		row := tuigateway.SkillHubRow{Name: name, Enabled: !disabled[name], Essential: skills.EssentialSkills[name], Source: "local"}
		if c, ok := s["category"].(string); ok {
			row.Category = c
		}
		if d, ok := s["description"].(string); ok {
			row.Description = d
		}
		switch entry := fromHub[name]; {
		case entry != nil:
			row.Source, row.Origin, row.Trust = "hub", text(entry, "source"), text(entry, "trust_level")
		case bundled[name]:
			row.Source = "builtin"
		}
		out.Skills = append(out.Skills, row)
	}
	taps, err := x.Hub.Taps("").ListTaps()
	if err != nil {
		return nil, err
	}
	for _, t := range taps {
		obj, isObj := t.(*pyjson.Object)
		if !isObj {
			return nil, fmt.Errorf("a skill source entry is %T, expected a mapping", t)
		}
		out.Taps = append(out.Taps, tuigateway.SkillTapRow{Name: text(obj, "repo"), Path: text(obj, "path")})
	}
	bundles, err := x.S.ListBundles()
	if err != nil {
		return nil, err
	}
	for _, b := range bundles {
		row := tuigateway.SkillBundleRow{Name: pyrepr.Str(b["slug"]), Description: pyrepr.Str(b["description"]), Instruction: pyrepr.Str(b["instruction"]),
			Path: pyrepr.Str(b["path"]), Skills: []string{}}
		if members, ok := b["skills"].([]any); ok {
			for _, m := range members {
				row.Skills = append(row.Skills, pyrepr.Str(m))
			}
		}
		out.Bundles = append(out.Bundles, row)
	}
	for _, t := range settings.SkillsTrustedProjectDirs.Get(ctx) {
		out.Trusted = append(out.Trusted, pyrepr.Str(t))
	}
	sort.Strings(out.Trusted)
	if _, err := os.Stat(filepath.Join(x.S.SkillsDir(), skills.NoBundledSkillsMarker)); err == nil {
		out.OptedOut = true
	}
	return ok(rid, tuigateway.Plain(out)), nil
}

// Verbs of skills.hub.run that need a skill name
var skillsRunNeedsName = map[string]bool{"uninstall": true, "reset": true, "diff": true, "repair-official": true}

// Runs one skills command the way icebath skills runs it, without its questions, answering what it printed
func skillsHubRun(ctx context.Context, rid any, params map[string]any, _ *tuihost.Session) (map[string]any, error) {
	p, err := tuigateway.SkillsHubRun.Params(params)
	if err != nil {
		return nil, err
	}
	if skillsRunNeedsName[p.Action] && strings.TrimSpace(p.Name) == "" {
		return rpcErr(rid, 4019, p.Action+" needs a skill name", nil), nil
	}
	captured := &capturedLines{}
	h := home.Dir(ctx)
	switch p.Action {
	case "trust", "untrust":
		err := skills.SkillsTrustCommand(ctx, h, p.Action, p.Path, captured)
		lines := captured.text()
		if err != nil {
			lines = append(lines, err.Error())
		}
		return ok(rid, tuigateway.Plain(tuigateway.SkillsHubRunResult{OK: err == nil, Lines: lines})), nil
	}
	a := skills.SkillsArgs{Action: p.Action, Name: p.Name, Identifier: p.Identifier, Category: p.Category, Force: p.Force, Yes: true,
		Restore: p.Restore, Sync: p.Sync, Remove: p.Remove, Deep: p.Deep, Repo: p.Repo, To: p.To, SkillPath: p.Path, Source: "all"}
	switch p.Action {
	case "install":
		if strings.TrimSpace(p.Identifier) == "" {
			return rpcErr(rid, 4019, "install needs the skill to install", nil), nil
		}
	case "check", "update", "audit", "uninstall", "reset", "list-modified", "diff", "opt-out", "opt-in", "repair-official", "publish":
	case "snapshot-export":
		a.Action, a.SnapshotAction, a.Output = "snapshot", "export", p.Path
	case "snapshot-import":
		a.Action, a.SnapshotAction, a.Input = "snapshot", "import", p.Path
	case "tap-add", "tap-remove":
		a.Action, a.Tap = "tap", strings.TrimPrefix(p.Action, "tap-")
		if strings.TrimSpace(p.Repo) == "" {
			return rpcErr(rid, 4019, "a skill source needs its repo", nil), nil
		}
	default:
		return rpcErr(rid, 4017, "unknown skills action: "+p.Action, nil), nil
	}
	x := skillsHubCLI(ctx)
	x.C = captured.console()
	refused := func(string) (string, error) { return "", fmt.Errorf("the skills screen asks no questions") }
	x.Input, x.LineInput = refused, refused
	code, err := x.SkillsCommand(ctx, a)
	lines := captured.text()
	if err != nil {
		lines = append(lines, err.Error())
	}
	return ok(rid, tuigateway.Plain(tuigateway.SkillsHubRunResult{OK: err == nil && code == 0, Lines: lines})), nil
}

// Writes a skill bundle through the same save the bundles command uses
func skillsBundleSave(ctx context.Context, rid any, params map[string]any, _ *tuihost.Session) (map[string]any, error) {
	p, err := tuigateway.SkillsBundleSave.Params(params)
	if err != nil {
		return nil, err
	}
	var members []string
	for _, s := range p.Skills {
		if s = strings.TrimSpace(s); s != "" {
			members = append(members, s)
		}
	}
	if strings.TrimSpace(p.Name) == "" || len(members) == 0 {
		return rpcErr(rid, 4019, "a bundle needs a name and at least one skill", nil), nil
	}
	path, err := skills.NewScope(home.Dir(ctx), nil).SaveBundle(ctx, strings.TrimSpace(p.Name), members, strings.TrimSpace(p.Description), strings.TrimSpace(p.Instruction), p.Force)
	if err != nil {
		return rpcErr(rid, 4021, err.Error(), nil), nil
	}
	return ok(rid, tuigateway.Plain(tuigateway.SkillsBundleResult{Path: path})), nil
}

// Removes a skill bundle
func skillsBundleDelete(ctx context.Context, rid any, params map[string]any, _ *tuihost.Session) (map[string]any, error) {
	p, err := tuigateway.SkillsBundleDelete.Params(params)
	if err != nil {
		return nil, err
	}
	path, err := skills.NewScope(home.Dir(ctx), nil).DeleteBundle(strings.TrimSpace(p.Name))
	if err != nil {
		return rpcErr(rid, 4021, err.Error(), nil), nil
	}
	return ok(rid, tuigateway.Plain(tuigateway.SkillsBundleResult{Path: path})), nil
}

// Reads the bundles folder again and answers the bundles it found added and removed
func skillsBundleReload(ctx context.Context, rid any, _ map[string]any, _ *tuihost.Session) (map[string]any, error) {
	diff, err := skills.NewScope(home.Dir(ctx), nil).ReloadBundles()
	if err != nil {
		return nil, err
	}
	names := func(key string) []string {
		out := []string{}
		if rows, ok := diff[key].([]map[string]any); ok {
			for _, r := range rows {
				out = append(out, pyrepr.Str(r["name"]))
			}
		}
		return out
	}
	total, _ := diff["total"].(int)
	return ok(rid, tuigateway.Plain(tuigateway.SkillsBundleReloadResult{Added: names("added"), Removed: names("removed"), Total: int64(total)})), nil
}

var hubSkillsRegistry = &handlerRegistry{}

func init() {
	scopedRPC(hubSkillsRegistry, tuigateway.SkillsHubList.Name(), 5024, rpcShape{}, skillsHubList)
	scopedRPC(hubSkillsRegistry, tuigateway.SkillsHubRun.Name(), 5024, rpcShape{}, skillsHubRun)
	scopedRPC(hubSkillsRegistry, tuigateway.SkillsBundleSave.Name(), 5024, rpcShape{}, skillsBundleSave)
	scopedRPC(hubSkillsRegistry, tuigateway.SkillsBundleDelete.Name(), 5024, rpcShape{}, skillsBundleDelete)
	scopedRPC(hubSkillsRegistry, tuigateway.SkillsBundleReload.Name(), 5024, rpcShape{}, skillsBundleReload)
	hubSkillsRegistry.install()
	addLongHandlers(tuigateway.SkillsHubRun.Name())
}
