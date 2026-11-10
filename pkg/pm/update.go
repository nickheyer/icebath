package pm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	xunicode "golang.org/x/text/encoding/unicode"
	"golang.org/x/text/encoding/unicode/utf32"
)

const (
	updateUserAgent = "icebath-pm"
	indexTimeout    = 60 * time.Second
	llamaBucket     = "ggml-org/install.sh"
)

var (
	llamaBucketAPI     = "https://huggingface.co/api/buckets/" + llamaBucket
	llamaBucketResolve = "https://huggingface.co/buckets/" + llamaBucket + "/resolve"
	versionPart        = regexp.MustCompile(`\p{Nd}+|[A-Za-z]+`)
	firstDigits        = regexp.MustCompile(`\p{Nd}+`)
	bucketTag          = regexp.MustCompile(`^(b\p{Nd}+)/`)
	nodeVersion        = regexp.MustCompile(`^\p{Nd}+\.\p{Nd}+\.\p{Nd}+$`)
	martinRiedlBuild   = regexp.MustCompile(`/download/(macos|linux)/(amd64|arm64)/(\p{Nd}+)_(\p{Nd}+\.\p{Nd}+\.\p{Nd}+)/ffmpeg\.zip`)
	btbnTag            = regexp.MustCompile(`^autobuild-(\p{Nd}{4})-(\p{Nd}{2})-\p{Nd}{2}-\p{Nd}{2}-\p{Nd}{2}$`)
	btbnAsset          = regexp.MustCompile(`^ffmpeg-n(\p{Nd}+\.\p{Nd}+\.\p{Nd}+)-.+-(win|linux)(64|arm64)-gpl-\p{Nd}+\.\p{Nd}+\.(?:zip|tar\.xz)$`)
	pbsTag             = regexp.MustCompile(`^[0-9]{8}$`)
)

var indexResponses struct {
	sync.Mutex
	current map[string]any
}

// Runs one resolve or pin operation sharing successful index reads only inside it
func ReuseIndexResponses[T any](operation func() (T, error)) (T, error) {
	indexResponses.Lock()
	previous := indexResponses.current
	indexResponses.current = map[string]any{}
	indexResponses.Unlock()
	defer func() {
		indexResponses.Lock()
		indexResponses.current = previous
		indexResponses.Unlock()
	}()
	return operation()
}

// Cached response of the current operation, ok false outside one or on a miss
func cachedIndexResponse(key string) (any, bool) {
	indexResponses.Lock()
	defer indexResponses.Unlock()
	if indexResponses.current == nil {
		return nil, false
	}
	v, ok := indexResponses.current[key]
	return v, ok
}

// Records a successful response for the rest of the current operation
func storeIndexResponse(key string, value any) {
	indexResponses.Lock()
	defer indexResponses.Unlock()
	if indexResponses.current != nil {
		indexResponses.current[key] = value
	}
}

// Integer a Unicode decimal digit run spells
func digitsValue(digits string) *big.Int {
	n := new(big.Int)
	ten := big.NewInt(10)
	for _, r := range digits {
		run := int64(0)
		for unicode.Is(unicode.Nd, r-rune(run)-1) {
			run++
		}
		n.Mul(n, ten)
		n.Add(n, big.NewInt(run%10))
	}
	return n
}

// versionKeyPart is one numeric or alphabetic segment of a version sort key
type versionKeyPart struct {
	alpha bool
	num   *big.Int
	text  string
}

// Sortable key of a version, numeric segments before alphabetic ones so a prerelease never beats its release
func VersionKey(version string) []versionKeyPart {
	var key []versionKeyPart
	for _, chunk := range strings.Split(strings.ReplaceAll(version, "-", "."), ".") {
		for _, m := range versionPart.FindAllString(chunk, -1) {
			if r, _ := utf8.DecodeRuneInString(m); unicode.Is(unicode.Nd, r) {
				key = append(key, versionKeyPart{num: digitsValue(m)})
			} else {
				key = append(key, versionKeyPart{alpha: true, text: m})
			}
		}
	}
	return key
}

// Orders two version keys the way Python compares their tuples
func CompareVersionKeys(a, b []versionKeyPart) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		x, y := a[i], b[i]
		if x.alpha != y.alpha {
			if !x.alpha {
				return -1
			}
			return 1
		}
		if c := strings.Compare(x.text, y.text); x.alpha && c != 0 {
			return c
		}
		if !x.alpha {
			if c := x.num.Cmp(y.num); c != 0 {
				return c
			}
		}
	}
	return len(a) - len(b)
}

// Orders two versions by their sort keys
func compareVersions(a, b string) int { return CompareVersionKeys(VersionKey(a), VersionKey(b)) }

// Minor is a major and minor pair, Ok false where Python returns None
type Minor struct {
	Major, Minor *big.Int
	Ok           bool
}

// Reports equal minors, two absent ones included
func (m Minor) Equal(o Minor) bool {
	if m.Ok != o.Ok {
		return false
	}
	return !m.Ok || (m.Major.Cmp(o.Major) == 0 && m.Minor.Cmp(o.Minor) == 0)
}

// Orders two present minors
func (m Minor) compare(o Minor) int {
	if c := m.Major.Cmp(o.Major); c != 0 {
		return c
	}
	return m.Minor.Cmp(o.Minor)
}

// Text key of a minor for set membership
func (m Minor) key() string { return m.Major.String() + "." + m.Minor.String() }

// Major and minor of a semver like string, absent when it has fewer than two numeric parts
func MinorOf(version string) Minor {
	var nums []*big.Int
	for _, m := range versionPart.FindAllString(version, -1) {
		if r, _ := utf8.DecodeRuneInString(m); unicode.Is(unicode.Nd, r) {
			nums = append(nums, digitsValue(m))
		}
	}
	if len(nums) < 2 {
		return Minor{}
	}
	return Minor{Major: nums[0], Minor: nums[1], Ok: true}
}

// Highest version whose major and minor match, ok false when none does
func BestInMinor(versions []string, minor Minor) (string, bool) {
	best, found := "", false
	for _, v := range versions {
		if MinorOf(v).Equal(minor) && (!found || compareVersions(v, best) > 0) {
			best, found = v, true
		}
	}
	return best, found
}

// TargetVersion is one target's exact version in a decision
type TargetVersion struct{ Target, Version string }

// TargetCandidates is one target's newest first candidate versions
type TargetCandidates struct {
	Target   string
	Versions []string
}

// Resolved is what pm update decided for one package, nil Locked and Version standing for None
type Resolved struct {
	Name            string
	Locked          *string
	Style           string
	Version         *string
	PerTarget       []TargetVersion
	Reason          string
	ArtifactUpdates map[string][]string
}

// Exact version a decision pins for a target
func (d *Resolved) TargetVersion(target string) (string, bool) {
	for _, tv := range d.PerTarget {
		if tv.Target == target {
			return tv.Version, true
		}
	}
	return "", false
}

// Reports whether the decision moves the lockfile
func (d *Resolved) Changed() bool {
	if d.Version == nil {
		return false
	}
	if d.Locked == nil {
		return true
	}
	if d.Style == "minor" {
		return len(d.ArtifactUpdates) > 0 || !MinorOf(*d.Version).Equal(MinorOf(*d.Locked))
	}
	return compareVersions(*d.Version, *d.Locked) > 0
}

// Pointer to a copy of a string
func strPtr(s string) *string { return &s }

// Intersects per target candidate lists into one decision, a target without candidates skipped
func ResolveBest(name string, latestByTarget []TargetCandidates, locked *string, style string) *Resolved {
	var present []TargetCandidates
	for _, tc := range latestByTarget {
		if len(tc.Versions) > 0 {
			present = append(present, tc)
		}
	}
	decision := &Resolved{Name: name, Locked: locked, Style: style, ArtifactUpdates: map[string][]string{}}
	if len(present) == 0 {
		decision.Reason = "no source"
		return decision
	}
	if style == "minor" {
		var common map[string]Minor
		for _, tc := range present {
			set := map[string]Minor{}
			for _, v := range tc.Versions {
				if m := MinorOf(v); m.Ok {
					set[m.key()] = m
				}
			}
			if common == nil {
				common = set
				continue
			}
			for k := range common {
				if _, ok := set[k]; !ok {
					delete(common, k)
				}
			}
		}
		if len(common) == 0 {
			decision.Reason = "no shared minor"
			return decision
		}
		var shared Minor
		for _, m := range common {
			if !shared.Ok || m.compare(shared) > 0 {
				shared = m
			}
		}
		for _, tc := range present {
			best, _ := BestInMinor(tc.Versions, shared)
			decision.PerTarget = append(decision.PerTarget, TargetVersion{tc.Target, best})
		}
		decision.Version = strPtr(shared.Major.String() + "." + shared.Minor.String())
		return decision
	}
	common := map[string]bool{}
	for _, v := range present[0].Versions {
		common[v] = true
	}
	for _, tc := range present[1:] {
		set := map[string]bool{}
		for _, v := range tc.Versions {
			set[v] = true
		}
		for v := range common {
			if !set[v] {
				delete(common, v)
			}
		}
	}
	if len(common) == 0 {
		decision.Reason = "no shared version"
		return decision
	}
	version, found := "", false
	for _, v := range present[0].Versions {
		if common[v] && (!found || compareVersions(v, version) > 0) {
			version, found = v, true
		}
	}
	decision.Version = strPtr(version)
	for _, tc := range present {
		decision.PerTarget = append(decision.PerTarget, TargetVersion{tc.Target, version})
	}
	return decision
}

// Artifact rows of a pinned value, a single row read as a one element list
func artifactRowList(v any) []any {
	if list, ok := v.([]any); ok {
		return list
	}
	return []any{v}
}

// URL of every artifact row, a KeyError for a row without one
func rowURLs(rows []any) ([]string, error) {
	urls := make([]string, 0, len(rows))
	for _, row := range rows {
		value, err := rowField(row, "url")
		if err != nil {
			return nil, err
		}
		urls = append(urls, pyrepr.Str(value))
	}
	return urls, nil
}

// Field of a decoded artifact row the way subscripting reads it
func rowField(row any, key string) (any, error) {
	m, ok := row.(map[string]any)
	if !ok {
		return nil, pySubscriptError(row, key)
	}
	v, ok := m[key]
	if !ok {
		return nil, &PyError{Class: "KeyError", Message: pyrepr.StrRepr(key)}
	}
	return v, nil
}

// Reports equal string lists
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Resolves a package's latest versions and detects new artifacts within a shared minor
func ResolvePackage(p Package, targets []string, locked *string, artifacts map[string]any) (*Resolved, error) {
	return ReuseIndexResponses(func() (*Resolved, error) {
		lockedText := ""
		if locked != nil {
			lockedText = *locked
		}
		latest := make([]TargetCandidates, 0, len(targets))
		for _, t := range targets {
			versions, err := p.LatestVersions(t, lockedText)
			if err != nil {
				return nil, err
			}
			latest = append(latest, TargetCandidates{Target: t, Versions: append([]string{}, versions...)})
		}
		decision := ResolveBest(p.Base().Name, latest, locked, p.Base().Style())
		if decision.Style == "minor" && decision.Version != nil && artifacts != nil {
			for _, tv := range decision.PerTarget {
				fallback, ok := artifacts["any"]
				if !ok {
					fallback = []any{}
				}
				current, ok := artifacts[tv.Target]
				if !ok {
					current = fallback
				}
				urls, err := p.FetchURLs(tv.Version, tv.Target)
				if err != nil {
					return nil, err
				}
				existing, err := rowURLs(artifactRowList(current))
				if err != nil {
					return nil, err
				}
				if !sameStrings(urls, existing) {
					decision.ArtifactUpdates[tv.Target] = urls
				}
			}
			if locked != nil && MinorOf(*decision.Version).Equal(MinorOf(*locked)) {
				decision.Version = strPtr(*locked)
			}
		}
		return decision, nil
	})
}

// Headers for GitHub API calls, a GH_TOKEN or GITHUB_TOKEN lifting the anonymous rate limit
func githubHeaders() map[string]string {
	headers := map[string]string{"User-Agent": updateUserAgent}
	token := envGet("GH_TOKEN")
	if token == "" {
		token = envGet("GITHUB_TOKEN")
	}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	return headers
}

// Optional bearer auth for Hugging Face bucket fetches
func hfHeaders() map[string]string {
	headers := map[string]string{}
	if token := envGet("HF_TOKEN"); token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	return headers
}

// Process environment value as Python os environ get reads it
func envGet(key string) string { return pyEnviron()[key] }

// indexOrigin is a normalized scheme, host and effective port, Port nil when the scheme has no default
type indexOrigin struct {
	Scheme, Host string
	Port         *int
}

// Reports equal origins
func (o indexOrigin) equal(p indexOrigin) bool {
	if o.Scheme != p.Scheme || o.Host != p.Host || (o.Port == nil) != (p.Port == nil) {
		return false
	}
	return o.Port == nil || *o.Port == *p.Port
}

// Origin of a URL the way url_origin normalizes it, a ValueError for a malformed port
func urlOrigin(raw string) (indexOrigin, error) {
	parts, err := pyURLSplit(raw)
	if err != nil {
		return indexOrigin{}, err
	}
	scheme := pystr.Lower(parts.Scheme)
	_, hostinfo := reverseCut(parts.Netloc, "@")
	hostname, port := "", ""
	if _, bracketed, found := strings.Cut(hostinfo, "["); found {
		var rest string
		hostname, rest, _ = strings.Cut(bracketed, "]")
		_, port, _ = strings.Cut(rest, ":")
	} else {
		hostname, port, _ = strings.Cut(hostinfo, ":")
	}
	if hostname != "" {
		name, zone, percent := strings.Cut(hostname, "%")
		hostname = pystr.Lower(name)
		if percent {
			hostname += "%" + zone
		}
	}
	origin := indexOrigin{Scheme: scheme, Host: strings.TrimRight(pystr.Lower(hostname), ".")}
	if port != "" {
		value, ok := asciiPort(port)
		if !ok {
			return indexOrigin{}, &PyError{Class: "ValueError", Message: "Port could not be cast to integer value as " + pyrepr.StrRepr(port)}
		}
		if value < 0 || value > 65535 {
			return indexOrigin{}, &PyError{Class: "ValueError", Message: "Port out of range 0-65535"}
		}
		origin.Port = &value
		return origin, nil
	}
	switch scheme {
	case "http":
		origin.Port = intPtr(80)
	case "https":
		origin.Port = intPtr(443)
	}
	return origin, nil
}

// Pointer to a copy of an int
func intPtr(n int) *int { return &n }

// Splits at the last separator, returning the part after it last the way rpartition does
func reverseCut(s, sep string) (string, string) {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i] + sep, s[i+len(sep):]
	}
	return "", s
}

// Integer of an all ASCII digit port, ok false otherwise or when it overflows the port range check
func asciiPort(port string) (int, bool) {
	for _, r := range port {
		if !pystr.IsDigit(r) || r > 127 {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(port, 10, 64)
	if err != nil {
		return 70000, true
	}
	if n > 1<<31 {
		return 70000, true
	}
	return int(n), true
}

var (
	githubOrigin = indexOrigin{Scheme: "https", Host: "api.github.com", Port: intPtr(443)}
	hfOrigin     = indexOrigin{Scheme: "https", Host: "huggingface.co", Port: intPtr(443)}
)

// Headers an index read sends, credentials only for their exact HTTPS origin
func indexHeaders(raw string) (map[string]string, error) {
	origin, err := urlOrigin(raw)
	if err != nil {
		return nil, err
	}
	switch {
	case origin.equal(githubOrigin):
		return githubHeaders(), nil
	case origin.equal(hfOrigin):
		headers := map[string]string{"User-Agent": updateUserAgent}
		for k, v := range hfHeaders() {
			headers[k] = v
		}
		return headers, nil
	}
	return map[string]string{"User-Agent": updateUserAgent}, nil
}

// Cache key of a read, its URL, sorted headers and kind
func indexKey(raw string, headers map[string]string, kind string) string {
	var b strings.Builder
	b.WriteString(raw)
	for _, k := range sortedKeys(headers) {
		b.WriteString("\x00" + k + "\x01" + headers[k])
	}
	b.WriteString("\x00" + kind)
	return b.String()
}

// Transport index reads use, replaceable by tests the way Python installs an opener
var indexTransport http.RoundTripper = &http.Transport{
	Proxy:              http.ProxyFromEnvironment,
	DisableCompression: true,
	IdleConnTimeout:    90 * time.Second,
}

// Headers kept on a request that leaves the original origin
var crossOriginSafeHeaders = map[string]bool{"accept": true, "user-agent": true}

// Opens a URL without forwarding credentials across origins, non 2xx statuses failing as HTTP errors
func openCredentialed(raw string, headers map[string]string, timeout time.Duration) (*http.Response, error) {
	origin, err := urlOrigin(raw)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		cancel()
		return nil, &invalidURLError{err: err}
	}
	req.Header.Set("Accept-Encoding", "identity")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Transport: indexTransport, CheckRedirect: func(next *http.Request, via []*http.Request) error {
		repeats := 0
		for _, seen := range via {
			if seen.URL.String() == next.URL.String() {
				repeats++
			}
		}
		if len(via) > 10 || repeats >= 4 {
			last := via[len(via)-1].Response
			code, reason := 302, "Found"
			if last != nil {
				code = last.StatusCode
				if _, text, ok := strings.Cut(last.Status, " "); ok {
					reason = text
				}
			}
			return &HTTPError{Code: code, Reason: "The HTTP server returned a redirect error that would lead to an infinite loop.\nThe last 30x error message was:\n" + reason, URL: via[len(via)-1].URL.String()}
		}
		if target, err := urlOrigin(next.URL.String()); err != nil || !target.equal(origin) {
			for name := range next.Header {
				if !crossOriginSafeHeaders[strings.ToLower(name)] {
					next.Header.Del(name)
				}
			}
		}
		return nil
	}}
	watch := &activityWatch{timeout: timeout}
	watch.timer = time.AfterFunc(timeout, func() { watch.fired.Store(true); cancel() })
	resp, err := client.Do(req)
	if err != nil {
		watch.timer.Stop()
		cancel()
		var httpErr *HTTPError
		if errors.As(err, &httpErr) {
			return nil, httpErr
		}
		if watch.fired.Load() {
			return nil, &url.Error{Op: "Get", URL: raw, Err: socketTimeout{}}
		}
		return nil, err
	}
	watch.timer.Reset(timeout)
	resp.Body = &watchedBody{body: resp.Body, watch: watch, cancel: cancel}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, httpErrorFrom(resp.Request.URL.String(), resp)
	}
	return resp, nil
}

// Reads a whole response body
func readBody(raw string, headers map[string]string) ([]byte, error) {
	resp, err := openCredentialed(raw, headers, indexTimeout)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if errors.Is(err, io.ErrUnexpectedEOF) && resp.ContentLength > int64(len(data)) {
		return nil, &IncompleteReadError{Partial: int64(len(data)), Expected: resp.ContentLength - int64(len(data))}
	}
	return data, err
}

// Text of JSON bytes decoded in the encoding json loads detects
func jsonBytesText(b []byte) ([]byte, error) {
	encoding := "utf-8"
	switch {
	case len(b) >= 4 && (string(b[:4]) == "\x00\x00\xfe\xff" || string(b[:4]) == "\xff\xfe\x00\x00"):
		encoding = "utf-32"
	case len(b) >= 2 && (string(b[:2]) == "\xfe\xff" || string(b[:2]) == "\xff\xfe"):
		encoding = "utf-16"
	case len(b) >= 3 && string(b[:3]) == "\xef\xbb\xbf":
		return b[3:], strictUTF8Surrogates(b[3:])
	case len(b) >= 4 && b[0] == 0 && b[1] != 0:
		encoding = "utf-16-be"
	case len(b) >= 4 && b[0] == 0:
		encoding = "utf-32-be"
	case len(b) >= 4 && b[1] == 0 && (b[2] != 0 || b[3] != 0):
		encoding = "utf-16-le"
	case len(b) >= 4 && b[1] == 0:
		encoding = "utf-32-le"
	case len(b) == 2 && b[0] == 0:
		encoding = "utf-16-be"
	case len(b) == 2 && b[1] == 0:
		encoding = "utf-16-le"
	}
	var out []byte
	var err error
	switch encoding {
	case "utf-8":
		return b, strictUTF8Surrogates(b)
	case "utf-32":
		out, err = utf32.UTF32(utf32.BigEndian, utf32.ExpectBOM).NewDecoder().Bytes(b)
	case "utf-16":
		out, err = xunicode.UTF16(xunicode.BigEndian, xunicode.ExpectBOM).NewDecoder().Bytes(b)
	case "utf-16-be":
		out, err = xunicode.UTF16(xunicode.BigEndian, xunicode.IgnoreBOM).NewDecoder().Bytes(b)
	case "utf-16-le":
		out, err = xunicode.UTF16(xunicode.LittleEndian, xunicode.IgnoreBOM).NewDecoder().Bytes(b)
	case "utf-32-be":
		out, err = utf32.UTF32(utf32.BigEndian, utf32.IgnoreBOM).NewDecoder().Bytes(b)
	case "utf-32-le":
		out, err = utf32.UTF32(utf32.LittleEndian, utf32.IgnoreBOM).NewDecoder().Bytes(b)
	}
	if err != nil {
		return nil, &PyError{Class: "ValueError", Message: fmt.Sprintf("'%s' codec can't decode bytes: %s", encoding, err)}
	}
	return out, nil
}

// Refuses bytes UTF-8 with surrogatepass rejects, encoded surrogates allowed
func strictUTF8Surrogates(b []byte) error {
	masked := append([]byte{}, b...)
	for i := 0; i+2 < len(masked); i++ {
		if masked[i] == 0xED && masked[i+1] >= 0xA0 && masked[i+1] <= 0xBF && masked[i+2] >= 0x80 && masked[i+2] <= 0xBF {
			masked[i], masked[i+1], masked[i+2] = 0xEE, 0x80, 0x80
			i += 2
		}
	}
	return utf8Strict(masked)
}

// Reads JSON from an index, credentials scoped to their origin, transient failures retried
func fetchJSON(raw string) (any, error) {
	return RetryNetwork(func() (any, error) {
		headers, err := indexHeaders(raw)
		if err != nil {
			return nil, err
		}
		key := indexKey(raw, headers, "json")
		if cached, ok := cachedIndexResponse(key); ok {
			return cached, nil
		}
		body, err := readBody(raw, headers)
		if err != nil {
			return nil, err
		}
		text, err := jsonBytesText(body)
		if err != nil {
			return nil, err
		}
		result, err := loadJSON(text)
		if err != nil {
			return nil, &PyError{Class: "JSONDecodeError", Message: err.Error()}
		}
		storeIndexResponse(key, result)
		return result, nil
	}, RetryOptions{})
}

// Reads text from an index with extra headers, credentials scoped to their origin, transient failures retried
func fetchText(raw string, extra map[string]string) (string, error) {
	headers, err := indexHeaders(raw)
	if err != nil {
		return "", err
	}
	for k, v := range extra {
		headers[k] = v
	}
	return RetryNetwork(func() (string, error) {
		key := indexKey(raw, headers, "text")
		if cached, ok := cachedIndexResponse(key); ok {
			return cached.(string), nil
		}
		body, err := readBody(raw, headers)
		if err != nil {
			return "", err
		}
		result, _ := decodeUTF8Replace(body, true)
		storeIndexResponse(key, result)
		return result, nil
	}, RetryOptions{})
}

// JSON index read behind every resolver, replaceable by tests
var getJSON = func(url string) (any, error) { return fetchJSON(url) }

// Text index read behind GetText, replaceable by tests
var getText = func(url string, headers map[string]string) (string, error) { return fetchText(url, headers) }

// Text of an index URL read with credentialed open, index headers, the operation's response cache and retries
func GetText(url string, headers map[string]string) (string, error) { return getText(url, headers) }

// Value of key in a decoded JSON object the way dict get reads it, refusing a non object
func jsonGet(v any, key string, fallback any) (any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, &PyError{Class: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(v))}
	}
	if value, ok := m[key]; ok {
		return value, nil
	}
	return fallback, nil
}

// Elements of a decoded JSON value iterated the way a for loop iterates it
func jsonIter(v any) ([]any, error) {
	switch t := v.(type) {
	case []any:
		return t, nil
	case map[string]any:
		out := make([]any, 0, len(t))
		for _, k := range sortedKeys(t) {
			out = append(out, k)
		}
		return out, nil
	case string:
		var out []any
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	case nil:
		return nil, &PyError{Class: "TypeError", Message: "'NoneType' object is not iterable"}
	}
	return nil, &PyError{Class: "TypeError", Message: fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(v))}
}

// String field of a decoded JSON object with a default
func jsonString(v any, key, fallback string) (string, error) {
	value, err := jsonGet(v, key, fallback)
	if err != nil {
		return "", err
	}
	s, ok := value.(string)
	if !ok {
		return "", &PyError{Class: "TypeError", Message: fmt.Sprintf("expected string or bytes-like object, got '%s'", pyrepr.TypeName(value))}
	}
	return s, nil
}

// Build tag the llama installer's latest pointer resolves to as a bare build number, ok false when unreachable
func LlamaAppLatest() (string, bool) {
	text, err := getText(llamaBucketResolve+"/latest", hfHeaders())
	if err != nil {
		return "", false
	}
	m := firstDigits.FindString(text)
	return m, m != ""
}

// Build numbers the llama installer bucket tree lists, newest first, empty when unreachable
func LlamaAppBucketVersions() []string {
	data, err := getJSON(llamaBucketAPI + "/tree?limit=1000&offset=0")
	if err != nil {
		return []string{}
	}
	if !pyrepr.Truthy(data) {
		return []string{}
	}
	entries, err := jsonIter(data)
	if err != nil {
		return []string{}
	}
	seen := map[string]bool{}
	var versions []*big.Int
	for _, entry := range entries {
		path, err := jsonString(entry, "path", "")
		if err != nil {
			return []string{}
		}
		if m := bucketTag.FindStringSubmatch(path); m != nil {
			tag := m[1][1:]
			if !seen[tag] {
				seen[tag] = true
				versions = append(versions, digitsValue(tag))
			}
		}
	}
	sort.SliceStable(versions, func(i, j int) bool { return versions[i].Cmp(versions[j]) > 0 })
	out := make([]string, len(versions))
	for i, v := range versions {
		out[i] = v.String()
	}
	return out
}

// Release objects of up to the given pages of a GitHub releases listing, stopping at a short or empty page
func githubReleasePages(repo string, pages int, each func(release any) (bool, error)) error {
	for page := 1; page <= pages; page++ {
		data, err := getJSON(fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=30&page=%d", repo, page))
		if err != nil {
			return err
		}
		if !pyrepr.Truthy(data) {
			return nil
		}
		releases, err := jsonIter(data)
		if err != nil {
			return err
		}
		for _, release := range releases {
			stop, err := each(release)
			if err != nil || stop {
				return err
			}
		}
		if len(releases) < 30 {
			return nil
		}
	}
	return nil
}

// Reports a draft or prerelease release
func unpublished(release any) (bool, error) {
	draft, err := jsonGet(release, "draft", nil)
	if err != nil {
		return false, err
	}
	pre, err := jsonGet(release, "prerelease", nil)
	if err != nil {
		return false, err
	}
	return pyrepr.Truthy(draft) || pyrepr.Truthy(pre), nil
}

// Newest first version shaped release tags of a GitHub repo, a prefix stripped and drafts and prereleases skipped
func GithubReleaseTags(repo, stripPrefix string) ([]string, error) {
	tags := []string{}
	err := githubReleasePages(repo, 3, func(release any) (bool, error) {
		if skip, err := unpublished(release); err != nil || skip {
			return false, err
		}
		tag, err := jsonString(release, "tag_name", "")
		if err != nil {
			return false, err
		}
		if tag == "latest" {
			return false, nil
		}
		if stripPrefix != "" && strings.HasPrefix(tag, stripPrefix) {
			tag = tag[len(stripPrefix):]
		}
		if r, _ := utf8.DecodeRuneInString(tag); tag == "" || !pystr.IsDigit(r) {
			return false, nil
		}
		tags = append(tags, tag)
		return false, nil
	})
	return tags, err
}

// Dist tags of an npm package from the configured registry
func NpmDistTags(name string) (map[string]any, error) {
	data, err := getJSON(NPMRegistry(pyEnviron()) + "-/package/" + name + "/dist-tags")
	if err != nil {
		return nil, err
	}
	tags, ok := data.(map[string]any)
	if !ok {
		return nil, &PyError{Class: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(data))}
	}
	return tags, nil
}

// Newest first node versions from nodejs.org's index
func NodeLatestVersions() ([]string, error) {
	data, err := getJSON("https://nodejs.org/dist/index.json")
	if err != nil {
		return nil, err
	}
	entries, err := jsonIter(data)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, entry := range entries {
		v, err := jsonString(entry, "version", "")
		if err != nil {
			return nil, err
		}
		v = strings.TrimPrefix(v, "v")
		if nodeVersion.MatchString(v) {
			out = append(out, v)
		}
	}
	return out, nil
}

// martinRiedlBuilds is the martin riedl index with every target's versions in page order
type martinRiedlBuilds struct {
	epochs map[string]map[string]string
	order  map[string][]string
}

// Parses the martin riedl download page, an unreachable page reading as no builds
func martinRiedl() martinRiedlBuilds {
	builds := martinRiedlBuilds{epochs: map[string]map[string]string{}, order: map[string][]string{}}
	page, err := getText("https://ffmpeg.martin-riedl.de/", nil)
	if err != nil {
		return builds
	}
	for _, m := range martinRiedlBuild.FindAllStringSubmatch(page, -1) {
		osName, arch, epoch, version := m[1], m[2], m[3], m[4]
		targetArch := arch
		if arch == "amd64" {
			targetArch = "x64"
		}
		target := "linux-" + targetArch
		if osName == "macos" {
			target = "darwin-" + targetArch
		}
		versions := builds.epochs[target]
		if versions == nil {
			versions = map[string]string{}
			builds.epochs[target] = versions
		}
		previous, seen := versions[version]
		if !seen {
			builds.order[target] = append(builds.order[target], version)
		}
		if !seen || digitsValue(epoch).Cmp(digitsValue(previous)) > 0 {
			versions[version] = epoch
		}
	}
	return builds
}

// Target to version to epoch map of the martin riedl ffmpeg builds page
func MartinRiedlIndex() map[string]map[string]string { return martinRiedl().epochs }

// Release versions the martin riedl page lists for a target, in page order
func MartinRiedlVersions(target string) []string {
	versions := martinRiedl().order[target]
	if versions == nil {
		return []string{}
	}
	return versions
}

// btbnBuilds is the BtbN index with every target's versions in discovery order
type btbnBuilds struct {
	assets map[string]map[string][2]string
	order  map[string][]string
}

// Newest retained month end static GPL asset per target and version
func btbn() (btbnBuilds, error) {
	builds := btbnBuilds{assets: map[string]map[string][2]string{}, order: map[string][]string{}}
	var releases []any
	for page := 1; page <= 3; page++ {
		data, err := getJSON(fmt.Sprintf("https://api.github.com/repos/BtbN/FFmpeg-Builds/releases?per_page=30&page=%d", page))
		if err != nil {
			return builds, err
		}
		if !pyrepr.Truthy(data) {
			break
		}
		items, err := jsonIter(data)
		if err != nil {
			return builds, err
		}
		releases = append(releases, items...)
		if len(items) < 30 {
			break
		}
	}
	tags := make([]string, len(releases))
	for i, release := range releases {
		tag, err := jsonString(release, "tag_name", "")
		if err != nil {
			return builds, err
		}
		tags[i] = tag
	}
	index := make([]int, len(releases))
	for i := range index {
		index[i] = i
	}
	sort.SliceStable(index, func(a, b int) bool { return tags[index[a]] > tags[index[b]] })
	newest := ""
	monthsSeen := map[string]bool{}
	for _, i := range index {
		release := releases[i]
		if skip, err := unpublished(release); err != nil {
			return builds, err
		} else if skip {
			continue
		}
		dated := btbnTag.FindStringSubmatch(tags[i])
		if dated == nil {
			continue
		}
		month := dated[1] + "\x00" + dated[2]
		if newest == "" {
			newest = month
		}
		if month == newest || monthsSeen[month] {
			continue
		}
		monthsSeen[month] = true
		assets, err := jsonGet(release, "assets", []any{})
		if err != nil {
			return builds, err
		}
		items, err := jsonIter(assets)
		if err != nil {
			return builds, err
		}
		for _, asset := range items {
			name, err := jsonString(asset, "name", "")
			if err != nil {
				return builds, err
			}
			m := btbnAsset.FindStringSubmatch(name)
			if m == nil {
				continue
			}
			version, osName, arch := m[1], m[2], m[3]
			osKey := "linux"
			if osName == "win" {
				osKey = "win32"
			}
			targetArch := "arm64"
			if arch == "64" {
				targetArch = "x64"
			}
			target := osKey + "-" + targetArch
			if builds.assets[target] == nil {
				builds.assets[target] = map[string][2]string{}
			}
			if _, seen := builds.assets[target][version]; !seen {
				builds.assets[target][version] = [2]string{tags[i], name}
				builds.order[target] = append(builds.order[target], version)
			}
		}
	}
	return builds, nil
}

// Target to version to tag and asset name map of the newest retained BtbN builds
func BtbnIndex() (map[string]map[string][2]string, error) {
	builds, err := btbn()
	return builds.assets, err
}

// Release versions with a supported BtbN asset for a target, in index order
func BtbnVersions(target string) ([]string, error) {
	builds, err := btbn()
	if err != nil {
		return nil, err
	}
	if versions := builds.order[target]; versions != nil {
		return versions, nil
	}
	return []string{}, nil
}

// Exact interpreter identity the newest matching python build standalone release advertises
func PbsVersions(minor, triple string) ([]string, error) {
	var found []string
	err := githubReleasePages("astral-sh/python-build-standalone", 2, func(release any) (bool, error) {
		if skip, err := unpublished(release); err != nil || skip {
			return false, err
		}
		tag, err := jsonString(release, "tag_name", "")
		if err != nil {
			return false, err
		}
		if !pbsTag.MatchString(tag) {
			return false, nil
		}
		pattern := regexp.MustCompile(`^cpython-(` + regexp.QuoteMeta(minor) + `\.[0-9]+\+` + tag + `)-` + regexp.QuoteMeta(triple) + `-install_only\.tar\.gz$`)
		assets, err := jsonGet(release, "assets", []any{})
		if err != nil {
			return false, err
		}
		items, err := jsonIter(assets)
		if err != nil {
			return false, err
		}
		for _, asset := range items {
			name, err := jsonString(asset, "name", "")
			if err != nil {
				return false, err
			}
			if m := pattern.FindStringSubmatch(name); m != nil {
				found = []string{m[1]}
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return []string{}, nil
	}
	return found, nil
}

// UpdateArgs are pm update's arguments, Target empty for every target
type UpdateArgs struct {
	Names      []string
	Check      bool
	Target     string
	SkipTarget []string
	UV         bool
	NPM        bool
	Termux     bool
}

// Seams the update driver calls, replaceable by tests the way Python patches the cli module
var (
	updateLockfile       = currentLockfile
	updateHashURL        = HashURL
	updateResolvePackage = ResolvePackage
	updatePinArtifacts   = pinArtifacts
	updateInstallNames   = func(ctx context.Context, names []string) int { return installNames(ctx, names, "", true) }
	updateSyncVenvStep   = syncVenvStep
	updateRegenerate     = regenerateBootstrapPins
	updateLockProject    = lockProject
	updateRunLive        = runLive
	updateTermuxPass     = termuxPass
	bootstrapPinsPython  = func(ctx context.Context) (string, error) {
		if _, python, found, err := Toolchain(ctx, false, false); err != nil || found {
			return python, err
		}
		return exec.LookPath("python3")
	}
)

// Left aligns text to a width counted in code points like a Python format spec
func padRight(text string, width int) string {
	if n := utf8.RuneCountInString(text); n < width {
		return text + strings.Repeat(" ", width-n)
	}
	return text
}

// Text of an optional version, the em dash standing for none
func orDash(v *string) string {
	if v == nil || *v == "" {
		return "—"
	}
	return *v
}

// Optional version a lockfile pins
func lockedVersion(lockfile *Lockfile, name string) *string {
	if v, ok := lockfile.Version(name); ok {
		return &v
	}
	return nil
}

// Resolves each package's latest, an upstream outage recorded as a failure instead of aborting the run
func resolveUpdates(names []string, lockfile *Lockfile, onlyTarget string, skip map[string]bool) ([]*Resolved, []string, error) {
	var resolved []*Resolved
	var failures []string
	for _, name := range names {
		p, err := GetPackage(name)
		if err != nil {
			return nil, nil, err
		}
		var targets []string
		for _, t := range AllTargets {
			if p.MissingReason(t) == "" && !skip[t] && (onlyTarget == "" || t == onlyTarget) {
				targets = append(targets, t)
			}
		}
		if len(targets) == 0 {
			continue
		}
		decision, err := updateResolvePackage(p, targets, lockedVersion(lockfile, name), lockfile.PinnedArtifacts(name))
		if err != nil {
			decision = &Resolved{Name: name, Locked: lockedVersion(lockfile, name), Style: p.Base().Style(),
				Reason: "resolve failed: " + err.Error(), ArtifactUpdates: map[string][]string{}}
			failures = append(failures, name)
		}
		resolved = append(resolved, decision)
	}
	return resolved, failures, nil
}

// Prints one decision line
func reportUpdate(out io.Writer, d *Resolved, width int) {
	name := padRight(d.Name, width)
	switch {
	case d.Version == nil:
		reason := d.Reason
		if reason == "" {
			reason = "up to date"
		}
		fmt.Fprintf(out, "%s  %s\n", name, reason)
	case !d.Changed():
		fmt.Fprintf(out, "%s  %s up to date\n", name, pyNoneText(d.Locked))
	case d.Locked != nil && *d.Version == *d.Locked && len(d.ArtifactUpdates) > 0:
		fmt.Fprintf(out, "%s  %s: newer artifacts for %s\n", name, *d.Version, strings.Join(sortedKeys(d.ArtifactUpdates), ", "))
	default:
		per := ""
		distinct := map[string]bool{}
		for _, tv := range d.PerTarget {
			distinct[tv.Version] = true
		}
		if len(d.PerTarget) > 0 && len(distinct) > 1 {
			pairs := append([]TargetVersion{}, d.PerTarget...)
			sort.Slice(pairs, func(i, j int) bool { return pairs[i].Target < pairs[j].Target })
			parts := make([]string, len(pairs))
			for i, tv := range pairs {
				parts[i] = tv.Target + "=" + tv.Version
			}
			per = " (" + strings.Join(parts, ", ") + ")"
		}
		fmt.Fprintf(out, "%s  %s → %s%s\n", name, orDash(d.Locked), *d.Version, per)
	}
}

// Text of an optional string the way Python formats None
func pyNoneText(v *string) string {
	if v == nil {
		return "None"
	}
	return *v
}

// Syncs the dependency environment after a re-pin, reporting the outcome
func syncVenvStep(ctx context.Context, out io.Writer) (bool, error) {
	err := installSyncVenv(ctx, SyncVenvOptions{Explicit: true})
	var ie *InstallError
	if errors.As(err, &ie) {
		fmt.Fprintf(out, "✗ %s\n", err)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	fmt.Fprintln(out, "✓ venv")
	return true, nil
}

// Splices new bootstrap pins into a checkout's installers, a lock outside a checkout feeding none
func regenerateBootstrapPins(ctx context.Context, lockfile *Lockfile) (int, error) {
	script := pypath.Join(pypath.Join(pypath.Dirname(pypath.Dirname(lockfile.Path)), "scripts"), "gen-bootstrap-pins.py")
	if !isRegularFile(script) {
		return 0, nil
	}
	python, err := bootstrapPinsPython(ctx)
	if err != nil {
		return 0, err
	}
	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, python, script)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	runErr := cmd.Run()
	if runCtx.Err() == context.DeadlineExceeded {
		return 0, &uvTimeoutError{Argv: cmd.Args, Timeout: 60 * time.Second}
	}
	return exitStatus(runErr)
}

// Python class name of an error as type name reports it
func pyErrorName(err error) string {
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return "HTTPError"
	}
	return errorClass(err)
}

// Re-pins every changed package, one failing pin reported without blocking the rest
func applyPins(ctx context.Context, out io.Writer, changed []*Resolved, lockfile *Lockfile) (int, error) {
	if len(changed) == 0 {
		fmt.Fprintln(out, "pm update: nothing to update")
		return updateRegenerate(ctx, lockfile)
	}
	var pinned []string
	var failedNames []string
	failed := map[string]string{}
	for _, d := range changed {
		p, err := GetPackage(d.Name)
		if err != nil {
			return 0, err
		}
		artifacts, err := updatePinArtifacts(p, d, lockfile.PinnedArtifacts(d.Name))
		if err != nil {
			detail := err.Error()
			var httpErr *HTTPError
			if errors.As(err, &httpErr) && httpErr.URL != "" {
				detail = fmt.Sprintf("%s (%s)", err, httpErr.URL)
			}
			fmt.Fprintf(out, "✗ %s pin failed: %s\n", d.Name, detail)
			if _, seen := failed[d.Name]; !seen {
				failedNames = append(failedNames, d.Name)
			}
			failed[d.Name] = pyErrorName(err) + ": " + detail
			continue
		}
		lockfile.SetPin(d.Name, *d.Version, artifacts)
		pinned = append(pinned, d.Name)
		fmt.Fprintf(out, "✓ %s pinned %s → %s\n", d.Name, orDash(d.Locked), *d.Version)
	}
	rc := 1
	if len(pinned) > 0 {
		if err := lockfile.Save(); err != nil {
			return 0, err
		}
		fragments, err := updateRegenerate(ctx, lockfile)
		if err != nil {
			return 0, err
		}
		rc = 0
		if updateInstallNames(ctx, pinned) != 0 {
			rc = 1
		} else if synced, err := updateSyncVenvStep(ctx, out); err != nil {
			return 0, err
		} else if !synced || fragments != 0 {
			rc = 1
		}
	} else {
		fmt.Fprintln(out, "pm update: every pin failed; lockfile untouched")
	}
	if len(failed) == 0 {
		return rc, nil
	}
	fmt.Fprintf(out, "pm update: pinned %d, failed %d: %s\n", len(pinned), len(failed), strings.Join(failedNames, ", "))
	token := BeginReceipt(ctx, "update")
	for _, name := range failedNames {
		RecordStep(ctx, "pin "+name, false, failed[name])
	}
	FinalizeReceipt(ctx, "failed", 1, token)
	return 1, nil
}

// Re-resolves the project's uv.lock with upgrades, then syncs the dependency environment
func refreshUVLock(ctx context.Context, out io.Writer) (int, error) {
	err := updateLockProject(ctx, LockProjectOptions{Source: RepoRoot(), Upgrade: true, Explicit: true})
	var ie *InstallError
	if errors.As(err, &ie) {
		fmt.Fprintf(out, "✗ Python lock refresh failed: %s\n", err)
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	fmt.Fprintln(out, "✓ uv.lock refreshed")
	synced, err := updateSyncVenvStep(ctx, out)
	if err != nil || synced {
		return 0, err
	}
	return 1, nil
}

// Streams a child's output with the bounded drain and diagnostic tail, a timeout raising a RuntimeError
func runLive(ctx context.Context, out io.Writer, command []string, cwd string, env map[string]string, timeout time.Duration) (int, string, error) {
	result, err := runStreaming(ctx, command, cwd, env, timeout, func(text string) { writeFlush(out, text) })
	var timedOut *uvTimeoutError
	if errors.As(err, &timedOut) {
		return 0, "", &PyError{Class: "RuntimeError", Message: fmt.Sprintf("%s timed out after %ss", command[0], pySeconds(timeout))}
	}
	if err != nil {
		return 0, "", err
	}
	return result.Code, result.Stderr, nil
}

// Refreshes package-lock.json through the pinned npm and Node
func refreshNPMLock(ctx context.Context, out io.Writer) (int, error) {
	npm, err := Installed(ctx, "npm", false)
	if err != nil {
		return 0, err
	}
	node, err := Installed(ctx, "node", false)
	if err != nil {
		return 0, err
	}
	if npm == nil || npm.Binary == "" || node == nil || node.Binary == "" {
		fmt.Fprintln(out, "✗ npm or Node: not installed; run `icebath pm install`")
		return 1, nil
	}
	writable, err := WritableStoreRoot(ctx)
	if err != nil {
		return 0, err
	}
	env := NpmEnv(pypath.Join(writable, ".npm-cache"), EnvFor([]string{"npm"}, nil))
	code, tail, err := updateRunLive(ctx, out, []string{npm.Binary, "update"}, RepoRoot(), env, 3600*time.Second)
	if err != nil {
		return 0, err
	}
	if code != 0 {
		fmt.Fprintf(out, "✗ npm update failed:\n%s\n", tail)
		return 1, nil
	}
	fmt.Fprintln(out, "✓ package-lock.json refreshed")
	return 0, nil
}

// Runs pm update, re-pinning changed packages unless checking, the termux pool pass running on its own
// Replaces the release resolver and the artifact pinner of the update driver until the returned restore runs
func SwapUpdateResolution(resolve func(Package, []string, *string, map[string]any) (*Resolved, error),
	pin func(Package, *Resolved, map[string]any) (map[string]any, error)) func() {
	resolvePrev, pinPrev := updateResolvePackage, updatePinArtifacts
	updateResolvePackage, updatePinArtifacts = resolve, pin
	return func() { updateResolvePackage, updatePinArtifacts = resolvePrev, pinPrev }
}

func CmdUpdate(ctx context.Context, args UpdateArgs, out io.Writer) (int, error) {
	if args.Termux {
		var ignored []string
		for _, flag := range []struct {
			name  string
			given bool
		}{{"names", len(args.Names) > 0}, {"--target", args.Target != ""}, {"--uv", args.UV}, {"--npm", args.NPM}} {
			if flag.given {
				ignored = append(ignored, flag.name)
			}
		}
		if len(ignored) > 0 {
			fmt.Fprintf(out, "::warning::--termux repairs the termux pool pins only; ignoring %s\n", strings.Join(ignored, ", "))
		}
		return updateTermuxPass(ctx, out, args.Check)
	}
	lockfile := updateLockfile()
	names := args.Names
	if len(names) == 0 {
		for _, n := range lockfile.Names() {
			p, err := GetPackage(n)
			if err != nil {
				return 0, err
			}
			if !p.Base().Internal || n == "uv" {
				names = append(names, n)
			}
		}
	}
	target := args.Target
	if target != "" && !args.Check {
		fmt.Fprintln(out, "::warning::--target is a CHECK-only cross-resolution flag; ignoring it for apply (the lockfile pins every target)")
		target = ""
	}
	skip := map[string]bool{}
	for _, t := range args.SkipTarget {
		skip[t] = true
	}
	resolved, failures, err := resolveUpdates(names, lockfile, target, skip)
	if err != nil {
		return 0, err
	}
	var changed []*Resolved
	for _, d := range resolved {
		if d.Changed() {
			changed = append(changed, d)
		}
	}
	if len(resolved) == 0 {
		fmt.Fprintln(out, "pm update: nothing to check (no resolvable packages)")
		return 0, nil
	}
	width := 0
	for _, d := range resolved {
		width = max(width, utf8.RuneCountInString(d.Name))
	}
	for _, d := range resolved {
		reportUpdate(out, d, width)
	}
	if len(failures) > 0 {
		fmt.Fprintf(out, "pm update: resolution failed for %s; no changes applied\n", strings.Join(failures, ", "))
		return 1, nil
	}
	if args.Check {
		if args.UV {
			fmt.Fprintln(out, "uv deps: would run `uv lock --upgrade` + venv sync")
		}
		if args.NPM {
			fmt.Fprintln(out, "npm deps: would run `npm update`")
		}
		if len(changed) > 0 {
			return 1, nil
		}
		return 0, nil
	}
	if rc, err := applyPins(ctx, out, changed, lockfile); err != nil || rc != 0 {
		return rc, err
	}
	if args.UV {
		if rc, err := refreshUVLock(ctx, out); err != nil || rc != 0 {
			return rc, err
		}
	}
	if args.NPM {
		return refreshNPMLock(ctx, out)
	}
	return 0, nil
}

// Repins the pool archives Termux retired under the pins, a check reporting without writing
func termuxPass(ctx context.Context, out io.Writer, check bool) (int, error) {
	table, err := LoadTermuxTable("")
	if err != nil {
		return 0, err
	}
	lockfile := updateLockfile()
	pins, err := TermuxPins(table, lockfile)
	if err != nil {
		return 0, err
	}
	pool, err := termuxIndex()
	if err != nil {
		return 0, err
	}
	stale, err := RetiredPins(table, lockfile, pool)
	if err != nil {
		return 0, err
	}
	if len(stale) == 0 {
		fmt.Fprintf(out, "termux pins: %d rows still served by the pool\n", len(pins))
		return 0, nil
	}
	unresolved, width := 0, 0
	for _, entry := range stale {
		if entry.Replacement == nil {
			unresolved++
		}
		width = max(width, utf8.RuneCountInString(entry.Pin.Name))
	}
	for _, entry := range stale {
		if entry.Replacement == nil {
			fmt.Fprintf(out, "%s  %s: the pool no longer carries it\n", padRight(entry.Pin.Name, width), entry.Pin.Version)
		} else {
			fmt.Fprintf(out, "%s  %s → %s\n", padRight(entry.Pin.Name, width), entry.Pin.Version, entry.Replacement.Version)
		}
	}
	if check {
		return 1, nil
	}
	applied, err := RepairTermuxPins(table, lockfile, stale, nil)
	if err != nil {
		return 0, err
	}
	if applied > 0 {
		if err := SaveTermuxTable(table, ""); err != nil {
			return 0, err
		}
		if err := lockfile.Save(); err != nil {
			return 0, err
		}
		fmt.Fprintf(out, "✓ %d termux pins repinned; restage the bionic payload to pick them up\n", applied)
	}
	if unresolved > 0 {
		return 1, nil
	}
	return 0, nil
}

// Artifact rows for a decision, unresolved targets retained and hashes reused for unchanged URLs
func pinArtifacts(p Package, d *Resolved, current map[string]any) (map[string]any, error) {
	return ReuseIndexResponses(func() (map[string]any, error) {
		perTarget := d.PerTarget
		if len(perTarget) == 0 {
			for _, t := range AllTargets {
				perTarget = append(perTarget, TargetVersion{t, pyNoneText(d.Version)})
			}
		}
		artifacts := map[string]any{}
		for k, v := range current {
			artifacts[k] = v
		}
		hashes := map[string]string{}
		for _, tv := range perTarget {
			if p.MissingReason(tv.Target) != "" {
				continue
			}
			fallback, ok := current["any"]
			if !ok {
				fallback = []any{}
			}
			old, ok := current[tv.Target]
			if !ok {
				old = fallback
			}
			rows := artifactRowList(old)
			known := map[string]any{}
			var existing []any
			for _, row := range rows {
				u, err := rowField(row, "url")
				if err != nil {
					return nil, err
				}
				sha, err := rowField(row, "sha256")
				if err != nil {
					return nil, err
				}
				if text, ok := u.(string); ok {
					known[text] = sha
				}
				existing = append(existing, u)
			}
			urls, updated := d.ArtifactUpdates[tv.Target]
			if !updated {
				var err error
				if urls, err = p.FetchURLs(tv.Version, tv.Target); err != nil {
					return nil, err
				}
			}
			unchanged := len(urls) == len(existing)
			for i := 0; unchanged && i < len(urls); i++ {
				unchanged = jsonEquals(existing[i], urls[i])
			}
			if unchanged {
				continue
			}
			var pinned []any
			for _, u := range urls {
				var digest any = known[u]
				if !pyrepr.Truthy(digest) {
					if h, ok := hashes[u]; ok && h != "" {
						digest = h
					}
				}
				if !pyrepr.Truthy(digest) {
					sha, err := p.KnownSHA256(tv.Version, u)
					if err != nil {
						return nil, err
					}
					if sha == "" {
						if sha, err = updateHashURL(u); err != nil {
							return nil, err
						}
					}
					hashes[u] = sha
					digest = sha
				}
				pinned = append(pinned, map[string]any{"url": u, "sha256": digest})
			}
			if len(pinned) == 1 {
				artifacts[tv.Target] = pinned[0]
			} else {
				artifacts[tv.Target] = pinned
			}
		}
		return artifacts, nil
	})
}
