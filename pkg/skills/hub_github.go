package skills

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/gitcred"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyjwt"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// GitHub tap repo to the provider label the docs site catalog shows
var GitHubTapProviders = map[string]string{
	"openai/skills": "OpenAI", "anthropics/skills": "Anthropic", "huggingface/skills": "HuggingFace",
	"nvidia/skills": "NVIDIA", "voltagent/awesome-agent-skills": "VoltAgent", "garrytan/gstack": "gstack",
	"minimax-ai/cli": "MiniMax", "wandb/skills": "Weights & Biases", "qdrant/skills": "Qdrant",
	"pinecone-io/skills": "Pinecone",
}

var providerFilterValues = func() map[string]bool {
	out := map[string]bool{}
	for _, v := range GitHubTapProviders {
		out[pystr.Lower(v)] = true
	}
	return out
}()

const (
	githubAPI        = "https://api.github.com/repos"
	githubAcceptJSON = "application/vnd.github.v3+json"
)

// Provider label of an owner and repo tap ignoring case, empty when unknown
func githubProviderFor(repo string) string {
	if repo == "" {
		return ""
	}
	return GitHubTapProviders[pystr.Lower(pystr.Strip(repo))]
}

// Results whose extra provider equals the requested provider ignoring case
func filterResultsByProvider(results []SkillMeta, provider string) []SkillMeta {
	want := pystr.Lower(pystr.Strip(provider))
	out := []SkillMeta{}
	for _, r := range results {
		got := ""
		if r.Extra != nil {
			if v, ok := r.Extra.Get("provider"); ok {
				got = pyrepr.Str(v)
			}
		}
		if pystr.Lower(got) == want {
			out = append(out, r)
		}
	}
	return out
}

// Normalized provider filter when the source filter names a tap provider, else empty
func providerFilterOf(sourceFilter string) string {
	value := pystr.Lower(pystr.Strip(sourceFilter))
	if providerFilterValues[value] {
		return value
	}
	return ""
}

// Disk cache key of one tap listing
func tapCacheKey(repo, path, bucket string) string {
	return strings.NewReplacer("/", "_", " ", "_").Replace(repo + "_" + path + "_" + bucket)
}

// A 429, or a 403 whose remaining quota header reads zero
func isRateLimitResponse(resp *pyhttpx.Response) bool {
	return resp.StatusCode == 429 || (resp.StatusCode == 403 && pyhttpx.HeaderGet(resp.Header, "X-RateLimit-Remaining", "") == "0")
}

// GitHub API credentials tried as PAT, gh CLI, GitHub App, then anonymous, skipping rejected ones
type GitHubAuth struct {
	S                               *Scope
	mu                              sync.Mutex
	cachedToken, cachedMethod       string
	appTokenExpiry                  float64
	rejectedTokens, rejectedMethods map[string]bool
	rejected                        []string
	tryPat, tryGhCLI, tryGitHubApp  func() string
	appTokenURL                     string
	appPost                         func(req *http.Request) (*pyhttpx.Response, error)
	unixNow                         func() float64
}

// Credential chain over the scope secrets
func NewGitHubAuth(s *Scope) *GitHubAuth {
	a := &GitHubAuth{S: s, rejectedTokens: map[string]bool{}, rejectedMethods: map[string]bool{},
		appTokenURL: "https://api.github.com/app/installations/%s/access_tokens"}
	a.tryPat, a.tryGhCLI, a.tryGitHubApp = a.patToken, gitcred.GhCliToken, a.githubAppToken
	return a
}

func (a *GitHubAuth) now() float64 {
	if a.unixNow != nil {
		return a.unixNow()
	}
	return float64(time.Now().UnixNano()) / 1e9
}

// Accept header plus the token authorization when a credential resolves
func (a *GitHubAuth) GetHeaders() map[string]string {
	a.mu.Lock()
	token := a.resolveToken()
	a.mu.Unlock()
	out := map[string]string{"Accept": githubAcceptJSON}
	if token != "" {
		out["Authorization"] = "token " + token
	}
	return out
}

// Whether any credential resolves
func (a *GitHubAuth) IsAuthenticated() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.resolveToken() != ""
}

// One of pat, gh-cli, github-app or anonymous
func (a *GitHubAuth) AuthMethod() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resolveToken()
	if a.cachedMethod == "" {
		return "anonymous"
	}
	return a.cachedMethod
}

// Labels of the credentials GitHub answered 401 to, in rejection order
func (a *GitHubAuth) Rejected() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.rejected)
}

// Cached token while valid, else the first method yielding an unrejected token, anonymous remembered
func (a *GitHubAuth) resolveToken() string {
	if a.cachedToken != "" && (a.cachedMethod != "github-app" || a.now() < a.appTokenExpiry) {
		return a.cachedToken
	}
	if a.cachedMethod == "anonymous" {
		return ""
	}
	for _, m := range []struct {
		name    string
		resolve func() string
	}{{"pat", a.tryPat}, {"gh-cli", a.tryGhCLI}, {"github-app", a.tryGitHubApp}} {
		token := ""
		if !a.rejectedMethods[m.name] {
			token = m.resolve()
		}
		if token != "" && !a.rejectedTokens[token] {
			a.cachedToken, a.cachedMethod = token, m.name
			if m.name == "github-app" {
				a.appTokenExpiry = a.now() + 3500
			}
			return token
		}
	}
	a.cachedMethod = "anonymous"
	return ""
}

// Stops sending a token GitHub answered 401 to so the next credential and anonymous get tried
func (a *GitHubAuth) Reject(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rejectedTokens[token] {
		return
	}
	a.rejectedTokens[token] = true
	if token == a.cachedToken && a.cachedMethod != "" {
		label, ok := map[string]string{"pat": "GITHUB_TOKEN/GH_TOKEN", "gh-cli": "`gh auth token`"}[a.cachedMethod]
		if !ok {
			label = "the GitHub App token"
		}
		a.rejectedMethods[a.cachedMethod] = true
		a.rejected = append(a.rejected, label)
		a.cachedToken, a.cachedMethod = "", ""
		a.S.logger().Warn(fmt.Sprintf("GitHub rejected %s (401 Bad credentials); trying the next credential", label))
	}
}

// Profile scoped GITHUB_TOKEN, then GH_TOKEN
func (a *GitHubAuth) patToken() string {
	if v := a.S.Secrets.String("GITHUB_TOKEN"); v != "" {
		return v
	}
	return a.S.Secrets.String("GH_TOKEN")
}

// Installation token minted from a GitHub App JWT, empty on any failure
func (a *GitHubAuth) githubAppToken() string {
	appID, keyPath := a.S.Secrets.String("GITHUB_APP_ID"), a.S.Secrets.String("GITHUB_APP_PRIVATE_KEY_PATH")
	installationID := a.S.Secrets.String("GITHUB_APP_INSTALLATION_ID")
	if appID == "" || keyPath == "" || installationID == "" {
		return ""
	}
	if _, err := os.Stat(keyPath); err != nil {
		return ""
	}
	token, err := a.mintAppToken(appID, keyPath, installationID)
	if err != nil {
		a.S.logger().Debug(fmt.Sprintf("GitHub App auth failed: %s", err))
		return ""
	}
	return token
}

func (a *GitHubAuth) mintAppToken(appID, keyPath, installationID string) (string, error) {
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(raw) {
		return "", fmt.Errorf("'utf-8' codec can't decode the private key file %s", keyPath)
	}
	key := strings.TrimPrefix(string(raw), "\xEF\xBB\xBF")
	now := int64(a.now())
	claims := objectOf("iat", now-60, "exp", now+600, "iss", appID)
	jwt, err := pyjwt.EncodeRS256(claims, key)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf(a.appTokenURL, installationID), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", githubAcceptJSON)
	post := a.appPost
	if post == nil {
		post = func(r *http.Request) (*pyhttpx.Response, error) {
			return pyhttpx.Do(pyhttpx.PlainClient(false), r, 10*time.Second)
		}
	}
	resp, err := post(req)
	if err != nil || resp.StatusCode != 201 {
		return "", err
	}
	data, err := resp.JSON()
	if err != nil {
		return "", err
	}
	o, ok := data.(*pyjson.Object)
	if !ok {
		return "", fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(data))
	}
	tok, _ := o.Get("token")
	if !pyrepr.Truthy(tok) {
		return "", nil
	}
	return pyrepr.Str(tok), nil
}

// Owner and repo plus the skill path of an identifier with at least three segments
func splitRepoID(identifier string) (repo, path string, ok bool) {
	parts := strings.SplitN(identifier, "/", 3)
	if len(parts) < 3 {
		return "", "", false
	}
	return parts[0] + "/" + parts[1], parts[2], true
}

// File path inside a skill directory, the repo root when the skill path is empty
func skillFilePath(skillPath, filename string) string {
	if skillPath != "" {
		return skillPath + "/" + filename
	}
	return filename
}

// Dotfiles, bytecode and pycache members never ship in a bundle
func skipBundleFile(relPath string) bool {
	base := relPath[strings.LastIndex(relPath, "/")+1:]
	return strings.HasPrefix(base, ".") || strings.HasSuffix(base, ".pyc") || slices.Contains(strings.Split(relPath, "/"), "__pycache__")
}

type treeMember struct {
	rel, item string
	regular   bool
}

// Python iteration of a registry value, the TypeError for anything not iterable
func pyIterate(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case string:
		out := []any{}
		for _, r := range x {
			out = append(out, string(r))
		}
		return out
	case *pyjson.Object:
		out := make([]any, len(x.Keys))
		for i, k := range x.Keys {
			out[i] = k
		}
		return out
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		out := make([]any, len(keys))
		for i, k := range keys {
			out[i] = k
		}
		return out
	}
	hubRaise(fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(v)))
	return nil
}

// Git tree entries under prefix with their relative path and whether each is a regular blob
func treeMembers(entries any, prefix string) []treeMember {
	var out []treeMember
	for _, item := range pyIterate(entries) {
		itemPath := pyGet(item, "path", "")
		s, ok := itemPath.(string)
		if !ok {
			hubRaise(fmt.Errorf("'%s' object has no attribute 'startswith'", pyrepr.TypeName(itemPath)))
		}
		if strings.HasPrefix(s, prefix) {
			out = append(out, treeMember{s[len(prefix):], s, pyGet(item, "type", nil) == "blob" && pyGet(item, "mode", nil) != "120000"})
		}
	}
	return out
}

// Repo, path and bucket of one GitHub tap, an empty bucket meaning none
type GitHubTap struct{ Repo, Path, Bucket string }

// Taps every GitHub source searches before the user taps
var DefaultGitHubTaps = func() []GitHubTap {
	taps := []GitHubTap{
		{"openai/skills", "skills/.curated/", ""}, {"openai/skills", "skills/.system/", ""},
		{"anthropics/skills", "skills/", ""}, {"huggingface/skills", "skills/", ""}, {"NVIDIA/skills", "skills/", ""},
		{"wandb/skills", "skills/", ""}, {"qdrant/skills", "skills/", ""}, {"pinecone-io/skills", "skills/", ""},
		{"garrytan/gstack", "", ""}, {"K-Dense-AI/scientific-agent-skills", "skills/", "science"},
	}
	for _, c := range []string{"biology", "chemistry", "cloud-compute", "coding", "data-engineering", "databases",
		"document-parsing", "llm-tools", "ml-inference", "ml-training", "other", "physics", "quantum", "research",
		"scholar-evaluation", "visualization", "writing"} {
		taps = append(taps, GitHubTap{"synthetic-sciences/openscience", "backend/cli/skills/" + c + "/", "science"})
	}
	return taps
}()

// Taps of the user taps file, repo required, path and bucket optional
func tapsFromList(list []any) []GitHubTap {
	out := make([]GitHubTap, 0, len(list))
	for _, t := range list {
		o, ok := t.(*pyjson.Object)
		if !ok {
			hubRaise(fmt.Errorf("'%s' object is not subscriptable", pyrepr.TypeName(t)))
		}
		repo, found := o.Get("repo")
		if !found {
			hubRaise(fmt.Errorf("KeyError: 'repo'"))
		}
		tap := GitHubTap{Repo: pyrepr.Str(repo), Path: pyrepr.Str(pyGet(o, "path", ""))}
		if b := pyGet(o, "bucket", nil); pyrepr.Truthy(b) {
			tap.Bucket = pyrepr.Str(b)
		}
		out = append(out, tap)
	}
	return out
}

type ghTree struct {
	Branch  string
	Entries any
}

// Options of one GitHub API GET, zero timeout and retries meaning 15 s and three attempts
type ghGet struct {
	Params     pyhttpx.Params
	Headers    map[string]string
	Timeout    time.Duration
	MaxRetries int
}

// Overrides of the GitHub source internals the hermes tests patch per instance
type ghSeams struct {
	githubGet           func(url string, o ghGet) *pyhttpx.Response
	githubJSON          func(url string, o ghGet) any
	fetchFileContent    func(repo, path, ref string) (string, bool)
	fetchFileBytes      func(repo, path, ref string) ([]byte, bool)
	getRepoTree         func(repo string) *ghTree
	getSkillshGroupings func(repo string) map[string]string
	inspect             func(identifier string) *SkillMeta
	fetch               func(identifier string) *SkillBundle
	findSkillInRepoTree func(repo, skillName string) (string, bool)
	findRepoRootSkill   func(repo string) (string, bool)
	sleep               func(d time.Duration)
}

// Skills of GitHub repos read through the Contents and Trees APIs
type GitHubSource struct {
	SourceBase
	h                *Hub
	Auth             *GitHubAuth
	Taps             []GitHubTap
	treeCache        map[string]*ghTree
	treeRevisions    map[string]string
	skillshGroupings map[string]map[string]string
	rateLimited      bool
	rawUnusable      bool
	seams            ghSeams
}

// Source over the default taps followed by the extra taps
func NewGitHubSource(h *Hub, auth *GitHubAuth, extraTaps []GitHubTap) *GitHubSource {
	if auth == nil {
		auth = NewGitHubAuth(h.S)
	}
	return &GitHubSource{SourceBase: SourceBase{ID: "github"}, h: h, Auth: auth,
		Taps: append(slices.Clone(DefaultGitHubTaps), extraTaps...), treeCache: map[string]*ghTree{},
		treeRevisions: map[string]string{}, skillshGroupings: map[string]map[string]string{}}
}

var githubParseFrontmatterQuick = hubParseFrontmatter

// Whether a terminal rate limit exhaustion happened during this flow
func (g *GitHubSource) IsRateLimited() bool { return g.rateLimited }

// Trusted when the owner and repo of the identifier is a trusted repo
func (g *GitHubSource) TrustLevelFor(identifier string) string {
	parts := strings.SplitN(identifier, "/", 3)
	if len(parts) >= 2 && TrustedRepos[parts[0]+"/"+parts[1]] {
		return "trusted"
	}
	return "community"
}

// Search across every tap without a provider filter
func (g *GitHubSource) Search(ctx context.Context, query string, limit int) ([]SkillMeta, error) {
	return g.SearchProvider(ctx, query, limit, "")
}

// Substring matches of every tap of the provider, deduped by trust and cut to limit
func (g *GitHubSource) SearchProvider(ctx context.Context, query string, limit int, providerFilter string) ([]SkillMeta, error) {
	results := []SkillMeta{}
	ql := pystr.Lower(query)
	want := pystr.Lower(pystr.Strip(providerFilter))
	for _, tap := range g.Taps {
		if want != "" && pystr.Lower(githubProviderFor(tap.Repo)) != want {
			continue
		}
		var err error
		func() {
			defer hubRecover(&err)
			for _, skill := range g.listSkillsInRepo(ctx, tap.Repo, tap.Path, tap.Bucket) {
				if matchesQuery(ql, skill.Name, skill.Description, skill.Tags) {
					results = append(results, skill)
				}
			}
		}()
		if err != nil {
			g.h.S.logger().Debug(fmt.Sprintf("Failed to search %s: %s", tap.Repo, err))
		}
	}
	return pyHeadMetas(dedupeByTrust(results), limit), nil
}

// First limit results with Python slice semantics for a negative limit
func pyHeadMetas(results []SkillMeta, limit int) []SkillMeta {
	if limit < 0 {
		limit = max(len(results)+limit, 0)
	}
	return results[:min(limit, len(results))]
}

// Skill directory downloaded at the pinned tree revision
func (g *GitHubSource) Fetch(ctx context.Context, identifier string) (b *SkillBundle, err error) {
	defer hubRecover(&err)
	return g.fetch(ctx, identifier), nil
}

func (g *GitHubSource) fetch(ctx context.Context, identifier string) *SkillBundle {
	if g.seams.fetch != nil {
		return g.seams.fetch(identifier)
	}
	repo, skillPath, ok := splitRepoID(identifier)
	if !ok {
		return nil
	}
	skillDir := strings.TrimRight(skillPath, "/")
	tree := g.getRepoTree(ctx, repo)
	pinned := g.treeRevisions[repo]
	skillMD, ok := g.fetchFileContent(ctx, repo, skillFilePath(skillDir, "SKILL.md"), pinned)
	if !ok {
		return nil
	}
	referenced, ok, err := referencedSupportPaths(skillMD)
	if err != nil {
		hubRaise(err)
	}
	if !ok {
		return nil
	}
	files := []BundleFile{{Path: "SKILL.md", Data: []byte(skillMD)}}
	revision := ""
	if tree != nil {
		complete, ok := g.collectTreeFiles(ctx, repo, skillDir, tree.Entries, pinned, referenced, &files)
		if !ok {
			return nil
		}
		if complete {
			revision = pinned
			if revision == "" {
				revision = tree.Branch
			}
		}
	} else {
		for _, rel := range sortedKeys(referenced) {
			g.addSupportFile(ctx, repo, skillFilePath(skillDir, rel), rel, &files, rel, "")
		}
	}
	url := "https://github.com/" + repo + "/" + skillPath
	if revision != "" {
		url = "https://github.com/" + repo + "/tree/" + revision
		if skillPath != "" {
			url += "/" + skillPath
		}
	}
	name := skillDir[strings.LastIndex(skillDir, "/")+1:]
	if name == "" {
		name = repo[strings.LastIndex(repo, "/")+1:]
	}
	return &SkillBundle{Name: name, Files: files, Source: "github", Identifier: identifier, TrustLevel: g.TrustLevelFor(identifier),
		Metadata: objectOf("source_url", url, "source_revision", revision)}
}

// Sets a bundle member in place when the path exists, else appends it
func setBundleFile(files *[]BundleFile, path string, data []byte, binary bool) {
	for i := range *files {
		if (*files)[i].Path == path {
			(*files)[i].Data, (*files)[i].Binary = data, binary
			return
		}
	}
	*files = append(*files, BundleFile{Path: path, Data: data, Binary: binary})
}

// Tree sha the default branch resolves to, one cached tree lookup per repo
func (g *GitHubSource) CurrentRevision(ctx context.Context, identifier string) (rev string, err error) {
	defer hubRecover(&err)
	repo, _, ok := splitRepoID(identifier)
	if !ok {
		return "", nil
	}
	g.getRepoTree(ctx, repo)
	return g.treeRevisions[repo], nil
}

// Fetches one support file into the bundle, warning and reporting false on a failed fetch
func (g *GitHubSource) addSupportFile(ctx context.Context, repo, itemPath, relPath string, files *[]BundleFile, shown, ref string) bool {
	content, ok := g.fetchFileBytes(ctx, repo, itemPath, ref)
	if !ok {
		g.h.S.logger().Warn(fmt.Sprintf("Failed to fetch referenced skill support file; continuing without it: %s", shown))
		return false
	}
	setBundleFile(files, relPath, content, true)
	return true
}

// Whole skill directory of the pinned tree, ok false on an unsafe path or a linked non regular entry
func (g *GitHubSource) collectTreeFiles(ctx context.Context, repo, skillPath string, entries any, ref string,
	referenced map[string]bool, files *[]BundleFile) (complete, ok bool) {
	prefix := ""
	if skillPath != "" {
		prefix = skillPath + "/"
	}
	symlinked := map[string]bool{}
	complete = true
	log := g.h.S.logger()
	for _, m := range treeMembers(entries, prefix) {
		if !m.regular {
			symlinked[m.rel] = true
			continue
		}
		if m.rel == "SKILL.md" || skipBundleFile(m.rel) {
			continue
		}
		rel, err := validateBundleRelPath(m.rel)
		if err != nil {
			log.Warn(fmt.Sprintf("Rejected unsafe file path in skill bundle: %s", m.item))
			return false, false
		}
		complete = g.addSupportFile(ctx, repo, m.item, rel, files, m.item, ref) && complete
	}
	for _, rel := range sortedKeys(referenced) {
		if symlinked[rel] {
			log.Warn(fmt.Sprintf("Rejected non-regular referenced file in skill bundle: %s%s", prefix, rel))
			return false, false
		}
		if !slices.ContainsFunc(*files, func(f BundleFile) bool { return f.Path == rel }) {
			log.Warn(fmt.Sprintf("Referenced skill support file is missing; continuing without it: %s%s", prefix, rel))
		}
	}
	return complete, true
}

// SKILL.md metadata of a skill for preview
func (g *GitHubSource) Inspect(ctx context.Context, identifier string) (m *SkillMeta, err error) {
	defer hubRecover(&err)
	return g.inspect(ctx, identifier), nil
}

func (g *GitHubSource) inspect(ctx context.Context, identifier string) *SkillMeta {
	if g.seams.inspect != nil {
		return g.seams.inspect(identifier)
	}
	repo, sp, ok := splitRepoID(identifier)
	if !ok {
		return nil
	}
	skillPath := strings.TrimRight(sp, "/")
	content, ok := g.fetchFileContent(ctx, repo, skillFilePath(skillPath, "SKILL.md"), "")
	if !ok || content == "" {
		return nil
	}
	fm := hubParseFrontmatter(content)
	tags := hubTags(fm)
	if !pyrepr.Truthy(tags) {
		tags = []any{}
		if l, isList := fm["tags"].([]any); isList {
			tags = l
		}
	}
	tagList := []string{}
	for _, t := range pyIterate(tags) {
		tagList = append(tagList, pyrepr.Str(t))
	}
	name := skillPath[strings.LastIndex(skillPath, "/")+1:]
	if name == "" {
		name = repo[strings.LastIndex(repo, "/")+1:]
	}
	if v, has := fm["name"]; has {
		name = pyrepr.Str(v)
	}
	desc := ""
	if v, has := fm["description"]; has {
		desc = pyrepr.Str(v)
	}
	extra := pyjson.NewObject()
	if p := githubProviderFor(repo); p != "" {
		extra.Set("provider", p)
	}
	return &SkillMeta{Name: name, Description: desc, Source: "github", Identifier: identifier,
		TrustLevel: g.TrustLevelFor(identifier), Repo: strPtr(repo), Path: strPtr(skillPath), Tags: tagList, Extra: extra}
}

// Skill directories of one repo path through the index cache, stamped with their category
func (g *GitHubSource) listSkillsInRepo(ctx context.Context, repo, path, bucket string) []SkillMeta {
	key := tapCacheKey(repo, path, bucket)
	cached, ok, err := g.h.cachedMetas(key)
	if err != nil {
		hubRaise(err)
	}
	if ok {
		return cached
	}
	prefix := strings.TrimRight(path, "/")
	resp := g.githubGet(ctx, githubAPI+"/"+repo+"/contents/"+prefix, ghGet{})
	if resp == nil || resp.StatusCode != 200 {
		return []SkillMeta{}
	}
	data, err := resp.JSON()
	if err != nil {
		hubRaise(err)
	}
	entries, isList := data.([]any)
	if !isList {
		return []SkillMeta{}
	}
	skills := []SkillMeta{}
	groupings := g.getSkillshGroupings(ctx, repo)
	for _, entry := range entries {
		if pyGet(entry, "type", nil) != "dir" {
			continue
		}
		nameV, found := entry.(*pyjson.Object).Get("name")
		if !found {
			hubRaise(errors.New("KeyError: 'name'"))
		}
		dirName, isStr := nameV.(string)
		if !isStr {
			hubRaise(fmt.Errorf("'%s' object has no attribute 'startswith'", pyrepr.TypeName(nameV)))
		}
		if strings.HasPrefix(dirName, ".") || strings.HasPrefix(dirName, "_") {
			continue
		}
		id := repo + "/" + dirName
		if prefix != "" {
			id = repo + "/" + prefix + "/" + dirName
		}
		meta := g.inspect(ctx, id)
		if meta == nil {
			continue
		}
		category := ""
		if len(groupings) > 0 {
			if category = groupings[meta.Name]; category == "" {
				category = groupings[dirName]
			}
		}
		if category == "" {
			category = bucket
		}
		if category != "" {
			if meta.Extra == nil {
				meta.Extra = pyjson.NewObject()
			}
			meta.Extra.Set("category", category)
		}
		skills = append(skills, *meta)
	}
	if err := g.h.cacheMetas(key, skills); err != nil {
		hubRaise(err)
	}
	return skills
}

// Cached default branch and tree entries of a repo, nil for an unreachable or truncated tree
func (g *GitHubSource) getRepoTree(ctx context.Context, repo string) *ghTree {
	if g.seams.getRepoTree != nil {
		return g.seams.getRepoTree(repo)
	}
	if t, ok := g.treeCache[repo]; ok {
		return t
	}
	g.treeCache[repo] = nil
	repoData := g.githubJSON(ctx, githubAPI+"/"+repo, ghGet{})
	if repoData == nil {
		return nil
	}
	branch := pyrepr.Str(pyGet(repoData, "default_branch", "main"))
	treeData := g.githubJSON(ctx, githubAPI+"/"+repo+"/git/trees/"+branch,
		ghGet{Params: pyhttpx.Params{{"recursive", "1"}}, Timeout: 30 * time.Second})
	if treeData == nil {
		return nil
	}
	if pyrepr.Truthy(pyGet(treeData, "truncated", nil)) {
		g.h.S.logger().Debug(fmt.Sprintf("Git tree truncated for %s", repo))
		return nil
	}
	if sha, ok := pyGet(treeData, "sha", nil).(string); ok && sha != "" {
		g.treeRevisions[repo] = sha
	}
	t := &ghTree{Branch: branch, Entries: pyGet(treeData, "tree", []any{})}
	g.treeCache[repo] = t
	return t
}

// Decoded body of a 200 API answer, nil otherwise
func (g *GitHubSource) githubJSON(ctx context.Context, url string, o ghGet) any {
	if g.seams.githubJSON != nil {
		return g.seams.githubJSON(url, o)
	}
	resp := g.githubGet(ctx, url, o)
	if resp == nil || resp.StatusCode != 200 {
		return nil
	}
	v, err := resp.JSON()
	if err != nil {
		return nil
	}
	return v
}

func (g *GitHubSource) sleep(seconds float64) {
	d := time.Duration(seconds * float64(time.Second))
	if g.seams.sleep != nil {
		g.seams.sleep(d)
		return
	}
	time.Sleep(d)
}

// API GET with backoff on rate limits, 5xx and transport errors, a 401 retried with the next credential
func (g *GitHubSource) githubGet(ctx context.Context, url string, o ghGet) *pyhttpx.Response {
	if g.seams.githubGet != nil {
		return g.seams.githubGet(url, o)
	}
	if o.Timeout == 0 {
		o.Timeout = 15 * time.Second
	}
	if o.MaxRetries == 0 {
		o.MaxRetries = 3
	}
	hdrs := o.Headers
	if hdrs == nil {
		hdrs = g.Auth.GetHeaders()
	}
	log := g.h.S.logger()
	backoff := 1.0
	var last *pyhttpx.Response
	for attempt := 0; attempt < o.MaxRetries; attempt++ {
		lastAttempt := attempt >= o.MaxRetries-1
		wait := backoff
		resp, err := g.h.skillsHubHTTPGet(ctx, url, hubGet{Timeout: o.Timeout, Params: o.Params, Headers: hdrs, FollowRedirects: true})
		if err != nil {
			if pass := hubPassThrough(err); pass != nil {
				hubRaise(pass)
			}
			log.Debug(fmt.Sprintf("GitHub GET %s failed (attempt %d/%d): %s", url, attempt+1, o.MaxRetries, err))
			if lastAttempt {
				return nil
			}
		} else {
			last = resp
			if resp.StatusCode == 200 {
				return resp
			}
			sent := hdrs["Authorization"]
			if resp.StatusCode == 401 && sent != "" {
				g.Auth.Reject(sent[strings.Index(sent, " ")+1:])
				retry := map[string]string{}
				for k, v := range hdrs {
					if k != "Authorization" {
						retry[k] = v
					}
				}
				if a, ok := g.Auth.GetHeaders()["Authorization"]; ok {
					retry["Authorization"] = a
				}
				return g.githubGet(ctx, url, ghGet{Params: o.Params, Headers: retry, Timeout: o.Timeout, MaxRetries: o.MaxRetries})
			}
			if resp.StatusCode == 403 || resp.StatusCode == 429 {
				limited := isRateLimitResponse(resp)
				if !limited || lastAttempt {
					if limited {
						g.rateLimited = true
						log.Warn("GitHub API rate limit exhausted (unauthenticated: 60 req/hr). " +
							"Set GITHUB_TOKEN or install the gh CLI to raise the limit to 5,000/hr.")
					}
					return resp
				}
				reset := pyhttpx.HeaderGet(resp.Header, "X-RateLimit-Reset", "")
				if ra, ok := pyhttpx.RetryAfterSeconds(resp.Header); ok {
					wait = math.Min(ra, 60)
				} else if reset != "" && !strings.ContainsFunc(reset, func(r rune) bool { return !pystr.IsDigit(r) }) {
					if f, err := pyint.ParseFloat(reset); err == nil {
						if delta := f - g.Auth.now(); delta > 0 && delta <= 60 {
							wait = delta
						}
					}
				}
				log.Debug(fmt.Sprintf("GitHub rate limited on %s, waiting %.1fs (attempt %d/%d)", url, wait, attempt+1, o.MaxRetries))
			} else if resp.StatusCode < 500 || resp.StatusCode >= 600 || lastAttempt {
				return resp
			}
		}
		g.sleep(wait)
		backoff = math.Min(backoff*2, 30)
	}
	return last
}

// Identifier of the skill named skillName anywhere in the repo tree, or a lone generic skills dir
func (g *GitHubSource) findSkillInRepoTree(ctx context.Context, repo, skillName string) (string, bool) {
	if g.seams.findSkillInRepoTree != nil {
		return g.seams.findSkillInRepoTree(repo, skillName)
	}
	tree := g.getRepoTree(ctx, repo)
	if tree == nil {
		return "", false
	}
	suffix := "/" + skillName + "/SKILL.md"
	var dirs []string
	for _, entry := range pyIterate(tree.Entries) {
		pathV := pyGet(entry, "path", "")
		if pyGet(entry, "type", nil) != "blob" || pyGet(entry, "mode", nil) == "120000" || !strings.HasSuffix("/"+pyrepr.Str(pathV), "/SKILL.md") {
			continue
		}
		path := pyStrOf(pathV)
		if strings.HasSuffix(path, suffix) || path == suffix[1:] {
			return repo + "/" + path[:len(path)-len("/SKILL.md")], true
		}
		dirs = append(dirs, strings.TrimRight(path[:len(path)-len("SKILL.md")], "/"))
	}
	if len(dirs) == 1 && (dirs[0] == "skills" || dirs[0] == ".agents/skills" || dirs[0] == ".claude/skills") {
		return repo + "/" + dirs[0], true
	}
	return "", false
}

// Identifier of a single skill repo whose only SKILL.md sits at the repo root
func (g *GitHubSource) findRepoRootSkill(ctx context.Context, repo string) (string, bool) {
	if g.seams.findRepoRootSkill != nil {
		return g.seams.findRepoRootSkill(repo)
	}
	tree := g.getRepoTree(ctx, repo)
	if tree == nil {
		return "", false
	}
	var mds []string
	for _, entry := range pyIterate(tree.Entries) {
		if pyGet(entry, "type", nil) != "blob" || pyGet(entry, "mode", nil) == "120000" {
			continue
		}
		p := pyGet(entry, "path", "")
		if p == "SKILL.md" || strings.HasSuffix(pyStrOf(p), "/SKILL.md") {
			mds = append(mds, pyStrOf(p))
		}
	}
	if len(mds) == 1 && mds[0] == "SKILL.md" {
		return repo + "/", true
	}
	return "", false
}

// Text of one repo file, false on a miss or bytes that are not UTF-8
func (g *GitHubSource) fetchFileContent(ctx context.Context, repo, path, ref string) (string, bool) {
	if g.seams.fetchFileContent != nil {
		return g.seams.fetchFileContent(repo, path, ref)
	}
	b, ok := g.fetchFileBytes(ctx, repo, path, ref)
	if !ok || !utf8.Valid(b) {
		return "", false
	}
	return string(b), true
}

// Exact bytes of one repo file, a pinned file from the raw host first, the Contents API otherwise
func (g *GitHubSource) fetchFileBytes(ctx context.Context, repo, path, ref string) ([]byte, bool) {
	if g.seams.fetchFileBytes != nil {
		return g.seams.fetchFileBytes(repo, path, ref)
	}
	if ref != "" && !g.rawUnusable {
		raw, err := g.h.skillsHubHTTPGet(ctx, "https://raw.githubusercontent.com/"+repo+"/"+ref+"/"+pyurl.Quote(path, "/"),
			hubGet{Timeout: 15 * time.Second})
		if err != nil {
			var invalid *hubInvalidURLError
			if errors.As(err, &invalid) {
				hubRaise(err)
			}
			g.rawUnusable = true
		} else if raw.StatusCode == 200 {
			return raw.Content, true
		}
	}
	hdrs := g.Auth.GetHeaders()
	hdrs["Accept"] = "application/vnd.github.v3.raw"
	var params pyhttpx.Params
	if ref != "" {
		params = pyhttpx.Params{{"ref", ref}}
	}
	resp := g.githubGet(ctx, githubAPI+"/"+repo+"/contents/"+pyurl.Quote(path, "/"), ghGet{Params: params, Headers: hdrs})
	if resp == nil || resp.StatusCode != 200 {
		return nil, false
	}
	return resp.Content, true
}

// Repo root skills.sh.json groupings flattened per skill, cached per repo, nil when absent or unusable
func (g *GitHubSource) getSkillshGroupings(ctx context.Context, repo string) map[string]string {
	if g.seams.getSkillshGroupings != nil {
		return g.seams.getSkillshGroupings(repo)
	}
	if m, ok := g.skillshGroupings[repo]; ok {
		return m
	}
	var m map[string]string
	if content, ok := g.fetchFileContent(ctx, repo, "skills.sh.json", ""); ok && content != "" {
		m, _ = parseSkillshGroupings(content)
	}
	g.skillshGroupings[repo] = m
	return m
}

// Skill name to grouping title with the first grouping winning, false when the document is unusable
func parseSkillshGroupings(content string) (map[string]string, bool) {
	data, err := pyjson.Loads(content)
	if err != nil {
		return nil, false
	}
	obj, ok := data.(*pyjson.Object)
	if !ok {
		return nil, false
	}
	gv, _ := obj.Get("groupings")
	groups, ok := gv.([]any)
	if !ok {
		return nil, false
	}
	mapping := map[string]string{}
	for _, group := range groups {
		g, ok := group.(*pyjson.Object)
		if !ok {
			continue
		}
		tv, _ := g.Get("title")
		mv, _ := g.Get("skills")
		title, isStr := tv.(string)
		members, isList := mv.([]any)
		if !isStr || !isList {
			continue
		}
		for _, member := range members {
			if s, ok := member.(string); ok && s != "" {
				if _, seen := mapping[s]; !seen {
					mapping[s] = title
				}
			}
		}
	}
	return mapping, true
}
