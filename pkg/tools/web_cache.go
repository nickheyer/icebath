package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

var webLimitBuckets = []int{10, 20, 50, 100}

const (
	defaultWebCacheTTLMinutes = 20
	extractIndexFilename      = "extract-index.json"
	extractIndexMaxEntries    = 500
)

// Reports whether web.cache_enabled allows both caches, on by default
func webCacheEnabled(env *Env) bool {
	v, ok := webCfg(env).Lookup("cache_enabled")
	if !ok || v == nil {
		return true
	}
	return truthy(v)
}

// TTL from web.cache_ttl_minutes clamped to 1 to 1440 minutes
func webCacheTTL(env *Env) time.Duration {
	minutes := float64(defaultWebCacheTTLMinutes)
	if v, ok := webCfg(env).Lookup("cache_ttl_minutes"); ok && v != nil {
		if f, ok := pyFloatValue(v); ok {
			minutes = f
		} else if s, isString := v.(string); isString {
			if parsed, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
				minutes = parsed
			}
		}
	}
	return time.Duration(max(1.0, min(minutes, 1440.0)) * float64(time.Minute))
}

// BucketLimit rounds a requested result count up to the nearest bucket
func BucketLimit(limit int) int {
	for _, b := range webLimitBuckets {
		if limit <= b {
			return b
		}
	}
	return webLimitBuckets[len(webLimitBuckets)-1]
}

var whitespaceRunRe = regexp.MustCompile(`\s+`)

// NormalizeQuery case folds and collapses whitespace so trivial variants share an entry
func NormalizeQuery(query string) string {
	return whitespaceRunRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(query)), " ")
}

var hostSlugRe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// Filesystem safe hostname slug for cache filenames, page when hostless
func hostSlug(raw string) string {
	host := "page"
	if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	host = strings.ReplaceAll(host, ":", "_")
	slug := hostSlugRe.ReplaceAllString(host, "-")
	if len(slug) > 60 {
		slug = slug[:60]
	}
	slug = strings.Trim(slug, "-")
	if slug == "" {
		return "page"
	}
	return slug
}

// memoKey is one search memo key
type memoKey struct {
	provider, query string
	limit           int
}

// SearchMemo is the TTL memo and single flight coalescer for search responses
type SearchMemo struct {
	mu    sync.Mutex
	store map[memoKey]memoEntry
	locks map[memoKey]*sync.Mutex
}

type memoEntry struct {
	expires  time.Time
	response *WebSearchResponse
}

// NewSearchMemo returns an empty memo
func NewSearchMemo() *SearchMemo {
	return &SearchMemo{store: map[memoKey]memoEntry{}, locks: map[memoKey]*sync.Mutex{}}
}

var searchMemo = NewSearchMemo()

func memoKeyOf(provider, query string, limit int) memoKey {
	return memoKey{provider, NormalizeQuery(query), BucketLimit(limit)}
}

// Lookup returns a copy of a fresh entry, nil on a miss
func (m *SearchMemo) Lookup(env *Env, provider, query string, limit int) *WebSearchResponse {
	if !webCacheEnabled(env) {
		return nil
	}
	key := memoKeyOf(provider, query, limit)
	m.mu.Lock()
	hit, ok := m.store[key]
	if !ok || !time.Now().Before(hit.expires) {
		delete(m.store, key)
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()
	envLog(env).Info("web_search cache hit", "query", query, "provider", provider)
	return hit.response.Clone()
}

// Store caches a successful response for the bucketed key, sweeping expired entries
func (m *SearchMemo) Store(env *Env, provider, query string, limit int, response *WebSearchResponse) {
	if !webCacheEnabled(env) || response == nil || !response.Success {
		return
	}
	key := memoKeyOf(provider, query, limit)
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for k, e := range m.store {
		if !now.Before(e.expires) {
			delete(m.store, k)
		}
	}
	m.store[key] = memoEntry{expires: now.Add(webCacheTTL(env)), response: response.Clone()}
}

// FlightLock is the per key lock held around miss, paid request and store
func (m *SearchMemo) FlightLock(provider, query string, limit int) *sync.Mutex {
	key := memoKeyOf(provider, query, limit)
	m.mu.Lock()
	defer m.mu.Unlock()
	lock, ok := m.locks[key]
	if !ok {
		if len(m.locks) > 256 {
			kept := map[memoKey]*sync.Mutex{}
			for k, l := range m.locks {
				if !l.TryLock() {
					kept[k] = l
				} else {
					l.Unlock()
				}
			}
			m.locks = kept
		}
		lock = &sync.Mutex{}
		m.locks[key] = lock
	}
	return lock
}

// Clear drops every cached entry
func (m *SearchMemo) Clear() {
	m.mu.Lock()
	m.store = map[memoKey]memoEntry{}
	m.locks = map[memoKey]*sync.Mutex{}
	m.mu.Unlock()
}

// SliceSearchResponse trims a bucketed response's result list down to the caller's limit
func SliceSearchResponse(response *WebSearchResponse, limit int) *WebSearchResponse {
	if response == nil || response.Data == nil || len(response.Data.Web) <= limit {
		return response
	}
	out := response.Clone()
	out.Data.Web = out.Data.Web[:limit]
	return out
}

var extractIndexMu sync.Mutex

// Cache directory, empty when it cannot be created
func extractCacheDir(env *Env) string {
	dir, err := webCacheDir(env)
	if err != nil {
		return ""
	}
	return dir
}

// Index entries keyed by digest, empty when absent or damaged
func loadExtractIndex(env *Env) map[string]map[string]any {
	dir := extractCacheDir(env)
	if dir == "" {
		return map[string]map[string]any{}
	}
	data, err := os.ReadFile(filepath.Join(dir, extractIndexFilename))
	if err != nil {
		return map[string]map[string]any{}
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(string(data), "\xEF\xBB\xBF")), &raw); err != nil {
		return map[string]map[string]any{}
	}
	out := map[string]map[string]any{}
	for k, v := range raw {
		if m, ok := v.(map[string]any); ok {
			out[k] = m
		}
	}
	return out
}

// Writes the index atomically keeping the newest entries past the cap
func saveExtractIndex(env *Env, index map[string]map[string]any) {
	dir := extractCacheDir(env)
	if dir == "" {
		return
	}
	if len(index) > extractIndexMaxEntries {
		type kv struct {
			key     string
			fetched float64
		}
		var rows []kv
		for k, e := range index {
			f, _ := pyFloatValue(e["fetched_at"])
			rows = append(rows, kv{k, f})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].fetched > rows[j].fetched })
		trimmed := map[string]map[string]any{}
		for _, r := range rows[:extractIndexMaxEntries] {
			trimmed[r.key] = index[r.key]
		}
		index = trimmed
	}
	body, err := json.Marshal(index)
	if err != nil {
		return
	}
	if err := config.AtomicWrite(filepath.Join(dir, extractIndexFilename), body, 0o644); err != nil {
		envLog(env).Debug("Failed to save web extract cache index", "error", err)
	}
}

// Digest keyed on url, format and provider
func urlDigest(u, format, provider string) string {
	if format == "" {
		format = "markdown"
	}
	sum := sha256.Sum256([]byte(u + "\n" + format + "\n" + provider))
	return hex.EncodeToString(sum[:])[:16]
}

// Dedicated cache file per url, format and provider, empty when the cache dir is unusable
func extractEntryFilePath(env *Env, u, format, provider string) string {
	dir := extractCacheDir(env)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, hostSlug(u)+"-"+urlDigest(u, format, provider)+".cache.md")
}

// Case insensitive exact, wildcard or bare domain suffix match
func hostMatchesPattern(host, pattern string) bool {
	host = strings.Trim(strings.ToLower(host), ".")
	pattern = strings.TrimPrefix(strings.Trim(strings.ToLower(strings.TrimSpace(pattern)), "."), "*.")
	return pattern != "" && (host == pattern || strings.HasSuffix(host, "."+pattern))
}

// True when the host matches web.cache_exempt_hosts
func isCacheExemptHost(env *Env, raw string) bool {
	patterns := webCfg(env).Get("cache_exempt_hosts")
	list := toAnyList(patterns)
	u, err := url.Parse(raw)
	if err != nil || len(list) == 0 {
		return false
	}
	host := strings.Trim(u.Hostname(), "[]")
	if host == "" {
		return false
	}
	for _, p := range list {
		if hostMatchesPattern(host, asStr(p)) {
			return true
		}
	}
	return false
}

// True for loopback, private, LAN and unparseable URLs that must never be cached
func isLocalDevURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return true
	}
	host := strings.ToLower(strings.Trim(u.Hostname(), "[]"))
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return true
	}
	if !strings.Contains(host, ".") && !strings.Contains(host, ":") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ipIsReserved(ip)
}

// Python ipaddress is_reserved for the IPv4 240/4 block and IPv6 reserved ranges
func ipIsReserved(ip netip.Addr) bool {
	if ip.Is4() {
		return ip.As4()[0] >= 240
	}
	for _, p := range []string{"::/8", "100::/8", "200::/7", "400::/6", "800::/5", "1000::/4", "4000::/3", "6000::/3", "8000::/3", "a000::/3", "c000::/3", "e000::/4", "f000::/5", "f800::/6", "fe00::/9"} {
		if netip.MustParsePrefix(p).Contains(ip) {
			return true
		}
	}
	return false
}

// Extract cache gate, enabled, not a local dev host, not user exempted
func extractCacheable(env *Env, u string) bool {
	return webCacheEnabled(env) && !isLocalDevURL(u) && !isCacheExemptHost(env, u)
}

// SHA-256 of the text as it reads back, BOM dropped and line endings normalized
func contentSHA(content string) string {
	normalized := strings.ReplaceAll(strings.ReplaceAll(strings.TrimPrefix(content, "\uFEFF"), "\r\n", "\n"), "\r", "\n")
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

// ExtractCacheGet returns a fresh cached page as a WebPage with Cached set, nil on a miss
func ExtractCacheGet(env *Env, u, format, provider string) *WebPage {
	if !extractCacheable(env, u) {
		return nil
	}
	extractIndexMu.Lock()
	entry := loadExtractIndex(env)[urlDigest(u, format, provider)]
	extractIndexMu.Unlock()
	if entry == nil {
		return nil
	}
	fetched, _ := pyFloatValue(entry["fetched_at"])
	if time.Since(time.Unix(0, int64(fetched*1e9))) >= webCacheTTL(env) {
		return nil
	}
	filePath := asStr(entry["file"])
	cacheRoot := extractCacheDir(env)
	if filePath == "" || cacheRoot == "" {
		return nil
	}
	resolvedFile, err := filepath.EvalSymlinks(filePath)
	if err != nil {
		return nil
	}
	resolvedRoot, err := filepath.EvalSymlinks(cacheRoot)
	if err != nil || !strings.HasPrefix(resolvedFile, resolvedRoot+string(filepath.Separator)) {
		return nil
	}
	data, err := os.ReadFile(resolvedFile)
	if err != nil {
		return nil
	}
	content := strings.ReplaceAll(strings.ReplaceAll(strings.TrimPrefix(string(data), "\xEF\xBB\xBF"), "\r\n", "\n"), "\r", "\n")
	_, hasSHA := entry["content_sha256"]
	_, hasFinal := entry["final_url"]
	if (hasSHA || hasFinal) && asStr(entry["content_sha256"]) != contentSHA(content) {
		return nil
	}
	envLog(env).Info("web_extract cache hit", "url", u)
	final := asStr(entry["final_url"])
	if final == "" {
		final = u
	}
	return &WebPage{URL: u, Title: asStr(entry["title"]), Content: content, Cached: true, FinalURL: final}
}

// ExtractCacheRecordFinalURL remembers the post redirect URL behind a cached page, compare and set on the content
func ExtractCacheRecordFinalURL(env *Env, u, finalURL, content, format, provider string) {
	extractIndexMu.Lock()
	defer extractIndexMu.Unlock()
	index := loadExtractIndex(env)
	entry := index[urlDigest(u, format, provider)]
	if entry == nil || asStr(entry["content_sha256"]) != contentSHA(content) {
		return
	}
	entry["final_url"] = finalURL
	saveExtractIndex(env, index)
}

// ExtractCachePut stores one successful extraction's clean text for TTL reuse, true when this call wrote it
func ExtractCachePut(env *Env, u, content, title, format, provider string) bool {
	if content == "" || !extractCacheable(env, u) {
		return false
	}
	filePath := extractEntryFilePath(env, u, format, provider)
	if len([]rune(content)) > MaxStoredTextChars || filePath == "" {
		return false
	}
	if err := WriteTextExclusive(filePath, content, false, true); err != nil {
		envLog(env).Debug("Failed to cache web extract", "url", u, "error", err)
		return false
	}
	extractIndexMu.Lock()
	defer extractIndexMu.Unlock()
	index := loadExtractIndex(env)
	index[urlDigest(u, format, provider)] = map[string]any{"url": u, "file": filePath, "title": title,
		"fetched_at": float64(time.Now().UnixNano()) / 1e9, "content_sha256": contentSHA(content)}
	saveExtractIndex(env, index)
	return true
}

// Python float() of a string, trimmed
func parseFloatText(s string) (float64, error) { return strconv.ParseFloat(strings.TrimSpace(s), 64) }
