package dashboard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Labels per hub source id, matching the provenance the skills search command prints
var skillHubSourceLabels = map[string]string{
	"official": "Official (Nous)", "hermes-index": "Hermes Index", "skills-sh": "skills.sh", "well-known": "Well-Known",
	"url": "Direct URL", "github": "GitHub", "clawhub": "ClawHub", "lobehub": "LobeHub", "browse-sh": "browse.sh",
}

// Sources an available hermes index subsumes, left out of the per source fan out
var skillHubAPISourceIDs = map[string]bool{"github": true, "skills-sh": true, "clawhub": true, "lobehub": true, "well-known": true}

// Hub lookups of the skills hub routes, the module attributes hermes tests patch
var (
	hubCreateSourceRouter = func(ctx context.Context) ([]skills.SkillSource, error) {
		return skillsHubOf(ctx).SourceRouter()
	}
	hubResolveSourceMetaAndBundle = skills.ResolveSourceMetaAndBundle
	hubListLocalOfficial          = func(ctx context.Context) ([]skills.SkillMeta, error) {
		return skills.NewOptionalSkillSource(skillsHubOf(ctx), nil).ListLocal()
	}
	hubInstalledIdentifiers = installedHubIdentifiers
	hubQuarantineBundle     = func(ctx context.Context, bundle *skills.SkillBundle) (string, error) {
		return skillsHubOf(ctx).QuarantineBundle(bundle)
	}
	hubScanSkill     = skills.ScanSkill
	hubRemoveScanDir = os.RemoveAll
)

// Skills scope of the home bound to ctx
func skillsScopeOf(ctx context.Context) *skills.Scope { return skills.NewScope(home.Dir(ctx), nil) }

// Skills hub of the home bound to ctx
func skillsHubOf(ctx context.Context) *skills.Hub { return skills.NewHub(skillsScopeOf(ctx)) }

// Source router built under the config scope of the profile
func hubSources(ctx context.Context, profile string) ([]skills.SkillSource, error) {
	sctx, _, err := configProfileScope(ctx, profile)
	if err != nil {
		return nil, err
	}
	return hubCreateSourceRouter(sctx)
}

// Meta and bundle of a hub identifier resolved under the config scope of the profile
func resolveHubSkill(ctx context.Context, ident, profile string) (*skills.SkillMeta, *skills.SkillBundle, error) {
	sctx, _, err := configProfileScope(ctx, profile)
	if err != nil {
		return nil, nil, err
	}
	sources, err := hubCreateSourceRouter(sctx)
	if err != nil {
		return nil, nil, err
	}
	meta, bundle, _ := hubResolveSourceMetaAndBundle(sctx, ident, sources)
	return meta, bundle, nil
}

// Truth of a source flag method, false when the source lacks it or it panics
func hubFlag(read func() bool) (out bool) {
	defer func() {
		if recover() != nil {
			out = false
		}
	}()
	return read()
}

// Optional text as a JSON value, null for none
func optTextValue(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// Hub listing row of one skill
func skillMetaToPayload(m skills.SkillMeta) *pyjson.Object {
	return pyjson.ObjectOf("name", m.Name, "description", m.Description, "source", m.Source, "identifier", m.Identifier,
		"trust_level", m.TrustLevel, "repo", optTextValue(m.Repo), "tags", stringsAny(m.Tags))
}

// Drops the skills system prompt snapshot after a write so the next session sees the change
func clearSkillsPromptCache(ctx context.Context) {
	defer func() { _ = recover() }()
	skills.ClearSkillsSystemPromptCache(home.Dir(ctx), true)
}

// Body profile when set, else the query profile
func skillsTargetProfile(body *pyjson.Object, profile string) string {
	if body != nil {
		if p, _ := body.GetOr("profile", nil).(string); p != "" {
			return p
		}
	}
	return profile
}

// Spawns the skills install of a hub identifier into the requested profile
func installSkillHub(ctx context.Context, body *pyjson.Object, profile string) (any, error) {
	identifier, err := require(bodyStr(body, "identifier"), "identifier is required")
	if err != nil {
		return nil, err
	}
	return spawnProfileAction(ctx, skillsTargetProfile(body, profile), []string{"skills", "install", identifier, "--yes"},
		hubActionName("install", identifier), "Failed to spawn skills install", "Failed to install skill")
}

// Spawns the skills uninstall of a named skill in the requested profile
func uninstallSkillHub(ctx context.Context, body *pyjson.Object, profile string) (any, error) {
	name, err := require(bodyStr(body, "name"), "name is required")
	if err != nil {
		return nil, err
	}
	return spawnProfileAction(ctx, skillsTargetProfile(body, profile), []string{"skills", "uninstall", name, "--yes"},
		hubActionName("uninstall", name), "Failed to spawn skills uninstall", "Failed to uninstall skill")
}

// Spawns the skills update of the requested profile
func updateSkillsHub(ctx context.Context, body *pyjson.Object, profile string) (any, error) {
	return spawnProfileAction(ctx, skillsTargetProfile(body, profile), []string{"skills", "update"}, "skills-update",
		"Failed to spawn skills update", "Failed to update skills")
}

// Whole optional skills catalog from the local scan, each row marked installed for the profile
func listOfficialSkills(ctx context.Context, profile string) (any, error) {
	var out any
	err := raised(func() error {
		installed, err := hubInstalledIdentifiers(ctx, profile)
		if err != nil {
			return err
		}
		metas, err := hubListLocalOfficial(ctx)
		if err != nil {
			return err
		}
		rows := []any{}
		for _, m := range metas {
			payload := skillMetaToPayload(m)
			ident := m.Identifier
			rel := ident
			if _, after, found := strings.Cut(ident, "/"); found {
				rel = after
			}
			category := "general"
			if before, _, found := strings.Cut(rel, "/"); found {
				category = before
			}
			payload.Set("category", category)
			_, held := installed.Get(ident)
			payload.Set("installed", held)
			rows = append(rows, payload)
		}
		out = pyjson.ObjectOf("skills", rows)
		return nil
	})
	if err != nil {
		return nil, httpFailure(err, "official skills catalog listing failed", 502, "Official catalog failed", nil)
	}
	return out, nil
}

// Configured hub sources with their flags, the featured index skills and the installed hub skills of the profile
func listSkillsHubSources(ctx context.Context, profile string) (any, error) {
	var out any
	err := raised(func() error {
		sources, err := hubSources(ctx, profile)
		if err != nil {
			return err
		}
		entries := []*pyjson.Object{}
		indexAvailable := false
		featured := []any{}
		for _, src := range sources {
			sid := src.SourceID()
			label, held := skillHubSourceLabels[sid]
			if !held {
				label = sid
			}
			entry := pyjson.ObjectOf("id", sid, "label", label)
			if sid == "github" {
				limited, has := src.(interface{ IsRateLimited() bool })
				entry.Set("rate_limited", has && hubFlag(limited.IsRateLimited))
			}
			if sid == "hermes-index" {
				available, has := src.(interface{ IsAvailable() bool })
				indexAvailable = has && hubFlag(available.IsAvailable)
				entry.Set("available", indexAvailable)
				if indexAvailable {
					featured = []any{}
					if raised(func() error {
						metas, err := src.Search(ctx, "", 12)
						if err != nil {
							return err
						}
						for _, m := range metas {
							featured = append(featured, skillMetaToPayload(m))
						}
						return nil
					}) != nil {
						featured = []any{}
					}
				}
			}
			entries = append(entries, entry)
		}
		rows := make([]any, len(entries))
		for i, entry := range entries {
			id, _ := entry.GetOr("id", "").(string)
			entry.Set("searchable", !(indexAvailable && skillHubAPISourceIDs[id]))
			rows[i] = entry
		}
		installed, err := hubInstalledIdentifiers(ctx, profile)
		if err != nil {
			return err
		}
		out = pyjson.ObjectOf("sources", rows, "index_available", indexAvailable, "featured", featured, "installed", installed)
		return nil
	})
	if err != nil {
		return nil, httpFailure(err, "skills hub sources listing failed", 502, "Hub sources failed", nil)
	}
	return out, nil
}

// Hub search across the configured sources, deduped by identifier with the higher trust level kept
func searchSkillsHub(ctx context.Context, q, source string, limit int64, profile string) (any, error) {
	query := pystr.Strip(q)
	if query == "" {
		return pyjson.ObjectOf("results", []any{}, "source_counts", pyjson.NewObject(), "timed_out", []any{}, "installed", pyjson.NewObject()), nil
	}
	var out any
	err := raised(func() error {
		sources, err := hubSources(ctx, profile)
		if err != nil {
			return err
		}
		capped := min(max(limit, 1), 50)
		filter := source
		if filter == "" {
			filter = "all"
		}
		all, counts, timedOut := skillsHubOf(ctx).ParallelSearchSources(ctx, sources, query, nil, filter, 30*time.Second, nil)
		rank := map[string]int{"builtin": 2, "trusted": 1, "community": 0}
		seen := map[string]skills.SkillMeta{}
		order := []string{}
		for _, r := range all {
			prev, held := seen[r.Identifier]
			if !held {
				order = append(order, r.Identifier)
			}
			if !held || rank[r.TrustLevel] > rank[prev.TrustLevel] {
				seen[r.Identifier] = r
			}
		}
		results := []any{}
		for i, id := range order {
			if int64(i) >= capped {
				break
			}
			results = append(results, skillMetaToPayload(seen[id]))
		}
		installed, err := hubInstalledIdentifiers(ctx, profile)
		if err != nil {
			return err
		}
		out = pyjson.ObjectOf("results", results, "source_counts", counts, "timed_out", stringsAny(timedOut), "installed", installed)
		return nil
	})
	if err != nil {
		return nil, httpFailure(err, "skills hub search failed", 502, "Hub search failed", nil)
	}
	return out, nil
}

// Result of a hub lookup, any failure a 502 with the prefix, a nil result a 404 naming the identifier
func hubLookup(fn func() (any, error), ident, logMsg, prefix string) (any, error) {
	var result any
	if err := raised(func() error {
		var err error
		result, err = fn()
		return err
	}); err != nil {
		return nil, httpFailure(err, logMsg, 502, prefix, nil)
	}
	if result == nil {
		return nil, NewHTTPException(404, "Skill not found: "+ident)
	}
	return result, nil
}

// SKILL.md and file manifest of a hub skill resolved for the profile, nothing installed
func previewSkillHub(ctx context.Context, identifier, profile string) (any, error) {
	ident, err := require(identifier, "identifier is required")
	if err != nil {
		return nil, err
	}
	return hubLookup(func() (any, error) {
		meta, bundle, err := resolveHubSkill(ctx, ident, profile)
		if err != nil {
			return nil, err
		}
		if bundle == nil && meta == nil {
			return nil, nil
		}
		files := map[string]string{}
		skillMD := ""
		if bundle != nil {
			for _, f := range bundle.Files {
				if f.Binary && !utf8.Valid(f.Data) {
					files[f.Path] = "(binary file)"
				} else {
					files[f.Path] = string(f.Data)
				}
			}
			skillMD = files["SKILL.md"]
		}
		names := make([]string, 0, len(files))
		for name := range files {
			names = append(names, name)
		}
		sort.Strings(names)
		name, description, source, id, trust := ident, "", "", ident, "community"
		var repo any
		tags := []any{}
		if meta != nil {
			name, description, source = meta.Name, meta.Description, meta.Source
			if meta.Identifier != "" {
				id = meta.Identifier
			}
			if meta.TrustLevel != "" {
				trust = meta.TrustLevel
			}
			repo, tags = optTextValue(meta.Repo), stringsAny(meta.Tags)
		} else {
			name, source = bundle.Name, bundle.Source
			if bundle.Identifier != "" {
				id = bundle.Identifier
			}
			if bundle.TrustLevel != "" {
				trust = bundle.TrustLevel
			}
		}
		return pyjson.ObjectOf("name", name, "description", description, "source", source, "identifier", id, "trust_level", trust,
			"repo", repo, "tags", tags, "skill_md", skillMD, "files", stringsAny(names)), nil
	}, ident, "skills hub preview failed", "Hub preview failed")
}

// Advisory SkillEvaluator second opinion on a quarantined skill, nil when the scanner is absent, off or failing
func hubTier1Advisory(ctx context.Context, quarantinePath string) (tier1 any) {
	if err := raised(func() error {
		if !skillsScopeOf(ctx).Tier1AdvisoryEnabled() {
			return nil
		}
		report, err := skills.RunTier1Scan(ctx, quarantinePath, skills.ScanTimeoutSeconds)
		if err != nil {
			return err
		}
		if !report.Available {
			return nil
		}
		findings := []any{}
		for _, f := range report.Findings {
			var line any
			if f.Line != nil {
				line = pyint.Value(f.Line)
			}
			findings = append(findings, pyjson.ObjectOf("check", f.Check, "validator", f.Validator, "severity", f.Severity,
				"message", f.Message, "file", f.File, "line", line, "secrets_class", f.IsSecretsClass()))
		}
		tier1 = pyjson.ObjectOf("passed", report.Passed, "incomplete_checks", stringsAny(report.IncompleteChecks), "findings", findings)
		return nil
	}); err != nil {
		routersLog().Debug("Tier 1 advisory scan skipped", "exc_info", err)
		return nil
	}
	return tier1
}

// Install time security scan of a hub skill on a quarantined bundle, nothing installed
func scanSkillHub(ctx context.Context, identifier, profile string) (any, error) {
	ident, err := require(identifier, "identifier is required")
	if err != nil {
		return nil, err
	}
	return hubLookup(func() (any, error) {
		meta, bundle, err := resolveHubSkill(ctx, ident, profile)
		if err != nil {
			return nil, err
		}
		if bundle == nil {
			return nil, nil
		}
		scanSource := "official"
		if bundle.Source != "official" {
			scanSource = bundle.Identifier
			if scanSource == "" && meta != nil {
				scanSource = meta.Identifier
			}
			if scanSource == "" {
				scanSource = ident
			}
		}
		quarantinePath, err := hubQuarantineBundle(ctx, bundle)
		if err != nil {
			return nil, err
		}
		var result skills.ScanResult
		var tier1 any
		func() {
			defer func() { _ = hubRemoveScanDir(quarantinePath) }()
			result = hubScanSkill(quarantinePath, scanSource)
			tier1 = hubTier1Advisory(ctx, quarantinePath)
		}()
		allowed, reason := skills.ShouldAllowInstall(result, false)
		policy := "block"
		if allowed == nil {
			policy = "ask"
		} else if *allowed {
			policy = "allow"
		}
		findings := []any{}
		counts := pyjson.ObjectOf("critical", int64(0), "high", int64(0), "medium", int64(0), "low", int64(0))
		for _, f := range result.Findings {
			findings = append(findings, pyjson.ObjectOf("severity", f.Severity, "category", f.Category, "file", f.File,
				"line", int64(f.Line), "description", f.Description))
			if n, held := counts.Get(f.Severity); held {
				counts.Set(f.Severity, n.(int64)+1)
			}
		}
		return pyjson.ObjectOf("name", result.SkillName, "identifier", ident, "source", result.Source, "trust_level", result.TrustLevel,
			"verdict", result.Verdict, "summary", result.Summary, "policy", policy, "policy_reason", reason, "findings", findings,
			"severity_counts", counts, "tier1", tier1), nil
	}, ident, "skills hub scan failed", "Hub scan failed")
}

// Locked skill directory names and the directory name of each loadable skill by load name
func lockedIndex(scope *skills.Scope) (map[string]bool, map[string]string, error) {
	names, err := scope.LockedSkillNames()
	if err != nil {
		return nil, nil, err
	}
	if len(names) == 0 {
		return map[string]bool{}, map[string]string{}, nil
	}
	locked := make(map[string]bool, len(names))
	for _, name := range names {
		locked[name] = true
	}
	return locked, scope.CatalogDirNames(true), nil
}

// Every loadable skill of the profile with its lock, enabled state, activity count and provenance
func getSkills(ctx context.Context, profile string) (any, error) {
	sctx, _, release, err := profileScope(ctx, profile)
	if err != nil {
		return nil, err
	}
	rows, disabled, usage, bundled, hub, external, lockedDirs, dirByName, err := func() ([]map[string]any, map[string]bool, *pyjson.Object,
		map[string]bool, map[string]bool, map[string]bool, map[string]bool, map[string]string, error) {
		defer release()
		cfg, err := webLoadConfig(sctx)
		if err != nil {
			return nil, nil, nil, nil, nil, nil, nil, nil, err
		}
		scope := skillsScopeOf(sctx)
		disabled := skills.GetDisabledSkills(cfg, "")
		rows := scope.FindAllSkills(true)
		usage, err := scope.LoadUsage()
		if err != nil {
			return nil, nil, nil, nil, nil, nil, nil, nil, err
		}
		bundled, err := scope.ReadBundledNames()
		if err != nil {
			return nil, nil, nil, nil, nil, nil, nil, nil, err
		}
		hub := scope.ReadHubInstalledNames()
		external := map[string]bool{}
		for name := range scope.ExternalSkillNames() {
			if !bundled[name] && !hub[name] {
				external[name] = true
			}
		}
		lockedDirs, dirByName, err := lockedIndex(scope)
		return rows, disabled, usage, bundled, hub, external, lockedDirs, dirByName, err
	}()
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(rows))
	for _, s := range rows {
		name, _ := s["name"].(string)
		dir, held := dirByName[name]
		if !held {
			dir = name
		}
		record, _ := usage.GetOr(name, nil).(*pyjson.Object)
		if record == nil {
			record = pyjson.NewObject()
		}
		count, err := skills.ActivityCount(record)
		if err != nil {
			return nil, err
		}
		provenance := "agent"
		switch {
		case hub[name]:
			provenance = "hub"
		case bundled[name]:
			provenance = "bundled"
		case external[name]:
			provenance = "external"
		}
		out = append(out, pyjson.ObjectOf("name", s["name"], "description", s["description"], "category", s["category"],
			"locked", lockedDirs[dir], "enabled", !disabled[name], "usage", pyint.Value(count), "provenance", provenance))
	}
	return out, nil
}

// Locks or unlocks a skill of the requested profile against agent edits
func lockSkill(ctx context.Context, body *pyjson.Object, profile string) (any, error) {
	name := bodyStr(body, "name")
	locked, _ := body.GetOr("locked", nil).(bool)
	wctx, release, err := configWriteScope(ctx, skillsTargetProfile(body, profile))
	if err != nil {
		return nil, err
	}
	defer release()
	scope := skillsScopeOf(wctx)
	existing, found := scope.FindSkill(name)
	if !found {
		return nil, NewHTTPException(404, fmt.Sprintf("Skill '%s' not found.", name))
	}
	key := filepath.Base(existing)
	if !scope.SetLocked(wctx, key, locked) {
		verb := "unlock"
		if locked {
			verb = "lock"
		}
		return nil, NewHTTPException(500, fmt.Sprintf("Could not %s '%s': skills/.usage.json is not writable.", verb, key))
	}
	return pyjson.ObjectOf("ok", true, "name", name, "locked", locked), nil
}

// Enables or disables a skill in the config of the requested profile
func toggleSkill(ctx context.Context, body *pyjson.Object, profile string) (any, error) {
	name := bodyStr(body, "name")
	enabled, _ := body.GetOr("enabled", nil).(bool)
	wctx, release, err := configWriteScope(ctx, skillsTargetProfile(body, profile))
	if err != nil {
		return nil, err
	}
	defer release()
	cfg, err := webLoadConfig(wctx)
	if err != nil {
		return nil, err
	}
	disabled := skills.GetDisabledSkills(cfg, "")
	if enabled {
		delete(disabled, name)
	} else {
		disabled[name] = true
	}
	if err := skills.SaveDisabledSkills(wctx, home.Dir(wctx), cfg, disabled, ""); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", true, "name", name, "enabled", enabled), nil
}

// Raw SKILL.md text of a skill of the requested profile for the editor
func getSkillContent(ctx context.Context, name, profile string) (any, error) {
	return scopedToThread(ctx, profile, func(sctx context.Context) (any, error) {
		found, ok := skillsScopeOf(sctx).FindSkill(name)
		if !ok {
			return nil, NewHTTPException(404, fmt.Sprintf("Skill '%s' not found.", name))
		}
		skillMD := filepath.Join(found, "SKILL.md")
		if _, err := os.Stat(skillMD); err != nil {
			return nil, NewHTTPException(404, fmt.Sprintf("Skill '%s' has no SKILL.md.", name))
		}
		content, err := readTextUTF8Sig(skillMD)
		if err != nil {
			if pypath.IsOSError(err) {
				return nil, NewHTTPException(500, pypath.OSErrorText(err))
			}
			return nil, err
		}
		return pyjson.ObjectOf("name", name, "content", content, "path", skillMD), nil
	})
}

// Text of a result field the way str reads it, def when the field is absent
func resultText(result *pyjson.Object, key, def string) any {
	if v, held := result.Get(key); held {
		return v
	}
	return def
}

// Creates a skill in the requested profile through the manage write path
func createSkill(ctx context.Context, body *pyjson.Object, profile string) (any, error) {
	var category any
	if c, _ := body.GetOr("category", nil).(string); c != "" {
		category = c
	}
	result, err := scopedToThread(ctx, skillsTargetProfile(body, profile), func(sctx context.Context) (*pyjson.Object, error) {
		return skillsScopeOf(sctx).CreateSkill(sctx, bodyStr(body, "name"), bodyStr(body, "content"), category)
	})
	if err != nil {
		return nil, err
	}
	if !config.PyTruthy(result.GetOr("success", nil)) {
		return nil, NewHTTPException(400, resultText(result, "error", "Failed to create skill."))
	}
	clearSkillsPromptCache(ctx)
	return result, nil
}

// Replaces the SKILL.md of an existing skill of the requested profile
func updateSkillContent(ctx context.Context, body *pyjson.Object, profile string) (any, error) {
	result, err := scopedToThread(ctx, skillsTargetProfile(body, profile), func(sctx context.Context) (*pyjson.Object, error) {
		return skillsScopeOf(sctx).EditSkillContent(sctx, bodyStr(body, "name"), bodyStr(body, "content"))
	})
	if err != nil {
		return nil, err
	}
	if !config.PyTruthy(result.GetOr("success", nil)) {
		detail := resultText(result, "error", "Failed to update skill.")
		status := 400
		if strings.Contains(pystr.Lower(fmt.Sprint(detail)), "not found") {
			status = 404
		}
		return nil, NewHTTPException(status, detail)
	}
	clearSkillsPromptCache(ctx)
	return result, nil
}

// Skills hub routes of the dashboard in their declaration order
func skillsHubRouter() *APIRouter {
	r := NewAPIRouter("")
	optProfile := QueryParam("profile", Optional(Str), nil)
	identifier := QueryParam("identifier", Str, "")
	r.Post("/api/skills/hub/install", &Endpoint{
		Name:   "install_skill_hub",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", skillInstallRequestModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return installSkillHub(ctx, c.Object("body"), profileQuery(c))
		},
	})
	r.Post("/api/skills/hub/uninstall", &Endpoint{
		Name:   "uninstall_skill_hub",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", skillUninstallRequestModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return uninstallSkillHub(ctx, c.Object("body"), profileQuery(c))
		},
	})
	r.Post("/api/skills/hub/update", &Endpoint{
		Name:   "update_skills_hub",
		Params: []Param{optProfile},
		Body:   []BodyParam{{Name: "body", Type: Optional(skillsUpdateRequestModel)}},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return updateSkillsHub(ctx, c.Object("body"), profileQuery(c))
		},
	})
	r.Get("/api/skills/hub/official", &Endpoint{
		Name:   "list_official_skills",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return listOfficialSkills(ctx, profileQuery(c)) },
	})
	r.Get("/api/skills/hub/sources", &Endpoint{
		Name:   "list_skills_hub_sources",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return listSkillsHubSources(ctx, profileQuery(c)) },
	})
	r.Get("/api/skills/hub/search", &Endpoint{
		Name:   "search_skills_hub",
		Params: []Param{QueryParam("q", Str, ""), QueryParam("source", Str, "all"), QueryParam("limit", Int, int64(20)), optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return searchSkillsHub(ctx, c.Str("q"), c.Str("source"), c.Int("limit"), profileQuery(c))
		},
	})
	r.Get("/api/skills/hub/preview", &Endpoint{
		Name:   "preview_skill_hub",
		Params: []Param{identifier, optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return previewSkillHub(ctx, c.Str("identifier"), profileQuery(c))
		},
	})
	r.Get("/api/skills/hub/scan", &Endpoint{
		Name:   "scan_skill_hub",
		Params: []Param{identifier, optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return scanSkillHub(ctx, c.Str("identifier"), profileQuery(c))
		},
	})
	return r
}

// Skills routes of the dashboard in their declaration order
func skillsRouter() *APIRouter {
	r := NewAPIRouter("")
	optProfile := QueryParam("profile", Optional(Str), nil)
	r.Get("/api/skills", &Endpoint{
		Name:   "get_skills",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return getSkills(ctx, profileQuery(c)) },
	})
	r.Put("/api/skills/lock", &Endpoint{
		Name:   "lock_skill",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", skillLockModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return lockSkill(ctx, c.Object("body"), profileQuery(c))
		},
	})
	r.Put("/api/skills/toggle", &Endpoint{
		Name:   "toggle_skill",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", skillToggleModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return toggleSkill(ctx, c.Object("body"), profileQuery(c))
		},
	})
	r.Get("/api/skills/content", &Endpoint{
		Name:   "get_skill_content",
		Params: []Param{RequiredQuery("name", Str), optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return getSkillContent(ctx, c.Str("name"), profileQuery(c))
		},
	})
	r.Post("/api/skills", &Endpoint{
		Name:   "create_skill",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", skillCreateModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return createSkill(ctx, c.Object("body"), profileQuery(c))
		},
	})
	r.Put("/api/skills/content", &Endpoint{
		Name:   "update_skill_content",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", skillContentUpdateModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return updateSkillContent(ctx, c.Object("body"), profileQuery(c))
		},
	})
	return r
}
