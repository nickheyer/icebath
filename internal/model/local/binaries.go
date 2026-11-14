package local

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/hostinfo"
	"github.com/nickheyer/icebath/internal/toolchain"
)

var backendPackages = map[string]string{
	"cuda":   toolchain.LlamaCUDA,
	"vulkan": toolchain.LlamaVulkan,
	"metal":  toolchain.LlamaMetal,
	"hip":    toolchain.LlamaHIP,
	"cpu":    toolchain.LlamaCPU,
}

func BackendPackage(backend string) (string, bool) {
	name, ok := backendPackages[backend]
	return name, ok
}

type BinaryResolutionError struct{ Reason string }

func (e *BinaryResolutionError) Error() string { return e.Reason }

type Engine struct {
	Backend string
	Tag     string
	Binary  string
}

func (r *Runtime) RuntimesRoot() string { return home.LlamaCppRuntimes.In(r.root) }

func (r *Runtime) PinnedTag(backend string) (string, error) {
	name, ok := backendPackages[backend]
	if !ok {
		return "", &BinaryResolutionError{Reason: "unknown backend " + backend}
	}
	version, err := r.tools.Version(name)
	if err != nil || version == "" {
		return "", &BinaryResolutionError{Reason: fmt.Sprintf("llama.cpp %s has no toolchain version pin", backend)}
	}
	return "b" + version, nil
}

func SelectBackend(gpuVendor string, macOS bool) string {
	if macOS {
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

func backendCandidates(requested, gpuVendor string, target toolchain.Target) ([]string, error) {
	if requested != "auto" {
		if _, ok := backendPackages[requested]; !ok {
			return nil, &BinaryResolutionError{Reason: "unknown backend " + requested}
		}
		return []string{requested}, nil
	}
	out := []string{SelectBackend(gpuVendor, strings.HasPrefix(string(target), "darwin-"))}
	fallback := []string{"cpu"}
	if gpuVendor != "" {
		fallback = []string{"vulkan", "cpu"}
	}
	for _, b := range fallback {
		if b != out[0] && (len(out) < 2 || out[1] != b) {
			out = append(out, b)
		}
	}
	return out, nil
}

func (r *Runtime) detectGPUVendor(ctx context.Context) string {
	if reading, err := r.host.NvidiaSMI(ctx); err == nil && reading.Name != "" {
		return "nvidia " + reading.Name
	}
	switch class := r.host.GPUClass(); class {
	case hostinfo.GPUNvidia, hostinfo.GPUAMD, hostinfo.GPUIntel:
		return string(class)
	}
	return ""
}

func (r *Runtime) UnavailableReason(ctx context.Context, backend string) (string, error) {
	name, ok := backendPackages[backend]
	if !ok {
		return "unknown backend " + backend, nil
	}
	return r.tools.Unavailable(ctx, name)
}

func (r *Runtime) ResolveBackend(ctx context.Context, requested string) (string, error) {
	var vendor string
	if requested == "auto" {
		vendor = r.detectGPUVendor(ctx)
	}
	target, err := toolchain.HostTarget(ctx, r.host)
	if err != nil {
		return "", err
	}
	candidates, err := backendCandidates(requested, vendor, target)
	if err != nil {
		return "", err
	}
	var reasons []string
	for _, backend := range candidates {
		reason, err := r.UnavailableReason(ctx, backend)
		if err != nil {
			return "", err
		}
		if reason == "" {
			return backend, nil
		}
		reasons = append(reasons, reason)
	}
	return "", &BinaryResolutionError{Reason: strings.Join(reasons, "; ")}
}

func (r *Runtime) installedAmong(ctx context.Context, candidates []string, allowOutdated bool) (*Engine, error) {
	for _, candidate := range candidates {
		inst, err := r.tools.Installed(ctx, backendPackages[candidate], allowOutdated)
		if errors.Is(err, toolchain.ErrNotInstalled) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if inst.Binary != "" {
			return &Engine{Backend: candidate, Tag: "b" + inst.Version, Binary: inst.Binary}, nil
		}
	}
	return nil, nil
}

func (r *Runtime) InstalledEngine(ctx context.Context, backend string, allowOutdated bool) (*Engine, error) {
	var vendor string
	if backend == "auto" {
		vendor = r.detectGPUVendor(ctx)
	}
	target, err := toolchain.HostTarget(ctx, r.host)
	if err != nil {
		return nil, err
	}
	candidates, err := backendCandidates(backend, vendor, target)
	if err != nil {
		return nil, err
	}
	engine, err := r.installedAmong(ctx, candidates, allowOutdated)
	if err != nil || engine != nil {
		return engine, err
	}
	for _, candidate := range candidates {
		if r.adoptLegacyEngine(ctx, candidate) {
			return r.installedAmong(ctx, candidates, allowOutdated)
		}
	}
	return nil, nil
}

type legacyManifest struct {
	Tag             string          `json:"tag"`
	Backend         string          `json:"backend"`
	VerifiedVersion any             `json:"verified_version"`
	RawAssets       json.RawMessage `json:"assets"`
}

func (r *Runtime) legacyInstalls(backend string) []toolchain.LegacyInstall {
	matches, _ := filepath.Glob(filepath.Join(r.RuntimesRoot(), "b*", backend, "manifest.json"))
	type found struct {
		number  int
		install toolchain.LegacyInstall
	}
	var list []found
	for _, manifestPath := range matches {
		dir := filepath.Dir(manifestPath)
		tag := filepath.Base(filepath.Dir(dir))
		number, err := strconv.Atoi(strings.TrimPrefix(tag, "b"))
		if err != nil {
			continue
		}
		text, err := os.ReadFile(manifestPath)
		if err != nil {
			continue
		}
		var m legacyManifest
		if json.Unmarshal(bytes.TrimPrefix(text, []byte("\xef\xbb\xbf")), &m) != nil {
			continue
		}
		var assets map[string]any
		if json.Unmarshal(m.RawAssets, &assets) != nil || assets == nil {
			continue
		}
		if m.Tag != tag || m.Backend != backend || !jsonTruthy(m.VerifiedVersion) {
			continue
		}
		r.adoptMu.Lock()
		left := r.leftInPlace[dir]
		r.adoptMu.Unlock()
		if left {
			continue
		}
		shas := map[string]string{}
		for name, v := range assets {
			if s, ok := v.(string); ok {
				shas[name] = s
			}
		}
		list = append(list, found{number, toolchain.LegacyInstall{Version: strconv.Itoa(number), Dir: dir, Assets: shas}})
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].number != list[j].number {
			return list[i].number > list[j].number
		}
		return list[i].install.Dir > list[j].install.Dir
	})
	out := make([]toolchain.LegacyInstall, len(list))
	for i, f := range list {
		out[i] = f.install
	}
	return out
}

func (r *Runtime) adoptLegacyEngine(ctx context.Context, backend string) bool {
	installs := r.legacyInstalls(backend)
	if len(installs) == 0 {
		return false
	}
	r.adoptMu.Lock()
	defer r.adoptMu.Unlock()
	adopted, refused, err := r.tools.AdoptLegacy(ctx, backendPackages[backend], installs)
	for _, dir := range refused {
		r.leftInPlace[dir] = true
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("could not move the pre-store llama.cpp %s engine into the toolchain store: %v", backend, err))
		return false
	}
	return adopted
}

func (r *Runtime) EnsureEngine(ctx context.Context, backend string, progress func(toolchain.Progress)) (*Engine, error) {
	resolved, err := r.ResolveBackend(ctx, backend)
	if err != nil {
		return nil, err
	}
	if _, err := r.tools.Ensure(ctx, backendPackages[resolved], toolchain.EnsureOptions{Explicit: true, Progress: progress}); err != nil {
		return nil, err
	}
	engine, err := r.InstalledEngine(ctx, resolved, false)
	if err != nil {
		return nil, err
	}
	if engine == nil {
		return nil, &BinaryResolutionError{Reason: fmt.Sprintf("llama.cpp %s install has no usable pinned binary", resolved)}
	}
	return engine, nil
}
