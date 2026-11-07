package plugins

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Recovers a shipped caller's publication row through the boot journal owner
func RecoverPluginPublication(ctx context.Context, project string, row map[string]any, journal string) error {
	return pm.RecoverPluginPublication(ctx, project, row, journal)
}

// Whether a plugin tree is part of the effective enabled selection, counting the tree being installed
func activePluginTree(ctx context.Context, target string, installing bool) (bool, error) {
	opts := pm.EnabledPluginsOptions{}
	if installing {
		opts.Installing = target
	}
	dirs, err := enabledPluginDirs(ctx, opts)
	if err != nil {
		return false, err
	}
	resolved := resolvePath(target)
	for _, dir := range dirs {
		if resolvePath(dir) == resolved {
			return true, nil
		}
	}
	return false, nil
}

// How a staged tree is published and whose consent it already carries
type publishOptions struct {
	TargetDigest   *string
	RequireConsent bool
	AssumeConsent  bool
}

// Publishes a staged tree over its target and the metadata change through one PM sync
func publishPlugin(ctx context.Context, staged, target string, oldMetadata, newMetadata map[string]any, opts publishOptions) error {
	_, statErr := os.Stat(target)
	exists := statErr == nil
	if opts.RequireConsent && !opts.AssumeConsent {
		active, err := activePluginTree(ctx, target, true)
		if err != nil {
			return err
		}
		if active {
			manifest, err := ReadManifestForInstall(staged)
			if err != nil {
				return err
			}
			consented, reason := installPluginPythonDeps(ctx, manifest, staged, NewConsole(os.Stdout), false)
			if !consented {
				outcome := "Install declined: %s. Nothing was installed."
				if exists {
					outcome = "Reinstall declined: %s. The installed plugin and active environment are unchanged."
				}
				class := "other"
				if reason != nil && reason.FailureClass != "" {
					class = reason.FailureClass
				}
				return &PluginOperationError{Msg: fmt.Sprintf(outcome, reason.Text), FailureClass: class}
			}
		}
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	var digest any
	if opts.TargetDigest != nil {
		digest = *opts.TargetDigest
	} else if exists {
		if digest, err = pm.TreeDigest(target); err != nil {
			return err
		}
	}
	return syncVenv(ctx, pm.SyncVenvOptions{Explicit: true, Plugins: &pm.StagedUpdate{Data: map[string]any{
		"staged": resolvePath(staged), "target": absTarget, "old_metadata": oldMetadata,
		"new_metadata": newMetadata, "target_digest": digest,
	}}})
}

// Effective enabled plugin trees, swapped by tests that pin the selection the way hermes patches it
var enabledPluginDirs = pm.EnabledPluginDirs

// Venv publication of a staged plugin, swapped by tests that act inside the window before its lock
var syncVenv = pm.SyncVenv

// Node sidecar install of a plugin tree, swapped by tests that stand in for npm
var installNodeSidecar = pm.InstallNodeSidecar

// File bytes, nil when the file cannot be read
func bytesOrNil(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return data
}

func isDirPath(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func isFilePath(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// Rebuilds a moved Node sidecar and gates newly declared Python dependencies like an install does
func refreshDeclaredDependencies(ctx context.Context, target, staged string, manifest map[string]any, interactive bool) error {
	if isDirPath(filepath.Join(target, "node_modules")) && isFilePath(filepath.Join(staged, "package.json")) {
		moved := !isDirPath(filepath.Join(staged, "node_modules"))
		for _, name := range []string{"package.json", "package-lock.json"} {
			if !bytes.Equal(bytesOrNil(filepath.Join(target, name)), bytesOrNil(filepath.Join(staged, name))) {
				moved = true
			}
		}
		if moved {
			if reason := installNodeSidecar(ctx, staged, true); reason != "" {
				return &PluginOperationError{Msg: fmt.Sprintf("Node dependencies could not be refreshed: %s. "+
					"The installed plugin and active environment are unchanged.", reason)}
			}
		}
	}
	active, err := activePluginTree(ctx, target, true)
	if err != nil || !active {
		return err
	}
	beforeDecl, err := pm.ReadPythonDeclaration(target)
	if err != nil {
		return err
	}
	afterDecl, err := pm.ReadPythonDeclaration(staged)
	if err != nil {
		return err
	}
	before, err := beforeDecl.InstallRequirements()
	if err != nil {
		return err
	}
	after, err := afterDecl.InstallRequirements()
	if err != nil {
		return err
	}
	had, err := beforeDecl.InstallsPackages()
	if err != nil {
		return err
	}
	has, err := afterDecl.InstallsPackages()
	if err != nil {
		return err
	}
	previous := toSet(before)
	added := []string{}
	for _, spec := range after {
		if !previous[spec] {
			added = append(added, spec)
		}
	}
	if len(added) == 0 && !(has && !had) {
		return nil
	}
	if !interactive {
		shown := strings.Join(added, ", ")
		if shown == "" {
			shown = "in its pyproject.toml"
		}
		return &PluginOperationError{Msg: fmt.Sprintf("The update declares new Python dependencies (%s); "+
			"run `icebath plugins update %s` in a terminal to review them.", shown, filepath.Base(target))}
	}
	name := filepath.Base(target)
	if v, ok := manifest["name"]; ok {
		name = pyrepr.Str(v)
	}
	consented, reason := consentPythonDeps(ctx, name, added, NewConsole(os.Stdout), false)
	if !consented {
		return &PluginOperationError{Msg: fmt.Sprintf("Update declined: %s. The installed plugin and active environment are unchanged.", reason.Text)}
	}
	return nil
}

// Copy ignore rule dropping bytecode caches
func ignorePycache(_ string, names []string) map[string]bool {
	out := map[string]bool{}
	for _, name := range names {
		if name == "__pycache__" {
			out[name] = true
		}
	}
	return out
}

// Update inputs, the catalog entry selects a re pin and its absence a git pull
type updatePluginOptions struct {
	CatalogEntry   *PluginCatalogEntry
	Interactive    bool
	Force          bool
	ScanDecision   func(scan any) bool
	CarryUserFiles func(staged string) ([]string, error)
}

// Prepares a catalog re pin or a custom git pull in a staged copy and publishes it when it changed
func updatePlugin(ctx context.Context, target string, opts updatePluginOptions) (string, error) {
	h := home.Dir(ctx)
	target = resolvePath(target)
	name := filepath.Base(target)
	metadata, err := readInstallMetadata(h)
	if err != nil {
		return "", err
	}
	record := map[string]any{}
	if row, ok := metadata[name].(map[string]any); ok {
		for k, v := range row {
			record[k] = v
		}
	}
	entry := opts.CatalogEntry
	if entry == nil && jsonTruthy(record["pinned"]) {
		return "", &PluginOperationError{Msg: fmt.Sprintf("Plugin '%s' is pinned; reinstall with an explicit --ref to change it.", name)}
	}
	source := ""
	if entry != nil {
		source = entry.InstallIdentifier()
	} else if jsonTruthy(record["source"]) {
		source = pyrepr.Str(record["source"])
	}
	if source == "" || (entry == nil && !isDirPath(filepath.Join(target, ".git"))) {
		return "", &PluginOperationError{Msg: fmt.Sprintf("Plugin '%s' has no owned Git checkout; reinstall from its source.", name)}
	}
	feedRevision := ""
	if entry == nil {
		checked := checkLocalProvenance(Provenance{Name: name, Class: ProvenanceGit, Path: target, Row: record})
		if checked.NeedsFixing != "" {
			return "", &PluginOperationError{Msg: checked.NeedsFixing}
		}
		if jsonTruthy(record["update_url"]) {
			text, err := defaultFetch(ctx, pyrepr.Str(record["update_url"]))
			if err != nil {
				return "", err
			}
			feed, err := parseFeedYml(text)
			if err != nil {
				return "", err
			}
			artifacts, _ := feed["artifacts"].(map[string]any)
			proposed, _ := artifacts["git"].(string)
			bare, _, _ := strings.Cut(source, "#")
			if exactCommitRE.MatchString(proposed) {
				feedRevision = strings.ToLower(proposed)
			} else if proposed != source && proposed != bare {
				return "", &PluginOperationError{Msg: "Update feed must select a commit or the recorded Git source."}
			}
			if floor, ok := feed["min_icebath"]; ok {
				if err := checkManifestVersion(h, map[string]any{"requires_icebath": floor}, name); err != nil {
					return "", err
				}
			}
		}
	}
	if err := refuseIfInstalledRemoved(h, name, target); err != nil {
		return "", err
	}
	before, err := pm.TreeDigest(target)
	if err != nil {
		return "", err
	}
	directory, err := os.MkdirTemp(filepath.Dir(target), ".update-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(directory)
	output, err := stageAndPublishUpdate(ctx, h, target, filepath.Join(directory, "plugin"), source, feedRevision, before, metadata, record, opts)
	var opErr *PluginOperationError
	if err != nil && !errors.As(err, &opErr) {
		return "", &PluginOperationError{Msg: fmt.Sprintf("Plugin '%s' update was not published: %v", name, err)}
	}
	return output, err
}

// Body of an update inside its temporary directory, any failure left for the caller to classify
func stageAndPublishUpdate(ctx context.Context, h, target, staged, source, feedRevision, before string,
	metadata, record map[string]any, opts updatePluginOptions) (string, error) {
	entry := opts.CatalogEntry
	var output, revision string
	if entry != nil {
		gitURL, subdir, err := resolveGitURL(source)
		if err != nil {
			return "", err
		}
		if revision, err = clonePluginRepo(ctx, h, staged, gitURL, entry.Sha, ""); err != nil {
			return "", err
		}
		if subdir != "" {
			if staged, err = resolveSubdirWithin(staged, subdir); err != nil {
				return "", err
			}
		}
		output = "Updated to catalog pin " + revision
		catalogRecord := map[string]any{"name": entry.Name, "repo": entry.Repo, "tier": entry.Tier, "pin": entry.Sha, "sha": revision}
		canonical, err := canonicalSource(gitURL, subdir)
		if err != nil {
			return "", err
		}
		record["catalog"], record["pinned"], record["source"] = catalogRecord, true, canonical
		delete(record, "catalog_name")
		delete(record, "catalog_tier")
		writeCatalogSidecarRecord(staged, catalogRecord, revision)
	} else {
		if err := pyfs.CopyTree(target, staged, true, false, ignorePycache); err != nil {
			return "", err
		}
		if feedRevision != "" {
			gitExe := resolveGitExecutable()
			status, err := gitOrRaise(ctx, gitExe, target, "Could not inspect plugin edits: ", gitCall{}, "status", "--porcelain")
			if err != nil {
				return "", err
			}
			if strings.TrimSpace(status.Stdout) != "" {
				return "", &PluginOperationError{Msg: "Pinned feed update has local changes; save them before updating."}
			}
			if err := checkoutExactRevision(ctx, h, staged, gitExe, feedRevision, ""); err != nil {
				return "", err
			}
			output = "Updated to feed commit " + feedRevision
		} else {
			ok, pulled, err := gitPullPluginDir(ctx, staged)
			if err != nil {
				return "", err
			}
			if !ok {
				return "", &PluginOperationError{Msg: pulled}
			}
			output = pulled
		}
		var err error
		if revision, err = gitHeadRevision(ctx, staged, resolveGitExecutable()); err != nil {
			return "", err
		}
	}
	var merged []string
	if opts.CarryUserFiles != nil {
		var err error
		if merged, err = opts.CarryUserFiles(staged); err != nil {
			return "", err
		}
	}
	manifest, err := ReadManifestForInstall(staged)
	if err != nil {
		return "", err
	}
	installedName := filepath.Base(target)
	if jsonTruthy(manifest["name"]) {
		installedName = pyrepr.Str(manifest["name"])
	}
	if entry == nil && installedName != filepath.Base(target) {
		return "", &PluginOperationError{Msg: "The updated plugin changed its installed name; reinstall it explicitly."}
	}
	newTarget, err := sanitizePluginName(installedName, filepath.Dir(target), false)
	if err != nil {
		return "", &PluginOperationError{Msg: err.Error()}
	}
	if _, statErr := os.Lstat(newTarget); newTarget != target && statErr == nil {
		return "", &PluginOperationError{Msg: fmt.Sprintf("The updated plugin renamed itself to '%s', but that plugin already exists.", installedName)}
	}
	if err := checkManifestVersion(h, manifest, installedName); err != nil {
		return "", err
	}
	if _, err := scanMergedTree(h, staged, source, merged, scanOptions{Force: opts.Force, ScanDecision: opts.ScanDecision,
		ReviewedPin: entry != nil}); err != nil {
		return "", err
	}
	copyExampleFiles(staged, NewConsole(os.Stdout))
	if err := refreshDeclaredDependencies(ctx, target, staged, manifest, opts.Interactive); err != nil {
		return "", err
	}
	current, err := pm.TreeDigest(target)
	if err != nil {
		return "", err
	}
	if current != before {
		return "", &PluginOperationError{Msg: "Plugin files changed while preparing the update; retry."}
	}
	record["revision"] = revision
	stagedDigest, err := pm.TreeDigest(staged)
	if err != nil {
		return "", err
	}
	if newTarget == target && stagedDigest == before {
		return output, nil
	}
	newMetadata := map[string]any{}
	for k, v := range metadata {
		newMetadata[k] = v
	}
	newMetadata[installedName] = record
	publish := publishOptions{}
	if newTarget == target {
		publish.TargetDigest = &before
	}
	return output, publishPlugin(ctx, staged, newTarget, metadata, newMetadata, publish)
}
