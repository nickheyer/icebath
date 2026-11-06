package skills

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/observability"
)

// Options of one hub install, the zero value matching the Python defaults except DeferCache
type HubInstallOptions struct {
	Category           string
	Force, SkipConfirm bool
	// Leaves the prompt cache alone, as invalidate_cache=False
	DeferCache   bool
	NameOverride string
	// Adapter the identifier is pinned to, empty for none
	SourceID string
	Console  *pyrich.Console
}

type rateLimitReporter interface{ IsRateLimited() bool }
type githubBacked interface{ githubSource() *GitHubSource }
type githubAuthed interface{ githubAuth() *GitHubAuth }

func (s *SkillsShSource) githubSource() *GitHubSource { return s.GitHub }
func (s *SkillsShSource) githubAuth() *GitHubAuth     { return s.Auth }
func (g *GitHubSource) githubAuth() *GitHubAuth       { return g.Auth }
func (x *HermesIndexSource) githubAuth() *GitHubAuth  { return x.Auth }

// Errors Python raises as OSError rather than ValueError
func isOSError(err error) bool {
	var pe *fs.PathError
	var le *os.LinkError
	var se *os.SyscallError
	var errno syscall.Errno
	return errors.As(err, &pe) || errors.As(err, &le) || errors.As(err, &se) || errors.As(err, &errno)
}

// Prints the blocked line, drops the quarantine copy and appends the audit row
func (x *HubCLI) installBlocked(c *pyrich.Console, b *SkillBundle, message, verdict, detail, qPath, lead, label string) error {
	c.Print(fmt.Sprintf("%s[bold red]%s[/] %s", lead, label, message))
	if qPath != "" {
		os.RemoveAll(qPath)
	}
	return x.Hub.AppendAuditLog("BLOCKED", b.Name, b.Source, b.TrustLevel, verdict, detail)
}

// User facing sentence of a scan blocked install naming the force policy and the inspect next step
func scanBlockMessage(result ScanResult, identifier string) string {
	n := len(result.Findings)
	findings := "high-risk patterns"
	if n > 0 {
		findings = fmt.Sprintf("%d high-risk pattern(s)", n)
	}
	hard := result.Verdict == "dangerous" && (result.TrustLevel == "community" || result.TrustLevel == "trusted")
	policy := "Re-run with --force to install anyway."
	if hard {
		policy = "Icebath never installs unverified skills with high-risk findings, even with --force."
	}
	return fmt.Sprintf("the security scan found %s in '%s' (listed above). %s Review the findings or ask the author to fix them; "+
		"to read the skill without installing, run `icebath skills inspect %s`.", findings, identifier, policy, identifier)
}

func (x *HubCLI) invalidPath(c *pyrich.Console, b *SkillBundle, err error, qPath string) error {
	return x.installBlocked(c, b, err.Error()+"\n", "invalid_path", err.Error(), qPath, "", "Installation blocked:")
}

// Names a URL bundle whose SKILL.md has none from the override, a prompt or a refusal, false aborting
func (x *HubCLI) resolveURLBundleName(c *pyrich.Console, b *SkillBundle, meta *SkillMeta, identifier, nameOverride string, skipConfirm bool) bool {
	if x.seams.resolveURLBundleName != nil {
		return x.seams.resolveURLBundleName(c, b, meta, identifier, nameOverride, skipConfirm)
	}
	if b.Metadata == nil {
		b.Metadata = pyjson.NewObject()
	}
	awaiting, _ := b.Metadata.Get("awaiting_name")
	if b.Source != "url" || (b.Name != "" && !pyrepr.Truthy(awaiting)) {
		return true
	}
	url := identifier
	if v, ok := b.Metadata.Get("url"); ok && pyrepr.Truthy(v) {
		url = pyrepr.Str(v)
	}
	switch {
	case nameOverride != "" && isValidInstalledSkillName(nameOverride):
		b.Name = strings.TrimSpace(nameOverride)
	case nameOverride != "":
		c.Print(fmt.Sprintf("[bold red]Invalid --name:[/] %s. Must be a lowercase identifier (letters, digits, hyphens, underscores; starts with a letter).\n", pyrepr.Repr(nameOverride)))
		return false
	case skipConfirm:
		c.Print(fmt.Sprintf("[bold red]Cannot install from URL:[/] %s\n"+
			"[yellow]The SKILL.md has no `name:` in its frontmatter, and the URL path doesn't produce a valid identifier.[/]\n\n"+
			"Retry with an explicit name:\n"+
			"  [bold]/skills install %s --name <your-name>[/]\n"+
			"  [bold]icebath skills install %s --name <your-name>[/]\n\n"+
			"[dim]Or ask the SKILL.md's author to add a `name:` field to its YAML frontmatter.[/]\n", url, url, url))
		return false
	default:
		chosen, ok := x.promptForSkillName(c, url, "")
		if !ok || chosen == "" {
			c.Print("[dim]Installation cancelled.[/]\n")
			return false
		}
		b.Name = chosen
	}
	b.Metadata.Set("awaiting_name", false)
	if meta != nil {
		meta.Name = b.Name
		meta.Path = strPtr(b.Name)
	}
	return true
}

// Offers an installed skill blueprint through the suggestions list, never scheduling it
func (x *HubCLI) announceBlueprint(ctx context.Context, c *pyrich.Console, skillName string) {
	spec, err := x.S.BlueprintSpecForInstalled(skillName)
	var be *BlueprintError
	if errors.As(err, &be) {
		c.Print(fmt.Sprintf("[yellow]Blueprint block present but invalid:[/] %s\n", be.Error()))
		return
	}
	if err != nil || spec == nil {
		return
	}
	lead := fmt.Sprintf("[bold cyan]Blueprint:[/] '%s' is an automation (schedule [bold]%s[/])", skillName, spec.Schedule)
	record, err := RegisterBlueprintSuggestion(ctx, spec)
	if err != nil {
		return
	}
	if record != nil {
		c.Print(lead + ".")
		c.Print("[dim]Added to your suggestions — run[/] [bold]/suggestions[/] [dim]to schedule or dismiss it.[/]\n")
		return
	}
	c.Print(lead + ", but it wasn't added to your suggestions (already offered/dismissed, or the pending list is full — run [bold]/suggestions[/] to review).")
	c.Print("[dim]You can still schedule it any time by asking the agent or via[/] [bold]icebath cron add[/][dim].[/]\n")
}

// Sources restricted to the pinned adapter, nil with an error line when none matches
func pinnedSources(c *pyrich.Console, sources []SkillSource, sourceID, identifier string) []SkillSource {
	if sourceID == "" {
		if len(sources) > 0 {
			return sources
		}
	} else {
		var pinned []SkillSource
		for _, src := range sources {
			if sourceMatches(src, sourceID) {
				pinned = append(pinned, src)
			}
		}
		if len(pinned) > 0 {
			return pinned
		}
	}
	printHubError(c, fmt.Sprintf("no source adapter for '%s'. Refusing to resolve '%s' against other registries (that would change the skill's provenance).", pyrepr.Str(sourceIDOrNone(sourceID)), identifier))
	return nil
}

func sourceIDOrNone(id string) any {
	if id == "" {
		return nil
	}
	return id
}

// Whether any adapter or the GitHub source behind it hit the rate limit
func hubRateLimited(sources []SkillSource) bool {
	for _, src := range sources {
		if r, ok := src.(rateLimitReporter); ok && r.IsRateLimited() {
			return true
		}
		if g, ok := src.(githubBacked); ok && g.githubSource() != nil && g.githubSource().IsRateLimited() {
			return true
		}
	}
	return false
}

// Credentials GitHub refused with 401 on the first adapter that holds any
func hubRejectedCredentials(sources []SkillSource) []string {
	for _, src := range sources {
		if a, ok := src.(githubAuthed); ok && a.githubAuth() != nil {
			if r := a.githubAuth().Rejected(); len(r) > 0 {
				return r
			}
		}
	}
	return nil
}

// Explains a resolved identifier no adapter served and returns the failure class
func (x *HubCLI) printFetchFailure(c *pyrich.Console, sources []SkillSource, identifier string, metaHit bool, source SkillSource) string {
	rateLimited := hubRateLimited(sources)
	rejected := hubRejectedCredentials(sources)
	if metaHit && !rateLimited && len(rejected) == 0 {
		srcID := "the registry"
		if source != nil {
			srcID = source.SourceID()
		}
		c.Print(fmt.Sprintf("[bold red]Error:[/] '%s' is listed in the %s index, but its files no longer exist upstream.", identifier, srcID))
		c.Print("[dim]Stale index entry: the skill was likely renamed or removed by its author. Try `icebath skills search` for an alternative.[/]\n")
		return "stale_index"
	}
	c.Print(fmt.Sprintf("[bold red]Error:[/] Could not download '%s'.", identifier))
	if len(rejected) > 0 {
		c.Print(fmt.Sprintf("[yellow]Hint:[/] GitHub rejected %s (401 Bad credentials). Replace or remove the token in %s/.env, or re-run [bold]gh auth login[/].",
			strings.Join(rejected, " and "), home.Display(x.S.Home)))
	}
	if rateLimited {
		c.Print("[yellow]Hint:[/] GitHub API rate limit exhausted (unauthenticated: 60 requests/hour).\n" +
			"Set [bold]GITHUB_TOKEN[/] in your .env or install the [bold]gh[/] CLI and run [bold]gh auth login[/] to raise the limit to 5,000/hr.\n")
	} else {
		c.Print(fmt.Sprintf("Check the name with [bold]icebath skills search %s[/] and check your internet connection. If it keeps failing, run [bold]icebath doctor[/].\n",
			identifierSlug(identifier)))
	}
	switch {
	case rateLimited:
		return "rate_limited"
	case len(rejected) > 0:
		return "auth_rejected"
	}
	return "fetch_failed"
}

// Runs the cached security scan of the quarantined bundle and prints its report and provenance
func (x *HubCLI) scanQuarantined(c *pyrich.Console, qPath string, b *SkillBundle, meta *SkillMeta, identifier string) (ScanResult, error) {
	c.Print("[bold]Running security scan...[/]")
	scanSource := b.Identifier
	if b.Source == "official" {
		scanSource = "official"
	} else if scanSource == "" && meta != nil {
		scanSource = meta.Identifier
	}
	if scanSource == "" {
		scanSource = identifier
	}
	result, prov, err := ScanSkillCached(qPath, scanSource, sourceURLForBundle(b), filepath.Join(x.Hub.HubDir(), "scan-cache"))
	if err != nil {
		return result, err
	}
	c.Print(FormatScanReport(result))
	fresh := "cached"
	if pyrepr.Truthy(prov["fresh"]) {
		fresh = "fresh"
	}
	c.Print(fmt.Sprintf("[dim]Scan provenance: %s; scanner %s; hash %s[/]", fresh, pyrepr.Str(prov["scanner_version"]), pyrepr.Str(prov["bundle_hash"])))
	var rules []string
	switch r := prov["rules"].(type) {
	case []string:
		rules = r
	case []any:
		for _, v := range r {
			rules = append(rules, pyrepr.Str(v))
		}
	}
	joined := strings.Join(rules, ", ")
	if joined == "" {
		joined = "none"
	}
	c.Print(fmt.Sprintf("[dim]Source: %s; scanned %s; rules: %s[/]", pyrepr.Str(prov["source_url"]), pyrepr.Str(prov["scanned_at"]), joined))
	return result, nil
}

var installPanels = map[string][3]string{
	"official": {"[bold bright_cyan]This is an official optional skill maintained by Nous Research.[/]\n\n" +
		"It ships with icebath-agent but is not activated by default.\n" +
		"Installing will copy it to your skills directory where the agent can use it.\n\n", "Official Skill", "bright_cyan"},
	"external": {"[bold yellow]You are installing a third-party skill at your own risk.[/]\n\n" +
		"External skills can contain instructions that influence agent behavior,\n" +
		"shell commands, and scripts. Even after automated scanning, you should\n" +
		"review the installed files before use.\n\n", "Disclaimer", "yellow"},
}

// Source appropriate disclaimer panel and the install prompt
func (x *HubCLI) confirmInstall(c *pyrich.Console, b *SkillBundle, category string) bool {
	cat := ""
	if category != "" {
		cat = category + "/"
	}
	filesAt := fmt.Sprintf("Files will be at: [cyan]%s/skills/%s%s/[/]", home.Display(x.S.Home), cat, b.Name)
	kind := "external"
	if b.Source == "official" {
		kind = "official"
	}
	p := installPanels[kind]
	c.Print()
	c.PrintPanel(pyrich.Panel{Body: p[0] + filesAt, Title: p[1], BorderStyle: p[2]})
	return x.confirmOrCancel(c, "[dim]Installation cancelled.[/]\n", fmt.Sprintf("[bold]Install '%s'?[/]", b.Name))
}

var skillMetricSources = map[string]string{"official": "catalog", "url": "url", "bundled": "bundled"}

// Install of a name icebath ships, pointing at the active copy or restoring it from the bundled source
func (x *HubCLI) installBundled(ctx context.Context, c *pyrich.Console, name string, invalidate bool) (*SkillBundle, string, HubOutcome, error) {
	result, err := x.S.EnsureBundledSkill(ctx, name)
	if err != nil {
		return nil, "", HubNoop, err
	}
	if !pyrepr.Truthy(result["ok"]) {
		printHubError(c, pyrepr.Str(result["message"]))
		return nil, "failed", HubFailed, nil
	}
	switch result["action"] {
	case "hub_shadowed":
		printHubError(c, pyrepr.Str(result["message"]))
		return nil, "", HubFailed, nil
	case "present":
		where := ""
		if pyrepr.Truthy(result["path"]) {
			where = " at " + pyrepr.Str(result["path"])
		}
		c.Print(fmt.Sprintf("[green]'%s' is a built-in skill and is already available%s.[/]\n", name, where))
		return nil, "", HubNoop, nil
	}
	c.Print(fmt.Sprintf("[bold green]Restored built-in skill:[/] %s [dim](%s)[/]\n", name, pyrepr.Str(result["path"])))
	x.finishChange(c, invalidate, "Skill will be available", "activate", true)
	return &SkillBundle{Name: name, Source: "bundled"}, "success", HubOK, nil
}

// Identifier prefixes each adapter accepts with the registry they name
func registryPrefixes() [][2]string {
	out := [][2]string{{"official/", "official"}, {"clawhub/", "clawhub"}, {"lobehub/", "lobehub"}, {"browse-sh/", "browse-sh"}}
	for _, p := range skillsShIDPrefixes {
		out = append(out, [2]string{p, "skills-sh"})
	}
	return append(out, [2]string{"well-known:", "well-known"}, [2]string{"http://", "url"}, [2]string{"https://", "url"})
}

// Registry an identifier was meant for, unresolved for a bare owner repo path
func registryFromIdentifier(identifier string) string {
	lowered := strings.ToLower(identifier)
	if (strings.HasPrefix(lowered, "http://") || strings.HasPrefix(lowered, "https://")) && strings.Contains(lowered, "/.well-known/skills/") {
		return "well-known"
	}
	for _, p := range registryPrefixes() {
		if strings.HasPrefix(lowered, p[0]) {
			return p[1]
		}
	}
	return "unresolved"
}

// Records one shared metrics extension install, official as catalog, URL anonymous and the rest hub
func (x *HubCLI) recordSkillInstall(ctx context.Context, identifier string, b *SkillBundle, outcome string, attempt *installAttempt, err error) {
	if attempt == nil {
		attempt = &installAttempt{}
	}
	origin := ""
	switch {
	case b != nil && b.Source != "":
		origin = b.Source
	case attempt.origin != nil && *attempt.origin != "":
		origin = *attempt.origin
	case strings.HasPrefix(identifier, "official/"):
		origin = "official"
	case strings.HasPrefix(identifier, "http://") || strings.HasPrefix(identifier, "https://"):
		origin = "url"
	default:
		origin = "hub"
	}
	source, ok := skillMetricSources[origin]
	if !ok {
		source = "hub"
	}
	var name any
	if source != "url" {
		name = identifier
		if b != nil && b.Name != "" {
			name = b.Name
		}
	}
	opt := func(p *string) any {
		if p == nil {
			return nil
		}
		return *p
	}
	record := observability.RecordExtensionInstall
	if x.seams.recordInstall != nil {
		record = x.seams.recordInstall
	}
	record(ctx, "skill", source, name, outcome, opt(attempt.failureClass), opt(attempt.registry), err)
}

func (x *HubCLI) lockInstalled(name string) (*pyjson.Object, error) {
	raw, err := x.Hub.LockFile("").GetInstalled(name)
	if err != nil {
		return nil, err
	}
	obj, _ := raw.(*pyjson.Object)
	return obj, nil
}

// Fetches, quarantines, scans, confirms and installs a skill, recording a first install once
func (x *HubCLI) DoInstall(ctx context.Context, identifier string, o HubInstallOptions) (HubOutcome, error) {
	if x.seams.install != nil {
		return x.seams.install(ctx, identifier, o)
	}
	existing, err := x.lockInstalled(identifierSlug(strings.TrimRight(identifier, "/")))
	if err != nil {
		return HubNoop, err
	}
	fresh := existing == nil
	attempt := &installAttempt{}
	b, outcome, installed, err := x.installSkill(ctx, identifier, o, x.console(o.Console), attempt)
	if err != nil {
		if fresh {
			x.recordSkillInstall(ctx, identifier, nil, "failed", attempt, err)
		}
		return HubNoop, err
	}
	if fresh && outcome != "" {
		x.recordSkillInstall(ctx, identifier, b, outcome, attempt, nil)
	}
	return installed, nil
}

// Body of an install returning the bundle, the install event and the command outcome
func (x *HubCLI) installSkill(ctx context.Context, identifier string, o HubInstallOptions, c *pyrich.Console, attempt *installAttempt) (*SkillBundle, string, HubOutcome, error) {
	invalidate := !o.DeferCache
	category := o.Category
	attempt.registry = strPtr("none")
	if err := x.Hub.EnsureHubDirs(); err != nil {
		return nil, "", HubNoop, err
	}
	if o.SourceID == "" && o.NameOverride == "" {
		builtin, err := x.S.BundledSkillForInstall(identifier)
		if err != nil {
			return nil, "", HubNoop, err
		}
		if builtin != "" {
			attempt.failureClass, attempt.origin = strPtr("filesystem_error"), strPtr("bundled")
			return x.installBundled(ctx, c, builtin, invalidate)
		}
	}
	attempt.registry = strPtr("unresolved")
	sources := pinnedSources(c, x.sources(), o.SourceID, identifier)
	if sources == nil {
		attempt.failureClass = strPtr("not_found")
		return nil, "failed", HubFailed, nil
	}
	sctx, done := x.Hub.HTTPSession(ctx)
	identifier = x.fullIdentifier(sctx, identifier, sources, c, attempt)
	if identifier == "" {
		done()
		if hubRateLimited(sources) {
			attempt.failureClass = strPtr("rate_limited")
		} else if len(hubRejectedCredentials(sources)) > 0 {
			attempt.failureClass = strPtr("auth_rejected")
		}
		return nil, "failed", HubFailed, nil
	}
	c.Print(fmt.Sprintf("\n[bold]Fetching:[/] %s", identifier))
	meta, b, matched := x.resolveSourceMetaAndBundle(sctx, identifier, sources)
	done()
	if matched != nil {
		id := matched.SourceID()
		if id == "" {
			id = "other"
		}
		attempt.registry = strPtr(id)
	} else if attempt.registry != nil && *attempt.registry == "unresolved" {
		attempt.registry = strPtr(registryFromIdentifier(identifier))
	}
	if b == nil {
		attempt.failureClass = strPtr(x.printFetchFailure(c, sources, identifier, meta != nil, matched))
		return nil, "failed", HubFailed, nil
	}
	if !x.resolveURLBundleName(c, b, meta, identifier, o.NameOverride, o.SkipConfirm) {
		attempt.failureClass = strPtr("invalid_name")
		return b, "failed", HubFailed, nil
	}
	if b.Source == "url" && category == "" && !o.SkipConfirm {
		category = x.promptForCategory(c, x.existingCategories())
	}
	if b.Source == "official" && category == "" {
		parts := strings.Split(b.Identifier, "/")
		if len(parts) > 2 {
			category = strings.Join(parts[1:len(parts)-1], "/")
		}
	}
	existing, err := x.lockInstalled(b.Name)
	if err != nil {
		return b, "", HubNoop, err
	}
	failed := "failed"
	if existing != nil {
		path, _ := existing.Get("install_path")
		c.Print(fmt.Sprintf("[yellow]Warning:[/] '%s' is already installed at %s", b.Name, pyrepr.Str(path)))
		if !o.Force {
			c.Print("Use --force to reinstall.\n")
			return b, "", HubNoop, nil
		}
		failed = ""
	}
	extra := pyjson.NewObject()
	for _, src := range []*pyjson.Object{metaExtra(meta), b.Metadata} {
		if src == nil {
			continue
		}
		for _, k := range src.Keys {
			v, _ := src.Get(k)
			extra.Set(k, v)
		}
	}
	qPath, err := x.Hub.QuarantineBundle(b)
	if err != nil {
		if isOSError(err) {
			return b, "", HubNoop, err
		}
		if aerr := x.invalidPath(c, b, err, ""); aerr != nil {
			return b, "", HubNoop, aerr
		}
		attempt.failureClass = strPtr("invalid_bundle")
		return b, failed, HubFailed, nil
	}
	rel, _ := filepath.Rel(filepath.Dir(filepath.Dir(filepath.Dir(qPath))), qPath)
	c.Print(fmt.Sprintf("[dim]Quarantined to %s[/]", rel))
	result, err := x.scanQuarantined(c, qPath, b, meta, identifier)
	if err != nil {
		return b, "", HubNoop, err
	}
	allowed, _ := ShouldAllowInstall(result, o.Force)
	if allowed == nil || !*allowed {
		if err := x.installBlocked(c, b, scanBlockMessage(result, identifier), result.Verdict,
			fmt.Sprintf("%d_findings", len(result.Findings)), qPath, "\n", "Not installed:"); err != nil {
			return b, "", HubNoop, err
		}
		attempt.failureClass = strPtr("scan_blocked")
		return b, failed, HubFailed, nil
	}
	x.printTier1Advisory(ctx, qPath, c)
	if lines := formatExtraMetadataLines(extra); len(lines) > 0 {
		c.PrintPanel(pyrich.Panel{Body: strings.Join(lines, "\n"), Title: "Upstream Metadata", BorderStyle: "blue"})
	}
	if !o.Force && !o.SkipConfirm && !x.confirmInstall(c, b, category) {
		os.RemoveAll(qPath)
		return b, "", HubNoop, nil
	}
	installDir, err := x.Hub.InstallFromQuarantine(ctx, qPath, b.Name, category, b, result, nil)
	if err != nil {
		if isOSError(err) {
			return b, "", HubNoop, err
		}
		if aerr := x.invalidPath(c, b, err, qPath); aerr != nil {
			return b, "", HubNoop, aerr
		}
		attempt.failureClass = strPtr("invalid_bundle")
		return b, failed, HubFailed, nil
	}
	relInstall, err := filepath.Rel(resolvePath(x.S.SkillsDir()), resolvePath(installDir))
	if err != nil {
		return b, "", HubNoop, err
	}
	c.Print(fmt.Sprintf("[bold green]Installed:[/] %s", filepath.ToSlash(relInstall)))
	files := make([]string, len(b.Files))
	for i, f := range b.Files {
		files[i] = f.Path
	}
	c.Print(fmt.Sprintf("[dim]Files: %s[/]\n", strings.Join(files, ", ")))
	x.announceBlueprint(ctx, c, b.Name)
	x.finishChange(c, invalidate, "Skill will be available", "activate", true)
	if existing != nil {
		return b, "", HubOK, nil
	}
	return b, "success", HubOK, nil
}

func metaExtra(m *SkillMeta) *pyjson.Object {
	if m == nil {
		return nil
	}
	return m.Extra
}

// Advisory SkillEvaluator Tier 1 report that never blocks, secrets rendered red
func (x *HubCLI) printTier1Advisory(ctx context.Context, skillDir string, c *pyrich.Console) {
	enabled, scan := x.S.Tier1AdvisoryEnabled, RunTier1Scan
	if x.seams.tier1Enabled != nil {
		enabled = x.seams.tier1Enabled
	}
	if x.seams.tier1Scan != nil {
		scan = x.seams.tier1Scan
	}
	if !enabled() {
		return
	}
	report, err := scan(ctx, skillDir, ScanTimeoutSeconds)
	if err != nil {
		x.S.logger().Debug(fmt.Sprintf("Tier 1 advisory scan skipped: %v", err))
		return
	}
	if !report.Available {
		return
	}
	text := FormatTier1Report(report, 10)
	if len(report.Findings) == 0 {
		c.Print(fmt.Sprintf("[dim]%s[/]", text))
		return
	}
	border := "yellow"
	if len(report.SecretsFindings()) > 0 {
		border = "red"
	}
	c.PrintPanel(pyrich.Panel{Body: text, Title: "SkillEvaluator Tier 1 (advisory)", BorderStyle: border})
	if len(report.SecretsFindings()) > 0 {
		c.Print("[bold red]Possible credentials detected above.[/] Review the flagged lines before using this skill.\n")
	}
}
