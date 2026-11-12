package selfupdate

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
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const (
	frontendProductMarker = ".icebath-product"
	frontendProductOwner  = "hermes-frontend-product-v1\n"
)

var (
	frontendDeveloperOutputs = []string{"ui-tui/dist", "cmd/icebath/web_dist", "apps/desktop/dist", "apps/desktop/dist-webapp",
		"apps/desktop/build/native-deps"}
	frontendNativeTargetOutput = regexp.MustCompile(`^apps[\\/]desktop[\\/]build[\\/]native-deps-(darwin|linux|win32)-(x64|arm64)$`)
	frontendHeldRetryDelays    = []time.Duration{100, 200, 400, 800, 1600}
)

// Real path of file, resolving the deepest existing ancestor when the file does not exist yet
func frontendCanonical(file string) string {
	if real, err := filepath.EvalSymlinks(file); err == nil {
		return real
	}
	parent := filepath.Dir(file)
	if parent == file {
		return file
	}
	return filepath.Join(frontendCanonical(parent), filepath.Base(file))
}

func frontendContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && (rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." && !filepath.IsAbs(rel)))
}

func frontendDeveloperOutput(source, out string) bool {
	if source == "" {
		return false
	}
	rel, err := filepath.Rel(source, out)
	if err != nil {
		return false
	}
	for _, name := range frontendDeveloperOutputs {
		if rel == filepath.FromSlash(name) {
			return true
		}
	}
	return frontendNativeTargetOutput.MatchString(rel)
}

// Refuses an existing output that is a file, a symlink or a directory another tool owns
func frontendRequireOwnedOutput(out, source string) error {
	info, err := os.Lstat(out)
	if err != nil {
		return nil
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("Output must be a directory, not a file or symlink: %s", out)
	}
	if frontendDeveloperOutput(source, out) {
		return nil
	}
	marker := filepath.Join(out, frontendProductMarker)
	raw, err := os.ReadFile(marker)
	if mi, lerr := os.Lstat(marker); lerr != nil || !mi.Mode().IsRegular() || err != nil || string(raw) != frontendProductOwner {
		return fmt.Errorf("Output already exists and is not builder-owned; choose a fresh output directory: %s", out)
	}
	return nil
}

// Validated source and output of a product build, the output outside every input and owned by the builder
func frontendProductOutput(source, out string, inputs []string) (string, string, error) {
	if source == "" || out == "" {
		return "", "", errors.New("source and output paths are required")
	}
	absSource, _ := filepath.Abs(source)
	src, err := filepath.EvalSymlinks(absSource)
	if err != nil {
		return "", "", err
	}
	absOut, _ := filepath.Abs(out)
	dest := frontendCanonical(absOut)
	overlap := frontendContains(dest, src)
	for _, input := range inputs {
		protected := frontendCanonical(filepath.Join(src, filepath.FromSlash(input)))
		if frontendContains(dest, protected) || frontendContains(protected, dest) {
			overlap = true
		}
	}
	if overlap {
		return "", "", fmt.Errorf("Output must not overlap build inputs: %s", out)
	}
	generated := frontendDeveloperOutput(src, dest)
	for _, name := range []string{".build", "apps/desktop/build/products"} {
		root := filepath.Join(src, filepath.FromSlash(name))
		if dest != root && frontendContains(root, dest) {
			generated = true
		}
	}
	if frontendContains(src, dest) && !generated {
		return "", "", fmt.Errorf("Output must be outside source or in a supported generated destination: %s", out)
	}
	if err := frontendRequireOwnedOutput(out, src); err != nil {
		return "", "", err
	}
	return src, dest, nil
}

// Retries a filesystem operation another process holds, backing off up to 1.6 seconds
func frontendRetryHeld(op func() error) error {
	for _, delay := range append(frontendHeldRetryDelays, -1) {
		err := op()
		if err == nil {
			return nil
		}
		if delay < 0 || !frontendHeld(err) {
			return err
		}
		time.Sleep(delay * time.Millisecond)
	}
	return nil
}

// Publishes a staged product over out, keeping the previous product until the swap succeeds
func frontendPublishDirectory(staged, out, source string) error {
	if err := frontendRequireOwnedOutput(out, source); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(staged, frontendProductMarker), []byte(frontendProductOwner), 0o666); err != nil {
		return err
	}
	if _, err := os.Lstat(out); err != nil {
		return frontendRetryHeld(func() error { return os.Rename(staged, out) })
	}
	backupRoot, err := os.MkdirTemp(filepath.Dir(out), "."+filepath.Base(out)+"-previous-")
	if err != nil {
		return err
	}
	backup := filepath.Join(backupRoot, "product")
	if err := frontendRetryHeld(func() error { return os.Rename(out, backup) }); err != nil {
		os.RemoveAll(backupRoot)
		return err
	}
	if err := frontendRetryHeld(func() error { return os.Rename(staged, out) }); err != nil {
		if rerr := frontendRetryHeld(func() error { return os.Rename(backup, out) }); rerr != nil {
			return fmt.Errorf("%w; rollback failed (%v); previous product kept at %s", err, rerr, backup)
		}
		os.RemoveAll(backupRoot)
		return err
	}
	return os.RemoveAll(backupRoot)
}

// Compiles into a scratch product beside out and publishes it, the scratch removed either way
func frontendWithProduct(out, source string, compile func(product, scratch string) error) error {
	if err := frontendRequireOwnedOutput(out, source); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o777); err != nil {
		return err
	}
	scratch, err := os.MkdirTemp(filepath.Dir(out), "."+filepath.Base(out)+"-build-")
	if err != nil {
		return err
	}
	product := filepath.Join(scratch, "product")
	err = os.Mkdir(product, 0o777)
	if err == nil {
		err = compile(product, scratch)
	}
	if err == nil {
		err = frontendPublishDirectory(product, out, source)
	}
	if rmErr := frontendRmTree(scratch); rmErr != nil {
		return rmErr
	}
	return err
}

// Removes dir, retrying a few times when a file manager drops metadata into it mid removal
func frontendRmTree(dir string) error {
	for attempt := 1; ; attempt++ {
		err := os.RemoveAll(dir)
		if err == nil || !frontendNotEmpty(err) || attempt >= 5 {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Removes dir as Node rmSync with maxRetries 3 does, retrying busy, full and permission errors with a linear backoff
func frontendRemoveRetrying(dir string) error {
	for attempt := 1; ; attempt++ {
		err := os.RemoveAll(dir)
		if err == nil || attempt > 3 || !(frontendHeld(err) || frontendNotEmpty(err) || frontendTooManyFiles(err)) {
			return err
		}
		time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
	}
}

// Temporary directory Node reports for env, TMPDIR TMP TEMP on POSIX and TEMP TMP on Windows
func frontendNodeTmpdir(env map[string]string) string {
	names := []string{"TMPDIR", "TMP", "TEMP"}
	fallback := "/tmp"
	if runtime.GOOS == "windows" {
		names, fallback = []string{"TEMP", "TMP"}, filepath.Join(env["SystemRoot"], "temp")
		if env["SystemRoot"] == "" {
			fallback = filepath.Join(env["windir"], "temp")
		}
	}
	for _, name := range names {
		if v := env[name]; v != "" {
			if len(v) > 1 && strings.HasSuffix(v, string(filepath.Separator)) && !(runtime.GOOS == "windows" && strings.HasSuffix(v, ":\\")) {
				v = v[:len(v)-1]
			}
			return v
		}
	}
	return fallback
}

// Command line script of a prepared workspace dependency, from the workspace node_modules or the root one
func frontendWorkspaceBin(source, workspace, name, bin string) (string, error) {
	for _, root := range []string{filepath.Join(source, workspace), source} {
		pkgDir := filepath.Join(root, "node_modules", name)
		raw, err := os.ReadFile(filepath.Join(pkgDir, "package.json"))
		if err != nil {
			continue
		}
		var manifest struct {
			Bin json.RawMessage `json:"bin"`
		}
		if err := json.Unmarshal(raw, &manifest); err != nil {
			return "", err
		}
		var single string
		if json.Unmarshal(manifest.Bin, &single) == nil && single != "" {
			return filepath.Join(pkgDir, filepath.FromSlash(single)), nil
		}
		var named map[string]string
		if json.Unmarshal(manifest.Bin, &named) == nil && named[bin] != "" {
			return filepath.Join(pkgDir, filepath.FromSlash(named[bin])), nil
		}
		return "", fmt.Errorf("%s has no %s command", name, bin)
	}
	return "", fmt.Errorf("Missing prepared %s dependency in %s or %s/node_modules", name, workspace, source)
}

// Node binary and npm CLI script found on the build PATH, the script npm_execpath names first
func frontendNPMCommand(env map[string]string) (string, string, error) {
	path := env["PATH"]
	if path == "" {
		path = env["Path"]
	}
	node, err := lookPathIn("node", path)
	if err != nil {
		return "", "", errors.New("node is required on PATH")
	}
	names := []string{"npm"}
	if runtime.GOOS == "windows" {
		names = []string{"npm.cmd", "npm"}
	}
	candidates := []string{env["npm_execpath"]}
	for _, dir := range filepath.SplitList(path) {
		for _, name := range names {
			bin := filepath.Join(dir, name)
			if _, err := os.Stat(bin); err != nil {
				continue
			}
			real, err := filepath.EvalSymlinks(bin)
			if err != nil {
				continue
			}
			prefix := filepath.Dir(real)
			lib := filepath.Join(prefix, "..", "lib")
			candidates = append(candidates, filepath.Join(prefix, "node_modules", "npm", "bin", "npm-cli.js"),
				filepath.Join(prefix, "..", "lib", "node_modules", "npm", "bin", "npm-cli.js"))
			if entries, err := os.ReadDir(lib); err == nil {
				var npms []string
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), "npm") {
						npms = append(npms, e.Name())
					}
				}
				sort.Strings(npms)
				for _, n := range npms {
					candidates = append(candidates, filepath.Join(lib, n, "bin", "npm-cli.js"))
				}
			}
			candidates = append(candidates, real)
		}
	}
	for _, c := range candidates {
		if strings.HasSuffix(c, ".js") {
			if _, err := os.Stat(c); err == nil {
				return node, c, nil
			}
		}
	}
	return "", "", errors.New("npm CLI is required on PATH")
}

func lookPathIn(name, path string) (string, error) {
	for _, dir := range filepath.SplitList(path) {
		for _, candidate := range []string{name, name + ".exe"} {
			p := filepath.Join(dir, candidate)
			if info, err := os.Stat(p); err == nil && !info.IsDir() && (runtime.GOOS == "windows" || info.Mode()&0o111 != 0) {
				return p, nil
			}
		}
	}
	return "", exec.ErrNotFound
}

// Version of the npm CLI from its package manifest, asking the CLI when the manifest is unreadable
func frontendNPMVersion(node, cli, source string, env map[string]string) (string, error) {
	if real, err := filepath.EvalSymlinks(cli); err == nil {
		if raw, err := os.ReadFile(filepath.Join(filepath.Dir(real), "..", "package.json")); err == nil {
			var m struct {
				Version string `json:"version"`
			}
			if json.Unmarshal(raw, &m) == nil && m.Version != "" {
				return m.Version, nil
			}
		}
	}
	cmd := exec.Command(node, cli, "--version")
	cmd.Dir, cmd.Env = source, frontendEnvList(env)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func frontendEnvList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// True when the completed install receipt matches key and every locked package manifest is on disk
func frontendCompletedInstallMatches(source, receipt, hiddenLock, key string, nativeKey *string) (bool, error) {
	if _, err := os.Stat(receipt); err != nil {
		return false, nil
	}
	if _, err := os.Stat(hiddenLock); err != nil {
		return false, nil
	}
	installed, err := os.ReadFile(hiddenLock)
	if err != nil {
		return false, err
	}
	expected := key + "\n" + sha256Hex(installed) + "\n"
	if nativeKey != nil {
		native, err := os.ReadFile(receipt + ".native-toolchain")
		if err != nil {
			if _, serr := os.Stat(receipt + ".native-toolchain"); serr != nil {
				return false, nil
			}
			return false, err
		}
		if string(native) != expected+*nativeKey+"\n" {
			return false, nil
		}
	}
	saved, err := os.ReadFile(receipt)
	if err != nil {
		return false, err
	}
	if string(saved) != expected {
		return false, nil
	}
	var lock map[string]json.RawMessage
	if err := json.Unmarshal(installed, &lock); err != nil {
		return false, err
	}
	var packages map[string]json.RawMessage
	if raw, ok := lock["packages"]; !ok || json.Unmarshal(raw, &packages) != nil || packages == nil {
		return false, errors.New("TypeError: Cannot convert undefined or null to object")
	}
	for path := range packages {
		if _, err := os.Stat(filepath.Join(source, filepath.FromSlash(path), "package.json")); err != nil {
			return false, nil
		}
	}
	return true, nil
}

// Locked workspace install of the selected workspaces, reusing a completed identical install when asked
func frontendPrepareNodeDependencies(ctx context.Context, st *frontendStep, source string, workspaces []string, env map[string]string,
	reuse, install, electron bool, nativeToolchain *string) error {
	source, _ = filepath.Abs(source)
	if len(workspaces) == 0 {
		return errors.New("Select at least one workspace; implicit all-workspace installation is not allowed")
	}
	manifestRaw, err := os.ReadFile(filepath.Join(source, "package.json"))
	if err != nil {
		return err
	}
	lockRaw, err := os.ReadFile(filepath.Join(source, "package-lock.json"))
	if err != nil {
		return err
	}
	var manifest struct {
		Engines map[string]string `json:"engines"`
	}
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		return err
	}
	lockV, err := pyjson.Loads(string(lockRaw))
	if err != nil {
		return err
	}
	lock, _ := lockV.(*pyjson.Object)
	packagesV, _ := lock.Get("packages")
	packages, _ := packagesV.(*pyjson.Object)
	if packages == nil {
		packages = pyjson.NewObject()
	}
	field := func(path, key string) any {
		entry, _ := packages.Vals[path].(*pyjson.Object)
		if entry == nil {
			return nil
		}
		v, _ := entry.Get(key)
		return v
	}
	var linked []string
	for _, path := range packages.Keys {
		if pyTruthyJS(field(path, "link")) {
			if r, ok := field(path, "resolved").(string); ok {
				linked = append(linked, r)
			}
		}
	}
	seen := map[string]bool{}
	var selected []string
	for _, workspace := range workspaces {
		found := ""
		for _, path := range linked {
			if path == workspace || field(path, "name") == workspace {
				found = path
				break
			}
		}
		if _, err := os.Stat(filepath.Join(source, filepath.FromSlash(found), "package.json")); found == "" || err != nil {
			return fmt.Errorf("Unknown or missing locked workspace: %s", workspace)
		}
		if !seen[found] {
			seen[found] = true
			selected = append(selected, found)
		}
	}
	sort.Slice(selected, func(i, j int) bool { return jsLess(selected[i], selected[j]) })
	prepareElectron := false
	if electron && seen["apps/desktop"] {
		if raw, err := os.ReadFile(filepath.Join(source, "apps", "desktop", "package.json")); err == nil {
			var desktop struct {
				DevDependencies map[string]string `json:"devDependencies"`
			}
			prepareElectron = json.Unmarshal(raw, &desktop) == nil && desktop.DevDependencies["electron"] != ""
		}
	}
	node, npm, err := frontendNPMCommand(env)
	if err != nil {
		return err
	}
	host, err := frontendNodeHost(node)
	if err != nil {
		return err
	}
	npmVersion, err := frontendNPMVersion(node, npm, source, env)
	if err != nil {
		return err
	}
	for _, pair := range [][2]string{{"node", host.version}, {"npm", npmVersion}} {
		rng := manifest.Engines[pair[0]]
		if rng == "" {
			continue
		}
		constraint, cerr := semver.NewConstraint(rng)
		version, verr := semver.NewVersion(pair[1])
		if cerr != nil || verr != nil || !constraint.Check(version) {
			return fmt.Errorf("%s %s violates %s", pair[0], pair[1], rng)
		}
	}
	args := []any{"ci", "--no-audit", "--no-fund", "--engine-strict", "--include=dev", "--include=optional", "--include-workspace-root=true"}
	for _, w := range selected {
		args = append(args, "--workspace", w)
	}
	npmConfigRE := regexp.MustCompile(`(?i)^npm_config_`)
	excludedRE := regexp.MustCompile(`(?i)^npm_config_(cache|offline|prefer_offline)$`)
	var config [][2]string
	for k, v := range env {
		if npmConfigRE.MatchString(k) && !excludedRE.MatchString(k) {
			config = append(config, [2]string{strings.ToLower(k), v})
		}
	}
	sort.Slice(config, func(i, j int) bool { return jsLess(config[i][0]+","+config[i][1], config[j][0]+","+config[j][1]) })
	configJS := []any{}
	for _, c := range config {
		configJS = append(configJS, []any{c[0], c[1]})
	}
	h := sha256.New()
	h.Write([]byte(jsStringify(pyjson.ObjectOf("node", host.version, "npm", npmVersion, "platform", host.platform,
		"arch", host.arch, "args", args, "config", configJS))))
	files := []string{"package-lock.json", ".npmrc"}
	for _, path := range packages.Keys {
		if !strings.Contains("/"+path+"/", "/node_modules/") {
			files = append(files, filepath.Join(filepath.FromSlash(path), "package.json"))
		}
	}
	sort.Slice(files, func(i, j int) bool { return jsLess(files[i], files[j]) })
	for _, file := range files {
		h.Write([]byte(file + "\x00"))
		if raw, err := os.ReadFile(filepath.Join(source, file)); err == nil {
			h.Write(raw)
		} else {
			h.Write([]byte("<missing>"))
		}
		h.Write([]byte{0})
	}
	key := hex.EncodeToString(h.Sum(nil))
	receipt := filepath.Join(source, "node_modules", ".icebath-node-deps")
	nativeReceipt := receipt + ".native-toolchain"
	hiddenLock := filepath.Join(source, "node_modules", ".package-lock.json")
	matches := false
	if reuse {
		if matches, err = frontendCompletedInstallMatches(source, receipt, hiddenLock, key, nativeToolchain); err != nil {
			return err
		}
	}
	if matches {
		if prepareElectron {
			if _, err := frontendPrepareElectronRuntime(ctx, st, source, install, env, node); err != nil {
				return err
			}
		}
		st.say("node-deps: reusing completed install (%s)", strings.Join(selected, ", "))
		return nil
	}
	if !install {
		return errors.New("Workspace dependencies are stale or missing and lazy installs are disabled; run an explicit build/update")
	}
	os.Remove(receipt)
	os.Remove(nativeReceipt)
	st.say("node-deps: installing workspace dependencies with npm ci (%s)...", strings.Join(selected, ", "))
	argv := []string{node, npm}
	for _, a := range args {
		argv = append(argv, a.(string))
	}
	if err := frontendRunNPMCi(ctx, st, source, argv, env); err != nil {
		return err
	}
	if prepareElectron {
		if _, err := frontendPrepareElectronRuntime(ctx, st, source, install, env, node); err != nil {
			return err
		}
	}
	if reuse {
		installed, err := os.ReadFile(hiddenLock)
		if err != nil {
			return err
		}
		completed := key + "\n" + sha256Hex(installed) + "\n"
		if err := os.WriteFile(receipt, []byte(completed), 0o666); err != nil {
			return err
		}
		if nativeToolchain != nil {
			return os.WriteFile(nativeReceipt, []byte(completed+*nativeToolchain+"\n"), 0o666)
		}
	}
	return nil
}

// JavaScript truth value of a decoded JSON value
func pyTruthyJS(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int64:
		return x != 0
	case float64:
		return x != 0
	}
	return true
}

// Runs npm ci under custody, retrying once from an empty node_modules when its logs show ENOTEMPTY
func frontendRunNPMCi(ctx context.Context, st *frontendStep, source string, argv []string, env map[string]string) error {
	logsDir, err := os.MkdirTemp(frontendNodeTmpdir(env), "hermes-npm-logs-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(logsDir)
	command := append(append([]string(nil), argv...), "--progress=true", "--logs-dir="+logsDir)
	run := func() error { return st.run(ctx, source, command, source, env) }
	err = run()
	if err == nil {
		return nil
	}
	entries, rerr := os.ReadDir(logsDir)
	if rerr != nil {
		return rerr
	}
	logged := false
	for _, e := range entries {
		raw, rerr := os.ReadFile(filepath.Join(logsDir, e.Name()))
		if rerr != nil {
			return rerr
		}
		logged = logged || strings.Contains(string(raw), "ENOTEMPTY")
	}
	if !logged {
		return err
	}
	st.say("node-deps: npm ci hit ENOTEMPTY; removing node_modules and retrying once...")
	if err := frontendRemoveRetrying(filepath.Join(source, "node_modules")); err != nil {
		return err
	}
	return run()
}

// Electron binary of the locked desktop dependency, downloading it through its installer when missing
func frontendPrepareElectronRuntime(ctx context.Context, st *frontendStep, source string, install bool, env map[string]string, node string) (string, error) {
	var manifestPath string
	for _, p := range []string{filepath.Join(source, "apps", "desktop", "node_modules", "electron", "package.json"),
		filepath.Join(source, "node_modules", "electron", "package.json")} {
		if _, err := os.Stat(p); err == nil {
			manifestPath = p
			break
		}
	}
	if manifestPath == "" {
		return "", errors.New("Missing prepared Electron dependency; install the locked desktop workspace first")
	}
	packageRoot := filepath.Dir(manifestPath)
	var electron struct {
		Version      string            `json:"version"`
		Dependencies map[string]string `json:"dependencies"`
	}
	var desktop struct {
		DevDependencies map[string]string `json:"devDependencies"`
	}
	var lock struct {
		Packages map[string]struct {
			Link     bool   `json:"link"`
			Resolved string `json:"resolved"`
			Version  string `json:"version"`
		} `json:"packages"`
	}
	for path, dest := range map[string]any{manifestPath: &electron, filepath.Join(source, "apps", "desktop", "package.json"): &desktop,
		filepath.Join(source, "package-lock.json"): &lock} {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		if err := json.Unmarshal(raw, dest); err != nil {
			return "", err
		}
	}
	rel, _ := filepath.Rel(source, packageRoot)
	entry := lock.Packages[filepath.ToSlash(rel)]
	locked := entry.Version
	if entry.Link {
		locked = lock.Packages[entry.Resolved].Version
	}
	if electron.Version != desktop.DevDependencies["electron"] || electron.Version != locked {
		return "", errors.New("Installed Electron is not the desktop lock-pinned version")
	}
	host, err := frontendNodeHost(node)
	if err != nil {
		return "", err
	}
	binaryName := map[string]string{"darwin": "Electron.app/Contents/MacOS/Electron", "win32": "electron.exe"}[host.platform]
	if binaryName == "" {
		binaryName = "electron"
	}
	executable := filepath.Join(packageRoot, "dist", filepath.FromSlash(binaryName))
	prepared := func() bool {
		pathTxt, err1 := os.ReadFile(filepath.Join(packageRoot, "path.txt"))
		version, err2 := os.ReadFile(filepath.Join(packageRoot, "dist", "version"))
		info, err3 := os.Stat(executable)
		return err1 == nil && err2 == nil && err3 == nil && strings.TrimSpace(string(pathTxt)) == binaryName &&
			strings.TrimPrefix(strings.TrimSpace(string(version)), "v") == electron.Version && info.Mode().IsRegular()
	}
	if prepared() {
		return executable, nil
	}
	if !install {
		return "", errors.New("Electron binary is missing or stale and lazy installs are disabled; run an explicit desktop build/update")
	}
	excluded := map[string]bool{"electron_skip_binary_download": true, "electron_override_dist_path": true, "electron_install_arch": true,
		"electron_install_platform": true, "npm_config_arch": true, "npm_config_platform": true}
	installerEnv := map[string]string{}
	for k, v := range env {
		if !excluded[strings.ToLower(k)] {
			installerEnv[k] = v
		}
	}
	installerEnv["ELECTRON_INSTALL_PLATFORM"], installerEnv["ELECTRON_INSTALL_ARCH"] = host.platform, host.arch
	installerEnv["npm_config_platform"], installerEnv["npm_config_arch"] = host.platform, host.arch
	if host.platform == "win32" && env["ICEBATH_VC_RUNTIME"] != "" && electron.Dependencies["@electron-internal/extract-zip"] != "" {
		entry, err := frontendRequireResolve(packageRoot, "@electron-internal/extract-zip")
		if err != nil {
			return "", err
		}
		binding := filepath.Dir(entry)
		raw, err := os.ReadFile(env["ICEBATH_VC_RUNTIME"])
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(binding, "vcruntime140.dll"), raw, 0o666); err != nil {
			return "", err
		}
	}
	st.say("electron-runtime: preparing locked Electron %s (%s-%s)...", electron.Version, host.platform, host.arch)
	if err := st.run(ctx, source, []string{node, filepath.Join(packageRoot, "install.js")}, source, installerEnv); err != nil {
		return "", err
	}
	if !prepared() {
		return "", errors.New("Electron installer did not produce the prepared host runtime")
	}
	return executable, nil
}

// Typechecks and bundles the web dashboard into out with the given icons, then records its receipt
func frontendBuildWeb(ctx context.Context, st *frontendStep, source, icons, out string, env map[string]string) error {
	src, dest, err := frontendProductOutput(source, out, []string{"web", "apps/shared", "node_modules"})
	if err != nil {
		return err
	}
	if icons == "" {
		return errors.New("Prepared icons are required (--icons)")
	}
	absIcons, _ := filepath.Abs(icons)
	publicIcons := filepath.Join(absIcons, "web", "public")
	favicon := filepath.Join(publicIcons, "favicon.ico")
	if info, err := os.Stat(favicon); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("Missing prepared icon: %s", favicon)
	}
	if _, _, err := frontendProductOutput(icons, dest, []string{"web/public"}); err != nil {
		return err
	}
	root := filepath.Join(src, "web")
	inputs, err := frontendBuildInputs(src, "web", map[string]string{"icons": publicIcons}, env)
	if err != nil {
		return err
	}
	node, err := lookPathIn("node", env["PATH"])
	if err != nil {
		return errors.New("node is required on PATH")
	}
	host, err := frontendNodeHost(node)
	if err != nil {
		return err
	}
	tsc, err := frontendWorkspaceBin(src, "web", "typescript", "tsc")
	if err != nil {
		return err
	}
	vite, err := frontendWorkspaceBin(src, "web", "vite", "vite")
	if err != nil {
		return err
	}
	vitePkg := filepath.Dir(filepath.Dir(vite))
	return frontendWithProduct(dest, src, func(product, scratch string) error {
		if err := frontendTypecheck(ctx, node, tsc, root, scratch, env); err != nil {
			return err
		}
		publicDir := filepath.Join(scratch, "public")
		if err := os.Mkdir(publicDir, 0o777); err != nil {
			return err
		}
		if info, err := os.Stat(filepath.Join(root, "public")); err == nil && info.IsDir() {
			if err := os.CopyFS(publicDir, os.DirFS(filepath.Join(root, "public"))); err != nil {
				return err
			}
		}
		if err := copyTreeOver(publicIcons, publicDir); err != nil {
			return err
		}
		quote := func(s string) string { raw, _ := json.Marshal(s); return string(raw) }
		viteMain := "file://" + filepath.ToSlash(filepath.Join(vitePkg, "dist", "node", "index.js"))
		config := "import { loadConfigFromFile, mergeConfig } from " + quote(viteMain) + "\n" +
			"export default async (env) => {\n" +
			"  const loaded = await loadConfigFromFile(env, " + quote(filepath.Join(root, "vite.config.ts")) + ", " + quote(root) +
			", undefined, undefined, 'runner')\n" +
			"  return mergeConfig(loaded?.config ?? {}, { root: " + quote(root) + ", publicDir: " + quote(publicDir) +
			", cacheDir: " + quote(filepath.Join(scratch, "vite-cache")) + ", build: { outDir: " + quote(product) +
			", emptyOutDir: true } })\n" +
			"}\n"
		wrapper := filepath.Join(scratch, "vite.config.mjs")
		if err := os.WriteFile(wrapper, []byte(config), 0o666); err != nil {
			return err
		}
		if err := st.run(ctx, src, []string{node, vite, "build", "--config", wrapper, "--configLoader", "native"}, root, env); err != nil {
			return err
		}

		if _, err := os.Stat(filepath.Join(product, "index.html")); err != nil {
			return errors.New("Web build did not produce index.html")
		}
		return frontendRecordProduct(host, src, "web", product, inputs, env, nil, false)
	})
}

// Type checks the project graph of root/tsconfig.json as a forced build, build state in scratch and nothing emitted into the source
func frontendTypecheck(ctx context.Context, node, tsc, root, scratch string, env map[string]string) error {
	type tsProject struct {
		CompilerOptions map[string]any `json:"compilerOptions"`
		References      []struct {
			Path string `json:"path"`
		} `json:"references"`
		Files   *[]string `json:"files"`
		Include *[]string `json:"include"`
	}
	run := func(args ...string) (string, int, error) {
		cmd := exec.CommandContext(ctx, node, append([]string{tsc}, args...)...)
		cmd.Dir, cmd.Env = root, frontendEnvList(env)
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return string(out), exit.ExitCode(), nil
		}
		return string(out), 0, err
	}
	configs := map[string]tsProject{}
	state := map[string]int{}
	var order, stack []string
	var visit func(path string) error
	visit = func(path string) error {
		switch state[path] {
		case 1:
			cycle := append(append([]string(nil), stack[slices.Index(stack, path):]...), path)
			return fmt.Errorf("TypeScript build failed (4)\nerror TS6202: Project references may not form a circular graph. Cycle detected: %s",
				strings.Join(cycle, "\n"))
		case 2:
			return nil
		}
		state[path], stack = 1, append(stack, path)
		out, code, err := run("--showConfig", "-p", path)
		if err != nil {
			return err
		}
		var project tsProject
		if code != 0 || json.Unmarshal([]byte(out), &project) != nil {
			return fmt.Errorf("TypeScript build failed (3)\n%s", out)
		}
		for _, ref := range project.References {
			target := filepath.FromSlash(ref.Path)
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(path), target)
			}
			if info, err := os.Stat(target); err == nil && info.IsDir() {
				target = filepath.Join(target, "tsconfig.json")
			}
			if err := visit(target); err != nil {
				return err
			}
		}
		configs[path], state[path], stack = project, 2, stack[:len(stack)-1]
		order = append(order, path)
		return nil
	}
	if err := visit(filepath.Join(root, "tsconfig.json")); err != nil {
		return err
	}
	status, report := 0, ""
	for i, path := range order {
		project := configs[path]
		if project.Files != nil && len(*project.Files) == 0 && project.Include == nil && len(project.References) > 0 {
			continue
		}
		args := []string{"-p", path, "--incremental", "--tsBuildInfoFile", filepath.Join(scratch, fmt.Sprintf("ts-%d.tsbuildinfo", i)),
			"--listEmittedFiles", "--pretty"}
		emitDir, outDir := filepath.Join(scratch, fmt.Sprintf("emit-%d", i)), filepath.Dir(path)
		if dir, ok := project.CompilerOptions["outDir"].(string); ok {
			outDir = filepath.Join(filepath.Dir(path), filepath.FromSlash(dir))
		}
		if project.CompilerOptions["noEmit"] != true {
			args = append(args, "--outDir", emitDir)
			if project.CompilerOptions["declaration"] == true || project.CompilerOptions["composite"] == true {
				args = append(args, "--declarationDir", emitDir)
			}
		}
		out, code, err := run(args...)
		if err != nil {
			return err
		}
		var diagnostics []string
		for _, line := range strings.Split(out, "\n") {
			emitted, isEmit := strings.CutPrefix(strings.TrimRight(line, "\r"), "TSFILE: ")
			if !isEmit {
				diagnostics = append(diagnostics, line)
				continue
			}
			if !strings.HasSuffix(emitted, ".tsbuildinfo") {
				if rel, err := filepath.Rel(emitDir, emitted); err == nil && !strings.HasPrefix(rel, "..") {
					emitted = filepath.Join(outDir, rel)
				}
				return fmt.Errorf("Unexpected TypeScript emit: %s", emitted)
			}
		}
		if code != 0 {
			status = max(status, code)
			report += strings.Join(diagnostics, "\n")
		}
	}
	if status != 0 {
		return fmt.Errorf("TypeScript build failed (%d)\n%s", status, report)
	}
	return nil
}

// Copies every file of src over dst, replacing files that exist in both
func copyTreeOver(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o777)
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o666)
	})
}

// Entry file Node require resolves for a bare package name from a module directory, walking node_modules upward
func frontendRequireResolve(fromDir, name string) (string, error) {
	for dir := fromDir; ; dir = filepath.Dir(dir) {
		if filepath.Base(dir) != "node_modules" {
			pkg := filepath.Join(dir, "node_modules", filepath.FromSlash(name))
			if info, err := os.Stat(pkg); err == nil && info.IsDir() {
				if entry, ok, err := frontendPackageEntry(pkg); err != nil || ok {
					return entry, err
				}
			}
		}
		if filepath.Dir(dir) == dir {
			return "", fmt.Errorf("Cannot find module '%s'", name)
		}
	}
}

// Entry of a package directory for require, the exports root target, then main, then index
func frontendPackageEntry(pkg string) (string, bool, error) {
	if raw, err := os.ReadFile(filepath.Join(pkg, "package.json")); err == nil {
		v, err := pyjson.Loads(string(raw))
		if err != nil {
			return "", false, err
		}
		manifest, _ := v.(*pyjson.Object)
		if manifest != nil {
			if exports, ok := manifest.Get("exports"); ok && exports != nil {
				root := exports
				if obj, isObj := exports.(*pyjson.Object); isObj && len(obj.Keys) > 0 && strings.HasPrefix(obj.Keys[0], ".") {
					root, _ = obj.Get(".")
				}
				target, found := frontendExportTarget(root)
				if !found {
					return "", false, fmt.Errorf("No \"exports\" main defined in %s", filepath.Join(pkg, "package.json"))
				}
				return filepath.Join(pkg, filepath.FromSlash(target)), true, nil
			}
			if main, _ := manifest.Get("main"); main != nil {
				if m, ok := main.(string); ok && m != "" {
					if entry, ok := frontendProbeFile(filepath.Join(pkg, filepath.FromSlash(m)), true); ok {
						return entry, true, nil
					}
				}
			}
		}
	}
	entry, ok := frontendProbeFile(filepath.Join(pkg, "index"), false)
	return entry, ok, nil
}

// Target of an exports value under the require, node, node-addons and default conditions in key order
func frontendExportTarget(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, strings.HasPrefix(x, "./")
	case []any:
		for _, item := range x {
			if target, ok := frontendExportTarget(item); ok {
				return target, true
			}
		}
	case *pyjson.Object:
		for _, key := range x.Keys {
			switch key {
			case "require", "node", "node-addons", "default":
				if target, ok := frontendExportTarget(x.Vals[key]); ok {
					return target, true
				}
			}
		}
	}
	return "", false
}

// File require loads for a main path, exact then with .js .json .node then its index, or for an index its extensions only
func frontendProbeFile(path string, main bool) (string, bool) {
	candidates := []string{path + ".js", path + ".json", path + ".node"}
	if main {
		candidates = append([]string{path}, candidates...)
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate, true
		}
	}
	if main {
		return frontendProbeFile(filepath.Join(path, "index"), false)
	}
	return "", false
}
