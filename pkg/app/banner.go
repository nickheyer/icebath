package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/tools"
)

const icebathAgentLogo = "[bold #FFD700]██╗ ██████╗███████╗██████╗  █████╗ ████████╗██╗  ██╗       █████╗  ██████╗ ███████╗███╗   ██╗████████╗[/]\n" +
	"[bold #FFD700]██║██╔════╝██╔════╝██╔══██╗██╔══██╗╚══██╔══╝██║  ██║      ██╔══██╗██╔════╝ ██╔════╝████╗  ██║╚══██╔══╝[/]\n" +
	"[#FFBF00]██║██║     █████╗  ██████╔╝███████║   ██║   ███████║█████╗███████║██║  ███╗█████╗  ██╔██╗ ██║   ██║[/]\n" +
	"[#FFBF00]██║██║     ██╔══╝  ██╔══██╗██╔══██║   ██║   ██╔══██║╚════╝██╔══██║██║   ██║██╔══╝  ██║╚██╗██║   ██║[/]\n" +
	"[#CD7F32]██║╚██████╗███████╗██████╔╝██║  ██║   ██║   ██║  ██║      ██║  ██║╚██████╔╝███████╗██║ ╚████║   ██║[/]\n" +
	"[#CD7F32]╚═╝ ╚═════╝╚══════╝╚═════╝ ╚═╝  ╚═╝   ╚═╝   ╚═╝  ╚═╝      ╚═╝  ╚═╝ ╚═════╝ ╚══════╝╚═╝  ╚═══╝   ╚═╝[/]"

const icebathCaduceus = "[#CD7F32]⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⢀⣀⡀⠀⣀⣀⠀⢀⣀⡀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀[/]\n" +
	"[#CD7F32]⠀⠀⠀⠀⠀⠀⢀⣠⣴⣾⣿⣿⣇⠸⣿⣿⠇⣸⣿⣿⣷⣦⣄⡀⠀⠀⠀⠀⠀⠀[/]\n" +
	"[#FFBF00]⠀⢀⣠⣴⣶⠿⠋⣩⡿⣿⡿⠻⣿⡇⢠⡄⢸⣿⠟⢿⣿⢿⣍⠙⠿⣶⣦⣄⡀⠀[/]\n" +
	"[#FFBF00]⠀⠀⠉⠉⠁⠶⠟⠋⠀⠉⠀⢀⣈⣁⡈⢁⣈⣁⡀⠀⠉⠀⠙⠻⠶⠈⠉⠉⠀⠀[/]\n" +
	"[#FFD700]⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⣴⣿⡿⠛⢁⡈⠛⢿⣿⣦⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀[/]\n" +
	"[#FFD700]⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠿⣿⣦⣤⣈⠁⢠⣴⣿⠿⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀[/]\n" +
	"[#FFBF00]⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠈⠉⠻⢿⣿⣦⡉⠁⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀[/]\n" +
	"[#FFBF00]⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠘⢷⣦⣈⠛⠃⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀[/]\n" +
	"[#CD7F32]⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⢠⣴⠦⠈⠙⠿⣦⡄⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀[/]\n" +
	"[#CD7F32]⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠸⣿⣤⡈⠁⢤⣿⠇⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀[/]\n" +
	"[#B8860B]⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠉⠛⠷⠄⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀[/]\n" +
	"[#B8860B]⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⢀⣀⠑⢶⣄⡀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀[/]\n" +
	"[#B8860B]⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⣿⠁⢰⡆⠈⡿⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀[/]\n" +
	"[#B8860B]⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠈⠳⠈⣡⠞⠁⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀[/]\n" +
	"[#B8860B]⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠈⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀[/]"

const releaseURLBase = "https://github.com/NousResearch/hermes-agent/releases/tag"

const bannerSnapshotVersion = 1

// One memoized value, the unset state apart from a computed nil
type bannerMemo struct {
	set   bool
	value any
}

var (
	bannerMemoMu          sync.Mutex
	availableSkillsCache  bannerMemo
	gitBannerStateCache   bannerMemo
	latestReleaseCache    bannerMemo
	updateResult          *int64
	updateCheck           = newUpdateCheckSignal()
	bannerPrefetchStarted bool
	deferredNoticeStarted bool
	bannerTerminalColumns = terminalColumns
	bannerUpdateCheck     = func(ctx context.Context) (*pyjson.Object, error) {
		return selfupdate.CheckForUpdates(ctx, selfupdate.SourceCheckOptions{Passive: true})
	}
	bannerFindAllSkills = func(ctx context.Context) ([]map[string]any, error) {
		return skillsScope(ctx).FindAllSkills(true), nil
	}
	bannerActiveSkin   = config.GetActiveSkin
	bannerCodeIdentity = updaterecord.GetCodeIdentity
	bannerMCPStatus    = func(ctx context.Context) []mcp.ServerStatus {
		return mcp.Status(ctx, bannerToolEnv(ctx), nil, true)
	}
	bannerActiveProfileName = profiles.GetActiveProfileName
	bannerCodexRuntime      = codexRuntimeActive
	bannerMCPConfigured     = mcpConfigured
	bannerMoAAggregator     = moaAggregatorLabel
	bannerRecommendedUpdate = config.RecommendedUpdateCommand
	bannerManagedUpdate     = config.GetManagedUpdateCommand
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

// Registry query of the retired updater kept inert, the commit distance is never known
func checkViaPyPI() *int64 { return nil }

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

// Color of the active skin under key, the fallback when the skin engine fails
func skinColor(ctx context.Context, key, fallback string) string {
	return quiet(func() (string, error) { return bannerActiveSkin(ctx).GetColor(key, fallback), nil }, fallback)
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

// Forgets the scanned skills so the next banner walks the skills tree again
func ResetAvailableSkillsCache() {
	bannerMemoMu.Lock()
	availableSkillsCache = bannerMemo{}
	bannerMemoMu.Unlock()
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

// Git hashes of the default checkout for the startup banner, computed once per process
func GitBannerState() *BannerGitState { return getGitBannerState(context.Background(), "") }

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

// Latest local git tag and its release page, nil when there is none, a miss cached too
func getLatestReleaseTag(ctx context.Context, repoDir string) *[2]string {
	release, _ := memoBanner(ctx, &latestReleaseCache, func() (any, bool) {
		rd := repoDir
		if rd == "" {
			rd = resolveRepoDir(ctx)
		}
		if rd == "" {
			return (*[2]string)(nil), true
		}
		tag, _ := selfupdate.GitStdoutWithin(ctx, []string{"describe", "--tags", "--abbrev=0"}, rd, 3*time.Second)
		if tag == "" {
			return (*[2]string)(nil), true
		}
		return &[2]string{tag, releaseURLBase + "/" + tag}, true
	}).(*[2]string)
	return release
}

// Version label shown in the startup banner title
func formatBannerVersionLabel() string { return FormatBannerVersionLabel(context.Background()) }

// Version label of the install, its channel or its upstream and local revisions
func FormatBannerVersionLabel(ctx context.Context) string {
	root := pm.RepoRoot()
	stamp := pm.ReadInstallStamp(root)
	stampValue := func(key string) any {
		if stamp == nil {
			return nil
		}
		v, _ := stamp.Get(key)
		return v
	}
	info, err := updaterecord.GetVersionInfo()
	if err != nil {
		panic(err)
	}
	if stampValue("distribution") == "desktop-app" {
		label := "Icebath Agent v" + info.DerivedVersion
		if stampValue("source") == "commit-build" {
			commit := ""
			if c := stampValue("commit"); pyrepr.Truthy(c) {
				commit = pyrepr.Str(c)
			}
			return label + " · commit-build · " + pystr.Slice(commit, 0, 12)
		}
		if tag := stampValue("tag"); pyrepr.Truthy(tag) {
			if selfupdate.IsCanaryTag(tag) {
				return label + " · canary"
			}
			return label + " · stable"
		}
		if stampValue("payload") == "bootstrap" {
			return label + " · installer"
		}
		return label
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
		panic(err)
	}
	if channel != "main" {
		label := base + " · " + channel
		if head := selfupdate.GitStdout([]string{"rev-parse", "HEAD"}, root); head != "" {
			label += " · local " + pystr.Slice(head, 0, 12)
		}
		return label
	}
	state := getGitBannerState(ctx, "")
	if state == nil {
		return base
	}
	if state.Ahead <= 0 || state.Upstream == state.Local {
		return base + " · upstream " + state.Upstream
	}
	return fmt.Sprintf("%s · upstream %s · local %s (+%d carried %s)", base, state.Upstream, state.Local, state.Ahead, plural(state.Ahead, "commit"))
}

// Runs target on its own goroutine with any failure swallowed
func bannerDaemon(target func()) {
	go func() {
		defer func() { _ = recover() }()
		target()
	}()
}

// Starts the passive update check in the background, finishing at once when prefetch is off
func PrefetchUpdateCheck() {
	signal := currentUpdateCheck()
	if skipBackgroundPrefetch() {
		signal.set()
		return
	}
	bannerDaemon(func() {
		status, err := bannerUpdateCheck(context.Background())
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

// Warms git state, the release tag and the skills index in the background, once per process
func PrefetchBannerData() {
	bannerMemoMu.Lock()
	started := bannerPrefetchStarted
	bannerPrefetchStarted = true
	bannerMemoMu.Unlock()
	if started || skipBackgroundPrefetch() {
		return
	}
	bannerDaemon(func() {
		ctx := context.Background()
		for _, warm := range []func(){
			func() { getGitBannerState(ctx, "") },
			func() { getLatestReleaseTag(ctx, "") },
			func() { getAvailableSkills(ctx) },
		} {
			func() {
				defer func() { _ = recover() }()
				warm()
			}()
		}
	})
}

// Commits behind from the prefetched check after waiting up to timeout, nil when unknown or not ready
func getUpdateResult(timeout time.Duration) *int64 {
	currentUpdateCheck().wait(timeout)
	bannerMemoMu.Lock()
	defer bannerMemoMu.Unlock()
	return updateResult
}

// Update warning markup for a non zero behind count, the managed command when the count is unknown
func formatUpdateNotice(ctx context.Context, behind int64) (string, error) {
	h := home.Dir(ctx)
	if behind > 0 {
		command, err := bannerRecommendedUpdate(h)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("[bold yellow]⚠ %d %s behind[/][dim yellow] — run [bold]%s[/bold] to update[/]", behind, plural(int(behind), "commit"), command), nil
	}
	suffix := ""
	if managed := bannerManagedUpdate(h); managed != "" {
		suffix = "[dim yellow] — run [bold]" + managed + "[/bold][/]"
	}
	return "[bold yellow]⚠ update available[/]" + suffix, nil
}

// Rich markup rendered to true color ANSI at the terminal width with no trailing newline
func renderMarkupToANSI(markup string) string {
	var buf bytes.Buffer
	(&pyrich.Console{Out: &buf, Color: true, Terminal: true, NoHighlight: true, Width: pyrich.DetectWidth(), ColorSystem: "truecolor"}).Print(markup)
	return strings.TrimRight(buf.String(), "\n")
}

// Prints the update warning above the prompt once the prefetched check lands, at most once per process
func deferUpdateNotice(ctx context.Context, maxWait time.Duration) {
	bannerMemoMu.Lock()
	started := deferredNoticeStarted
	deferredNoticeStarted = true
	bannerMemoMu.Unlock()
	if started {
		return
	}
	signal := currentUpdateCheck()
	bannerDaemon(func() {
		if !signal.wait(maxWait) {
			return
		}
		bannerMemoMu.Lock()
		behind := updateResult
		bannerMemoMu.Unlock()
		if behind == nil || *behind == 0 {
			return
		}
		notice, err := formatUpdateNotice(ctx, *behind)
		if err != nil {
			return
		}
		cprint(renderMarkupToANSI(notice))
	})
}

// The word, with an s unless n is one
func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// Token count as 128K or 1M, one decimal when it is not within a twentieth of a whole unit
func formatContextLength(tokens int) string {
	for _, unit := range []struct {
		name string
		div  float64
	}{{"M", 1_000_000}, {"K", 1_000}} {
		if float64(tokens) >= unit.div {
			val := float64(tokens) / unit.div
			rounded := math.RoundToEven(val)
			if math.Abs(val-rounded) < 0.05 {
				return strconv.FormatFloat(rounded, 'f', 0, 64) + unit.name
			}
			return strconv.FormatFloat(val, 'f', 1, 64) + unit.name
		}
	}
	return strconv.Itoa(tokens)
}

// Toolset id without its legacy _tools suffix, unknown for an empty id
func displayToolsetName(toolsetName string) string {
	if toolsetName == "" {
		return "unknown"
	}
	return strings.TrimSuffix(toolsetName, "_tools")
}

// Name cut to twenty five characters and an ellipsis once it passes twenty eight
func shortLabel(name string) string {
	if pystr.Len(name) > 28 {
		return pystr.Slice(name, 0, 25) + "..."
	}
	return name
}

// File holding the banner tool panel inputs of the last launch
func bannerSnapshotPath(ctx context.Context) string {
	return filepath.Join(home.CacheDir(home.Dir(ctx)), "banner_snapshot.json")
}

// Fingerprint of config.yaml, .env, the code revision and the checkout the tool panel depends on
func bannerSnapshotFingerprint(ctx context.Context) string {
	h := home.Dir(ctx)
	parts := []string{"v" + strconv.Itoa(bannerSnapshotVersion)}
	for _, p := range []string{home.ConfigPath(h), home.EnvPath(h)} {
		name := filepath.Base(p)
		if st, err := os.Stat(p); err == nil {
			parts = append(parts, fmt.Sprintf("%s:%d:%d", name, st.ModTime().UnixNano(), st.Size()))
		} else {
			parts = append(parts, name+":absent")
		}
	}
	info, err := updaterecord.GetVersionInfo()
	if err != nil {
		panic(err)
	}
	if info.Commit != nil && *info.Commit != "" {
		parts = append(parts, *info.Commit)
	} else {
		parts = append(parts, info.DerivedVersion)
	}
	if state := getGitBannerState(ctx, ""); state != nil {
		parts = append(parts, state.Local)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])
}

// Ordered JSON object of alternating keys and values
func orderedObject(pairs ...any) *pyjson.Object {
	obj := pyjson.NewObject()
	for i := 0; i+1 < len(pairs); i += 2 {
		obj.Set(pairs[i].(string), pairs[i+1])
	}
	return obj
}

// Tool environment of the context, one rooted at the active home when the context carries none
func bannerToolEnv(ctx context.Context) *tools.Env {
	if env := tools.ToolEnvFrom(ctx); env != nil {
		return env
	}
	return &tools.Env{Home: home.Dir(ctx)}
}

// Value under key of a native map or an ordered JSON object, present and isDict reporting what was found
func bannerDictGet(v any, key string) (value any, present, isDict bool) {
	switch d := v.(type) {
	case map[string]any:
		value, present = d[key]
		return value, present, d != nil
	case *pyjson.Object:
		if d == nil {
			return nil, false, false
		}
		value, present = d.Get(key)
		return value, present, true
	case map[string]string:
		s, ok := d[key]
		return s, ok, d != nil
	}
	return nil, false, false
}

// Keys of a native map sorted, or of an ordered JSON object in its own order
func bannerDictKeys(v any) []string {
	switch d := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(d))
		for k := range d {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys
	case map[string][]string:
		keys := make([]string, 0, len(d))
		for k := range d {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys
	case *pyjson.Object:
		if d != nil {
			return append([]string(nil), d.Keys...)
		}
	}
	return nil
}

// Elements of a native or decoded list, false for any other value
func bannerListItems(v any) ([]any, bool) {
	switch l := v.(type) {
	case []any:
		return l, true
	case []string:
		out := make([]any, len(l))
		for i, s := range l {
			out[i] = s
		}
		return out, true
	case []map[string]any:
		out := make([]any, len(l))
		for i, m := range l {
			out[i] = m
		}
		return out, true
	}
	return nil, false
}

// Stored banner snapshot when its fingerprint, toolsets and shape are current, nil otherwise
func loadBannerSnapshot(ctx context.Context, enabledToolsets []string) *pyjson.Object {
	blob := quiet(func() (*pyjson.Object, error) {
		raw, err := os.ReadFile(bannerSnapshotPath(ctx))
		if err != nil {
			return nil, err
		}
		raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
		if !utf8.Valid(raw) {
			return nil, fmt.Errorf("snapshot is not UTF-8")
		}
		doc, err := pyjson.Loads(string(raw))
		if err != nil {
			return nil, err
		}
		obj, _ := doc.(*pyjson.Object)
		return obj, nil
	}, nil)
	if blob == nil {
		return nil
	}
	fp := bannerSnapshotFingerprint(ctx)
	stored, _ := blob.Get("fingerprint")
	if fp == "" || stored != fp {
		return nil
	}
	wanted := append([]string(nil), enabledToolsets...)
	sort.Strings(wanted)
	enabled, _ := blob.Get("enabled_toolsets")
	have, isList := enabled.([]any)
	if !isList || len(have) != len(wanted) {
		return nil
	}
	for i, name := range wanted {
		if have[i] != name {
			return nil
		}
	}
	if toolList, _ := blob.Get("tools"); !isJSONList(toolList) {
		return nil
	}
	for _, key := range []string{"toolset_map", "availability", "skills_by_category"} {
		section, _ := blob.Get(key)
		if obj, ok := section.(*pyjson.Object); !ok || obj == nil {
			return nil
		}
	}
	return blob
}

// True for a decoded JSON array
func isJSONList(v any) bool {
	_, ok := v.([]any)
	return ok
}

// Persists the tool panel inputs of this launch for the next one, any failure leaving the old file
func saveBannerSnapshot(ctx context.Context, toolDefs any, enabledToolsets []string, availability, toolsetMap any) {
	fp := bannerSnapshotFingerprint(ctx)
	if fp == "" {
		return
	}
	enabled := append([]string{}, enabledToolsets...)
	sort.Strings(enabled)
	names := []any{}
	items, _ := bannerListItems(toolDefs)
	for _, item := range items {
		fn, _, isDict := bannerDictGet(item, "function")
		if !isDict {
			continue
		}
		if name, _, _ := bannerDictGet(fn, "name"); pyrepr.Truthy(name) {
			names = append(names, orderedObject("function", orderedObject("name", name)))
		}
	}
	section := func(key string) any {
		if v, present, _ := bannerDictGet(availability, key); present {
			return v
		}
		return []any{}
	}
	listed := func(key string) any {
		items, _ := bannerListItems(section(key))
		return append([]any{}, items...)
	}
	payload := orderedObject("fingerprint", fp, "enabled_toolsets", enabled, "tools", names, "toolset_map", toolsetMap,
		"availability", orderedObject("unavailable_toolsets", section("unavailable_toolsets"),
			"lazy_tools", listed("lazy_tools"), "disabled_tools", listed("disabled_tools")),
		"skills_by_category", getAvailableSkills(ctx))
	_ = quiet(func() (bool, error) {
		path := bannerSnapshotPath(ctx)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return false, err
		}
		return true, config.AtomicJSONWriteMode(ctx, path, payload, 0o600)
	}, false)
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

// One banner line for an MCP server status entry
func mcpServerLine(srv mcp.ServerStatus, dim, text string) string {
	if srv.Connected {
		return fmt.Sprintf("[dim %s]%s[/] [%s](%s)[/] [dim %s]—[/] [%s]%d tool(s)[/]", dim, srv.Name, text, srv.Transport, dim, text, srv.Tools)
	}
	if srv.Status == "lazy" {
		return fmt.Sprintf("[dim %s]%s[/] [%s](%s)[/] [dim %s]—[/] [%s]%d tool(s)[/] [dim %s](lazy, starts on first use)[/]",
			dim, srv.Name, text, srv.Transport, dim, text, srv.Tools, dim)
	}
	status := srv.Status
	if srv.Disabled {
		status = "disabled"
	}
	suffix, known := map[string]string{"disabled": "[dim " + dim + "]— disabled[/]", "connecting": "[yellow]— connecting[/]",
		"configured": "[dim " + dim + "]— configured[/]"}[status]
	if known {
		return fmt.Sprintf("[dim %s]%s[/] [dim](%s)[/] %s", dim, srv.Name, srv.Transport, suffix)
	}
	return mcpFailedLine(srv.Name, srv.Transport, srv.Error)
}

var mcpAuthFailureRE = regexp2.MustCompile(`\b401\b|unauthori[sz]ed`, regexp2.IgnoreCase)

// Failed MCP connect line with the short reason and the exact next command
func mcpFailedLine(name, transport, errText string) string {
	reason := pyrich.Escape(pystr.Slice(strings.Join(pystr.SplitWhitespace(errText, -1), " "), 0, 120))
	if reason == "" {
		reason = "no details recorded"
	}
	nextCmd := "icebath mcp test " + name
	if matched, _ := mcpAuthFailureRE.MatchString(reason); matched {
		nextCmd = "icebath mcp login " + name
	}
	return fmt.Sprintf("[red]%s[/] [dim](%s)[/] [red]— could not connect:[/] %s [dim]— run `%s`[/]", name, transport, reason, nextCmd)
}

// Tool names cut to about forty two columns, the second result true when the tail was elided
func truncateToolNames(toolNames []string) ([]string, bool) {
	if pystr.Len(strings.Join(toolNames, ", ")) <= 45 {
		return append([]string(nil), toolNames...), false
	}
	var short []string
	length := 0
	for _, name := range toolNames {
		if length+pystr.Len(name)+2 > 42 {
			return short, true
		}
		short = append(short, name)
		length += pystr.Len(name) + 2
	}
	return short, false
}

// Skill names joined into avail columns, ending with a count of the ones that did not fit
func packSkillNames(skillNames []string, avail int) string {
	var parts []string
	length := 0
	for i, name := range skillNames {
		needed := pystr.Len(name)
		if len(parts) > 0 {
			needed += 2
		}
		after := len(skillNames) - (i + 1)
		indLen := 0
		if after > 0 {
			indLen = len(fmt.Sprintf(", +%d more", after))
		}
		if len(parts) > 0 && length+needed+indLen > avail {
			parts = append(parts, fmt.Sprintf("+%d more", len(skillNames)-len(parts)))
			break
		}
		parts = append(parts, name)
		length += needed
	}
	return strings.Join(parts, ", ")
}

// Short aggregator model label of a MoA preset, empty when the preset has none
func moaAggregatorLabel(ctx context.Context, presetName string) (string, error) {
	cfg, err := config.Load(home.Dir(ctx), nil)
	if err != nil {
		return "", err
	}
	var raw any = map[string]any{}
	if moa := cfg["moa"]; pyrepr.Truthy(moa) {
		raw = moa
	}
	presets, _, _ := bannerDictGet(provider.NormalizeMoAConfig(raw), "presets")
	preset, _, _ := bannerDictGet(presets, presetName)
	aggregator, _, _ := bannerDictGet(preset, "aggregator")
	model, _, _ := bannerDictGet(aggregator, "model")
	label := ""
	if pyrepr.Truthy(model) {
		label = pyrepr.Str(model)
	}
	return label[strings.LastIndex(label, "/")+1:], nil
}

// True when config.yaml or the plugin key cache names an MCP server, or either probe fails
func mcpConfigured(ctx context.Context) bool {
	native := quiet(func() (bool, error) {
		cfg, err := config.Load(home.Dir(ctx), nil)
		if err != nil {
			return false, err
		}
		return pyrepr.Truthy(cfg["mcp_servers"]), nil
	}, true)
	if native {
		return true
	}
	return quiet(func() (bool, error) { return len(plugins.GetPortableMCPServerNamesNowait(ctx)) > 0, nil }, true)
}

// True when the codex app server runtime is active and builds its own tool list
func codexRuntimeActive(ctx context.Context) (bool, error) {
	cfg, err := config.Load(home.Dir(ctx), nil)
	if err != nil {
		return false, err
	}
	return provider.GetCurrentCodexRuntime(map[string]any(cfg)) == "codex_app_server", nil
}

// Model, working directory and session lines under the hero art
func bannerLeftLines(ctx context.Context, model, cwd, sessionID string, contextLength int, providerName, accent, dim string, contextPinned bool) []string {
	dimSep := func(label string) string { return fmt.Sprintf(" [dim %s]·[/] [dim %s]%s[/]", dim, dim, label) }
	var lines []string
	pin := ""
	if contextPinned {
		pin = " (pinned)"
	}
	ctxStr := ""
	if contextLength != 0 {
		ctxStr = dimSep(formatContextLength(contextLength) + " context" + pin)
	}
	nousStr := dimSep("Nous Research")
	switch stripped := pystr.Strip(model); {
	case strings.ToLower(pystr.Strip(providerName)) == "moa":
		aggStr := ""
		if aggLabel := quiet(func() (string, error) { return bannerMoAAggregator(ctx, model) }, ""); aggLabel != "" {
			aggStr = dimSep("agg " + aggLabel)
		}
		lines = append(lines, fmt.Sprintf("[%s]MoA: %s[/]%s%s%s", accent, shortLabel(model), aggStr, ctxStr, nousStr))
	case stripped == "" || strings.ToLower(stripped) == "unknown":
		lines = append(lines, fmt.Sprintf("[bold red]no model configured[/] [dim %s]— run /model or icebath setup[/]", dim))
	default:
		modelShort := strings.TrimSuffix(model[strings.LastIndex(model, "/")+1:], ".gguf")
		lines = append(lines, fmt.Sprintf("[%s]%s[/]%s%s", accent, shortLabel(modelShort), ctxStr, nousStr))
	}
	if os.Getenv("ICEBATH_YOLO_MODE") != "" {
		lines = append(lines, fmt.Sprintf("[bold red]⚠ YOLO mode[/] [dim %s]— all approval prompts bypassed[/]", dim))
	}
	lines = append(lines, fmt.Sprintf("[dim %s]%s[/]", dim, cwd))
	if sessionID != "" {
		lines = append(lines, fmt.Sprintf("[dim %s]Session: %s[/]", skinColor(ctx, "session_border", "#8B8682"), sessionID))
	}
	return lines
}

// Available Tools section listing up to eight toolsets, each cut to about forty two columns
func bannerToolLines(toolDefs, unavailableToolsets any, toolsetForTool func(string) string, lazyTools, disabledTools map[string]bool, accent, dim, text string) ([]string, error) {
	lines := []string{fmt.Sprintf("[bold %s]Available Tools[/]", accent)}
	toolsets := map[string][]string{}
	defs, _ := bannerListItems(toolDefs)
	for _, def := range defs {
		fn, present, isDict := bannerDictGet(def, "function")
		if !isDict || !present {
			return nil, fmt.Errorf("KeyError: 'function'")
		}
		rawName, present, isDict := bannerDictGet(fn, "name")
		if !isDict || !present {
			return nil, fmt.Errorf("KeyError: 'name'")
		}
		toolName := pyrepr.Str(rawName)
		toolset := toolsetForTool(toolName)
		if toolset == "" {
			toolset = "other"
		}
		toolset = displayToolsetName(toolset)
		toolsets[toolset] = append(toolsets[toolset], toolName)
	}
	unavailable, _ := bannerListItems(unavailableToolsets)
	for _, item := range unavailable {
		id, present, isDict := bannerDictGet(item, "id")
		if !isDict {
			return nil, fmt.Errorf("AttributeError: unavailable toolset entry has no attribute 'get'")
		}
		if !present {
			if id, present, _ = bannerDictGet(item, "name"); !present {
				id = "unknown"
			}
		}
		key := "unknown"
		if pyrepr.Truthy(id) {
			key = displayToolsetName(pyrepr.Str(id))
		}
		names := toolsets[key]
		members, _, _ := bannerDictGet(item, "tools")
		memberList, _ := bannerListItems(members)
		for _, member := range memberList {
			name := pyrepr.Str(member)
			seen := false
			for _, have := range names {
				seen = seen || have == name
			}
			if !seen {
				names = append(names, name)
			}
		}
		toolsets[key] = names
	}
	colorTool := func(name string) string {
		color := text
		if disabledTools[name] {
			color = "red"
		} else if lazyTools[name] {
			color = "yellow"
		}
		return fmt.Sprintf("[%s]%s[/]", color, name)
	}
	sortedToolsets := make([]string, 0, len(toolsets))
	for name := range toolsets {
		sortedToolsets = append(sortedToolsets, name)
	}
	sort.Strings(sortedToolsets)
	for _, toolset := range sortedToolsets[:min(8, len(sortedToolsets))] {
		names := append([]string(nil), toolsets[toolset]...)
		sort.Strings(names)
		shown, elided := truncateToolNames(names)
		colored := make([]string, 0, len(shown)+1)
		for _, name := range shown {
			colored = append(colored, colorTool(name))
		}
		if elided {
			colored = append(colored, "[dim]...[/]")
		}
		lines = append(lines, fmt.Sprintf("[dim %s]%s:[/] %s", dim, toolset, strings.Join(colored, ", ")))
	}
	if len(sortedToolsets) > 8 {
		lines = append(lines, fmt.Sprintf("[dim %s](and %d more toolsets...)[/]", dim, len(sortedToolsets)-8))
	}
	return lines, nil
}

// Skill names of every category as sorted text lists, an error for a category that is not a list
func bannerSkillNames(skillsByCategory any) (map[string][]string, error) {
	out := map[string][]string{}
	if typed, ok := skillsByCategory.(map[string][]string); ok {
		for category, names := range typed {
			out[category] = append([]string{}, names...)
		}
		return out, nil
	}
	for _, category := range bannerDictKeys(skillsByCategory) {
		value, _, _ := bannerDictGet(skillsByCategory, category)
		items, ok := bannerListItems(value)
		if !ok {
			return nil, fmt.Errorf("TypeError: skills of category %s are not a list", pyrepr.Repr(category))
		}
		names := make([]string, len(items))
		for i, item := range items {
			name, isStr := item.(string)
			if !isStr {
				return nil, fmt.Errorf("TypeError: skill name %s is not a string", pyrepr.Repr(item))
			}
			names[i] = name
		}
		out[category] = names
	}
	return out, nil
}

// Available Skills body sized to about sixty percent of the terminal width
func bannerSkillLines(skills map[string][]string, skillsEnabled bool, dim, text string) []string {
	if !skillsEnabled {
		return []string{fmt.Sprintf("[dim %s]Skills toolset disabled[/]", dim)}
	}
	if len(skills) == 0 {
		return []string{fmt.Sprintf("[dim %s]No skills installed[/]", dim)}
	}
	rightColWidth := max(int(float64(bannerTerminalColumns())*0.6)-10, 30)
	categories := make([]string, 0, len(skills))
	for category := range skills {
		categories = append(categories, category)
	}
	sort.Strings(categories)
	var lines []string
	for _, category := range categories {
		names := append([]string(nil), skills[category]...)
		sort.Strings(names)
		packed := packSkillNames(names, max(rightColWidth-pystr.Len(category)-2, 20))
		lines = append(lines, fmt.Sprintf("[dim %s]%s:[/] [%s]%s[/]", dim, category, text, packed))
	}
	return lines
}

// Inputs of the welcome banner, nil Availability computing it and nil SkillsByCategory scanning the skills tree
type welcomeBanner struct {
	Model            string
	Cwd              string
	Tools            any
	EnabledToolsets  []string
	SessionID        string
	ToolsetForTool   func(name string) string
	ContextLength    int
	Provider         string
	Availability     any
	SkillsByCategory any
	ContextPinned    bool
}

// Prints the welcome banner, hero art and model on the left, tools, MCP servers and skills on the right
func buildWelcomeBanner(ctx context.Context, console *pyrich.Console, o welcomeBanner) error {
	toolsetForTool := o.ToolsetForTool
	if toolsetForTool == nil {
		scope := bannerToolEnv(ctx).Home
		toolsetForTool = func(name string) string { return tools.Default.ToolsetForTool(ctx, scope, name) }
	}
	toolDefs, _ := bannerListItems(o.Tools)
	availability := o.Availability
	if availability == nil {
		availability = computeToolsetAvailability(ctx, o.EnabledToolsets)
	}
	enabled := map[string]bool{}
	for _, name := range o.EnabledToolsets {
		enabled[name] = true
	}
	accent := skinColor(ctx, "banner_accent", "#FFBF00")
	dim := skinColor(ctx, "banner_dim", "#B8860B")
	text := skinColor(ctx, "banner_text", "#FFF8DC")
	skin := quiet(func() (*config.SkinConfig, error) { return bannerActiveSkin(ctx), nil }, nil)
	hero, logo := icebathCaduceus, icebathAgentLogo
	if skin != nil {
		if pyrepr.Truthy(skin.BannerHero) {
			custom, isStr := skin.BannerHero.(string)
			if !isStr {
				return fmt.Errorf("TypeError: sequence item 1: expected str instance, %s found", pyrepr.TypeName(skin.BannerHero))
			}
			hero = custom
		}
		if pyrepr.Truthy(skin.BannerLogo) {
			logo = pyrepr.Str(skin.BannerLogo)
		}
	}
	leftLines := append([]string{"", hero, ""}, bannerLeftLines(ctx, o.Model, o.Cwd, o.SessionID, o.ContextLength, o.Provider, accent, dim, o.ContextPinned)...)
	setOf := func(key string) map[string]bool {
		value, _, _ := bannerDictGet(availability, key)
		items, _ := bannerListItems(value)
		set := map[string]bool{}
		for _, item := range items {
			set[pyrepr.Str(item)] = true
		}
		return set
	}
	unavailable, _, _ := bannerDictGet(availability, "unavailable_toolsets")
	rightLines, err := bannerToolLines(toolDefs, unavailable, toolsetForTool, setOf("lazy_tools"), setOf("disabled_tools"), accent, dim, text)
	if err != nil {
		return err
	}
	var mcpStatus []mcp.ServerStatus
	if bannerMCPConfigured(ctx) {
		mcpStatus = quiet(func() ([]mcp.ServerStatus, error) { return bannerMCPStatus(ctx), nil }, nil)
	}
	if len(mcpStatus) > 0 {
		rightLines = append(rightLines, "", fmt.Sprintf("[bold %s]MCP Servers[/]", accent))
		for _, srv := range mcpStatus {
			rightLines = append(rightLines, mcpServerLine(srv, dim, text))
		}
	}
	rightLines = append(rightLines, "", fmt.Sprintf("[bold %s]Available Skills[/]", accent))
	skillsEnabled := len(enabled) == 0 || enabled["skills"]
	var skillsSource any = pyjson.NewObject()
	if skillsEnabled {
		skillsSource = o.SkillsByCategory
		if skillsSource == nil {
			skillsSource = getAvailableSkills(ctx)
		}
	}
	skills, err := bannerSkillNames(skillsSource)
	if err != nil {
		return err
	}
	totalSkills := 0
	for _, names := range skills {
		totalSkills += len(names)
	}
	rightLines = append(rightLines, bannerSkillLines(skills, skillsEnabled, dim, text)...)
	rightLines = append(rightLines, "")
	mcpConnected := 0
	for _, srv := range mcpStatus {
		if srv.Connected {
			mcpConnected++
		}
	}
	summaryParts := []string{fmt.Sprintf("%d tools", len(toolDefs)), fmt.Sprintf("%d skills", totalSkills)}
	if mcpConnected > 0 {
		summaryParts = append(summaryParts, fmt.Sprintf("%d MCP servers", mcpConnected))
	}
	summaryParts = append(summaryParts, "/help for commands")
	if quiet(func() (bool, error) { return bannerCodexRuntime(ctx) }, false) {
		rightLines = append(rightLines, fmt.Sprintf("[bold %s]Runtime:[/] [%s]codex app-server[/] [dim %s](terminal/file ops/MCP run inside codex)[/]", accent, text, dim))
	}
	if profileName := quiet(func() (string, error) { return bannerActiveProfileName(ctx), nil }, ""); profileName != "" && profileName != "default" {
		rightLines = append(rightLines, fmt.Sprintf("[bold %s]Profile:[/] [%s]%s[/]", accent, text, profileName))
	}
	rightLines = append(rightLines, fmt.Sprintf("[dim %s]%s[/]", dim, strings.Join(summaryParts, " · ")))
	if notice := quiet(func() (string, error) {
		behind := getUpdateResult(50 * time.Millisecond)
		if behind == nil {
			if !currentUpdateCheck().isSet() {
				deferUpdateNotice(ctx, 30*time.Second)
			}
			return "", nil
		}
		if *behind == 0 {
			return "", nil
		}
		return formatUpdateNotice(ctx, *behind)
	}, ""); notice != "" {
		rightLines = append(rightLines, notice)
	}
	grid := pyrich.NewGrid(2, pyrich.Column{Header: "left", Justify: "left"}, pyrich.Column{Header: "right", Justify: "left"})
	grid.AddRow(strings.Join(leftLines, "\n"), strings.Join(rightLines, "\n"))
	versionLabel := bannerVersionLabel()
	if release := getLatestReleaseTag(ctx, ""); release != nil {
		versionLabel = "[link=" + release[1] + "]" + versionLabel + "[/link]"
	}
	console.Print()
	if bannerTerminalColumns() >= 95 {
		console.Print(logo)
		console.Print()
	}
	console.PrintPanel(pyrich.Panel{Content: grid, Title: fmt.Sprintf("[bold %s]%s[/]", skinColor(ctx, "banner_title", "#FFD700"), versionLabel),
		BorderStyle: skinColor(ctx, "banner_border", "#CD7F32"), PadX: 2})
	return nil
}
