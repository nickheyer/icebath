package install

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http/httpproxy"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Repository the installer downloads releases from unless ICEBATH_REPO_URL names another
const DefaultRepoURL = "https://github.com/nickheyer/icebath.git"

const officialRepository = "nickheyer/icebath"

var (
	stableTagRE   = regexp.MustCompile(`^v(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	fullSHARE     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	scpRepoRE     = regexp.MustCompile(`^(?:ssh://)?git@([^:/]+)[:/]([^/]+)/([^/]+?)(?:\.git)?/?$`)
	locationTagRE = regexp.MustCompile(`/releases/tag/([^/]+)$`)
	checksumRE    = regexp.MustCompile(`^([0-9a-fA-F]{64})\s+\*?(\S.*)$`)
)

// Reports a vX.Y.Z tag of the stable shape
func ValidStableTag(tag string) bool { return stableTagRE.MatchString(tag) }

// Reports a 40 hex digit lowercase commit
func IsFullSHA(value string) bool { return fullSHARE.MatchString(value) }

type Repo struct {
	Web      string
	API      string
	Owner    string
	Name     string
	Official bool
}

// Web, API and identity of a repository URL in https or ssh form
func ParseRepo(raw string) (Repo, error) {
	raw = pystr.Strip(raw)
	var scheme, host, owner, name string
	if m := scpRepoRE.FindStringSubmatch(raw); m != nil {
		scheme, host, owner, name = "https", m[1], m[2], m[3]
	} else {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return Repo{}, fmt.Errorf("unsupported repository URL %q", raw)
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return Repo{}, fmt.Errorf("repository URL %q does not name owner/repo", raw)
		}
		scheme, host, owner, name = u.Scheme, u.Host, parts[0], strings.TrimSuffix(parts[1], ".git")
	}
	r := Repo{Web: scheme + "://" + host + "/" + owner + "/" + name, Owner: owner, Name: name}
	if strings.EqualFold(host, "github.com") {
		r.API = "https://api.github.com/repos/" + owner + "/" + name
		r.Official = strings.EqualFold(owner+"/"+name, officialRepository)
	} else {
		r.API = scheme + "://" + host + "/api/v3/repos/" + owner + "/" + name
	}
	return r, nil
}

type releaseAsset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"`
}

type releaseInfo struct {
	Tag        string         `json:"tag_name"`
	Target     string         `json:"target_commitish"`
	Draft      bool           `json:"draft"`
	Prerelease bool           `json:"prerelease"`
	Published  string         `json:"published_at"`
	Assets     []releaseAsset `json:"assets"`
}

// Release chosen for an install, its commit when known and its assets when the API answered
type ReleaseTarget struct {
	Tag    string
	Commit string
	Assets []releaseAsset
	Repo   Repo
}

// Download URL of a named asset, the API listing first then the conventional release path
func (t *ReleaseTarget) AssetURL(name string) string {
	for _, a := range t.Assets {
		if a.Name == name && a.URL != "" {
			return a.URL
		}
	}
	return t.Repo.Web + "/releases/download/" + url.PathEscape(t.Tag) + "/" + url.PathEscape(name)
}

// sha256 an API asset digest names, empty when the listing carries none
func (t *ReleaseTarget) AssetDigest(name string) string {
	for _, a := range t.Assets {
		if a.Name == name {
			if hexDigest, ok := strings.CutPrefix(a.Digest, "sha256:"); ok && len(hexDigest) == 64 {
				if _, err := hex.DecodeString(hexDigest); err == nil {
					return strings.ToLower(hexDigest)
				}
			}
		}
	}
	return ""
}

// Release binary name for goos and goarch
func BinaryAssetName(goos, goarch string) string {
	name := "icebath-" + goos + "-" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// Client honouring the proxy environment with header and whole request bounds
func releaseHTTPClient(follow bool) *http.Client {
	proxy := httpproxy.FromEnvironment().ProxyFunc()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = func(r *http.Request) (*url.URL, error) { return proxy(r.URL) }
	transport.ResponseHeaderTimeout = 20 * time.Second
	c := &http.Client{Timeout: 60 * time.Second, Transport: transport}
	if !follow {
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	return c
}

// Token release API requests carry, ICEBATH_GITHUB_TOKEN before GITHUB_TOKEN
func githubToken() string {
	for _, name := range []string{"ICEBATH_GITHUB_TOKEN", "GITHUB_TOKEN"} {
		if token := pystr.Strip(os.Getenv(name)); token != "" {
			return token
		}
	}
	return ""
}

// Body of a successful GET, the status as an error otherwise
func httpGet(ctx context.Context, target string, api bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "icebath-installer")
	if api {
		req.Header.Set("Accept", "application/vnd.github+json")
		if token := githubToken(); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	resp, err := releaseHTTPClient(true).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, target)
	}
	return body, nil
}

// One release by API path suffix
func fetchRelease(ctx context.Context, r Repo, suffix string) (*releaseInfo, error) {
	body, err := httpGet(ctx, r.API+suffix, true)
	if err != nil {
		return nil, err
	}
	var rel releaseInfo
	if err := json.Unmarshal(body, &rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

// Every listed release, newest pages first, up to ten pages of one hundred
func listReleases(ctx context.Context, r Repo) ([]releaseInfo, error) {
	var all []releaseInfo
	for page := 1; page <= 10; page++ {
		body, err := httpGet(ctx, r.API+"/releases?per_page=100&page="+strconv.Itoa(page), true)
		if err != nil {
			return nil, err
		}
		var batch []releaseInfo
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			break
		}
	}
	return all, nil
}

// Tag of the releases/latest redirect, empty when it does not answer with one
func latestRedirectTag(ctx context.Context, r Repo) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, r.Web+"/releases/latest", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "icebath-installer")
	resp, err := releaseHTTPClient(false).Do(req)
	if err != nil {
		return ""
	}
	resp.Body.Close()
	if m := locationTagRE.FindStringSubmatch(resp.Header.Get("Location")); m != nil {
		if tag, err := url.PathUnescape(m[1]); err == nil {
			return tag
		}
	}
	return ""
}

// Refs the repository advertises over smart HTTP, ref name to object id
func LsRemote(ctx context.Context, r Repo) (map[string]string, error) {
	body, err := httpGet(ctx, r.Web+".git/info/refs?service=git-upload-pack", false)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for len(body) >= 4 {
		n, err := strconv.ParseUint(string(body[:4]), 16, 16)
		if err != nil {
			return nil, fmt.Errorf("malformed ref advertisement from %s", r.Web)
		}
		if n == 0 {
			body = body[4:]
			continue
		}
		if int(n) > len(body) || n < 4 {
			return nil, fmt.Errorf("truncated ref advertisement from %s", r.Web)
		}
		line := strings.TrimRight(string(body[4:n]), "\n")
		body = body[n:]
		if i := strings.IndexByte(line, 0); i >= 0 {
			line = line[:i]
		}
		if strings.HasPrefix(line, "# service=") {
			continue
		}
		sha, ref, ok := strings.Cut(line, " ")
		if ok && IsFullSHA(sha) {
			refs[ref] = sha
		}
	}
	return refs, nil
}

// Commit a tag points at, the peeled object first
func tagCommit(refs map[string]string, tag string) string {
	if sha := refs["refs/tags/"+tag+"^{}"]; sha != "" {
		return sha
	}
	return refs["refs/tags/"+tag]
}

// Numeric components of a vX.Y.Z tag for ordering
func stableParts(tag string) [3]uint64 {
	var out [3]uint64
	if m := stableTagRE.FindStringSubmatch(tag); m != nil {
		for i := 0; i < 3; i++ {
			out[i], _ = strconv.ParseUint(m[i+1], 10, 64)
		}
	}
	return out
}

// Latest stable release tag, the API first, then the latest redirect, then the highest advertised vX.Y.Z tag
func LatestStableRelease(ctx context.Context, r Repo) string {
	if rel, err := fetchRelease(ctx, r, "/releases/latest"); err == nil && ValidStableTag(rel.Tag) {
		return rel.Tag
	}
	if tag := latestRedirectTag(ctx, r); ValidStableTag(tag) {
		return tag
	}
	refs, err := LsRemote(ctx, r)
	if err != nil {
		return ""
	}
	var tags []string
	for ref := range refs {
		if tag, ok := strings.CutPrefix(ref, "refs/tags/"); ok && ValidStableTag(tag) {
			tags = append(tags, tag)
		}
	}
	sort.Slice(tags, func(i, j int) bool {
		a, b := stableParts(tags[i]), stableParts(tags[j])
		for k := 0; k < 3; k++ {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
		}
		return false
	})
	if len(tags) == 0 {
		return ""
	}
	return tags[len(tags)-1]
}

// Release target of a tag with its listed assets and commit, the conventional paths when the API is silent
func targetForTag(ctx context.Context, r Repo, tag string) *ReleaseTarget {
	t := &ReleaseTarget{Tag: tag, Repo: r}
	if rel, err := fetchRelease(ctx, r, "/releases/tags/"+url.PathEscape(tag)); err == nil {
		t.Assets = rel.Assets
		if IsFullSHA(rel.Target) {
			t.Commit = rel.Target
		}
	}
	if t.Commit == "" {
		if refs, err := LsRemote(ctx, r); err == nil {
			t.Commit = tagCommit(refs, tag)
		}
	}
	return t
}

// Newest published release whose build target or tag commit is the pinned commit
func releaseForCommit(ctx context.Context, r Repo, commit string) (*ReleaseTarget, error) {
	releases, err := listReleases(ctx, r)
	if err != nil {
		return nil, err
	}
	refs, _ := LsRemote(ctx, r)
	for _, rel := range newestFirst(releases) {
		if rel.Draft {
			continue
		}
		if rel.Target == commit || (refs != nil && tagCommit(refs, rel.Tag) == commit) {
			return &ReleaseTarget{Tag: rel.Tag, Commit: commit, Assets: rel.Assets, Repo: r}, nil
		}
	}
	return nil, nil
}

// Newest published release built from the branch
func releaseForBranch(ctx context.Context, r Repo, branch string) (*ReleaseTarget, error) {
	releases, err := listReleases(ctx, r)
	if err != nil {
		return nil, err
	}
	for _, rel := range newestFirst(releases) {
		if rel.Draft || rel.Target != branch {
			continue
		}
		t := &ReleaseTarget{Tag: rel.Tag, Assets: rel.Assets, Repo: r}
		if refs, err := LsRemote(ctx, r); err == nil {
			t.Commit = tagCommit(refs, rel.Tag)
		}
		return t, nil
	}
	return nil, nil
}

// Releases ordered by publication time, newest first
func newestFirst(releases []releaseInfo) []releaseInfo {
	out := append([]releaseInfo(nil), releases...)
	sort.SliceStable(out, func(i, j int) bool {
		a, _ := time.Parse(time.RFC3339, out[i].Published)
		b, _ := time.Parse(time.RFC3339, out[j].Published)
		return a.After(b)
	})
	return out
}

// sha256 of name in a checksums.txt body, empty when it is not listed
func ChecksumFor(body []byte, name string) string {
	sc := bufio.NewScanner(strings.NewReader(string(body)))
	for sc.Scan() {
		if m := checksumRE.FindStringSubmatch(strings.TrimSpace(sc.Text())); m != nil && m[2] == name {
			return strings.ToLower(m[1])
		}
	}
	return ""
}

// Verified sha256 of a release asset from checksums.txt, refusing a disagreeing API digest
func (t *ReleaseTarget) ExpectedSHA256(ctx context.Context, name string) (string, error) {
	body, err := httpGet(ctx, t.AssetURL("checksums.txt"), false)
	if err != nil {
		return "", fmt.Errorf("could not fetch checksums.txt for %s: %w", t.Tag, err)
	}
	sum := ChecksumFor(body, name)
	if sum == "" {
		return "", fmt.Errorf("release %s lists no checksum for %s", t.Tag, name)
	}
	if digest := t.AssetDigest(name); digest != "" && digest != sum {
		return "", errDigestMismatch{fmt.Sprintf("release %s checksums.txt (%s) and the published digest (%s) of %s disagree", t.Tag, sum, digest, name)}
	}
	return sum, nil
}

type errDigestMismatch struct{ msg string }

func (e errDigestMismatch) Error() string { return e.msg }

// Reports a checksum disagreement or a downloaded digest mismatch
func isDigestMismatch(err error) bool {
	var d errDigestMismatch
	if errors.As(err, &d) {
		return true
	}
	return pm.IsDownloadKind(err, pm.DownloadHashFailed)
}

// Host platform key of the release assets
func hostKey() (string, string) { return runtime.GOOS, runtime.GOARCH }
