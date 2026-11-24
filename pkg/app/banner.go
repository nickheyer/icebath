package app

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/tools"

	"github.com/nickheyer/icebath/pkg/internal/textutil"
)

// One memoized value, the unset state apart from a computed nil
type bannerMemo struct {
	set   bool
	value any
}

var (
	bannerMemoMu         sync.Mutex
	availableSkillsCache bannerMemo
	gitBannerStateCache  bannerMemo
	updateResult         *int64
	updateCheck          = newUpdateCheckSignal()
	bannerFindAllSkills  = func(ctx context.Context) ([]map[string]any, error) {
		return skillsScope(ctx).FindAllSkills(true), nil
	}
	bannerCodeIdentity = updaterecord.GetCodeIdentity
	// True inside a test binary, where the background prefetch goroutines must not shell out to git
	skipBackgroundPrefetch = testing.Testing
)

// Event the update check sets once it has a result
type updateCheckSignal struct {
	done chan struct{}
	once sync.Once
}

func newUpdateCheckSignal() *updateCheckSignal { return &updateCheckSignal{done: make(chan struct{})} }

// Marks the check finished so waiters stop waiting
func (u *updateCheckSignal) set() { u.once.Do(func() { close(u.done) }) }

// True once the check has finished
func (u *updateCheckSignal) isSet() bool {
	select {
	case <-u.done:
		return true
	default:
		return false
	}
}

// Waits up to timeout for the check, true when it finished in time
func (u *updateCheckSignal) wait(timeout time.Duration) bool {
	select {
	case <-u.done:
		return true
	case <-time.After(timeout):
		return u.isSet()
	}
}

// Update check signal in force right now
func currentUpdateCheck() *updateCheckSignal {
	bannerMemoMu.Lock()
	defer bannerMemoMu.Unlock()
	return updateCheck
}

// Result of fn, or def when fn fails or panics
func quiet[T any](fn func() (T, error), def T) (out T) {
	defer func() {
		if r := recover(); r != nil {
			out = def
		}
	}()
	value, err := fn()
	if err != nil {
		return def
	}
	return value
}

// Cached value of a banner input computed once per process, recomputed on every call under a routed home
func memoBanner(ctx context.Context, slot *bannerMemo, compute func() (any, bool)) any {
	if home.Override(ctx) != "" {
		value, _ := compute()
		return value
	}
	bannerMemoMu.Lock()
	if slot.set {
		defer bannerMemoMu.Unlock()
		return slot.value
	}
	bannerMemoMu.Unlock()
	value, cacheable := compute()
	if cacheable {
		bannerMemoMu.Lock()
		slot.set, slot.value = true, value
		bannerMemoMu.Unlock()
	}
	return value
}

// Every installed skill, not ok when the scan fails or panics
func scanBannerSkills(ctx context.Context) (found []map[string]any, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			found, ok = nil, false
		}
	}()
	found, err := bannerFindAllSkills(ctx)
	return found, err == nil
}

// Skill names grouped by category in scan order, empty and uncached when the scan fails
func getAvailableSkills(ctx context.Context) *pyjson.Object {
	result := memoBanner(ctx, &availableSkillsCache, func() (any, bool) {
		all, ok := scanBannerSkills(ctx)
		if !ok {
			return nil, false
		}
		grouped := pyjson.NewObject()
		for _, skill := range all {
			category := "general"
			if c := skill["category"]; pyrepr.Truthy(c) {
				category = pyrepr.Str(c)
			}
			names, _ := grouped.Get(category)
			list, _ := names.([]any)
			grouped.Set(category, append(list, skill["name"]))
		}
		return grouped, true
	})
	if grouped, ok := result.(*pyjson.Object); ok && grouped != nil {
		return grouped
	}
	return pyjson.NewObject()
}

// Active git checkout of the running code or the copy under the home, empty when neither is one
func resolveRepoDir(ctx context.Context) string {
	repoDir := pm.RepoRoot()
	if resolved, err := filepath.EvalSymlinks(repoDir); err == nil {
		repoDir = resolved
	}
	if !pathExists(filepath.Join(repoDir, ".git")) {
		repoDir = filepath.Join(home.Dir(ctx), "icebath-agent")
	}
	if pathExists(filepath.Join(repoDir, ".git")) {
		return repoDir
	}
	return ""
}

// True when the path exists after following symlinks
func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Upstream and local short hashes of the checkout with the commits carried on top
type BannerGitState struct {
	Upstream, Local string
	Ahead           int
}

// Git hashes of repoDir, or of the default checkout through the per process cache when repoDir is empty
func getGitBannerState(ctx context.Context, repoDir string) *BannerGitState {
	if repoDir != "" {
		return computeGitBannerState(ctx, repoDir)
	}
	state, _ := memoBanner(ctx, &gitBannerStateCache, func() (any, bool) {
		return computeGitBannerState(ctx, ""), true
	}).(*BannerGitState)
	return state
}

// Banner state from the baked build revision, nil when the build carries none
func bakedBannerState() *BannerGitState {
	baked := quiet(func() (string, error) {
		identity, err := bannerCodeIdentity(false)
		if err != nil || identity == nil {
			return "", err
		}
		short, _ := identity.Get("short_sha")
		if !pyrepr.Truthy(short) {
			return "", nil
		}
		return pyrepr.Str(short), nil
	}, "")
	if baked == "" {
		return nil
	}
	return &BannerGitState{Upstream: baked, Local: baked, Ahead: 0}
}

// Live git hashes of the checkout, the baked state when there is no checkout or a lookup fails
func computeGitBannerState(ctx context.Context, repoDir string) *BannerGitState {
	if repoDir == "" {
		repoDir = resolveRepoDir(ctx)
	}
	if repoDir == "" {
		return bakedBannerState()
	}
	upstream, _ := selfupdate.GitStdoutWithin(ctx, []string{"rev-parse", "--short=8", "origin/main"}, repoDir, 5*time.Second)
	local, _ := selfupdate.GitStdoutWithin(ctx, []string{"rev-parse", "--short=8", "HEAD"}, repoDir, 5*time.Second)
	if upstream == "" || local == "" {
		return bakedBannerState()
	}
	ahead, _ := selfupdate.GitCount(ctx, []string{"rev-list", "--count", "origin/main..HEAD"}, repoDir)
	return &BannerGitState{Upstream: upstream, Local: local, Ahead: int(max(ahead, 0))}
}

// Version label of the install, panicking with the lookup error like the exception Python leaves uncaught
func FormatBannerVersionLabel(ctx context.Context) string {
	label, err := BannerVersionLabel(ctx)
	if err != nil {
		panic(err)
	}
	return label
}

// Version label of the install, its channel or its revisions, or the version or channel lookup error
func BannerVersionLabel(ctx context.Context) (string, error) {
	root := pm.RepoRoot()
	info, err := updaterecord.GetVersionInfo()
	if err != nil {
		return "", err
	}
	base := fmt.Sprintf("Icebath Agent v%s (%s)", info.DerivedVersion, ReleaseDate)
	cfg := quiet(func() (any, error) {
		tree, lerr := config.Load(home.Dir(ctx), nil)
		if lerr != nil {
			return nil, lerr
		}
		return map[string]any(tree), nil
	}, nil)
	channel, err := selfupdate.ResolveUpdateChannel(cfg, root)
	if err != nil {
		return "", err
	}
	if channel != "main" {
		label := base + " · " + channel
		if head := selfupdate.GitStdout([]string{"rev-parse", "HEAD"}, root); head != "" {
			label += " · local " + textutil.First(head, 12)
		}
		return label, nil
	}
	state := getGitBannerState(ctx, "")
	if state == nil {
		return base, nil
	}
	if state.Ahead <= 0 || state.Upstream == state.Local {
		return base + " · upstream " + state.Upstream, nil
	}
	return fmt.Sprintf("%s · upstream %s · local %s (+%d carried %s)", base, state.Upstream, state.Local, state.Ahead, plural(state.Ahead, "commit")), nil
}

// Runs target on its own goroutine with any failure swallowed
func bannerDaemon(target func()) {
	go func() {
		defer func() { _ = recover() }()
		target()
	}()
}

// Starts the passive update check in the background, finishing at once when prefetch is off or no updater is linked
func PrefetchUpdateCheck() {
	signal := currentUpdateCheck()
	check := selfupdate.BannerUpdateCheck
	if skipBackgroundPrefetch() || check == nil {
		signal.set()
		return
	}
	bannerDaemon(func() {
		status, err := check(context.Background())
		if err != nil {
			return
		}
		var behind *int64
		if status != nil {
			if v, ok := status.Get("behind"); ok && v != nil {
				if n, isInt := bannerInt(v); isInt {
					behind = &n
				}
			}
		}
		bannerMemoMu.Lock()
		updateResult = behind
		bannerMemoMu.Unlock()
		signal.set()
	})
}

// Integer of a decoded JSON number, false for any other value
func bannerInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		if n == math.Trunc(n) {
			return int64(n), true
		}
	}
	return 0, false
}

// Commits behind from the prefetched check after waiting up to timeout, nil when unknown or not ready
func getUpdateResult(timeout time.Duration) *int64 {
	currentUpdateCheck().wait(timeout)
	bannerMemoMu.Lock()
	defer bannerMemoMu.Unlock()
	return updateResult
}

// The word, with an s unless n is one
func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// Tool environment of the context, one rooted at the active home when the context carries none
func bannerToolEnv(ctx context.Context) *tools.Env {
	if env := tools.ToolEnvFrom(ctx); env != nil {
		return env
	}
	return &tools.Env{Home: home.Dir(ctx)}
}

// Unavailable toolsets among the enabled ones with their tools split into lazy and disabled names
func computeToolsetAvailability(ctx context.Context, enabledToolsets []string) map[string]any {
	env := bannerToolEnv(ctx)
	_, unavailable := tools.Default.CheckToolAvailability(ctx, env)
	enabled := map[string]bool{}
	for _, name := range enabledToolsets {
		enabled[name] = true
	}
	requirements := tools.Default.ToolsetRequirements(ctx, env.Home)
	kept := []any{}
	lazy, disabled := map[string]bool{}, map[string]bool{}
	for _, item := range unavailable {
		if len(enabled) > 0 && !enabled[item.Name] {
			continue
		}
		toolNames := make([]any, len(item.Tools))
		for i, name := range item.Tools {
			toolNames[i] = name
		}
		envVars := make([]any, len(item.EnvVars))
		for i, name := range item.EnvVars {
			envVars[i] = name
		}
		kept = append(kept, map[string]any{"name": item.Name, "env_vars": envVars, "tools": toolNames})
		target := disabled
		if requirements[item.Name]["check_fn"] != nil {
			target = lazy
		}
		for _, name := range item.Tools {
			target[name] = true
		}
	}
	return map[string]any{"unavailable_toolsets": kept, "lazy_tools": sortedSetNames(lazy), "disabled_tools": sortedSetNames(disabled)}
}

// Names of a set in sorted order as a JSON style list
func sortedSetNames(set map[string]bool) []any {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]any, len(names))
	for i, name := range names {
		out[i] = name
	}
	return out
}
