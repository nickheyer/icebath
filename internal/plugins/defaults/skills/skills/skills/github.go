package skills

import (
	"bytes"
	"cmp"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	hubconfig "github.com/nickheyer/icebath/internal/plugins/defaults/skills/skills/config"
	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/internal/web"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/httpx"
	"github.com/nickheyer/icebath/pkg/proc"
)

// Provider each GitHub repository of skills the hub knows is listed under, by its owner and name in lower case
var tapProviders = map[string]string{
	"openai/skills": "OpenAI", "anthropics/skills": "Anthropic", "huggingface/skills": "HuggingFace", "nvidia/skills": "NVIDIA",
	"voltagent/awesome-agent-skills": "VoltAgent", "garrytan/gstack": "gstack", "minimax-ai/cli": "MiniMax", "wandb/skills": "Weights & Biases",
	"qdrant/skills": "Qdrant", "pinecone-io/skills": "Pinecone",
}

// Provider of a repository, empty when it is none the hub knows
func providerFor(repo string) string {
	return tapProviders[strings.ToLower(strings.TrimSpace(repo))]
}

// A GitHub repository the hub searches for skills: the folder of skills in it, and the category its skills get when it groups none
type tap struct {
	Repo   string `json:"repo"`
	Path   string `json:"path"`
	Bucket string `json:"bucket,omitempty"`
}

// Repositories every GitHub search covers, before the user's own
var defaultTaps = func() []tap {
	taps := []tap{
		{"openai/skills", "skills/.curated/", ""}, {"openai/skills", "skills/.system/", ""}, {"anthropics/skills", "skills/", ""},
		{"huggingface/skills", "skills/", ""}, {"NVIDIA/skills", "skills/", ""}, {"wandb/skills", "skills/", ""}, {"qdrant/skills", "skills/", ""},
		{"pinecone-io/skills", "skills/", ""}, {"garrytan/gstack", "", ""}, {"K-Dense-AI/scientific-agent-skills", "skills/", "science"},
	}
	for _, category := range []string{"biology", "chemistry", "cloud-compute", "coding", "data-engineering", "databases", "document-parsing",
		"llm-tools", "ml-inference", "ml-training", "other", "physics", "quantum", "research", "scholar-evaluation", "visualization", "writing"} {
		taps = append(taps, tap{"synthetic-sciences/openscience", "backend/cli/skills/" + category + "/", "science"})
	}
	return taps
}()

const (
	githubAPI  = "https://api.github.com/repos"
	acceptJSON = "application/vnd.github.v3+json"
)

// Trust of a skill by its repository: trusted for a repository the hub trusts, community for any other
func githubTrust(identifier string) string {
	if parts := strings.SplitN(identifier, "/", 3); len(parts) >= 2 && trustedRepos[parts[0]+"/"+parts[1]] {
		return "trusted"
	}
	return "community"
}

// Owner and repository of an identifier of three parts or more, and the path of the skill in it
func splitRepo(identifier string) (string, string, bool) {
	parts := strings.SplitN(identifier, "/", 3)
	if len(parts) < 3 {
		return "", "", false
	}
	return parts[0] + "/" + parts[1], parts[2], true
}

// Path of a file in a skill's folder, the repository's root when the skill has no path
func within(dir, name string) string {
	if dir != "" {
		return dir + "/" + name
	}
	return name
}

// Credentials the hub reads GitHub with, tried in turn: a personal token, the gh command's sign in, a GitHub App; anonymous when none
// works. A credential GitHub refuses is not sent again
type auth struct {
	cfg    *config.Config
	client *http.Client

	mu            sync.Mutex
	token, method string
	expiry        time.Time
	refused       map[string]bool
	refusedMethod map[string]bool
	// Credentials GitHub refused, as the user names them, in the order refused
	rejected []string
}

// Headers of a request to the API, with the credential when one resolves
func (a *auth) headers(ctx context.Context) map[string]string {
	a.mu.Lock()
	token := a.resolve(ctx)
	a.mu.Unlock()
	out := map[string]string{"Accept": acceptJSON}
	if token != "" {
		out["Authorization"] = "token " + token
	}
	return out
}

// Whether any credential resolves
func (a *auth) signedIn(ctx context.Context) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.resolve(ctx) != ""
}

// Credentials GitHub refused
func (a *auth) refusals() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.rejected)
}

// The credential in use while it lasts, else the first that yields a token GitHub did not refuse; anonymous kept once found
func (a *auth) resolve(ctx context.Context) string {
	if a.token != "" && (a.method != "github-app" || time.Now().Before(a.expiry)) {
		return a.token
	}
	if a.method == "anonymous" {
		return ""
	}
	for _, m := range []struct {
		name string
		get  func(context.Context) string
	}{{"pat", a.pat}, {"gh-cli", ghToken}, {"github-app", a.app}} {
		if a.refusedMethod[m.name] {
			continue
		}
		if token := m.get(ctx); token != "" && !a.refused[token] {
			a.token, a.method = token, m.name
			if m.name == "github-app" {
				a.expiry = time.Now().Add(3500 * time.Second)
			}
			return token
		}
	}
	a.token, a.method = "", "anonymous"
	return ""
}

// Stops sending a token GitHub answered 401 to, so the next credential and then none are tried
func (a *auth) reject(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.refused[token] {
		return
	}
	a.refused[token] = true
	if token != a.token || a.method == "" {
		return
	}
	label, ok := map[string]string{"pat": "GITHUB_TOKEN/GH_TOKEN", "gh-cli": "`gh auth token`"}[a.method]
	if !ok {
		label = "the GitHub App token"
	}
	a.refusedMethod[a.method] = true
	a.rejected = append(a.rejected, label)
	a.token, a.method = "", ""
	slog.Warn(fmt.Sprintf("GitHub rejected %s (401 Bad credentials); trying the next credential", label))
}

func (a *auth) pat(context.Context) string {
	return strings.TrimSpace(a.cfg.GetString(hubconfig.GitHubToken))
}

// Token the gh command is signed in with, never asking anything
func ghToken(ctx context.Context) string {
	gh, err := exec.LookPath("gh")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gh, "auth", "token")
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); name != "GH_TOKEN" && name != "GITHUB_TOKEN" {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "GH_PROMPT_DISABLED=1", "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	proc.HideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		slog.Debug("gh auth token lookup failed", "error", err)
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Installation token of the GitHub App the settings name, empty when they name none or it can't be had
func (a *auth) app(ctx context.Context) string {
	id, keyPath := strings.TrimSpace(a.cfg.GetString(hubconfig.GitHubAppID)), strings.TrimSpace(a.cfg.GetString(hubconfig.GitHubAppKey))
	installation := strings.TrimSpace(a.cfg.GetString(hubconfig.GitHubAppInstallation))
	if id == "" || keyPath == "" || installation == "" || !fsx.Exists(keyPath) {
		return ""
	}
	token, err := a.mint(ctx, id, keyPath, installation)
	if err != nil {
		slog.Debug("GitHub App auth failed", "error", err)
		return ""
	}
	return token
}

// Signs a ten minute JWT as the App and trades it for a token of the installation
func (a *auth) mint(ctx context.Context, id, keyPath, installation string) (string, error) {
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(raw) {
		return "", fmt.Errorf("private key file %s is not valid UTF-8", keyPath)
	}
	block, _ := pem.Decode(bytes.TrimPrefix(raw, []byte("\xEF\xBB\xBF")))
	if block == nil {
		return "", fmt.Errorf("private key file %s holds no PEM key", keyPath)
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		parsed, err8 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err8 != nil {
			return "", err
		}
		var ok bool
		if key, ok = parsed.(*rsa.PrivateKey); !ok {
			return "", fmt.Errorf("private key file %s holds no RSA key", keyPath)
		}
	}
	now := time.Now().Unix()
	claims, err := json.Marshal(map[string]any{"iat": now - 60, "exp": now + 600, "iss": id})
	if err != nil {
		return "", err
	}
	encode := base64.RawURLEncoding.EncodeToString
	signing := encode([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + encode(claims)
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	endpoint := fmt.Sprintf("https://api.github.com/app/installations/%s/access_tokens", url.PathEscape(installation))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+signing+"."+encode(signature))
	req.Header.Set("Accept", acceptJSON)
	resp, err := a.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("GitHub answered %d minting the installation token", resp.StatusCode)
	}
	var answer struct {
		Token string `json:"token"`
	}
	return answer.Token, json.NewDecoder(resp.Body).Decode(&answer)
}

// The GitHub API as one operation of the hub reads it: its credentials, and the trees and groupings of the repositories it looked at
type github struct {
	r    *run
	auth *auth

	mu sync.Mutex
	// Default branch and entries of each repository, nil for one that could not be read whole
	trees map[string]*tree
	// Tree each repository's default branch was at when read
	revisions map[string]string
	// Category of each skill by its name, from the skills.sh.json of each repository; nil for none
	groupings map[string]map[string]string
	// Whether the anonymous rate limit ran out
	limited bool
	// Whether raw.githubusercontent.com could not be reached, so the contents API is asked instead
	rawUnusable bool
}

type tree struct {
	branch  string
	entries []treeEntry
}

type treeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Mode string `json:"mode"`
}

// Whether the entry is a regular file, not a link
func (e treeEntry) regular() bool {
	return e.Type == "blob" && e.Mode != "120000"
}

func newGitHub(r *run) *github {
	return &github{r: r, auth: &auth{cfg: r.cfg, client: r.direct, refused: map[string]bool{}, refusedMethod: map[string]bool{}},
		trees: map[string]*tree{}, revisions: map[string]string{}, groupings: map[string]map[string]string{}}
}

func (g *github) rateLimited() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.limited
}

// Waits, false when the context ends first
func pause(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// GET of the API, retried with backoff on a rate limit, a server error or a failed connection; a 401 retried once with the next credential
func (g *github) get(ctx context.Context, raw string, query url.Values, header map[string]string, timeout time.Duration) *response {
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	if header == nil {
		header = g.auth.headers(ctx)
	}
	const attempts = 3
	backoff := time.Second
	var last *response
	for attempt := range attempts {
		final := attempt == attempts-1
		wait := backoff
		resp, err := g.r.get(ctx, raw, request{query: query, header: header, timeout: timeout, follow: true})
		switch {
		case err != nil:
			slog.Debug(fmt.Sprintf("GitHub GET %s failed (attempt %d/%d): %s", raw, attempt+1, attempts, err))
			if final {
				return nil
			}
		case resp.status == http.StatusOK:
			return resp
		case resp.status == http.StatusUnauthorized && header["Authorization"] != "":
			sent := header["Authorization"]
			g.auth.reject(sent[strings.Index(sent, " ")+1:])
			retry := map[string]string{}
			for k, v := range header {
				if k != "Authorization" {
					retry[k] = v
				}
			}
			if value, ok := g.auth.headers(ctx)["Authorization"]; ok {
				retry["Authorization"] = value
			}
			return g.get(ctx, raw, query, retry, timeout)
		case resp.status == http.StatusForbidden || resp.status == http.StatusTooManyRequests:
			last = resp
			limited := resp.status == http.StatusTooManyRequests || resp.header.Get("X-RateLimit-Remaining") == "0"
			if !limited || final {
				if limited {
					g.mu.Lock()
					g.limited = true
					g.mu.Unlock()
					slog.Warn("GitHub API rate limit exhausted (unauthenticated: 60 req/hr). Set GITHUB_TOKEN or install the gh CLI to raise the limit to 5,000/hr.")
				}
				return resp
			}
			if after, ok := httpx.RetryAfter(resp.header.Get("Retry-After")); ok {
				wait = min(after, time.Minute)
			} else if reset, err := strconv.ParseInt(resp.header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
				if delta := time.Until(time.Unix(reset, 0)); delta > 0 && delta <= time.Minute {
					wait = delta
				}
			}
			slog.Debug(fmt.Sprintf("GitHub rate limited on %s, waiting %.1fs (attempt %d/%d)", raw, wait.Seconds(), attempt+1, attempts))
		default:
			last = resp
			if resp.status < 500 || resp.status >= 600 || final {
				return resp
			}
		}
		if !pause(ctx, wait) {
			return last
		}
		backoff = min(backoff*2, 30*time.Second)
	}
	return last
}

// Answer of the API read as JSON into v, false on any other status or when it is not that JSON
func (g *github) json(ctx context.Context, raw string, query url.Values, timeout time.Duration, v any) bool {
	resp := g.get(ctx, raw, query, nil, timeout)
	return resp.ok() && json.Unmarshal(resp.body, v) == nil
}

// Default branch and entries of a repository, read once per operation; nil when it can't be read or GitHub cut its tree short
func (g *github) tree(ctx context.Context, repo string) *tree {
	g.mu.Lock()
	t, seen := g.trees[repo]
	g.mu.Unlock()
	if seen {
		return t
	}
	t = g.readTree(ctx, repo)
	g.mu.Lock()
	g.trees[repo] = t
	g.mu.Unlock()
	return t
}

func (g *github) readTree(ctx context.Context, repo string) *tree {
	var info struct {
		DefaultBranch string `json:"default_branch"`
	}
	if !g.json(ctx, githubAPI+"/"+repo, nil, 0, &info) {
		return nil
	}
	branch := cmp.Or(info.DefaultBranch, "main")
	var data struct {
		SHA       string      `json:"sha"`
		Truncated bool        `json:"truncated"`
		Tree      []treeEntry `json:"tree"`
	}
	if !g.json(ctx, githubAPI+"/"+repo+"/git/trees/"+branch, url.Values{"recursive": {"1"}}, 30*time.Second, &data) {
		return nil
	}
	if data.Truncated {
		slog.Debug("Git tree truncated for " + repo)
		return nil
	}
	if data.SHA != "" {
		g.mu.Lock()
		g.revisions[repo] = data.SHA
		g.mu.Unlock()
	}
	return &tree{branch: branch, entries: data.Tree}
}

// Tree the repository's default branch was at when its tree was read, empty when it wasn't
func (g *github) revision(ctx context.Context, repo string) string {
	g.tree(ctx, repo)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.revisions[repo]
}

// Bytes of one file of a repository: from raw.githubusercontent.com at a pinned revision when it answers, else from the contents API
func (g *github) bytes(ctx context.Context, repo, path, ref string) ([]byte, bool) {
	g.mu.Lock()
	raw := ref != "" && !g.rawUnusable
	g.mu.Unlock()
	if raw {
		resp, err := g.r.get(ctx, "https://raw.githubusercontent.com/"+repo+"/"+ref+"/"+escapePath(path), request{timeout: 15 * time.Second})
		switch {
		case err != nil:
			g.mu.Lock()
			g.rawUnusable = true
			g.mu.Unlock()
		case resp.ok():
			return resp.body, true
		}
	}
	header := g.auth.headers(ctx)
	header["Accept"] = "application/vnd.github.v3.raw"
	var query url.Values
	if ref != "" {
		query = url.Values{"ref": {ref}}
	}
	resp := g.get(ctx, githubAPI+"/"+repo+"/contents/"+escapePath(path), query, header, 0)
	if !resp.ok() {
		return nil, false
	}
	return resp.body, true
}

// Text of one file of a repository, false when it is missing or not UTF-8
func (g *github) text(ctx context.Context, repo, path, ref string) (string, bool) {
	data, ok := g.bytes(ctx, repo, path, ref)
	if !ok || !utf8.Valid(data) {
		return "", false
	}
	return string(data), true
}

// Path with each part escaped for a URL, the slashes kept
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

// Category of each skill a repository's skills.sh.json groups, the first grouping winning; nil when it has none
func (g *github) grouped(ctx context.Context, repo string) map[string]string {
	g.mu.Lock()
	m, seen := g.groupings[repo]
	g.mu.Unlock()
	if seen {
		return m
	}
	if content, ok := g.text(ctx, repo, "skills.sh.json", ""); ok && content != "" {
		var doc struct {
			Groupings []json.RawMessage `json:"groupings"`
		}
		if json.Unmarshal([]byte(content), &doc) == nil && doc.Groupings != nil {
			m = map[string]string{}
			for _, raw := range doc.Groupings {
				var group struct {
					Title  *string           `json:"title"`
					Skills []json.RawMessage `json:"skills"`
				}
				if json.Unmarshal(raw, &group) != nil || group.Title == nil || group.Skills == nil {
					continue
				}
				for _, member := range group.Skills {
					var name string
					if json.Unmarshal(member, &name) == nil && name != "" {
						if _, taken := m[name]; !taken {
							m[name] = *group.Title
						}
					}
				}
			}
		}
	}
	g.mu.Lock()
	g.groupings[repo] = m
	g.mu.Unlock()
	return m
}

// Key of a tap's listing in the index cache
func tapKey(repo, path, bucket string) string {
	return strings.NewReplacer("/", "_", " ", "_").Replace(repo + "_" + path + "_" + bucket)
}

// Skills in the folders of a repository's path, each read from its SKILL.md and given its category; cached
func (g *github) list(ctx context.Context, repo, path, bucket string) []Meta {
	key := tapKey(repo, path, bucket)
	var cached []Meta
	if g.r.cached(key, cacheTTL, &cached) {
		return cached
	}
	prefix := strings.TrimRight(path, "/")
	var entries []struct {
		Type web.Text `json:"type"`
		Name web.Text `json:"name"`
	}
	if !g.json(ctx, githubAPI+"/"+repo+"/contents/"+prefix, nil, 0, &entries) {
		return nil
	}
	groups := g.grouped(ctx, repo)
	out := []Meta{}
	for _, entry := range entries {
		name := string(entry.Name)
		if entry.Type != "dir" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		id := repo + "/" + name
		if prefix != "" {
			id = repo + "/" + prefix + "/" + name
		}
		meta := g.inspect(ctx, id)
		if meta == nil {
			continue
		}
		category := cmp.Or(groups[meta.Name], groups[name], bucket)
		if category != "" {
			if meta.Extra == nil {
				meta.Extra = map[string]any{}
			}
			meta.Extra["category"] = category
		}
		out = append(out, *meta)
	}
	g.r.keep(key, out)
	return out
}

// What a skill's SKILL.md says of it, nil when it has none
func (g *github) inspect(ctx context.Context, identifier string) *Meta {
	repo, path, ok := splitRepo(identifier)
	if !ok {
		return nil
	}
	path = strings.TrimRight(path, "/")
	content, ok := g.text(ctx, repo, within(path, "SKILL.md"), "")
	if !ok || content == "" {
		return nil
	}
	fm, _ := skills.Parse(content)
	tags := tagsOf(fm.Meta()["tags"])
	if len(tags) == 0 {
		if list, isList := fm["tags"].([]any); isList {
			tags = tagsOf(list)
		}
	}
	name := path[strings.LastIndex(path, "/")+1:]
	if name == "" {
		name = repo[strings.LastIndex(repo, "/")+1:]
	}
	if v, ok := fm["name"]; ok && v != nil {
		name = fmt.Sprint(v)
	}
	description := ""
	if v, ok := fm["description"]; ok && v != nil {
		description = fmt.Sprint(v)
	}
	extra := map[string]any{}
	if provider := providerFor(repo); provider != "" {
		extra["provider"] = provider
	}
	return &Meta{Name: name, Description: description, Source: "github", Identifier: identifier, Trust: githubTrust(identifier), Repo: repo,
		Path: path, Tags: tags, Extra: extra}
}

// Texts of a YAML list, empty for anything else
func tagsOf(v any) []string {
	list, _ := v.([]any)
	out := []string{}
	for _, item := range list {
		out = append(out, fmt.Sprint(item))
	}
	return out
}

// A skill's folder at the tree its default branch was read at: SKILL.md, then every file of the folder, or the files SKILL.md names when
// the tree can't be read; nil when SKILL.md is missing or the folder holds an unsafe path or a linked file it names
func (g *github) fetch(ctx context.Context, identifier string) *Bundle {
	repo, path, ok := splitRepo(identifier)
	if !ok {
		return nil
	}
	dir := strings.TrimRight(path, "/")
	t := g.tree(ctx, repo)
	g.mu.Lock()
	pinned := g.revisions[repo]
	g.mu.Unlock()
	content, ok := g.text(ctx, repo, within(dir, "SKILL.md"), pinned)
	if !ok {
		return nil
	}
	named, ok := referenced(content)
	if !ok {
		return nil
	}
	bundle := &Bundle{Files: []File{{Path: "SKILL.md", Data: []byte(content)}}}
	revision := ""
	if t != nil {
		complete, ok := g.collect(ctx, repo, dir, t.entries, pinned, named, bundle)
		if !ok {
			return nil
		}
		if complete {
			revision = cmp.Or(pinned, t.branch)
		}
	} else {
		for _, rel := range slices.Sorted(maps.Keys(named)) {
			g.support(ctx, repo, within(dir, rel), rel, bundle, "")
		}
	}
	sourceURL := "https://github.com/" + repo + "/" + path
	if revision != "" {
		sourceURL = "https://github.com/" + repo + "/tree/" + revision
		if path != "" {
			sourceURL += "/" + path
		}
	}
	name := dir[strings.LastIndex(dir, "/")+1:]
	if name == "" {
		name = repo[strings.LastIndex(repo, "/")+1:]
	}
	bundle.Name, bundle.Source, bundle.Identifier, bundle.Trust = name, "github", identifier, githubTrust(identifier)
	bundle.Metadata = map[string]any{"source_url": sourceURL, "source_revision": revision}
	return bundle
}

// Adds one supporting file to the bundle, false with a warning when it can't be fetched
func (g *github) support(ctx context.Context, repo, path, rel string, bundle *Bundle, ref string) bool {
	data, ok := g.bytes(ctx, repo, path, ref)
	if !ok {
		slog.Warn("Failed to fetch referenced skill support file; continuing without it: " + path)
		return false
	}
	bundle.set(rel, data)
	return true
}

// Every file of the skill's folder in the tree into the bundle; complete false when one could not be fetched, ok false for an unsafe path
// or a file SKILL.md names that is a link
func (g *github) collect(ctx context.Context, repo, dir string, entries []treeEntry, ref string, named map[string]bool, bundle *Bundle) (bool, bool) {
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	linked := map[string]bool{}
	complete := true
	for _, entry := range entries {
		rel, ok := strings.CutPrefix(entry.Path, prefix)
		if !ok {
			continue
		}
		if !entry.regular() {
			linked[rel] = true
			continue
		}
		if rel == "SKILL.md" || skipped(rel) {
			continue
		}
		safe, err := bundlePath(rel)
		if err != nil {
			slog.Warn("Rejected unsafe file path in skill bundle: " + entry.Path)
			return false, false
		}
		complete = g.support(ctx, repo, entry.Path, safe, bundle, ref) && complete
	}
	for _, rel := range slices.Sorted(maps.Keys(named)) {
		if linked[rel] {
			slog.Warn("Rejected non-regular referenced file in skill bundle: " + prefix + rel)
			return false, false
		}
		if _, ok := bundle.file(rel); !ok {
			slog.Warn("Referenced skill support file is missing; continuing without it: " + prefix + rel)
		}
	}
	return complete, true
}

// Identifier of the skill of the name anywhere in a repository's tree, or of its one skills folder in a usual place
func (g *github) findSkill(ctx context.Context, repo, name string) string {
	t := g.tree(ctx, repo)
	if t == nil {
		return ""
	}
	suffix := "/" + name + "/SKILL.md"
	var dirs []string
	for _, entry := range t.entries {
		if !entry.regular() || !strings.HasSuffix("/"+entry.Path, "/SKILL.md") {
			continue
		}
		if strings.HasSuffix(entry.Path, suffix) || entry.Path == suffix[1:] {
			return repo + "/" + strings.TrimSuffix(entry.Path, "/SKILL.md")
		}
		dirs = append(dirs, strings.TrimRight(strings.TrimSuffix(entry.Path, "SKILL.md"), "/"))
	}
	if len(dirs) == 1 && slices.Contains([]string{"skills", ".agents/skills", ".claude/skills"}, dirs[0]) {
		return repo + "/" + dirs[0]
	}
	return ""
}

// Identifier of a repository that is one skill, its only SKILL.md at its root
func (g *github) rootSkill(ctx context.Context, repo string) string {
	t := g.tree(ctx, repo)
	if t == nil {
		return ""
	}
	var found []string
	for _, entry := range t.entries {
		if entry.regular() && (entry.Path == "SKILL.md" || strings.HasSuffix(entry.Path, "/SKILL.md")) {
			found = append(found, entry.Path)
		}
	}
	if len(found) == 1 && found[0] == "SKILL.md" {
		return repo + "/"
	}
	return ""
}

// The GitHub repositories of skills: the hub's default ones and the user's taps
type githubSource struct {
	r    *run
	taps []tap
}

func (g *githubSource) ID() string { return "github" }

func (g *githubSource) Search(ctx context.Context, query string, limit int, provider string) []Meta {
	q := strings.ToLower(query)
	var results []Meta
	for _, t := range g.taps {
		if provider != "" && strings.ToLower(providerFor(t.Repo)) != provider {
			continue
		}
		for _, skill := range g.r.gh.list(ctx, t.Repo, t.Path, t.Bucket) {
			if matches(q, skill.Name, skill.Description, skill.Tags) {
				results = append(results, skill)
			}
		}
	}
	return head(dedupe(results), limit)
}

func (g *githubSource) Fetch(ctx context.Context, identifier string) *Bundle {
	return g.r.gh.fetch(ctx, identifier)
}

func (g *githubSource) Inspect(ctx context.Context, identifier string) *Meta {
	return g.r.gh.inspect(ctx, identifier)
}

func (g *githubSource) Revision(ctx context.Context, identifier string) string {
	repo, _, ok := splitRepo(identifier)
	if !ok {
		return ""
	}
	return g.r.gh.revision(ctx, repo)
}
