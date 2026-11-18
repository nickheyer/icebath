package pm

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unicode"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
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

// Environment a sealed payload ships beside its tree, empty when there is none
func PayloadVenv(projectRoot string) (string, error) {
	manifest, payloadRoot, ok, err := payloadManifest(resolvePath(projectRoot))
	if err != nil || !ok {
		return "", err
	}
	venv, _ := manifest["venv"].(string)
	if venv == "" {
		return "", nil
	}
	return payloadPath(payloadRoot, venv, "environment")
}

// Payload environment, then the checkout's in tree venv, then the venv path a first install creates
func BaseVenv(projectRoot string) (string, error) {
	payload, err := PayloadVenv(projectRoot)
	if err != nil || payload != "" {
		return payload, err
	}
	root := resolvePath(projectRoot)
	dir, err := projectVenvDir(root)
	if err != nil || dir != "" {
		return dir, err
	}
	return filepath.Join(root, "venv"), nil
}

// Committed environment of the install, or its original environment before the first sync
func SelectedVenv(ctx context.Context, projectRoot string) (string, error) {
	recorded, err := recordedVenv(ctx, projectRoot)
	if err != nil || recorded != "" {
		return recorded, err
	}
	return BaseVenv(projectRoot)
}

// Environment PM committed for the install or a sealed payload's own, never the pre PM in tree venv
func CommittedVenv(ctx context.Context, projectRoot string) (string, error) {
	recorded, err := recordedVenv(ctx, projectRoot)
	if err != nil || recorded != "" {
		return recorded, err
	}
	return PayloadVenv(projectRoot)
}

// Generation the install's facts select, refusing a damaged record or a tree outside the install
func recordedVenv(ctx context.Context, projectRoot string) (string, error) {
	path := EnvironmentRuntimeFactsPath(ctx, projectRoot)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	unreadable := &PyError{Class: "RuntimeError", Message: "cannot read dependency environment: " + path}
	if err != nil {
		return "", unreadable
	}
	data = trimBOM(data)
	if utf8Strict(data) != nil {
		return "", unreadable
	}
	doc, err := loadJSON(data)
	if err != nil {
		return "", unreadable
	}
	invalid := &PyError{Class: "RuntimeError", Message: "invalid dependency environment record: " + path}
	record, ok := doc.(map[string]any)
	if !ok {
		return "", invalid
	}
	var packages any = map[string]any{}
	if v, present := record["packages"]; present {
		packages = v
	}
	packageMap, ok := packages.(map[string]any)
	if !ok {
		return "", invalid
	}
	var fact any = map[string]any{}
	if v, present := packageMap["venv"]; present {
		fact = v
	}
	factMap, ok := fact.(map[string]any)
	if !ok {
		return "", invalid
	}
	value := factMap["environment"]
	if value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", &PyError{Class: "RuntimeError", Message: "invalid dependency environment path"}
	}
	environment, err := pathResolve(text)
	if err != nil {
		return "", err
	}
	generations := resolvePath(filepath.Join(InstallStateDir(ctx, projectRoot), "environments"))
	if !isRelativeTo(environment, generations) || !isRegularFile(filepath.Join(environment, "pyvenv.cfg")) {
		return "", &PyError{Class: "RuntimeError", Message: "dependency environment is missing or outside this install: " + environment}
	}
	return environment, nil
}

// Interpreter of the install's selected dependency environment
func ProjectPython(ctx context.Context, projectRoot string) (string, error) {
	venv, err := SelectedVenv(ctx, projectRoot)
	if err != nil {
		return "", err
	}
	return VenvPython(venv), nil
}

// Decimal text Python int gives a digit string, a ValueError for digit forms int refuses
func pyIntText(s string) (string, error) {
	for _, r := range s {
		if !isDecimalRune(r) {
			return "", &PyError{Class: "ValueError", Message: "invalid literal for int() with base 10: " + pyrepr.StrRepr(s)}
		}
	}
	return digitsValue(s).String(), nil
}

// Major and minor version a POSIX venv holds from pyvenv.cfg or its lib directory, ok false when neither says
func VenvPythonVersion(venv string) (major, minor string, ok bool, err error) {
	parse := func(text string) (string, string, bool, error) {
		head, rest, _ := strings.Cut(text, ".")
		tail, _, _ := strings.Cut(rest, ".")
		if !pystr.All(head, pystr.IsDigit) || !pystr.All(tail, pystr.IsDigit) {
			return "", "", false, nil
		}
		a, err := pyIntText(head)
		if err != nil {
			return "", "", false, err
		}
		b, err := pyIntText(tail)
		if err != nil {
			return "", "", false, err
		}
		return a, b, true, nil
	}
	if data, readErr := os.ReadFile(filepath.Join(venv, "pyvenv.cfg")); readErr == nil {
		data = trimBOM(data)
		if err := utf8Strict(data); err != nil {
			return "", "", false, err
		}
		for _, line := range pystr.Splitlines(string(data), false) {
			key, value, _ := strings.Cut(line, "=")
			if k := pystr.Strip(key); k != "version" && k != "version_info" {
				continue
			}
			if a, b, found, err := parse(pystr.Strip(value)); err != nil || found {
				return a, b, found, err
			}
		}
	}
	entries, readErr := os.ReadDir(filepath.Join(venv, "lib"))
	if readErr != nil {
		return "", "", false, nil
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "python3") || (runtime.GOOS == "windows" && strings.HasPrefix(strings.ToLower(name), "python3")) {
			names = append(names, name)
		}
	}
	sortPathNames(names)
	for _, name := range names {
		if a, b, found, err := parse(strings.TrimPrefix(name, "python")); err != nil || found {
			return a, b, found, err
		}
	}
	return "", "", false, nil
}

// Reports a Unicode decimal digit, the only digit form Python int accepts
func isDecimalRune(r rune) bool { return unicode.Is(unicode.Nd, r) }

// Major and minor version of the pinned Python PM builds environments with, this process's interpreter
func pinnedPythonVersion() (string, string, error) {
	version, ok := currentLockfile().Version("python")
	major, rest, _ := strings.Cut(version, ".")
	minor, _, _ := strings.Cut(rest, ".")
	if !ok || major == "" || minor == "" {
		return "", "", &PyError{Class: "RuntimeError", Message: fmt.Sprintf("the lock pins no python version: %s", pyrepr.StrRepr(version))}
	}
	return major, minor, nil
}

// Major and minor version of the pinned Python for callers outside pm, a RuntimeError when the lock pins none
func PinnedPythonVersion() (major, minor string, err error) { return pinnedPythonVersion() }

// Site packages directory of a venv for the interpreter version the tree actually holds
func SitePackages(venv string) (string, error) {
	if runtime.GOOS == "windows" {
		return filepath.Join(venv, "Lib", "site-packages"), nil
	}
	major, minor, ok, err := VenvPythonVersion(venv)
	if err != nil {
		return "", err
	}
	if !ok {
		if major, minor, err = pinnedPythonVersion(); err != nil {
			return "", err
		}
	}
	return filepath.Join(venv, "lib", "python"+major+"."+minor, "site-packages"), nil
}

// Directory of the resolved running executable, what this process runs from
var processPrefix = func() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Dir(resolvePath(exe)), nil
}

// Exported form for pkg/memory setup, same answer as the package function
func RunningFromSelectedEnvironment(ctx context.Context, projectRoot string) (bool, string, error) {
	return runningFromSelectedEnvironment(ctx, projectRoot)
}

// Reports whether this process resolves imports from the install's selected environment, with what it runs from
func runningFromSelectedEnvironment(ctx context.Context, projectRoot string) (bool, string, error) {
	prefix, err := processPrefix()
	if err != nil {
		return false, "", err
	}
	selected, err := SelectedVenv(ctx, projectRoot)
	var site, resolved string
	if err == nil {
		site, err = SitePackages(selected)
	}
	if err == nil {
		resolved, err = pathResolve(site)
	}
	if err != nil {
		if pyOSRuntimeOrValueError(err) {
			return false, prefix, nil
		}
		return false, prefix, err
	}
	for _, entry := range importPathEntries() {
		if entry != "" && resolvePath(entry) == resolved {
			return true, prefix, nil
		}
	}
	return false, prefix, nil
}

// Reports a failure an except clause naming OSError, RuntimeError and ValueError catches, subclasses included
func pyOSRuntimeOrValueError(err error) bool {
	if pypath.IsOSError(err) {
		return true
	}
	switch errorClass(err) {
	case "OSError", "FileNotFoundError", "FileExistsError", "PermissionError", "RuntimeError", "InstallError", "ResolutionConflict",
		"DownloadError", "DownloadPaused", "HashError", "_RangeError", "DownloadTransportError", "ValueError", "UnicodeDecodeError":
		return true
	}
	return false
}
