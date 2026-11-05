package pm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

var memberExclude = map[string]bool{".git": true, ".venv": true, "venv": true, "node_modules": true, "__pycache__": true}

// Names a member copy and stamp leave out, VCS, environments, caches and egg-info
func memberIgnored(_ string, names []string) map[string]bool {
	out := map[string]bool{}
	for _, name := range names {
		if memberExclude[name] || strings.HasSuffix(name, ".egg-info") {
			out[name] = true
		}
	}
	return out
}

// Installed identities mapped to build inputs, both resolved, a repeated identity taking its last source
func MemberSources(pluginDirs []MemberDir) []MemberDir {
	var out []MemberDir
	at := map[string]int{}
	for _, d := range pluginDirs {
		identity, source := resolvePath(absoluteUnresolved(d.Identity)), resolvePath(absoluteUnresolved(d.Source))
		if i, ok := at[identity]; ok {
			out[i].Source = source
			continue
		}
		at[identity] = len(out)
		out = append(out, MemberDir{Identity: identity, Source: source})
	}
	return out
}

// Orders paths the way pathlib compares them, component by component with case folded on Windows
func pathLess(a, b string) bool {
	if runtime.GOOS == "windows" {
		a, b = strings.ToLower(strings.ReplaceAll(a, "/", `\`)), strings.ToLower(strings.ReplaceAll(b, "/", `\`))
	}
	pa, pb := strings.Split(a, string(filepath.Separator)), strings.Split(b, string(filepath.Separator))
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return len(pa) < len(pb)
}

// Hash of the member inputs a generation copies, keyed by identity so staging paths never change it
func MembersStamp(pluginDirs []MemberDir) (string, error) {
	h := sha256.New()
	sources := MemberSources(pluginDirs)
	sort.SliceStable(sources, func(i, j int) bool { return pathLess(sources[i].Identity, sources[j].Identity) })
	for _, member := range sources {
		h.Write([]byte(member.Identity))
		h.Write([]byte{0})
		declaration, err := ReadPythonDeclaration(member.Source)
		if err != nil {
			return "", err
		}
		for _, source := range declaration.Files {
			data, err := os.ReadFile(source)
			if err != nil {
				return "", err
			}
			h.Write([]byte(filepath.Base(source)))
			h.Write(data)
			h.Write([]byte{0})
		}
		if !isRegularFile(filepath.Join(member.Source, "pyproject.toml")) {
			continue
		}
		if err := walkMemberTree(member.Source, member.Source, func(rel, path string, link bool) error {
			h.Write([]byte(rel))
			h.Write([]byte{0})
			if link {
				target, err := os.Readlink(path)
				if err != nil {
					return err
				}
				h.Write([]byte(target))
			} else {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				h.Write(data)
			}
			h.Write([]byte{0})
			return nil
		}); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Visits files under a tree as os.walk lists them, sorted, ignored names skipped and directory links not entered
func walkMemberTree(root, directory string, visit func(rel, path string, link bool) error) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil
	}
	var dirs, files []string
	for _, e := range entries {
		path := filepath.Join(directory, e.Name())
		if st, err := os.Stat(path); err == nil && st.IsDir() {
			dirs = append(dirs, e.Name())
		} else {
			files = append(files, e.Name())
		}
	}
	ignoredDirs, ignoredFiles := memberIgnored(directory, dirs), memberIgnored(directory, files)
	sort.Strings(files)
	for _, name := range files {
		if ignoredFiles[name] {
			continue
		}
		path := filepath.Join(directory, name)
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		st, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if err := visit(filepath.ToSlash(rel), path, st.Mode()&fs.ModeSymlink != 0); err != nil {
			return err
		}
	}
	sort.Strings(dirs)
	for _, name := range dirs {
		path := filepath.Join(directory, name)
		if ignoredDirs[name] {
			continue
		}
		if st, err := os.Lstat(path); err == nil && st.Mode()&fs.ModeSymlink != 0 {
			continue
		}
		if err := walkMemberTree(root, path, visit); err != nil {
			return err
		}
	}
	return nil
}

// Tool store entries published in a directory when its facts.json records them, plus fetch caches
func publishedStoreEntries(directory string) map[string]bool {
	out := map[string]bool{}
	text, err := readUTF8SigText(filepath.Join(directory, "facts.json"))
	if err != nil {
		return out
	}
	facts, err := loadJSON([]byte(text))
	if err != nil {
		return out
	}
	document, ok := facts.(map[string]any)
	if !ok {
		return out
	}
	packages, ok := document["packages"].(map[string]any)
	if !ok {
		return out
	}
	names := map[string]bool{}
	for _, fact := range packages {
		if row, ok := fact.(map[string]any); ok {
			if entry, ok := row["entry"].(string); ok && entry != "" && pyPathName(entry) == entry {
				names[entry] = true
			}
		}
	}
	if entries, err := os.ReadDir(directory); err == nil {
		for _, e := range entries {
			if fnmatchCase(e.Name(), "fetch-*") && isDirectory(filepath.Join(directory, e.Name())) {
				names[e.Name()] = true
			}
		}
	}
	for name := range names {
		if isDirectory(filepath.Join(directory, name)) {
			out[name] = true
		}
	}
	return out
}

// Copies a tree like shutil.copytree with copy2, links copied as links or followed, failures gathered
func copyTreeLike(src, dst string, symlinks bool, ignore func(dir string, names []string) map[string]bool) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	ignored := map[string]bool{}
	if ignore != nil {
		ignored = ignore(src, names)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
		return err
	}
	if err := os.Mkdir(dst, 0o777); err != nil {
		return err
	}
	var failures []string
	for _, name := range names {
		if ignored[name] {
			continue
		}
		from, to := filepath.Join(src, name), filepath.Join(dst, name)
		st, err := os.Lstat(from)
		if err != nil {
			failures = append(failures, fmt.Sprintf("(%s, %s, %s)", pyRepr(from), pyRepr(to), pyRepr(pyOSErrorText(err))))
			continue
		}
		if st.Mode()&fs.ModeSymlink != 0 && symlinks {
			target, err := os.Readlink(from)
			if err == nil {
				err = os.Symlink(target, to)
			}
			if err != nil {
				failures = append(failures, fmt.Sprintf("(%s, %s, %s)", pyRepr(from), pyRepr(to), pyRepr(pyOSErrorText(err))))
			}
			continue
		}
		if isDirectory(from) {
			err = copyTreeLike(from, to, symlinks, ignore)
		} else {
			err = copy2(from, to)
		}
		if err != nil {
			failures = append(failures, fmt.Sprintf("(%s, %s, %s)", pyRepr(from), pyRepr(to), pyRepr(err.Error())))
		}
	}
	if err := copyStat(src, dst); err != nil {
		failures = append(failures, fmt.Sprintf("(%s, %s, %s)", pyRepr(src), pyRepr(dst), pyRepr(pyOSErrorText(err))))
	}
	if len(failures) > 0 {
		return &PyError{Class: "Error", Message: "[" + strings.Join(failures, ", ") + "]"}
	}
	return nil
}

// Paths under root a pathlib glob pattern matches, double star spanning directories without following links
func pathlibGlob(root, pattern string) []string {
	parts := strings.FieldsFunc(filepath.ToSlash(pattern), func(r rune) bool { return r == '/' })
	var walk func(dir string, parts []string) []string
	walk = func(dir string, parts []string) []string {
		if len(parts) == 0 {
			return []string{dir}
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}
		var out []string
		if parts[0] == "**" {
			if len(parts) == 1 {
				for _, e := range entries {
					child := filepath.Join(dir, e.Name())
					out = append(out, child)
					if st, err := os.Lstat(child); err == nil && st.IsDir() {
						out = append(out, walk(child, parts)...)
					}
				}
				return out
			}
			out = append(out, walk(dir, parts[1:])...)
			for _, e := range entries {
				child := filepath.Join(dir, e.Name())
				if st, err := os.Lstat(child); err == nil && st.IsDir() {
					out = append(out, walk(child, parts)...)
				}
			}
			return out
		}
		for _, e := range entries {
			if !fnmatchCase(e.Name(), parts[0]) {
				continue
			}
			child := filepath.Join(dir, e.Name())
			if len(parts) > 1 && !isDirectory(child) {
				continue
			}
			out = append(out, walk(child, parts[1:])...)
		}
		return out
	}
	seen := map[string]bool{}
	var out []string
	for _, match := range walk(root, parts) {
		if !seen[match] {
			seen[match] = true
			out = append(out, match)
		}
	}
	return out
}

// Snapshots the core project's build inputs, packages and named files, never from the signed source in place
func copyCoreInputs(source, destination string) error {
	text, err := readUTF8SigText(filepath.Join(source, "pyproject.toml"))
	if err != nil {
		return err
	}
	metadata, err := tomlLoads(text)
	if err != nil {
		return err
	}
	project, err := tomlGet(metadata, "project", NewOrderedObject())
	if err != nil {
		return err
	}
	setuptools := any(NewOrderedObject())
	include := any([]any{"*"})
	tool, err := tomlGet(metadata, "tool", NewOrderedObject())
	if err == nil {
		setuptools, err = tomlGet(tool, "setuptools", NewOrderedObject())
	}
	var packages, find any
	if err == nil {
		packages, err = tomlGet(setuptools, "packages", NewOrderedObject())
	}
	if err == nil {
		find, err = tomlGet(packages, "find", NewOrderedObject())
	}
	if err == nil {
		include, err = tomlGet(find, "include", include)
	}
	if err != nil {
		return err
	}
	patterns, err := pyIterate(include)
	if err != nil {
		return err
	}
	packageRoots := map[string]bool{}
	for _, pattern := range patterns {
		text, ok := pattern.(string)
		if !ok {
			return &PyError{Class: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'split'", tomlTypeName(pattern))}
		}
		packageRoots[strings.SplitN(text, ".", 2)[0]] = true
	}
	files := map[string]bool{"pyproject.toml": true, "setup.py": true, "setup.cfg": true}
	readme, err := tomlGet(project, "readme", nil)
	if err != nil {
		return err
	}
	switch r := readme.(type) {
	case string:
		files[r] = true
	case *OrderedObject:
		if file, ok := r.Get("file"); ok {
			files[pyStr(file)] = true
		}
	}
	licenseFiles, err := tomlGet(project, "license-files", []any{})
	if err != nil {
		return err
	}
	licensePatterns, err := pyIterate(licenseFiles)
	if err != nil {
		return err
	}
	for _, pattern := range licensePatterns {
		for _, match := range pathlibGlob(source, pyStr(pattern)) {
			if rel, err := filepath.Rel(source, match); err == nil {
				files[rel] = true
			}
		}
	}
	for _, match := range pathlibGlob(source, "*.py") {
		files[filepath.Base(match)] = true
	}
	excluded := map[string]bool{".git": true, ".venv": true, "venv": true, "node_modules": true, "__pycache__": true, "build": true, "dist": true, "release": true}
	ignore := func(directory string, names []string) map[string]bool {
		published := publishedStoreEntries(directory)
		out := map[string]bool{}
		for _, name := range names {
			nested := excluded[name] && name != "dist"
			st, err := os.Lstat(filepath.Join(directory, name))
			link := err == nil && st.Mode()&fs.ModeSymlink != 0
			if nested || strings.HasPrefix(name, ".") || published[name] || strings.HasSuffix(name, ".egg-info") || link {
				out[name] = true
			}
		}
		return out
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	resolvedDestination := resolvePath(destination)
	for _, e := range entries {
		entry := filepath.Join(source, e.Name())
		st, err := os.Lstat(entry)
		if err != nil || st.Mode()&fs.ModeSymlink != 0 || !isDirectory(entry) {
			continue
		}
		if excluded[e.Name()] || strings.HasPrefix(e.Name(), ".") || resolvePath(entry) == resolvedDestination {
			continue
		}
		matched := false
		for pattern := range packageRoots {
			if fnmatchCase(e.Name(), pattern) {
				matched = true
				break
			}
		}
		if matched {
			if err := copyTreeLike(entry, filepath.Join(destination, e.Name()), false, ignore); err != nil {
				return err
			}
		}
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	resolvedSource := resolvePath(source)
	for _, name := range names {
		entry := filepath.Join(source, name)
		st, err := os.Lstat(entry)
		if !isRegularFile(entry) || (err == nil && st.Mode()&fs.ModeSymlink != 0) {
			continue
		}
		if !pathIsRelativeTo(resolvePath(entry), resolvedSource) {
			return NewInstallError("venv", "build input escapes the core project: "+name, "")
		}
		target := filepath.Join(destination, name)
		if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
			return err
		}
		if err := copy2(entry, target); err != nil {
			return err
		}
	}
	return nil
}

// Whether a path equals or lies below a base, compared by components as pathlib is_relative_to does
func pathIsRelativeTo(path, base string) bool {
	rel, err := filepath.Rel(base, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Table under key, created empty when absent, the AttributeError Python raises when the parent is no table
func tomlSetDefaultTable(parent any, key string) (*OrderedObject, error) {
	obj, ok := parent.(*OrderedObject)
	if !ok {
		return nil, &PyError{Class: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'setdefault'", tomlTypeName(parent))}
	}
	value, present := obj.Get(key)
	if !present {
		table := NewOrderedObject()
		obj.Set(key, table)
		return table, nil
	}
	table, ok := value.(*OrderedObject)
	if !ok {
		return nil, &PyError{Class: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'setdefault'", tomlTypeName(value))}
	}
	return table, nil
}

// Removes a key from an ordered object, reporting whether it was present
func orderedPop(obj *OrderedObject, key string) bool {
	if _, ok := obj.Values[key]; !ok {
		return false
	}
	delete(obj.Values, key)
	for i, k := range obj.Keys {
		if k == key {
			obj.Keys = append(obj.Keys[:i:i], obj.Keys[i+1:]...)
			break
		}
	}
	return true
}

// Snapshots core and plugin build inputs into a fresh generation for the interpreter that installs it
func generatePyproject(ctx context.Context, pluginDirs []MemberDir, root, source, python string, extras []string) error {
	source = resolvePath(absoluteUnresolved(source))
	resolvedRoot := resolvePath(absoluteUnresolved(root))
	if resolvedRoot == source || pathIsRelativeTo(source, resolvedRoot) {
		return NewInstallError("venv", "workspace must not replace the core source", "")
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o777); err != nil {
		return err
	}
	if err := os.Mkdir(root, 0o777); err != nil {
		return err
	}
	coreText, err := readUTF8SigText(filepath.Join(source, "pyproject.toml"))
	if err != nil {
		return err
	}
	var candidates []MemberDir
	for _, member := range MemberSources(pluginDirs) {
		declaration, err := ReadPythonDeclaration(member.Source)
		if err != nil {
			return err
		}
		installs, err := declaration.InstallsPackages()
		if err != nil {
			return err
		}
		if installs {
			candidates = append(candidates, member)
		}
	}
	target := map[string]string{}
	if len(candidates) > 0 {
		if target, err = targetEnvironment(ctx, python); err != nil {
			return err
		}
	}
	var members []string
	for _, member := range candidates {
		path, err := workspaceMember(member.Source, root, member.Identity, extras, target)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		members = append(members, filepath.ToSlash(rel))
	}
	var text string
	if len(members) > 0 {
		document, err := tomlLoads(coreText)
		if err != nil {
			return err
		}
		tool, err := tomlSetDefaultTable(document, "tool")
		if err != nil {
			return err
		}
		uvPolicy, err := tomlSetDefaultTable(tool, "uv")
		if err != nil {
			return err
		}
		orderedPop(uvPolicy, "exclude-newer")
		orderedPop(uvPolicy, "exclude-newer-package")
		sort.Strings(members)
		memberList := make([]any, len(members))
		for i, m := range members {
			memberList[i] = m
		}
		workspace := NewOrderedObject()
		workspace.Set("members", memberList)
		uvPolicy.Set("workspace", workspace)
		sources, err := tomlSetDefaultTable(uvPolicy, "sources")
		if err != nil {
			return err
		}
		project, err := tomlIndex(document, "project")
		if err != nil {
			return err
		}
		name, err := tomlIndex(project, "name")
		if err != nil {
			return err
		}
		core := NewOrderedObject()
		core.Set("workspace", true)
		sources.Set(pyStr(name), core)
		if err := solveOnlyFor(uvPolicy, target); err != nil {
			return err
		}
		if text, err = tomliWDumps(document); err != nil {
			return err
		}
	} else {
		text = strings.TrimRight(coreText, "\n") + "\n"
	}
	text = withoutReleaseAge(text)
	if err := copyCoreInputs(source, root); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "pyproject.toml"), []byte(text), 0o666)
}

var releaseAgeLine = regexp.MustCompile(`(?m)^exclude-newer[\t\n\v\f\r \x1c-\x1f\x85\p{Z}]*=.*\n`)

// Drops tool.uv exclude-newer lines from a generated root's pyproject text
func withoutReleaseAge(text string) string {
	return releaseAgeLine.ReplaceAllString(text, "")
}

// Prints packaging's default marker environment as computed by the probed interpreter itself
const markerEnvironmentProbe = `
import json, os, platform, sys
v = sys.implementation.version
level = "" if v.releaselevel == "final" else f"{v.releaselevel[0]}{v.serial}"
print(json.dumps({
    "implementation_name": sys.implementation.name,
    "implementation_version": f"{v.major}.{v.minor}.{v.micro}{level}",
    "os_name": os.name,
    "platform_machine": platform.machine(),
    "platform_release": platform.release(),
    "platform_system": platform.system(),
    "platform_version": platform.version(),
    "python_full_version": platform.python_version(),
    "platform_python_implementation": platform.python_implementation(),
    "python_version": ".".join(platform.python_version_tuple()[:2]),
    "sys_platform": sys.platform,
}))
`

// PEP 508 marker environment of the interpreter that installs a generation, asked of that interpreter
func targetEnvironment(ctx context.Context, python string) (map[string]string, error) {
	argv := []string{Native(python), "-I", "-c", markerEnvironmentProbe}
	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.WaitDelay = time.Second
	var stdout strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &strings.Builder{}
	err := cmd.Run()
	if runCtx.Err() == context.DeadlineExceeded {
		return nil, &PyError{Class: "TimeoutExpired", Message: fmt.Sprintf("Command '%s' timed out after 60 seconds", pyListRepr(argv))}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil, &PyError{Class: "CalledProcessError", Message: fmt.Sprintf("Command '%s' returned non-zero exit status %d.", pyListRepr(argv), pyReturnCode(exitErr))}
	}
	if err != nil {
		return nil, err
	}
	if err := strictUTF8([]byte(stdout.String())); err != nil {
		return nil, &PyError{Class: "UnicodeDecodeError", Message: err.Error()}
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(stdout.String()), &out); err != nil {
		return nil, &PyError{Class: "JSONDecodeError", Message: err.Error()}
	}
	return out, nil
}

// Narrows tool.uv.environments to the platform and machine of the installing interpreter
func solveOnlyFor(uvPolicy *OrderedObject, target map[string]string) error {
	host := fmt.Sprintf("sys_platform == %s and platform_machine == %s", pyRepr(target["sys_platform"]), pyRepr(target["platform_machine"]))
	var narrowed []any
	if existing, ok := uvPolicy.Get("environments"); ok && tomlTruthy(existing) {
		entries, err := pyIterate(existing)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			narrowed = append(narrowed, fmt.Sprintf("(%s) and %s", pyStr(entry), host))
		}
	}
	if len(narrowed) == 0 {
		narrowed = []any{host}
	}
	uvPolicy.Set("environments", narrowed)
	return nil
}

// PluginEntry is one selected plugin, the home plugins dir naming it, its selection key and its tree
type PluginEntry struct {
	PluginsDir string
	Name       string
	PluginDir  string
}

// Every selected plugin in config order with the key its home names it by
func enabledPluginEntries(ctx context.Context, opts EnabledPluginsOptions) ([]PluginEntry, error) {
	selection, err := EnabledPluginsOrdered(ctx, opts)
	if err != nil {
		return nil, err
	}
	var entries []PluginEntry
	for _, home := range selection {
		for _, name := range home.Names {
			pluginDir, err := SelectedPluginDir(home.PluginsDir, name, opts.Installing)
			if err != nil {
				return nil, err
			}
			if pluginDir != "" {
				entries = append(entries, PluginEntry{PluginsDir: home.PluginsDir, Name: name, PluginDir: pluginDir})
			}
		}
	}
	return entries, nil
}

// Tree a selection key names, the home's own plugins dir first then the bundled plugins, empty when neither has it
func SelectedPluginDir(pluginsDir, name, installing string) (string, error) {
	relative := filepath.FromSlash(name)
	if filepath.IsAbs(relative) || strings.HasPrefix(name, "/") {
		return "", NewInstallError("venv", "invalid plugin key: "+name, "")
	}
	for _, part := range strings.Split(filepath.ToSlash(relative), "/") {
		if part == ".." {
			return "", NewInstallError("venv", "invalid plugin key: "+name, "")
		}
	}
	pluginDir := filepath.Join(pluginsDir, relative)
	if installing != "" && resolvePath(pluginDir) == resolvePath(installing) {
		return pluginDir, nil
	}
	isDir, err := pluginIsDirectory(pluginDir)
	if err != nil {
		return "", err
	}
	if !isDir {
		pluginDir = filepath.Join(RepoRoot(), "plugins", relative)
	}
	if isDir, err = pluginIsDirectory(pluginDir); err != nil || !isDir {
		return "", err
	}
	return pluginDir, nil
}

// Effective plugin selection as distinct plugin trees, declarations not yet consulted
func enabledPluginDirs(ctx context.Context, opts EnabledPluginsOptions) ([]string, error) {
	entries, err := enabledPluginEntries(ctx, opts)
	if err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	for _, entry := range entries {
		if !seen[entry.PluginDir] {
			seen[entry.PluginDir] = true
			out = append(out, entry.PluginDir)
		}
	}
	return out, nil
}

// MemberSelection proposes one home's enabled and disabled lists, an empty home reading every config as written
type MemberSelection struct {
	ProposedHome string
	Enabled      []string
	Disabled     []string
}

// Selected plugins that install packages, misfits sitting out and a too new manifest refused
func EnabledMemberDirs(ctx context.Context, selection MemberSelection) ([]string, error) {
	selected, err := enabledPluginDirs(ctx, EnabledPluginsOptions{
		ProposedHome: selection.ProposedHome, Enabled: selection.Enabled, Disabled: selection.Disabled,
		SkipInvalidSecondary: selection.ProposedHome == "",
	})
	if err != nil {
		return nil, err
	}
	var members []string
	for _, path := range selected {
		declaration, err := ReadPythonDeclaration(path)
		if err != nil {
			return nil, err
		}
		misfit, err := hermesFitError(ctx, declaration, []string{})
		if err != nil {
			return nil, err
		}
		if misfit != "" {
			continue
		}
		reason, err := ManifestVersionError(declaration.Manifest, filepath.Base(path))
		if err != nil {
			return nil, err
		}
		if reason != "" {
			return nil, NewInstallError("venv", reason, "")
		}
		installs, err := declaration.InstallsPackages()
		if err != nil {
			return nil, err
		}
		if installs {
			members = append(members, path)
		}
	}
	return members, nil
}

// Enabled members for a build selecting core extras, judged on the markers of the build's interpreter
func BuildMemberDirs(ctx context.Context, extras []string, python string) ([]MemberDir, error) {
	if extras == nil {
		extras = []string{}
	}
	members, err := EnabledMemberDirs(ctx, MemberSelection{})
	if err != nil {
		return nil, err
	}
	var target map[string]string
	if len(members) > 0 && python != "" {
		if target, err = targetEnvironment(ctx, python); err != nil {
			return nil, err
		}
	}
	var out []string
	for _, path := range members {
		declaration, err := ReadPythonDeclaration(path)
		if err != nil {
			return nil, err
		}
		reason, err := coreRequirementError(ctx, declaration, extras, target)
		if err != nil {
			return nil, err
		}
		if reason == "" {
			out = append(out, path)
		}
	}
	return MemberPaths(out), nil
}

var memberNameUnsafe = regexp.MustCompile(`[^a-z0-9._-]+`)

// Plugin dir name plus sixteen hex digits of its resolved path hash, unique across homes and readable in uv errors
func memberKey(identity string) string {
	sum := sha256.Sum256([]byte(resolvePath(absoluteUnresolved(identity))))
	name := strings.Trim(memberNameUnsafe.ReplaceAllString(pyLower(pyPathName(identity)), "-"), "-.")
	if name == "" {
		name = "plugin"
	}
	return name + "-" + hex.EncodeToString(sum[:])[:16]
}

// Requirements among specs that name the project itself, unparsable ones skipped
func selfReferences(specs []any, own string) ([]*pyRequirement, error) {
	var out []*pyRequirement
	for _, spec := range specs {
		text, ok := spec.(string)
		if !ok {
			return nil, &PyError{Class: "TypeError", Message: fmt.Sprintf("expected string or bytes-like object, got '%s'", tomlTypeName(spec))}
		}
		req, err := ParseRequirement(text)
		if err != nil {
			var invalid *InvalidRequirementError
			if errors.As(err, &invalid) {
				continue
			}
			return nil, err
		}
		if CanonicalizeName(req.Name) == own {
			out = append(out, req)
		}
	}
	return out, nil
}

// markerTarget yields the marker environment a reference is judged on, nil when every reference applies
type markerTarget func() (map[string]string, error)

// Fixed marker environment, nil meaning no target
func fixedTarget(target map[string]string) markerTarget {
	if target == nil {
		return nil
	}
	return func() (map[string]string, error) { return target, nil }
}

// Whether a requirement applies on the target under any installed extra, undefined comparisons counting as applying
func referenceActive(req *pyRequirement, contexts []string, target markerTarget) (bool, error) {
	if len(req.Marker) == 0 || target == nil {
		return true, nil
	}
	environment, err := target()
	if err != nil {
		return false, err
	}
	for _, extra := range contexts {
		env := map[string]string{}
		for k, v := range environment {
			env[k] = v
		}
		env["extra"] = extra
		ok, err := evaluateMarker(req.Marker, nil, env)
		if err != nil {
			var undefined *UndefinedMarkerError
			if errors.As(err, &undefined) {
				return true, nil
			}
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// Member extras a generation installs, closed over self references, and the extras only named
func selectedMemberExtras(document *OrderedObject, extras []string, target markerTarget) (map[string]bool, map[string]bool, error) {
	project, err := tomlGet(document, "project", NewOrderedObject())
	if err != nil {
		return nil, nil, err
	}
	optional, err := tomlGet(project, "optional-dependencies", NewOrderedObject())
	if err != nil {
		return nil, nil, err
	}
	optionalTable, ok := optional.(*OrderedObject)
	if !ok {
		return nil, nil, &PyError{Class: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'items'", tomlTypeName(optional))}
	}
	declared := map[string][]any{}
	for _, key := range optionalTable.Keys {
		specs, err := pyIterate(optionalTable.Values[key])
		if err != nil {
			return nil, nil, err
		}
		declared[ExtraKey(key)] = specs
	}
	name, err := tomlGet(project, "name", "")
	if err != nil {
		return nil, nil, err
	}
	own := CanonicalizeName(pyStr(name))
	selected := map[string]bool{}
	for _, extra := range extras {
		if _, ok := declared[ExtraKey(extra)]; ok {
			selected[ExtraKey(extra)] = true
		}
	}
	base, err := tomlGet(project, "dependencies", []any{})
	if err != nil {
		return nil, nil, err
	}
	baseSpecs, err := pyIterate(base)
	if err != nil {
		return nil, nil, err
	}
	baseRefs, err := selfReferences(baseSpecs, own)
	if err != nil {
		return nil, nil, err
	}
	references := map[string][]*pyRequirement{"": baseRefs}
	for {
		for _, key := range sortedKeys(selected) {
			if _, done := references[key]; !done {
				if references[key], err = selfReferences(declared[key], own); err != nil {
					return nil, nil, err
				}
			}
		}
		contexts := append([]string{""}, sortedKeys(selected)...)
		reached := map[string]bool{}
		for _, requirements := range references {
			for _, req := range requirements {
				active, err := referenceActive(req, contexts, target)
				if err != nil {
					return nil, nil, err
				}
				if !active {
					continue
				}
				for _, extra := range req.Extras {
					if _, ok := declared[ExtraKey(extra)]; ok {
						reached[ExtraKey(extra)] = true
					}
				}
			}
		}
		grew := false
		for key := range reached {
			if !selected[key] {
				selected[key] = true
				grew = true
			}
		}
		if !grew {
			break
		}
	}
	named := map[string]bool{}
	for _, requirements := range references {
		for _, req := range requirements {
			for _, extra := range req.Extras {
				if _, ok := declared[ExtraKey(extra)]; ok && !selected[ExtraKey(extra)] {
					named[ExtraKey(extra)] = true
				}
			}
		}
	}
	return selected, named, nil
}

// Drops tool.uv.sources alternatives for extras or groups the generation removed, reporting a change
func pruneMemberSources(settings any, installed map[string]bool) bool {
	table, ok := settings.(*OrderedObject)
	if !ok {
		return false
	}
	sources, ok := table.Values["sources"].(*OrderedObject)
	if !ok {
		return false
	}
	applies := func(spec any) bool {
		row, ok := spec.(*OrderedObject)
		if !ok {
			return true
		}
		if _, group := row.Get("group"); group {
			return false
		}
		extra, has := row.Get("extra")
		return !has || installed[ExtraKey(pyStr(extra))]
	}
	pruned := NewOrderedObject()
	changed := false
	for _, pkg := range sources.Keys {
		entry := sources.Values[pkg]
		if list, ok := entry.([]any); ok {
			var alternatives []any
			for _, spec := range list {
				if applies(spec) {
					alternatives = append(alternatives, spec)
				}
			}
			if len(alternatives) != len(list) {
				changed = true
			}
			if len(alternatives) > 0 {
				pruned.Set(pkg, alternatives)
			}
		} else if applies(entry) {
			pruned.Set(pkg, entry)
		} else {
			changed = true
		}
	}
	if !changed {
		return false
	}
	if len(pruned.Keys) > 0 {
		table.Set("sources", pruned)
	} else {
		orderedPop(table, "sources")
	}
	return true
}

var leadingRequirementName = regexp.MustCompile(`^[\t\n\v\f\r \x1c-\x1f\x85\p{Z}]*[A-Za-z0-9][A-Za-z0-9._-]*`)

// Gives a virtual member its unique workspace name, self references in dependencies, extras and conflicts included
func renameVirtualMember(document *OrderedObject, name string) error {
	project, err := tomlSetDefaultTable(document, "project")
	if err != nil {
		return err
	}
	declared, _ := project.Get("name")
	if declared == nil {
		declared = ""
	}
	own := CanonicalizeName(pyStr(declared))
	project.Set("name", name)
	if own == "" {
		return nil
	}
	renamed := func(spec any) any {
		text, ok := spec.(string)
		if !ok {
			return spec
		}
		req, err := ParseRequirement(text)
		if err != nil || CanonicalizeName(req.Name) != own {
			return spec
		}
		loc := leadingRequirementName.FindStringIndex(text)
		if loc == nil {
			return spec
		}
		return name + text[loc[1]:]
	}
	renameAll := func(value any) ([]any, error) {
		specs, err := pyIterate(value)
		if err != nil {
			return nil, err
		}
		out := make([]any, len(specs))
		for i, spec := range specs {
			out[i] = renamed(spec)
		}
		return out, nil
	}
	if deps, ok := project.Get("dependencies"); ok {
		out, err := renameAll(deps)
		if err != nil {
			return err
		}
		project.Set("dependencies", out)
	}
	optional, err := tomlGet(project, "optional-dependencies", NewOrderedObject())
	if err != nil {
		return err
	}
	if table, ok := optional.(*OrderedObject); ok {
		for _, extra := range table.Keys {
			out, err := renameAll(table.Values[extra])
			if err != nil {
				return err
			}
			table.Set(extra, out)
		}
	}
	tool, err := tomlGet(document, "tool", NewOrderedObject())
	if err != nil {
		return err
	}
	uv, err := tomlGet(tool, "uv", NewOrderedObject())
	if err != nil {
		return err
	}
	conflicts, err := tomlGet(uv, "conflicts", []any{})
	if err != nil {
		return err
	}
	groups, err := pyIterate(conflicts)
	if err != nil {
		return err
	}
	for _, conflict := range groups {
		items, err := pyIterate(conflict)
		if err != nil {
			return err
		}
		for _, item := range items {
			row, ok := item.(*OrderedObject)
			if !ok {
				continue
			}
			pkg, _ := row.Get("package")
			if pkg == nil {
				pkg = ""
			}
			if CanonicalizeName(pyStr(pkg)) == own {
				row.Set("package", name)
			}
		}
	}
	return nil
}

// Keeps what a generation installs from a member's metadata, dropping groups, unselected extras and their sources
func runtimeMemberMetadata(document *OrderedObject, extras []string, target markerTarget) (bool, error) {
	changed := orderedPop(document, "dependency-groups")
	projectValue, err := tomlGet(document, "project", NewOrderedObject())
	if err != nil {
		return false, err
	}
	keep := map[string]bool{}
	optionalValue, err := tomlGet(projectValue, "optional-dependencies", nil)
	if err != nil {
		return false, err
	}
	if optionalValue != nil && tomlTruthy(optionalValue) {
		optional, ok := optionalValue.(*OrderedObject)
		if !ok {
			return false, &PyError{Class: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'items'", tomlTypeName(optionalValue))}
		}
		selected, named, err := selectedMemberExtras(document, extras, target)
		if err != nil {
			return false, err
		}
		keep = selected
		kept := NewOrderedObject()
		for _, key := range optional.Keys {
			extra := ExtraKey(key)
			switch {
			case keep[extra]:
				kept.Set(key, optional.Values[key])
			case named[extra]:
				kept.Set(key, []any{})
			}
		}
		if !orderedEqual(kept, optional) {
			changed = true
			project := projectValue.(*OrderedObject)
			if len(kept.Keys) > 0 {
				project.Set("optional-dependencies", kept)
			} else {
				orderedPop(project, "optional-dependencies")
			}
		}
	}
	keptExtras := map[string]bool{}
	if project, ok := projectValue.(*OrderedObject); ok {
		if optional, ok := project.Values["optional-dependencies"].(*OrderedObject); ok {
			for _, key := range optional.Keys {
				keptExtras[ExtraKey(key)] = true
			}
		}
	}
	tool, err := tomlGet(document, "tool", NewOrderedObject())
	if err != nil {
		return false, err
	}
	settingsValue, err := tomlGet(tool, "uv", NewOrderedObject())
	if err != nil {
		return false, err
	}
	changed = pruneMemberSources(settingsValue, keep) || changed
	settings, ok := settingsValue.(*OrderedObject)
	if !ok {
		return changed, nil
	}
	for _, key := range []string{"default-groups", "dependency-groups", "dev-dependencies"} {
		if orderedPop(settings, key) {
			changed = true
		}
	}
	if conflictsValue, ok := settings.Get("conflicts"); ok {
		groups, err := pyIterate(conflictsValue)
		if err != nil {
			return false, err
		}
		var conflicts []any
		for _, group := range groups {
			specs, err := pyIterate(group)
			if err != nil {
				return false, err
			}
			var kept []any
			for _, spec := range specs {
				row, _ := spec.(*OrderedObject)
				if row != nil {
					if _, isGroup := row.Get("group"); isGroup {
						continue
					}
					if extra, hasExtra := row.Get("extra"); hasExtra && !keptExtras[ExtraKey(pyStr(extra))] {
						continue
					}
				}
				kept = append(kept, spec)
			}
			if len(kept) > 1 {
				conflicts = append(conflicts, kept)
			}
		}
		if !jsonEqual(conflicts, groups) {
			if len(conflicts) > 0 {
				settings.Set("conflicts", conflicts)
			} else {
				orderedPop(settings, "conflicts")
			}
			changed = true
		}
	}
	return changed, nil
}

// Deep equality of two decoded TOML values, tables compared as mappings
func jsonEqual(a, b any) bool {
	switch x := a.(type) {
	case *OrderedObject:
		y, ok := b.(*OrderedObject)
		return ok && orderedEqual(x, y)
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jsonEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case nil:
		if y, ok := b.([]any); ok {
			return len(y) == 0
		}
		return b == nil
	}
	return a == b
}

// Mapping equality of two ordered objects, key order ignored as Python dict comparison does
func orderedEqual(a, b *OrderedObject) bool {
	if len(a.Keys) != len(b.Keys) {
		return false
	}
	for _, k := range a.Keys {
		v, ok := b.Values[k]
		if !ok || !jsonEqual(a.Values[k], v) {
			return false
		}
	}
	return true
}

// Extras of the generation this install selected, what gates outside a build evaluate plugin extras under
func recordedExtras(ctx context.Context) ([]string, error) {
	facts, err := NewFacts(EnvironmentRuntimeFactsPath(ctx, RepoRoot()), false)
	if err != nil {
		return nil, err
	}
	venv, _ := facts.Get("venv")
	extras, ok := venv["extras"].([]any)
	out := []string{}
	if !ok {
		return out, nil
	}
	for _, extra := range extras {
		if s, ok := extra.(string); ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// Marker environment of the interpreter that runs plugin code, the selected environment's Python or the pinned one
func loaderMarkerEnvironment(ctx context.Context) (map[string]string, error) {
	venv, err := SelectedVenv(ctx, RepoRoot())
	if err != nil {
		return nil, err
	}
	python := VenvPython(venv)
	if !isRegularFile(python) {
		_, pinned, found, err := Toolchain(ctx, false, false)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, NewInstallError("venv", "no Python interpreter is installed to judge plugin requirement markers", "")
		}
		python = pinned
	}
	return targetEnvironment(ctx, python)
}

// Why a plugin's applicable requirement on icebath-agent rejects the running version, empty when it fits
func coreRequirementError(ctx context.Context, declaration *PythonDeclaration, extras []string, target map[string]string) (string, error) {
	if declaration.External() {
		return "", nil
	}
	environment := fixedTarget(target)
	if target == nil {
		var cached map[string]string
		environment = func() (map[string]string, error) {
			if cached == nil {
				env, err := loaderMarkerEnvironment(ctx)
				if err != nil {
					return nil, err
				}
				cached = env
			}
			return cached, nil
		}
	}
	specs := make([]any, len(declaration.Requirements))
	for i, spec := range declaration.Requirements {
		specs[i] = spec
	}
	installed := map[string]bool{}
	if declaration.Pyproject != "" {
		text, err := readUTF8SigText(declaration.Pyproject)
		if err != nil {
			return "", err
		}
		document, err := tomlLoads(text)
		if err != nil {
			return "", err
		}
		selection := extras
		if selection == nil {
			if selection, err = recordedExtras(ctx); err != nil {
				return "", err
			}
		}
		if installed, _, err = selectedMemberExtras(document, selection, environment); err != nil {
			return "", err
		}
		project, err := tomlIndex(document, "project")
		if err != nil {
			return "", err
		}
		optional, err := tomlGet(project, "optional-dependencies", NewOrderedObject())
		if err != nil {
			return "", err
		}
		if table, ok := optional.(*OrderedObject); ok {
			for _, name := range table.Keys {
				if !installed[ExtraKey(name)] {
					continue
				}
				values, err := pyIterate(table.Values[name])
				if err != nil {
					return "", err
				}
				specs = append(specs, values...)
			}
		}
	}
	references, err := selfReferences(specs, "icebath-agent")
	if err != nil {
		return "", err
	}
	contexts := append([]string{""}, sortedKeys(installed)...)
	var pins []*pyRequirement
	for _, req := range references {
		if len(req.Specifier) == 0 {
			continue
		}
		active, err := referenceActive(req, contexts, environment)
		if err != nil {
			return "", err
		}
		if active {
			pins = append(pins, req)
		}
	}
	if len(pins) == 0 {
		return "", nil
	}
	host, err := manifestHost()
	if err != nil {
		return "", err
	}
	current := host.RunningVersion()
	if _, valid, err := parsePEP440(current); err != nil || !valid {
		return "", nil
	}
	for _, req := range pins {
		ok, err := specifierSetContains(req.Specifier, current)
		if err != nil {
			return "", err
		}
		if !ok {
			text, err := req.String()
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("requires %s, running %s", text, current), nil
		}
	}
	return "", nil
}

// Why the running icebath is not one the plugin accepts, its requires_icebath or an applicable core requirement
func hermesFitError(ctx context.Context, declaration *PythonDeclaration, extras []string) (string, error) {
	host, err := manifestHost()
	if err != nil {
		return "", err
	}
	if reason := host.RequiresError(declaration.Manifest); reason != "" {
		return reason, nil
	}
	return coreRequirementError(ctx, declaration, extras, nil)
}

// Workspace member of a plugin inside the generation, its pyproject trimmed to what installs or a generated deps project
func workspaceMember(pluginDir, root, identity string, extras []string, target map[string]string) (string, error) {
	key := memberKey(identity)
	declaration, err := ReadPythonDeclaration(pluginDir)
	if err != nil {
		return "", err
	}
	install, err := declaration.InstallRequirements()
	if err != nil {
		return "", err
	}
	if declaration.Pyproject == "" {
		member := filepath.Join(root, "plugin-deps", key)
		if err := os.MkdirAll(filepath.Dir(member), 0o777); err != nil {
			return "", err
		}
		if err := os.Mkdir(member, 0o777); err != nil {
			return "", err
		}
		specs := make([]any, len(install))
		for i, spec := range install {
			specs[i] = spec
		}
		encoded, err := dumpsJSON(specs, false)
		if err != nil {
			return "", err
		}
		text := fmt.Sprintf("[project]\nname = \"icebath-plugin-%s\"\nversion = \"0.0.0\"\nrequires-python = \">=3.11\"\ndependencies = %s\n[tool.uv]\npackage = false\n", key, encoded)
		return member, os.WriteFile(filepath.Join(member, "pyproject.toml"), []byte(text), 0o666)
	}
	member := filepath.Join(root, "plugin-sources", key)
	if err := copyTreeLike(pluginDir, member, true, memberIgnored); err != nil {
		return "", err
	}
	text, err := readUTF8SigText(declaration.Pyproject)
	if err != nil {
		return "", err
	}
	document, err := tomlLoads(text)
	if err != nil {
		return "", err
	}
	_, buildable := document.Get("build-system")
	tool, err := tomlGet(document, "tool", NewOrderedObject())
	if err != nil {
		return "", err
	}
	uv, err := tomlGet(tool, "uv", NewOrderedObject())
	if err != nil {
		return "", err
	}
	packageMode, err := tomlGet(uv, "package", nil)
	if err != nil {
		return "", err
	}
	virtual := !buildable && packageMode != true
	changed := false
	if !jsonEqual(stringsAny(install), stringsAny(declaration.Requirements)) {
		project, err := tomlIndex(document, "project")
		if err != nil {
			return "", err
		}
		project.(*OrderedObject).Set("dependencies", stringsAny(install))
		changed = true
	}
	if virtual {
		if err := renameVirtualMember(document, "icebath-plugin-"+key); err != nil {
			return "", err
		}
	}
	trimmed, err := runtimeMemberMetadata(document, extras, fixedTarget(target))
	if err != nil {
		return "", err
	}
	changed = trimmed || changed
	if moved, err := rebaseMemberSources(document, pluginDir, identity); err != nil {
		return "", err
	} else if moved {
		changed = true
	}
	if project, ok := document.Values["project"].(*OrderedObject); ok {
		_, hasVersion := project.Get("version")
		dynamic, err := tomlGet(project, "dynamic", []any{})
		if err != nil {
			return "", err
		}
		dynamicItems, err := pyIterate(dynamic)
		if err != nil {
			return "", err
		}
		isDynamic := false
		for _, item := range dynamicItems {
			if item == "version" {
				isDynamic = true
			}
		}
		if !hasVersion && !isDynamic {
			project.Set("version", "0.0.0")
			changed = true
		}
	}
	if virtual || changed {
		rendered, err := tomliWDumps(document)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(member, "pyproject.toml"), []byte(rendered), 0o666); err != nil {
			return "", err
		}
	}
	return member, nil
}

// Strings as decoded TOML array items
func stringsAny(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// Points relative tool.uv.sources paths that leave the plugin tree at the installed identity, reporting a change
func rebaseMemberSources(document *OrderedObject, pluginDir, identity string) (bool, error) {
	tool, err := tomlGet(document, "tool", NewOrderedObject())
	if err != nil {
		return false, err
	}
	uv, err := tomlGet(tool, "uv", NewOrderedObject())
	if err != nil {
		return false, err
	}
	sources, err := tomlGet(uv, "sources", NewOrderedObject())
	if err != nil {
		return false, err
	}
	table, ok := sources.(*OrderedObject)
	if !ok {
		return false, &PyError{Class: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'values'", tomlTypeName(sources))}
	}
	changed := false
	resolvedPlugin := resolvePath(absoluteUnresolved(pluginDir))
	for _, pkg := range table.Keys {
		entry := table.Values[pkg]
		specs, isList := entry.([]any)
		if !isList {
			specs = []any{entry}
		}
		for _, spec := range specs {
			row, ok := spec.(*OrderedObject)
			if !ok {
				continue
			}
			pathValue, ok := row.Get("path")
			if !ok {
				continue
			}
			relative, ok := pathValue.(string)
			if !ok {
				return false, &PyError{Class: "TypeError", Message: fmt.Sprintf("argument should be a str or an os.PathLike object where __fspath__ returns a str, not '%s'", tomlTypeName(pathValue))}
			}
			if filepath.IsAbs(filepath.FromSlash(relative)) {
				continue
			}
			resolved := resolvePath(absoluteUnresolved(filepath.Join(pluginDir, filepath.FromSlash(relative))))
			if pathIsRelativeTo(resolved, resolvedPlugin) {
				continue
			}
			rebased := resolvePath(absoluteUnresolved(filepath.Join(identity, filepath.FromSlash(relative))))
			row.Set("path", filepath.ToSlash(rebased))
			changed = true
		}
	}
	return changed, nil
}

// Package acquisition the sidecar install goes through, replaceable by tests the way Python patches pm ensure
var sidecarEnsure = Ensure

// Installs a plugin's Node dependencies with PM's npm, returning a diagnostic or empty on success
func InstallNodeSidecar(ctx context.Context, pluginDir string, explicit bool) string {
	if !isRegularFile(filepath.Join(pluginDir, "package.json")) {
		return ""
	}
	if !explicit && !LazyInstallsAllowedFor(ctx) {
		return "lazy installs are disabled — run `icebath plugins install` and approve Node dependencies"
	}
	installCmd := "install"
	if isRegularFile(filepath.Join(pluginDir, "package-lock.json")) {
		installCmd = "ci"
	}
	runner, err := sidecarEnsure(ctx, "npm", EnsureOptions{Explicit: explicit})
	if err != nil {
		return fmt.Sprintf("npm %s failed to run: %s", installCmd, err)
	}
	searchPath := runner.Env["PATH"]
	npm := whichOn("npm", &searchPath)
	if npm == "" {
		return "npm is missing from the prepared PM environment"
	}
	argv := []string{npm, installCmd, "--no-audit", "--no-fund"}
	runCtx, cancel := context.WithTimeout(ctx, 900*time.Second)
	defer cancel()
	cmd := runner.Command(runCtx, argv[0], argv[1:]...)
	cmd.Dir = pluginDir
	cmd.WaitDelay = time.Second
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if runCtx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("npm %s failed to run: Command '%s' timed out after 900 seconds", installCmd, pyListRepr(argv))
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return fmt.Sprintf("npm %s failed to run: %s", installCmd, pyOSErrorText(err))
	}
	if exitErr == nil {
		return ""
	}
	decode := func(raw string) string {
		text, _ := decodeUTF8Replace([]byte(raw), true)
		return universalNewlines(text)
	}
	tail := decode(stderr.String())
	if tail == "" {
		tail = decode(stdout.String())
	}
	tail = pyStrip(tail)
	if n := utf8.RuneCountInString(tail); n > 300 {
		tail = string([]rune(tail)[n-300:])
	}
	return fmt.Sprintf("npm %s exited %d: %s", installCmd, pyReturnCode(exitErr), tail)
}

// LockAndSyncOptions name a fresh generation root, the core source, an optional seed lock or recorded replay, and the engine
type LockAndSyncOptions struct {
	Root        string
	Source      string
	SeedLock    string
	Replay      string
	Environment *PythonEnvironment
	Frozen      bool
}

// Prepares a fresh generation from explicit inputs or a recorded workspace and syncs it with the prepared engine
func LockAndSync(ctx context.Context, pluginDirs []MemberDir, extras []string, opts LockAndSyncOptions) error {
	if _, err := os.Lstat(opts.Root); err == nil {
		return NewInstallError("venv", "workspace must be fresh: "+opts.Root, "")
	}
	frozen := opts.Frozen
	if opts.Replay == "" {
		if err := generatePyproject(ctx, pluginDirs, opts.Root, opts.Source, opts.Environment.Python, extras); err != nil {
			return err
		}
		if opts.SeedLock != "" {
			data, err := os.ReadFile(opts.SeedLock)
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(opts.Root, "uv.lock"), data, 0o666); err != nil {
				return err
			}
		}
	} else {
		if !isRegularFile(filepath.Join(opts.Replay, "pyproject.toml")) || !isRegularFile(filepath.Join(opts.Replay, "uv.lock")) {
			return NewInstallError("venv", "recorded workspace is missing: "+opts.Replay, "")
		}
		if err := copyTreeLike(opts.Replay, opts.Root, true, memberIgnored); err != nil {
			return err
		}
		manifest := filepath.Join(opts.Root, "pyproject.toml")
		text, err := readUTF8SigText(manifest)
		if err != nil {
			return err
		}
		if err := os.WriteFile(manifest, []byte(withoutReleaseAge(text)), 0o666); err != nil {
			return err
		}
		frozen = true
	}
	if extras == nil {
		extras = []string{}
	}
	return opts.Environment.Sync(ctx, opts.Root, SyncOptions{Extras: extras, Resolve: !frozen})
}
