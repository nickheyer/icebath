package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// What an operation of the hub came to
type Outcome int

const (
	// Nothing changed, such as a skill installed already
	Unchanged Outcome = iota
	Installed
	// Fetched and scanned, waiting for the user's yes
	Staged
	// A skill at a URL with no usable name, waiting for the user to give one
	NeedsName
	Failed
)

// What an install is asked to do
type Install struct {
	// Skill to install: an identifier, a short name, or a URL of a SKILL.md
	Identifier string
	// Category folder it goes in, its source's when empty
	Category string
	// Name for a skill at a URL that has none
	Name string
	// Installs over an installed copy and despite a verdict the trust of its source does not let pass, a dangerous one of a community or
	// trusted source excepted; asks nothing
	Force bool
	// Asks nothing
	Yes bool
	// Source the identifier is pinned to, as an update pins the one a skill came from; any when empty
	Source string
}

// What an install waiting for the user tells them
type Pending struct {
	ID     string
	Name   string
	Source string
	// Whether the skill is an official one of icebath, which ships with it
	Official bool
	// Whether to ask for a category, with those the skills folder has; a skill at a URL installed without one
	AskCategory bool
	Categories  []string
	// Category it goes in unless the user gives one
	Category string
	// The skills folder as the user reads it
	SkillsDir string
	// Address of a skill that needs a name
	URL string
}

// A skill fetched, quarantined and scanned, waiting for the user's yes
type staged struct {
	home string
	// Its folder in quarantine, and the folder of this install there
	dir, holder string
	bundle      *Bundle
	result      Result
	prov        provenance
	category    string
	at          time.Time
}

// How long a staged install waits for the user
const stagedTTL = time.Hour

// Drops the staged installs the user left waiting too long, with their quarantined files
func (h *Hub) expire() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, st := range h.staged {
		if time.Since(st.at) > stagedTTL {
			os.RemoveAll(st.holder)
			delete(h.staged, id)
		}
	}
}

// Fetches a skill, quarantines and scans it, and installs it when asked to go ahead without asking; else leaves it staged for Commit.
// A name of a skill icebath ships is restored from its own copy; a skill at a URL with no usable name waits for one
func (h *Hub) Stage(ctx context.Context, scope *skills.Scope, req Install, say Say) (Outcome, *Pending, error) {
	r := h.run(scope, say)
	if err := r.ensure(); err != nil {
		return Failed, nil, err
	}
	h.expire()
	if req.Source == "" && req.Name == "" {
		if name := scope.BundledFor(req.Identifier); name != "" {
			return r.restore(ctx, name), nil, nil
		}
	}
	sources, err := r.sources()
	if err != nil {
		return Failed, nil, err
	}
	if req.Source != "" {
		alias := map[string]string{"skills.sh": "skills-sh"}[req.Source]
		sources = slices.DeleteFunc(sources, func(s Source) bool { return s.ID() != req.Source && s.ID() != alias })
		if len(sources) == 0 {
			r.fail("no source adapter for '%s'. Refusing to resolve '%s' against other registries (that would change the skill's provenance).",
				req.Source, req.Identifier)
			return Failed, nil, nil
		}
	}
	identifier := r.full(ctx, req.Identifier, sources)
	if identifier == "" {
		return Failed, nil, nil
	}
	r.line(Plain, "")
	r.line(Heading, "Fetching: %s", identifier)
	meta, bundle, matched := r.resolve(ctx, identifier, sources)
	if bundle == nil {
		r.fetchFailure(identifier, meta != nil, matched)
		return Failed, nil, nil
	}
	if outcome, pending := r.name(bundle, meta, identifier, req); outcome != Installed {
		return outcome, pending, nil
	}
	category := req.Category
	if bundle.Source == "official" && category == "" {
		if parts := strings.Split(bundle.Identifier, "/"); len(parts) > 2 {
			category = strings.Join(parts[1:len(parts)-1], "/")
		}
	}
	lock, err := skills.ReadLock(scope.Dir())
	if err != nil {
		return Failed, nil, err
	}
	if entry := lock.Installed[bundle.Name]; entry != nil {
		r.line(Warn, "Warning: '%s' is already installed at %s", bundle.Name, entry.InstallPath)
		if !req.Force {
			r.line(Plain, "Use --force to reinstall.")
			return Unchanged, nil, nil
		}
	}
	extra := map[string]any{}
	if meta != nil {
		maps.Copy(extra, meta.Extra)
	}
	maps.Copy(extra, bundle.Metadata)
	id := strs.Random(8)
	dir, err := r.quarantineBundle(bundle, id)
	if err != nil {
		os.RemoveAll(filepath.Join(r.quarantine(), id))
		return r.blocked(bundle, err)
	}
	st := &staged{home: scope.Home(), dir: dir, holder: filepath.Join(r.quarantine(), id), bundle: bundle, category: category, at: time.Now()}
	rel, _ := filepath.Rel(scope.Dir(), dir)
	r.line(Dim, "Quarantined to %s", filepath.ToSlash(rel))
	r.line(Heading, "Running security scan...")
	source := bundle.Identifier
	switch {
	case bundle.Source == "official":
		source = "official"
	case source == "" && meta != nil:
		source = meta.Identifier
	}
	if source == "" {
		source = identifier
	}
	st.result, st.prov, err = r.scan(ctx, dir, source, sourceURL(bundle), r.scanCache())
	if err != nil {
		os.RemoveAll(st.holder)
		return Failed, nil, err
	}
	r.line(Plain, report(st.result))
	fresh := "cached"
	if st.prov.Fresh {
		fresh = "fresh"
	}
	r.line(Dim, "Scan provenance: %s; scanner %s; hash %s", fresh, st.prov.ScannerVersion, st.prov.BundleHash)
	rules := strings.Join(st.prov.Rules, ", ")
	if rules == "" {
		rules = "none"
	}
	r.line(Dim, "Source: %s; scanned %s; rules: %s", st.prov.SourceURL, st.prov.ScannedAt, rules)
	if decision, _ := decide(st.result, req.Force); decision != "allow" {
		r.line(Plain, "")
		r.line(Bad, "Not installed: %s", blockMessage(st.result, identifier))
		os.RemoveAll(st.holder)
		r.audit("BLOCKED", bundle.Name, bundle.Source, bundle.Trust, st.result.Verdict, fmt.Sprintf("%d_findings", len(st.result.Findings)))
		return Failed, nil, nil
	}
	r.advise(ctx, dir)
	if lines := Details(extra); len(lines) > 0 {
		r.line(Heading, "Upstream Metadata")
		for _, line := range lines {
			r.line(Plain, "  "+line)
		}
	}
	if req.Force || req.Yes {
		return h.commit(ctx, r, st, category), nil, nil
	}
	pending := &Pending{ID: id, Name: bundle.Name, Source: bundle.Source, Official: bundle.Source == "official", Category: category,
		AskCategory: bundle.Source == "url" && category == "", SkillsDir: skills.Display(scope.Dir())}
	if pending.AskCategory {
		pending.Categories = r.categories()
	}
	h.mu.Lock()
	h.staged[id] = st
	h.mu.Unlock()
	return Staged, pending, nil
}

// Installs a skill Stage left waiting, into the category given or its own
func (h *Hub) Commit(ctx context.Context, scope *skills.Scope, id, category string, say Say) (Outcome, error) {
	r := h.run(scope, say)
	h.mu.Lock()
	st := h.staged[id]
	delete(h.staged, id)
	h.mu.Unlock()
	if st == nil || st.home != scope.Home() {
		r.fail("That install is no longer waiting; run the install again.")
		return Failed, nil
	}
	if category == "" {
		category = st.category
	}
	return h.commit(ctx, r, st, category), nil
}

// Drops a skill Stage left waiting, with its quarantined files
func (h *Hub) Discard(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if st := h.staged[id]; st != nil {
		os.RemoveAll(st.holder)
		delete(h.staged, id)
	}
}

// Moves a staged skill into place and records it
func (h *Hub) commit(ctx context.Context, r *run, st *staged, category string) Outcome {
	defer os.RemoveAll(st.holder)
	dir, err := r.place(ctx, st, category)
	if err != nil {
		outcome, _, _ := r.blocked(st.bundle, err)
		return outcome
	}
	rel, err := filepath.Rel(resolved(r.scope.Dir()), resolved(dir))
	if err != nil {
		rel = dir
	}
	r.line(Good, "Installed: %s", filepath.ToSlash(rel))
	var files []string
	for _, f := range st.bundle.Files {
		files = append(files, f.Path)
	}
	r.line(Dim, "Files: %s", strings.Join(files, ", "))
	r.line(Plain, "")
	r.announce(ctx, st.bundle.Name)
	h.registry.Forget()
	return Installed
}

// Says an install was refused for an unsafe path or folder, and records it; an error that is no refusal of the install is returned
func (r *run) blocked(bundle *Bundle, err error) (Outcome, *Pending, error) {
	var refusal *refused
	if !errors.As(err, &refusal) {
		return Failed, nil, err
	}
	r.line(Bad, "Installation blocked: %s", refusal.message)
	r.audit("BLOCKED", bundle.Name, bundle.Source, bundle.Trust, "invalid_path", refusal.message)
	return Failed, nil, nil
}

// An install refused for where it would go or what it holds
type refused struct {
	message string
}

func (e *refused) Error() string { return e.message }

func refuse(format string, args ...any) error {
	return &refused{fmt.Sprintf(format, args...)}
}

// Names a skill at a URL that has none, from the name asked for; Installed to go on, NeedsName to ask the user, Failed when it can't be
// named
func (r *run) name(bundle *Bundle, meta *Meta, identifier string, req Install) (Outcome, *Pending) {
	waiting, _ := bundle.Metadata["awaiting_name"].(bool)
	if bundle.Source != "url" || bundle.Name != "" && !waiting {
		return Installed, nil
	}
	address := identifier
	if v, ok := bundle.Metadata["url"].(string); ok && v != "" {
		address = v
	}
	switch {
	case req.Name != "" && installable(req.Name):
		bundle.Name = strings.TrimSpace(req.Name)
	case req.Name != "":
		r.line(Bad, "Invalid --name: %q. Must be a lowercase identifier (letters, digits, hyphens, underscores; starts with a letter).", req.Name)
		return Failed, nil
	case req.Yes:
		r.line(Bad, "Cannot install from URL: %s", address)
		r.line(Warn, "The SKILL.md has no `name:` in its frontmatter, and the URL path doesn't produce a valid identifier.")
		r.line(Plain, "")
		r.line(Plain, "Retry with an explicit name:")
		r.line(Plain, "  /skills install %s --name <your-name>", address)
		r.line(Plain, "  icebath skills install %s --name <your-name>", address)
		r.line(Plain, "")
		r.line(Dim, "Or ask the SKILL.md's author to add a `name:` field to its YAML frontmatter.")
		return Failed, nil
	default:
		return NeedsName, &Pending{URL: address, SkillsDir: skills.Display(r.scope.Dir())}
	}
	if bundle.Metadata == nil {
		bundle.Metadata = map[string]any{}
	}
	bundle.Metadata["awaiting_name"] = false
	if meta != nil {
		meta.Name, meta.Path = bundle.Name, bundle.Name
	}
	return Installed, nil
}

// Puts a skill icebath ships back in place, or says where it is
func (r *run) restore(ctx context.Context, name string) Outcome {
	ensured, err := r.scope.Ensure(ctx, name)
	switch {
	case err != nil:
		r.fail("%s", err)
		return Failed
	case ensured.Action == "hub_shadowed":
		r.fail("%s", ensured.Message)
		return Failed
	case ensured.Action == "present":
		where := ""
		if ensured.Path != "" {
			where = " at " + ensured.Path
		}
		r.line(Good, "'%s' is a built-in skill and is already available%s.", name, where)
		return Unchanged
	}
	r.line(Good, "Restored built-in skill: %s (%s)", name, ensured.Path)
	r.hub.registry.Forget()
	return Installed
}

// An identifier as given when it names a path, else the one a short name resolves to through a search; empty when none or several do
func (r *run) full(ctx context.Context, identifier string, sources []Source) string {
	if strings.Contains(identifier, "/") {
		return identifier
	}
	r.line(Dim, "Resolving '%s'...", identifier)
	results := r.unified(ctx, sources, identifier, "all", 20)
	exact := exactHits(results, identifier)
	switch {
	case len(exact) == 1:
		r.line(Dim, "Resolved to: %s", exact[0].Identifier)
		return exact[0].Identifier
	case len(exact) > 1:
		official := slices.DeleteFunc(slices.Clone(exact), func(m Meta) bool { return m.Source != "official" })
		if len(official) == 1 {
			r.line(Dim, "Resolved to: %s (official catalog)", official[0].Identifier)
			return official[0].Identifier
		}
		r.line(Plain, "")
		r.line(Warn, "Multiple skills named '%s' found:", identifier)
		var b strings.Builder
		table := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintln(table, "SOURCE\tTRUST\tIDENTIFIER")
		for _, m := range exact {
			fmt.Fprintf(table, "%s\t%s\t%s\n", m.Source, trustLabel(m.Trust, m.Source, "official"), m.Identifier)
		}
		table.Flush()
		r.line(Plain, strings.TrimRight(b.String(), "\n"))
		r.line(Heading, "Use the full identifier to install a specific one.")
		return ""
	case len(results) > 0:
		r.line(Warn, "No exact match for '%s'. Did you mean one of these?", identifier)
		for _, m := range head(results, 5) {
			r.line(Plain, "  %s — %s", m.Name, m.Identifier)
		}
		return ""
	}
	r.fail("No skill named '%s' found in any source.", identifier)
	return ""
}

// Label of a trust level, the label given for an official skill
func trustLabel(trust, source, official string) string {
	if source == "official" {
		return official
	}
	return trust
}

// Name without anything but its letters and digits, lower case
func slugKey(text string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, strings.ToLower(text))
}

// Results named as the name is, else whose name or identifier's last part is the name once punctuation is dropped
func exactHits(results []Meta, name string) []Meta {
	query := strings.ToLower(strings.TrimSpace(name))
	var hits []Meta
	for _, m := range results {
		if strings.ToLower(strings.TrimSpace(m.Name)) == query {
			hits = append(hits, m)
		}
	}
	if len(hits) > 0 {
		return hits
	}
	key := slugKey(query)
	if key == "" {
		return nil
	}
	for _, m := range results {
		if key == slugKey(m.Name) || key == slugKey(m.Identifier[strings.LastIndex(m.Identifier, "/")+1:]) {
			hits = append(hits, m)
		}
	}
	return hits
}

// What a source says of the skill and its files, from the first source that delivers them; else what the first that knows it says
func (r *run) resolve(ctx context.Context, identifier string, sources []Source) (*Meta, *Bundle, Source) {
	var first *Meta
	var from Source
	for _, src := range sources {
		meta := src.Inspect(ctx, identifier)
		if bundle := src.Fetch(ctx, identifier); bundle != nil {
			if meta == nil {
				meta = src.Inspect(ctx, identifier)
			}
			return meta, bundle, src
		}
		if first == nil && meta != nil {
			first, from = meta, src
		}
	}
	return first, nil, from
}

// Says why a skill could not be downloaded: gone upstream though listed, refused credentials, the rate limit, or the network
func (r *run) fetchFailure(identifier string, listed bool, source Source) {
	limited, rejected := r.gh.rateLimited(), r.gh.auth.refusals()
	if listed && !limited && len(rejected) == 0 {
		where := "the registry"
		if source != nil {
			where = source.ID()
		}
		r.fail("'%s' is listed in the %s index, but its files no longer exist upstream.", identifier, where)
		r.line(Dim, "Stale index entry: the skill was likely renamed or removed by its author. Try `icebath skills search` for an alternative.")
		return
	}
	r.fail("Could not download '%s'.", identifier)
	if len(rejected) > 0 {
		r.line(Warn, "Hint: GitHub rejected %s (401 Bad credentials). Replace or remove the token (GITHUB_TOKEN, GH_TOKEN or the setting skills.hub.github_token), or re-run gh auth login.",
			strings.Join(rejected, " and "))
	}
	if limited {
		r.line(Warn, "Hint: GitHub API rate limit exhausted (unauthenticated: 60 requests/hour).")
		r.line(Plain, "Set GITHUB_TOKEN or install the gh CLI and run gh auth login to raise the limit to 5,000/hr.")
		return
	}
	r.line(Plain, "Check the name with icebath skills search %s and check your internet connection.", identifier[strings.LastIndex(identifier, "/")+1:])
}

// Why a scan kept a skill out, the force policy and the way to read it without installing
func blockMessage(result Result, identifier string) string {
	findings := "high-risk patterns"
	if n := len(result.Findings); n > 0 {
		findings = fmt.Sprintf("%d high-risk pattern(s)", n)
	}
	policy := "Re-run with --force to install anyway."
	if result.Verdict == "dangerous" && (result.Trust == "community" || result.Trust == "trusted") {
		policy = "Icebath never installs unverified skills with high-risk findings, even with --force."
	}
	return fmt.Sprintf("the security scan found %s in '%s' (listed above). %s Review the findings or ask the author to fix them; to read the skill without "+
		"installing, run `icebath skills inspect %s`.", findings, identifier, policy, identifier)
}

// Address a person can read a bundle at: what its metadata names, its GitHub folder, else its identifier
func sourceURL(b *Bundle) string {
	for _, key := range []string{"source_url", "url"} {
		if v, ok := b.Metadata[key]; ok && config.Truthy(v) {
			return fmt.Sprint(v)
		}
	}
	if b.Source == "github" {
		if parts := strings.SplitN(b.Identifier, "/", 3); len(parts) >= 2 {
			suffix := ""
			if len(parts) == 3 {
				suffix = "/tree/main/" + parts[2]
			}
			return "https://github.com/" + parts[0] + "/" + parts[1] + suffix
		}
	}
	return b.Identifier
}

// Lines of what a registry says of a skill besides: its pages, its install command, its installs and its audits
func Details(extra map[string]any) []string {
	var lines []string
	for _, field := range [][2]string{{"repo_url", "Repo"}, {"detail_url", "Detail Page"}, {"index_url", "Index"}, {"endpoint", "Endpoint"},
		{"install_command", "Install Command"}, {"installs", "Installs"}, {"weekly_installs", "Weekly Installs"}} {
		if v, ok := extra[field[0]]; ok && v != nil && (config.Truthy(v) || field[0] == "installs") {
			lines = append(lines, field[1]+": "+fmt.Sprint(v))
		}
	}
	if audits, ok := extra["security_audits"].(map[string]any); ok && len(audits) > 0 {
		var parts []string
		for _, name := range slices.Sorted(maps.Keys(audits)) {
			parts = append(parts, name+"="+fmt.Sprint(audits[name]))
		}
		lines = append(lines, "Security: "+strings.Join(parts, ", "))
	}
	return lines
}

// Writes a bundle into its own folder of the quarantine, every path checked first
func (r *run) quarantineBundle(b *Bundle, id string) (string, error) {
	name, err := skillName(b.Name)
	if err != nil {
		return "", &refused{err.Error()}
	}
	rels := make([]string, len(b.Files))
	for i, f := range b.Files {
		if rels[i], err = bundlePath(f.Path); err != nil {
			return "", &refused{err.Error()}
		}
	}
	dest := filepath.Join(r.quarantine(), id, name)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}
	for i, f := range b.Files {
		path := filepath.Join(dest, filepath.FromSlash(rels[i]))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(path, f.Data, 0o644); err != nil {
			return "", err
		}
	}
	return dest, nil
}

// Refuses an install that would nest in a skill, replace a file, or wipe a category of other skills
func (r *run) checkTarget(dir string) error {
	root := resolved(r.scope.Dir())
	for ancestor := filepath.Dir(dir); ancestor != root && inside(ancestor, root); ancestor = filepath.Dir(ancestor) {
		if info, err := os.Stat(filepath.Join(ancestor, "SKILL.md")); err == nil && info.Mode().IsRegular() {
			return refuse("Refusing to install into '%s': it is an existing skill directory, not a category. Choose a different category.", filepath.Base(ancestor))
		}
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil
	}
	if !info.IsDir() {
		return refuse("Refusing to install: '%s' already exists and is not a directory. Remove it or choose a different skill name.", filepath.Base(dir))
	}
	if fsx.Exists(filepath.Join(dir, "SKILL.md")) {
		return nil
	}
	if held := skills.Categories(dir); len(held) > 0 {
		return refuse("Refusing to overwrite category directory '%s' which contains %d skill(s): %s. Use a different --name or install into a subcategory.",
			dir, len(held), strings.Join(held, ", "))
	}
	return nil
}

// Moves a staged skill from quarantine into the skills folder and records it in the lock and the audit log
func (r *run) place(ctx context.Context, st *staged, category string) (string, error) {
	b := st.bundle
	name, err := skillName(b.Name)
	if err != nil {
		return "", &refused{err.Error()}
	}
	rel := name
	if category != "" {
		parent, err := clean(category, "install parent path", true)
		if err != nil {
			return "", &refused{err.Error()}
		}
		rel = parent + "/" + name
	}
	if !inside(resolved(st.dir), resolved(r.quarantine())) {
		return "", refuse("Unsafe quarantine path: %s", st.dir)
	}
	dir, err := r.installDir(rel, name)
	if err != nil {
		return "", &refused{err.Error()}
	}
	if err := r.checkTarget(dir); err != nil {
		return "", err
	}
	if info, err := os.Stat(filepath.Join(st.dir, "SKILL.md")); err == nil && info.Size() > 100_000 {
		slog.Warn(fmt.Sprintf("Skill '%s' has a large SKILL.md (%s chars). Large skills consume significant context when loaded. Consider asking the author to split it into smaller files.",
			name, strs.Thousands(info.Size())))
	}
	var linked string
	filepath.WalkDir(st.dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && linked == "" && p != st.dir && redirect(p) {
			linked, _ = filepath.Rel(st.dir, p)
		}
		return nil
	})
	if linked != "" {
		return "", refuse("Installed skill contains symlinks, which is not allowed: %s", linked)
	}
	if fsx.Exists(dir) {
		if err := os.RemoveAll(dir); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(st.dir, dir); err != nil {
		return "", err
	}
	hash := skills.ContentHash(dir)
	installPath, err := filepath.Rel(resolved(r.scope.Dir()), dir)
	if err != nil {
		return "", err
	}
	installPath = filepath.ToSlash(installPath)
	if _, err := lockPath(installPath, name); err != nil {
		return "", &refused{err.Error()}
	}
	files := make([]string, len(b.Files))
	for i, f := range b.Files {
		files[i] = f.Path
	}
	metadata := b.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	attested, err := json.Marshal(st.prov)
	if err != nil {
		return "", err
	}
	stamp := time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")
	err = skills.UpdateLock(ctx, r.scope.Dir(), func(lock *skills.Lock) (bool, error) {
		lock.Installed[name] = &skills.Installed{Source: b.Source, Identifier: b.Identifier, Trust: b.Trust, Verdict: st.result.Verdict,
			ContentHash: hash, InstallPath: installPath, Files: files, Metadata: metadata, ScanProvenance: attested, InstalledAt: stamp, UpdatedAt: stamp}
		return true, nil
	})
	if err != nil {
		return "", err
	}
	r.audit("INSTALL", name, b.Source, b.Trust, st.result.Verdict, hash)
	if err := r.scope.Installed(ctx, name); err != nil {
		slog.Warn("Could not record the installed skill's usage", "skill", name, "error", err)
	}
	return dir, nil
}

// Category folders of the skills folder: folders holding skills below them that are no skill themselves
func (r *run) categories() []string {
	return slices.DeleteFunc(skills.Categories(r.scope.Dir()), func(name string) bool {
		return fsx.Exists(filepath.Join(r.scope.Dir(), name, "SKILL.md"))
	})
}

// Offers an installed skill's blueprint to the scheduler as a job to schedule; never schedules it
func (r *run) announce(ctx context.Context, name string) {
	blueprint, err := r.scope.Blueprint(name)
	if err != nil {
		r.line(Warn, "Blueprint block present but invalid: %s", err)
		return
	}
	if blueprint == nil {
		return
	}
	lead := fmt.Sprintf("Blueprint: '%s' is an automation (schedule %s)", name, blueprint.Schedule)
	offered, err := r.hub.registry.Suggest(ctx, r.cfg, blueprint)
	if err != nil {
		slog.Debug("The blueprint of an installed skill was not offered", "skill", name, "error", err)
		return
	}
	if offered {
		r.line(Accent, lead+".")
		r.line(Dim, "Added to your suggestions — run /suggestions to schedule or dismiss it.")
		return
	}
	r.line(Accent, lead+", but it wasn't added to your suggestions (already offered/dismissed, or the pending list is full — run /suggestions to review).")
	r.line(Dim, "You can still schedule it any time by asking the agent or via icebath cron add.")
}

// Short digest of a bundle's files and their paths, the same ContentHash gives the folder once installed
func bundleHash(b *Bundle) string {
	files := map[string][]byte{}
	for _, f := range b.Files {
		files[strings.ReplaceAll(f.Path, `\`, "/")] = f.Data
	}
	h := sha256.New()
	for _, path := range slices.Sorted(maps.Keys(files)) {
		h.Write([]byte(path))
		h.Write([]byte{0})
		h.Write(files[path])
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))[:16]
}
