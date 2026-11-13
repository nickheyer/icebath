package plugins

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/fnmatch"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/tools"
	"gopkg.in/yaml.v3"
)

// Failure of the real plugin runtime to load the Doctor target
type doctorLoadError struct{ msg string }

func (e *doctorLoadError) Error() string { return e.msg }

// Explicit path or plugin id that names no plugin
type PluginPathNotFoundError struct{ Msg string }

func (e *PluginPathNotFoundError) Error() string { return e.Msg }

// RuntimeError a guest network request raises while Plugin Doctor runs
func doctorDenyNetwork() error {
	return &HostBuiltinError{Type: "RuntimeError", Message: "network access is disabled while Plugin Doctor runs"}
}

// Whether this host serves a Plugin Doctor load whose guests must not reach the network
func (h *PluginHost) networkDenied() bool { return h.envOverride[HostDenyNetworkEnv] == "1" }

var doctorManifestNames = []string{"plugin.yaml", "plugin.yml", "plugin.json"}

var doctorStagingSkippedDirs = []string{".git", "__pycache__", ".pytest_cache"}

// Tree copy the staging step runs, swapped by tests to observe or fail the copy
var doctorCopyTree = func(src, dst string, ignore pyfs.IgnoreErrFunc) error {
	return pyfs.CopyTreeAbortable(src, dst, false, false, ignore)
}

// Copies the plugin into staging, refusing any directory that leads into the destination
func doctorStageCopy(source, staged string) error {
	dest, err := pypath.Resolve(staged)
	if err != nil {
		return err
	}
	refuseOverlap := func(path string) error {
		real, err := pypath.Resolve(path)
		if err != nil {
			return err
		}
		_, destInside := pypath.PathRelativeTo(dest, real)
		_, realInside := pypath.PathRelativeTo(real, dest)
		if destInside || realInside {
			return &doctorLoadError{fmt.Sprintf("Refusing to stage %s: %s leads into Doctor's temporary directory "+
				"%s, so the copy would recurse into itself. Point TMPDIR outside "+
				"the plugin and remove links to it.", source, path, pypath.PathParent(staged))}
		}
		return nil
	}
	ignore := func(directory string, names []string) (map[string]bool, error) {
		for _, name := range names {
			if isDir(filepath.Join(directory, name)) {
				if err := refuseOverlap(pypath.PathJoin(directory, name)); err != nil {
					return nil, err
				}
			}
		}
		skipped := map[string]bool{}
		for _, pattern := range append(append([]string{}, doctorStagingSkippedDirs...), "*.pyc") {
			for _, name := range fnmatch.Filter(names, pattern) {
				skipped[name] = true
			}
		}
		return skipped, nil
	}
	if err := refuseOverlap(source); err != nil {
		return err
	}
	return doctorCopyTree(source, staged, ignore)
}

// What one Doctor load registered, read while the throwaway runtime is still alive
type doctorHost struct {
	manifest            *Manifest
	manager             *Manager
	registeredTools     []string
	registeredHooks     []string
	registeredProviders []string
}

// Loads one staged plugin under a temporary home with network denied, then restores every registry it touched
func doctorRuntime(ctx context.Context, pluginPath string, body func(*doctorHost) error) (err error) {
	tmp, err := os.MkdirTemp("", "icebath-plugin-doctor-")
	if err != nil {
		return err
	}
	defer pyfs.CloseTempDir(tmp, &err)
	bundled := filepath.Join(tmp, "bundled-plugins")
	pluginsRoot := filepath.Join(tmp, "plugins")
	for _, dir := range []string{bundled, pluginsRoot} {
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return err
		}
	}
	stagingRoot := filepath.Join(pluginsRoot, filepath.Base(pluginPath))
	staged := stagingRoot
	if doctorHasManifest(pluginPath) {
		if err := doctorStageCopy(pluginPath, staged); err != nil {
			return err
		}
	} else {
		members := doctorCategoryPlugins(pluginPath)
		if len(members) != 1 {
			names := make([]string, len(members))
			for i, member := range members {
				names[i] = filepath.Base(member)
			}
			return &doctorLoadError{fmt.Sprintf("%s holds %d plugins (%s); point Doctor at one of them",
				pluginPath, len(members), strings.Join(names, ", "))}
		}
		staged = filepath.Join(stagingRoot, filepath.Base(members[0]))
		if err := doctorStageCopy(members[0], staged); err != nil {
			return err
		}
	}
	hctx := home.WithOverride(ctx, tmp)
	manager := NewManager(tmp)
	host, err := manager.pluginHost()
	if err != nil {
		return err
	}
	host.envOverride = map[string]string{home.EnvHome: tmp, "ICEBATH_BUNDLED_PLUGINS": bundled,
		EnvEnableProjectPlugins: "0", HostDenyNetworkEnv: "1"}
	scope := manager.home
	entriesBefore := map[string]*tools.Def{}
	for _, name := range tools.Default.AllToolNames(hctx, scope) {
		entriesBefore[name] = tools.Default.SnapshotRegistration(scope, name)
	}
	policyModule := ""
	var policyBefore *tools.PluginOverridePolicy
	defer func() {
		manager.Unload(nil)
		names := map[string]bool{}
		for name := range entriesBefore {
			names[name] = true
		}
		for _, name := range tools.Default.AllToolNames(hctx, scope) {
			names[name] = true
		}
		for name := range names {
			current := tools.Default.SnapshotRegistration(scope, name)
			if current != nil && current != entriesBefore[name] {
				tools.Default.RestoreRegistration(name, current, entriesBefore[name], scope)
			}
		}
		if policyModule != "" {
			if current := tools.Default.SnapshotPluginOverridePolicy(policyModule, scope); current != policyBefore {
				tools.Default.RestorePluginOverridePolicy(policyModule, current, policyBefore, scope)
			}
		}
		host.Shutdown()
	}()
	manifests := scanDirectory(pluginsRoot, "user", nil, "", 0)
	if len(manifests) == 0 {
		return &doctorLoadError{fmt.Sprintf("Icebath discovery found no valid plugin manifest under %s", stagingRoot)}
	}
	if len(manifests) != 1 {
		return &doctorLoadError{fmt.Sprintf("Expected one plugin manifest, discovered %d under %s", len(manifests), stagingRoot)}
	}
	mf := manifests[0]
	if mf.Kind == "model-provider" {
		registered, err := doctorLoadModelProvider(hctx, staged)
		if err != nil {
			return err
		}
		return body(&doctorHost{manifest: mf, manager: manager, registeredProviders: registered})
	}
	policyModule = manager.policyModuleName(mf)
	policyBefore = tools.Default.SnapshotPluginOverridePolicy(policyModule, scope)
	if err := manager.loadPlugin(hctx, mf); err != nil {
		return err
	}
	manager.mu.RLock()
	loaded := manager.plugins[mf.ID()]
	var loadedCopy LoadedPlugin
	if loaded != nil {
		loadedCopy = *loaded
	}
	manager.mu.RUnlock()
	if loaded == nil {
		return &doctorLoadError{"Plugin registration produced no runtime record"}
	}
	if loadedCopy.Error != "" {
		return &doctorLoadError{"Plugin registration failed: " + loadedCopy.Error}
	}
	if !loadedCopy.Enabled {
		return &doctorLoadError{"Plugin registration did not enable the runtime record"}
	}
	toolNames := append([]string{}, loadedCopy.ToolsRegistered...)
	sort.Strings(toolNames)
	return body(&doctorHost{manifest: mf, manager: manager, registeredTools: toolNames,
		registeredHooks: append([]string{}, loadedCopy.HooksRegistered...)})
}

// Module name a home provider plugin imports under, the provider registry formula with an empty home key
func doctorProviderModuleName(pluginDir string) string {
	sum := sha1.Sum([]byte(""))
	return "_icebath_user_provider_" + hex.EncodeToString(sum[:])[:10] + "_" + strings.ReplaceAll(filepath.Base(pluginDir), "-", "_")
}

// Provider profile names a model-provider plugin registers at import, imported in a fresh network denied child
func doctorLoadModelProvider(ctx context.Context, pluginDir string) ([]string, error) {
	payload, err := cachedProfileExtraction(ctx, pluginDir, doctorProviderModuleName(pluginDir))
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	if reason := payload["error"]; config.PyTruthy(reason) {
		logWarn(fmt.Sprintf("Failed to load user provider plugin %s in the plugin host: %s", filepath.Base(pluginDir), config.PyStr(reason)))
	} else {
		for _, raw := range listOrEmpty(payload["profiles"]) {
			if entry, ok := raw.(map[string]any); ok {
				names[config.PyStr(entry["name"])] = true
			}
		}
	}
	if len(names) == 0 {
		return nil, &doctorLoadError{"model-provider plugin registered no ProviderProfile at import (see the warning above)"}
	}
	return sortedSet(names), nil
}

// One Doctor finding at error or warning level
type DoctorFinding struct {
	Level   string
	Message string
}

// Plugin Doctor result for one target
type PluginDoctorReport struct {
	Path                string
	Manifest            *Manifest
	Findings            []DoctorFinding
	RegisteredTools     []string
	RegisteredHooks     []string
	RegisteredProviders []string
}

// True when no finding is an error
func (r *PluginDoctorReport) OK() bool {
	for _, f := range r.Findings {
		if f.Level == "error" {
			return false
		}
	}
	return true
}

func (r *PluginDoctorReport) addError(message string) {
	r.Findings = append(r.Findings, DoctorFinding{Level: "error", Message: message})
}

func (r *PluginDoctorReport) addWarning(message string) {
	r.Findings = append(r.Findings, DoctorFinding{Level: "warning", Message: message})
}

// Human report with the manifest line, every finding, the verdict and the registration counts
func (r *PluginDoctorReport) FormatText() string {
	lines := []string{"Plugin Doctor: " + r.Path}
	if r.Manifest != nil {
		version := r.Manifest.Version
		if version == "" {
			version = "(no version)"
		}
		lines = append(lines, fmt.Sprintf("  manifest: %s %s (%s)", r.Manifest.Name, version, r.Manifest.Kind))
	}
	for _, f := range r.Findings {
		level := "WARN"
		if f.Level == "error" {
			level = "ERROR"
		}
		lines = append(lines, fmt.Sprintf("  %s: %s", level, f.Message))
	}
	if r.OK() {
		lines = append(lines, "  OK: runtime discovery, manifest parsing, import, and registration passed")
	}
	counts := fmt.Sprintf("  registrations: %d tool(s), %d hook(s)", len(r.RegisteredTools), len(r.RegisteredHooks))
	if len(r.RegisteredProviders) > 0 {
		counts += ", provider(s): " + strings.Join(r.RegisteredProviders, ", ")
	}
	return strings.Join(append(lines, counts), "\n")
}

func doctorHasManifest(path string) bool {
	for _, name := range doctorManifestNames {
		if _, err := os.Lstat(filepath.Join(path, name)); err == nil {
			return true
		}
	}
	return false
}

// Subdirectories that could be plugins, skipping what discovery or the staging copy skips
func doctorCandidateDirs(path string) []string {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		child := filepath.Join(path, e.Name())
		if !isDir(child) || isSkippedPluginDir(e.Name()) {
			continue
		}
		skipped := false
		for _, name := range doctorStagingSkippedDirs {
			skipped = skipped || e.Name() == name
		}
		if !skipped {
			out = append(out, child)
		}
	}
	sort.Strings(out)
	return out
}

// Plugin subdirectories of a manifest-less category, empty unless every candidate holds a manifest
func doctorCategoryPlugins(path string) []string {
	children := doctorCandidateDirs(path)
	for _, child := range children {
		if !doctorHasManifest(child) {
			return nil
		}
	}
	return children
}

func doctorHoldsPlugin(path string) bool {
	return isDir(path) && (doctorHasManifest(path) || len(doctorCategoryPlugins(path)) > 0)
}

// Relative id without dot components that can name an installed plugin
func isDoctorPluginID(raw string) bool {
	if raw == "" || pypath.IsAbs(raw) {
		return false
	}
	anchor, parts := pypath.PathParts(raw)
	if anchor != "" || len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

// Explicit plugin path or installed, bundled or project plugin id resolved to its directory
func ResolvePluginPath(ctx context.Context, target string) (string, error) {
	raw := target
	if raw == "" {
		raw = "."
	}
	direct := pypath.Expanduser(raw)
	directIsDir := isDir(direct)
	if directIsDir && doctorHoldsPlugin(direct) {
		return pypath.Resolve(direct)
	}
	var candidates []string
	if isDoctorPluginID(raw) {
		candidates = append(candidates, filepath.Join(home.Dir(ctx), "plugins", raw))
		if bundled := bundledOverrideDir(); bundled != "" {
			candidates = append(candidates, filepath.Join(bundled, raw), filepath.Join(bundled, "platforms", raw),
				filepath.Join(bundled, "model-providers", raw))
		}
		if cwd, err := os.Getwd(); err == nil {
			candidates = append(candidates, filepath.Join(cwd, ".icebath", "plugins", raw))
		}
	}
	for _, candidate := range candidates {
		if doctorHoldsPlugin(candidate) {
			return pypath.Resolve(candidate)
		}
	}
	if directIsDir {
		var nested []string
		for _, c := range doctorCandidateDirs(direct) {
			if doctorHasManifest(c) {
				nested = append(nested, pypath.Join(raw, filepath.Base(c)))
			}
		}
		resolved, err := pypath.Resolve(direct)
		if err != nil {
			return "", err
		}
		hint := "Point Doctor at a plugin directory."
		if len(nested) > 0 {
			hint = fmt.Sprintf("Point Doctor at a plugin directory inside it: %s.", strings.Join(nested, ", "))
		}
		return "", &PluginPathNotFoundError{fmt.Sprintf("%s holds no plugin manifest (%s), and %s is not an installed plugin id. %s",
			resolved, strings.Join(doctorManifestNames, ", "), config.PyRepr(raw), hint)}
	}
	return "", &PluginPathNotFoundError{fmt.Sprintf("Plugin %s was not found as a path or installed plugin id", config.PyRepr(raw))}
}

var (
	pyDepDistSplit = regexp.MustCompile(`[<>=!~\[;\s]`)
	pyDepUpperPin  = regexp.MustCompile(`<|==|~=`)
)

// Manifest v2 checks of versions, plugin dependencies, Python dependencies and the config schema
func checkManifestV2(r *PluginDoctorReport, mf *Manifest) {
	if mf.ManifestVersion > SupportedManifestVersion {
		r.addWarning(fmt.Sprintf("manifest_version %d is newer than this Icebath supports (%d); unknown fields are ignored",
			mf.ManifestVersion, SupportedManifestVersion))
	}
	if mf.APIVersion != nil && *mf.APIVersion < 1 {
		r.addWarning(fmt.Sprintf("api_version %d is not a valid API generation (>= 1)", *mf.APIVersion))
	}
	for _, dep := range mf.RequiresPlugins {
		if dep.ID == "" {
			vr := "None"
			if dep.VersionRange != "" {
				vr = config.PyRepr(dep.VersionRange)
			}
			r.addWarning(fmt.Sprintf("requires_plugins entry {'id': %s, 'version_range': %s} has no plugin id", config.PyRepr(dep.ID), vr))
			continue
		}
		if dep.VersionRange != "" {
			r.addWarning(fmt.Sprintf("requires plugin %s (%s) — version ranges are advisory; a missing dependency logs a warning at load",
				config.PyRepr(dep.ID), dep.VersionRange))
		}
	}
	for _, key := range configSchemaKeys(mf) {
		spec := mf.ConfigSchema[key]
		stype, ok := spec["type"]
		if !ok || stype == nil {
			continue
		}
		if _, known := ConfigSchemaTypes[strings.ToLower(config.PyStr(stype))]; !known {
			r.addWarning(fmt.Sprintf("config_schema key %s declares unknown type %s", config.PyRepr(key), config.PyRepr(stype)))
		}
	}
}

// Config schema keys in the order the manifest file lists them, keys it does not show sorted after
func configSchemaKeys(mf *Manifest) []string {
	keys := []string{}
	seen := map[string]bool{}
	for _, key := range manifestSchemaOrder(mf.Path) {
		if _, ok := mf.ConfigSchema[key]; ok && !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	rest := map[string]bool{}
	for key := range mf.ConfigSchema {
		if !seen[key] {
			rest[key] = true
		}
	}
	return append(keys, sortedSet(rest)...)
}

// Keys of config_schema, else settings, in the manifest file's own order
func manifestSchemaOrder(dir string) []string {
	for _, name := range doctorManifestNames {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var doc yaml.Node
		if yaml.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &doc) != nil || len(doc.Content) == 0 {
			return nil
		}
		top := doc.Content[0]
		for _, field := range []string{"config_schema", "settings"} {
			for i := 0; i+1 < len(top.Content); i += 2 {
				if top.Content[i].Value != field || top.Content[i+1].Kind != yaml.MappingNode {
					continue
				}
				var order []string
				for j := 0; j+1 < len(top.Content[i+1].Content); j += 2 {
					order = append(order, top.Content[i+1].Content[j].Value)
				}
				return order
			}
		}
		return nil
	}
	return nil
}

// Warns when the Desktop app runs a stale copy of this package's desktop plugin.js
func checkDesktopHalfCopy(ctx context.Context, r *PluginDoctorReport, path string) {
	sourceDir := desktopSource(path)
	if sourceDir == "" {
		return
	}
	source := filepath.Join(sourceDir, "plugin.js")
	rel, _ := filepath.Rel(path, source)
	shown := filepath.ToSlash(rel)
	appRoot := filepath.Join(home.RootOf(home.Dir(ctx)), "desktop-plugins")
	copies, err := filepath.Glob(filepath.Join(appRoot, "*", ".icebath-package.json"))
	if err != nil {
		return
	}
	sort.Strings(copies)
	sourceParent, err := pypath.Resolve(filepath.Dir(source))
	if err != nil {
		return
	}
	for _, markerFile := range copies {
		raw, err := os.ReadFile(markerFile)
		if err != nil {
			continue
		}
		var marker map[string]any
		if json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &marker) != nil {
			continue
		}
		markerSource, ok := marker["source"].(string)
		if !ok {
			continue
		}
		if resolved, err := pypath.Resolve(markerSource); err != nil || resolved != sourceParent {
			continue
		}
		copied, err := os.ReadFile(filepath.Join(filepath.Dir(markerFile), "plugin.js"))
		if err != nil {
			continue
		}
		current, err := os.ReadFile(source)
		if err != nil {
			continue
		}
		if bytes.Equal(copied, current) {
			return
		}
		r.addWarning(fmt.Sprintf("Desktop runs a stale copy of %s (%s); your edits are not "+
			"loaded. Refresh it with Capabilities → Plugins → Rescan in the Desktop app (or restart it). "+
			"To develop in place, symlink the checkout into plugins/ instead: the Desktop then re-syncs "+
			"the copy on every save.", shown, filepath.Dir(markerFile)))
		return
	}
}

// Raw manifest list field, the list itself or ok false when the value is not a list
func rawManifestList(mf *Manifest, keys ...string) ([]any, bool) {
	for _, key := range keys {
		if value, present := mf.Raw[key]; present {
			list, ok := value.([]any)
			return list, ok
		}
	}
	return []any{}, true
}

// Validates one plugin through the real scanner and registration path, an error for an interrupt or an unresolvable path
func doctorPlugin(ctx context.Context, target string) (*PluginDoctorReport, error) {
	path, err := ResolvePluginPath(ctx, target)
	var notFound *PluginPathNotFoundError
	if err != nil && !errors.As(err, &notFound) {
		return nil, err
	}
	if err != nil {
		raw := target
		if raw == "" {
			raw = "."
		}
		report := &PluginDoctorReport{Path: pypath.PathStr(pypath.Expanduser(raw))}
		report.addError(err.Error())
		return report, nil
	}
	report := &PluginDoctorReport{Path: path}
	runErr := doctorRuntime(ctx, path, func(host *doctorHost) error {
		report.Manifest = host.manifest
		report.RegisteredTools = host.registeredTools
		report.RegisteredHooks = host.registeredHooks
		report.RegisteredProviders = host.registeredProviders
		declaredHooks, ok := rawManifestList(host.manifest, "provides_hooks", "hooks")
		if !ok {
			report.addError("provides_hooks must be a list")
			declaredHooks = nil
		}
		declaredTools, ok := rawManifestList(host.manifest, "provides_tools")
		if !ok {
			report.addError("provides_tools must be a list")
			declaredTools = nil
		}
		for _, name := range declaredHooks {
			if s, isStr := name.(string); !isStr {
				report.addError("provides_hooks entries must be strings")
			} else if !validHooks[s] {
				report.addError(fmt.Sprintf("unknown hook %s in provides_hooks", config.PyRepr(s)))
			}
		}
		for _, hookName := range doctorHookOrder(host) {
			if !validHooks[hookName] {
				report.addError(fmt.Sprintf("registered unknown hook %s", config.PyRepr(hookName)))
			}
			host.manager.mu.RLock()
			entries := append([]*hookEntry{}, host.manager.hooks[hookName]...)
			host.manager.mu.RUnlock()
			for _, entry := range entries {
				if !entry.acceptsVarKwargs() {
					report.addError(fmt.Sprintf("hook callback %s for %s must accept **kwargs for forward compatibility",
						config.PyRepr(entry.name), config.PyRepr(hookName)))
				}
			}
		}
		for _, check := range []struct {
			kind       string
			declared   []any
			registered []string
		}{{"hook", declaredHooks, host.registeredHooks}, {"tool", declaredTools, host.registeredTools}} {
			declaredNames, registeredNames := map[string]bool{}, map[string]bool{}
			for _, name := range check.declared {
				if s, isStr := name.(string); isStr {
					declaredNames[s] = true
				}
			}
			for _, name := range check.registered {
				registeredNames[name] = true
			}
			for _, name := range sortedSet(declaredNames) {
				if !registeredNames[name] {
					report.addWarning(fmt.Sprintf("manifest declares %s %s but registration did not add it", check.kind, config.PyRepr(name)))
				}
			}
			for _, name := range sortedSet(registeredNames) {
				if !declaredNames[name] {
					report.addWarning(fmt.Sprintf("registration adds %s %s not listed in provides_%ss", check.kind, config.PyRepr(name), check.kind))
				}
			}
		}
		checkManifestV2(report, host.manifest)
		return nil
	})
	var loadErr *doctorLoadError
	switch {
	case runErr == nil:
	case errors.As(runErr, &loadErr):
		report.addError(loadErr.msg)
	case errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded):
		return nil, runErr
	default:
		report.addError(fmt.Sprintf("unexpected validation failure: %s: %s", doctorErrorClass(runErr), doctorErrorText(runErr)))
	}
	checkDesktopHalfCopy(ctx, report, path)
	return report, nil
}

// Hook names in the order the plugin registered them, then any other stored hook name sorted
func doctorHookOrder(host *doctorHost) []string {
	seen := map[string]bool{}
	var order []string
	for _, name := range host.registeredHooks {
		if !seen[name] {
			seen[name] = true
			order = append(order, name)
		}
	}
	host.manager.mu.RLock()
	rest := map[string]bool{}
	for name := range host.manager.hooks {
		if !seen[name] {
			rest[name] = true
		}
	}
	host.manager.mu.RUnlock()
	return append(order, sortedSet(rest)...)
}

// Python exception class an unexpected staging or load failure surfaces as
func doctorErrorClass(err error) string {
	var tree *pyfs.CopyTreeError
	if errors.As(err, &tree) {
		return "Error"
	}
	if isOSError(err) {
		return pypath.OSErrorClass(err)
	}
	var typed interface{ PyTypeName() string }
	if errors.As(err, &typed) {
		return typed.PyTypeName()
	}
	return "RuntimeError"
}

func doctorErrorText(err error) string {
	if isOSError(err) {
		var tree *pyfs.CopyTreeError
		if !errors.As(err, &tree) {
			return pypath.OSErrorText(err)
		}
	}
	return err.Error()
}
