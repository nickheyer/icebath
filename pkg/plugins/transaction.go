package plugins

import (
	"bytes"
	"context"
	"encoding/json"
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

// Whether a plugin tree is active in the home, its manifest named in plugins.enabled and not disabled or the memory provider
func activePluginTree(h, target string) (bool, error) {
	manifest, err := ReadManifestForInstall(target)
	if err != nil {
		return false, err
	}
	names := map[string]bool{filepath.Base(target): true}
	if name := scalarString(manifest["name"]); name != "" {
		names[name] = true
	}
	cfg := loadConfigOrEmpty(h)
	if intersects(names, disabledPlugins(cfg)) {
		return false, nil
	}
	if enabled, present := enabledPlugins(cfg); present && intersects(names, enabled) {
		return true, nil
	}
	return names[getCurrentMemoryProvider(h)], nil
}

// How a staged tree is published and the target digest it was prepared against
type publishOptions struct {
	TargetDigest *string
}

// Validates the staged tree's form, swaps it over its target and records the new install metadata under the metadata lock
func publishPlugin(ctx context.Context, h, staged, target string, oldMetadata, newMetadata map[string]any, opts publishOptions) error {
	manifest, err := ReadManifestForInstall(staged)
	if err != nil {
		return err
	}
	form := &ValidationReport{}
	checkLoadable(form, staged, manifest)
	if !form.OK() {
		return &PluginOperationError{Msg: fmt.Sprintf("%s was not installed: %s.", manifestText(manifest, "name", filepath.Base(target)), UnknownPluginFormText),
			FailureClass: "unsupported_form"}
	}
	return withInstallMetadataLock(ctx, h, func(ctx context.Context) error {
		current, err := readInstallMetadata(h)
		if err != nil {
			return err
		}
		if !jsonEqual(current, oldMetadata) {
			return &PluginOperationError{Msg: "Plugin install metadata changed while preparing the install; retry."}
		}
		_, statErr := os.Lstat(target)
		exists := statErr == nil
		if exists && opts.TargetDigest != nil {
			digest, err := pm.TreeDigest(target)
			if err != nil {
				return err
			}
			if digest != *opts.TargetDigest {
				return &PluginOperationError{Msg: "Plugin files changed while preparing the update; retry."}
			}
		}
		backup := ""
		if exists {
			holder, err := os.MkdirTemp(filepath.Dir(target), ".replaced-")
			if err != nil {
				return err
			}
			backup = filepath.Join(holder, "plugin")
			if err := os.Rename(target, backup); err != nil {
				os.Remove(holder)
				return err
			}
		}
		restore := func() {
			if backup != "" {
				os.RemoveAll(target)
				os.Rename(backup, target)
				os.Remove(filepath.Dir(backup))
			}
		}
		if err := os.Rename(staged, target); err != nil {
			restore()
			return err
		}
		if err := writeInstallMetadata(ctx, h, newMetadata); err != nil {
			os.Rename(target, staged)
			restore()
			return err
		}
		if backup != "" {
			os.RemoveAll(filepath.Dir(backup))
		}
		return nil
	})
}

// Whether two JSON documents hold the same values
func jsonEqual(a, b map[string]any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ja, jb)
}

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
				if err := checkManifestVersion(ctx, h, map[string]any{"requires_icebath": floor}, name); err != nil {
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
	if err := checkManifestVersion(ctx, h, manifest, installedName); err != nil {
		return "", err
	}
	if _, err := scanMergedTree(h, staged, source, merged, scanOptions{Force: opts.Force, ScanDecision: opts.ScanDecision,
		ReviewedPin: entry != nil}); err != nil {
		return "", err
	}
	copyExampleFiles(staged, NewConsole(os.Stdout))
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
	return output, publishPlugin(ctx, h, staged, newTarget, metadata, newMetadata, publish)
}
