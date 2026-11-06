package plugins

import (
	"embed"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/home"
)

//go:embed catalog/*.yaml
var inTreeCatalog embed.FS

const LiveCatalogMaxStaleSeconds = 24 * 60 * 60

var scpURLRe = regexp2.MustCompile(`^(?:[^@/\s]+@)?([^:/\s]+):(?!//)(.+)$`, regexp2.None)

// One catalog kill list entry naming a pulled plugin and why
type RemovedEntry struct {
	Name   string
	Repo   string
	Reason string
	Date   string
}

// Kill list entries from a raw removed list, skipping rows without a name
func removedFromList(raw any) []RemovedEntry {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	var out []RemovedEntry
	for _, item := range list {
		row, ok := item.(map[string]any)
		if !ok || !jsonTruthy(row["name"]) {
			continue
		}
		out = append(out, RemovedEntry{
			Name:   scalarString(row["name"]),
			Repo:   orEmpty(row["repo"]),
			Reason: orEmpty(row["reason"]),
			Date:   orEmpty(row["date"]),
		})
	}
	return out
}

// String form of a value, empty when the value is falsy
func orEmpty(v any) string {
	if !jsonTruthy(v) {
		return ""
	}
	return scalarString(v)
}

// The removed list of a catalog directory, or the embedded in-tree catalog when dir is empty
func loadRemovedList(dir string) []RemovedEntry {
	var raw []byte
	var err error
	label := "removed.yaml"
	if dir == "" {
		raw, err = inTreeCatalog.ReadFile("catalog/removed.yaml")
	} else {
		path := filepath.Join(dir, "removed.yaml")
		label = path
		if st, statErr := os.Stat(path); statErr != nil || !st.Mode().IsRegular() {
			return nil
		}
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		logWarn("Plugin catalog: failed to read "+label, "error", err)
		return nil
	}
	data, ok := readCatalogYAML(raw, label).(map[string]any)
	if !ok {
		return nil
	}
	return removedFromList(data["removed"])
}

// Canonical host and path of a repo URL ignoring scheme, user, www, .git and trailing slashes
func normalizeRepo(raw string) string {
	text := strings.TrimSpace(raw)
	var host, path string
	if m, _ := scpURLRe.FindStringMatch(text); m != nil {
		host, path = m.GroupByNumber(1).String(), m.GroupByNumber(2).String()
	} else if strings.Contains(text, "://") {
		if u, err := url.Parse(text); err == nil {
			host, path = strings.ToLower(u.Hostname()), u.Path
		} else {
			host, path = urlsplitFallback(text)
		}
	} else {
		path = text
	}
	host = strings.TrimPrefix(strings.ToLower(host), "www.")
	path = strings.TrimRight(strings.TrimSuffix(strings.Trim(path, "/"), ".git"), "/")
	path = strings.ToLower(path)
	if host != "" {
		return host + "/" + path
	}
	return path
}

// Host and path split the way urlsplit does for text net/url rejects
func urlsplitFallback(text string) (string, string) {
	rest := text[strings.Index(text, "://")+3:]
	netloc, path := rest, ""
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		netloc, path = rest[:i], rest[i:]
		if j := strings.IndexAny(path, "?#"); j >= 0 {
			path = path[:j]
		}
	}
	if i := strings.LastIndex(netloc, "@"); i >= 0 {
		netloc = netloc[i+1:]
	}
	if strings.HasPrefix(netloc, "[") {
		if j := strings.Index(netloc, "]"); j >= 0 {
			return strings.ToLower(netloc[1:j]), path
		}
	}
	if i := strings.Index(netloc, ":"); i >= 0 {
		netloc = netloc[:i]
	}
	return strings.ToLower(netloc), path
}

// First kill list entry whose name equals the candidate or whose repo normalizes to the same URL
func matchRemoved(candidate string, entries []RemovedEntry) *RemovedEntry {
	if candidate == "" {
		return nil
	}
	text := strings.TrimSpace(candidate)
	textRepo := normalizeRepo(text)
	for i := range entries {
		if text == entries[i].Name || (entries[i].Repo != "" && textRepo == normalizeRepo(entries[i].Repo)) {
			e := entries[i]
			return &e
		}
	}
	return nil
}

// Cached live catalog path under the home cache directory
func liveCachePath(h string) string {
	return filepath.Join(home.CacheDir(h), "plugin-catalog.json")
}

// Removes the cached live catalog of any home, logging at debug when removal fails
func InvalidateLiveCacheForHome(h string) {
	if err := os.Remove(liveCachePath(h)); err != nil && !os.IsNotExist(err) {
		logDebug("Plugin catalog: could not drop the live cache under "+h, "error", err)
	}
}

// Previously fetched live catalog with its entries dropped once older than the stale window
func staleLiveCache(cache string) map[string]any {
	st, err := os.Stat(cache)
	if err != nil || !st.Mode().IsRegular() {
		return nil
	}
	data, err := readLiveCacheDoc(cache)
	if err != nil {
		return nil
	}
	if time.Since(st.ModTime()).Seconds() > LiveCatalogMaxStaleSeconds {
		copied := make(map[string]any, len(data)+1)
		for k, v := range data {
			copied[k] = v
		}
		copied["entries"] = []any{}
		data = copied
	}
	return data
}

// In-tree kill list united with the last fetched live copy without any network round trip
func cachedRemovedEntries(h string) []RemovedEntry {
	out := loadRemovedList("")
	if cached := staleLiveCache(liveCachePath(h)); cached != nil {
		out = append(out, removedFromList(cached["removed"])...)
	}
	return out
}

// Python truthiness of a decoded JSON or YAML value
func jsonTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}
