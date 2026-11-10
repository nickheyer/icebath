package plugins

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyversion"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Published catalog document the docs build generates
var LiveCatalogURL = "https://hermes-agent.nousresearch.com/docs/api/plugin-catalog.json"

const (
	LiveCatalogTTLSeconds        = 6 * 60 * 60
	LiveCatalogFailureTTLSeconds = 60.0
	liveCatalogRequestTimeout    = 5 * time.Second
	maxLiveCatalogBytes          = 2 * 1024 * 1024
)

// Curated display fields a published doc older than the field does not carry
var curatedCatalogFields = []string{"onboarding", "title"}

var (
	liveFetchMu          sync.Mutex
	liveFetchFailedUntil float64
	writeLiveCatalog     = pyfs.AtomicWriteText
	liveCatalogClient    = &http.Client{Timeout: liveCatalogRequestTimeout}
)

// Seconds since the epoch as a float
func epochNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Decoded JSON object of a cache file, an error when it is unreadable or not an object
func readLiveCacheDoc(cache string) (map[string]any, error) {
	raw, err := os.ReadFile(cache)
	if err != nil {
		return nil, err
	}
	doc, err := pyjson.Loads(string(stripBOM(raw)))
	if err != nil {
		return nil, err
	}
	data, ok := pyjson.Plain(doc).(map[string]any)
	if !ok {
		return nil, errors.New("live catalog cache is not a JSON object")
	}
	return data, nil
}

// Published catalog cached for the TTL under the current home, nil on any failure with a dead host remembered for a minute
func FetchLiveCatalog(ctx context.Context, force bool) map[string]any {
	cache := liveCachePath(home.Dir(ctx))
	if st, err := os.Stat(cache); !force && err == nil && st.Mode().IsRegular() &&
		epochNow()-float64(st.ModTime().UnixNano())/1e9 < LiveCatalogTTLSeconds {
		data, err := readLiveCacheDoc(cache)
		if err == nil {
			return data
		}
		logDebug("Plugin catalog: unreadable live cache "+cache, "error", err)
	}
	liveFetchMu.Lock()
	failedUntil := liveFetchFailedUntil
	liveFetchMu.Unlock()
	if !force && epochNow() < failedUntil {
		return staleLiveCache(cache)
	}
	data, err := downloadLiveCatalog(ctx, cache)
	if err != nil {
		logDebug("Plugin catalog: live fetch failed", "error", err)
		liveFetchMu.Lock()
		liveFetchFailedUntil = epochNow() + LiveCatalogFailureTTLSeconds
		liveFetchMu.Unlock()
		return staleLiveCache(cache)
	}
	return data
}

// Downloads, validates and atomically caches the published catalog
func downloadLiveCatalog(ctx context.Context, cache string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, LiveCatalogURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := liveCatalogClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("Client error '%s' for url '%s'", resp.Status, resp.Request.URL)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxLiveCatalogBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxLiveCatalogBytes {
		return nil, errors.New("live catalog payload too large")
	}
	text, err := pyjson.DecodeBytes(body)
	if err != nil {
		return nil, err
	}
	doc, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	data, ok := pyjson.Plain(doc).(map[string]any)
	if !ok {
		return nil, errors.New("unexpected live catalog payload")
	}
	if _, ok := data["entries"].([]any); !ok {
		return nil, errors.New("unexpected live catalog payload")
	}
	if _, err := home.Mkdir(ctx, filepath.Dir(cache)); err != nil {
		return nil, err
	}
	dumped, err := pyjson.Dumps(doc)
	if err != nil {
		return nil, err
	}
	if err := writeLiveCatalog(ctx, cache, dumped, filepath.Base(cache)+".tmp-"); err != nil {
		return nil, err
	}
	return data, nil
}

var (
	inTreeCatalogTimeMu       sync.Mutex
	inTreeCatalogTimeResolved bool
	inTreeCatalogTimeValue    *float64
)

// Checkout root and catalog path the running binary was built from, swapped by tests
var catalogCheckout = func() (root, rel string) {
	return pm.RepoRoot(), filepath.Join("pkg", "plugins", "catalog")
}

// Commit time of the last change to the in-tree catalog, its newest file mtime on a treeless clone, nil outside a checkout
func InTreeCatalogTime() *float64 {
	inTreeCatalogTimeMu.Lock()
	defer inTreeCatalogTimeMu.Unlock()
	if inTreeCatalogTimeResolved {
		return inTreeCatalogTimeValue
	}
	root, rel := catalogCheckout()
	var resolved *float64
	if _, err := os.Lstat(filepath.Join(root, ".git")); err == nil {
		out := catalogGitProbe(root, rel)
		if out != "" {
			if v, err := strconv.ParseFloat(out, 64); err == nil {
				resolved = &v
			} else {
				logDebug("Plugin catalog: could not date the in-tree catalog", "error", err)
			}
		}
		if resolved == nil {
			resolved = catalogWorktreeMtime(filepath.Join(root, rel))
		}
	}
	inTreeCatalogTimeResolved, inTreeCatalogTimeValue = true, resolved
	return resolved
}

// Forgets the resolved in-tree catalog time so the next call resolves it again
func resetInTreeCatalogTime() {
	inTreeCatalogTimeMu.Lock()
	inTreeCatalogTimeResolved, inTreeCatalogTimeValue = false, nil
	inTreeCatalogTimeMu.Unlock()
}

// In-tree catalog time the live merge consults, swapped by tests
var inTreeCatalogTime = InTreeCatalogTime

// Last commit time of the catalog path with lazy fetch disabled, empty on any failure or after ten seconds
func catalogGitProbe(root, rel string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", root, "log", "-1", "--format=%ct", "--", rel)
	cmd.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=")
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Newest mtime of any file under the checked out catalog, nil when nothing is checked out
func catalogWorktreeMtime(dir string) *float64 {
	var newest *float64
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == dir {
				return nil
			}
			return err
		}
		if path == dir {
			return nil
		}
		st, statErr := os.Stat(path)
		if statErr != nil {
			if errors.Is(statErr, fs.ErrNotExist) {
				return nil
			}
			return statErr
		}
		if st.Mode().IsRegular() {
			t := float64(st.ModTime().UnixNano()) / 1e9
			if newest == nil || t > *newest {
				newest = &t
			}
		}
		return nil
	})
	if err != nil {
		return nil
	}
	return newest
}

// Epoch of the doc generated_at stamp, naive stamps read as UTC, nil when absent or unparseable
func liveGeneratedTime(data map[string]any) *float64 {
	raw, ok := data["generated_at"].(string)
	if !ok || raw == "" {
		return nil
	}
	t, aware, err := pydatetime.ParseISOFormat(raw)
	if err != nil {
		return nil
	}
	if !aware {
		t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
	}
	v := float64(t.Unix()) + float64(t.Nanosecond())/1e9
	return &v
}

// Whether the in-tree copy of an entry pinned differently outranks the live one
func preferInTreeEntry(tree, live *PluginCatalogEntry, treeIsNewer *bool) bool {
	if tree.Sha == live.Sha {
		return false
	}
	if treeIsNewer != nil {
		return *treeIsNewer
	}
	if tree.Version != "" && live.Version != "" {
		a, errA := pyversion.Parse(tree.Version)
		b, errB := pyversion.Parse(live.Version)
		return errA == nil && errB == nil && pyversion.Compare(a, b) > 0
	}
	return false
}

// Live or cached catalog entries merged with the in-tree ones, the newer source winning a differing pin
func LoadCatalogLive(ctx context.Context) []*PluginCatalogEntry {
	data := FetchLiveCatalog(ctx, false)
	if data == nil {
		return inTreeCatalogEntries()
	}
	rawEntries, _ := data["entries"].([]any)
	var entries []*PluginCatalogEntry
	for i, raw := range rawEntries {
		if e := EntryFromMapping(raw, fmt.Sprintf("%s#%d", LiveCatalogURL, i)); e != nil {
			entries = append(entries, e)
		}
	}
	if len(entries) == 0 {
		return inTreeCatalogEntries()
	}
	inTree := map[string]*PluginCatalogEntry{}
	for _, e := range inTreeCatalogEntries() {
		inTree[e.Name] = e
	}
	var treeIsNewer *bool
	if liveT, treeT := liveGeneratedTime(data), inTreeCatalogTime(); liveT != nil && treeT != nil {
		newer := *treeT > *liveT
		treeIsNewer = &newer
	}
	rawByName := map[string]map[string]any{}
	for _, raw := range rawEntries {
		if m, ok := asStringMap(raw); ok {
			rawByName[pyrepr.Str(m["name"])] = m
		}
	}
	out := make([]*PluginCatalogEntry, 0, len(entries))
	for _, e := range entries {
		tree := inTree[e.Name]
		if tree != nil && preferInTreeEntry(tree, e, treeIsNewer) {
			out = append(out, tree)
			continue
		}
		out = append(out, withCuratedFields(e, tree, rawByName[e.Name]))
	}
	return out
}

// Live entry with the curated fields its doc lacks taken from the same pin in tree
func withCuratedFields(live, tree *PluginCatalogEntry, raw map[string]any) *PluginCatalogEntry {
	if tree == nil || tree.Sha != live.Sha {
		return live
	}
	merged := *live
	changed := false
	for _, key := range curatedCatalogFields {
		if _, ok := raw[key]; ok {
			continue
		}
		changed = true
		switch key {
		case "onboarding":
			merged.Onboarding = tree.Onboarding
		case "title":
			merged.Title = tree.Title
		}
	}
	if !changed {
		return live
	}
	return &merged
}

// Kill list of the live or cached catalog, empty when it is unavailable
func LiveRemovedList(ctx context.Context) []RemovedEntry {
	data := FetchLiveCatalog(ctx, false)
	if data == nil {
		return nil
	}
	return removedFromList(data["removed"])
}

// Live catalog entry of a name, nil when absent
func GetLiveCatalogEntry(ctx context.Context, name string) *PluginCatalogEntry {
	for _, e := range LoadCatalogLive(ctx) {
		if e.Name == name {
			return e
		}
	}
	return nil
}
