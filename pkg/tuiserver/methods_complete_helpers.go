package tuiserver

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	fuzzyCacheTTL      = 5 * time.Second
	fuzzyCacheMaxFiles = 20000
	gitListTimeout     = 2 * time.Second
)

var (
	fuzzyFallbackExcludes = map[string]bool{".git": true, ".hg": true, ".svn": true, ".next": true, ".cache": true,
		".venv": true, "venv": true, "node_modules": true, "__pycache__": true, "dist": true, "build": true,
		"target": true, ".mypy_cache": true, ".pytest_cache": true, ".ruff_cache": true}
	fuzzyCacheMu sync.Mutex
	fuzzyCache   = map[string]fuzzyCacheEntry{}
	fuzzyClock   = time.Now
)

// Listing of one root and when it was taken
type fuzzyCacheEntry struct {
	at    time.Time
	files []string
}

// Stdout of a non interactive git run with the window hidden, false on a spawn failure, timeout or nonzero exit
func runListingGit(args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), gitListTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	for key, value := range tools.NoninteractiveGitEnv(nil) {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.SysProcAttr = tools.WindowsHideAttr()
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil || ctx.Err() != nil {
		return "", false
	}
	return strings.ToValidUTF8(stdout.String(), "�"), true
}

// Tracked and untracked git paths relative to root up to limit, empty outside a repo or on git failure
func gitRepoFiles(root string, limit int) []string {
	topOut, ok := runListingGit("-C", root, "rev-parse", "--show-toplevel")
	if !ok {
		return nil
	}
	top := pystr.Strip(topOut)
	listed, ok := runListingGit("-C", top, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if !ok {
		return nil
	}
	var files []string
	for _, p := range strings.Split(listed, "\x00") {
		if p == "" {
			continue
		}
		rel, err := filepath.Rel(absPath(root), absPath(filepath.Join(top, p)))
		if err != nil {
			panic(err)
		}
		rel = strings.ReplaceAll(rel, string(os.PathSeparator), "/")
		if strings.HasPrefix(rel, "../") {
			continue
		}
		if files = append(files, rel); len(files) >= limit {
			break
		}
	}
	return files
}

// Absolute normalized path like os.path.abspath
func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

// Files under root top down without following links, vendor build and dot directories pruned, up to limit
func walkRepoFiles(root string, limit int) []string {
	var files []string
	var walk func(dir, rel string) bool
	walk = func(dir, rel string) bool {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return true
		}
		var dirs []string
		for _, e := range entries {
			full := filepath.Join(dir, e.Name())
			if st, err := os.Stat(full); err == nil && st.IsDir() {
				dirs = append(dirs, e.Name())
				continue
			}
			name := e.Name()
			if rel != "" {
				name = rel + "/" + name
			}
			if files = append(files, strings.ReplaceAll(name, string(os.PathSeparator), "/")); len(files) >= limit {
				return false
			}
		}
		for _, d := range dirs {
			if fuzzyFallbackExcludes[d] || strings.HasPrefix(d, ".") {
				continue
			}
			full := filepath.Join(dir, d)
			if st, err := os.Lstat(full); err != nil || st.Mode()&os.ModeSymlink != 0 {
				continue
			}
			sub := d
			if rel != "" {
				sub = rel + "/" + d
			}
			if !walk(full, sub) {
				return false
			}
		}
		return true
	}
	walk(root, "")
	return files
}

// File paths relative to root from git else a bounded walk, cached per root for a few seconds
func listRepoFiles(root string) []string {
	now := fuzzyClock()
	fuzzyCacheMu.Lock()
	if cached, found := fuzzyCache[root]; found && now.Sub(cached.at) < fuzzyCacheTTL {
		fuzzyCacheMu.Unlock()
		return cached.files
	}
	fuzzyCacheMu.Unlock()
	files := gitRepoFiles(root, fuzzyCacheMaxFiles)
	if len(files) == 0 {
		files = walkRepoFiles(root, fuzzyCacheMaxFiles)
	}
	fuzzyCacheMu.Lock()
	defer fuzzyCacheMu.Unlock()
	for key, entry := range fuzzyCache {
		if now.Sub(entry.at) >= fuzzyCacheTTL {
			delete(fuzzyCache, key)
		}
	}
	fuzzyCache[root] = fuzzyCacheEntry{now, files}
	return files
}

// Tier and length of a name against a query, lower wins, false rejects
func fuzzyBasenameRank(name, query string) (int, int, bool) {
	n := pystr.Len(name)
	if query == "" {
		return 3, n, true
	}
	nl, ql := pystr.Lower(name), pystr.Lower(query)
	if nl == ql {
		return 0, n, true
	}
	if strings.HasPrefix(nl, ql) {
		return 1, n, true
	}
	var parts []string
	var buf []rune
	for _, ch := range name {
		boundary := strings.ContainsRune("-_.", ch)
		if boundary || (unicode.IsUpper(ch) && len(buf) > 0 && !unicode.IsUpper(buf[len(buf)-1])) {
			if len(buf) > 0 {
				parts = append(parts, string(buf))
			}
			buf = nil
			if !boundary {
				buf = []rune{ch}
			}
		} else {
			buf = append(buf, ch)
		}
	}
	if len(buf) > 0 {
		parts = append(parts, string(buf))
	}
	for _, p := range parts {
		if strings.HasPrefix(pystr.Lower(p), ql) {
			return 2, n, true
		}
	}
	if strings.Contains(nl, ql) {
		return 3, n, true
	}
	rest := []rune(nl)
	for _, q := range ql {
		i := 0
		for i < len(rest) && rest[i] != q {
			i++
		}
		if i == len(rest) {
			return 0, 0, false
		}
		rest = rest[i+1:]
	}
	return 4, n, true
}

// Whether a typed path reads as absolute, its parent existing and its partial last segment matching an entry
func absCompletionPrefixExists(pathPart string) bool {
	expanded := normalizeCompletionPath(pathPart)
	trimmed := strings.TrimRight(expanded, "/")
	parent := pypath.Dirname(trimmed)
	if parent == "" {
		parent = "/"
	}
	tail := pypath.Basename(trimmed)
	if !isDir(parent) {
		return false
	}
	if tail == "" || strings.HasSuffix(expanded, "/") {
		return isDir(expanded) || expanded == "/"
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return false
	}
	tailLower := pystr.Lower(tail)
	for _, e := range entries {
		if strings.HasPrefix(pystr.Lower(e.Name()), tailLower) {
			return true
		}
	}
	return false
}

// Picker inputs of the config with the live session route layered on, a custom route keeping its identity
func modelPickerContext(ctx context.Context, a *agent.AIAgent) (provider.PickerConfigContext, error) {
	pc, err := provider.LoadPickerContext(ctx)
	if err != nil {
		return pc, err
	}
	providerName, baseURL, model, requested := "", "", "", ""
	if a != nil {
		providerName, baseURL, model, requested = a.Provider(), a.BaseURL(), a.Model(), a.RequestedProvider()
	}
	if strings.ToLower(pystr.Strip(providerName)) == "custom" {
		if identity := canonicalCustomIdentityQuietly(ctx, baseURL, pc.CurrentProvider, model, requested); identity != "" {
			providerName = identity
		}
	}
	if model == "" {
		model = resolveModel(ctx)
	}
	if providerName != "" {
		pc.CurrentProvider = providerName
	}
	if model != "" {
		pc.CurrentModel = model
	}
	if baseURL != "" {
		pc.CurrentBaseURL = baseURL
	}
	return pc, nil
}

// Canonical identity of a custom route, empty when recovery fails
func canonicalCustomIdentityQuietly(ctx context.Context, baseURL, configProvider, model, requested string) (identity string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("custom provider identity recovery failed (model picker)", "error", r)
			identity = ""
		}
	}()
	return provider.CanonicalCustomIdentity(ctx, baseURL, configProvider, model, requested)
}
