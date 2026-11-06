package skills

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
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
func pyGet(v any, key string, def any) any {
	o, ok := v.(*pyjson.Object)
	if !ok {
		hubRaise(fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(v)))
	}
	if x, found := o.Get(key); found {
		return x
	}
	return def
}

// Registry value used as str, the TypeError for anything else
func pyStrOf(v any) string {
	s, ok := v.(string)
	if !ok {
		hubRaise(fmt.Errorf("'%s' object is not a str", pyrepr.TypeName(v)))
	}
	return s
}

// First n code points of a registry str
func pyHead(v any, n int) string {
	rs := pystr.Runes(pyStrOf(v))
	if len(rs) > n {
		rs = rs[:n]
	}
	return pystr.FromRunes(rs)
}

// Registry tag list as strings, empty when the value is not a list
func tagStrings(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, len(list))
	for i, t := range list {
		out[i] = pyrepr.Str(t)
	}
	return out
}

func strPtr(s string) *string { return &s }

func objectOf(kv ...any) *pyjson.Object {
	o := pyjson.NewObject()
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

func (w *WellKnownSkillSource) meta(idx *pyjson.Object, skillName, description string, files any, extra ...any) SkillMeta {
	base := pyStrOf(pyGet(idx, "base_url", nil))
	ex := objectOf(append([]any{"index_url", pyGet(idx, "index_url", nil), "base_url", base, "files", files}, extra...)...)
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
	skills := pyGet(idx, "skills", nil).([]any)
	end := min(limit, len(skills))
	if limit < 0 {
		end = max(len(skills)+limit, 0)
	}
	for _, entry := range skills[:end] {
		name, isStr := pyGet(entry, "name", nil).(string)
		if !isStr || name == "" {
			continue
		}
		files := pyGet(entry, "files", []any{"SKILL.md"})
		if _, isList := files.([]any); !isList {
			files = []any{"SKILL.md"}
		}
		out = append(out, w.meta(idx, name, pyrepr.Str(pyGet(entry, "description", "")), files))
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
	if pyrepr.Truthy(fm["name"]) {
		name = pyrepr.Str(fm["name"])
	}
	desc := fm["description"]
	if !pyrepr.Truthy(desc) {
		desc = pyGet(entry, "description", nil)
	}
	description := ""
	if pyrepr.Truthy(desc) {
		description = pyrepr.Str(desc)
	}
	idx := objectOf("index_url", p.indexURL, "base_url", p.baseURL)
	meta := w.meta(idx, name, description, pyGet(entry, "files", []any{"SKILL.md"}), "endpoint", p.skillURL)
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
	if entry == nil || !pyrepr.Truthy(entry) {
		return nil, nil
	}
	files := pyGet(entry, "files", []any{"SKILL.md"})
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
			w.h.S.logger().Warn(fmt.Sprintf("Well-known skill %s advertised unsafe file path: %s", identifier, pyrepr.Repr(rel)))
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
	query = pystr.Strip(query)
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
	u, err := pyurl.Parse(raw)
	if err != nil {
		return nil, err
	}
	frag := u.Fragment
	u.Fragment = ""
	clean := pyurl.Unparse(u)
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
func (w *WellKnownSkillSource) parseIndex(ctx context.Context, indexURL string) *pyjson.Object {
	sum := md5.Sum([]byte(indexURL))
	v, err := w.h.memoJSON("well_known_index_"+hex.EncodeToString(sum[:]), func() (any, error) {
		resp := w.h.guardedHTTPGet(ctx, indexURL, defaultHubHTTPTimeout, nil)
		if resp == nil || resp.StatusCode != 200 {
			return nil, nil
		}
		data, err := resp.JSON()
		if err != nil {
			if _, isDecode := err.(*pyjson.DecodeError); isDecode {
				return nil, nil
			}
			return nil, err
		}
		var skills any = []any{}
		if o, isObj := data.(*pyjson.Object); isObj {
			skills = pyGet(o, "skills", []any{})
		}
		if _, isList := skills.([]any); !isList {
			return nil, nil
		}
		return objectOf("index_url", indexURL, "base_url", strings.TrimSuffix(indexURL, "/index.json"), "skills", skills), nil
	}, func(c any) bool {
		o, isObj := c.(*pyjson.Object)
		if !isObj {
			return false
		}
		_, isList := pyGet(o, "skills", nil).([]any)
		return isList
	})
	if err != nil {
		hubRaise(err)
	}
	o, _ := v.(*pyjson.Object)
	return o
}

// Index entry naming the skill, nil when absent
func (w *WellKnownSkillSource) indexEntry(ctx context.Context, indexURL, skillName string) any {
	idx := w.parseIndex(ctx, indexURL)
	if idx == nil || !pyrepr.Truthy(idx) {
		return nil
	}
	skills, _ := pyGet(idx, "skills", nil).([]any)
	for _, e := range skills {
		if o, isObj := e.(*pyjson.Object); isObj && pyGet(o, "name", nil) == skillName {
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
	ident := pystr.Strip(identifier)
	lower := pystr.Lower(ident)
	if (!strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://")) || strings.Contains(ident, "/.well-known/skills/") ||
		strings.HasSuffix(strings.TrimRight(ident, "/"), "/index.json") {
		return false
	}
	parsed, err := pyurl.Parse(ident)
	if err != nil {
		return false
	}
	return strings.HasSuffix(pystr.Lower(parsed.Path), ".md")
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
	url := pystr.Strip(identifier)
	text, ok := u.h.fetchText(ctx, url)
	if !ok {
		return nil
	}
	fm := hubParseFrontmatter(text)
	return &urlLoaded{url: url, text: text, fm: fm, name: urlResolveSkillName(fm, url)}
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
	if pyrepr.Truthy(l.fm["description"]) {
		desc = pyrepr.Str(l.fm["description"])
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
	baseURL := l.url[:strings.LastIndex(l.url, "/")] + "/"
	origin, err := pyurl.Parse(l.url)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(referenced))
	for p := range referenced {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, rel := range paths {
		supportURL, err := pyurl.Join(baseURL, pyurl.Quote(rel, "/"))
		if err != nil {
			return nil, err
		}
		su, err := pyurl.Parse(supportURL)
		if err != nil {
			return nil, err
		}
		if su.Netloc != origin.Netloc {
			return nil, nil
		}
		content, ok := u.h.fetchBytes(ctx, supportURL)
		if !ok {
			u.h.S.logger().Warn(fmt.Sprintf("URL skill %s: referenced support file %s could not be fetched from %s; skipping it",
				l.url, pyrepr.Repr(rel), supportURL))
			continue
		}
		files = append(files, BundleFile{Path: rel, Data: content, Binary: true})
	}
	skillName := ""
	if l.name != nil {
		if skillName, err = validateHubSkillName(*l.name); err != nil {
			u.h.S.logger().Warn(fmt.Sprintf("URL skill %s produced unsafe skill name: %s", l.url, pyrepr.Repr(*l.name)))
			return nil, nil
		}
	}
	return &SkillBundle{Name: skillName, Files: files, Source: "url", Identifier: l.url, TrustLevel: "community",
		Metadata: objectOf("url", l.url, "source_url", l.url, "awaiting_name", skillName == "")}, nil
}

// Reports a lowercase identifier shaped name other than the generic sentinels
func urlIsValidSkillName(name string) bool {
	candidate := pystr.Lower(pystr.Strip(name))
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
		return strPtr(pystr.Strip(n))
	}
	u, err := pyurl.Parse(rawURL)
	if err != nil {
		return nil
	}
	var parts []string
	for _, p := range strings.Split(u.Path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) >= 2 && pystr.Lower(parts[len(parts)-1]) == "skill.md" && urlIsValidSkillName(parts[len(parts)-2]) {
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
	if !pyrepr.Truthy(index) {
		return nil
	}
	agents := index
	if o, isObj := index.(*pyjson.Object); isObj {
		agents = pyGet(o, "agents", o)
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
	tags := pyGet(pyGet(agent, "meta", agent), "tags", []any{})
	return SkillMeta{Name: name, Description: description, Source: "lobehub", Identifier: "lobehub/" + name,
		TrustLevel: "community", Tags: tagStrings(tags), Extra: pyjson.NewObject()}
}

// Agents whose title, description or tags match the query
func (l *LobeHubSource) Search(ctx context.Context, query string, limit int) (out []SkillMeta, err error) {
	defer hubRecover(&err)
	agents := l.agents(ctx)
	if agents == nil {
		return []SkillMeta{}, nil
	}
	fields := func(agent any) []any {
		meta := pyGet(agent, "meta", agent)
		var tags any = pyGet(meta, "tags", []any{})
		if _, isList := tags.([]any); !isList {
			tags = ""
		}
		return []any{pyGet(meta, "title", pyGet(agent, "identifier", "")), pyGet(meta, "description", ""), tags}
	}
	toMeta := func(agent any) *SkillMeta {
		meta := pyGet(agent, "meta", agent)
		title := pyGet(meta, "title", pyGet(agent, "identifier", ""))
		identifier := pyGet(agent, "identifier", strings.ReplaceAll(pystr.Lower(pyStrOf(title)), " ", "-"))
		m := lobeHubAgentMeta(agent, pyStrOf(identifier), pyHead(pyGet(meta, "description", ""), 200))
		return &m
	}
	return firstMatching(pystr.Lower(query), agents, fields, toMeta, limit), nil
}

// One agent converted to a SKILL.md bundle
func (l *LobeHubSource) Fetch(ctx context.Context, identifier string) (b *SkillBundle, err error) {
	defer hubRecover(&err)
	agentID := lobeHubAgentID(identifier)
	data, err := l.h.getJSON(ctx, "https://chat-agents.lobehub.com/"+agentID+".json", hubGet{Timeout: 15e9})
	if err != nil || !pyrepr.Truthy(data) {
		return nil, err
	}
	return &SkillBundle{Name: agentID, Files: []BundleFile{{Path: "SKILL.md", Data: []byte(lobeHubConvertToSkillMD(data))}},
		Source: "lobehub", Identifier: "lobehub/" + agentID, TrustLevel: "community", Metadata: pyjson.NewObject()}, nil
}

// Index metadata of one agent
func (l *LobeHubSource) Inspect(ctx context.Context, identifier string) (m *SkillMeta, err error) {
	defer hubRecover(&err)
	agentID := lobeHubAgentID(identifier)
	for _, a := range l.agents(ctx) {
		if pyGet(a, "identifier", nil) == agentID {
			if !pyrepr.Truthy(a) {
				return nil, nil
			}
			meta := lobeHubAgentMeta(a, agentID, pyStrOf(pyGet(pyGet(a, "meta", a), "description", "")))
			return &meta, nil
		}
	}
	return nil, nil
}

// SKILL.md text of a LobeHub agent document
func lobeHubConvertToSkillMD(agentData any) string {
	meta := pyGet(agentData, "meta", agentData)
	identifier := pyStrOf(pyGet(agentData, "identifier", "lobehub-agent"))
	title := pyStrOf(pyGet(meta, "title", identifier))
	description := pyStrOf(pyGet(meta, "description", ""))
	tags := tagStrings(pyGet(meta, "tags", []any{}))
	systemRole := pyGet(pyGet(agentData, "config", pyjson.NewObject()), "systemRole", "")
	role := "(No system role defined)"
	if pyrepr.Truthy(systemRole) {
		role = pyStrOf(systemRole)
	}
	fm := []string{"---", "name: " + identifier, "description: " + pyHead(description, 500), "metadata:", "  hermes:",
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
		if o, isObj := data.(*pyjson.Object); isObj {
			skills = pyGet(o, "skills", []any{})
		}
		if list, isList := skills.([]any); isList {
			return list, nil
		}
		return nil, nil
	}, nil)
	if err != nil {
		hubRaise(err)
	}
	if !pyrepr.Truthy(v) {
		return []any{}
	}
	list, ok := v.([]any)
	if !ok {
		hubRaise(fmt.Errorf("'%s' object is not a catalog list", pyrepr.TypeName(v)))
	}
	return list
}

func (b *BrowseShSource) itemToMeta(item any) *SkillMeta {
	slug := pyGet(item, "slug", "")
	name := pyGet(item, "name", "")
	desc := pyGet(item, "description", pyGet(item, "title", name))
	if !pyrepr.Truthy(slug) || !pyrepr.Truthy(name) {
		return nil
	}
	description := pyStrOf(desc)
	if rs := pystr.Runes(description); len(rs) > 1024 {
		description = pystr.FromRunes(rs[:1021]) + "..."
	}
	return &SkillMeta{Name: pyrepr.Str(name), Description: description, Source: "browse-sh", Identifier: "browse-sh/" + pyrepr.Str(slug),
		TrustLevel: "community", Tags: tagStrings(pyGet(item, "tags", []any{})),
		Extra: objectOf("slug", slug, "hostname", pyGet(item, "hostname", ""), "category", pyGet(item, "category", ""),
			"source_url", pyGet(item, "sourceUrl", ""), "recommended_method", pyGet(item, "recommendedMethod", ""),
			"proxies", pyGet(item, "proxies", false), "install_count", pyGet(item, "installCount", int64(0)))}
}

// Catalog skills matching the query
func (b *BrowseShSource) Search(ctx context.Context, query string, limit int) (out []SkillMeta, err error) {
	defer hubRecover(&err)
	fields := func(item any) []any {
		return []any{pyGet(item, "name", ""), pyGet(item, "title", ""), pyGet(item, "description", ""),
			pyGet(item, "hostname", ""), pyGet(item, "category", ""), pyGet(item, "tags", []any{})}
	}
	return firstMatching(pystr.Lower(query), b.catalog(ctx), fields, b.itemToMeta, limit), nil
}

func (b *BrowseShSource) catalogItem(ctx context.Context, identifier string) any {
	slug := strings.TrimPrefix(identifier, "browse-sh/")
	if slug == "" {
		return nil
	}
	for _, i := range b.catalog(ctx) {
		if pyGet(i, "slug", nil) == slug {
			return i
		}
	}
	return nil
}

// Catalog metadata of one skill
func (b *BrowseShSource) Inspect(ctx context.Context, identifier string) (m *SkillMeta, err error) {
	defer hubRecover(&err)
	item := b.catalogItem(ctx, identifier)
	if !pyrepr.Truthy(item) {
		return nil, nil
	}
	return b.itemToMeta(item), nil
}

// SKILL.md of one catalog skill from its detail skillMdUrl or a raw GitHub source URL
func (b *BrowseShSource) Fetch(ctx context.Context, identifier string) (bundle *SkillBundle, err error) {
	defer hubRecover(&err)
	item := b.catalogItem(ctx, identifier)
	if !pyrepr.Truthy(item) {
		return nil, nil
	}
	slug, ok := item.(*pyjson.Object).Get("slug")
	if !ok {
		return nil, fmt.Errorf("KeyError: 'slug'")
	}
	mdURL, err := b.resolveSkillMDURL(ctx, pyrepr.Str(slug), item)
	if err != nil || mdURL == "" {
		return nil, err
	}
	content, ok, err := b.h.getText(ctx, mdURL, hubGet{FollowRedirects: true})
	if err != nil || !ok {
		return nil, err
	}
	name := pyStrOf(slug)[strings.LastIndex(pyStrOf(slug), "/")+1:]
	if meta := b.itemToMeta(item); meta != nil {
		name = meta.Name
	}
	return &SkillBundle{Name: name, Files: []BundleFile{{Path: "SKILL.md", Data: []byte(content)}}, Source: "browse-sh",
		Identifier: identifier, TrustLevel: "community", Metadata: objectOf("slug", slug, "hostname", pyGet(item, "hostname", ""),
			"source_url", pyGet(item, "sourceUrl", ""), "skill_md_url", mdURL)}, nil
}

// Detail skillMdUrl of a slug, else a raw.githubusercontent.com sourceUrl, empty when neither exists
func (b *BrowseShSource) resolveSkillMDURL(ctx context.Context, slug string, item any) (string, error) {
	data, err := b.h.getJSON(ctx, "https://browse.sh/api/skills/"+slug, hubGet{FollowRedirects: true})
	if err != nil {
		return "", err
	}
	if o, isObj := data.(*pyjson.Object); isObj {
		if md, isStr := pyGet(o, "skillMdUrl", nil).(string); isStr && strings.HasPrefix(md, "http") {
			return md, nil
		}
	}
	var sourceURL any = ""
	if _, isObj := item.(*pyjson.Object); isObj {
		sourceURL = pyGet(item, "sourceUrl", "")
	}
	if !pyrepr.Truthy(sourceURL) {
		return "", nil
	}
	matches, err := pyurl.BaseURLHostMatches(pyStrOf(sourceURL), "raw.githubusercontent.com")
	if err != nil || !matches {
		return "", err
	}
	return pyStrOf(sourceURL), nil
}
