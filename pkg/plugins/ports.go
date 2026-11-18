package plugins

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugintree"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/profiles"
)

var (
	portsMu     sync.RWMutex
	portsByName = map[string]Plugin{}
)

// Source an install of a port no catalog lists records
const builtinPortSource = "builtin:"

// Adds a compiled-in port of a plugin that ships outside core, listed and loaded only once installed into a home
func RegisterPort(p Plugin) {
	name := p.Manifest().Name
	if name == "" {
		panic("plugins: RegisterPort with an empty plugin name")
	}
	portsMu.Lock()
	defer portsMu.Unlock()
	if _, dup := portsByName[name]; dup {
		panic("plugins: port " + name + " registered twice")
	}
	portsByName[name] = p
}

// Manifest of a catalog port read from the plugin.yaml of its compiled-in catalog tree
func CatalogPortManifest(name string) Manifest {
	tree, ok := plugintree.CatalogTree(name)
	if !ok {
		panic("plugins: no catalog tree compiled in for " + name)
	}
	raw, err := fs.ReadFile(tree, "plugin.yaml")
	if err != nil {
		panic(fmt.Sprintf("plugins: catalog tree of %s: %v", name, err))
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		panic(fmt.Sprintf("plugins: catalog tree of %s: %v", name, err))
	}
	return *manifestFromData(doc, "", "user", "")
}

// Directory of the installed plugin tree the context loads, empty for a plugin compiled into the bundled set
func (c *Context) PluginDir() string { return c.manifest.Path }

// Compiled-in port registered under a plugin name
func compiledPort(name string) (Plugin, bool) {
	portsMu.RLock()
	defer portsMu.RUnlock()
	p, ok := portsByName[name]
	return p, ok
}

// Names of every compiled-in port in sorted order
func portNames() []string {
	portsMu.RLock()
	defer portsMu.RUnlock()
	names := make([]string, 0, len(portsByName))
	for name := range portsByName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Port an installed manifest runs, a directory named like the port that its compiled-in publication installed
func installedPort(mf *Manifest) (Plugin, bool) {
	if mf == nil || mf.Path == "" || mf.Source == "bundled" || mf.Portable || mf.HasEntry() || manifestRuntime(mf) != "" {
		return nil, false
	}
	if filepath.Base(mf.Path) != mf.Name || !runsAsPort(mf.Path) {
		return nil, false
	}
	return compiledPort(mf.Name)
}

// Manifest file a compiled-in port publishes, the plugin.yaml of its catalog tree or the one written from its manifest
func portManifestFile(name string) ([]byte, bool) {
	p, ok := compiledPort(name)
	if !ok {
		return nil, false
	}
	if tree, listed := plugintree.CatalogTree(name); listed {
		data, err := fs.ReadFile(tree, "plugin.yaml")
		return data, err == nil
	}
	data, err := builtinPortManifest(p)
	return data, err == nil
}

// Whether a staged tree publishing at target carries the manifest its compiled-in port publishes, whose own source never loads
func publishesPort(staged, target string) bool {
	want, ok := portManifestFile(filepath.Base(target))
	if !ok {
		return false
	}
	got, err := os.ReadFile(filepath.Join(staged, "plugin.yaml"))
	return err == nil && bytes.Equal(got, want)
}

// Whether an installed plugin directory was published by its compiled-in port, the catalog install or the builtin one
func runsAsPort(pluginDir string) bool {
	name := filepath.Base(pluginDir)
	if _, ok := compiledPort(name); !ok {
		return false
	}
	pluginsDir := filepath.Dir(pluginDir)
	metadata, err := readInstallMetadata(filepath.Dir(pluginsDir))
	if err != nil || filepath.Clean(home.PluginsDir(filepath.Dir(pluginsDir))) != filepath.Clean(pluginsDir) {
		return false
	}
	record, _ := metadata[name].(map[string]any)
	if port, ok := builtinPortRecord(record); ok {
		return port == name
	}
	block, _ := record["catalog"].(map[string]any)
	return portCatalogEntry(name) != nil && scalarString(block["name"]) == name
}

// Catalog entry of a compiled-in port whose catalog tree is compiled in, nil for any other name
func portCatalogEntry(name string) *PluginCatalogEntry {
	if _, ok := compiledPort(name); !ok {
		return nil
	}
	if _, ok := plugintree.CatalogTree(name); !ok {
		return nil
	}
	return GetCatalogEntry(name, "")
}

// Catalog entry of a name, the compiled pin of a catalog port without asking the live catalog
func catalogEntryFor(ctx context.Context, name string) *PluginCatalogEntry {
	if entry := portCatalogEntry(name); entry != nil {
		return entry
	}
	return GetLiveCatalogEntry(ctx, name)
}

// Kill list entry naming a candidate, the bundled list alone for a catalog port whose files the binary carries
func findRemovedFor(ctx context.Context, port bool, candidate string) *RemovedEntry {
	if port {
		return matchRemoved(candidate, loadRemovedList(""))
	}
	return FindRemoved(ctx, candidate, "")
}

// Bare repository url a source compares as, scheme and host kept, trailing slash and .git dropped
func comparableRepo(url string) string {
	return strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(url), "/"), ".git")
}

// Compiled-in tree a catalog install of this source at this revision publishes, false when none is
func compiledCatalogTree(gitURL, subdir, revision string) (fs.FS, bool) {
	if revision == "" {
		return nil, false
	}
	for _, name := range portNames() {
		entry := portCatalogEntry(name)
		if entry == nil || !strings.EqualFold(entry.Sha, revision) || strings.Trim(entry.Subdir, "/") != strings.Trim(subdir, "/") {
			continue
		}
		if comparableRepo(entry.Repo) != comparableRepo(gitURL) {
			continue
		}
		return plugintree.CatalogTree(name)
	}
	return nil, false
}

// Revision a catalog entry installs at, the explicit ref when one is given
func catalogInstallRevision(entry *PluginCatalogEntry, ref string) string {
	if strings.TrimSpace(ref) != "" {
		return strings.ToLower(strings.TrimSpace(ref))
	}
	return strings.ToLower(entry.Sha)
}

// Whether a catalog install of the entry at ref publishes the tree compiled into the binary
func catalogEntryCompiled(entry *PluginCatalogEntry, ref string) bool {
	if entry == nil {
		return false
	}
	gitURL, subdir, err := resolveGitURL(entry.InstallIdentifier())
	if err != nil {
		return false
	}
	_, ok := compiledCatalogTree(gitURL, subdir, catalogInstallRevision(entry, ref))
	return ok
}

// Writes the compiled-in catalog tree at the revision where a clone restricted to subdir puts it, else clones
func fetchPluginSource(ctx context.Context, h, tmpClone, gitURL, revision, subdir string) (string, error) {
	tree, ok := compiledCatalogTree(gitURL, subdir, revision)
	if !ok {
		return clonePluginRepo(ctx, h, tmpClone, gitURL, revision, subdir)
	}
	dest := tmpClone
	if subdir != "" {
		dest = filepath.Join(tmpClone, filepath.FromSlash(strings.Trim(subdir, "/")))
	}
	if err := writePortTree(tree, dest); err != nil {
		return "", err
	}
	return strings.ToLower(revision), nil
}

// Copies every file of a tree under dir the way a checkout writes them
func writePortTree(tree fs.FS, dir string) error {
	return fs.WalkDir(tree, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(target, 0o777)
		}
		data, err := fs.ReadFile(tree, p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o666)
	})
}

// Compiled-in port no catalog lists, false for a catalog name or any other name
func builtinOnlyPort(name string) (Plugin, bool) {
	p, ok := compiledPort(name)
	if !ok {
		return nil, false
	}
	if _, listed := plugintree.CatalogTree(name); listed {
		return nil, false
	}
	return p, true
}

// Port no catalog lists that an identifier names, by plugin name or by the upstream repository its manifest records
func builtinPortFor(identifier string) (Plugin, bool) {
	if p, ok := builtinOnlyPort(identifier); ok {
		return p, true
	}
	gitURL, subdir, err := resolveGitURL(identifier)
	if err != nil || subdir != "" {
		return nil, false
	}
	for _, name := range portNames() {
		p, ok := builtinOnlyPort(name)
		if !ok {
			continue
		}
		repo, _ := p.Manifest().Raw["repository"].(string)
		if repo != "" && strings.EqualFold(comparableRepo(repo), comparableRepo(gitURL)) {
			return p, true
		}
	}
	return nil, false
}

// Manifest file an install of a port no catalog lists publishes, written from the compiled-in manifest
func builtinPortManifest(p Plugin) ([]byte, error) {
	m := p.Manifest()
	kind := m.Kind
	if kind == "" {
		kind = "standalone"
	}
	fields := []struct {
		key   string
		value any
		keep  bool
	}{
		{"name", m.Name, true}, {"kind", kind, true}, {"version", m.Version, true}, {"author", m.Author, m.Author != ""},
		{"homepage", m.Homepage, m.Homepage != ""}, {"license", m.License, m.License != ""}, {"description", m.Description, true},
		{"provides_tools", append([]string{}, m.ProvidesTools...), true}, {"provides_hooks", append([]string{}, m.ProvidesHooks...), true},
	}
	doc := yaml.Node{Kind: yaml.MappingNode}
	for _, f := range fields {
		if !f.keep {
			continue
		}
		var v yaml.Node
		if err := v.Encode(f.value); err != nil {
			return nil, err
		}
		doc.Content = append(doc.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: f.key}, &v)
	}
	return yaml.Marshal(&doc)
}

// Publishes the manifest of a compiled-in port no catalog lists into the home with its builtin source recorded
func installBuiltinPort(ctx context.Context, name string, force, assumeDepsConsent bool) (string, map[string]any, string, error) {
	p, ok := builtinOnlyPort(name)
	if !ok {
		return "", nil, "", &PluginOperationError{Msg: fmt.Sprintf("'%s' is not compiled into Icebath.", name), FailureClass: "invalid_source"}
	}
	h := home.Dir(ctx)
	dir, err := pluginsDir(h)
	if err != nil {
		return "", nil, "", err
	}
	link := filepath.Join(dir, name)
	if st, lerr := os.Lstat(link); (lerr == nil && st.Mode()&os.ModeSymlink != 0) || pm.IsJunction(link) {
		return "", nil, "", &PluginOperationError{Msg: fmt.Sprintf("Plugin '%s' is already installed outside Icebath: %s is a link to %s. "+
			"Delete that link to install the compiled-in version.", name, link, resolvePath(link)), FailureClass: "already_installed"}
	}
	target, err := sanitizePluginName(name, dir, false)
	if err != nil {
		return "", nil, "", &PluginOperationError{Msg: err.Error(), FailureClass: "manifest_invalid"}
	}
	if _, statErr := os.Stat(target); statErr == nil && !force {
		command, err := pm.CLICommandName(pm.RepoRoot())
		if err != nil {
			return "", nil, "", err
		}
		return "", nil, "", &PluginOperationError{Msg: fmt.Sprintf("Plugin '%s' already exists. Use force reinstall or run `%s plugins update %s`.",
			name, command, name), FailureClass: "already_installed"}
	}
	oldMetadata, err := readInstallMetadata(h)
	if err != nil {
		return "", nil, "", err
	}
	data, err := builtinPortManifest(p)
	if err != nil {
		return "", nil, "", err
	}
	tmp, err := os.MkdirTemp(dir, ".install-")
	if err != nil {
		return "", nil, "", err
	}
	defer os.RemoveAll(tmp)
	staged := filepath.Join(tmp, "plugin")
	if err := os.MkdirAll(staged, 0o777); err != nil {
		return "", nil, "", err
	}
	if err := os.WriteFile(filepath.Join(staged, "plugin.yaml"), data, 0o666); err != nil {
		return "", nil, "", err
	}
	manifest, err := ReadManifestForInstall(staged)
	if err != nil {
		return "", nil, "", err
	}
	if err := checkManifestVersion(ctx, h, manifest, name); err != nil {
		return "", nil, "", err
	}
	newMetadata := map[string]any{}
	for k, v := range oldMetadata {
		newMetadata[k] = v
	}
	newMetadata[name] = map[string]any{"pinned": false, "revision": RunningVersion, "source": builtinPortSource + name}
	if err := publishPlugin(ctx, staged, target, oldMetadata, newMetadata, publishOptions{RequireConsent: true, AssumeConsent: assumeDepsConsent}); err != nil {
		return "", nil, "", &PluginOperationError{Msg: fmt.Sprintf("Plugin '%s' was not published: %v", name, err), FailureClass: publishFailureClass(err)}
	}
	installed, err := readManifest(target)
	if err != nil {
		return "", nil, "", err
	}
	return target, installed, name, nil
}

// Installs a compiled-in port no catalog lists the way a catalog install runs, then prompts, enables and reports
func installBuiltinPortCommand(ctx context.Context, h string, console *Console, opts InstallCommandOptions, p Plugin) error {
	m := p.Manifest()
	console.Print(fmt.Sprintf("[bold]%s[/bold] [cyan]\\[built in][/cyan] [dim]compiled into Icebath %s[/dim]", m.Name, RunningVersion))
	if !opts.Brief {
		console.Print(m.Description)
	}
	if strings.TrimSpace(opts.Ref) != "" {
		return fail(console, fmt.Sprintf("[red]Error:[/red] --ref selects a Git revision; '%s' is compiled into Icebath and has none.", m.Name))
	}
	target, manifest, name, err := installBuiltinPort(ctx, m.Name, opts.Force, opts.YesDeps)
	var opErr *PluginOperationError
	if errors.As(err, &opErr) {
		return fail(console, fmt.Sprintf("[red]Error:[/red] %v", err))
	}
	if err != nil {
		return err
	}
	return finishInstall(ctx, h, console, opts, opts.Identifier, target, manifest, name)
}

// Install command of a plugin for a home, the profile flag added for any home but the sticky default
func pluginInstallCommand(name, h string) string {
	profile := home.ProfileNameFor(h)
	if profile == "" || (profile == "default" && profiles.GetActiveProfile("") == "default") {
		return "icebath plugins install " + name
	}
	return fmt.Sprintf("icebath -p %s plugins install %s", profile, name)
}

// Install sentence for the bundled key or catalog name of a plugin that left core, empty for any other name
func movedPluginHint(name string) string {
	for _, row := range leftCoreBundledKeys {
		if name == row.Key || name == row.Plugin {
			return fmt.Sprintf("%s moved out of core into the '%s' plugin: install it with `%s`.", row.Label, row.Plugin,
				pluginInstallCommand(row.Plugin, home.Dir(context.Background())))
		}
	}
	return ""
}

// Whether an install record names a port no catalog lists
func builtinPortRecord(record map[string]any) (string, bool) {
	source, _ := record["source"].(string)
	name, ok := strings.CutPrefix(source, builtinPortSource)
	if !ok {
		return "", false
	}
	_, compiled := builtinOnlyPort(name)
	return name, compiled
}

// Republishes an installed port no catalog lists from the running binary, true when its manifest changed
func updateBuiltinPort(ctx context.Context, name, target string) (bool, error) {
	before, err := os.ReadFile(filepath.Join(target, "plugin.yaml"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if _, _, _, err := installBuiltinPort(ctx, name, true, true); err != nil {
		return false, err
	}
	after, err := os.ReadFile(filepath.Join(target, "plugin.yaml"))
	if err != nil {
		return false, err
	}
	return string(before) != string(after), nil
}
