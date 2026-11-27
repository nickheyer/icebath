package tools

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	webconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/web/config"
	"github.com/nickheyer/icebath/internal/web"
	"github.com/nickheyer/icebath/pkg/fsx"
)

const (
	indexFile = "extract-index.json"
	// Most pages the index keeps, the newest
	maxIndexed = 500
)

// Page of the cache index
type entry struct {
	URL     string  `json:"url"`
	File    string  `json:"file"`
	Title   string  `json:"title"`
	Fetched float64 `json:"fetched_at"`
	SHA256  string  `json:"content_sha256"`
	// URL the page ended at after redirects, when it is not the one asked for
	Final string `json:"final_url,omitempty"`
}

// Folder of the profile's web cache: saved full texts and cached pages
func cacheDir(cfg *config.Config) (string, error) {
	dir := filepath.Join(cfg.ProfileDir(cfg.Profile()), "cache", "web")
	return dir, os.MkdirAll(dir, 0o755)
}

var slugChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// Host of the URL as a file name part, page when it has none
func hostSlug(raw string) string {
	host := "page"
	if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	slug := slugChars.ReplaceAllString(strings.ReplaceAll(host, ":", "_"), "-")
	return cmp.Or(strings.Trim(slug[:min(len(slug), 60)], "-"), "page")
}

// Whether pages of the URL may be cached: the cache is on, the host is not a local or private one, and not one exempted
func cacheable(cfg *config.Config, raw string) bool {
	return cfg.GetBool(webconfig.CacheEnabled) && !local(raw) && !exempt(cfg, raw)
}

// Whether the URL is on a host that must never be cached: loopback, private, link local, reserved, a bare name, or one not read
func local(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return true
	}
	host := strings.ToLower(strings.Trim(u.Hostname(), "[]"))
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || !strings.ContainsAny(host, ".:") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || reserved(ip)
}

// Whether the address is in the IPv4 240/4 block or a reserved IPv6 range
func reserved(ip netip.Addr) bool {
	if ip.Is4() {
		return ip.As4()[0] >= 240
	}
	for _, block := range []string{"::/8", "100::/8", "200::/7", "400::/6", "800::/5", "1000::/4", "4000::/3", "6000::/3", "8000::/3", "a000::/3",
		"c000::/3", "e000::/4", "f000::/5", "f800::/6", "fe00::/9"} {
		if netip.MustParsePrefix(block).Contains(ip) {
			return true
		}
	}
	return false
}

// Whether the URL's host is a site of web.cache_exempt_hosts or under one
func exempt(cfg *config.Config, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.Trim(strings.ToLower(strings.Trim(u.Hostname(), "[]")), ".")
	return host != "" && slices.ContainsFunc(cfg.GetStringSlice(webconfig.CacheExemptHosts), func(pattern string) bool {
		pattern = strings.TrimPrefix(strings.Trim(strings.ToLower(strings.TrimSpace(pattern)), "."), "*.")
		return pattern != "" && (host == pattern || strings.HasSuffix(host, "."+pattern))
	})
}

// Key of a page in the index, by its URL, format and backend
func digest(raw, format, backend string) string {
	sum := sha256.Sum256([]byte(raw + "\n" + cmp.Or(format, "markdown") + "\n" + backend))
	return hex.EncodeToString(sum[:])[:16]
}

// SHA-256 of the text as it reads back, without a byte order mark and with its line endings normalized
func contentSum(content string) string {
	sum := sha256.Sum256([]byte(normalLines(content)))
	return hex.EncodeToString(sum[:])
}

func normalLines(content string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.TrimPrefix(content, "\uFEFF"), "\r\n", "\n"), "\r", "\n")
}

// Index of the cached pages, empty when it is missing or damaged. The index lock is held
func (r *registrant) loadIndex(dir string) map[string]entry {
	index := map[string]entry{}
	data, err := os.ReadFile(filepath.Join(dir, indexFile))
	if err != nil || json.Unmarshal([]byte(strings.TrimPrefix(string(data), "\uFEFF")), &index) != nil {
		return map[string]entry{}
	}
	return index
}

// Writes the index, keeping the newest pages past the cap. The index lock is held
func (r *registrant) saveIndex(dir string, index map[string]entry) {
	if len(index) > maxIndexed {
		keys := make([]string, 0, len(index))
		for key := range index {
			keys = append(keys, key)
		}
		slices.SortStableFunc(keys, func(a, b string) int { return cmp.Compare(index[b].Fetched, index[a].Fetched) })
		for _, key := range keys[maxIndexed:] {
			delete(index, key)
		}
	}
	data, err := json.Marshal(index)
	if err != nil {
		return
	}
	fsx.Write(filepath.Join(dir, indexFile), data, 0o600)
}

// Fresh cached page of the URL read by the backend, nil on a miss. A page that ended at another URL after redirects is served only while
// that URL still passes the website blocklist and the private address checks
func (r *registrant) cached(cfg *config.Config, raw, format, backend string) *web.Page {
	if !cacheable(cfg, raw) {
		return nil
	}
	dir, err := cacheDir(cfg)
	if err != nil {
		return nil
	}
	r.index.Lock()
	page, ok := r.loadIndex(dir)[digest(raw, format, backend)]
	r.index.Unlock()
	if !ok || time.Since(time.Unix(0, int64(page.Fetched*1e9))) >= ttl(cfg) || page.File == "" {
		return nil
	}
	file, err := filepath.EvalSymlinks(page.File)
	if err != nil {
		return nil
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil || !strings.HasPrefix(file, root+string(filepath.Separator)) {
		return nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	content := normalLines(string(data))
	if page.SHA256 != contentSum(content) {
		return nil
	}
	if final := page.Final; final != "" && final != raw && (web.Blocked(cfg, final) != nil || !web.SafeURL(cfg, final)) {
		return nil
	}
	return &web.Page{URL: raw, Title: page.Title, Content: content}
}

// Keeps a page the backend read for reuse while it is fresh; true when this call wrote it
func (r *registrant) store(cfg *config.Config, raw, content, title, format, backend string) bool {
	if content == "" || !cacheable(cfg, raw) || len([]rune(content)) > maxStored {
		return false
	}
	dir, err := cacheDir(cfg)
	if err != nil {
		return false
	}
	key := digest(raw, format, backend)
	file := filepath.Join(dir, hostSlug(raw)+"-"+key+".cache.md")
	if fsx.Write(file, []byte(content), 0o600) != nil {
		return false
	}
	r.index.Lock()
	defer r.index.Unlock()
	index := r.loadIndex(dir)
	index[key] = entry{URL: raw, File: file, Title: title, Fetched: float64(time.Now().UnixNano()) / 1e9, SHA256: contentSum(content)}
	r.saveIndex(dir, index)
	return true
}

// Notes the URL a cached page ended at after redirects, while the cached text is still the one read
func (r *registrant) redirected(cfg *config.Config, raw, final, content, format, backend string) {
	dir, err := cacheDir(cfg)
	if err != nil {
		return
	}
	r.index.Lock()
	defer r.index.Unlock()
	index := r.loadIndex(dir)
	key := digest(raw, format, backend)
	page, ok := index[key]
	if !ok || page.SHA256 != contentSum(content) {
		return
	}
	page.Final = final
	index[key] = page
	r.saveIndex(dir, index)
}
