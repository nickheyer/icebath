package plugins

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Install provenance class of a plugin directory
type ProvenanceClass string

const (
	ProvenanceGit           ProvenanceClass = "git"
	ProvenanceManual        ProvenanceClass = "manual"
	ProvenanceSelfCloned    ProvenanceClass = "self-cloned"
	ProvenanceDrift         ProvenanceClass = "drift"
	gitOriginTimeout                        = 10 * time.Second
	installMetadataFileName                 = ".install-metadata.json"
)

// Provenance of one installed plugin dir with its sidecar row, origin and saved feed tag
type Provenance struct {
	Name           string
	Class          ProvenanceClass
	Path           string
	Row            map[string]any
	OriginURL      string
	SavedUpdateURL string
}

// Install metadata rows of a plugins dir, empty when absent, unreadable or not an object
func ReadSidecarRows(pluginsDir string) map[string]any {
	sidecar := filepath.Join(pluginsDir, installMetadataFileName)
	if st, err := os.Stat(sidecar); err != nil || !st.Mode().IsRegular() {
		return map[string]any{}
	}
	raw, err := os.ReadFile(sidecar)
	if err != nil {
		return map[string]any{}
	}
	raw = stripBOM(raw)
	if !utf8.Valid(raw) {
		return map[string]any{}
	}
	data, err := pyjson.Loads(string(raw))
	if err != nil {
		return map[string]any{}
	}
	if rows, ok := pyjson.Plain(data).(map[string]any); ok {
		return rows
	}
	return map[string]any{}
}

// Origin URL of an installed clone through git, else parsed from its .git/config
func gitOriginURL(ctx context.Context, pluginDir string) string {
	if gitExe := resolveGitExecutable(); gitExe != "" {
		runCtx, cancel := context.WithTimeout(ctx, gitOriginTimeout)
		cmd := exec.CommandContext(runCtx, gitExe, "-C", pluginDir, "remote", "get-url", "origin")
		out, err := cmd.Output()
		cancel()
		if err == nil {
			if url := pystr.Strip(strings.ToValidUTF8(string(out), "\uFFFD")); url != "" {
				return url
			}
		}
	}
	config := filepath.Join(pluginDir, ".git", "config")
	if st, err := os.Stat(config); err != nil || !st.Mode().IsRegular() {
		return ""
	}
	raw, err := os.ReadFile(config)
	if err != nil {
		return ""
	}
	inOrigin := false
	for _, line := range pystr.Splitlines(strings.TrimPrefix(strings.ToValidUTF8(string(raw), "\uFFFD"), "\uFEFF"), false) {
		stripped := pystr.Strip(line)
		if stripped == `[remote "origin"]` {
			inOrigin = true
			continue
		}
		if strings.HasPrefix(stripped, "[") {
			inOrigin = false
			continue
		}
		if inOrigin && strings.HasPrefix(stripped, "url") {
			_, value, _ := strings.Cut(stripped, "=")
			return pystr.Strip(value)
		}
	}
	return ""
}

// Classifies every visible plugin dir by sidecar row and git checkout presence
func PluginsProvenance(ctx context.Context, pluginsDir string) []Provenance {
	rows := ReadSidecarRows(pluginsDir)
	out := []Provenance{}
	entries, err := os.ReadDir(pluginsDir)
	if err != nil {
		return out
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		dir := filepath.Join(pluginsDir, entry.Name())
		if st, err := os.Stat(dir); err != nil || !st.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		_, gitErr := os.Stat(filepath.Join(dir, ".git"))
		hasGit := gitErr == nil
		row, _ := rows[entry.Name()].(map[string]any)
		hasRow := len(row) > 0
		class := ProvenanceManual
		switch {
		case hasRow && hasGit:
			class = ProvenanceGit
		case hasRow:
			class = ProvenanceDrift
		case hasGit:
			class = ProvenanceSelfCloned
		}
		if !hasRow {
			row = nil
		}
		prov := Provenance{Name: entry.Name(), Class: class, Path: dir, Row: row}
		if class == ProvenanceSelfCloned {
			prov.OriginURL = gitOriginURL(ctx, dir)
		}
		if row != nil && jsonTruthy(row["update_url"]) {
			prov.SavedUpdateURL, _ = row["update_url"].(string)
		}
		out = append(out, prov)
	}
	return out
}
