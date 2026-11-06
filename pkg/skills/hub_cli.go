package skills

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	validInstalledNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	validCategoryRe      = regexp.MustCompile(`^[a-z][a-z0-9_/-]*$`)
	slugKeyRe            = regexp.MustCompile(`[^a-z0-9]+`)
	trustStyle           = map[string]string{"builtin": "bright_cyan", "trusted": "green", "community": "yellow", "local": "dim"}
	trustRank            = map[string]int{"builtin": 3, "trusted": 2, "community": 1}
	browseLimits         = map[string]int{"hermes-index": 1000000, "official": 200, "skills-sh": 200, "well-known": 50,
		"github": 200, "clawhub": 500, "lobehub": 500, "browse-sh": 500}
	browseAPILimits = map[string]int{"hermes-index": 5000, "official": 100, "skills-sh": 100, "well-known": 25,
		"github": 100, "clawhub": 50, "lobehub": 50, "browse-sh": 500}
	extraMetaLabels = [][2]string{{"repo_url", "Repo"}, {"detail_url", "Detail Page"}, {"index_url", "Index"},
		{"endpoint", "Endpoint"}, {"install_command", "Install Command"}, {"installs", "Installs"},
		{"weekly_installs", "Weekly Installs"}}
)

// Result of a hub command that Python returns as True, False or None
type HubOutcome int8

const (
	HubNoop   HubOutcome = 0
	HubOK     HubOutcome = 1
	HubFailed HubOutcome = -1
)

// Skills hub command runner over one scope, hermes_cli/skills_hub.py with its module globals as fields
type HubCLI struct {
	S   *Scope
	Hub *Hub
	// Console every command prints to unless one is passed in
	C *pyrich.Console
	// Plain stdout of print and sys.stdout.write
	Stdout io.Writer
	// input() of the confirm prompt and line_input of the name and category prompts
	Input     func(prompt string) (string, error)
	LineInput func(prompt string) (string, error)
	Now       func() time.Time
	// GitHub repos API base and client of the publish flow
	GitHubAPI  string
	HTTPClient *http.Client
	// Source router of every hub command, nil builds the configured router as _sources does
	Sources func() []SkillSource
	seams   hubCLISeams
}

// Replaceable collaborators, nil fields run the real implementation
type hubCLISeams struct {
	unifiedSearch        func(ctx context.Context, query string, sources []SkillSource, sourceFilter string, limit int) []SkillMeta
	parallelSearch       func(ctx context.Context, sources []SkillSource, query string, limits map[string]int, sourceFilter string, timeout time.Duration, onDone func(string, int)) ([]SkillMeta, *pyjson.Object, []string)
	checkUpdates         func(ctx context.Context, name string) ([]SkillUpdateCheck, error)
	install              func(ctx context.Context, identifier string, o HubInstallOptions) (HubOutcome, error)
	resolveMetaAndBundle func(ctx context.Context, identifier string, sources []SkillSource) (*SkillMeta, *SkillBundle, SkillSource)
	resolveURLBundleName func(c *pyrich.Console, b *SkillBundle, meta *SkillMeta, identifier, nameOverride string, skipConfirm bool) bool
	recordInstall        func(ctx context.Context, kind, source string, name any, outcome string, failureClass, registry any, err error)
	disabledSkills       func(platform string) map[string]bool
	uninstall            func(ctx context.Context, name string, c *pyrich.Console, skipConfirm, deferCache bool) (HubOutcome, error)
	uninstallSkill       func(name string) (bool, string, error)
	tier1Enabled         func() bool
	tier1Scan            func(ctx context.Context, skillDir string, timeout int) (Tier1Report, error)
}

// Uninstall command the CLI and slash dispatchers run, the real DoUninstall unless a test replaced it
func (x *HubCLI) doUninstall(ctx context.Context, name string, c *pyrich.Console, skipConfirm, deferCache bool) (HubOutcome, error) {
	if x.seams.uninstall != nil {
		return x.seams.uninstall(ctx, name, c, skipConfirm, deferCache)
	}
	return x.DoUninstall(ctx, name, c, skipConfirm, deferCache)
}

// Removal of a hub installed skill through the hub, the real UninstallSkill unless a test replaced it
func (x *HubCLI) uninstallSkill(name string) (bool, string, error) {
	if x.seams.uninstallSkill != nil {
		return x.seams.uninstallSkill(name)
	}
	return x.Hub.UninstallSkill(name)
}

// Disabled skill names of the scope config for the platform, an empty platform resolving from the environment
func (x *HubCLI) disabledSkillNames(platform string) map[string]bool {
	if x.seams.disabledSkills != nil {
		return x.seams.disabledSkills(platform)
	}
	return x.S.DisabledSkillNames(platform)
}

// Hub command runner over a scope, printing to stdout
func NewHubCLI(s *Scope) *HubCLI {
	return &HubCLI{S: s, Hub: NewHub(s), C: pyrich.NewConsole(os.Stdout), Stdout: os.Stdout,
		Input:     func(prompt string) (string, error) { return cliui.Default().Input(prompt) },
		LineInput: func(prompt string) (string, error) { return cliui.Default().LineInput(prompt) },
		Now:       time.Now, GitHubAPI: "https://api.github.com/repos", HTTPClient: &http.Client{}}
}

func (x *HubCLI) console(c *pyrich.Console) *pyrich.Console {
	if c != nil {
		return c
	}
	return x.C
}

// Source label of a result row, GitHub tap rows showing their provider
func displaySource(r SkillMeta) string {
	if r.Source == "github" && r.Extra != nil {
		if v, ok := r.Extra.Get("provider"); ok && pyrepr.Truthy(v) {
			return pyrepr.Str(v)
		}
	}
	return r.Source
}

// Styled trust cell, official rows showing the official label instead of the level
func trustCell(trustLevel, source, officialLabel string) string {
	label := trustLevel
	if source == "official" {
		label = officialLabel
	}
	style, ok := trustStyle[trustLevel]
	if !ok {
		style = "dim"
	}
	return "[" + style + "]" + label + "[/]"
}

// First width characters of text plus an ellipsis when it was longer
func truncateText(text string, width int) string {
	r := []rune(text)
	if len(r) > width {
		return string(r[:width]) + "..."
	}
	return text
}

func identCol(style string) pyrich.Column {
	return pyrich.Column{Header: "Identifier", Style: style, Overflow: "fold"}
}

func dimCol(header string) pyrich.Column { return pyrich.Column{Header: header, Style: "dim"} }

// Asks Confirm [y/N] and reports yes, an EOF or interrupt counting as no
func (x *HubCLI) confirm() bool {
	answer, err := x.Input("Confirm [y/N]: ")
	if err != nil {
		return false
	}
	a := strings.ToLower(strings.TrimSpace(answer))
	return a == "y" || a == "yes"
}

// Prints the lines and asks to confirm, printing cancel on refusal
func (x *HubCLI) confirmOrCancel(c *pyrich.Console, cancel string, lines ...string) bool {
	for _, l := range lines {
		c.Print(l)
	}
	if x.confirm() {
		return true
	}
	c.Print(cancel)
	return false
}

const cancelledLine = "[dim]Cancelled.[/]\n"

// Clears the skills prompt cache now or tells the user the change lands next session
func (x *HubCLI) finishChange(c *pyrich.Console, invalidate bool, what, verb string, notice bool) {
	if invalidate {
		ClearSkillsSystemPromptCache(x.S.Home, true)
		return
	}
	if !notice {
		return
	}
	c.Print(fmt.Sprintf("[dim]%s in your next session.[/]", what))
	c.Print(fmt.Sprintf("[dim]Use /reset to start a new session now, or --now to %s immediately (invalidates prompt cache).[/]\n", verb))
}

func (x *HubCLI) finishDefault(c *pyrich.Console, invalidate bool) {
	x.finishChange(c, invalidate, "Change will take effect", "apply", true)
}

// One unwrapped red error line
func printHubError(c *pyrich.Console, message string) {
	c.PrintSoft(fmt.Sprintf("[bold red]Error:[/] %s\n", message))
}

func printListed(c *pyrich.Console, label string, items []string) {
	if len(items) > 0 {
		c.Print(fmt.Sprintf("[dim]%s: %s[/]", label, strings.Join(items, ", ")))
	}
}

func reportPair(c *pyrich.Console, success bool, msg string) bool {
	if success {
		c.Print(fmt.Sprintf("[bold green]%s[/]\n", msg))
	} else {
		printHubError(c, msg)
	}
	return success
}

// Green message of an ok result, red error otherwise
func reportOK(c *pyrich.Console, result map[string]any, fallback string) bool {
	if pyrepr.Truthy(result["ok"]) {
		c.Print(fmt.Sprintf("[bold green]%s[/]", pyrepr.Str(result["message"])))
		return true
	}
	msg, ok := result["message"]
	if !ok {
		msg = fallback
	}
	printHubError(c, pyrepr.Str(msg))
	return false
}

// First 50 lines of the bundle SKILL.md with a count of the rest, false when it has none
func skillMDPreview(b *SkillBundle) (string, bool) {
	if b == nil {
		return "", false
	}
	data, ok := b.File("SKILL.md")
	if !ok {
		return "", false
	}
	lines := strings.Split(pystr.DecodeUTF8Replace(data), "\n")
	more := ""
	if len(lines) > 50 {
		more = fmt.Sprintf("\n\n... (%d more lines)", len(lines)-50)
		lines = lines[:50]
	}
	return strings.Join(lines, "\n") + more, true
}

// Labelled upstream metadata lines, installs shown even when zero, security audits sorted by name
func formatExtraMetadataLines(extra *pyjson.Object) []string {
	var lines []string
	if extra == nil {
		return lines
	}
	for _, kl := range extraMetaLabels {
		v, ok := extra.Get(kl[0])
		if ok && v != nil && (pyrepr.Truthy(v) || kl[0] == "installs") {
			lines = append(lines, fmt.Sprintf("[bold]%s:[/] %s", kl[1], pyrepr.Str(v)))
		}
	}
	if sec, ok := extra.Get("security_audits"); ok {
		if obj, ok := sec.(*pyjson.Object); ok && obj.Len() > 0 {
			keys := append([]string(nil), obj.Keys...)
			sort.Strings(keys)
			parts := make([]string, len(keys))
			for i, k := range keys {
				v, _ := obj.Get(k)
				parts[i] = k + "=" + pyrepr.Str(v)
			}
			lines = append(lines, "[bold]Security:[/] "+strings.Join(parts, ", "))
		}
	}
	return lines
}

// Separator insensitive comparison key of a name
func slugKey(text string) string { return slugKeyRe.ReplaceAllString(strings.ToLower(text), "") }

func identifierSlug(identifier string) string {
	parts := strings.Split(identifier, "/")
	return parts[len(parts)-1]
}

// Results whose display name is the name, else whose name or identifier slug matches it ignoring separators
func exactNameHits(results []SkillMeta, name string) []SkillMeta {
	query := strings.ToLower(strings.TrimSpace(name))
	var hits []SkillMeta
	for _, r := range results {
		if strings.ToLower(strings.TrimSpace(r.Name)) == query {
			hits = append(hits, r)
		}
	}
	if len(hits) > 0 {
		return hits
	}
	key := slugKey(query)
	if key == "" {
		return nil
	}
	for _, r := range results {
		if key == slugKey(r.Name) || key == slugKey(identifierSlug(r.Identifier)) {
			hits = append(hits, r)
		}
	}
	return hits
}

// Metric record of one install attempt, nil fields absent from the Python dict
type installAttempt struct {
	registry, failureClass, origin *string
}

func (x *HubCLI) sources() []SkillSource {
	if x.Sources != nil {
		return x.Sources()
	}
	return x.Hub.createSourceRouter(NewGitHubAuth(x.S))
}

func (x *HubCLI) unifiedSearch(ctx context.Context, query string, sources []SkillSource, sourceFilter string, limit int) []SkillMeta {
	if x.seams.unifiedSearch != nil {
		return x.seams.unifiedSearch(ctx, query, sources, sourceFilter, limit)
	}
	return x.Hub.UnifiedSearch(ctx, query, sources, sourceFilter, limit)
}

func (x *HubCLI) parallelSearch(ctx context.Context, sources []SkillSource, limits map[string]int, sourceFilter string, onDone func(string, int)) ([]SkillMeta, *pyjson.Object, []string) {
	if x.seams.parallelSearch != nil {
		return x.seams.parallelSearch(ctx, sources, "", limits, sourceFilter, 30*time.Second, onDone)
	}
	return x.Hub.ParallelSearchSources(ctx, sources, "", limits, sourceFilter, 30*time.Second, onDone)
}

// Full identifier of a short name through search, empty when ambiguous or missing
func (x *HubCLI) resolveShortName(ctx context.Context, name string, sources []SkillSource, c *pyrich.Console, attempt *installAttempt) string {
	if attempt == nil {
		attempt = &installAttempt{}
	}
	c.Print(fmt.Sprintf("[dim]Resolving '%s'...[/]", name))
	results := x.unifiedSearch(ctx, name, sources, "all", 20)
	exact := exactNameHits(results, name)
	if len(exact) == 1 {
		c.Print(fmt.Sprintf("[dim]Resolved to: %s[/]", exact[0].Identifier))
		attempt.registry = strPtr(exact[0].Source)
		return exact[0].Identifier
	}
	if len(exact) > 1 {
		var official []SkillMeta
		for _, r := range exact {
			if r.Source == "official" {
				official = append(official, r)
			}
		}
		if len(official) == 1 {
			c.Print(fmt.Sprintf("[dim]Resolved to: %s (official catalog)[/]", official[0].Identifier))
			attempt.registry = strPtr("official")
			return official[0].Identifier
		}
		c.Print(fmt.Sprintf("\n[yellow]Multiple skills named '%s' found:[/]", name))
		table := &pyrich.Table{Columns: []pyrich.Column{dimCol("Source"), dimCol("Trust"), identCol("bold cyan")}}
		for _, r := range exact {
			table.AddRow(r.Source, trustCell(r.TrustLevel, r.Source, "official"), r.Identifier)
		}
		c.PrintTable(table)
		c.Print("[bold]Use the full identifier to install a specific one.[/]\n")
		attempt.failureClass = strPtr("ambiguous")
		return ""
	}
	if len(results) > 0 {
		c.Print(fmt.Sprintf("[yellow]No exact match for '%s'. Did you mean one of these?[/]", name))
		for _, r := range results[:min(5, len(results))] {
			c.Print(fmt.Sprintf("  [cyan]%s[/] — %s", r.Name, r.Identifier))
		}
		c.Print()
		attempt.failureClass = strPtr("not_found")
		return ""
	}
	printHubError(c, fmt.Sprintf("No skill named '%s' found in any source.", name))
	attempt.failureClass = strPtr("not_found")
	return ""
}

// Meta, bundle and source from one adapter, else the first metadata only hit
func (x *HubCLI) resolveSourceMetaAndBundle(ctx context.Context, identifier string, sources []SkillSource) (*SkillMeta, *SkillBundle, SkillSource) {
	if x.seams.resolveMetaAndBundle != nil {
		return x.seams.resolveMetaAndBundle(ctx, identifier, sources)
	}
	var firstMeta *SkillMeta
	var firstSource SkillSource
	for _, src := range sources {
		meta, err := src.Inspect(ctx, identifier)
		if err != nil {
			meta = nil
		}
		bundle, err := src.Fetch(ctx, identifier)
		if err == nil && bundle != nil {
			if meta == nil {
				if m, err := src.Inspect(ctx, identifier); err == nil {
					meta = m
				}
			}
			return meta, bundle, src
		}
		if firstMeta == nil && meta != nil {
			firstMeta, firstSource = meta, src
		}
	}
	return firstMeta, nil, firstSource
}

func (x *HubCLI) fullIdentifier(ctx context.Context, identifier string, sources []SkillSource, c *pyrich.Console, attempt *installAttempt) string {
	if strings.Contains(identifier, "/") {
		return identifier
	}
	return x.resolveShortName(ctx, identifier, sources, c, attempt)
}

// Short name resolution plus the meta, bundle and source inside one pooled hub session
func (x *HubCLI) resolveIdentifier(ctx context.Context, identifier string, sources []SkillSource, c *pyrich.Console) (string, *SkillMeta, *SkillBundle, SkillSource) {
	ctx, done := x.Hub.HTTPSession(ctx)
	defer done()
	identifier = x.fullIdentifier(ctx, identifier, sources, c, nil)
	if identifier == "" {
		return "", nil, nil, nil
	}
	meta, bundle, src := x.resolveSourceMetaAndBundle(ctx, identifier, sources)
	return identifier, meta, bundle, src
}

// Identifier shaped names other than empty and sentinel values
func isValidInstalledSkillName(name string) bool {
	candidate := strings.ToLower(strings.TrimSpace(name))
	switch candidate {
	case "", "skill", "readme", "index", "unnamed-skill":
		return false
	}
	return validInstalledNameRe.MatchString(candidate)
}

// Sorted category buckets under the skills dir that hold no SKILL.md of their own
func (x *HubCLI) existingCategories() []string {
	dir := x.S.SkillsDir()
	names, err := categorySkillDirs(dir)
	if err != nil {
		return []string{}
	}
	seen := map[string]bool{}
	out := []string{}
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		if _, err := os.Stat(dir + "/" + n + "/SKILL.md"); err != nil {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// Stripped interactive line, false on EOF or interrupt
func (x *HubCLI) lineInput(prompt string) (string, bool) {
	answer, err := x.LineInput(prompt)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(answer), true
}

// Interactive skill name, false on cancel or an invalid answer
func (x *HubCLI) promptForSkillName(c *pyrich.Console, url, def string) (string, bool) {
	c.Print()
	c.Print(fmt.Sprintf("[yellow]The SKILL.md at %s doesn't declare a `name:` in its frontmatter,[/]\n"+
		"[yellow]and the URL path doesn't produce a valid identifier either.[/]", url))
	hint := ""
	if def != "" {
		hint = " [" + def + "]"
	}
	c.Print(fmt.Sprintf("[bold]Enter a skill name%s:[/] [dim](lowercase letters, digits, hyphens, underscores; starts with a letter)[/]", hint))
	answer, ok := x.lineInput("Name: ")
	if !ok {
		return "", false
	}
	if answer == "" {
		answer = def
	}
	if !isValidInstalledSkillName(answer) {
		c.Print(fmt.Sprintf("[bold red]Invalid name:[/] %s. Aborting install.\n", pyrepr.Repr(answer)))
		return "", false
	}
	return answer, true
}

// Interactive category, empty meaning a flat install
func (x *HubCLI) promptForCategory(c *pyrich.Console, existing []string) string {
	c.Print()
	if len(existing) > 0 {
		c.Print("[bold]Pick a category[/] [dim](reuse an existing bucket, type a new one, or press Enter to install flat)[/]")
		c.Print(fmt.Sprintf("[dim]Existing: %s[/]", strings.Join(existing, ", ")))
	} else {
		c.Print(fmt.Sprintf("[bold]Category[/] [dim](optional — press Enter to install flat at %s/skills/<name>/)[/]", home.Display(x.S.Home)))
	}
	answer, _ := x.lineInput("Category: ")
	if answer != "" && !validCategoryRe.MatchString(answer) {
		c.Print(fmt.Sprintf("[dim]Invalid category %s — installing flat.[/]", pyrepr.Repr(answer)))
		return ""
	}
	return answer
}

// Searches the registries and prints a table, or a JSON array for scripting
func (x *HubCLI) DoSearch(ctx context.Context, query, source string, limit int, c *pyrich.Console, asJSON bool) error {
	c = x.console(c)
	sources := x.sources()
	if asJSON {
		results := x.unifiedSearch(ctx, query, sources, source, limit)
		rows := make([]any, len(results))
		for i, r := range results {
			rows[i] = objOf("name", r.Name, "identifier", r.Identifier, "source", r.Source,
				"trust_level", r.TrustLevel, "description", r.Description)
		}
		text, err := pyjson.Dumps(rows, pyjson.Indent(2))
		if err != nil {
			return err
		}
		_, err = io.WriteString(x.Stdout, text+"\n")
		return err
	}
	c.Print(fmt.Sprintf("\n[bold]Searching for:[/] %s", query))
	st := c.Status("[bold]Searching registries...")
	results := x.unifiedSearch(ctx, query, sources, source, limit)
	st.Stop()
	if len(results) == 0 {
		c.Print("[dim]No skills found matching your query.[/]\n")
		return nil
	}
	table := &pyrich.Table{Title: fmt.Sprintf("Skills Hub — %d result(s)", len(results)), Columns: []pyrich.Column{
		{Header: "Name", Style: "bold cyan"}, {Header: "Description", MaxWidth: 60}, dimCol("Source"), dimCol("Trust"), identCol("dim")}}
	for _, r := range results {
		table.AddRow(r.Name, truncateText(r.Description, 60), displaySource(r), trustCell(r.TrustLevel, r.Source, "official"), r.Identifier)
	}
	c.PrintTable(table)
	c.Print("[dim]Use: icebath skills inspect <identifier> to preview, icebath skills install <identifier> to install (--json for scripting)[/]\n")
	return nil
}

// Dedupes by identifier keeping the higher trust, sorts official first and slices one page
func rankAndPage(all []SkillMeta, page, pageSize int) (deduped, items []SkillMeta, outPage, totalPages, start int) {
	rank := func(r SkillMeta) int { return trustRank[r.TrustLevel] }
	seen := map[string]int{}
	for _, r := range all {
		if i, ok := seen[r.Identifier]; !ok {
			seen[r.Identifier] = len(deduped)
			deduped = append(deduped, r)
		} else if rank(r) > rank(deduped[i]) {
			deduped[i] = r
		}
	}
	sort.SliceStable(deduped, func(i, j int) bool {
		a, b := deduped[i], deduped[j]
		if rank(a) != rank(b) {
			return rank(a) > rank(b)
		}
		if (a.Source != "official") != (b.Source != "official") {
			return a.Source == "official"
		}
		return pystr.Lower(a.Name) < pystr.Lower(b.Name)
	})
	totalPages = max(1, (len(deduped)+pageSize-1)/pageSize)
	outPage = max(1, min(page, totalPages))
	start = (outPage - 1) * pageSize
	end := min(len(deduped), start+pageSize)
	return deduped, deduped[min(start, end):end], outPage, totalPages, start
}

// Fetches every filtered source in parallel with a live per source progress status
func (x *HubCLI) fetchBrowseResults(ctx context.Context, c *pyrich.Console, source string) ([]SkillMeta, *pyjson.Object, []string) {
	st := c.Status("[bold]Fetching skills from registries...")
	defer st.Stop()
	var done []string
	onDone := func(sid string, count int) {
		done = append(done, fmt.Sprintf("%s (%d)", sid, count))
		st.Update(fmt.Sprintf("[bold]Fetching skills from registries...[/]  [dim]done: %s[/]", strings.Join(done, ", ")))
	}
	return x.parallelSearch(ctx, x.sources(), browseLimits, source, onDone)
}

func (x *HubCLI) renderBrowsePage(c *pyrich.Console, deduped, items []SkillMeta, page, totalPages, start int, source string, counts *pyjson.Object, timedOut []string) {
	official := 0
	for _, r := range deduped {
		if r.Source == "official" {
			official++
		}
	}
	loaded := fmt.Sprintf("%d skills loaded", len(deduped))
	if len(timedOut) > 0 {
		loaded += fmt.Sprintf(", %d source(s) still loading", len(timedOut))
	}
	label := source
	if source == "all" {
		label = "all sources"
	}
	c.Print(fmt.Sprintf("\n[bold]Skills Hub — Browse — %s[/]  [dim](%s, page %d/%d)[/]", label, loaded, page, totalPages))
	if official > 0 && page == 1 {
		c.Print(fmt.Sprintf("[bright_cyan]★ %d official optional skill(s) from Nous Research[/]", official))
	}
	c.Print()
	table := &pyrich.Table{Columns: []pyrich.Column{
		{Header: "#", Style: "dim", Width: 4, Justify: "right"}, {Header: "Name", Style: "bold cyan", MaxWidth: 22},
		{Header: "Description", MaxWidth: 44}, {Header: "Source", Style: "dim", Width: 12}, {Header: "Trust", Width: 10}, identCol("dim")}}
	for i, r := range items {
		table.AddRow(fmt.Sprint(start+1+i), r.Name, truncateText(r.Description, 44), displaySource(r),
			trustCell(r.TrustLevel, r.Source, "★ official"), r.Identifier)
	}
	c.PrintTable(table)
	var nav []string
	if page > 1 {
		nav = append(nav, fmt.Sprintf("[cyan]--page %d[/] ← prev", page-1))
	}
	if page < totalPages {
		nav = append(nav, fmt.Sprintf("[cyan]--page %d[/] → next", page+1))
	}
	if len(nav) > 0 {
		c.Print("  " + strings.Join(nav, " | "))
	}
	if source == "all" && counts != nil && counts.Len() > 0 {
		keys := append([]string(nil), counts.Keys...)
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			v, _ := counts.Get(k)
			parts[i] = k + ": " + pyrepr.Str(v)
		}
		c.Print(fmt.Sprintf("  [dim]Sources: %s[/]", strings.Join(parts, ", ")))
	}
	if len(timedOut) > 0 {
		c.Print(fmt.Sprintf("  [yellow]⚡ Slow sources skipped: %s — run again for cached results[/]", strings.Join(timedOut, ", ")))
	}
	c.Print("[dim]Tip: 'icebath skills inspect <identifier>' to preview, 'icebath skills install <identifier>' to install, 'icebath skills search <query>' to search deeper[/]\n")
}

// Browses every registry paginated with official skills first
func (x *HubCLI) DoBrowse(ctx context.Context, page, pageSize int, source string, c *pyrich.Console) {
	pageSize = max(1, min(pageSize, 100))
	c = x.console(c)
	all, counts, timedOut := x.fetchBrowseResults(ctx, c, source)
	if len(all) == 0 {
		if providerFilterOf(source) != "" {
			c.Print(fmt.Sprintf("[dim]No skills found for provider '%s'.[/]\n", source))
		} else {
			c.Print("[dim]No skills found in the Skills Hub.[/]\n")
		}
		return
	}
	deduped, items, page, totalPages, start := rankAndPage(all, page, pageSize)
	x.renderBrowsePage(c, deduped, items, page, totalPages, start, source, counts, timedOut)
}

// One row of a programmatic browse page
type BrowseItem struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
	Trust       string `json:"trust"`
	Identifier  string `json:"identifier"`
}

// Programmatic browse page
type BrowsePage struct {
	Items      []BrowseItem `json:"items"`
	Page       int          `json:"page"`
	TotalPages int          `json:"total_pages"`
	Total      int          `json:"total"`
}

// Paginated hub browse for programmatic callers such as the TUI gateway
func (x *HubCLI) BrowseSkills(ctx context.Context, page, pageSize int, source string) BrowsePage {
	pageSize = max(1, min(pageSize, 100))
	all, _, _ := x.parallelSearch(ctx, x.sources(), browseAPILimits, source, nil)
	if len(all) == 0 {
		return BrowsePage{Items: []BrowseItem{}, Page: 1, TotalPages: 1}
	}
	deduped, items, page, totalPages, _ := rankAndPage(all, page, pageSize)
	out := BrowsePage{Items: make([]BrowseItem, len(items)), Page: page, TotalPages: totalPages, Total: len(deduped)}
	for i, r := range items {
		out.Items[i] = BrowseItem{Name: r.Name, Description: r.Description, Source: r.Source, Trust: r.TrustLevel, Identifier: r.Identifier}
	}
	return out
}

// Previews a skill SKILL.md without installing
func (x *HubCLI) DoInspect(ctx context.Context, identifier string, c *pyrich.Console) {
	c = x.console(c)
	identifier, meta, bundle, _ := x.resolveIdentifier(ctx, identifier, x.sources(), c)
	if identifier == "" {
		return
	}
	if meta == nil {
		printHubError(c, fmt.Sprintf("Could not find '%s' in any source.", identifier))
		return
	}
	c.Print()
	info := []string{"[bold]Name:[/] " + meta.Name, "[bold]Description:[/] " + meta.Description, "[bold]Source:[/] " + meta.Source,
		"[bold]Trust:[/] " + trustCell(meta.TrustLevel, meta.Source, "official"), "[bold]Identifier:[/] " + meta.Identifier}
	if len(meta.Tags) > 0 {
		info = append(info, "[bold]Tags:[/] "+strings.Join(meta.Tags, ", "))
	}
	info = append(info, formatExtraMetadataLines(meta.Extra)...)
	c.PrintPanel(pyrich.Panel{Body: strings.Join(info, "\n"), Title: "Skill: " + meta.Name})
	if preview, ok := skillMDPreview(bundle); ok {
		c.PrintPanel(pyrich.Panel{Body: preview, Title: "SKILL.md Preview", Subtitle: "icebath skills install <id> to install"})
	}
	c.Print()
}

// Skill metadata and SKILL.md preview for programmatic callers
type InspectResult struct {
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Source         string   `json:"source"`
	Identifier     string   `json:"identifier"`
	Tags           []string `json:"tags"`
	SkillMDPreview *string  `json:"skill_md_preview,omitempty"`
}

// Metadata of a skill with its SKILL.md preview, nil when unresolved
func (x *HubCLI) InspectSkill(ctx context.Context, identifier string) *InspectResult {
	quiet := &pyrich.Console{Quiet: true, Width: 80}
	ident, meta, bundle, _ := x.resolveIdentifier(ctx, identifier, x.sources(), quiet)
	if ident == "" || meta == nil {
		return nil
	}
	out := &InspectResult{Name: meta.Name, Description: meta.Description, Source: meta.Source, Identifier: meta.Identifier,
		Tags: append([]string{}, meta.Tags...)}
	if preview, ok := skillMDPreview(bundle); ok {
		out.SkillMDPreview = &preview
	}
	return out
}

// Ordered object of alternating keys and values
func objOf(kv ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}
