package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Vite build manifest of a webapp generation, the files that generation emitted
const frontendWebappManifest = "renderer-manifest.json"

// Destination of the warnings a webapp rebuild prints about chunks it could not keep, a test seam
var frontendWebappWarn io.Writer = os.Stderr

// Copy of one kept chunk, the seam the keep cases fault exactly where the node cases fault copyFileSync
var frontendWebappCopy = frontendCopyExclusive

// Copies a regular file to a name that must not exist yet
func frontendCopyExclusive(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

// Node style error code of a file system error, the error text when it carries none
func frontendErrCode(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		for code, known := range map[string]syscall.Errno{"EEXIST": syscall.EEXIST, "ENOTDIR": syscall.ENOTDIR, "ENOSPC": syscall.ENOSPC,
			"ENOENT": syscall.ENOENT, "EACCES": syscall.EACCES, "EPERM": syscall.EPERM, "EISDIR": syscall.EISDIR} {
			if errno == known {
				return code
			}
		}
	}
	switch {
	case errors.Is(err, fs.ErrExist):
		return "EEXIST"
	case errors.Is(err, fs.ErrNotExist):
		return "ENOENT"
	}
	return err.Error()
}

// Reports an output whose receipt names product and whose bytes still match it, the stale marker ignored
func frontendOutputIntact(out, product string) bool {
	raw, err := os.ReadFile(filepath.Join(out, frontendReceiptName))
	if err != nil {
		return false
	}
	v, err := pyjson.Loads(string(raw))
	saved, ok := v.(*pyjson.Object)
	if err != nil || !ok {
		return false
	}
	schema, _ := saved.Get("schema")
	name, _ := saved.Get("product")
	recorded, _ := saved.Get("outputHash")
	if jsStringify(schema) != "1" || name != product {
		return false
	}
	hash, err := frontendOutputHash(out, frontendStaleMarker)
	return err == nil && recorded == hash
}

// Keeps in next the lazy chunks an open tab of the previous intact generation still loads, never over a file
func frontendKeepPreviousGeneration(previous, next string) []string {
	kept := []string{}
	if !frontendOutputIntact(previous, "webapp") {
		return kept
	}
	root, err := filepath.EvalSymlinks(previous)
	if err != nil {
		return kept
	}
	raw, err := os.ReadFile(filepath.Join(previous, frontendWebappManifest))
	if err != nil {
		return kept
	}
	chunks, ok := frontendManifestValues(raw)
	if !ok {
		return kept
	}
	var names []string
	seen := map[string]bool{}
	add := func(v any) {
		if name, ok := v.(string); ok && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	list := func(v any) []any { items, _ := v.([]any); return items }
	for _, value := range chunks {
		chunk, _ := value.(map[string]any)
		add(chunk["file"])
		for _, field := range []string{"css", "assets"} {
			for _, item := range list(chunk[field]) {
				add(item)
			}
		}
	}
	warn := func(from string, err error) {
		fmt.Fprintf(frontendWebappWarn, "webapp: could not keep %s for tabs on the previous generation (%s)\n", from, frontendErrCode(err))
	}
	for _, name := range names {
		if name == "" || filepath.IsAbs(name) || strings.HasPrefix(name, "/") || strings.ContainsRune(name, 0) ||
			slices.Contains(strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' }), "..") {
			continue
		}
		from := filepath.Join(previous, filepath.FromSlash(name))
		to := filepath.Join(next, filepath.FromSlash(name))
		if _, err := os.Lstat(to); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		info, err := os.Lstat(from)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		real, err := filepath.EvalSymlinks(from)
		if err != nil {
			continue
		}
		if rel, err := filepath.Rel(root, real); err != nil || rel != filepath.Clean(filepath.FromSlash(name)) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o777); err != nil {
			if code := frontendErrCode(err); code != "EEXIST" && code != "ENOTDIR" {
				warn(from, err)
			}
			continue
		}
		if err := frontendWebappCopy(from, to); err != nil {
			if frontendErrCode(err) == "EEXIST" {
				continue
			}
			warn(from, err)
			if rmErr := os.Remove(to); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
				fmt.Fprintf(frontendWebappWarn, "webapp: could not remove the partial copy %s (%s)\n", to, frontendErrCode(rmErr))
			}
			continue
		}
		kept = append(kept, name)
	}
	return kept
}

// Values of a JSON manifest in the order Object.values yields them, false when the text is not JSON
func frontendManifestValues(raw []byte) ([]any, bool) {
	var probe any
	if json.Unmarshal(raw, &probe) != nil {
		return nil, false
	}
	switch v := probe.(type) {
	case []any:
		return v, true
	case map[string]any:
	default:
		return nil, true
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if _, err := dec.Token(); err != nil {
		return nil, false
	}
	var keys []string
	values := map[string]any{}
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, _ := token.(string)
		var value any
		if err := dec.Decode(&value); err != nil {
			return nil, false
		}
		if _, dup := values[key]; !dup {
			keys = append(keys, key)
		}
		values[key] = value
	}
	slices.SortStableFunc(keys, func(a, b string) int {
		ai, aok := jsArrayIndex(a)
		bi, bok := jsArrayIndex(b)
		switch {
		case aok && bok:
			return int(ai) - int(bi)
		case aok:
			return -1
		case bok:
			return 1
		}
		return 0
	})
	out := make([]any, len(keys))
	for i, key := range keys {
		out[i] = values[key]
	}
	return out, true
}

// Canonical array index a JS property key names, the keys Object.values orders first
func jsArrayIndex(key string) (uint32, bool) {
	if key == "" || (len(key) > 1 && key[0] == '0') || len(key) > 10 {
		return 0, false
	}
	var n uint64
	for _, r := range key {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + uint64(r-'0')
	}
	if n >= 1<<32-1 {
		return 0, false
	}
	return uint32(n), true
}

// Makes the workspace hash like the checkout for the webapp product and copies every locked workspace manifest
func frontendMirrorWebappInputs(source, workspace string) error {
	inputs, err := frontendProductInputs("webapp")
	if err != nil {
		return err
	}
	type entry struct{ name, kind string }
	collect := func(root string) ([]entry, error) {
		var out []entry
		err := frontendWalkTree(root, inputs, frontendSkipInput("webapp"), func(name, kind string) error {
			if kind != "missing" {
				out = append(out, entry{name, kind})
			}
			return nil
		})
		return out, err
	}
	wantedList, err := collect(source)
	if err != nil {
		return err
	}
	wanted := map[string]string{}
	for _, e := range wantedList {
		wanted[e.name] = e.kind
	}
	present, err := collect(workspace)
	if err != nil {
		return err
	}
	for i := len(present) - 1; i >= 0; i-- {
		if wanted[present[i].name] != present[i].kind {
			if err := os.RemoveAll(filepath.Join(workspace, filepath.FromSlash(present[i].name))); err != nil {
				return err
			}
		}
	}
	for _, e := range wantedList {
		target := filepath.Join(workspace, filepath.FromSlash(e.name))
		dir := filepath.Dir(target)
		if e.kind == "directory" {
			dir = target
		}
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return err
		}
		if e.kind == "file" {
			if err := copyFileBytes(filepath.Join(source, filepath.FromSlash(e.name)), target); err != nil {
				return err
			}
		}
	}
	raw, err := os.ReadFile(filepath.Join(source, "package-lock.json"))
	if err != nil {
		return err
	}
	var lock struct {
		Packages map[string]struct {
			Link     bool   `json:"link"`
			Resolved string `json:"resolved"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(raw, &lock); err != nil {
		return err
	}
	for _, pkg := range lock.Packages {
		if !pkg.Link || pkg.Resolved == "" {
			continue
		}
		manifest := filepath.Join(filepath.FromSlash(pkg.Resolved), "package.json")
		if _, err := os.Stat(filepath.Join(source, manifest)); err != nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(workspace, manifest)), 0o777); err != nil {
			return err
		}
		if err := copyFileBytes(filepath.Join(source, manifest), filepath.Join(workspace, manifest)); err != nil {
			return err
		}
	}
	return nil
}

// Copies a file's bytes over dest
func copyFileBytes(src, dest string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, data, 0o666)
}

// Node dependency preparation of the webapp workspace, the seam the private install case records
var frontendWebappPrepare = func(ctx context.Context, st *frontendStep, workspace string, install bool, env map[string]string) error {
	return frontendPrepareNodeDependencies(ctx, st, workspace, []string{"apps/desktop"}, env, true, install, false, nil)
}

// Mirrors the webapp inputs into the private workspace and installs its dependencies without compiling
func frontendPrepareWebappWorkspace(ctx context.Context, st *frontendStep, source, workspace string, install bool, env map[string]string) (string, string, error) {
	source, err := filepath.Abs(source)
	if err != nil {
		return "", "", err
	}
	if workspace == "" {
		workspace = filepath.Join(source, ".build", "webapp-workspace")
	}
	if workspace, err = filepath.Abs(workspace); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(workspace, 0o777); err != nil {
		return "", "", err
	}
	if err := frontendMirrorWebappInputs(source, workspace); err != nil {
		return "", "", err
	}
	prepared := make(map[string]string, len(env)+1)
	for k, v := range env {
		prepared[k] = v
	}
	prepared["npm_config_ignore_scripts"] = "true"
	return source, workspace, frontendWebappPrepare(ctx, st, workspace, install, prepared)
}

// Compiles the browser renderer from a prepared workspace into out under a receipt certifying the checkout
func frontendBuildWebapp(ctx context.Context, st *frontendStep, source, workspace, out string, env map[string]string) error {
	if workspace == "" {
		return errors.New("A prepared Webapp workspace is required")
	}
	const app = "apps/desktop"
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return err
	}
	absSource, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	children, err := os.ReadDir(filepath.Join(absSource, filepath.FromSlash(app)))
	if err != nil {
		return err
	}
	var protected []string
	for _, child := range children {
		if name := child.Name(); name != "dist" && name != "dist-webapp" && name != "build" {
			protected = append(protected, app+"/"+name)
		}
	}
	relWorkspace, err := filepath.Rel(absSource, workspace)
	if err != nil {
		return err
	}
	protected = append(protected, "apps/shared", "package.json", "package-lock.json", "scripts/build", "node_modules", filepath.ToSlash(relWorkspace))
	src, dest, err := frontendProductOutput(absSource, out, protected)
	if err != nil {
		return err
	}
	inputs, err := frontendBuildInputs(src, "webapp", nil, env)
	if err != nil {
		return err
	}
	mirrored, err := frontendSourceHash(workspace, "webapp")
	if err != nil {
		return err
	}
	if wantHash, _ := inputs.Get("sourceHash"); mirrored != wantHash {
		return fmt.Errorf("Prepared Webapp workspace %s does not match %s; prepare it again", workspace, src)
	}
	node, err := lookPathIn("node", env["PATH"])
	if err != nil {
		return errors.New("node is required on PATH")
	}
	host, err := frontendNodeHost(node)
	if err != nil {
		return err
	}
	vite, err := frontendWorkspaceBin(workspace, app, "vite", "vite")
	if err != nil {
		return err
	}
	vitePkg := filepath.Dir(filepath.Dir(vite))
	root := filepath.Join(workspace, filepath.FromSlash(app))
	return frontendWithProduct(dest, src, func(product, scratch string) error {
		quote := func(s string) string { raw, _ := json.Marshal(s); return string(raw) }
		viteMain := "file://" + filepath.ToSlash(filepath.Join(vitePkg, "dist", "node", "index.js"))
		config := "import { loadConfigFromFile, mergeConfig } from " + quote(viteMain) + "\n" +
			"export default async (env) => {\n" +
			"  const loaded = await loadConfigFromFile(env, undefined, " + quote(root) + ", undefined, undefined, 'runner')\n" +
			"  return mergeConfig(loaded?.config ?? {}, { root: " + quote(root) + ", cacheDir: " + quote(filepath.Join(scratch, "vite-cache")) +
			", build: { outDir: " + quote(product) + ", emptyOutDir: true, manifest: " + quote(frontendWebappManifest) + " } })\n" +
			"}\n"
		wrapper := filepath.Join(scratch, "vite.config.mjs")
		if err := os.WriteFile(wrapper, []byte(config), 0o666); err != nil {
			return err
		}
		if err := st.run(ctx, src, []string{node, vite, "build", "--config", wrapper, "--configLoader", "native"}, root, env); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(product, "index.html")); err != nil {
			return errors.New("Webapp build did not produce index.html")
		}
		frontendKeepPreviousGeneration(dest, product)
		return frontendRecordProduct(host, src, "webapp", product, inputs, env, nil, false)
	})
}

// Prepares the private workspace and builds the browser renderer into apps/desktop/dist-webapp
func frontendBuildSourceWebapp(ctx context.Context, st *frontendStep, source, workspace string, install bool, env map[string]string) error {
	src, ws, err := frontendPrepareWebappWorkspace(ctx, st, source, workspace, install, env)
	if err != nil {
		return err
	}
	return frontendBuildWebapp(ctx, st, src, ws, filepath.Join(src, "apps", "desktop", "dist-webapp"), env)
}

// Prepares the webapp workspace of a checkout and installs its dependencies without compiling
func PrepareWebappWorkspace(ctx context.Context, projectRoot, workspace string, install bool, env map[string]string) (string, error) {
	st := newFrontendStep("Preparing the Webapp workspace")
	_, ws, err := frontendPrepareWebappWorkspace(ctx, st, projectRoot, workspace, install, env)
	return ws, st.close(err)
}

// Prepares the private workspace and compiles the browser renderer of a checkout
func BuildSourceWebapp(ctx context.Context, projectRoot, workspace string, install bool, env map[string]string) error {
	st := newFrontendStep("Building the Webapp renderer")
	return st.close(compileSourceWebapp(ctx, st, projectRoot, workspace, install, env))
}

// Prepares the locked Electron runtime of a checkout's installed desktop dependencies and returns its executable
func PrepareElectronRuntime(ctx context.Context, projectRoot string, install bool, env map[string]string) (string, error) {
	st := newFrontendStep("Preparing the Electron runtime")
	node, err := lookPathIn("node", env["PATH"])
	if err != nil {
		return "", st.close(errors.New("node is required on PATH"))
	}
	executable, err := frontendPrepareElectronRuntime(ctx, st, projectRoot, install, env, node)
	return executable, st.close(err)
}
