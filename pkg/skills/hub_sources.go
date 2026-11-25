package skills

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
)

// Python exception raised while walking registry JSON, carried by panic to the adapter method boundary
type hubDataError struct{ err error }

func hubRaise(err error) { panic(&hubDataError{err}) }

// Turns a registry data exception raised below into the returned error of an adapter method
func hubRecover(err *error) {
	if r := recover(); r != nil {
		he, ok := r.(*hubDataError)
		if !ok {
			panic(r)
		}
		*err = he.err
	}
}

// dict.get on registry JSON, the AttributeError when the value is not a mapping
func lookup(v any, key string, def any) any {
	o, ok := v.(*jsonutil.Object)
	if !ok {
		hubRaise(fmt.Errorf("expected a mapping holding %s, got %s", key, errchain.Kind(v)))
	}
	if x, found := o.Get(key); found {
		return x
	}
	return def
}

// Registry value used as str, the TypeError for anything else
func hubText(v any) string {
	s, ok := v.(string)
	if !ok {
		hubRaise(fmt.Errorf("expected text, got %s", errchain.Kind(v)))
	}
	return s
}

// First n code points of a registry str
func headValue(v any, n int) string {
	rs := []rune(hubText(v))
	if len(rs) > n {
		rs = rs[:n]
	}
	return string(rs)
}

// Registry tag list as strings, empty when the value is not a list
func tagStrings(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, len(list))
	for i, t := range list {
		out[i] = anyutil.String(t)
	}
	return out
}

func strPtr(s string) *string { return &s }

func objectOf(kv ...any) *jsonutil.Object {
	o := jsonutil.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

// Skills from a domain exposing /.well-known/skills/index.json
type WellKnownSkillSource struct {
	SourceBase
	h *Hub
}

const wellKnownBasePath = "/.well-known/skills"

// Well known endpoint adapter over the hub
func NewWellKnownSkillSource(h *Hub) *WellKnownSkillSource {
	return &WellKnownSkillSource{SourceBase: SourceBase{ID: "well-known"}, h: h}
}

type wellKnownID struct{ indexURL, baseURL, skillName, skillURL string }

func (w *WellKnownSkillSource) meta(idx *jsonutil.Object, skillName, description string, files any, extra ...any) SkillMeta {
	base := hubText(lookup(idx, "base_url", nil))
	ex := objectOf(append([]any{"index_url", lookup(idx, "index_url", nil), "base_url", base, "files", files}, extra...)...)
	return SkillMeta{Name: skillName, Description: description, Source: "well-known", Identifier: wellKnownWrap(base, skillName),
		TrustLevel: "community", Path: strPtr(skillName), Tags: []string{}, Extra: ex}
}

// Index entries named in an http index URL, at most limit
func (w *WellKnownSkillSource) Search(ctx context.Context, query string, limit int) (out []SkillMeta, err error) {
	defer hubRecover(&err)
	out = []SkillMeta{}
	indexURL, ok := wellKnownIndexURL(query)
	if !ok {
		return out, nil
	}
	idx := w.parseIndex(ctx, indexURL)
	if idx == nil {
		return out, nil
	}
	skills := lookup(idx, "skills", nil).([]any)
	end := min(limit, len(skills))
	if limit < 0 {
		end = max(len(skills)+limit, 0)
	}
	for _, entry := range skills[:end] {
		name, isStr := lookup(entry, "name", nil).(string)
		if !isStr || name == "" {
			continue
		}
		files := lookup(entry, "files", []any{"SKILL.md"})
		if _, isList := files.([]any); !isList {
			files = []any{"SKILL.md"}
		}
		out = append(out, w.meta(idx, name, anyutil.String(lookup(entry, "description", "")), files))
	}
	return out, nil
}

// Metadata of one advertised skill read from its SKILL.md frontmatter
func (w *WellKnownSkillSource) Inspect(ctx context.Context, identifier string) (m *SkillMeta, err error) {
	defer hubRecover(&err)
	p, err := wellKnownParseIdentifier(identifier)
	if err != nil || p == nil {
		return nil, err
	}
	entry := w.indexEntry(ctx, p.indexURL, p.skillName)
	if entry == nil {
		return nil, nil
	}
	skillMD, ok := w.h.fetchText(ctx, p.skillURL+"/SKILL.md")
	if !ok {
		return nil, nil
	}
	fm := hubParseFrontmatter(skillMD)
	name := p.skillName
	if anyutil.IsTrue(fm["name"]) {
		name = anyutil.String(fm["name"])
	}
	desc := fm["description"]
	if !anyutil.IsTrue(desc) {
		desc = lookup(entry, "description", nil)
	}
	description := ""
	if anyutil.IsTrue(desc) {
		description = anyutil.String(desc)
	}
	idx := objectOf("index_url", p.indexURL, "base_url", p.baseURL)
	meta := w.meta(idx, name, description, lookup(entry, "files", []any{"SKILL.md"}), "endpoint", p.skillURL)
	return &meta, nil
}

// Every advertised file of one skill, nil when any is unsafe or missing
func (w *WellKnownSkillSource) Fetch(ctx context.Context, identifier string) (b *SkillBundle, err error) {
	defer hubRecover(&err)
	p, err := wellKnownParseIdentifier(identifier)
	if err != nil || p == nil {
		return nil, err
	}
	skillName, verr := validateHubSkillName(p.skillName)
	if verr != nil {
		w.h.S.logger().Warn(fmt.Sprintf("Well-known skill identifier contained unsafe skill name: %s", identifier))
		return nil, nil
	}
	entry := w.indexEntry(ctx, p.indexURL, p.skillName)
	if entry == nil || !anyutil.IsTrue(entry) {
		return nil, nil
	}
	files := lookup(entry, "files", []any{"SKILL.md"})
	if list, isList := files.([]any); !isList || len(list) == 0 {
		files = []any{"SKILL.md"}
	}
	var downloaded []BundleFile
	put := func(path string, data []byte) {
		for i := range downloaded {
			if downloaded[i].Path == path {
				downloaded[i].Data = data
				return
			}
		}
		downloaded = append(downloaded, BundleFile{Path: path, Data: data})
	}
	for _, rp := range files.([]any) {
		rel, isStr := rp.(string)
		if !isStr || rel == "" {
			continue
		}
		safe, verr := validateBundleRelPath(rel)
		if verr != nil {
			w.h.S.logger().Warn(fmt.Sprintf("Well-known skill %s advertised unsafe file path: %s", identifier, errchain.Show(rel)))
			return nil, nil
		}
		text, ok := w.h.fetchText(ctx, p.skillURL+"/"+safe)
		if !ok {
			return nil, nil
		}
		put(safe, []byte(text))
	}
	bundle := &SkillBundle{Name: skillName, Files: downloaded, Source: "well-known", Identifier: wellKnownWrap(p.baseURL, skillName),
		TrustLevel: "community", Metadata: objectOf("index_url", p.indexURL, "base_url", p.baseURL, "endpoint", p.skillURL, "files", files)}
	if _, ok := bundle.File("SKILL.md"); !ok {
		return nil, nil
	}
	return bundle, nil
}

// Index URL an http query names, ok false for anything else
func wellKnownIndexURL(query string) (string, bool) {
	query = strings.TrimSpace(query)
	if !strings.HasPrefix(query, "http://") && !strings.HasPrefix(query, "https://") {
		return "", false
	}
	if strings.HasSuffix(query, "/index.json") {
		return query, true
	}
	if before, _, found := strings.Cut(query, wellKnownBasePath+"/"); found {
		return before + wellKnownBasePath + "/index.json", true
	}
	return strings.TrimRight(query, "/") + wellKnownBasePath + "/index.json", true
}

// Index, base, name and skill URL of an identifier, nil when it is not a well known URL
func wellKnownParseIdentifier(identifier string) (*wellKnownID, error) {
	raw := strings.TrimPrefix(identifier, "well-known:")
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	frag := u.Fragment
	u.Fragment, u.RawFragment = "", ""
	clean := u.String()
	var p wellKnownID
	if strings.HasSuffix(clean, "/index.json") {
		if frag == "" {
			return nil, nil
		}
		p.baseURL, p.skillName = strings.TrimSuffix(clean, "/index.json"), frag
		p.skillURL = p.baseURL + "/" + p.skillName
	} else {
		if strings.HasSuffix(clean, "/SKILL.md") {
			p.skillURL = strings.TrimSuffix(clean, "/SKILL.md")
		} else {
			p.skillURL = strings.TrimRight(clean, "/")
		}
		if !strings.Contains(p.skillURL, wellKnownBasePath+"/") {
			return nil, nil
		}
		i := strings.LastIndex(p.skillURL, "/")
		p.baseURL, p.skillName = p.skillURL[:i], p.skillURL[i+1:]
	}
	p.indexURL = p.baseURL + "/index.json"
	return &p, nil
}

// Index document of a well known endpoint through the shared cache, nil when unavailable
func (w *WellKnownSkillSource) parseIndex(ctx context.Context, indexURL string) *jsonutil.Object {
	sum := md5.Sum([]byte(indexURL))
	v, err := w.h.memoJSON("well_known_index_"+hex.EncodeToString(sum[:]), func() (any, error) {
		resp := w.h.guardedHTTPGet(ctx, indexURL, defaultHubHTTPTimeout, nil)
		if resp == nil || resp.StatusCode != 200 {
			return nil, nil
		}
		data, err := resp.JSON()
		if err != nil {
			if _, isDecode := err.(*jsonutil.SyntaxError); isDecode {
				return nil, nil
			}
			return nil, err
		}
		var skills any = []any{}
		if o, isObj := data.(*jsonutil.Object); isObj {
			skills = lookup(o, "skills", []any{})
		}
		if _, isList := skills.([]any); !isList {
			return nil, nil
		}
		return objectOf("index_url", indexURL, "base_url", strings.TrimSuffix(indexURL, "/index.json"), "skills", skills), nil
	}, func(c any) bool {
		o, isObj := c.(*jsonutil.Object)
		if !isObj {
			return false
		}
		_, isList := lookup(o, "skills", nil).([]any)
		return isList
	})
	if err != nil {
		hubRaise(err)
	}
	o, _ := v.(*jsonutil.Object)
	return o
}

// Index entry naming the skill, nil when absent
func (w *WellKnownSkillSource) indexEntry(ctx context.Context, indexURL, skillName string) any {
	idx := w.parseIndex(ctx, indexURL)
	if idx == nil || !anyutil.IsTrue(idx) {
		return nil
	}
	skills, _ := lookup(idx, "skills", nil).([]any)
	for _, e := range skills {
		if o, isObj := e.(*jsonutil.Object); isObj && lookup(o, "name", nil) == skillName {
			return o
		}
	}
	return nil
}

func wellKnownWrap(baseURL, skillName string) string {
	return "well-known:" + strings.TrimRight(baseURL, "/") + "/" + skillName
}

// Direct SKILL.md URL with its explicitly referenced allowlisted support files
type UrlSource struct {
	SourceBase
	h *Hub
}

// Direct URL adapter over the hub
func NewUrlSource(h *Hub) *UrlSource { return &UrlSource{SourceBase: SourceBase{ID: "url"}, h: h} }

var urlSourceValidName = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// Search is meaningless for a direct URL
func (u *UrlSource) Search(context.Context, string, int) ([]SkillMeta, error) {
	return []SkillMeta{}, nil
}

// Claims bare http URLs ending in .md, leaving wrapped and well known identifiers to their adapters
func (u *UrlSource) matches(identifier string) bool {
	ident := strings.TrimSpace(identifier)
	lower := strings.ToLower(ident)
	if (!strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://")) || strings.Contains(ident, "/.well-known/skills/") ||
		strings.HasSuffix(strings.TrimRight(ident, "/"), "/index.json") {
		return false
	}
	parsed, err := url.Parse(ident)
	if err != nil {
		return false
	}
	return strings.HasSuffix(strings.ToLower(parsed.Path), ".md")
}

type urlLoaded struct {
	url, text string
	fm        map[string]any
	name      *string
}

func (u *UrlSource) load(ctx context.Context, identifier string) *urlLoaded {
	if !u.matches(identifier) {
		return nil
	}
	target := strings.TrimSpace(identifier)
	text, ok := u.h.fetchText(ctx, target)
	if !ok {
		return nil
	}
	fm := hubParseFrontmatter(text)
	return &urlLoaded{url: target, text: text, fm: fm, name: urlResolveSkillName(fm, target)}
}

// Metadata of a claimed URL, awaiting_name set when no usable name resolves
func (u *UrlSource) Inspect(ctx context.Context, identifier string) (m *SkillMeta, err error) {
	defer hubRecover(&err)
	l := u.load(ctx, identifier)
	if l == nil {
		return nil, nil
	}
	name := ""
	if l.name != nil {
		name = *l.name
	}
	desc := ""
	if anyutil.IsTrue(l.fm["description"]) {
		desc = anyutil.String(l.fm["description"])
	}
	return &SkillMeta{Name: name, Description: desc, Source: "url", Identifier: l.url, TrustLevel: "community", Path: strPtr(name),
		Tags: tagStrings(hubTags(l.fm)), Extra: objectOf("url", l.url, "awaiting_name", l.name == nil)}, nil
}

// SKILL.md plus same host referenced support files, a missing support file skipped with a warning
func (u *UrlSource) Fetch(ctx context.Context, identifier string) (b *SkillBundle, err error) {
	defer hubRecover(&err)
	l := u.load(ctx, identifier)
	if l == nil {
		return nil, nil
	}
	referenced, ok, err := referencedSupportPaths(l.text)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	files := []BundleFile{{Path: "SKILL.md", Data: []byte(l.text)}}
	origin, err := url.Parse(l.url)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(referenced))
	for p := range referenced {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, rel := range paths {
		su, err := origin.Parse(urlutil.Escape(rel, "/"))
		if err != nil {
			return nil, err
		}
		if su.Host != origin.Host {
			return nil, nil
		}
		supportURL := su.String()
		content, ok := u.h.fetchBytes(ctx, supportURL)
		if !ok {
			u.h.S.logger().Warn(fmt.Sprintf("URL skill %s: referenced support file %s could not be fetched from %s; skipping it",
				l.url, errchain.Show(rel), supportURL))
			continue
		}
		files = append(files, BundleFile{Path: rel, Data: content, Binary: true})
	}
	skillName := ""
	if l.name != nil {
		if skillName, err = validateHubSkillName(*l.name); err != nil {
			u.h.S.logger().Warn(fmt.Sprintf("URL skill %s produced unsafe skill name: %s", l.url, errchain.Show(*l.name)))
			return nil, nil
		}
	}
	return &SkillBundle{Name: skillName, Files: files, Source: "url", Identifier: l.url, TrustLevel: "community",
		Metadata: objectOf("url", l.url, "source_url", l.url, "awaiting_name", skillName == "")}, nil
}

// Reports a lowercase identifier shaped name other than the generic sentinels
func urlIsValidSkillName(name string) bool {
	candidate := strings.ToLower(strings.TrimSpace(name))
	switch candidate {
	case "", "skill", "readme", "index", "unnamed-skill":
		return false
	}
	return urlSourceValidName.MatchString(candidate)
}

var mdSuffixRe = regexp.MustCompile(`(?i)\.md$`)

// Frontmatter name when valid, else the URL slug, nil when nothing usable resolves
func urlResolveSkillName(fm map[string]any, rawURL string) *string {
	if n, ok := fm["name"].(string); ok && urlIsValidSkillName(n) {
		return strPtr(strings.TrimSpace(n))
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	var parts []string
	for _, p := range strings.Split(u.Path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) >= 2 && strings.ToLower(parts[len(parts)-1]) == "skill.md" && urlIsValidSkillName(parts[len(parts)-2]) {
		return strPtr(parts[len(parts)-2])
	}
	candidate := ""
	if len(parts) > 0 {
		last := parts[len(parts)-1]
		if loc := mdSuffixRe.FindStringIndex(last); loc != nil {
			last = last[:loc[0]]
		}
		candidate = last
	}
	if urlIsValidSkillName(candidate) {
		return strPtr(candidate)
	}
	return nil
}

// LobeHub agent marketplace converted to SKILL.md on fetch
type LobeHubSource struct {
	SourceBase
	h *Hub
}

const lobeHubIndexURL = "https://chat-agents.lobehub.com/index.json"

// LobeHub adapter over the hub
func NewLobeHubSource(h *Hub) *LobeHubSource {
	return &LobeHubSource{SourceBase: SourceBase{ID: "lobehub"}, h: h}
}

func (l *LobeHubSource) agents(ctx context.Context) []any {
	index, err := l.h.memoJSON("lobehub_index", func() (any, error) {
		return l.h.getJSON(ctx, lobeHubIndexURL, hubGet{Timeout: 30e9})
	}, nil)
	if err != nil {
		hubRaise(err)
	}
	if !anyutil.IsTrue(index) {
		return nil
	}
	agents := index
	if o, isObj := index.(*jsonutil.Object); isObj {
		agents = lookup(o, "agents", o)
	}
	list, _ := agents.([]any)
	return list
}

func lobeHubAgentID(identifier string) string {
	if strings.HasPrefix(identifier, "lobehub/") {
		_, after, _ := strings.Cut(identifier, "/")
		return after
	}
	return identifier
}

func lobeHubAgentMeta(agent any, name, description string) SkillMeta {
	tags := lookup(lookup(agent, "meta", agent), "tags", []any{})
	return SkillMeta{Name: name, Description: description, Source: "lobehub", Identifier: "lobehub/" + name,
		TrustLevel: "community", Tags: tagStrings(tags), Extra: jsonutil.NewObject()}
}

// Agents whose title, description or tags match the query
func (l *LobeHubSource) Search(ctx context.Context, query string, limit int) (out []SkillMeta, err error) {
	defer hubRecover(&err)
	agents := l.agents(ctx)
	if agents == nil {
		return []SkillMeta{}, nil
	}
	fields := func(agent any) []any {
		meta := lookup(agent, "meta", agent)
		var tags any = lookup(meta, "tags", []any{})
		if _, isList := tags.([]any); !isList {
			tags = ""
		}
		return []any{lookup(meta, "title", lookup(agent, "identifier", "")), lookup(meta, "description", ""), tags}
	}
	toMeta := func(agent any) *SkillMeta {
		meta := lookup(agent, "meta", agent)
		title := lookup(meta, "title", lookup(agent, "identifier", ""))
		identifier := lookup(agent, "identifier", strings.ReplaceAll(strings.ToLower(hubText(title)), " ", "-"))
		m := lobeHubAgentMeta(agent, hubText(identifier), headValue(lookup(meta, "description", ""), 200))
		return &m
	}
	return firstMatching(strings.ToLower(query), agents, fields, toMeta, limit), nil
}

// One agent converted to a SKILL.md bundle
func (l *LobeHubSource) Fetch(ctx context.Context, identifier string) (b *SkillBundle, err error) {
	defer hubRecover(&err)
	agentID := lobeHubAgentID(identifier)
	data, err := l.h.getJSON(ctx, "https://chat-agents.lobehub.com/"+agentID+".json", hubGet{Timeout: 15e9})
	if err != nil || !anyutil.IsTrue(data) {
		return nil, err
	}
	return &SkillBundle{Name: agentID, Files: []BundleFile{{Path: "SKILL.md", Data: []byte(lobeHubConvertToSkillMD(data))}},
		Source: "lobehub", Identifier: "lobehub/" + agentID, TrustLevel: "community", Metadata: jsonutil.NewObject()}, nil
}

// Index metadata of one agent
func (l *LobeHubSource) Inspect(ctx context.Context, identifier string) (m *SkillMeta, err error) {
	defer hubRecover(&err)
	agentID := lobeHubAgentID(identifier)
	for _, a := range l.agents(ctx) {
		if lookup(a, "identifier", nil) == agentID {
			if !anyutil.IsTrue(a) {
				return nil, nil
			}
			meta := lobeHubAgentMeta(a, agentID, hubText(lookup(lookup(a, "meta", a), "description", "")))
			return &meta, nil
		}
	}
	return nil, nil
}

// SKILL.md text of a LobeHub agent document
func lobeHubConvertToSkillMD(agentData any) string {
	meta := lookup(agentData, "meta", agentData)
	identifier := hubText(lookup(agentData, "identifier", "lobehub-agent"))
	title := hubText(lookup(meta, "title", identifier))
	description := hubText(lookup(meta, "description", ""))
	tags := tagStrings(lookup(meta, "tags", []any{}))
	systemRole := lookup(lookup(agentData, "config", jsonutil.NewObject()), "systemRole", "")
	role := "(No system role defined)"
	if anyutil.IsTrue(systemRole) {
		role = hubText(systemRole)
	}
	fm := []string{"---", "name: " + identifier, "description: " + headValue(description, 500), "metadata:", "  icebath:",
		"    tags: [" + strings.Join(tags, ", ") + "]", "  lobehub:", "    source: lobehub", "---"}
	body := []string{"# " + title, "", description, "", "## Instructions", "", role}
	return strings.Join(fm, "\n") + "\n\n" + strings.Join(body, "\n") + "\n"
}

// Browserbase browse.sh catalog of site specific browser automation skills
type BrowseShSource struct {
	SourceBase
	h *Hub
}

const browseShCatalogURL = "https://browse.sh/api/skills"

// browse.sh adapter over the hub
func NewBrowseShSource(h *Hub) *BrowseShSource {
	return &BrowseShSource{SourceBase: SourceBase{ID: "browse-sh"}, h: h}
}

func (b *BrowseShSource) catalog(ctx context.Context) []any {
	v, err := b.h.memoJSON("browse_sh_catalog", func() (any, error) {
		data, err := b.h.getJSON(ctx, browseShCatalogURL, hubGet{})
		if err != nil {
			return nil, err
		}
		var skills any = []any{}
		if o, isObj := data.(*jsonutil.Object); isObj {
			skills = lookup(o, "skills", []any{})
		}
		if list, isList := skills.([]any); isList {
			return list, nil
		}
		return nil, nil
	}, nil)
	if err != nil {
		hubRaise(err)
	}
	if !anyutil.IsTrue(v) {
		return []any{}
	}
	list, ok := v.([]any)
	if !ok {
		hubRaise(fmt.Errorf("skills catalog is %s where a list was expected", errchain.Kind(v)))
	}
	return list
}

func (b *BrowseShSource) itemToMeta(item any) *SkillMeta {
	slug := lookup(item, "slug", "")
	name := lookup(item, "name", "")
	desc := lookup(item, "description", lookup(item, "title", name))
	if !anyutil.IsTrue(slug) || !anyutil.IsTrue(name) {
		return nil
	}
	description := hubText(desc)
	if rs := []rune(description); len(rs) > 1024 {
		description = string(rs[:1021]) + "..."
	}
	return &SkillMeta{Name: anyutil.String(name), Description: description, Source: "browse-sh", Identifier: "browse-sh/" + anyutil.String(slug),
		TrustLevel: "community", Tags: tagStrings(lookup(item, "tags", []any{})),
		Extra: objectOf("slug", slug, "hostname", lookup(item, "hostname", ""), "category", lookup(item, "category", ""),
			"source_url", lookup(item, "sourceUrl", ""), "recommended_method", lookup(item, "recommendedMethod", ""),
			"proxies", lookup(item, "proxies", false), "install_count", lookup(item, "installCount", int64(0)))}
}

// Catalog skills matching the query
func (b *BrowseShSource) Search(ctx context.Context, query string, limit int) (out []SkillMeta, err error) {
	defer hubRecover(&err)
	fields := func(item any) []any {
		return []any{lookup(item, "name", ""), lookup(item, "title", ""), lookup(item, "description", ""),
			lookup(item, "hostname", ""), lookup(item, "category", ""), lookup(item, "tags", []any{})}
	}
	return firstMatching(strings.ToLower(query), b.catalog(ctx), fields, b.itemToMeta, limit), nil
}

func (b *BrowseShSource) catalogItem(ctx context.Context, identifier string) any {
	slug := strings.TrimPrefix(identifier, "browse-sh/")
	if slug == "" {
		return nil
	}
	for _, i := range b.catalog(ctx) {
		if lookup(i, "slug", nil) == slug {
			return i
		}
	}
	return nil
}

// Catalog metadata of one skill
func (b *BrowseShSource) Inspect(ctx context.Context, identifier string) (m *SkillMeta, err error) {
	defer hubRecover(&err)
	item := b.catalogItem(ctx, identifier)
	if !anyutil.IsTrue(item) {
		return nil, nil
	}
	return b.itemToMeta(item), nil
}

// SKILL.md of one catalog skill from its detail skillMdUrl or a raw GitHub source URL
func (b *BrowseShSource) Fetch(ctx context.Context, identifier string) (bundle *SkillBundle, err error) {
	defer hubRecover(&err)
	item := b.catalogItem(ctx, identifier)
	if !anyutil.IsTrue(item) {
		return nil, nil
	}
	slug, ok := item.(*jsonutil.Object).Get("slug")
	if !ok {
		return nil, errors.New("the browse.sh entry has no slug")
	}
	mdURL, err := b.resolveSkillMDURL(ctx, anyutil.String(slug), item)
	if err != nil || mdURL == "" {
		return nil, err
	}
	content, ok, err := b.h.getText(ctx, mdURL, hubGet{FollowRedirects: true})
	if err != nil || !ok {
		return nil, err
	}
	name := hubText(slug)[strings.LastIndex(hubText(slug), "/")+1:]
	if meta := b.itemToMeta(item); meta != nil {
		name = meta.Name
	}
	return &SkillBundle{Name: name, Files: []BundleFile{{Path: "SKILL.md", Data: []byte(content)}}, Source: "browse-sh",
		Identifier: identifier, TrustLevel: "community", Metadata: objectOf("slug", slug, "hostname", lookup(item, "hostname", ""),
			"source_url", lookup(item, "sourceUrl", ""), "skill_md_url", mdURL)}, nil
}

// Detail skillMdUrl of a slug, else a raw.githubusercontent.com sourceUrl, empty when neither exists
func (b *BrowseShSource) resolveSkillMDURL(ctx context.Context, slug string, item any) (string, error) {
	data, err := b.h.getJSON(ctx, "https://browse.sh/api/skills/"+slug, hubGet{FollowRedirects: true})
	if err != nil {
		return "", err
	}
	if o, isObj := data.(*jsonutil.Object); isObj {
		if md, isStr := lookup(o, "skillMdUrl", nil).(string); isStr && strings.HasPrefix(md, "http") {
			return md, nil
		}
	}
	var sourceURL any = ""
	if _, isObj := item.(*jsonutil.Object); isObj {
		sourceURL = lookup(item, "sourceUrl", "")
	}
	if !anyutil.IsTrue(sourceURL) {
		return "", nil
	}
	matches, err := urlutil.BaseHostMatches(hubText(sourceURL), "raw.githubusercontent.com")
	if err != nil || !matches {
		return "", err
	}
	return hubText(sourceURL), nil
}
