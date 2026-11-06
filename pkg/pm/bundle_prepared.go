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
	"regexp"
	"slices"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Preparation record path beside a native output
func preparedPath(out string) string {
	return filepath.Join(filepath.Dir(out), filepath.Base(out)+".prepared.json")
}

// Payload entries a native preparation refuses to find symlinked
var preparedOutputNames = []string{"icebath-agent", "tools", "venv", "pm-runtime", "uv-cache", "bin",
	"enabled-features.json", "manifest.json"}

// Takes the exclusive non waiting preparation lock of a real, unlinked native output
func preparationLock(out string) (func(), error) {
	if out != resolvePath(out) {
		return nil, &PyError{Class: "ValueError", Message: "symlinked native output"}
	}
	for _, name := range preparedOutputNames {
		path := filepath.Join(out, name)
		info, err := os.Lstat(path)
		if (err == nil && info.Mode()&os.ModeSymlink != 0) || (pathExists(path) && IsJunction(path)) {
			return nil, &PyError{Class: "ValueError", Message: "symlinked native output: " + path}
		}
	}
	lock, err := os.OpenFile(filepath.Join(filepath.Dir(out), filepath.Base(out)+".prepare.lock"), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return nil, err
	}
	locked, err := tryLockFile(lock)
	if err != nil || !locked {
		lock.Close()
		if err != nil {
			return nil, err
		}
		return nil, &PyError{Class: "ValueError", Message: "native output is already in use"}
	}
	return func() { lock.Close() }, nil
}

// Tree digest of a directory or sha256 of a file's bytes
func preparedDigest(path string) (string, error) {
	if isDirectory(path) {
		return TreeDigest(path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

var projectDistSeparators = regexp.MustCompile(`[-_.]+`)

// Digest of admitted source that ignores only assembly owned products and bytecode caches
func sourceDigest(code string) (string, error) {
	project, err := loadProjectTable(filepath.Join(code, "pyproject.toml"))
	if err != nil {
		return "", err
	}
	name, ok := project.Get("name")
	if !ok {
		return "", &PyError{Class: "KeyError", Message: pyrepr.StrRepr("name")}
	}
	nameText, ok := name.(string)
	if !ok {
		return "", &PyError{Class: "TypeError", Message: fmt.Sprintf("expected string or bytes-like object, got %s", pyrepr.StrRepr(tomlTypeName(name)))}
	}
	version, ok := project.Get("version")
	if !ok {
		return "", &PyError{Class: "KeyError", Message: pyrepr.StrRepr("version")}
	}
	dist := projectDistSeparators.ReplaceAllString(nameText, "_")
	generated := []string{"install-stamp.json", "cmd/icebath/tui_dist", "cmd/icebath/web_dist",
		fmt.Sprintf("%s-%s.dist-info", dist, pyrepr.Str(version))}
	files := map[string]any{}
	var walkErr error
	pywalk(code, func(directory string, dirs, names []string) []string {
		var kept []string
		for _, d := range dirs {
			rel, _ := filepath.Rel(code, filepath.Join(directory, d))
			if d != "__pycache__" && !slices.Contains(generated, filepath.ToSlash(rel)) {
				kept = append(kept, d)
			}
		}
		for _, d := range kept {
			path := filepath.Join(directory, d)
			if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
				rel, _ := filepath.Rel(code, path)
				target, err := os.Readlink(path)
				if err != nil && walkErr == nil {
					walkErr = err
				}
				files[filepath.ToSlash(rel)] = target
			}
		}
		for _, n := range names {
			path := filepath.Join(directory, n)
			rel, _ := filepath.Rel(code, path)
			relative := filepath.ToSlash(rel)
			if slices.Contains(generated, relative) {
				continue
			}
			if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
				target, err := os.Readlink(path)
				if err != nil && walkErr == nil {
					walkErr = err
				}
				files[relative] = target
				continue
			}
			digest, err := preparedDigest(path)
			if err != nil && walkErr == nil {
				walkErr = err
			}
			files[relative] = digest
		}
		return kept
	})
	if walkErr != nil {
		return "", walkErr
	}
	encoded, err := pyjson.Dumps(files, pyjson.SortKeys(true))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(encoded))
	return hex.EncodeToString(sum[:]), nil
}

// Top down walk like os.walk without following links, visit returning the subdirectories to descend
func pywalk(top string, visit func(directory string, dirs, names []string) []string) {
	entries, err := os.ReadDir(top)
	if err != nil {
		return
	}
	var dirs, names []string
	for _, entry := range entries {
		isDir := entry.IsDir()
		if entry.Type()&fs.ModeSymlink != 0 {
			info, err := os.Stat(filepath.Join(top, entry.Name()))
			isDir = err == nil && info.IsDir()
		}
		if isDir {
			dirs = append(dirs, entry.Name())
		} else {
			names = append(names, entry.Name())
		}
	}
	for _, d := range visit(top, dirs, names) {
		path := filepath.Join(top, d)
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink == 0 {
			pywalk(path, visit)
		}
	}
}

// Refuses a prepared path that is relative, missing or resolves outside the output
func ownedPreparedPath(path, out string) error {
	if !filepath.IsAbs(path) || !pathExists(path) || !isRelativeTo(resolvePath(path), out) {
		return &PyError{Class: "ValueError", Message: "missing or escaped prepared path: " + path}
	}
	return nil
}

// Refuses any link or junction under path whose target leaves the output
func checkPreparedLinks(path, out string) error {
	var failure error
	pywalk(path, func(directory string, dirs, names []string) []string {
		for _, name := range append(slices.Clone(dirs), names...) {
			entry := filepath.Join(directory, name)
			info, err := os.Lstat(entry)
			if failure == nil && ((err == nil && info.Mode()&os.ModeSymlink != 0) || IsJunction(entry)) {
				failure = ownedPreparedPath(entry, out)
			}
		}
		if failure != nil {
			return nil
		}
		return dirs
	})
	return failure
}

// Prepared paths an assembly consumes, refusing a layout that is not the contained native one
func preparedInputPaths(inputs AgentInputs, out string) ([]string, error) {
	same := func(value, expected string) bool {
		return value != "" && pypath.PathStr(value) == pypath.PathStr(expected)
	}
	resourcesMatch := len(inputs.Resources) == len(resourceEnv)
	for _, name := range resourceEnvNames() {
		if path, ok := inputs.Resources[name]; !ok || pypath.PathStr(path) != pypath.PathStr(filepath.Join(inputs.Code, name)) {
			resourcesMatch = false
		}
	}
	if !same(inputs.Code, filepath.Join(out, "icebath-agent")) ||
		!same(inputs.Project, filepath.Join(out, "icebath-agent", "pyproject.toml")) ||
		!same(inputs.Tools, filepath.Join(out, "tools")) || !same(inputs.Environment, filepath.Join(out, "venv")) ||
		!same(inputs.PMRuntime, filepath.Join(out, "pm-runtime")) ||
		!same(inputs.Features, filepath.Join(out, "enabled-features.json")) ||
		inputs.Placement != "contained" || inputs.Repo != "icebath-agent" || len(inputs.Frontends) > 0 ||
		inputs.Stamp != "" || inputs.CommandDir != "" || (inputs.Env != nil && inputs.Env.Len() > 0) || !resourcesMatch ||
		!pathIsRelativeTo(inputs.SitePackages, inputs.Environment) {
		return nil, &PyError{Class: "ValueError", Message: "native prepared input layout changed"}
	}
	paths := []string{inputs.Code, inputs.Tools, inputs.Environment, inputs.PMRuntime, inputs.Python,
		inputs.Features, filepath.Join(out, "uv-cache")}
	checked := []string{inputs.Project, inputs.SitePackages}
	for _, name := range orderedKeys(inputs.Resources, resourceEnvNames()) {
		checked = append(checked, inputs.Resources[name])
	}
	for _, path := range append(checked, paths...) {
		if err := ownedPreparedPath(path, out); err != nil {
			return nil, err
		}
	}
	return paths, nil
}

// Output relative posix name of a prepared path
func preparedName(path, out string) string {
	rel, _ := filepath.Rel(out, path)
	return filepath.ToSlash(rel)
}

// Digest a prepared path records, the source digest for the code snapshot
func preparedPathDigest(path string, inputs AgentInputs) (string, error) {
	if pypath.PathStr(path) == pypath.PathStr(inputs.Code) {
		return sourceDigest(path)
	}
	return preparedDigest(path)
}

// Publishes the preparation record binding the validated inputs to their digests and the PM lock
func publishPrepared(ctx context.Context, out, source, revision string, inputs AgentInputs) (string, error) {
	if err := inputs.validate(out); err != nil {
		return "", err
	}
	paths, err := preparedInputPaths(inputs, out)
	if err != nil {
		return "", err
	}
	digests := pyjson.NewObject()
	for _, path := range paths {
		if err := ownedPreparedPath(path, out); err != nil {
			return "", err
		}
		if err := checkPreparedLinks(path, out); err != nil {
			return "", err
		}
		digest, err := preparedPathDigest(path, inputs)
		if err != nil {
			return "", err
		}
		digests.Set(preparedName(path, out), digest)
	}
	lock, err := preparedDigest(LockfilePath())
	if err != nil {
		return "", err
	}
	data := pyjson.NewObject()
	data.Set("schema", 1)
	data.Set("out", pypath.PathStr(out))
	data.Set("source", pypath.PathStr(source))
	data.Set("revision", revision)
	data.Set("lock", lock)
	data.Set("inputs", inputs.asDict())
	data.Set("digests", digests)
	encoded, err := pyjson.Dumps(data, pyjson.Indent(2))
	if err != nil {
		return "", err
	}
	prepared := preparedPath(out)
	if err := DurableWriteBytes(prepared, []byte(encoded+"\n")); err != nil {
		return "", err
	}
	return prepared, nil
}

// Message of an error the way str(exc) renders it for the preparation refusal
func preparedErrorText(err error) string {
	var py *PyError
	if errors.As(err, &py) {
		return py.Message
	}
	return pypath.OSErrorText(err)
}

var revisionPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

// Verified inputs of a preparation record whose identity, inventory and every digest still hold
func loadPrepared(ctx context.Context, prepared string) (AgentInputs, error) {
	inputs, err := loadPreparedRecord(ctx, prepared)
	if err != nil {
		var py *PyError
		if errors.As(err, &py) && !slices.Contains([]string{"ValueError", "KeyError", "TypeError", "FileNotFoundError", "JSONDecodeError", "UnicodeDecodeError"}, py.Class) {
			return AgentInputs{}, err
		}
		return AgentInputs{}, &PyError{Class: "ValueError", Message: "invalid native preparation; run preparation again: " + preparedErrorText(err)}
	}
	return inputs, nil
}

// Body of loadPrepared whose failures the caller rewraps as one ValueError
func loadPreparedRecord(ctx context.Context, prepared string) (AgentInputs, error) {
	stdout := SyncOutput(ctx)
	data, err := readJSONObject(prepared)
	if err != nil {
		return AgentInputs{}, err
	}
	field := func(key string) (any, error) {
		value, ok := data.Get(key)
		if !ok {
			return nil, &PyError{Class: "KeyError", Message: pyrepr.StrRepr(key)}
		}
		return value, nil
	}
	outValue, err := field("out")
	if err != nil {
		return AgentInputs{}, err
	}
	outText, ok := outValue.(string)
	if !ok {
		return AgentInputs{}, &PyError{Class: "TypeError", Message: fmt.Sprintf("argument should be a str or an os.PathLike object where __fspath__ returns a str, not %s", pyrepr.StrRepr(pyrepr.TypeName(outValue)))}
	}
	out := pypath.PathStr(outText)
	identity := &PyError{Class: "ValueError", Message: "native preparation identity changed"}
	if info, err := os.Lstat(prepared); (err == nil && info.Mode()&os.ModeSymlink != 0) || out != resolvePath(out) {
		return AgentInputs{}, identity
	}
	schema, err := field("schema")
	if err != nil {
		return AgentInputs{}, err
	}
	if n, ok := pyrepr.Int(schema); !ok || n.Int64() != 1 || !n.IsInt64() {
		if f, isFloat := schema.(float64); !isFloat || f != 1 {
			return AgentInputs{}, identity
		}
	}
	if pypath.PathStr(prepared) != preparedPath(out) {
		return AgentInputs{}, identity
	}
	lockValue, err := field("lock")
	if err != nil {
		return AgentInputs{}, err
	}
	lock, err := preparedDigest(LockfilePath())
	if err != nil {
		return AgentInputs{}, err
	}
	if lockValue != lock {
		return AgentInputs{}, identity
	}
	revisionValue, err := field("revision")
	if err != nil {
		return AgentInputs{}, err
	}
	revision, ok := revisionValue.(string)
	if !ok {
		return AgentInputs{}, &PyError{Class: "TypeError", Message: fmt.Sprintf("expected string or bytes-like object, got %s", pyrepr.StrRepr(pyrepr.TypeName(revisionValue)))}
	}
	if !revisionPattern.MatchString(revision) {
		return AgentInputs{}, identity
	}
	inputsValue, err := field("inputs")
	if err != nil {
		return AgentInputs{}, err
	}
	inputs, err := agentInputsFromDict(inputsValue)
	if err != nil {
		return AgentInputs{}, err
	}
	target, err := CurrentTarget()
	if err != nil {
		return AgentInputs{}, err
	}
	if inputs.Target != target || inputs.Ref == nil || *inputs.Ref == "" {
		return AgentInputs{}, &PyError{Class: "ValueError", Message: "native preparation target/ref changed"}
	}
	if err := inputs.validate(out); err != nil {
		return AgentInputs{}, err
	}
	paths, err := preparedInputPaths(inputs, out)
	if err != nil {
		return AgentInputs{}, err
	}
	digestsValue, err := field("digests")
	if err != nil {
		return AgentInputs{}, err
	}
	digests, ok := digestsValue.(*pyjson.Object)
	if !ok {
		return AgentInputs{}, &PyError{Class: "TypeError", Message: fmt.Sprintf("%s object is not iterable", pyrepr.StrRepr(pyrepr.TypeName(digestsValue)))}
	}
	required := map[string]bool{}
	for _, path := range paths {
		required[preparedName(path, out)] = true
	}
	inventory := len(required) == len(digests.Keys)
	for _, name := range digests.Keys {
		inventory = inventory && required[name]
	}
	if !inventory {
		return AgentInputs{}, &PyError{Class: "ValueError", Message: "native preparation inventory changed"}
	}
	for index, name := range digests.Keys {
		progress := fmt.Sprintf("payload verification [%d/%d]: %s", index+1, len(digests.Keys), name)
		fmt.Fprintf(stdout, "%s — checking paths and links\n", progress)
		path := filepath.Join(out, filepath.FromSlash(name))
		if err := ownedPreparedPath(path, out); err != nil {
			return AgentInputs{}, err
		}
		if err := checkPreparedLinks(path, out); err != nil {
			return AgentInputs{}, err
		}
		fmt.Fprintf(stdout, "%s — hashing files\n", progress)
		actual, err := preparedPathDigest(path, inputs)
		if err != nil {
			return AgentInputs{}, err
		}
		if digests.Vals[name] != actual {
			return AgentInputs{}, &PyError{Class: "ValueError", Message: "prepared bytes changed: " + name}
		}
	}
	fmt.Fprintln(stdout, "payload verification complete")
	return inputs, nil
}
