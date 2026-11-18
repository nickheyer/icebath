package pm

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Launcher a sealed payload records for the icebath command, empty when none is recorded
func payloadIcebathCommand(root string) (string, error) {
	manifest, payloadRoot, ok, err := payloadManifest(root)
	if err != nil || !ok {
		return "", err
	}
	rt, _ := manifest["runtime"].(map[string]any)
	commands, _ := rt["commands"].(map[string]any)
	command, _ := commands["icebath"].(string)
	if command == "" {
		return "", nil
	}
	return payloadPath(payloadRoot, command, "launcher")
}

// Name a person types to run this install's CLI, a channel build's qualified launcher name included
func CLICommandName(projectRoot string) (string, error) {
	command, err := payloadIcebathCommand(resolvePath(projectRoot))
	if err != nil {
		return "", err
	}
	if command == "" {
		return "icebath", nil
	}
	return strings.TrimSuffix(filepath.Base(command), ".exe"), nil
}

// Trees this process resolves Python imports against, the path its Python children inherit
var importPath struct {
	sync.Mutex
	entries []string
}

// Snapshot of this process's import path
func importPathEntries() []string {
	importPath.Lock()
	defer importPath.Unlock()
	return append([]string{}, importPath.entries...)
}

// ImportPathEntries exports importPathEntries
func ImportPathEntries() []string { return importPathEntries() }

// Puts the running tree first on the import path, dropping entries that resolve to the same tree
func EnsureProjectRootOnPath() {
	root := RepoRoot()
	normalized := pypath.Normcase(realpathOrSelf(root))
	importPath.Lock()
	defer importPath.Unlock()
	kept := []string{root}
	for _, entry := range importPath.entries {
		if entry == "" || pypath.Normcase(realpathOrSelf(entry)) != normalized {
			kept = append(kept, entry)
		}
	}
	importPath.entries = kept
}

// Resolved path, or the path as given when resolving raises, as a relative path does under a deleted cwd
func realpathOrSelf(path string) string {
	resolved, err := pypath.Realpath(path, pypath.Lax)
	if err != nil {
		return path
	}
	return resolved
}

// Value of a journal row key the way subscripting a decoded object reads it
func rowItem(row any, key string) (any, error) {
	m, ok := row.(map[string]any)
	if !ok {
		return nil, &PyError{Class: "TypeError", Message: fmt.Sprintf("'%s' object is not subscriptable", pyrepr.TypeName(row))}
	}
	v, present := m[key]
	if !present {
		return nil, &PyError{Class: "KeyError", Message: pyrepr.StrRepr(key)}
	}
	return v, nil
}

// Path text of a journal value, a TypeError for anything but a string
func rowPath(row any, key string) (string, error) {
	v, err := rowItem(row, key)
	if err != nil {
		return "", err
	}
	s, ok := v.(string)
	if !ok {
		return "", &PyError{Class: "TypeError", Message: fmt.Sprintf("argument should be a str or an os.PathLike object, not '%s'", pyrepr.TypeName(v))}
	}
	return s, nil
}

// Bytes of a strict base64 journal value, nil for JSON null
func rowBase64(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, &PyError{Class: "TypeError", Message: fmt.Sprintf("argument should be a bytes-like object or ASCII string, not '%s'", pyrepr.TypeName(v))}
	}
	out, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil {
		return nil, &PyError{Class: "ValueError", Message: err.Error()}
	}
	if out == nil {
		out = []byte{}
	}
	return out, nil
}

// Path with empty and dot components dropped the way pathlib spells it
func purePath(p string) string {
	if p == "" {
		return "."
	}
	volume := filepath.VolumeName(p)
	rest := p[len(volume):]
	rooted := rest != "" && os.IsPathSeparator(rest[0])
	var parts []string
	for _, part := range splitPathComponents(rest) {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	joined := strings.Join(parts, string(filepath.Separator))
	if rooted {
		joined = string(filepath.Separator) + joined
	}
	if volume+joined == "" {
		return "."
	}
	return volume + joined
}

// Hex digest of a file as a JSON value, nil when it does not exist
func digestValue(path string) (any, error) {
	digest, ok, err := FileDigest(path)
	if err != nil || !ok {
		return nil, err
	}
	return digest, nil
}

// Removes a tree, making read only entries writable and retrying each refused removal once
func rmtreeForce(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if st.Mode()&fs.ModeSymlink != 0 || IsJunction(path) {
		_ = os.Chmod(path, 0o200)
		return nil
	}
	return removeForce(path)
}

// Depth first removal whose failed steps chmod the entry writable and run again
func removeForce(dir string) error {
	retry := func(p string, op func() error) error {
		if err := op(); err != nil {
			_ = os.Chmod(p, 0o200)
			return op()
		}
		return nil
	}
	var entries []os.DirEntry
	if err := retry(dir, func() (err error) { entries, err = os.ReadDir(dir); return err }); err != nil {
		return err
	}
	for _, entry := range entries {
		child := filepath.Join(dir, entry.Name())
		if entry.IsDir() && !IsJunction(child) {
			if err := removeForce(child); err != nil {
				return err
			}
			continue
		}
		if err := retry(child, func() error { return os.Remove(child) }); err != nil {
			return err
		}
	}
	return retry(dir, func() error { return os.Remove(dir) })
}

// Rolls back or settles a shipped caller's plugin publication row
func RecoverPluginPublication(ctx context.Context, project string, row map[string]any, journal string) error {
	return recoverPluginPublication(ctx, project, row, journal)
}

// Rolls back or settles an interrupted plugin code publication from its journal row
func recoverPluginPublication(ctx context.Context, project string, row map[string]any, journal string) error {
	target, err := rowPath(row, "target")
	if err != nil {
		return err
	}
	backup, err := rowPath(row, "backup")
	if err != nil {
		return err
	}
	metadata, err := rowPath(row, "metadata")
	if err != nil {
		return err
	}
	target, backup, metadata = purePath(target), purePath(backup), purePath(metadata)
	homeRoot := resolvePath(DependencyHomeRoot(ctx))
	parent := filepath.Dir(target)
	if !isRelativeTo(resolvePath(target), homeRoot) || filepath.Base(parent) != "plugins" || filepath.Dir(backup) != parent ||
		!strings.HasPrefix(filepath.Base(backup), ".previous-") || metadata != filepath.Join(parent, ".install-metadata.json") {
		return &PyError{Class: "ValueError", Message: "plugin publication paths escape their home"}
	}
	committed := pyrepr.Truthy(row["committed"])
	if !committed {
		before, err := rowItem(row, "facts_before")
		if err != nil {
			return err
		}
		digest, err := digestValue(EnvironmentRuntimeFactsPath(ctx, project))
		if err != nil {
			return err
		}
		committed = !jsonEquals(digest, before)
	}
	if committed {
		if pathExists(backup) {
			if err := rmtreeForce(backup); err != nil {
				return err
			}
		}
	} else {
		rawBefore, err := rowItem(row, "metadata_before")
		if err != nil {
			return err
		}
		old, err := rowBase64(rawBefore)
		if err != nil {
			return err
		}
		current, _, err := ReadBytesOrNone(metadata)
		if err != nil {
			return err
		}
		rawAfter, err := rowItem(row, "metadata_after")
		if err != nil {
			return err
		}
		if rawAfter == nil {
			return &PyError{Class: "TypeError", Message: "argument should be a bytes-like object or ASCII string, not 'NoneType'"}
		}
		updated, err := rowBase64(rawAfter)
		if err != nil {
			return err
		}
		sameBytes := func(a, b []byte) bool { return (a == nil) == (b == nil) && string(a) == string(b) }
		if !sameBytes(current, old) && !sameBytes(current, updated) {
			return &PyError{Class: "ValueError", Message: "plugin metadata changed after publication; preserve it for manual recovery"}
		}
		if pathExists(backup) {
			if pathExists(target) {
				if err := rmtreeForce(target); err != nil {
					return err
				}
			}
			if err := renameFile(backup, target); err != nil {
				return err
			}
		} else {
			existed, err := rowItem(row, "target_existed")
			if err != nil {
				return err
			}
			if !pyrepr.Truthy(existed) && pathExists(target) {
				if err := rmtreeForce(target); err != nil {
					return err
				}
			}
		}
		if old == nil {
			if err := os.Remove(metadata); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		} else if err := DurableWriteBytes(metadata, old); err != nil {
			return err
		}
	}
	return os.Remove(journal)
}

// Text Python's str gives a recovery failure, OS errors spelled the way OSError prints them
func pyExceptionText(err error) string {
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	if errors.As(err, &pathErr) || errors.As(err, &linkErr) {
		return pypath.OSErrorText(err)
	}
	return err.Error()
}
