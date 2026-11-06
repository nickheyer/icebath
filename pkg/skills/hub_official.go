package skills

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var indexIDPrefixes = []string{"skills-sh/", "skills.sh/", "official/", "github/", "clawhub/"}

func stripIDPrefix(value string, prefixes []string) string {
	for _, p := range prefixes {
		if strings.HasPrefix(value, p) {
			return value[len(p):]
		}
	}
	return value
}

// Relative path parts without dot and empty segments, nil on traversal or emptiness
func cleanRelParts(p string) []string {
	var parts []string
	for _, s := range strings.Split(p, "/") {
		if s != "" && s != "." {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 || slices.Contains(parts, "..") {
		return nil
	}
	return parts
}

// Lowercased extra provider label of a raw index entry
func entryProvider(entry any) string {
	extra := pyGet(entry, "extra", nil)
	if !pyrepr.Truthy(extra) {
		extra = pyjson.NewObject()
	}
	return pystr.Lower(pyrepr.Str(pyGet(extra, "provider", "")))
}

const (
	officialRepo           = "NousResearch/hermes-agent"
	optionalSkillsPrefix   = "optional-skills"
	officialRemoteDirsDesc = "Official optional skill (from live repo; run install to fetch)"
)

// Official optional skills of the packaged tree with a live repo fallback, builtin trust
type OptionalSkillSource struct {
	SourceBase
	h          *Hub
	auth       *GitHubAuth
	tree       skillTree
	github     *GitHubSource
	remoteDirs *pyjson.Object
}

// Source over the packaged optional skills tree
func NewOptionalSkillSource(h *Hub, auth *GitHubAuth) *OptionalSkillSource {
	return &OptionalSkillSource{SourceBase: SourceBase{ID: "official", Trust: "builtin"}, h: h, auth: auth, tree: optionalTree()}
}

func optionalRel(identifier string) string {
	if strings.HasPrefix(identifier, "official/") {
		return identifier[strings.Index(identifier, "/")+1:]
	}
	return identifier
}

func (o *OptionalSkillSource) meta(relDir, name, description string, tags []string) SkillMeta {
	return SkillMeta{Name: name, Description: description, Source: "official", Identifier: "official/" + relDir,
		TrustLevel: "builtin", Repo: strPtr(officialRepo), Path: strPtr(optionalSkillsPrefix + "/" + relDir), Tags: tags}
}

func (o *OptionalSkillSource) remoteMeta(relDir string) SkillMeta {
	return o.meta(relDir, relDir[strings.LastIndex(relDir, "/")+1:], officialRemoteDirsDesc, []string{})
}

func officialBundle(relID string, files []BundleFile) *SkillBundle {
	return &SkillBundle{Name: relID[strings.LastIndex(relID, "/")+1:], Files: files, Source: "official",
		Identifier: "official/" + relID, TrustLevel: "builtin"}
}

// Local matches up to limit, then live repo skills missing locally whose path contains the query
func (o *OptionalSkillSource) Search(ctx context.Context, query string, limit int) (out []SkillMeta, err error) {
	defer hubRecover(&err)
	results := []SkillMeta{}
	ql := pystr.Lower(query)
	local := map[string]bool{}
	for _, m := range o.scanAll() {
		local[m.Identifier[strings.Index(m.Identifier, "/")+1:]] = true
		if matchesQuery(ql, m.Name, m.Description, m.Tags) {
			results = append(results, m)
		}
		if len(results) >= limit {
			break
		}
	}
	if len(results) < limit {
		remote := slices.Clone(o.listRemoteSkillDirs(ctx).Keys)
		slices.Sort(remote)
		for _, rel := range remote {
			if local[rel] || (ql != "" && !strings.Contains(pystr.Lower(rel), ql)) {
				continue
			}
			results = append(results, o.remoteMeta(rel))
			if len(results) >= limit {
				break
			}
		}
	}
	return results, nil
}

// Skill directory relative to the tree root after resolving links, false when it escapes the root
func (o *OptionalSkillSource) resolveRel(rel string) (string, bool) {
	if strings.HasPrefix(rel, "/") {
		return "", false
	}
	clean := path.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	if o.tree.root == "" {
		return clean, true
	}
	root, err := filepath.EvalSymlinks(o.tree.root)
	if err != nil {
		root = filepath.Clean(o.tree.root)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(clean)))
	if err != nil {
		return clean, true
	}
	r, err := filepath.Rel(root, resolved)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(r), true
}

func (o *OptionalSkillSource) isDir(rel string) bool {
	st, err := fs.Stat(o.tree.fsys, rel)
	return err == nil && st.IsDir()
}

// UTF-8 text of a tree file with a leading BOM removed, false when unreadable
func readFSUTF8Sig(fsys fs.FS, name string) (string, bool) {
	b, err := fs.ReadFile(fsys, name)
	if err != nil || !utf8.Valid(b) {
		return "", false
	}
	return strings.TrimPrefix(string(b), "\xEF\xBB\xBF"), true
}

// Packaged skill directory, its upstream redirect, or the live repo copy when missing locally
func (o *OptionalSkillSource) Fetch(ctx context.Context, identifier string) (b *SkillBundle, err error) {
	defer hubRecover(&err)
	rel := optionalRel(identifier)
	resolved, ok := o.resolveRel(rel)
	if !ok {
		return nil, nil
	}
	skillDir, found := resolved, o.isDir(resolved)
	if !found {
		skillDir, found = o.findSkillDir(rel[strings.LastIndex(rel, "/")+1:])
	}
	if !found {
		return o.fetchFromLiveRepo(ctx, rel), nil
	}
	relID := skillDir
	if md, ok := readFSUTF8Sig(o.tree.fsys, path.Join(skillDir, "SKILL.md")); ok {
		if repo, upath, isStub := upstreamPointerFromContent([]byte(md)); isStub {
			return o.fetchFromUpstream(ctx, repo, upath, relID), nil
		}
	}
	files := []BundleFile{}
	fs.WalkDir(o.tree.fsys, skillDir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return nil
		}
		if st, serr := fs.Stat(o.tree.fsys, p); serr != nil || !st.Mode().IsRegular() {
			return nil
		}
		relFile := strings.TrimPrefix(p, skillDir+"/")
		if skillDir == "." {
			relFile = p
		}
		if skipBundleFile(relFile) {
			return nil
		}
		data, rerr := fs.ReadFile(o.tree.fsys, p)
		if rerr == nil {
			files = append(files, BundleFile{Path: relFile, Data: data, Binary: true})
		}
		return nil
	})
	if len(files) == 0 {
		return nil, nil
	}
	return officialBundle(relID, files), nil
}

// Local metadata by skill name, else the live repo entry when exactly one remote dir matches
func (o *OptionalSkillSource) Inspect(ctx context.Context, identifier string) (m *SkillMeta, err error) {
	defer hubRecover(&err)
	rel := optionalRel(identifier)
	name := rel[strings.LastIndex(rel, "/")+1:]
	for _, meta := range o.scanAll() {
		if meta.Name == name {
			return &meta, nil
		}
	}
	if matches := o.remoteMatches(ctx, name); len(matches) == 1 {
		rm := o.remoteMeta(matches[0])
		return &rm, nil
	}
	return nil, nil
}

// Every optional skill of the packaged tree with its frontmatter metadata
func (o *OptionalSkillSource) ListLocal() (out []SkillMeta, err error) {
	defer hubRecover(&err)
	return o.scanAll(), nil
}

func (o *OptionalSkillSource) getGitHub() *GitHubSource {
	if o.github == nil {
		o.github = NewGitHubSource(o.h, o.auth, nil)
	}
	return o.github
}

func (o *OptionalSkillSource) remoteMatches(ctx context.Context, name string) []string {
	var out []string
	for _, d := range o.listRemoteSkillDirs(ctx).Keys {
		if d[strings.LastIndex(d, "/")+1:] == name {
			out = append(out, d)
		}
	}
	return out
}

// Full skill directory from the live default branch, located by name when no category matches
func (o *OptionalSkillSource) fetchFromLiveRepo(ctx context.Context, rel string) *SkillBundle {
	parts := cleanRelParts(strings.Trim(rel, "/"))
	if parts == nil {
		return nil
	}
	rel = strings.Join(parts, "/")
	github := o.getGitHub()
	if _, has := o.listRemoteSkillDirs(ctx).Get(rel); !has {
		matches := o.remoteMatches(ctx, parts[len(parts)-1])
		if len(matches) != 1 {
			return nil
		}
		rel = matches[0]
	}
	tree := github.getRepoTree(ctx, officialRepo)
	if tree == nil {
		return nil
	}
	files := []BundleFile{}
	for _, m := range treeMembers(tree.Entries, optionalSkillsPrefix+"/"+rel+"/") {
		if !m.regular || skipBundleFile(m.rel) {
			continue
		}
		content, ok := github.fetchFileBytes(ctx, officialRepo, m.item, "")
		if !ok {
			o.h.S.logger().Warn(fmt.Sprintf("Live-repo optional skill fetch failed for %s", m.item))
			return nil
		}
		setBundleFile(&files, m.rel, content, true)
	}
	b := &SkillBundle{Files: files}
	md, ok := b.File("SKILL.md")
	if !ok {
		return nil
	}
	if repo, upath, isStub := upstreamPointerFromContent(md); isStub {
		return o.fetchFromUpstream(ctx, repo, upath, rel)
	}
	o.h.S.logger().Info(fmt.Sprintf("Optional skill '%s' fetched from live repo (not in local checkout)", rel))
	return officialBundle(rel, files)
}

// Category and skill dirs under optional-skills on live main through the index cache, empty offline
func (o *OptionalSkillSource) listRemoteSkillDirs(ctx context.Context) *pyjson.Object {
	if o.remoteDirs != nil {
		return o.remoteDirs
	}
	v, err := o.h.memoJSON("official_optional_dirs", func() (any, error) {
		tree := o.getGitHub().getRepoTree(ctx, officialRepo)
		if tree == nil {
			return nil, nil
		}
		dirs := pyjson.NewObject()
		prefix, suffix := optionalSkillsPrefix+"/", "/SKILL.md"
		for _, item := range pyIterate(tree.Entries) {
			pv := pyGet(item, "path", "")
			if pyGet(item, "type", nil) != "blob" {
				continue
			}
			p := pyStrOf(pv)
			if strings.HasPrefix(p, prefix) && strings.HasSuffix(p, suffix) && len(p) >= len(prefix)+len(suffix) {
				rel := p[len(prefix) : len(p)-len(suffix)]
				if rel != "" && !isExcludedSkillPath(rel+suffix, "") {
					dirs.Set(rel, true)
				}
			}
		}
		if dirs.Len() == 0 {
			return nil, nil
		}
		return dirs, nil
	}, func(c any) bool { d, ok := c.(*pyjson.Object); return ok && d.Len() > 0 })
	if err != nil {
		hubRaise(err)
	}
	o.remoteDirs, _ = v.(*pyjson.Object)
	if o.remoteDirs == nil {
		o.remoteDirs = pyjson.NewObject()
	}
	return o.remoteDirs
}

// Upstream repo and clean path of a catalog pointer SKILL.md, false for a vendored skill
func upstreamPointerFromContent(content []byte) (repo, upath string, ok bool) {
	if !utf8.Valid(content) {
		return "", "", false
	}
	fm := hubParseFrontmatter(string(content))
	meta, isMap := fm["metadata"].(map[string]any)
	if !isMap {
		return "", "", false
	}
	var hm any
	for _, k := range []string{"icebath", "hermes"} {
		if v, has := meta[k]; has {
			hm = v
			break
		}
	}
	hermes, isMap := hm.(map[string]any)
	if !isMap {
		return "", "", false
	}
	up, isMap := hermes["upstream"].(map[string]any)
	if !isMap {
		return "", "", false
	}
	field := func(k string) string {
		v, has := up[k]
		if !has {
			v = ""
		}
		return strings.Trim(pystr.Strip(pyrepr.Str(v)), "/")
	}
	repo, upath = field("repo"), field("path")
	if repo == "" || strings.Count(repo, "/") != 1 || upath == "" {
		return "", "", false
	}
	parts := cleanRelParts(upath)
	if parts == nil {
		return "", "", false
	}
	return repo, strings.Join(parts, "/"), true
}

// Upstream maintained skill fetched through GitHub and relabelled as an official trusted entry
func (o *OptionalSkillSource) fetchFromUpstream(ctx context.Context, repo, upath, relID string) *SkillBundle {
	b := o.getGitHub().fetch(ctx, repo+"/"+upath)
	if b == nil {
		o.h.S.logger().Warn(fmt.Sprintf("Upstream fetch failed for optional skill %s (%s:%s)", relID, repo, upath))
		return nil
	}
	md := pyjson.NewObject()
	if b.Metadata != nil {
		for _, k := range b.Metadata.Keys {
			v, _ := b.Metadata.Get(k)
			md.Set(k, v)
		}
	}
	md.Set("upstream_repo", repo)
	md.Set("upstream_path", upath)
	return &SkillBundle{Name: b.Name, Files: b.Files, Source: "official", Identifier: "official/" + relID, TrustLevel: "trusted", Metadata: md}
}

// SKILL.md paths of the tree sorted by path parts, excluded paths dropped
func (o *OptionalSkillSource) localSkillMDs() []string {
	var out []string
	if !o.isDir(".") {
		return out
	}
	fs.WalkDir(o.tree.fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "SKILL.md" && !isExcludedSkillPath(p, o.tree.root) {
			out = append(out, p)
		}
		return nil
	})
	slices.SortFunc(out, func(a, b string) int { return slices.Compare(strings.Split(a, "/"), strings.Split(b, "/")) })
	return out
}

// Directory of the first optional skill whose directory name equals name
func (o *OptionalSkillSource) findSkillDir(name string) (string, bool) {
	for _, md := range o.localSkillMDs() {
		if dir := path.Dir(md); path.Base(dir) == name {
			return dir, true
		}
	}
	return "", false
}

// Metadata of every packaged optional skill
func (o *OptionalSkillSource) scanAll() []SkillMeta {
	results := []SkillMeta{}
	for _, md := range o.localSkillMDs() {
		parent := path.Dir(md)
		content, ok := readFSUTF8Sig(o.tree.fsys, md)
		if !ok {
			continue
		}
		fm := hubParseFrontmatter(content)
		tags := []string{}
		if list, isList := hubTags(fm).([]any); isList {
			tags = tagStrings(list)
		}
		name := path.Base(parent)
		if v, has := fm["name"]; has {
			name = pyrepr.Str(v)
		}
		desc := ""
		if v, has := fm["description"]; has {
			desc = pyHead(v, 200)
		}
		results = append(results, o.meta(parent, name, desc, tags))
	}
	return results
}

// Skills of the centralized index, every method empty when the index is unavailable
type HermesIndexSource struct {
	SourceBase
	h      *Hub
	Auth   *GitHubAuth
	index  any
	loaded bool
	github *GitHubSource
}

// Index source sharing the credentials for its GitHub fetches
func NewHermesIndexSource(h *Hub, auth *GitHubAuth) *HermesIndexSource {
	return &HermesIndexSource{SourceBase: SourceBase{ID: "hermes-index"}, h: h, Auth: auth}
}

func (x *HermesIndexSource) ensureLoaded(ctx context.Context) any {
	if !x.loaded {
		x.index, x.loaded = x.h.loadHermesIndex(ctx), true
	}
	if !pyrepr.Truthy(x.index) {
		return pyjson.NewObject()
	}
	return x.index
}

func (x *HermesIndexSource) skills(ctx context.Context) []any {
	return pyIterate(pyGet(x.ensureLoaded(ctx), "skills", []any{}))
}

func (x *HermesIndexSource) getGitHub() *GitHubSource {
	if x.github == nil {
		x.github = NewGitHubSource(x.h, x.Auth, nil)
	}
	return x.github
}

// Whether the index loaded with at least one skill
func (x *HermesIndexSource) IsAvailable() bool {
	return pyrepr.Truthy(pyGet(x.ensureLoaded(context.Background()), "skills", []any{}))
}

// Trust level recorded for the identifier in the index, community otherwise
func (x *HermesIndexSource) TrustLevelFor(identifier string) string {
	for _, s := range x.skills(context.Background()) {
		if pyGet(s, "identifier", nil) == identifier {
			if !pyrepr.Truthy(s) {
				break
			}
			return pyrepr.Str(pyGet(s, "trust_level", "community"))
		}
	}
	return "community"
}

// Search without a provider filter
func (x *HermesIndexSource) Search(ctx context.Context, query string, limit int) ([]SkillMeta, error) {
	return x.SearchProvider(ctx, query, limit, "")
}

// Ranked index matches, exact name first then prefix, provider, whole word, substring, other
func (x *HermesIndexSource) SearchProvider(ctx context.Context, query string, limit int, providerFilter string) (out []SkillMeta, err error) {
	defer hubRecover(&err)
	skills := x.skills(ctx)
	if want := pystr.Lower(pystr.Strip(providerFilter)); want != "" {
		kept := []any{}
		for _, s := range skills {
			if entryProvider(s) == want {
				kept = append(kept, s)
			}
		}
		skills = kept
	}
	if len(skills) == 0 {
		return []SkillMeta{}, nil
	}
	if pystr.Strip(query) == "" {
		out = []SkillMeta{}
		for _, s := range pyHeadAny(skills, limit) {
			out = append(out, hermesIndexToMeta(s))
		}
		return out, nil
	}
	ql := pystr.Lower(query)
	type scored struct {
		rank, i int
		s       any
	}
	var hits []scored
	for i, s := range skills {
		name := pystr.Lower(pyrepr.Str(pyGet(s, "name", "")))
		provider := entryProvider(s)
		var tags []string
		for _, t := range pyIterate(pyGet(s, "tags", []any{})) {
			tags = append(tags, pystr.Lower(pyrepr.Str(t)))
		}
		hay := strings.Join([]string{name, pystr.Lower(pyrepr.Str(pyGet(s, "description", ""))), strings.Join(tags, " "),
			pystr.Lower(pyrepr.Str(pyGet(s, "identifier", ""))), provider}, " ")
		if !strings.Contains(hay, ql) {
			continue
		}
		ranks := []bool{name == ql, strings.HasPrefix(name, ql), provider == ql,
			slices.Contains(strings.Fields(name), ql) || slices.Contains(strings.Fields(provider), ql), strings.Contains(name, ql), true}
		hits = append(hits, scored{slices.Index(ranks, true), i, s})
	}
	slices.SortStableFunc(hits, func(a, b scored) int {
		if a.rank != b.rank {
			return a.rank - b.rank
		}
		return a.i - b.i
	})
	if limit < 0 {
		hits = hits[:max(len(hits)+limit, 0)]
	} else {
		hits = hits[:min(limit, len(hits))]
	}
	out = []SkillMeta{}
	for _, h := range hits {
		out = append(out, hermesIndexToMeta(h.s))
	}
	return out, nil
}

// Bundle of the resolved GitHub id or repo path, a skills.sh slug located in the repo tree as a last resort
func (x *HermesIndexSource) Fetch(ctx context.Context, identifier string) (b *SkillBundle, err error) {
	defer hubRecover(&err)
	entry := x.findEntry(ctx, identifier)
	if entry == nil {
		return nil, nil
	}
	repoV, pathV := pyGet(entry, "repo", ""), pyGet(entry, "path", "")
	p := pyStrOf(pathV)
	slugBase := strings.TrimRight(p, "/")
	slug := slugBase[strings.LastIndex(slugBase, "/")+1:]
	github := x.getGitHub()
	src := pyGet(entry, "source", nil)
	skillsSh := src == "skills.sh" || src == "skills-sh"
	candidates := []any{pyGet(entry, "resolved_github_id", nil)}
	if pyrepr.Truthy(repoV) && pyrepr.Truthy(pathV) {
		candidates = append(candidates, pyrepr.Str(repoV)+"/"+p)
	}
	var bundle *SkillBundle
	for _, c := range candidates {
		if !pyrepr.Truthy(c) {
			continue
		}
		if bundle = github.fetch(ctx, pyStrOf(c)); bundle != nil {
			break
		}
	}
	if bundle == nil {
		if !skillsSh || !pyrepr.Truthy(repoV) || slug == "" {
			return nil, nil
		}
		repo := pyStrOf(repoV)
		found, ok := github.findSkillInRepoTree(ctx, repo, slug)
		if !ok || found == "" {
			found, ok = github.findRepoRootSkill(ctx, repo)
		}
		if !ok || found == "" {
			return nil, nil
		}
		if bundle = github.fetch(ctx, found); bundle == nil {
			return nil, nil
		}
	}
	if skillsSh && bundle.Name == "skills" {
		bundle.Name = slug
	}
	bundle.Source = pyrepr.Str(pyGet(entry, "source", "hermes-index"))
	bundle.Identifier = identifier
	return bundle, nil
}

// Index metadata of the identifier
func (x *HermesIndexSource) Inspect(ctx context.Context, identifier string) (m *SkillMeta, err error) {
	defer hubRecover(&err)
	if entry := x.findEntry(ctx, identifier); entry != nil {
		meta := hermesIndexToMeta(entry)
		return &meta, nil
	}
	return nil, nil
}

// Exact identifier match first, then a match with source prefixes stripped
func (x *HermesIndexSource) findEntry(ctx context.Context, identifier string) any {
	skills := x.skills(ctx)
	for _, s := range skills {
		if pyGet(s, "identifier", nil) == identifier && pyrepr.Truthy(s) {
			return s
		}
	}
	normalized := stripIDPrefix(identifier, indexIDPrefixes)
	for _, s := range skills {
		if stripIDPrefix(pyStrOf(pyGet(s, "identifier", "")), indexIDPrefixes) == normalized && pyrepr.Truthy(s) {
			return s
		}
	}
	return nil
}

// Search result of an index entry
func hermesIndexToMeta(entry any) SkillMeta {
	optStr := func(k string) *string {
		v := pyGet(entry, k, nil)
		if v == nil {
			return nil
		}
		return strPtr(pyrepr.Str(v))
	}
	extra, _ := pyGet(entry, "extra", pyjson.NewObject()).(*pyjson.Object)
	return SkillMeta{Name: pyrepr.Str(pyGet(entry, "name", "")), Description: pyrepr.Str(pyGet(entry, "description", "")),
		Source: pyrepr.Str(pyGet(entry, "source", "hermes-index")), Identifier: pyrepr.Str(pyGet(entry, "identifier", "")),
		TrustLevel: pyrepr.Str(pyGet(entry, "trust_level", "community")), Repo: optStr("repo"), Path: optStr("path"),
		Tags: tagStrings(pyGet(entry, "tags", []any{})), Extra: extra}
}
