package server

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	hub "github.com/nickheyer/icebath/internal/plugins/defaults/skills/skills/skills"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/skills"
	skillsconfig "github.com/nickheyer/icebath/internal/skills/config"
	skillsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/skills/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/skills/v1/skillsv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
)

type service struct {
	config *config.Config
	skills *skills.Registry
	taken  skills.Taken
	// The skills hub: its registries, its guard, and the skills it installs
	hub *hub.Hub
}

func Register(registry *server.Registry, cfg *config.Config, skillset *skills.Registry, taken skills.Taken, skillsHub *hub.Hub) {
	registry.Register(&service{config: cfg, skills: skillset, taken: taken, hub: skillsHub})
}

func (s *service) Mount(services *connect.Server) {
	skillsv1connect.RegisterSkillsServiceHandler(services, s)
}

// Scope a request is about, the profile's built in skills synced
func (s *service) scope(ctx context.Context, scope *skillsv1.Scope) (*skills.Scope, error) {
	cfg, err := server.Profile(s.config, scope.GetProfile())
	if err != nil {
		return nil, err
	}
	out := s.skills.Scope(cfg, scope.GetDirectory())
	return out, out.Seed(ctx)
}

// Where a skill comes from and how far it is trusted
func source(e skills.Entry, hub *skills.Lock, manifest map[string]bool) (string, string) {
	if entry := hub.Installed[e.Load]; entry != nil {
		return "hub", cmp.Or(entry.Trust, "community")
	}
	switch e.Tier {
	case skills.TierProject:
		return "project", "project"
	case skills.TierCreate:
		return "create_dir", "local"
	case skills.TierExternal:
		return "external", "external"
	}
	if manifest[e.Load] {
		return "builtin", "builtin"
	}
	return "local", "local"
}

func (s *service) List(ctx context.Context, request *skillsv1.ListRequest) (*skillsv1.ListResponse, error) {
	scope, err := s.scope(ctx, request.GetScope())
	if err != nil {
		return nil, err
	}
	hub, err := skills.ReadLock(scope.Dir())
	if err != nil {
		return nil, err
	}
	builtin, err := scope.Builtin()
	if err != nil {
		return nil, err
	}
	modified, err := scope.Modified()
	if err != nil {
		return nil, err
	}
	usage, err := scope.Usage()
	if err != nil {
		return nil, err
	}
	commands := scope.Commands(s.taken)
	global := s.disabled(scope, "")
	perSurface := map[string]map[string]bool{}
	response := &skillsv1.ListResponse{CreateDir: scope.CreateDisplay(), SkillsDir: scope.Dir(), Trusted: scope.TrustedDirs(),
		Secrets: int32(len(scope.VariableSettings())), Values: int32(len(scope.ConfigSettings()))}
	for _, surface := range skills.Surfaces {
		perSurface[surface.Name] = s.disabled(scope, surface.Name)
		response.Surfaces = append(response.Surfaces, &skillsv1.Surface{Name: surface.Name, Title: surface.Title})
	}
	project, trusted, held := scope.Project()
	response.Project, response.ProjectTrusted, response.ProjectSkills = project, trusted, int32(held)
	if _, err := os.Stat(scope.OptOutMarker()); err == nil {
		response.OptedOut = true
	}
	surface := request.GetSurface()
	for _, e := range scope.Loadable(true) {
		row := s.row(e, hub, builtin, modified, usage, commands, global, perSurface)
		row.Shown = scope.State(e.Load, e.Name, surface)
		response.Skills = append(response.Skills, row)
	}
	for _, p := range scope.Plugins() {
		if !skills.Supported(p.Frontmatter) {
			continue
		}
		name := p.Qualified()
		row := &skillsv1.Skill{Name: name, Description: p.Description, Category: "plugin", CategoryTitle: skills.CategoryTitle("plugin"), Source: "plugin",
			Trust: "plugin", Enabled: !global[name], Path: p.Path, Shown: scope.State(name, name, surface)}
		for _, known := range skills.Surfaces {
			if perSurface[known.Name][name] {
				row.DisabledOn = append(row.DisabledOn, known.Name)
			}
		}
		if _, ok := commands[strings.ToLower(name)]; ok {
			row.Command = strings.ToLower(name)
		}
		response.Skills = append(response.Skills, row)
	}
	response.Categories = categories(response.Skills)
	for _, category := range response.Categories {
		category.About = scope.CategoryAbout(category.GetName())
	}
	return response, nil
}

// Categories of the skills in the order they are listed, those in none last, each with how many of its skills are on in the app asked about
func categories(rows []*skillsv1.Skill) []*skillsv1.Category {
	byName := map[string]*skillsv1.Category{}
	for _, row := range rows {
		category := byName[row.GetCategory()]
		if category == nil {
			category = &skillsv1.Category{Name: row.GetCategory(), Title: row.GetCategoryTitle()}
			byName[row.GetCategory()] = category
		}
		category.Total++
		if shown := row.GetShown(); shown == skills.On || shown == skills.AlwaysOn {
			category.On++
		}
	}
	out := slices.Collect(maps.Values(byName))
	slices.SortFunc(out, func(a, b *skillsv1.Category) int {
		switch {
		case a.GetName() == "":
			return 1
		case b.GetName() == "":
			return -1
		}
		return strings.Compare(a.GetTitle(), b.GetTitle())
	})
	for _, category := range out {
		category.State = skills.CategoryState(int(category.GetOn()), int(category.GetTotal()))
	}
	return out
}

// Skills turned off everywhere, or on the surface alone
func (s *service) disabled(scope *skills.Scope, surface string) map[string]bool {
	if surface == "" {
		return namesSet(scope.Config.GetStringSlice(skillsconfig.Disabled))
	}
	table, _ := scope.Config.Get(skillsconfig.PlatformDisabled).(map[string]any)
	switch list := table[surface].(type) {
	case []any:
		out := map[string]bool{}
		for _, item := range list {
			if name, ok := item.(string); ok {
				out[strings.TrimSpace(name)] = true
			}
		}
		return out
	case string:
		return map[string]bool{strings.TrimSpace(list): true}
	}
	return map[string]bool{}
}

func namesSet(names []string) map[string]bool {
	out := map[string]bool{}
	for _, name := range names {
		out[strings.TrimSpace(name)] = true
	}
	return out
}

func (s *service) row(e skills.Entry, hub *skills.Lock, builtin map[string]bool, modified []string, usage map[string]*skills.Record, commands map[string]skills.Command, global map[string]bool, perSurface map[string]map[string]bool) *skillsv1.Skill {
	src, trust := source(e, hub, builtin)
	row := &skillsv1.Skill{
		Name:          e.Load,
		Description:   strings.Join(strings.Fields(e.Description), " "),
		Category:      e.Category,
		CategoryTitle: skills.CategoryTitle(e.Category),
		Source:        src,
		Trust:         trust,
		Enabled:       skills.Essential[e.Load] || !global[e.Load] && !global[e.Name],
		Path:          e.Path,
		Essential:     skills.Essential[e.Load],
		Modified:      slices.Contains(modified, e.Load),
	}
	for _, surface := range skills.Surfaces {
		if perSurface[surface.Name][e.Load] || perSurface[surface.Name][e.Name] {
			row.DisabledOn = append(row.DisabledOn, surface.Name)
		}
	}
	slug := skills.Slug(e.Name)
	if command, ok := commands[slug]; ok && command.Name == e.Name {
		row.Command = slug
	} else if slug != "" && s.taken != nil && s.taken(slug) {
		row.CommandNote = "Its slash command /" + slug + " is taken by another command"
	} else if slug != "" && row.Enabled {
		row.CommandNote = "Its slash command /" + slug + " is taken by another skill"
	}
	if record := usage[e.Load]; record != nil {
		row.UseCount, row.ViewCount, row.PatchCount = record.UseCount, record.ViewCount, record.PatchCount
		row.LastActivity, row.State, row.Pinned, row.Locked = record.Activity(), record.State, record.Pinned, record.Locked
	}
	return row
}

func (s *service) Show(ctx context.Context, request *skillsv1.ShowRequest) (*skillsv1.ShowResponse, error) {
	scope, err := s.scope(ctx, request.GetScope())
	if err != nil {
		return nil, err
	}
	view := scope.View(ctx, request.GetName(), "", false)
	if !view.OK() {
		return nil, connect.NewError(connect.CodeNotFound, view.Text("error"))
	}
	response := &skillsv1.ShowResponse{
		Skill:     &skillsv1.Skill{Name: view.Text("name"), Description: textOf(view["description"])},
		Content:   view.Text("content"),
		Dir:       view.Text("skill_dir"),
		Readiness: view.Text("readiness_status"),
		SetupNote: view.Text("setup_note"),
	}
	response.Tags, _ = view["tags"].([]string)
	response.Related, _ = view["related_skills"].([]string)
	if linked, ok := view["linked_files"].(map[string][]string); ok {
		for _, group := range []string{"references", "templates", "assets", "scripts"} {
			response.Files = append(response.Files, linked[group]...)
		}
	}
	listed, err := s.List(ctx, &skillsv1.ListRequest{Scope: request.GetScope()})
	if err != nil {
		return nil, err
	}
	for _, row := range listed.GetSkills() {
		if row.GetName() == request.GetName() || row.GetName() == response.Skill.GetName() {
			response.Skill = row
			break
		}
	}
	skillMD := filepath.Join(response.GetDir(), "SKILL.md")
	if response.GetDir() == "" {
		skillMD = response.Skill.GetPath()
	}
	if response.GetDir() != "" {
		blueprint, err := scope.Blueprint(filepath.Base(response.GetDir()))
		switch {
		case err != nil:
			response.BlueprintError = err.Error()
		case blueprint != nil:
			response.Blueprint = &skillsv1.Blueprint{Schedule: blueprint.Schedule, Deliver: blueprint.Deliver, NoAgent: blueprint.NoAgent, Toolsets: blueprint.Toolsets}
			if blueprint.Prompt != nil {
				response.Blueprint.Prompt = *blueprint.Prompt
			}
			if blueprint.Model != nil {
				response.Blueprint.Model = *blueprint.Model
			}
			if blueprint.Provider != nil {
				response.Blueprint.Provider = *blueprint.Provider
			}
		}
	}
	if findings, err := skills.LintFile(skillMD); err == nil {
		for _, f := range findings {
			response.Findings = append(response.Findings, &skillsv1.Finding{Severity: f.Severity, Rule: f.Rule, Message: f.Message})
		}
	}
	return response, nil
}

func textOf(v any) string {
	text, _ := v.(string)
	return text
}

func (s *service) Toggle(ctx context.Context, request *skillsv1.ToggleRequest) (*skillsv1.ToggleResponse, error) {
	scope, err := s.scope(ctx, request.GetScope())
	if err != nil {
		return nil, err
	}
	changed, err := scope.SetDisabled(request.GetNames(), request.GetEnabled(), request.GetSurface())
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	scope.Platform = request.GetSurface()
	disabled := scope.Disabled()
	response := &skillsv1.ToggleResponse{Changed: changed, Said: turned(request, changed) + s.held(scope, request)}
	for _, e := range scope.Loadable(true) {
		if disabled[e.Load] || disabled[e.Name] {
			response.Disabled++
		} else {
			response.Enabled++
		}
	}
	return response, nil
}

// What turning skills on or off did, in a sentence: the one skill asked about, or how many changed and which
func turned(request *skillsv1.ToggleRequest, changed []string) string {
	state, place := "off", ""
	if request.GetEnabled() {
		state = "on"
	}
	for _, known := range skills.Surfaces {
		if known.Name == request.GetSurface() {
			place = " in the " + strings.ToLower(known.Title)
		}
	}
	names := request.GetNames()
	switch {
	case len(names) == 1 && len(changed) == 0:
		return names[0] + " is " + state + place + " already."
	case len(names) == 1:
		return names[0] + " is now " + state + place + "."
	case len(changed) == 0:
		return "Those skills are " + state + place + " already."
	}
	return "Turned " + state + " " + strs.Count(len(changed), "skill") + place + ": " + strs.And(changed) + "."
}

// What still keeps skills that were turned on from the agent, as a sentence after what was done, empty when nothing does: one turned on in
// every app stays off in an app it was turned off in alone, and one turned on in one app stays off while it is off in every app
func (s *service) held(scope *skills.Scope, request *skillsv1.ToggleRequest) string {
	if !request.GetEnabled() {
		return ""
	}
	names := request.GetNames()
	among := func(off map[string]bool) bool {
		return slices.ContainsFunc(names, func(name string) bool {
			return off[strings.TrimSpace(name)] && !skills.Essential[strings.TrimSpace(name)]
		})
	}
	if request.GetSurface() != "" {
		switch {
		case !among(s.disabled(scope, "")):
			return ""
		case len(names) == 1:
			return " It stays off all the same, because it is off in every app."
		}
		return " Some of them stay off all the same, because they are off in every app."
	}
	var places []string
	for _, known := range skills.Surfaces {
		if among(s.disabled(scope, known.Name)) {
			places = append(places, "the "+strings.ToLower(known.Title))
		}
	}
	switch {
	case len(places) == 0:
		return ""
	case len(names) == 1:
		return " It stays off in " + strs.And(places) + ", where it was turned off alone."
	}
	return " Some of them stay off in " + strs.And(places) + ", where they were turned off alone."
}

func synced(result *skills.Synced) *skillsv1.Synced {
	if result == nil {
		return nil
	}
	return &skillsv1.Synced{
		Copied: result.Copied, Updated: result.Updated, Skipped: int32(result.Skipped), Modified: result.Modified, Cleaned: result.Cleaned,
		Suppressed: result.Suppressed, Relocated: result.Relocated, Deferred: result.Deferred, Backfilled: result.Backfilled,
		Total: int32(result.Total), OptedOut: result.OptedOut, Lines: result.Lines,
	}
}

func (s *service) Reset(ctx context.Context, request *skillsv1.ResetRequest) (*skillsv1.ResetResponse, error) {
	scope, err := s.scope(ctx, request.GetScope())
	if err != nil {
		return nil, err
	}
	message, result, err := scope.Reset(ctx, request.GetName(), request.GetRestore())
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return &skillsv1.ResetResponse{Message: message, Synced: synced(result)}, nil
}

func (s *service) Modified(ctx context.Context, request *skillsv1.ModifiedRequest) (*skillsv1.ModifiedResponse, error) {
	scope, err := s.scope(ctx, request.GetScope())
	if err != nil {
		return nil, err
	}
	names, err := scope.Modified()
	if err != nil {
		return nil, err
	}
	return &skillsv1.ModifiedResponse{Names: names}, nil
}

func (s *service) Diff(ctx context.Context, request *skillsv1.DiffRequest) (*skillsv1.DiffResponse, error) {
	scope, err := s.scope(ctx, request.GetScope())
	if err != nil {
		return nil, err
	}
	diffs, err := scope.Diff(request.GetName())
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	response := &skillsv1.DiffResponse{}
	for _, d := range diffs {
		response.Files = append(response.Files, &skillsv1.FileDiff{Path: d.Path, Status: d.Status, Diff: d.Diff})
	}
	return response, nil
}

func (s *service) OptOut(ctx context.Context, request *skillsv1.OptOutRequest) (*skillsv1.OptOutResponse, error) {
	scope, err := s.scope(ctx, request.GetScope())
	if err != nil {
		return nil, err
	}
	response := &skillsv1.OptOutResponse{Marker: scope.OptOutMarker()}
	if request.GetOut() && request.GetDry() {
		removed, kept, err := scope.RemovePristine(true)
		if err != nil {
			return nil, err
		}
		response.Removed = removed
		for _, k := range kept {
			response.Kept = append(response.Kept, &skillsv1.KeptSkill{Name: k.Name, Reason: k.Reason})
		}
		return response, nil
	}
	if response.Message, response.Changed, err = scope.OptOut(request.GetOut()); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	switch {
	case request.GetOut() && request.GetRemove():
		removed, kept, err := scope.RemovePristine(false)
		if err != nil {
			return nil, err
		}
		response.Removed = removed
		for _, k := range kept {
			response.Kept = append(response.Kept, &skillsv1.KeptSkill{Name: k.Name, Reason: k.Reason})
		}
	case !request.GetOut() && request.GetSync():
		result, err := scope.Sync(ctx)
		if err != nil {
			return nil, err
		}
		response.Synced = synced(result)
	}
	return response, nil
}

func (s *service) Repair(ctx context.Context, request *skillsv1.RepairRequest) (*skillsv1.RepairResponse, error) {
	scope, err := s.scope(ctx, request.GetScope())
	if err != nil {
		return nil, err
	}
	result, err := scope.Repair(request.GetName(), request.GetRestore())
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return &skillsv1.RepairResponse{Restored: result.Restored, Backfilled: result.Backfilled, BackedUp: result.BackedUp, BackupDir: result.BackupDir}, nil
}

func (s *service) Reload(ctx context.Context, request *skillsv1.ReloadRequest) (*skillsv1.ReloadResponse, error) {
	scope, err := s.scope(ctx, request.GetScope())
	if err != nil {
		return nil, err
	}
	reloaded, err := scope.Reload(s.taken)
	if err != nil {
		return nil, err
	}
	response := &skillsv1.ReloadResponse{Total: int32(reloaded.Total)}
	for _, c := range reloaded.Added {
		response.Added = append(response.Added, &skillsv1.Command{Name: c.Key, Description: c.Description})
	}
	for _, c := range reloaded.Removed {
		response.Removed = append(response.Removed, &skillsv1.Command{Name: c.Key, Description: c.Description})
	}
	return response, nil
}

func bundle(b skills.Bundle) *skillsv1.Bundle {
	return &skillsv1.Bundle{Name: b.Name, Slug: b.Slug, Skills: b.Skills, Path: b.Path, Description: b.Description, Instruction: b.Instruction}
}

func (s *service) Bundles(ctx context.Context, request *skillsv1.BundlesRequest) (*skillsv1.BundlesResponse, error) {
	scope, err := s.scope(ctx, request.GetScope())
	if err != nil {
		return nil, err
	}
	list, err := scope.BundleList()
	if err != nil {
		return nil, err
	}
	response := &skillsv1.BundlesResponse{Dir: scope.BundlesDir()}
	for _, b := range list {
		response.Bundles = append(response.Bundles, bundle(b))
	}
	return response, nil
}

func (s *service) SaveBundle(ctx context.Context, request *skillsv1.SaveBundleRequest) (*skillsv1.SaveBundleResponse, error) {
	scope, err := s.scope(ctx, request.GetScope())
	if err != nil {
		return nil, err
	}
	if _, err := scope.SaveBundle(request.GetName(), request.GetSkills(), request.GetDescription(), request.GetInstruction(), request.GetOverwrite()); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, connect.NewError(connect.CodeAlreadyExists, err.Error())
		}
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	saved, _, err := scope.Bundle(request.GetName())
	if err != nil {
		return nil, err
	}
	return &skillsv1.SaveBundleResponse{Bundle: bundle(saved)}, nil
}

func (s *service) DeleteBundle(ctx context.Context, request *skillsv1.DeleteBundleRequest) (*skillsv1.DeleteBundleResponse, error) {
	scope, err := s.scope(ctx, request.GetScope())
	if err != nil {
		return nil, err
	}
	path, err := scope.DeleteBundle(request.GetName())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, connect.NewError(connect.CodeNotFound, err.Error())
	}
	if err != nil {
		return nil, err
	}
	return &skillsv1.DeleteBundleResponse{Path: path}, nil
}
