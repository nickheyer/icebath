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
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"

	"github.com/nickheyer/icebath/pkg/home"
)

// EnvInstallRoot names the tree whose install stamp a steward wrapper keeps outside the binary dir
const EnvInstallRoot = "ICEBATH_INSTALL_ROOT"

// EnvRuntimeDir points a running process at a non default tool store
const EnvRuntimeDir = "ICEBATH_RUNTIME_DIR"

// Directory of the resolved running executable, the tree this process runs from
var repoRoot = func() string {
	exe, err := os.Executable()
	if err != nil {
		wd, _ := os.Getwd()
		return wd
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe)
}

// Tree the running binary lives in
func RepoRoot() string { return repoRoot() }

// Makes dir the tree the running binary reports until the returned restore runs
func PatchRepoRoot(dir string) func() {
	saved := repoRoot
	repoRoot = func() string { return dir }
	return func() { repoRoot = saved }
}

// Tree whose install stamp belongs to this process, the steward override or the binary dir
func InstallRoot() string {
	if env := os.Getenv(EnvInstallRoot); env != "" {
		return env
	}
	return RepoRoot()
}

// Install stamp shared by every stamp reader, the executing tree resolving through the install root
func InstallStampPath(projectRoot string) string {
	root := projectRoot
	if resolvePath(root) == resolvePath(RepoRoot()) {
		root = InstallRoot()
	}
	return filepath.Join(root, "install-stamp.json")
}

// Byte store of this process for the binary's own tree
func StoreRoot(ctx context.Context) (string, error) {
	return EnvironmentStoreRoot(ctx, RepoRoot(), true)
}

// Machine scoped downloader partials area under the default root, writable even when the store is sealed
func PartialsRoot() string {
	return filepath.Join(home.Root(), "cache", "partials")
}

// Installed state file of the byte store
func FactsPath(ctx context.Context) (string, error) {
	root, err := StoreRoot(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "facts.json"), nil
}

// Store that accepts writes, the default root's tools dir when the store sits inside a sealed payload
func WritableStoreRoot(ctx context.Context) (string, error) {
	root, err := StoreRoot(ctx)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(filepath.Join(filepath.Dir(root), "manifest.json")); err != nil || !st.Mode().IsRegular() {
		return root, nil
	}
	return filepath.Join(home.Root(), "tools"), nil
}

// Installed state file of this binary's own dependency state
func RuntimeFactsPath(ctx context.Context) string {
	return EnvironmentRuntimeFactsPath(ctx, RepoRoot())
}

// Absolute symlink resolved path the way os.path.realpath resolves it, missing components kept lexically
func resolvePath(path string) string {
	return realpath(path)
}

// Non strict realpath, every error ignored and a symlink loop left as spelled
func realpath(path string) string {
	resolved, _ := realpathMode(path, false)
	return resolved
}

// Reports whether a path remainder starts at the filesystem root
func isSeparator(rest string) bool {
	return rest != "" && os.IsPathSeparator(rest[0])
}

// Path components the way realpath splits them, empty components kept
func splitPathComponents(text string) []string {
	if runtime.GOOS == "windows" {
		return strings.Split(strings.ReplaceAll(text, "/", `\`), `\`)
	}
	return strings.Split(text, "/")
}

// Iterative posixpath realpath, allowMissing raising every error but a missing component, else ignoring all errors
func realpathMode(path string, allowMissing bool) (string, error) {
	if path == "" {
		path = "."
	}
	volume := filepath.VolumeName(path)
	rest := path[len(volume):]
	resolved := volume + string(filepath.Separator)
	if !isSeparator(rest) {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		resolved = wd
	}
	type part struct {
		name string
		link bool
	}
	var stack []part
	push := func(text string) int {
		comps := splitPathComponents(text)
		for i := len(comps) - 1; i >= 0; i-- {
			stack = append(stack, part{name: comps[i]})
		}
		return len(comps)
	}
	remaining := push(rest)
	seen := map[string]*string{}
	for remaining > 0 {
		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if p.link {
			done := resolved
			seen[p.name] = &done
			continue
		}
		remaining--
		if p.name == "" || p.name == "." {
			continue
		}
		if p.name == ".." {
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, p.name)
		ignored := func(err error) bool { return !allowMissing || errors.Is(err, fs.ErrNotExist) }
		st, err := os.Lstat(next)
		if err != nil && ignored(err) {
			resolved = next
			continue
		}
		if err != nil {
			return "", err
		}
		if st.Mode()&fs.ModeSymlink == 0 {
			if allowMissing && remaining > 0 && !st.IsDir() {
				return "", &fs.PathError{Op: "realpath", Path: next, Err: syscall.ENOTDIR}
			}
			resolved = next
			continue
		}
		if cached, ok := seen[next]; ok {
			if cached == nil {
				if allowMissing {
					return "", &fs.PathError{Op: "realpath", Path: next, Err: syscall.ELOOP}
				}
				resolved = next
				continue
			}
			resolved = *cached
			continue
		}
		target, err := os.Readlink(next)
		if err != nil && ignored(err) {
			resolved = next
			continue
		}
		if err != nil {
			return "", err
		}
		if v := filepath.VolumeName(target); v != "" {
			resolved, target = v+string(filepath.Separator), target[len(v):]
		} else if isSeparator(target) {
			resolved = filepath.VolumeName(resolved) + string(filepath.Separator)
		}
		seen[next] = nil
		stack = append(stack, part{name: next, link: true})
		remaining += push(target)
	}
	return resolved, nil
}

// Reports whether path equals root or lies beneath it, both compared as given
func isRelativeTo(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// First sixteen hex digits of the sha256 of the resolved checkout path
func InstallKey(projectRoot string) string {
	sum := sha256.Sum256([]byte(resolvePath(projectRoot)))
	return hex.EncodeToString(sum[:])[:16]
}

// Data root dependency state is scoped to, the context home override's root when one is active
func DependencyHomeRoot(ctx context.Context) string {
	if override := home.Override(ctx); override != "" {
		return home.RootOf(override)
	}
	return home.Root()
}

// Directory holding every checkout's dependency state
func InstallsRoot(ctx context.Context) string {
	return filepath.Join(DependencyHomeRoot(ctx), "installs")
}

// Dependency state directory of one checkout
func InstallStateDir(ctx context.Context, projectRoot string) string {
	return filepath.Join(InstallsRoot(ctx), InstallKey(projectRoot))
}

// Installed state file of one checkout's dependency state
func EnvironmentRuntimeFactsPath(ctx context.Context, projectRoot string) string {
	return filepath.Join(InstallStateDir(ctx, projectRoot), "facts.json")
}

// Describes an access failure inside this install's dependency state, empty when the failure lies elsewhere
func InstallStatePermissionMessage(ctx context.Context, projectRoot string, err error) string {
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) || pathErr.Path == "" {
		return ""
	}
	denied := resolvePath(pathErr.Path)
	if !isRelativeTo(denied, resolvePath(InstallStateDir(ctx, projectRoot))) {
		return ""
	}
	return fmt.Sprintf("install state is not writable by this user (%s); run as the install owner or grant write access", denied)
}

// Data root owning a borrowed checkout's committed state, empty when the active root's state is that state
func OwningHomeRoot(ctx context.Context, projectRoot string) string {
	root := resolvePath(projectRoot)
	key := InstallKey(root)
	var candidates []string
	for _, candidate := range []string{filepath.Dir(root), home.PlatformDefault()} {
		dup := false
		for _, seen := range candidates {
			if seen == candidate {
				dup = true
			}
		}
		if dup {
			continue
		}
		if st, err := os.Stat(filepath.Join(candidate, "installs", key, "facts.json")); err == nil && st.Mode().IsRegular() {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	owner := candidates[0]
	if resolvePath(filepath.Join(owner, "installs", key)) == resolvePath(InstallStateDir(ctx, root)) {
		return ""
	}
	return owner
}

// Sealed payload manifest whose tree is root, with the directory its entries resolve from
func payloadManifest(root string) (map[string]any, string, bool) {
	for _, path := range []string{filepath.Join(filepath.Dir(root), "manifest.json"), filepath.Join(root, "manifest.json")} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var manifest map[string]any
		if err := json.Unmarshal(trimBOM(data), &manifest); err != nil {
			continue
		}
		repo, ok := manifest["repo"].(string)
		if !ok {
			continue
		}
		var commands any
		if rt, present := manifest["runtime"]; !present {
			commands = map[string]any{}
		} else if rtMap, isMap := rt.(map[string]any); isMap {
			if c, has := rtMap["commands"]; has {
				commands = c
			} else {
				commands = map[string]any{}
			}
		}
		cmdMap, isMap := commands.(map[string]any)
		wellFormed := isMap
		if wellFormed {
			if v, has := cmdMap["icebath"]; has {
				_, wellFormed = v.(string)
			}
		}
		for _, key := range []string{"venv", "store"} {
			if v, has := manifest[key]; has {
				if _, isStr := v.(string); !isStr {
					wellFormed = false
				}
			}
		}
		if wellFormed && resolvePath(filepath.Join(filepath.Dir(path), repo)) == root {
			return manifest, filepath.Dir(path), true
		}
	}
	return nil, "", false
}

// Payload relative path resolved from its manifest's root, refused when it escapes that root
func payloadPath(payloadRoot, relative, what string) (string, error) {
	path := resolvePath(filepath.Join(payloadRoot, relative))
	if !isRelativeTo(path, payloadRoot) {
		return "", fmt.Errorf("payload %s escapes its root", what)
	}
	return path, nil
}

// Store of a tree from the runtime override, a payload manifest, an install stamp, or the default root
func EnvironmentStoreRoot(ctx context.Context, projectRoot string, honorRuntimeOverride bool) (string, error) {
	if honorRuntimeOverride {
		if override := os.Getenv(EnvRuntimeDir); override != "" {
			return resolvePath(override), nil
		}
	}
	root := resolvePath(projectRoot)
	if manifest, payloadRoot, ok := payloadManifest(root); ok {
		if store, _ := manifest["store"].(string); store != "" {
			return payloadPath(payloadRoot, store, "store")
		}
	}
	for directory := root; ; directory = filepath.Dir(directory) {
		stamp := InstallStampPath(directory)
		if st, err := os.Stat(stamp); err == nil && st.Mode().IsRegular() {
			data, err := os.ReadFile(stamp)
			if err != nil {
				return unstampedStore(ctx, root), nil
			}
			var doc any
			if json.Unmarshal(trimBOM(data), &doc) != nil {
				return unstampedStore(ctx, root), nil
			}
			if m, ok := doc.(map[string]any); ok {
				if value, has := m["runtimeDir"]; has && pyrepr.Truthy(value) {
					s, isStr := value.(string)
					if !isStr {
						return "", fmt.Errorf("install stamp %s runtimeDir is not a path: %v", stamp, value)
					}
					return resolvePath(s), nil
				}
			}
			return unstampedStore(ctx, root), nil
		}
		if filepath.Dir(directory) == directory {
			break
		}
	}
	return filepath.Join(home.Root(), "tools"), nil
}

// Store of a stamp that names none, the owning root's so a borrowing home reuses its tools
func unstampedStore(ctx context.Context, projectRoot string) string {
	owner := OwningHomeRoot(ctx, projectRoot)
	if owner == "" {
		owner = home.Root()
	}
	return filepath.Join(owner, "tools")
}

// Strips a UTF-8 byte order mark the way utf-8-sig decoding does
func trimBOM(data []byte) []byte {
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		return data[3:]
	}
	return data
}

// Platform name the way Python sys.platform spells it
func sysPlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "android":
		return "android"
	}
	return runtime.GOOS
}

// Scripts on Windows and bin elsewhere, returned whether or not the venv exists
func VenvBinDir(venv string, windows bool) string {
	if windows {
		return filepath.Join(venv, "Scripts")
	}
	return filepath.Join(venv, "bin")
}

// Interpreter inside a venv laid out for this platform, which may not exist
func VenvPython(venv string) string {
	bin := VenvBinDir(venv, runtime.GOOS == "windows")
	if filepath.Base(bin) == "Scripts" {
		return filepath.Join(bin, "python.exe")
	}
	return filepath.Join(bin, "python")
}
