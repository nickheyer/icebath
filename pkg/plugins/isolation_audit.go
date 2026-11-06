package plugins

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	VerdictHost      = "host"
	VerdictInProcess = "in_process"
	VerdictPortable  = "portable"
)

// Top level packages that are the agent's own process state when a plugin imports them
var IcebathPackages = map[string]bool{
	"agent": true, "tools": true, "hermes_cli": true, "icebath_cli": true, "gateway": true, "cron": true, "providers": true,
	"plugins": true, "tui_gateway": true, "acp_adapter": true, "run_agent": true, "cli": true, "model_tools": true,
	"toolsets": true, "hermes_state": true, "hermes_constants": true, "icebath_constants": true, "hermes_logging": true,
	"hermes_time": true, "utils": true, "registration_lifecycle": true, "hermes_platform": true,
}

var isolationKindReasons = map[string]string{
	"platform": "kind 'platform': gateway platform adapters run in the Icebath process",
}

var (
	isolationStreamingMarkers = []string{"StreamingResponse", "EventSourceResponse", ".websocket(", "WebSocket"}
	isolationSkipDirs         = map[string]bool{"tests": true, "test": true, ".git": true, "__pycache__": true, "node_modules": true, ".venv": true, "venv": true, "docs": true}
)

// Static classification of where a plugin can run, host isolation or in process only
type IsolationReport struct {
	Verdict    string
	Reasons    []string
	Notes      []string
	CtxMethods map[string]bool
}

// What one source file contributes to the audit
type isolationVisit struct {
	Reasons    []string
	Notes      []string
	CtxMethods map[string]bool
}

// Reports whether the plugin runs in the plugin host
func (r *IsolationReport) HostReady() bool {
	return r.Verdict == VerdictHost || r.Verdict == VerdictPortable
}

// One line verdict the validate command prints
func (r *IsolationReport) Summary() string {
	switch r.Verdict {
	case VerdictPortable:
		return "portable plugin (no Python): runs anywhere"
	case VerdictHost:
		return "runs in the plugin host (plugins.isolation: host)"
	}
	return "in-process only: " + strings.Join(r.Reasons, "; ")
}

// JSON document of the report in the upstream key order
func (r *IsolationReport) ToDict() *pyjson.Object {
	strs := func(items []string) []any {
		out := make([]any, len(items))
		for i, s := range items {
			out[i] = s
		}
		return out
	}
	out := pyjson.NewObject()
	out.Set("verdict", r.Verdict)
	out.Set("host_ready", r.HostReady())
	out.Set("reasons", strs(r.Reasons))
	out.Set("notes", strs(r.Notes))
	out.Set("ctx_methods", strs(sortedKeys(r.CtxMethods)))
	return out
}

// Audited Python sources of a plugin as relative slash paths in pathlib order, tests and hidden dirs skipped
func isolationPythonFiles(pluginDir string) []string {
	files := []string{}
	_ = filepath.WalkDir(pluginDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".py") {
			return nil
		}
		rel, err := filepath.Rel(pluginDir, p)
		if err != nil {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		for _, part := range parts[:len(parts)-1] {
			if isolationSkipDirs[part] || strings.HasPrefix(part, ".") {
				return nil
			}
		}
		name := parts[len(parts)-1]
		if strings.HasPrefix(name, "test_") || name == "conftest.py" {
			return nil
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	sort.SliceStable(files, func(i, j int) bool { return lessByParts(files[i], files[j]) })
	return files
}

// Text of a file decoded as utf-8 with replacement, the BOM dropped when sig is set
func readTextReplace(path string, sig bool) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if sig {
		raw = stripBOM(raw)
	}
	return pystr.DecodeUTF8Replace(raw), nil
}

// Classifies one plugin directory without importing it, reading its manifest when none is given
func auditPluginDir(pluginDir string, manifest map[string]any) (*IsolationReport, error) {
	if manifest == nil {
		manifest = readIsolationManifest(pluginDir)
	}
	if !isFile(filepath.Join(pluginDir, "__init__.py")) {
		return &IsolationReport{Verdict: VerdictPortable, Reasons: []string{}, Notes: []string{}, CtxMethods: map[string]bool{}}, nil
	}
	reasons, notes := []string{}, []string{}
	ctxMethods := map[string]bool{}
	kind := ""
	if config.PyTruthy(manifest["kind"]) {
		kind = strings.ToLower(strings.TrimSpace(config.PyStr(manifest["kind"])))
	}
	if reason, ok := isolationKindReasons[kind]; ok {
		reasons = append(reasons, reason)
	}
	dashboard := filepath.Join(pluginDir, "dashboard")
	dashManifest := filepath.Join(dashboard, "manifest.json")
	if isFile(dashManifest) {
		text, err := readTextReplace(dashManifest, false)
		if err != nil {
			return nil, err
		}
		if strings.Contains(text, `"api"`) {
			var api strings.Builder
			var readErr error
			_ = filepath.WalkDir(dashboard, func(p string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() && strings.HasSuffix(d.Name(), ".py") {
					text, err := readTextReplace(p, true)
					if err != nil {
						readErr = err
						return filepath.SkipAll
					}
					api.WriteString(text)
				}
				return nil
			})
			if readErr != nil {
				return nil, readErr
			}
			streams := false
			for _, marker := range isolationStreamingMarkers {
				if strings.Contains(api.String(), marker) {
					streams = true
				}
			}
			if streams {
				reasons = append(reasons, "dashboard backend API streams (SSE/websocket); the host bridge buffers responses")
			} else {
				notes = append(notes, "dashboard backend API is served through the plugin host (buffered responses)")
			}
		}
	}
	for _, rel := range isolationPythonFiles(pluginDir) {
		native := filepath.FromSlash(rel)
		source, err := readTextReplace(filepath.Join(pluginDir, native), true)
		if err != nil {
			return nil, err
		}
		visit, syntaxMsg, err := isolationVisitSource(native, source, kind == "model-provider")
		if err != nil {
			return nil, err
		}
		if visit == nil {
			notes = append(notes, native+": not parseable ("+syntaxMsg+"); not audited")
			continue
		}
		reasons = append(reasons, visit.Reasons...)
		notes = append(notes, visit.Notes...)
		for m := range visit.CtxMethods {
			ctxMethods[m] = true
		}
	}
	verdict := VerdictHost
	if len(reasons) > 0 {
		verdict = VerdictInProcess
	}
	return &IsolationReport{Verdict: verdict, Reasons: dedupeStrings(reasons), Notes: dedupeStrings(notes), CtxMethods: ctxMethods}, nil
}

// Items with later duplicates dropped, first occurrence order kept
func dedupeStrings(items []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range items {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// Native manifest mapping of a plugin directory, empty when missing or unreadable
func readIsolationManifest(pluginDir string) map[string]any {
	for _, name := range []string{"plugin.yaml", "plugin.yml"} {
		path := filepath.Join(pluginDir, name)
		if !isFile(path) {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil || strictUTF8(raw) != nil {
			return map[string]any{}
		}
		doc, err := config.FastSafeLoad(raw)
		if m, ok := doc.(map[string]any); err == nil && ok {
			return m
		}
		return map[string]any{}
	}
	return map[string]any{}
}
