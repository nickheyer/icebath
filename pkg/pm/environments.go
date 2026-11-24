//go:build !icebath_core && !icebath_noextras

package pm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Records which checkout owns an install state, the root the orphan collector and the update owner lookup read
func recordProjectRoot(ctx context.Context, projectRoot string) error {
	stamps := filepath.Join(InstallStateDir(ctx, projectRoot), "inputs")
	removeTreeQuiet(stamps)
	if err := os.MkdirAll(stamps, 0o777); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stamps, ".project-root"), []byte(resolvePath(projectRoot)), 0o666)
}

// Directory holding a sealed payload's own launchers, empty for every other tree
func PayloadCommandDir(projectRoot string) (string, error) {
	command, err := payloadIcebathCommand(resolvePath(projectRoot))
	if err != nil || command == "" {
		return "", err
	}
	return filepath.Dir(command), nil
}

// Flushes every finished file to disk before a selection record may name the tree
func flushBeforeSelecting() error {
	if filesystemSync == nil {
		return nil
	}
	return filesystemSync()
}

// Sealed payload interpreter its venvs start on, empty for Termux and other trees
func payloadStorePython(root string) (string, error) {
	manifest, payloadRoot, ok, err := payloadManifest(root)
	if err != nil || !ok {
		return "", err
	}
	target := ""
	if v, present := manifest["target"]; present {
		target = pyrepr.Str(v)
	}
	if strings.HasSuffix(target, "-bionic") {
		return "", nil
	}
	rt, _ := manifest["runtime"].(map[string]any)
	relative, _ := rt["storePython"].(string)
	if relative == "" {
		return "", nil
	}
	return payloadPath(payloadRoot, relative, "interpreter")
}

// Argv prefix running Python inside a venv, a sealed payload's venv first healed onto the payload's store interpreter
func managedVenvCommand(ctx context.Context, projectRoot, venv string, options []string) ([]string, error) {
	storePython, err := payloadStorePython(resolvePath(projectRoot))
	if err != nil {
		return nil, err
	}
	if storePython != "" {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := healVenvInterpreter(venv, storePython); err != nil {
			return nil, err
		}
	}
	return append([]string{VenvPython(venv)}, options...), nil
}

// NoCommittedEnvironment refuses PM's store binary with nothing committed, only a repair fixes it
type NoCommittedEnvironment struct{ Message string }

func (e *NoCommittedEnvironment) Error() string { return e.Message }

// RuntimeError class callers catch it as
func (e *NoCommittedEnvironment) Unwrap() error {
	return &PyError{Class: "RuntimeError", Message: e.Message}
}

// Refuses a boot from inside the tool store with no committed environment, other binaries keep their own
func requireOwnDependencies(ctx context.Context, projectRoot string) error {
	prefix, err := processPrefix()
	if err != nil {
		return err
	}
	store, err := EnvironmentStoreRoot(ctx, projectRoot, true)
	if err != nil {
		return err
	}
	if isRelativeTo(resolvePath(prefix), resolvePath(store)) {
		return &NoCommittedEnvironment{Message: "no dependency environment is committed for this install"}
	}
	return nil
}

// Selects the committed generation at boot, leasing it and pointing child imports and PATH at it
func managedActivateDependencies(ctx context.Context, projectRoot string) error {
	var environment, selected string
	if isDirectory(InstallStateDir(ctx, projectRoot)) {
		stop := false
		err := func() error {
			held, release, err := RuntimeLock(ctx, projectRoot, InstallLockTimeout)
			if err != nil {
				return err
			}
			defer release()
			if held {
				if err := recoverPublication(ctx, projectRoot); err != nil {
					return err
				}
			}
			if environment, err = CommittedVenv(ctx, projectRoot); err != nil {
				return err
			}
			if environment == "" {
				stop = true
				return requireOwnDependencies(ctx, projectRoot)
			}
			releaseLease, err := LeaseGeneration(environment)
			if err != nil {
				return err
			}
			for !held {
				current, err := CommittedVenv(ctx, projectRoot)
				if err != nil {
					return err
				}
				if current == "" || current == environment {
					break
				}
				releaseLease()
				environment = current
				if releaseLease, err = LeaseGeneration(current); err != nil {
					return err
				}
			}
			if selected, err = SitePackages(environment); err != nil {
				return err
			}
			stop = !isDirectory(selected) && !isRegularFile(EnvironmentRuntimeFactsPath(ctx, projectRoot))
			return nil
		}()
		if err != nil || stop {
			return err
		}
	} else {
		var err error
		if environment, err = PayloadVenv(projectRoot); err != nil {
			return err
		}
		if environment == "" {
			return requireOwnDependencies(ctx, projectRoot)
		}
		if selected, err = SitePackages(environment); err != nil {
			return err
		}
		if !isDirectory(selected) {
			return nil
		}
	}
	if !isDirectory(selected) {
		return &PyError{Class: "RuntimeError", Message: "dependency environment has no site-packages: " + selected}
	}
	root := resolvePath(projectRoot)
	importPath.Lock()
	var kept []string
	for _, entry := range importPath.entries {
		if name := pypath.PathName(entry); name == "site-packages" || name == "dist-packages" || resolvePath(entry) == root {
			continue
		}
		kept = append(kept, entry)
	}
	kept, err := addSiteDir(kept, selected, nil)
	if err != nil {
		importPath.entries = kept
		importPath.Unlock()
		return err
	}
	entries := []string{root, selected}
	for _, entry := range kept {
		if resolvePath(entry) != resolvePath(selected) {
			entries = append(entries, entry)
		}
	}
	importPath.entries = entries
	importPath.Unlock()
	sep := string(os.PathListSeparator)
	os.Setenv("PYTHONPATH", root+sep+selected)
	os.Unsetenv("VIRTUAL_ENV")
	commandDir, err := PayloadCommandDir(projectRoot)
	if err != nil {
		return err
	}
	if commandDir == "" {
		commandDir = filepath.Join(root, ".icebath", "bin")
	}
	directories := []string{commandDir}
	_, _, sealed, err := payloadManifest(root)
	if err != nil {
		return err
	}
	if !(runtime.GOOS == "windows" && sealed) {
		directories = append(directories, VenvBinDir(environment, runtime.GOOS == "windows"))
	}
	var prefix []string
	for _, directory := range directories {
		if isDirectory(directory) {
			prefix = append(prefix, directory)
		}
	}
	if len(prefix) > 0 {
		os.Setenv("PATH", strings.Join(append(prefix, os.Getenv("PATH")), sep))
	}
	return nil
}

// Ordered environment whose names match exactly the way a Python dict holds them
type environList struct {
	keys   []string
	values map[string]string
}

// Index of a name in the list, minus one when absent
func (e *environList) find(name string) int {
	for i, key := range e.keys {
		if key == name {
			return i
		}
	}
	return -1
}

// Sets a name in place or appends it
func (e *environList) set(name, value string) {
	if e.find(name) < 0 {
		e.keys = append(e.keys, name)
	}
	e.values[name] = value
}

// Removes a name when present
func (e *environList) pop(name string) {
	if i := e.find(name); i >= 0 {
		delete(e.values, e.keys[i])
		e.keys = append(e.keys[:i], e.keys[i+1:]...)
	}
}

// Entries as NAME=value strings in order
func (e *environList) entries() []string {
	out := make([]string, len(e.keys))
	for i, key := range e.keys {
		out[i] = key + "=" + e.values[key]
	}
	return out
}

// Process environment in order as Python os environ holds it, first occurrence kept and names uppercased on Windows
func pyEnvironOrdered() *environList {
	list := &environList{values: map[string]string{}}
	for _, kv := range os.Environ() {
		if runtime.GOOS == "windows" && strings.HasPrefix(kv, "=") {
			continue
		}
		key, value, found := strings.Cut(kv, "=")
		if !found {
			continue
		}
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(key)
		}
		if list.find(key) < 0 {
			list.set(key, value)
		}
	}
	return list
}

// Recorded environment diffs of the installed packages of names, dependencies first in the order env_for composes them
func installedEnvDiffs(ctx context.Context, names []string) ([]EnvDiff, error) {
	lockfile := currentLockfile()
	target, err := CurrentTarget()
	if err != nil {
		return nil, err
	}
	var diffs []EnvDiff
	for _, name := range names {
		chain, err := Walk([]string{name})
		var unknown *UnknownPackageError
		if errors.As(err, &unknown) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, p := range chain {
			if p.Base().Internal {
				continue
			}
			loc, err := InstalledLocation(ctx, p, lockfile, target, LocationOptions{})
			if err != nil {
				return nil, err
			}
			if loc != nil {
				diffs = append(diffs, loc.Facts.EnvFor(p.Base().Name, loc.Store.Root))
			}
		}
	}
	return diffs, nil
}

// Python compose_env over the process environment, each diff read in the sorted key order facts json stores
func composeEnviron(diffs []EnvDiff) *environList {
	env := pyEnvironOrdered()
	var pathDirs []string
	for i := len(diffs) - 1; i >= 0; i-- {
		if value, ok := diffs[i]["PATH"]; ok {
			for _, d := range envDirs(value) {
				if n := Native(d); !slices.Contains(pathDirs, n) {
					pathDirs = append(pathDirs, n)
				}
			}
		}
	}
	for _, diff := range diffs {
		for _, key := range sortedKeys(diff) {
			if key != "PATH" {
				env.set(key, Native(pyrepr.Str(diff[key])))
			}
		}
	}
	if len(pathDirs) > 0 {
		key := "PATH"
		for _, k := range env.keys {
			if strings.ToUpper(k) == "PATH" {
				key = k
				break
			}
		}
		value := strings.Join(pathDirs, string(os.PathListSeparator))
		if existing := env.values[key]; existing != "" {
			value += string(os.PathListSeparator) + existing
		}
		env.set(key, value)
	}
	return env
}

// Environment of the installed PM packages with the committed site packages
func managedActivationEnvironment(ctx context.Context, projectRoot string) ([]string, error) {
	diffs, err := installedEnvDiffs(ctx, AllPackages())
	if err != nil {
		return nil, err
	}
	env := composeEnviron(diffs)
	environment, err := CommittedVenv(ctx, projectRoot)
	if err != nil {
		return nil, err
	}
	env.pop("PYTHONHOME")
	env.pop("VIRTUAL_ENV")
	pythonPath, err := pathResolve(projectRoot)
	if err != nil {
		return nil, err
	}
	if environment != "" {
		site, err := SitePackages(environment)
		if err != nil {
			return nil, err
		}
		pythonPath += string(os.PathListSeparator) + site
	}
	env.set("PYTHONPATH", pythonPath)
	return env.entries(), nil
}

// Configs one journal row restores, each with its previous bytes and the digest written after publication
type journalConfig struct {
	path     string
	previous []byte
	after    any
}

// Rolls back or settles the publication journal row of one checkout
func recoverJournal(ctx context.Context, project, journal string, data []byte) error {
	doc, err := loadJSON(data)
	if err != nil {
		return &PyError{Class: "ValueError", Message: err.Error()}
	}
	row, ok := doc.(map[string]any)
	if !ok {
		return &PyError{Class: "AttributeError", Message: fmt.Sprintf("the publication journal %s must hold a mapping, got %s", journal, errchain.Kind(doc))}
	}
	if kind, _ := row["kind"].(string); kind == "plugin" {
		return recoverPluginPublication(ctx, project, row, journal)
	}
	entries := []any{row}
	if raw, present := row["configs"]; present {
		if entries, err = pyIterate(raw); err != nil {
			return err
		}
	}
	homeRoot := resolvePath(DependencyHomeRoot(ctx))
	var configs []journalConfig
	for _, entry := range entries {
		config, err := rowPath(entry, "config")
		if err != nil {
			return err
		}
		config = purePath(config)
		if filepath.Base(config) != "config.yaml" || !isRelativeTo(resolvePath(config), homeRoot) {
			return &PyError{Class: "ValueError", Message: "config path is outside icebath state"}
		}
		raw, err := rowItem(entry, "previous")
		if err != nil {
			return err
		}
		previous, err := rowBase64(raw)
		if err != nil {
			return err
		}
		configs = append(configs, journalConfig{path: config, previous: previous, after: entry.(map[string]any)["config_after"]})
	}
	if !pyrepr.Truthy(row["committed"]) {
		before, err := rowItem(row, "facts_before")
		if err != nil {
			return err
		}
		digest, err := digestValue(EnvironmentRuntimeFactsPath(ctx, project))
		if err != nil {
			return err
		}
		if jsonEquals(digest, before) {
			for _, c := range configs {
				var prior any
				if c.previous != nil {
					sum := sha256.Sum256(c.previous)
					prior = hex.EncodeToString(sum[:])
				}
				current, err := digestValue(c.path)
				if err != nil {
					return err
				}
				if !jsonEquals(current, prior) && !jsonEquals(current, c.after) {
					return &PyError{Class: "ValueError", Message: "config changed after publication began; preserve it for manual recovery"}
				}
			}
			for _, c := range configs {
				if c.previous == nil {
					if err := os.Remove(c.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
						return err
					}
				} else if err := DurableWriteBytes(c.path, c.previous); err != nil {
					return err
				}
			}
		}
	}
	return os.Remove(journal)
}

// Recovers an interrupted publication under the held dependency lock, before activation or another write
func recoverPublication(ctx context.Context, project string) error {
	journal := filepath.Join(InstallStateDir(ctx, project), "publication.json")
	data, ok, err := ReadBytesOrNone(journal)
	if err != nil || !ok {
		return err
	}
	if err := recoverJournal(ctx, project, journal, data); err != nil {
		if class := errorClass(err); class == "AttributeError" || class == "RuntimeError" {
			return err
		}
		return &PyError{Class: "RuntimeError", Message: fmt.Sprintf("cannot recover dependency publication: %s: %s", journal, pyExceptionText(err))}
	}
	return nil
}

// Marks the journal committed so recovery keeps a publication that changed code or config without a generation
func finishPublication(ctx context.Context, project string) error {
	journal := filepath.Join(InstallStateDir(ctx, project), "publication.json")
	data, err := os.ReadFile(journal)
	if err != nil {
		return err
	}
	doc, err := loadJSON(data)
	if err != nil {
		return &PyError{Class: "ValueError", Message: err.Error()}
	}
	row, ok := doc.(map[string]any)
	if !ok {
		return &PyError{Class: "TypeError", Message: fmt.Sprintf("the publication journal %s must hold a mapping, got %s", journal, errchain.Kind(doc))}
	}
	row["committed"] = true
	encoded, err := dumpsJSON(row, false)
	if err != nil {
		return err
	}
	if err := publicationWrite(journal, encoded); err != nil {
		return err
	}
	return recoverPublication(ctx, project)
}

// Removes unselected lease managed generations older than minAge once no reader holds them
func collectGenerations(ctx context.Context, project string, minAge time.Duration) ([]string, error) {
	var removed []string
	root := InstallStateDir(ctx, project)
	if _, err := os.Lstat(root); err != nil {
		return removed, nil
	}
	held, release, err := RuntimeLock(ctx, project, InstallLockTimeout)
	if err != nil {
		return nil, err
	}
	defer release()
	if !held {
		return removed, nil
	}
	if err := recoverPublication(ctx, project); err != nil {
		return nil, err
	}
	venv, err := SelectedVenv(ctx, project)
	if err != nil {
		return nil, err
	}
	selected := resolvePath(filepath.Dir(venv))
	generations := filepath.Join(root, "environments")
	if !isDirectory(generations) {
		return removed, nil
	}
	entries, err := os.ReadDir(generations)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		generation := filepath.Join(generations, entry.Name())
		if st, err := os.Lstat(generation); err != nil || st.Mode()&fs.ModeSymlink != 0 || !isDirectory(generation) || resolvePath(generation) == selected {
			continue
		}
		marker, err := os.Stat(filepath.Join(generation, ".lease-managed"))
		if err != nil || !marker.Mode().IsRegular() || time.Since(marker.ModTime()) < minAge {
			continue
		}
		leased, err := LeasesHeld(generation)
		if err != nil {
			return nil, err
		}
		if !leased {
			if err := RemoveTree(generation); err != nil {
				return nil, err
			}
			removed = append(removed, generation)
		}
	}
	return removed, nil
}

// Reports an error Python raises as OSError or ValueError
func isOSOrValueError(err error) bool {
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	var syscallErr *os.SyscallError
	if errors.As(err, &pathErr) || errors.As(err, &linkErr) || errors.As(err, &syscallErr) {
		return true
	}
	switch errorClass(err) {
	case "OSError", "FileNotFoundError", "FileExistsError", "PermissionError", "ValueError", "UnicodeDecodeError", "TOMLDecodeError":
		return true
	}
	return false
}

// Collects the dependency generations a publish superseded, never failing the committed update
func CollectSupersededGenerations(ctx context.Context, projectRoot string) error {
	removed, err := collectGenerations(ctx, projectRoot, 24*time.Hour)
	if err != nil {
		if isOSOrValueError(err) {
			pkgLog().Warn(fmt.Sprintf("dependency generation cleanup skipped: %s", pyExceptionText(err)))
			return nil
		}
		return err
	}
	if len(removed) > 0 {
		pkgLog().Info(fmt.Sprintf("collected %d unused dependency generations", len(removed)))
	}
	return nil
}

// Removes unselected lease managed generations older than minAge whose readers have exited
func managedCollectGenerations(ctx context.Context, project string, minAge time.Duration) ([]string, error) {
	return collectGenerations(ctx, project, minAge)
}
