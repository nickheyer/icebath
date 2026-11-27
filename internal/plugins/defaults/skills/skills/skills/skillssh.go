package skills

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	skillsShBase    = "https://skills.sh"
	skillsShSearch  = skillsShBase + "/api/search"
	skillsShSitemap = "https://www.skills.sh/sitemap.xml"
	// Name every shard of the sitemap that lists skills carries
	skillsShShard = "sitemap-skills"
	// Tries of a shard of the sitemap before the catalog is taken as partial
	catalogRetries = 5
)

var (
	htmlTag         = regexp.MustCompile(`<[^>]+>`)
	sitemapLoc      = regexp.MustCompile(`(?i)<loc>([^<]+)</loc>`)
	sitemapSkill    = regexp.MustCompile(`(?i)^https?://(?:www\.)?skills\.sh/([^/]+)/([^/]+)/([^/]+)/?$`)
	featuredLink    = regexp.MustCompile(`href=["']/([^"'/]+/[^"'/]+/[^"'/]+)["']`)
	installCommand  = regexp.MustCompile(`(?i)npx\s+skills\s+add\s+(https?://github\.com/[^\s<]+|[^\s<]+)(?:\s+--skill\s+([^\s<]+))?`)
	pageHeading     = regexp.MustCompile(`(?is)<h1[^>]*>(.*?)</h1>`)
	proseHeading    = regexp.MustCompile(`(?is)<div[^>]*class=["'][^"']*prose[^"']*["'][^>]*>.*?<h1[^>]*>(.*?)</h1>`)
	proseParagraph  = regexp.MustCompile(`(?is)<div[^>]*class=["'][^"']*prose[^"']*["'][^>]*>.*?<p[^>]*>(.*?)</p>`)
	weeklyInstalls  = regexp.MustCompile(`(?s)Weekly Installs.*?children\\":\\"([0-9.,Kk]+)\\"`)
	auditVerdict    = regexp.MustCompile(`(?i)(Pass|Warn|Fail)`)
	tokenSeparators = regexp.MustCompile(`[^a-z0-9/_-]+`)
	// Prefixes skills.sh identifiers are written with, the misspelled ones included
	skillsShPrefixes = []string{"skills-sh/", "skills.sh/", "skils-sh/", "skils.sh/"}
	// Folders repositories keep their skills in
	skillsShBases = []string{"skills/", ".agents/skills/", ".claude/skills/"}
)

// Text with every HTML tag removed
func stripHTML(value string) string {
	return htmlTag.ReplaceAllString(value, "")
}

func md5hex(text string) string {
	return fmt.Sprintf("%x", md5.Sum([]byte(text)))
}

// Skills skills.sh lists, fetched from their GitHub repositories
type skillsSh struct {
	r *run
}

func (s *skillsSh) ID() string { return "skills-sh" }

// Identifier without a skills.sh prefix
func skillsShCanonical(identifier string) string {
	return stripPrefix(identifier, skillsShPrefixes)
}

func skillsShWrap(canonical string) string {
	return "skills-sh/" + canonical
}

func skillsShURLs(canonical, repo string) map[string]any {
	return map[string]any{"detail_url": skillsShBase + "/" + canonical, "repo_url": "https://github.com/" + repo}
}

func skillsShMeta(canonical, name, description, path string, extra map[string]any) Meta {
	parts := strings.SplitN(canonical, "/", 3)
	if extra == nil {
		extra = map[string]any{}
	}
	return Meta{Name: name, Description: description, Source: "skills.sh", Identifier: skillsShWrap(canonical), Trust: githubTrust(canonical),
		Repo: strings.Join(parts[:min(2, len(parts))], "/"), Path: path, Tags: []string{}, Extra: extra}
}

// Matches of the search API, the whole catalog of the sitemap for an empty query
func (s *skillsSh) Search(ctx context.Context, query string, limit int, _ string) []Meta {
	if strings.TrimSpace(query) == "" {
		return s.catalog(ctx, limit)
	}
	key := "skills_sh_search_" + md5hex(fmt.Sprintf("%s|%d", query, limit))
	var cached []Meta
	if s.r.cached(key, cacheTTL, &cached) {
		return head(cached, limit)
	}
	var data struct {
		Skills []json.RawMessage `json:"skills"`
	}
	if !s.r.getJSON(ctx, skillsShSearch, request{query: url.Values{"q": {query}, "limit": {fmt.Sprint(limit)}}}, &data) {
		return nil
	}
	results := []Meta{}
	for _, item := range head(data.Skills, limit) {
		if m := skillsShItem(item); m != nil {
			results = append(results, *m)
		}
	}
	s.r.keep(key, results)
	return results
}

// One item of the search API, nil without an owner, repository and skill path
func skillsShItem(raw json.RawMessage) *Meta {
	var item struct {
		ID       any             `json:"id"`
		Source   any             `json:"source"`
		SkillID  any             `json:"skillId"`
		Installs json.RawMessage `json:"installs"`
		Name     any             `json:"name"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return nil
	}
	canonical, ok := item.ID.(string)
	if !ok || strings.Count(canonical, "/") < 2 {
		source, ok1 := item.Source.(string)
		skill, ok2 := item.SkillID.(string)
		if !ok1 || !ok2 {
			return nil
		}
		canonical = source + "/" + skill
	}
	repo, path, ok := splitRepo(canonical)
	if !ok {
		return nil
	}
	label := ""
	var installs any
	if len(item.Installs) > 0 {
		json.Unmarshal(item.Installs, &installs)
		if n, err := strconv.ParseInt(strings.TrimSpace(string(item.Installs)), 10, 64); err == nil {
			label = " · " + strs.Thousands(n) + " installs"
		}
	}
	name := path[strings.LastIndex(path, "/")+1:]
	if text, ok := item.Name.(string); ok && text != "" {
		name = text
	}
	extra := skillsShURLs(canonical, repo)
	extra["installs"] = installs
	m := skillsShMeta(canonical, name, "Indexed by skills.sh from "+repo+label, path, extra)
	return &m
}

// Body of a guarded GET of the sitemap
func (s *skillsSh) sitemap(ctx context.Context, raw string, timeout time.Duration) (string, bool) {
	resp := s.r.guarded(ctx, raw, request{timeout: timeout})
	if !resp.ok() {
		return "", false
	}
	return string(resp.body), true
}

// Every skill the sitemap's shards list, cached only when every shard answered; the homepage's featured skills when the sitemap has none
func (s *skillsSh) catalog(ctx context.Context, limit int) []Meta {
	const key = "skills_sh_sitemap_v1"
	trim := func(results []Meta) []Meta {
		if limit > 0 {
			return head(results, limit)
		}
		return results
	}
	var cached []Meta
	if s.r.cached(key, cacheTTL, &cached) {
		return trim(cached)
	}
	root, _ := s.sitemap(ctx, skillsShSitemap, 20*time.Second)
	var shards []string
	for _, match := range sitemapLoc.FindAllStringSubmatch(root, -1) {
		if strings.Contains(match[1], skillsShShard) {
			shards = append(shards, strings.TrimSpace(match[1]))
		}
	}
	if len(shards) == 0 {
		return s.featured(ctx, limit)
	}
	seen := map[string]bool{}
	results := []Meta{}
	partial := false
	for _, shard := range shards {
		var xml string
		var ok bool
		for attempt := 1; attempt <= catalogRetries; attempt++ {
			if xml, ok = s.sitemap(ctx, shard, 30*time.Second); ok || attempt == catalogRetries {
				break
			}
			if !pause(ctx, time.Duration(min(1<<attempt, 8))*time.Second) {
				break
			}
		}
		if !ok {
			partial = true
			continue
		}
		for _, loc := range sitemapLoc.FindAllStringSubmatch(xml, -1) {
			match := sitemapSkill.FindStringSubmatch(strings.TrimSpace(loc[1]))
			if match == nil {
				continue
			}
			repo, skill := match[1]+"/"+match[2], match[3]
			canonical := repo + "/" + skill
			if !seen[canonical] {
				seen[canonical] = true
				results = append(results, skillsShMeta(canonical, skill, "Indexed by skills.sh from "+repo, skill, skillsShURLs(canonical, repo)))
			}
		}
	}
	if len(results) == 0 {
		return s.featured(ctx, limit)
	}
	if !partial {
		s.r.keep(key, results)
	}
	return trim(results)
}

// Skills the skills.sh homepage links to, at most limit
func (s *skillsSh) featured(ctx context.Context, limit int) []Meta {
	const key = "skills_sh_featured"
	var cached []Meta
	if s.r.cached(key, cacheTTL, &cached) {
		return head(cached, limit)
	}
	html, ok := s.r.getText(ctx, skillsShBase, request{})
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	results := []Meta{}
	for _, match := range featuredLink.FindAllStringSubmatch(html, -1) {
		canonical := match[1]
		if seen[canonical] || strings.HasPrefix(canonical, "agents/") || strings.HasPrefix(canonical, "_next/") || strings.HasPrefix(canonical, "api/") {
			continue
		}
		seen[canonical] = true
		if repo, path, ok := splitRepo(canonical); ok {
			results = append(results, skillsShMeta(canonical, path[strings.LastIndex(path, "/")+1:], "Featured on skills.sh from "+repo, path, nil))
			if len(results) >= limit {
				break
			}
		}
	}
	s.r.keep(key, results)
	return results
}

// What a skill's skills.sh page says of it
type detail struct {
	Repo           string            `json:"repo"`
	InstallSkill   string            `json:"install_skill"`
	PageTitle      string            `json:"page_title"`
	BodyTitle      string            `json:"body_title"`
	BodySummary    string            `json:"body_summary"`
	WeeklyInstalls string            `json:"weekly_installs"`
	InstallCommand string            `json:"install_command"`
	DetailURL      string            `json:"detail_url"`
	RepoURL        string            `json:"repo_url"`
	SecurityAudits map[string]string `json:"security_audits"`
}

// The skill's skills.sh page read through the index cache, nil when it can't be had
func (s *skillsSh) detail(ctx context.Context, canonical string) *detail {
	key := "skills_sh_detail_" + md5hex(canonical)
	var cached detail
	if s.r.cached(key, cacheTTL, &cached) {
		return &cached
	}
	html, ok := s.r.getText(ctx, skillsShBase+"/"+canonical, request{})
	if !ok {
		return nil
	}
	d := parseDetail(canonical, html)
	if d != nil {
		s.r.keep(key, d)
	}
	return d
}

// First group of a pattern's first match with its tags removed, empty when there is none
func firstMatch(re *regexp.Regexp, html string) string {
	if match := re.FindStringSubmatch(html); match != nil {
		return strings.TrimSpace(stripHTML(match[1]))
	}
	return ""
}

// Repository, install command, titles, summary, installs and audits of a skill's page
func parseDetail(canonical, html string) *detail {
	repo, skill, ok := splitRepo(canonical)
	if !ok {
		return nil
	}
	d := &detail{Repo: repo, InstallSkill: skill}
	if match := installCommand.FindStringSubmatch(html); match != nil {
		d.InstallCommand = strings.TrimSpace(match[0])
		if match[2] != "" {
			d.InstallSkill = match[2]
		}
		d.InstallSkill = strings.TrimSpace(d.InstallSkill)
		parts := strings.Split(strings.Trim(strings.TrimPrefix(strings.TrimSpace(match[1]), "https://github.com/"), "/"), "/")
		if len(parts) >= 2 {
			d.Repo = parts[0] + "/" + parts[1]
		}
	}
	d.PageTitle, d.BodyTitle, d.BodySummary = firstMatch(pageHeading, html), firstMatch(proseHeading, html), firstMatch(proseParagraph, html)
	if match := weeklyInstalls.FindStringSubmatch(html); match != nil {
		d.WeeklyInstalls = match[1]
	}
	d.DetailURL, d.RepoURL = skillsShBase+"/"+canonical, "https://github.com/"+d.Repo
	d.SecurityAudits = map[string]string{}
	for _, audit := range []string{"agent-trust-hub", "socket", "snyk"} {
		at := strings.Index(html, "/security/"+audit)
		if at < 0 {
			continue
		}
		window := []rune(html[at:])
		if match := auditVerdict.FindStringSubmatch(string(window[:min(500, len(window))])); match != nil {
			d.SecurityAudits[audit] = strings.ToUpper(match[1][:1]) + strings.ToLower(match[1][1:])
		}
	}
	return d
}

// The identifier and its variants under the usual skills folders, the identifier first
func skillsShCandidates(canonical string) []string {
	repo, path, ok := splitRepo(canonical)
	if !ok {
		return []string{canonical}
	}
	path = strings.TrimLeft(path, "/")
	out := []string{repo + "/" + path}
	for _, base := range skillsShBases {
		if candidate := repo + "/" + base + path; candidate != out[0] {
			out = append(out, candidate)
		}
	}
	return out
}

// The skill from GitHub at the first candidate that answers, else where its repository holds it, kept as a skills.sh install
func (s *skillsSh) Fetch(ctx context.Context, identifier string) *Bundle {
	canonical := skillsShCanonical(identifier)
	d := s.detail(ctx, canonical)
	relabel := func(github string) *Bundle {
		if github == "" {
			return nil
		}
		bundle := s.r.gh.fetch(ctx, github)
		if bundle == nil {
			return nil
		}
		bundle.Source, bundle.Identifier = "skills.sh", skillsShWrap(canonical)
		if bundle.Name == "skills" {
			trimmed := strings.TrimRight(canonical, "/")
			bundle.Name = trimmed[strings.LastIndex(trimmed, "/")+1:]
		}
		if bundle.Metadata == nil {
			bundle.Metadata = map[string]any{}
		}
		maps.Copy(bundle.Metadata, skillsShMetadata(canonical, d))
		return bundle
	}
	for _, candidate := range skillsShCandidates(canonical) {
		if bundle := relabel(candidate); bundle != nil {
			return bundle
		}
	}
	return relabel(s.discover(ctx, canonical, d))
}

// What GitHub says of the skill, with the summary and installs of its skills.sh page
func (s *skillsSh) Inspect(ctx context.Context, identifier string) *Meta {
	canonical := skillsShCanonical(identifier)
	d := s.detail(ctx, canonical)
	var m *Meta
	for _, candidate := range skillsShCandidates(canonical) {
		if m = s.r.gh.inspect(ctx, candidate); m != nil {
			break
		}
	}
	if m == nil {
		if resolved := s.discover(ctx, canonical, d); resolved != "" {
			m = s.r.gh.inspect(ctx, resolved)
		}
	}
	if m == nil {
		return nil
	}
	m.Source, m.Identifier, m.Trust = "skills.sh", skillsShWrap(canonical), githubTrust(canonical)
	extra := maps.Clone(m.Extra)
	if extra == nil {
		extra = map[string]any{}
	}
	maps.Copy(extra, skillsShMetadata(canonical, d))
	m.Extra = extra
	if d != nil {
		switch {
		case d.BodySummary != "":
			m.Description = d.BodySummary
		case m.Description != "" && d.WeeklyInstalls != "":
			m.Description = fmt.Sprintf("%s · %s weekly installs on skills.sh", m.Description, d.WeeklyInstalls)
		}
	}
	return m
}

func (s *skillsSh) Revision(context.Context, string) string { return "" }

// Pages of the skill and what its skills.sh page says worth keeping with an install
func skillsShMetadata(canonical string, d *detail) map[string]any {
	out := map[string]any{"detail_url": skillsShBase + "/" + canonical}
	if parts := strings.SplitN(canonical, "/", 3); len(parts) >= 2 {
		out["repo_url"] = "https://github.com/" + parts[0] + "/" + parts[1]
	}
	if d == nil {
		return out
	}
	for key, value := range map[string]string{"weekly_installs": d.WeeklyInstalls, "install_command": d.InstallCommand, "repo_url": d.RepoURL,
		"detail_url": d.DetailURL} {
		if value != "" {
			out[key] = value
		}
	}
	if len(d.SecurityAudits) > 0 {
		audits := map[string]any{}
		for k, v := range d.SecurityAudits {
			audits[k] = v
		}
		out["security_audits"] = audits
	}
	return out
}

// Identifier of the skill in its repository: under a usual skills folder, anywhere in its tree, at its root, or in a folder of its own
// at the top; empty when it is nowhere
func (s *skillsSh) discover(ctx context.Context, canonical string, d *detail) string {
	repo, path, ok := splitRepo(canonical)
	if !ok {
		return ""
	}
	if d != nil && d.Repo != "" {
		repo = d.Repo
	}
	token := path[strings.LastIndex(path, "/")+1:]
	tokens := []string{token}
	if d != nil {
		tokens = append(tokens, d.InstallSkill, d.PageTitle, d.BodyTitle)
	}
	in := func(base string) string {
		for _, m := range s.r.gh.list(ctx, repo, base, "") {
			if sameSkill(m, tokens) {
				return m.Identifier
			}
		}
		return ""
	}
	for _, base := range skillsShBases {
		if found := in(base); found != "" {
			return found
		}
	}
	if found := s.r.gh.findSkill(ctx, repo, token); found != "" {
		return found
	}
	if found := s.r.gh.rootSkill(ctx, repo); found != "" {
		return found
	}
	var entries []struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if !s.r.gh.json(ctx, githubAPI+"/"+repo+"/contents/", nil, 15*time.Second, &entries) {
		return ""
	}
	for _, entry := range entries {
		name := entry.Name
		if entry.Type != "dir" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "skills" {
			continue
		}
		if m := s.r.gh.inspect(ctx, repo+"/"+name+"/"+token); m != nil {
			return m.Identifier
		}
		if found := in(name + "/"); found != "" {
			return found
		}
	}
	return ""
}

// Whether any spelling of a token is a spelling of the skill's name, path or identifier's tail
func sameSkill(m Meta, tokens []string) bool {
	candidates := map[string]bool{}
	tail := ""
	if m.Identifier != "" {
		parts := strings.SplitN(m.Identifier, "/", 3)
		tail = parts[len(parts)-1]
	}
	for _, value := range []string{m.Name, m.Path, tail} {
		maps.Copy(candidates, spellings(value))
	}
	for _, token := range tokens {
		for spelling := range spellings(token) {
			if candidates[spelling] {
				return true
			}
		}
	}
	return false
}

// Lower case spellings of a skill's token: as it is, with hyphens for underscores and slashes, its last part, without punctuation
func spellings(value string) map[string]bool {
	out := map[string]bool{}
	plain := strings.ToLower(strings.Trim(strings.TrimSpace(stripHTML(value)), "/"))
	if plain == "" {
		return out
	}
	base := plain[strings.LastIndex(plain, "/")+1:]
	sanitized := strings.Trim(tokenSeparators.ReplaceAllString(plain, "-"), "-")
	tail := strings.TrimLeft(base, "@")
	for _, v := range []string{plain, strings.ReplaceAll(plain, "_", "-"), strings.ReplaceAll(plain, "/", "-"), base, strings.ReplaceAll(base, "_", "-"),
		sanitized, strings.ReplaceAll(sanitized, "/", "-"), sanitized[strings.LastIndex(sanitized, "/")+1:], tail, strings.ReplaceAll(tail, "_", "-")} {
		if v != "" {
			out[v] = true
		}
	}
	return out
}
