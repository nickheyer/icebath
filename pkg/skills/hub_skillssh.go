package skills

import (
	"context"
	"crypto/md5"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var htmlTagRe = regexp.MustCompile(`<[^>]+>`)

// Text with every HTML tag removed
func stripHTML(value string) string { return htmlTagRe.ReplaceAllString(value, "") }

const (
	skillsShBaseURL       = "https://skills.sh"
	skillsShSearchURL     = skillsShBaseURL + "/api/search"
	skillsShSitemapURL    = "https://www.skills.sh/sitemap.xml"
	skillsShSitemapPrefix = "sitemap-skills"
)

var (
	skillsShLocRe         = pyMust(`(?i)<loc>([^<]+)</loc>`)
	skillsShSitemapSkill  = pyMustNamed(`(?i)^https?://(?:www\.)?skills\.sh/(?P<owner>[^/]+)/(?P<repo>[^/]+)/(?P<skill>[^/]+)/?$`)
	skillsShSkillLinkRe   = pyMustNamed(`href=["']/(?P<id>(?!agents/|_next/|api/)[^"'/]+/[^"'/]+/[^"'/]+)["']`)
	skillsShInstallCmdRe  = pyMustNamed(`(?i)npx\s+skills\s+add\s+(?P<repo>https?://github\.com/[^\s<]+|[^\s<]+)(?:\s+--skill\s+(?P<skill>[^\s<]+))?`)
	skillsShPageH1Re      = pyMust(`(?is)<h1[^>]*>(?P<title>.*?)</h1>`)
	skillsShProseH1Re     = pyMust(`(?is)<div[^>]*class=["'][^"']*prose[^"']*["'][^>]*>.*?<h1[^>]*>(?P<title>.*?)</h1>`)
	skillsShProsePRe      = pyMust(`(?is)<div[^>]*class=["'][^"']*prose[^"']*["'][^>]*>.*?<p[^>]*>(?P<body>.*?)</p>`)
	skillsShWeeklyRe      = pyMustNamed(`(?s)Weekly Installs.*?children\\":\\"(?P<count>[0-9.,Kk]+)\\"`)
	skillsShAuditVerdict  = pyMust(`(?i)(Pass|Warn|Fail)`)
	skillsShSanitizeToken = regexp.MustCompile(`[^a-z0-9/_-]+`)
	skillsShIDPrefixes    = []string{"skills-sh/", "skills.sh/", "skils-sh/", "skils.sh/"}
	skillsShBasePaths     = []string{"skills/", ".agents/skills/", ".claude/skills/"}
)

// Overrides of the skills.sh internals the hermes tests patch
type skillsShSeams struct {
	fetchDetailPage      func(identifier string) *pyjson.Object
	candidateIdentifiers func(identifier string) []string
	discoverIdentifier   func(identifier string, detail *pyjson.Object) (string, bool)
	sleep                func(d time.Duration)
}

// Skills discovered through skills.sh and fetched from their GitHub repos
type SkillsShSource struct {
	SourceBase
	h      *Hub
	Auth   *GitHubAuth
	GitHub *GitHubSource
	seams  skillsShSeams
}

// skills.sh source over its own GitHub source sharing the credentials
func NewSkillsShSource(h *Hub, auth *GitHubAuth) *SkillsShSource {
	if auth == nil {
		auth = NewGitHubAuth(h.S)
	}
	return &SkillsShSource{SourceBase: SourceBase{ID: "skills-sh"}, h: h, Auth: auth, GitHub: NewGitHubSource(h, auth, nil)}
}

// Trust of the underlying GitHub identifier
func (s *SkillsShSource) TrustLevelFor(identifier string) string {
	return s.GitHub.TrustLevelFor(skillsShNormalizeIdentifier(identifier))
}

func (s *SkillsShSource) meta(canonical, name, description, path string, extra *pyjson.Object) SkillMeta {
	parts := strings.SplitN(canonical, "/", 3)
	if extra == nil {
		extra = pyjson.NewObject()
	}
	return SkillMeta{Name: name, Description: description, Source: "skills.sh", Identifier: skillsShWrapIdentifier(canonical),
		TrustLevel: s.GitHub.TrustLevelFor(canonical), Repo: strPtr(strings.Join(parts[:min(2, len(parts))], "/")), Path: strPtr(path), Extra: extra}
}

func skillsShURLsFor(canonical, repo string) *pyjson.Object {
	return objectOf("detail_url", skillsShBaseURL+"/"+canonical, "repo_url", "https://github.com/"+repo)
}

func (s *SkillsShSource) sleep(seconds float64) {
	d := time.Duration(seconds * float64(time.Second))
	if s.seams.sleep != nil {
		s.seams.sleep(d)
		return
	}
	time.Sleep(d)
}

// Search API matches, the full sitemap catalog for an empty query
func (s *SkillsShSource) Search(ctx context.Context, query string, limit int) (out []SkillMeta, err error) {
	defer hubRecover(&err)
	if pystr.Strip(query) == "" {
		return s.sitemapCatalog(ctx, limit), nil
	}
	key := fmt.Sprintf("skills_sh_search_%x", md5.Sum([]byte(fmt.Sprintf("%s|%d", query, limit))))
	cached, ok, err := s.h.cachedMetas(key)
	if err != nil {
		return nil, err
	}
	if ok {
		return pyHeadMetas(cached, limit), nil
	}
	data, err := s.h.getJSON(ctx, skillsShSearchURL, hubGet{Params: [][2]string{{"q", query}, {"limit", fmt.Sprint(limit)}}})
	if err != nil {
		return nil, err
	}
	if data == nil {
		return []SkillMeta{}, nil
	}
	var items any = []any{}
	if o, isObj := data.(*pyjson.Object); isObj {
		items = pyGet(o, "skills", []any{})
	}
	list, isList := items.([]any)
	if !isList {
		return []SkillMeta{}, nil
	}
	results := []SkillMeta{}
	for _, item := range pyHeadAny(list, limit) {
		if m := s.metaFromSearchItem(item); m != nil {
			results = append(results, *m)
		}
	}
	if err := s.h.cacheMetas(key, results); err != nil {
		return nil, err
	}
	return results, nil
}

// First limit items with Python slice semantics for a negative limit
func pyHeadAny(list []any, limit int) []any {
	if limit < 0 {
		limit = max(len(list)+limit, 0)
	}
	return list[:min(limit, len(list))]
}

// GitHub bundle of the first resolvable candidate relabelled as a skills.sh install
func (s *SkillsShSource) Fetch(ctx context.Context, identifier string) (b *SkillBundle, err error) {
	defer hubRecover(&err)
	canonical := skillsShNormalizeIdentifier(identifier)
	detail := s.fetchDetailPage(ctx, canonical)
	relabel := func(githubID string, ok bool) *SkillBundle {
		if !ok || githubID == "" {
			return nil
		}
		bundle := s.GitHub.fetch(ctx, githubID)
		if bundle == nil {
			return nil
		}
		bundle.Source, bundle.Identifier = "skills.sh", skillsShWrapIdentifier(canonical)
		if bundle.Name == "skills" {
			trimmed := strings.TrimRight(canonical, "/")
			bundle.Name = trimmed[strings.LastIndex(trimmed, "/")+1:]
		}
		if bundle.Metadata == nil {
			bundle.Metadata = pyjson.NewObject()
		}
		md := s.detailToMetadata(canonical, detail)
		for _, k := range md.Keys {
			v, _ := md.Get(k)
			bundle.Metadata.Set(k, v)
		}
		return bundle
	}
	for _, cand := range s.candidateIdentifiers(canonical) {
		if bundle := relabel(cand, true); bundle != nil {
			return bundle, nil
		}
	}
	return relabel(s.discoverIdentifier(ctx, canonical, detail)), nil
}

// GitHub metadata of the resolved skill finalized with the skills.sh detail page
func (s *SkillsShSource) Inspect(ctx context.Context, identifier string) (m *SkillMeta, err error) {
	defer hubRecover(&err)
	canonical := skillsShNormalizeIdentifier(identifier)
	detail := s.fetchDetailPage(ctx, canonical)
	meta := s.resolveGitHubMeta(ctx, canonical, detail)
	if meta == nil {
		return nil, nil
	}
	return s.finalizeInspectMeta(meta, canonical, detail), nil
}

// Guarded sitemap XML body on 200
func (s *SkillsShSource) sitemapXML(ctx context.Context, url string, timeout time.Duration) (string, bool) {
	resp := s.h.guardedHTTPGet(ctx, url, timeout, map[string]string{"Accept-Encoding": "gzip"})
	if resp == nil || resp.StatusCode != 200 {
		return "", false
	}
	return resp.Text(), true
}

// Whole catalog from the sitemap shards, cached only when every shard answered
func (s *SkillsShSource) sitemapCatalog(ctx context.Context, limit int) []SkillMeta {
	const key = "skills_sh_sitemap_v1"
	headOrAll := func(r []SkillMeta) []SkillMeta {
		if limit > 0 {
			return pyHeadMetas(r, limit)
		}
		return r
	}
	cached, ok, err := s.h.cachedMetas(key)
	if err != nil {
		hubRaise(err)
	}
	if ok {
		return headOrAll(cached)
	}
	indexXML, _ := s.sitemapXML(ctx, skillsShSitemapURL, 20*time.Second)
	var shards []string
	for _, loc := range reGroups(skillsShLocRe, indexXML) {
		if strings.Contains(loc, skillsShSitemapPrefix) {
			shards = append(shards, pystr.Strip(loc))
		}
	}
	if len(shards) == 0 {
		return s.featuredSkills(ctx, limit)
	}
	seen := map[string]bool{}
	results := []SkillMeta{}
	partial := false
	for _, shard := range shards {
		var xml string
		var got bool
		for attempt := 1; attempt <= CatalogPageRetries; attempt++ {
			xml, got = s.sitemapXML(ctx, shard, 30*time.Second)
			if got || attempt == CatalogPageRetries {
				break
			}
			s.sleep(float64(min(1<<attempt, 8)))
		}
		if !got {
			partial = true
			continue
		}
		for _, loc := range reGroups(skillsShLocRe, xml) {
			m, err := skillsShSitemapSkill.FindStringMatch(pystr.Strip(loc))
			if err != nil || m == nil {
				continue
			}
			repo := skillsShSitemapSkill.group(m, "owner") + "/" + skillsShSitemapSkill.group(m, "repo")
			skill := skillsShSitemapSkill.group(m, "skill")
			canonical := repo + "/" + skill
			if !seen[canonical] {
				seen[canonical] = true
				results = append(results, s.meta(canonical, skill, "Indexed by skills.sh from "+repo, skill, skillsShURLsFor(canonical, repo)))
			}
		}
	}
	if len(results) == 0 {
		return s.featuredSkills(ctx, limit)
	}
	if !partial {
		if err := s.h.cacheMetas(key, results); err != nil {
			hubRaise(err)
		}
	}
	return headOrAll(results)
}

// Featured skill links of the skills.sh homepage up to limit
func (s *SkillsShSource) featuredSkills(ctx context.Context, limit int) []SkillMeta {
	const key = "skills_sh_featured"
	cached, ok, err := s.h.cachedMetas(key)
	if err != nil {
		hubRaise(err)
	}
	if ok {
		return pyHeadMetas(cached, limit)
	}
	html, ok, err := s.h.getText(ctx, skillsShBaseURL, hubGet{})
	if err != nil {
		hubRaise(err)
	}
	if !ok {
		return []SkillMeta{}
	}
	seen := map[string]bool{}
	results := []SkillMeta{}
	m, merr := skillsShSkillLinkRe.FindStringMatch(html)
	for merr == nil && m != nil {
		canonical := skillsShSkillLinkRe.group(m, "id")
		repo, skillPath, split := "", "", false
		if !seen[canonical] {
			repo, skillPath, split = splitRepoID(canonical)
		}
		seen[canonical] = true
		if split {
			results = append(results, s.meta(canonical, skillPath[strings.LastIndex(skillPath, "/")+1:], "Featured on skills.sh from "+repo, skillPath, nil))
			if len(results) >= limit {
				break
			}
		}
		m, merr = skillsShSkillLinkRe.FindNextMatch(m)
	}
	if err := s.h.cacheMetas(key, results); err != nil {
		hubRaise(err)
	}
	return results
}

// Python int of a JSON value as isinstance int sees it, bools included
func jsonInt(v any) (*big.Int, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return big.NewInt(1), true
		}
		return big.NewInt(0), true
	case int:
		return big.NewInt(int64(x)), true
	case int64:
		return big.NewInt(x), true
	case *big.Int:
		return x, true
	}
	return nil, false
}

// Decimal digits grouped by commas as the comma format spec renders them
func groupBigThousands(n *big.Int) string {
	s := n.String()
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return sign + b.String()
}

// Search API item as a result, nil without a usable owner, repo and skill path
func (s *SkillsShSource) metaFromSearchItem(item any) *SkillMeta {
	o, ok := item.(*pyjson.Object)
	if !ok {
		return nil
	}
	idV, _ := o.Get("id")
	srcV, _ := o.Get("source")
	skillV, _ := o.Get("skillId")
	canonical, isStr := idV.(string)
	if !isStr || strings.Count(canonical, "/") < 2 {
		repo, ok1 := srcV.(string)
		sp, ok2 := skillV.(string)
		if !ok1 || !ok2 {
			return nil
		}
		canonical = repo + "/" + sp
	}
	repo, skillPath, ok := splitRepoID(canonical)
	if !ok {
		return nil
	}
	installs, _ := o.Get("installs")
	label := ""
	if n, isInt := jsonInt(installs); isInt {
		label = " · " + groupBigThousands(n) + " installs"
	}
	name := skillPath[strings.LastIndex(skillPath, "/")+1:]
	if v, _ := o.Get("name"); pyrepr.Truthy(v) {
		name = pyrepr.Str(v)
	}
	extra := objectOf("installs", installs)
	urls := skillsShURLsFor(canonical, repo)
	for _, k := range urls.Keys {
		v, _ := urls.Get(k)
		extra.Set(k, v)
	}
	m := s.meta(canonical, name, "Indexed by skills.sh from "+repo+label, skillPath, extra)
	return &m
}

// Parsed skills.sh detail page through the index cache, nil when unreachable or unparsable
func (s *SkillsShSource) fetchDetailPage(ctx context.Context, identifier string) *pyjson.Object {
	if s.seams.fetchDetailPage != nil {
		return s.seams.fetchDetailPage(identifier)
	}
	key := fmt.Sprintf("skills_sh_detail_%x", md5.Sum([]byte(identifier)))
	v, err := s.h.memoJSON(key, func() (any, error) {
		html, ok, err := s.h.getText(ctx, skillsShBaseURL+"/"+identifier, hubGet{})
		if err != nil || !ok {
			return nil, err
		}
		if d := s.parseDetailPage(identifier, html); d != nil {
			return d, nil
		}
		return nil, nil
	}, func(c any) bool { _, ok := c.(*pyjson.Object); return ok })
	if err != nil {
		hubRaise(err)
	}
	o, _ := v.(*pyjson.Object)
	return o
}

// Repo, install skill, titles, summary, installs, command, urls and audits of a detail page
func (s *SkillsShSource) parseDetailPage(identifier, html string) *pyjson.Object {
	repo, installSkill, ok := splitRepoID(identifier)
	if !ok {
		return nil
	}
	var installCommand any
	if m, err := skillsShInstallCmdRe.FindStringMatch(html); err == nil && m != nil {
		installCommand = pystr.Strip(m.String())
		if g := skillsShInstallCmdRe.group(m, "skill"); g != "" {
			installSkill = g
		}
		installSkill = pystr.Strip(installSkill)
		if slug, ok := skillsShExtractRepoSlug(pystr.Strip(skillsShInstallCmdRe.group(m, "repo"))); ok {
			repo = slug
		}
	}
	firstOrNone := func(re *regexp2.Regexp) any {
		if v, ok := skillsShExtractFirstMatch(re, html); ok {
			return v
		}
		return nil
	}
	var weekly any
	if w, ok := skillsShExtractWeeklyInstalls(html); ok {
		weekly = w
	}
	out := objectOf("repo", repo, "install_skill", installSkill, "page_title", firstOrNone(skillsShPageH1Re),
		"body_title", firstOrNone(skillsShProseH1Re), "body_summary", firstOrNone(skillsShProsePRe),
		"weekly_installs", weekly, "install_command", installCommand)
	urls := skillsShURLsFor(identifier, repo)
	for _, k := range urls.Keys {
		v, _ := urls.Get(k)
		out.Set(k, v)
	}
	out.Set("security_audits", skillsShExtractSecurityAudits(html))
	return out
}

// Owner and repo of an install command repo value
func skillsShExtractRepoSlug(repoValue string) (string, bool) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(pystr.Strip(repoValue), "https://github.com/"), "/"), "/")
	if len(parts) >= 2 {
		return parts[0] + "/" + parts[1], true
	}
	return "", false
}

// First non empty group of the first match with tags stripped, false when empty
func skillsShExtractFirstMatch(re *regexp2.Regexp, text string) (string, bool) {
	m, err := re.FindStringMatch(text)
	if err != nil || m == nil {
		return "", false
	}
	for _, g := range m.Groups()[1:] {
		if g.String() != "" {
			v := pystr.Strip(stripHTML(g.String()))
			return v, v != ""
		}
	}
	return "", false
}

func skillsShExtractWeeklyInstalls(html string) (string, bool) {
	m, err := skillsShWeeklyRe.FindStringMatch(html)
	if err != nil || m == nil {
		return "", false
	}
	return skillsShWeeklyRe.group(m, "count"), true
}

// Pass, Warn or Fail verdict found within 500 characters after each audit provider link
func skillsShExtractSecurityAudits(html string) *pyjson.Object {
	audits := pyjson.NewObject()
	for _, audit := range []string{"agent-trust-hub", "socket", "snyk"} {
		idx := strings.Index(html, "/security/"+audit)
		if idx < 0 {
			continue
		}
		window := []rune(html[idx:])
		window = window[:min(500, len(window))]
		m, err := skillsShAuditVerdict.FindStringMatch(string(window))
		if err != nil || m == nil {
			continue
		}
		v := m.GroupByNumber(1).String()
		audits.Set(audit, strings.ToUpper(v[:1])+strings.ToLower(v[1:]))
	}
	return audits
}

// Identifier found under a standard base path, the repo tree, a root skill, or a top level dir
func (s *SkillsShSource) discoverIdentifier(ctx context.Context, identifier string, detail *pyjson.Object) (string, bool) {
	if s.seams.discoverIdentifier != nil {
		return s.seams.discoverIdentifier(identifier, detail)
	}
	defaultRepo, skillPath, ok := splitRepoID(identifier)
	if !ok {
		return "", false
	}
	repo := defaultRepo
	if detail != nil {
		repo = pyrepr.Str(pyGet(detail, "repo", defaultRepo))
	}
	token := skillPath[strings.LastIndex(skillPath, "/")+1:]
	tokens := []any{token}
	if detail != nil {
		for _, k := range []string{"install_skill", "page_title", "body_title"} {
			tokens = append(tokens, pyGet(detail, k, ""))
		}
	}
	matchIn := func(basePath string) (found string) {
		var err error
		defer hubRecover(&err)
		for _, m := range s.GitHub.listSkillsInRepo(ctx, repo, basePath, "") {
			if skillsShMatchesSkillTokens(m, tokens) {
				return m.Identifier
			}
		}
		return ""
	}
	for _, base := range skillsShBasePaths {
		if f := matchIn(base); f != "" {
			return f, true
		}
	}
	if f, ok := s.GitHub.findSkillInRepoTree(ctx, repo, token); ok && f != "" {
		return f, true
	}
	if f, ok := s.GitHub.findRepoRootSkill(ctx, repo); ok && f != "" {
		return f, true
	}
	var found string
	var err error
	func() {
		defer hubRecover(&err)
		entries, gerr := s.h.getJSON(ctx, "https://api.github.com/repos/"+repo+"/contents/",
			hubGet{Headers: s.GitHub.Auth.GetHeaders(), Timeout: 15 * time.Second, FollowRedirects: true})
		if gerr != nil {
			hubRaise(gerr)
		}
		list, _ := entries.([]any)
		for _, entry := range list {
			if pyGet(entry, "type", nil) != "dir" {
				continue
			}
			nameV, has := entry.(*pyjson.Object).Get("name")
			if !has {
				hubRaise(fmt.Errorf("KeyError: 'name'"))
			}
			dirName := pyStrOf(nameV)
			if strings.HasPrefix(dirName, ".") || strings.HasPrefix(dirName, "_") || dirName == "skills" || dirName == ".agents" || dirName == ".claude" {
				continue
			}
			if meta := s.GitHub.inspect(ctx, repo+"/"+dirName+"/"+token); meta != nil {
				found = meta.Identifier
				return
			}
			if f := matchIn(dirName + "/"); f != "" {
				found = f
				return
			}
		}
	}()
	return found, found != ""
}

// GitHub metadata of the first candidate that inspects, else of the discovered identifier
func (s *SkillsShSource) resolveGitHubMeta(ctx context.Context, identifier string, detail *pyjson.Object) *SkillMeta {
	for _, cand := range s.candidateIdentifiers(identifier) {
		if meta := s.GitHub.inspect(ctx, cand); meta != nil {
			return meta
		}
	}
	if resolved, ok := s.discoverIdentifier(ctx, identifier, detail); ok && resolved != "" {
		return s.GitHub.inspect(ctx, resolved)
	}
	return nil
}

// GitHub metadata relabelled as skills.sh with the detail page summary and installs
func (s *SkillsShSource) finalizeInspectMeta(meta *SkillMeta, canonical string, detail *pyjson.Object) *SkillMeta {
	meta.Source, meta.Identifier = "skills.sh", skillsShWrapIdentifier(canonical)
	meta.TrustLevel = s.TrustLevelFor(canonical)
	merged := pyjson.NewObject()
	for _, src := range []*pyjson.Object{meta.Extra, s.detailToMetadata(canonical, detail)} {
		if src == nil {
			continue
		}
		for _, k := range src.Keys {
			v, _ := src.Get(k)
			merged.Set(k, v)
		}
	}
	meta.Extra = merged
	if detail != nil {
		summary, weekly := pyGet(detail, "body_summary", nil), pyGet(detail, "weekly_installs", nil)
		if pyrepr.Truthy(summary) {
			meta.Description = pyrepr.Str(summary)
		} else if meta.Description != "" && pyrepr.Truthy(weekly) {
			meta.Description = fmt.Sprintf("%s · %s weekly installs on skills.sh", meta.Description, pyrepr.Str(weekly))
		}
	}
	return meta
}

// Whether any token variant meets a variant of the result name, path or identifier tail
func skillsShMatchesSkillTokens(meta SkillMeta, tokens []any) bool {
	candidates := map[string]bool{}
	var path, tail any
	if meta.Path != nil {
		path = *meta.Path
	}
	if meta.Identifier != "" {
		parts := strings.SplitN(meta.Identifier, "/", 3)
		tail = parts[len(parts)-1]
	}
	for _, v := range []any{meta.Name, path, tail} {
		for k := range skillsShTokenVariants(v) {
			candidates[k] = true
		}
	}
	for _, t := range tokens {
		for k := range skillsShTokenVariants(t) {
			if candidates[k] {
				return true
			}
		}
	}
	return false
}

// Lowercased spelling variants of a skill token, empty for a falsy value
func skillsShTokenVariants(value any) map[string]bool {
	out := map[string]bool{}
	if !pyrepr.Truthy(value) {
		return out
	}
	plain := pystr.Lower(strings.Trim(pystr.Strip(stripHTML(pyrepr.Str(value))), "/"))
	if plain == "" {
		return out
	}
	base := plain[strings.LastIndex(plain, "/")+1:]
	sanitized := strings.Trim(skillsShSanitizeToken.ReplaceAllString(plain, "-"), "-")
	tail := strings.TrimLeft(base, "@")
	for _, v := range []string{plain, strings.ReplaceAll(plain, "_", "-"), strings.ReplaceAll(plain, "/", "-"), base,
		strings.ReplaceAll(base, "_", "-"), sanitized, strings.ReplaceAll(sanitized, "/", "-"),
		sanitized[strings.LastIndex(sanitized, "/")+1:], tail, strings.ReplaceAll(tail, "_", "-")} {
		if v != "" {
			out[v] = true
		}
	}
	return out
}

// Detail and repo urls plus every truthy detail field worth keeping in bundle metadata
func (s *SkillsShSource) detailToMetadata(canonical string, detail *pyjson.Object) *pyjson.Object {
	parts := strings.SplitN(canonical, "/", 3)
	md := objectOf("detail_url", skillsShBaseURL+"/"+canonical)
	if len(parts) >= 2 {
		md.Set("repo_url", "https://github.com/"+parts[0]+"/"+parts[1])
	}
	if detail != nil {
		for _, k := range []string{"weekly_installs", "install_command", "repo_url", "detail_url", "security_audits"} {
			if v, _ := detail.Get(k); pyrepr.Truthy(v) {
				md.Set(k, v)
			}
		}
	}
	return md
}

// Identifier with a skills.sh alias prefix removed
func skillsShNormalizeIdentifier(identifier string) string {
	for _, p := range skillsShIDPrefixes {
		if strings.HasPrefix(identifier, p) {
			return identifier[len(p):]
		}
	}
	return identifier
}

// The identifier and its standard base path variants, deduplicated in order
func (s *SkillsShSource) candidateIdentifiers(identifier string) []string {
	if s.seams.candidateIdentifiers != nil {
		return s.seams.candidateIdentifiers(identifier)
	}
	repo, path, ok := splitRepoID(identifier)
	if !ok {
		return []string{identifier}
	}
	path = strings.TrimLeft(path, "/")
	out := []string{repo + "/" + path}
	for _, b := range skillsShBasePaths {
		if c := repo + "/" + b + path; c != out[0] {
			out = append(out, c)
		}
	}
	return out
}

func skillsShWrapIdentifier(identifier string) string { return "skills-sh/" + identifier }
