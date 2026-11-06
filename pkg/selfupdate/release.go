package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http/httpproxy"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Bound on one release request, the NETWORK_GIT_TIMEOUT_SECONDS of the fetch it replaces
const NetworkFetchTimeout = 300 * time.Second

var (
	releaseAPIBase       = "https://api.github.com"
	releaseFetchTimeout  = NetworkFetchTimeout
	releaseHeaderTimeout = 30 * time.Second
)

type ReleaseAsset struct {
	Name        string
	Size        int64
	Digest      string
	DownloadURL string
}

type Release struct {
	Tag         string
	PublishedAt string
	HTMLURL     string
	Draft       bool
	Prerelease  bool
	Assets      []ReleaseAsset
}

// Asset published for goos and goarch, nil when the release carries none
func (r *Release) Asset(name string) *ReleaseAsset {
	for i := range r.Assets {
		if r.Assets[i].Name == name {
			return &r.Assets[i]
		}
	}
	return nil
}

// Name of the release binary built for the running platform
func hostAssetName() string {
	name := "icebath-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// Lowercase sha256 hex an asset digest field names, empty when it names another algorithm
func digestSHA256(digest string) string {
	if hexDigest, ok := strings.CutPrefix(digest, "sha256:"); ok && len(hexDigest) == 64 {
		if _, err := hex.DecodeString(hexDigest); err == nil {
			return strings.ToLower(hexDigest)
		}
	}
	return ""
}

type FetchError struct {
	URL    string
	Status int
	Detail string
}

func (e *FetchError) Error() string { return e.Detail }

type ReleaseTarget struct {
	RequestedChannel string
	Channel          string
	Repository       string
	Release          *Release
	Version          string
	Ahead            bool
}

// Reports whether resolution moved the subscription to another channel
func (t ReleaseTarget) Retired() bool { return t.RequestedChannel != t.Channel }

// Human label of the target release, the waiting notice when the running build is newer
func (t ReleaseTarget) Label() string {
	if t.Ahead {
		return t.Channel + " v" + t.Version + "; this checkout is newer and waits for the next release" +
			" (`icebath update --set-channel main` follows every commit)"
	}
	if t.Release == nil {
		return t.Channel
	}
	if asset := t.Release.Asset(hostAssetName()); asset != nil {
		if sum := digestSHA256(asset.Digest); sum != "" {
			return t.Channel + " v" + t.Version + " (" + sum[:12] + ")"
		}
	}
	return t.Channel + " v" + t.Version
}

// Token the release requests carry, ICEBATH_GITHUB_TOKEN before GITHUB_TOKEN
func githubToken() string {
	for _, name := range []string{"ICEBATH_GITHUB_TOKEN", "GITHUB_TOKEN"} {
		if token := pystr.Strip(os.Getenv(name)); token != "" {
			return token
		}
	}
	return ""
}

// Client honouring the proxy environment as it stands now, with header and whole request bounds
func releaseClient() *http.Client {
	proxy := httpproxy.FromEnvironment().ProxyFunc()
	return &http.Client{
		Timeout: releaseFetchTimeout,
		Transport: &http.Transport{
			Proxy:                 func(r *http.Request) (*url.URL, error) { return proxy(r.URL) },
			ResponseHeaderTimeout: releaseHeaderTimeout,
			TLSHandshakeTimeout:   releaseHeaderTimeout,
			DialContext:           (&net.Dialer{Timeout: releaseHeaderTimeout}).DialContext,
		},
	}
}

// Fetch failure text in the curl and git forms the classifier reads, naming a stall as a timed out fetch
func transportFailure(target string, err error) *FetchError {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return &FetchError{URL: target, Detail: fmt.Sprintf("fatal: release fetch of %s timed out after %d seconds", target, int(releaseFetchTimeout/time.Second))}
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return &FetchError{URL: target, Detail: fmt.Sprintf("fatal: unable to access '%s': Could not resolve host: %s", target, dnsErr.Name)}
	}
	return &FetchError{URL: target, Detail: fmt.Sprintf("fatal: unable to access '%s': %v", target, err)}
}

// Fetch failure text of an HTTP error status, the GitHub message line first and the status line after
func statusFailure(target string, status int, body []byte, authenticated bool) *FetchError {
	var lines []string
	if decoded, err := pyjson.LoadsBytes(body); err == nil {
		if obj, ok := decoded.(*pyjson.Object); ok {
			if message, ok := stampGet(obj, "message").(string); ok && message != "" {
				lines = append(lines, "remote: "+message)
			}
		}
	}
	if status == http.StatusUnauthorized {
		if authenticated {
			lines = append(lines, fmt.Sprintf("fatal: Authentication failed for '%s'", target))
		} else {
			lines = append(lines, "fatal: could not read Username for 'https://github.com': terminal prompts disabled")
		}
	}
	lines = append(lines, fmt.Sprintf("error: The requested URL returned error: %d (%s)", status, target))
	return &FetchError{URL: target, Status: status, Detail: strings.Join(lines, "\n")}
}

// GETs one GitHub API document, retrying a rejected token anonymously, and returns its body and Link header
func fetchAPI(ctx context.Context, target string) ([]byte, string, error) {
	token := githubToken()
	for {
		ctx, cancel := context.WithTimeout(ctx, releaseFetchTimeout)
		body, link, err := fetchOnce(ctx, target, token)
		cancel()
		var failure *FetchError
		if token != "" && errors.As(err, &failure) && failure.Status == http.StatusUnauthorized {
			token = ""
			continue
		}
		return body, link, err
	}
}

func fetchOnce(ctx context.Context, target, token string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "icebath-update")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := releaseClient().Do(req)
	if err != nil {
		return nil, "", transportFailure(target, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, "", transportFailure(target, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", statusFailure(target, resp.StatusCode, body, token != "")
	}
	return body, resp.Header.Get("Link"), nil
}

var nextLinkRE = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// Decodes one release object, false when the value is no object with a string tag
func releaseFromJSON(value any) (*Release, bool) {
	obj, ok := value.(*pyjson.Object)
	if !ok {
		return nil, false
	}
	tag, ok := stampGet(obj, "tag_name").(string)
	if !ok {
		return nil, false
	}
	draft, draftOK := stampGet(obj, "draft").(bool)
	prerelease, preOK := stampGet(obj, "prerelease").(bool)
	if !draftOK || !preOK {
		return nil, false
	}
	rel := &Release{Tag: tag, Draft: draft, Prerelease: prerelease}
	rel.PublishedAt, _ = stampGet(obj, "published_at").(string)
	rel.HTMLURL, _ = stampGet(obj, "html_url").(string)
	assets, _ := stampGet(obj, "assets").([]any)
	for _, item := range assets {
		entry, ok := item.(*pyjson.Object)
		if !ok {
			continue
		}
		var asset ReleaseAsset
		asset.Name, _ = stampGet(entry, "name").(string)
		asset.Digest, _ = stampGet(entry, "digest").(string)
		asset.DownloadURL, _ = stampGet(entry, "browser_download_url").(string)
		if size, ok := jsonInteger(stampGet(entry, "size")); ok {
			asset.Size = size
		}
		rel.Assets = append(rel.Assets, asset)
	}
	return rel, true
}

// Integer value of a decoded JSON number that fits an int64
func jsonInteger(value any) (int64, bool) {
	switch n := value.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		if n == float64(int64(n)) {
			return int64(n), true
		}
	case fmt.Stringer:
		parsed, err := strconv.ParseInt(n.String(), 10, 64)
		return parsed, err == nil
	}
	return 0, false
}

// Every release of repository across the API's pages, malformed entries dropped
func listReleases(ctx context.Context, repository string) ([]*Release, error) {
	next := releaseAPIBase + "/repos/" + repository + "/releases?per_page=100"
	var releases []*Release
	for next != "" {
		body, link, err := fetchAPI(ctx, next)
		if err != nil {
			return nil, err
		}
		decoded, err := pyjson.LoadsBytes(body)
		if err != nil {
			return nil, err
		}
		items, ok := decoded.([]any)
		if !ok {
			return nil, errors.New("GitHub release listing is not a list")
		}
		for _, item := range items {
			if rel, ok := releaseFromJSON(item); ok {
				releases = append(releases, rel)
			}
		}
		next = ""
		if match := nextLinkRE.FindStringSubmatch(link); match != nil {
			next = match[1]
		}
	}
	return releases, nil
}

// Release GitHub designates latest for repository, the stable promotion
func latestRelease(ctx context.Context, repository string) (*Release, error) {
	body, _, err := fetchAPI(ctx, releaseAPIBase+"/repos/"+repository+"/releases/latest")
	if err != nil {
		return nil, err
	}
	decoded, err := pyjson.LoadsBytes(body)
	if err != nil {
		return nil, err
	}
	rel, ok := releaseFromJSON(decoded)
	if !ok {
		return nil, errors.New("No published stable release could be verified: malformed release metadata")
	}
	return rel, nil
}

// Release published under one exact tag
func ReleaseByTag(ctx context.Context, repository, tag string) (*Release, error) {
	body, _, err := fetchAPI(ctx, releaseAPIBase+"/repos/"+repository+"/releases/tags/"+url.PathEscape(tag))
	if err != nil {
		return nil, err
	}
	decoded, err := pyjson.LoadsBytes(body)
	if err != nil {
		return nil, err
	}
	rel, ok := releaseFromJSON(decoded)
	if !ok || rel.Tag != tag || rel.Draft {
		return nil, fmt.Errorf("%s is not a published release", tag)
	}
	return rel, nil
}

// Reports whether rel is a published member of channel under the tag shape that channel publishes
func releaseInChannel(rel *Release, channel string) bool {
	if rel.Draft {
		return false
	}
	switch channel {
	case ChannelStable:
		return !rel.Prerelease && updaterecord.StableTagRE.MatchString(rel.Tag)
	case ChannelCanary:
		return rel.Prerelease && (rel.Tag == pystr.Strip(rel.Tag) && IsCanaryTag(rel.Tag) || namedChannelTag(rel.Tag, channel))
	case ChannelMain:
		return true
	}
	return namedChannelTag(rel.Tag, channel)
}

// Reports whether tag is vX.Y.Z-<channel>.N
func namedChannelTag(tag, channel string) bool {
	pattern := `^v(?:0|[1-9]\p{Nd}{0,2})\.(?:0|[1-9]\p{Nd}*)\.(?:0|[1-9]\p{Nd}*)-` + regexp.QuoteMeta(channel) + `\.(?:0|[1-9][0-9]*)$`
	return regexp.MustCompile(pattern).MatchString(tag)
}

// Newest release under semver precedence, publication time breaking ties
func newestRelease(releases []*Release) *Release {
	sorted := append([]*Release(nil), releases...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if c := CompareVersions(sorted[i].Tag, sorted[j].Tag); c != 0 {
			return c > 0
		}
		return parseRFC3339(sorted[i].PublishedAt).After(parseRFC3339(sorted[j].PublishedAt))
	})
	if len(sorted) == 0 {
		return nil
	}
	return sorted[0]
}

func parseRFC3339(text string) time.Time {
	parsed, _ := time.Parse(time.RFC3339, text)
	return parsed
}

// Release channel selects, stable from the latest promotion and every other channel from the listing
func ResolveChannelRelease(ctx context.Context, channel, repository string) (*Release, error) {
	if channel == ChannelStable {
		rel, err := latestRelease(ctx, repository)
		if err != nil {
			return nil, err
		}
		if !releaseInChannel(rel, channel) {
			return nil, fmt.Errorf("%s is not a published %s release", rel.Tag, channel)
		}
		return rel, nil
	}
	releases, err := listReleases(ctx, repository)
	if err != nil {
		return nil, err
	}
	var members []*Release
	for _, rel := range releases {
		if releaseInChannel(rel, channel) {
			members = append(members, rel)
		}
	}
	if rel := newestRelease(members); rel != nil {
		return rel, nil
	}
	return nil, fmt.Errorf("No build published for channel %s", channel)
}

// Resolves a subscription to its release, pinning an unchosen default to a newer running build
func ResolveReleaseTarget(ctx context.Context, channel string, forwardOnly bool) (ReleaseTarget, error) {
	if _, err := ValidateChannelName(channel); err != nil {
		return ReleaseTarget{}, err
	}
	rel, err := ResolveChannelRelease(ctx, channel, OfficialRepository)
	if err != nil {
		return ReleaseTarget{}, err
	}
	target := ReleaseTarget{RequestedChannel: channel, Channel: channel, Repository: OfficialRepository,
		Release: rel, Version: strings.TrimPrefix(rel.Tag, "v")}
	if forwardOnly && CompareVersions(updaterecord.BuildVersion(), rel.Tag) > 0 {
		target.Ahead = true
	}
	return target, nil
}

type semverParts struct {
	core [3]uint64
	pre  []string
}

var semverRE = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

func parseSemver(text string) (semverParts, bool) {
	match := semverRE.FindStringSubmatch(text)
	if match == nil {
		return semverParts{}, false
	}
	var parts semverParts
	for i := 0; i < 3; i++ {
		n, err := strconv.ParseUint(match[i+1], 10, 64)
		if err != nil {
			return semverParts{}, false
		}
		parts.core[i] = n
	}
	if match[4] != "" {
		parts.pre = strings.Split(match[4], ".")
	}
	return parts, true
}

// Semver precedence of two tags, build metadata ignored and unparseable versions ranked lowest
func CompareVersions(a, b string) int {
	pa, okA := parseSemver(a)
	pb, okB := parseSemver(b)
	switch {
	case !okA && !okB:
		return 0
	case !okA:
		return -1
	case !okB:
		return 1
	}
	for i := 0; i < 3; i++ {
		if pa.core[i] != pb.core[i] {
			if pa.core[i] < pb.core[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(pa.pre) == 0 && len(pb.pre) == 0:
		return 0
	case len(pa.pre) == 0:
		return 1
	case len(pb.pre) == 0:
		return -1
	}
	for i := 0; i < len(pa.pre) && i < len(pb.pre); i++ {
		if c := comparePrerelease(pa.pre[i], pb.pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(pa.pre) < len(pb.pre):
		return -1
	case len(pa.pre) > len(pb.pre):
		return 1
	}
	return 0
}

// Orders one prerelease identifier pair, numeric below alphanumeric
func comparePrerelease(a, b string) int {
	na, errA := strconv.ParseUint(a, 10, 64)
	nb, errB := strconv.ParseUint(b, 10, 64)
	switch {
	case errA == nil && errB == nil:
		if na == nb {
			return 0
		}
		if na < nb {
			return -1
		}
		return 1
	case errA == nil:
		return -1
	case errB == nil:
		return 1
	}
	return strings.Compare(a, b)
}

var fetchFailureRules = []struct {
	matches func(string) bool
	message string
}{
	{func(s string) bool {
		return hasHTTPCode(s, "429") || strings.Contains(pystr.Lower(s), "rate limit")
	},
		"✗ GitHub is rate limiting requests or having an outage (HTTP 429) — try again in 5 minutes."},
	{func(s string) bool { return hasHTTPCode(s, "500", "502", "503", "504") },
		"✗ GitHub appears to be having an outage — try again in a few minutes (https://www.githubstatus.com)."},
	{func(s string) bool {
		return strings.Contains(s, "Could not resolve host") || strings.Contains(s, "unable to access")
	}, "✗ Network error — cannot reach the remote repository."},
	{func(s string) bool {
		return strings.Contains(s, "could not read Username") || strings.Contains(s, "terminal prompts disabled")
	}, "✗ GitHub rejected the anonymous fetch (asked for a login) — this usually means a GitHub outage;" +
		" try again in a few minutes (https://www.githubstatus.com). If it persists, check" +
		" `git remote -v` points at a public repo."},
	{func(s string) bool { return strings.Contains(s, "Authentication failed") },
		"✗ Authentication failed — check your git credentials or SSH key."},
	{func(s string) bool {
		return strings.Contains(s, "Permission denied (publickey)") || strings.Contains(s, "Host key verification failed")
	}, "✗ SSH authentication failed — check your SSH key is added to GitHub, or switch" +
		" `origin` to HTTPS: `git remote set-url origin https://github.com/nickheyer/icebath.git`."},
}

// Reports whether a fetch failure text carries any of the HTTP status codes in curl or git form
func hasHTTPCode(detail string, codes ...string) bool {
	for _, code := range codes {
		if strings.Contains(detail, "HTTP "+code) || strings.Contains(detail, "returned error: "+code) {
			return true
		}
	}
	return false
}

// One line diagnosis of a release fetch failure text
func ClassifyFetchFailure(detail string) string {
	for _, rule := range fetchFailureRules {
		if rule.matches(detail) {
			return rule.message
		}
	}
	return "✗ Failed to fetch updates from origin."
}

// Prints the diagnosis and at most fifteen raw failure lines
func PrintFetchFailure(w io.Writer, detail string) {
	detail = pystr.Strip(detail)
	fmt.Fprintln(w, ClassifyFetchFailure(detail))
	if detail == "" {
		return
	}
	lines := pystr.Splitlines(detail, false)
	for i, line := range lines {
		if i == 15 {
			fmt.Fprintf(w, "  ... (%d more lines omitted)\n", len(lines)-15)
			break
		}
		fmt.Fprintf(w, "  %s\n", line)
	}
}

// Lowercase sha256 hex of the running install's binary
func LiveBinarySHA256(projectRoot string) (string, error) {
	file, err := os.Open(liveBinaryPath(projectRoot))
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func liveBinaryPath(projectRoot string) string {
	name := "icebath"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(rootOrRepo(projectRoot), name)
}

func rootOrRepo(projectRoot string) string {
	if projectRoot == "" {
		return pm.RepoRoot()
	}
	return projectRoot
}

type LocalBinaryChange struct {
	Release  *Release
	Expected string
	Actual   string
}

// Compares the live binary with the host asset of the release its build version names, nil when they match
func LocalBinaryChanges(ctx context.Context, projectRoot string) (*LocalBinaryChange, error) {
	tag := updaterecord.BuildVersion()
	if !releaseBuildRE.MatchString(tag) && !IsCanaryTag(tag) {
		return nil, fmt.Errorf("this build (%s) names no published release to verify against", tag)
	}
	rel, err := ReleaseByTag(ctx, OfficialRepository, tag)
	if err != nil {
		return nil, err
	}
	asset := rel.Asset(hostAssetName())
	if asset == nil || digestSHA256(asset.Digest) == "" {
		return nil, fmt.Errorf("release %s publishes no sha256 digest for %s", tag, hostAssetName())
	}
	actual, err := LiveBinarySHA256(projectRoot)
	if err != nil {
		return nil, err
	}
	expected := digestSHA256(asset.Digest)
	if actual == expected {
		return nil, nil
	}
	return &LocalBinaryChange{Release: rel, Expected: expected, Actual: actual}, nil
}
