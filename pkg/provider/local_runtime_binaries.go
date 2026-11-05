package provider

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
)

var BackendPackages = map[string]string{
	"cuda":   "llamacpp-cuda",
	"vulkan": "llamacpp-vulkan",
	"metal":  "llamacpp-metal",
	"hip":    "llamacpp-hip",
	"cpu":    "llamacpp-cpu",
}

// BinaryResolutionError means the requested backend has no pinned or installed engine
type BinaryResolutionError struct{ Message string }

func (e *BinaryResolutionError) Error() string { return e.Message }

type Engine struct {
	Backend string
	Tag     string
	Binary  string
}

// Lockfile path and package verification behind seams so tests can pin a disposable lock and skip llama-server runs
var (
	llamaLockfilePath = pm.LockfilePath
	llamaVerify       = func(p pm.Package, entry, target string) string { return p.Verify(entry, target) }
)

// Machine scoped presets and server state directory, binaries live in the PM store
func RuntimesRoot() string {
	return filepath.Join(home.Root(), "runtimes", "llamacpp")
}

// Returns b plus the PM pinned llama.cpp version of backend
func PinnedTag(backend string) (string, error) {
	version, ok := pm.NewLockfile(llamaLockfilePath()).Version(BackendPackages[backend])
	if !ok {
		return "", &BinaryResolutionError{fmt.Sprintf("llama.cpp %s has no PM version pin", backend)}
	}
	return "b" + version, nil
}

// Picks metal on macOS, cuda for NVIDIA, vulkan for AMD or Intel, and cpu otherwise, empty osName detects the host
func SelectBackend(gpuVendor, osName string) (string, error) {
	if osName == "" {
		target, err := pm.CurrentTarget()
		if err != nil {
			return "", err
		}
		osName = "other"
		if strings.HasPrefix(target, "darwin-") {
			osName = "macos"
		}
	}
	return selectBackendFor(gpuVendor, osName), nil
}

func selectBackendFor(gpuVendor, osName string) string {
	if osName == "macos" {
		return "metal"
	}
	vendor := strings.ToLower(gpuVendor)
	if strings.Contains(vendor, "nvidia") {
		return "cuda"
	}
	for _, name := range []string{"amd", "intel", "radeon", "arc"} {
		if strings.Contains(vendor, name) {
			return "vulkan"
		}
	}
	return "cpu"
}

// Explicit backends are the only candidate, auto prefers the hardware pick then vulkan with a GPU then cpu
func llamaBackendCandidates(requested, gpuVendor, target string) ([]string, error) {
	if requested != "auto" {
		if _, ok := BackendPackages[requested]; !ok {
			return nil, &BinaryResolutionError{"unknown backend " + requested}
		}
		return []string{requested}, nil
	}
	osName := "other"
	if strings.HasPrefix(target, "darwin-") {
		osName = "macos"
	}
	ordered := []string{selectBackendFor(gpuVendor, osName)}
	if gpuVendor != "" {
		ordered = append(ordered, "vulkan", "cpu")
	} else {
		ordered = append(ordered, "cpu")
	}
	var out []string
	seen := map[string]bool{}
	for _, backend := range ordered {
		if !seen[backend] {
			seen[backend] = true
			out = append(out, backend)
		}
	}
	return out, nil
}

// Why backend cannot run on target, empty when built upstream and pinned, empty target meaning this host
func UnavailableReason(backend, target string) (string, error) {
	name, ok := BackendPackages[backend]
	if !ok {
		return "unknown backend " + backend, nil
	}
	if target == "" {
		current, err := pm.CurrentTarget()
		if err != nil {
			return "", err
		}
		target = current
	}
	pkg, err := pm.GetPackage(name)
	if err != nil {
		return "", err
	}
	if reason := pkg.MissingReason(target); reason != "" {
		return reason, nil
	}
	lock := pm.NewLockfile(llamaLockfilePath())
	if version, ok := lock.Version(name); !ok || version == "" || len(lock.Artifacts(name, target)) == 0 {
		return fmt.Sprintf("%s is not pinned for %s", name, target), nil
	}
	return "", nil
}

// Strict for explicit backends, auto detects the GPU without vendor or target and falls back to compatible pins
func ResolveBackend(requested string, gpuVendor *string, target string) (string, error) {
	vendor := ""
	if gpuVendor != nil {
		vendor = *gpuVendor
	} else if requested == "auto" && target == "" {
		detected, err := detectGPUVendor()
		if err != nil {
			return "", err
		}
		vendor = detected
	}
	if target == "" {
		current, err := pm.CurrentTarget()
		if err != nil {
			return "", err
		}
		target = current
	}
	candidates, err := llamaBackendCandidates(requested, vendor, target)
	if err != nil {
		return "", err
	}
	var reasons []string
	for _, backend := range candidates {
		reason, err := UnavailableReason(backend, target)
		if err != nil {
			return "", err
		}
		if reason == "" {
			return backend, nil
		}
		reasons = append(reasons, reason)
	}
	return "", &BinaryResolutionError{strings.Join(reasons, "; ")}
}

// First candidate whose PM entry exists and carries a binary
func installedLlamaEngine(ctx context.Context, candidates []string, allowOutdated bool) (*Engine, error) {
	for _, candidate := range candidates {
		found, err := pm.Installed(ctx, BackendPackages[candidate], allowOutdated)
		if err != nil {
			return nil, err
		}
		if found != nil && found.Binary != "" {
			return &Engine{Backend: candidate, Tag: "b" + found.Version, Binary: found.Binary}, nil
		}
	}
	return nil, nil
}

// Installed engine for backend, moving a pre PM engine into the store once and never downloading, nil when none
func InstalledEngine(ctx context.Context, backend string, allowOutdated bool) (*Engine, error) {
	vendor := ""
	if backend == "auto" {
		detected, err := detectGPUVendor()
		if err != nil {
			return nil, err
		}
		vendor = detected
	}
	target, err := pm.CurrentTarget()
	if err != nil {
		return nil, err
	}
	candidates, err := llamaBackendCandidates(backend, vendor, target)
	if err != nil {
		return nil, err
	}
	engine, err := installedLlamaEngine(ctx, candidates, allowOutdated)
	if err != nil || engine != nil {
		return engine, err
	}
	for _, candidate := range candidates {
		if AdoptLegacyEngine(ctx, candidate) {
			return installedLlamaEngine(ctx, candidates, allowOutdated)
		}
	}
	return nil, nil
}

var (
	legacySHA256       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	legacyAdoptionMu   sync.Mutex
	legacyLeftInPlace  = map[string]bool{}
	legacyLeftInPlaceM sync.Mutex
)

type legacyInstall struct {
	number   int
	version  string
	path     string
	manifest map[string]any
}

func legacyIsLeftInPlace(path string) bool {
	legacyLeftInPlaceM.Lock()
	defer legacyLeftInPlaceM.Unlock()
	return legacyLeftInPlace[path]
}

// Verified pre PM installs of backend with matching tag and backend manifests, newest first
func legacyInstalls(backend string) []legacyInstall {
	paths, _ := filepath.Glob(filepath.Join(RuntimesRoot(), "b*", backend, "manifest.json"))
	var found []legacyInstall
	for _, manifestPath := range paths {
		dir := filepath.Dir(manifestPath)
		tag := filepath.Base(filepath.Dir(dir))
		number, ok := pythonIntText(strings.TrimPrefix(tag, "b"))
		if !ok {
			continue
		}
		raw, err := os.ReadFile(manifestPath)
		if err != nil || !utf8.Valid(raw) {
			continue
		}
		decoded, ok := moaJSONLoads(strings.TrimPrefix(string(raw), "\uFEFF"))
		if !ok {
			continue
		}
		manifest, isMap := decoded.(map[string]any)
		if !isMap || manifest["tag"] != tag || manifest["backend"] != backend || !pyTruthy(manifest["verified_version"]) {
			continue
		}
		if _, assets := manifest["assets"].(map[string]any); !assets || legacyIsLeftInPlace(dir) {
			continue
		}
		found = append(found, legacyInstall{number: number, version: strconv.Itoa(number), path: dir, manifest: manifest})
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].number != found[j].number {
			return found[i].number > found[j].number
		}
		return found[i].path > found[j].path
	})
	return found
}

// Parses text the way Python int does for a decimal string, surrounding whitespace, sign and digit underscores allowed
func pythonIntText(text string) (int, bool) {
	text = strings.TrimSpace(text)
	digits := strings.TrimLeft(text, "+-")
	if len(text)-len(digits) > 1 || digits == "" || strings.HasPrefix(digits, "_") || strings.HasSuffix(digits, "_") ||
		strings.Contains(digits, "__") {
		return 0, false
	}
	n, err := strconv.Atoi(text[:len(text)-len(digits)] + strings.ReplaceAll(digits, "_", ""))
	return n, err == nil
}

// The release asset names PM pins for an engine version, in archive order
func llamaAssetNames(p pm.Package, version, target string) ([]string, error) {
	urls, err := p.FetchURLs(version, target)
	if err != nil {
		return nil, err
	}
	prefix := "https://github.com/ggml-org/llama.cpp/releases/download/b" + version + "/"
	var names []string
	for _, u := range urls {
		if strings.HasPrefix(u, prefix) {
			names = append(names, strings.TrimPrefix(u, prefix))
		}
	}
	return names, nil
}

// The manifest digests in PM archive order, nil when any engine asset lacks a sha256
func legacyArtifacts(p pm.Package, version, target string, assets map[string]any) ([]string, error) {
	names, err := llamaAssetNames(p, version, target)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, nil
	}
	shas := make([]string, 0, len(names))
	for _, name := range names {
		sha, ok := assets[name].(string)
		if !ok || !legacySHA256.MatchString(sha) {
			return nil, nil
		}
		shas = append(shas, sha)
	}
	return shas, nil
}

// Takes PM's store lock with a two second wait, false when another PM operation holds it
func llamaStoreLock(root string) (held bool, release func(), err error) {
	if err := os.MkdirAll(root, 0o777); err != nil {
		return false, nil, err
	}
	f, err := os.OpenFile(filepath.Join(root, ".install.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, nil, err
	}
	held, err = pm.LockFD(f, true, 2*time.Second)
	if err != nil {
		f.Close()
		return false, nil, err
	}
	return held, func() { f.Close() }, nil
}

// Moves the newest verified pre PM install of backend into PM's store and records it, never fails, true when moved
func AdoptLegacyEngine(ctx context.Context, backend string) bool {
	moved, err := adoptLegacyEngine(ctx, backend)
	if err != nil {
		slog.Warn(fmt.Sprintf("could not move the pre-PM llama.cpp %s engine into the PM store: %v", backend, err))
		return false
	}
	return moved
}

func adoptLegacyEngine(ctx context.Context, backend string) (bool, error) {
	candidates := legacyInstalls(backend)
	if len(candidates) == 0 {
		return false, nil
	}
	name := BackendPackages[backend]
	p, err := pm.GetPackage(name)
	if err != nil {
		return false, err
	}
	target, err := pm.CurrentTarget()
	if err != nil {
		return false, err
	}
	root, err := pm.WritableStoreRoot(ctx)
	if err != nil {
		return false, err
	}
	storeRoot, err := pm.StoreRoot(ctx)
	if err != nil {
		return false, err
	}
	factsPath := filepath.Join(root, "facts.json")
	if root == storeRoot {
		if factsPath, err = pm.FactsPath(ctx); err != nil {
			return false, err
		}
	}
	legacyAdoptionMu.Lock()
	defer legacyAdoptionMu.Unlock()
	held, release, err := llamaStoreLock(root)
	if err != nil {
		return false, err
	}
	defer release()
	if !held {
		return false, nil
	}
	if existing, err := pm.Installed(ctx, name, true); err != nil || existing != nil {
		return false, err
	}
	for _, c := range candidates {
		moved, err := adoptLegacyInstall(p, c, target, root, factsPath)
		if err != nil {
			return false, err
		}
		if moved {
			return true, nil
		}
		legacyLeftInPlaceM.Lock()
		legacyLeftInPlace[c.path] = true
		legacyLeftInPlaceM.Unlock()
	}
	return false, nil
}

// Renames one verified legacy install into its store entry and records its facts, restoring it when recording fails
func adoptLegacyInstall(p pm.Package, c legacyInstall, target, root, factsPath string) (bool, error) {
	assets, _ := c.manifest["assets"].(map[string]any)
	artifacts, err := legacyArtifacts(p, c.version, target, assets)
	if err != nil {
		return false, err
	}
	entryName := p.StoreEntry(c.version, target)
	entry := filepath.Join(root, entryName)
	if artifacts == nil {
		return false, nil
	}
	if _, err := os.Lstat(entry); err == nil {
		return false, nil
	}
	if reason := llamaVerify(p, c.path, target); reason != "" {
		slog.Warn(fmt.Sprintf("pre-PM llama.cpp at %s left in place: %s", c.path, reason))
		return false, nil
	}
	if err := os.Rename(c.path, entry); err != nil {
		slog.Warn(fmt.Sprintf("pre-PM llama.cpp at %s left in place: %v", c.path, err))
		return false, nil
	}
	if err := recordAdoptedEngine(p, c.version, entryName, entry, target, root, factsPath, artifacts); err != nil {
		_ = os.Rename(entry, c.path)
		return false, err
	}
	_ = os.Remove(filepath.Dir(c.path))
	slog.Info(fmt.Sprintf("moved llama.cpp b%s (%s) from %s into the PM store", c.version, p.Base().Name, c.path))
	return true, nil
}

func recordAdoptedEngine(p pm.Package, version, entryName, entry, target, root, factsPath string, artifacts []string) error {
	facts, err := pm.NewFacts(factsPath, false)
	if err != nil {
		return err
	}
	digest, err := pm.TreeDigest(entry)
	if err != nil {
		return err
	}
	return facts.Record(p.Base().Name, version, entryName, p.Env(entry, target), root,
		pm.FactRecord{Target: &target, Artifacts: artifacts, Digest: &digest})
}

// EnsureEngineOptions carries the install job's progress callbacks and pause switch
type EnsureEngineOptions struct {
	Progress         pm.InstallProgress
	PauseEvent       *pm.PauseEvent
	DownloadProgress pm.ProgressFunc
}

// Installs the resolved backend's pinned engine through PM and returns the current engine, only deliberate jobs call it
func EnsureEngine(ctx context.Context, backend string, opts EnsureEngineOptions) (Engine, error) {
	resolved, err := ResolveBackend(backend, nil, "")
	if err != nil {
		return Engine{}, err
	}
	if _, err := pm.Ensure(ctx, BackendPackages[resolved], pm.EnsureOptions{Explicit: true, Progress: opts.Progress,
		PauseEvent: opts.PauseEvent, DownloadProgress: opts.DownloadProgress}); err != nil {
		return Engine{}, err
	}
	engine, err := InstalledEngine(ctx, resolved, false)
	if err != nil {
		return Engine{}, err
	}
	if engine == nil {
		return Engine{}, &BinaryResolutionError{fmt.Sprintf("llama.cpp %s install has no usable pinned binary", resolved)}
	}
	return *engine, nil
}
