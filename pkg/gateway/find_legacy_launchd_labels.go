package gateway

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	plistutil "github.com/nickheyer/icebath/pkg/utils/plist"
)

// Resolved root home that owns the profile tree, false when it cannot be derived
func resolvedDefaultRoot() (root string, ok bool) {
	defer func() {
		if recover() != nil {
			root, ok = "", false
		}
	}()
	resolved, err := pathutil.Resolve(home.Root())
	return resolved, err == nil
}

// Launchd labels of this install the profile layout cannot derive, credited by the home each plist pins
func LegacyLaunchdLabelsForInstall(ctx context.Context, exclude map[string]bool) []string {
	account, err := serviceLookupAccount()
	if err != nil {
		return []string{}
	}
	root, ok := resolvedDefaultRoot()
	if !ok {
		return []string{}
	}
	agentsDir := filepath.Join(account.HomeDir, "Library", "LaunchAgents")
	if st, err := os.Stat(agentsDir); err != nil || !st.IsDir() {
		return []string{}
	}
	plists, _ := filepath.Glob(filepath.Join(agentsDir, launchdLabelBase+"*.plist"))
	sort.Strings(plists)
	found := map[string]bool{}
	for _, plistPath := range plists {
		data, err := os.ReadFile(plistPath)
		if err != nil {
			continue
		}
		loaded, err := plistutil.Decode(data)
		if err != nil {
			continue
		}
		doc, isDict := loaded.(map[string]any)
		if !isDict {
			continue
		}
		rawLabel, hasLabel := doc["Label"]
		variables, isDict := doc["EnvironmentVariables"].(map[string]any)
		if !hasLabel || !isDict {
			continue
		}
		rawHome, pinsHome := variables["ICEBATH_HOME"]
		if !pinsHome {
			continue
		}
		pinned, err := pathutil.Resolve(pathutil.ExpandUser(anyutil.String(rawHome)))
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(root, pinned)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			continue
		}
		label, isStr := rawLabel.(string)
		if !isStr || exclude[label] || !strings.HasPrefix(label, launchdLabelBase) {
			continue
		}
		parts := []string{}
		if rel != "." {
			parts = strings.Split(rel, string(filepath.Separator))
		}
		if len(parts) == 0 || (len(parts) == 2 && parts[0] == "profiles") {
			found[label] = true
		}
	}
	labels := make([]string, 0, len(found))
	for label := range found {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	return labels
}
