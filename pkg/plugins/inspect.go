package plugins

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"gopkg.in/yaml.v3"
)

const inspectManifestMaxBytes = 256 * 1024

var (
	inspectResultTTL      = 600 * time.Second
	inspectCatalogEntry   = GetLiveCatalogEntry
	inspectRaiseIfRemoved = raiseIfRemoved
	inspectClone          = clonePluginRepo
	inspectionsMu         sync.Mutex
	inspections           = map[[3]string]*inspectionSlot{}
)

// One inspection keyed by home, catalog name and source, finishedAt zero while its clone runs
type inspectionSlot struct {
	done       chan struct{}
	result     map[string]any
	err        error
	finishedAt time.Time
}

var agentPayloadFiles = []string{"__init__.py", filepath.Join("dashboard", "manifest.json")}

// Manifest of a small regular file that parses to a mapping, empty otherwise
func readInspectedManifest(path string, loads func(string) (any, error)) map[string]any {
	if path == "" {
		return map[string]any{}
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > inspectManifestMaxBytes {
		return map[string]any{}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	text, err := pyjson.DecodeBytes(stripBOM(raw))
	if err != nil {
		return map[string]any{}
	}
	data, err := loads(text)
	if err != nil {
		return map[string]any{}
	}
	switch m := data.(type) {
	case *pyjson.Object:
		return m.Vals
	default:
		if out := asMap(data); out != nil {
			return out
		}
	}
	return map[string]any{}
}

func inspectYAMLLoads(text string) (any, error) {
	var doc any
	err := yaml.Unmarshal([]byte(text), &doc)
	return doc, err
}

func inspectManifestName(data map[string]any) string {
	name, ok := data["name"].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(name)
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func isDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// Whether a plugin.yaml folder ships a package, dashboard or manifest only language pack the agent loads
func yamlShipsAgentPayload(root string, manifest map[string]any) bool {
	if slices.ContainsFunc(agentPayloadFiles, func(rel string) bool { return isRegularFile(filepath.Join(root, rel)) }) {
		return true
	}
	return jsonTruthy(manifest["provides_locales"]) && isDirectory(filepath.Join(root, "locales"))
}

// Agent and desktop halves a source tree ships and the agent half name, read as text only
func detectComponents(root, fallbackName string) map[string]any {
	yamlPath := ""
	for _, n := range []string{"plugin.yaml", "plugin.yml"} {
		if isRegularFile(filepath.Join(root, n)) {
			yamlPath = filepath.Join(root, n)
			break
		}
	}
	yamlManifest := readInspectedManifest(yamlPath, inspectYAMLLoads)
	portable := filepath.Join(root, "plugin.json")
	agent := (yamlPath != "" && yamlShipsAgentPayload(root, yamlManifest)) || isRegularFile(portable)
	desktop := isRegularFile(filepath.Join(root, "plugin.js")) || isRegularFile(filepath.Join(root, "desktop", "plugin.js"))
	var agentName any
	if agent {
		name := inspectManifestName(yamlManifest)
		if name == "" {
			name = inspectManifestName(readInspectedManifest(portable, pyjson.Loads))
		}
		if name == "" {
			name = fallbackName
		}
		agentName = name
	}
	return map[string]any{"agent": agent, "desktop": desktop, "agent_name": agentName}
}

// Last real segment of the subdirectory, else the repository name
func desktopFolderName(gitURL, subdir string) string {
	var parts []string
	for _, p := range strings.Split(strings.ReplaceAll(subdir, "\\", "/"), "/") {
		if p != "" && p != "." && p != "desktop" {
			parts = append(parts, p)
		}
	}
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return repoNameFromURL(gitURL)
}

func inspectFailure(message string, warnings []string, insecure bool) map[string]any {
	return map[string]any{"ok": false, "agent": false, "desktop": false, "warnings": warnings, "insecure": insecure, "error": message}
}

func inspectionKey(ctx context.Context, identifier, catalogName string) [3]string {
	return [3]string{home.Dir(ctx), catalogName, identifier}
}

// Which halves a source ships at the commit read, joining a running or recent read of the same request
func InspectPluginSource(ctx context.Context, identifier, catalogName string) (map[string]any, error) {
	key := inspectionKey(ctx, identifier, catalogName)
	now := time.Now()
	inspectionsMu.Lock()
	for k, s := range inspections {
		if !s.finishedAt.IsZero() && now.Sub(s.finishedAt) > inspectResultTTL {
			delete(inspections, k)
		}
	}
	slot, joined := inspections[key]
	if !joined {
		slot = &inspectionSlot{done: make(chan struct{})}
		inspections[key] = slot
	}
	inspectionsMu.Unlock()
	if joined {
		<-slot.done
		if slot.err != nil {
			return nil, slot.err
		}
		return copyInspection(slot.result), nil
	}
	result, err := inspectSource(ctx, identifier, catalogName)
	inspectionsMu.Lock()
	if err == nil && result["ok"] == true {
		slot.finishedAt = time.Now()
	} else {
		delete(inspections, key)
	}
	slot.result, slot.err = result, err
	inspectionsMu.Unlock()
	close(slot.done)
	if err != nil {
		return nil, err
	}
	return copyInspection(result), nil
}

func copyInspection(result map[string]any) map[string]any {
	out := make(map[string]any, len(result))
	for k, v := range result {
		out[k] = v
	}
	out["warnings"] = slices.Clone(result["warnings"].([]string))
	return out
}

// Drops the kept answer for a source in the current home, leaving a read still running to its joiners
func forgetInspection(ctx context.Context, identifier, catalogName string) {
	key := inspectionKey(ctx, identifier, catalogName)
	inspectionsMu.Lock()
	defer inspectionsMu.Unlock()
	if s, ok := inspections[key]; ok && !s.finishedAt.IsZero() {
		delete(inspections, key)
	}
}

func inspectSource(ctx context.Context, identifier, catalogName string) (map[string]any, error) {
	revision := ""
	var names []string
	if catalogName != "" {
		entry := inspectCatalogEntry(ctx, catalogName)
		if entry == nil {
			return inspectFailure("'"+catalogName+"' is not in the Icebath plugin catalog.", []string{}, false), nil
		}
		identifier, revision, names = entry.InstallIdentifier(), entry.Sha, []string{entry.Name}
	}
	gitURL, subdir, err := resolveGitURL(identifier)
	if err != nil {
		return inspectFailure(err.Error(), []string{}, false), nil
	}
	insecure := strings.HasPrefix(gitURL, "http://") || strings.HasPrefix(gitURL, "file://")
	warnings := []string{}
	if insecure {
		warnings = append(warnings, "This URL uses an insecure or local scheme. Prefer https:// or git@ for production installs.")
	}
	found, sha, err := inspectClonedTree(ctx, identifier, gitURL, revision, subdir, names)
	var opErr *PluginOperationError
	if errors.As(err, &opErr) {
		return inspectFailure(opErr.Error(), warnings, insecure), nil
	}
	if err != nil {
		return nil, err
	}
	if found["agent"] != true && found["desktop"] != true {
		return inspectFailure("No agent or desktop plugin artifacts found in this repository.", warnings, insecure), nil
	}
	result := map[string]any{"ok": true, "desktop_name": nil, "sha": sha, "warnings": warnings, "insecure": insecure}
	for k, v := range found {
		result[k] = v
	}
	if found["desktop"] == true {
		result["desktop_name"] = desktopFolderName(gitURL, subdir)
	}
	return result, nil
}

// Clones a source into a temporary directory outside every profile and detects its halves
func inspectClonedTree(ctx context.Context, identifier, gitURL, revision, subdir string, names []string) (map[string]any, string, error) {
	if err := inspectRaiseIfRemoved(ctx, append([]string{identifier, gitURL}, names...)...); err != nil {
		return nil, "", err
	}
	tmp, err := os.MkdirTemp("", "icebath-plugin-inspect-")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(tmp)
	clone := filepath.Join(tmp, "plugin")
	sha, err := inspectClone(ctx, home.Dir(ctx), clone, gitURL, revision, subdir)
	if err != nil {
		return nil, "", err
	}
	root := clone
	fallback := ""
	if subdir != "" {
		if root, err = resolveSubdirWithin(clone, subdir); err != nil {
			return nil, "", err
		}
		trimmed := strings.TrimRight(subdir, "/")
		fallback = trimmed[strings.LastIndex(trimmed, "/")+1:]
	} else {
		fallback = repoNameFromURL(gitURL)
	}
	return detectComponents(root, fallback), sha, nil
}
