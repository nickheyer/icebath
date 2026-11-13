package git

import (
	"errors"
	"math"
	"os"
	"runtime"
	"strings"
	"sync"
)

const (
	defaultScanMaxDepth = 3
	scanMaxConcurrency  = 32
)

var scanJunkDirs = map[string]bool{"Applications": true, "Library": true, "node_modules": true, "site-packages": true, "vendor": true, "venv": true}

// Home media folders whose scan raises a macOS TCC prompt
var scanMediaRootDirs = map[string]bool{"Movies": true, "Music": true, "Pictures": true, "Public": true}

var scanLibraryPackageSuffixes = []string{".photoslibrary", ".musiclibrary", ".tvlibrary", ".aplibrary"}

// Platform name a non string override takes, matching none of the Node platform names
const nonStringPlatform = "\x00"

func isLibraryPackage(name string) bool {
	lower := jsLower(name)
	for _, suffix := range scanLibraryPackageSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

// Node process.platform of the running binary
func nodePlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "solaris", "illumos":
		return "sunos"
	}
	return runtime.GOOS
}

func pathAPIFor(platform string) nodePath {
	if platform == "win32" {
		return win32Path
	}
	return posixPath
}

// Scan path with its comparison key, lowercased on Windows
type NormalizedScanPath struct {
	Key   string
	Value string
}

// RepoScanPathOptions overrides the home folder and platform a scan path is read against
type RepoScanPathOptions struct {
	HomeDir  *string
	Platform *string
}

func (o RepoScanPathOptions) platform() string {
	if o.Platform != nil {
		return *o.Platform
	}
	return nodePlatform()
}

func (o RepoScanPathOptions) homeDir() string {
	if o.HomeDir != nil {
		return *o.HomeDir
	}
	home, _ := os.UserHomeDir()
	return home
}

// Absolute normalized scan path with a leading tilde expanded, nil for a blank path
func NormalizeRepoScanPath(rawPath any, options RepoScanPathOptions) *NormalizedScanPath {
	platform := options.platform()
	homeDir := options.homeDir()
	api := pathAPIFor(platform)
	raw := ""
	if rawPath != nil {
		raw = jsTrim(jsString(rawPath))
	}
	if raw == "" {
		return nil
	}
	expanded := raw
	if raw == "~" {
		expanded = homeDir
	} else if strings.HasPrefix(raw, "~/") || strings.HasPrefix(raw, `~\`) {
		expanded = api.join(homeDir, raw[2:])
	}
	absolute := expanded
	if !api.isAbsolute(expanded) {
		absolute = api.resolve(homeDir, expanded)
	}
	value := api.normalize(absolute)
	key := value
	if platform == "win32" {
		key = jsLower(value)
	}
	return &NormalizedScanPath{Key: key, Value: value}
}

// True when candidate is parent or lies inside it after both are normalized
func RepoScanPathIsWithin(candidate, parent any, options RepoScanPathOptions) bool {
	api := pathAPIFor(options.platform())
	candidatePath := NormalizeRepoScanPath(candidate, options)
	parentPath := NormalizeRepoScanPath(parent, options)
	if candidatePath == nil || parentPath == nil {
		return false
	}
	relative := api.relative(parentPath.Key, candidatePath.Key)
	return relative == "" || (relative != ".." && !strings.HasPrefix(relative, ".."+api.sep()) && !api.isAbsolute(relative))
}

// One repository the scan found
type RepoScanResult struct {
	Root  string `json:"root"`
	Label string `json:"label"`
}

// Runs fn over items with at most limit calls in flight
func mapLimit(items []string, limit int, fn func(string)) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	cursor := 0
	for w := 0; w < min(limit, len(items)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if cursor >= len(items) {
					mu.Unlock()
					return
				}
				item := items[cursor]
				cursor++
				mu.Unlock()
				fn(item)
			}
		}()
	}
	wg.Wait()
}

// Repositories under the configured roots, nothing scanned when discovery is disabled or no root is given
func ScanGitRepos(roots any, options any) ([]RepoScanResult, error) {
	opts := jsObject(options)
	rootList, isArray := roots.([]any)
	if enabled, ok := opts["enabled"].(bool); (ok && !enabled) || !isArray || len(rootList) == 0 {
		return []RepoScanResult{}, nil
	}
	maxDepth := math.NaN()
	if v, present := opts["maxDepth"]; present {
		maxDepth = jsNumber(v)
	}
	if math.IsNaN(maxDepth) || math.IsInf(maxDepth, 0) || maxDepth < 0 {
		maxDepth = defaultScanMaxDepth
	}
	var pathOptions RepoScanPathOptions
	if p := opts["platform"]; jsTruthy(p) {
		name, isString := p.(string)
		if !isString {
			name = nonStringPlatform
		}
		pathOptions.Platform = &name
	}

	var searchRoots []string
	rootIndex := map[string]int{}
	for _, root := range rootList {
		entry := NormalizeRepoScanPath(root, pathOptions)
		if entry == nil {
			continue
		}
		if i, seen := rootIndex[entry.Key]; seen {
			searchRoots[i] = entry.Value
			continue
		}
		rootIndex[entry.Key] = len(searchRoots)
		searchRoots = append(searchRoots, entry.Value)
	}

	var exclusions []NormalizedScanPath
	if raw, present := opts["excludePaths"]; present && raw != nil {
		list, ok := raw.([]any)
		if !ok {
			return nil, errors.New("(options.excludePaths ?? []).map is not a function")
		}
		for _, excluded := range list {
			if entry := NormalizeRepoScanPath(excluded, pathOptions); entry != nil {
				exclusions = append(exclusions, *entry)
			}
		}
	}

	var mu sync.Mutex
	foundOrder := []string{}
	found := map[string]RepoScanResult{}

	isExcluded := func(candidate string) bool {
		for _, excluded := range exclusions {
			if RepoScanPathIsWithin(candidate, excluded.Value, pathOptions) {
				return true
			}
		}
		return false
	}

	skipTCC := pathOptions.platform() == "darwin"

	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if float64(depth) > maxDepth || isExcluded(dir) {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if entry.Name() == ".git" && entry.IsDir() {
				if !readable(hostPath.join(dir, ".git", "HEAD")) {
					return
				}
				if normalized := NormalizeRepoScanPath(dir, pathOptions); normalized != nil {
					label := hostPath.basename(normalized.Value)
					if label == "" {
						label = normalized.Value
					}
					mu.Lock()
					if _, seen := found[normalized.Key]; !seen {
						foundOrder = append(foundOrder, normalized.Key)
					}
					found[normalized.Key] = RepoScanResult{Root: normalized.Value, Label: label}
					mu.Unlock()
				}
				return
			}
		}
		var subdirs []string
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() || strings.HasPrefix(name, ".") || scanJunkDirs[name] {
				continue
			}
			if skipTCC && ((depth == 0 && scanMediaRootDirs[name]) || isLibraryPackage(name)) {
				continue
			}
			subdirs = append(subdirs, hostPath.join(dir, name))
		}
		mapLimit(subdirs, scanMaxConcurrency, func(subdir string) { walk(subdir, depth+1) })
	}

	mapLimit(searchRoots, scanMaxConcurrency, func(root string) { walk(root, 0) })

	out := make([]RepoScanResult, 0, len(foundOrder))
	for _, key := range foundOrder {
		out = append(out, found[key])
	}
	return out, nil
}
