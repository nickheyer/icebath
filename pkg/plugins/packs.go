package plugins

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	exactSHARe        = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	packSecretKeyRe   = regexp.MustCompile(`(?i)(token|secret|passw(or)?d|api[_-]?key|private[_-]?key|credential|auth)`)
	packGithubHTTPSRe = regexp.MustCompile(`^https://github\.com/([^/\s]+)/([^/\s#]+?)(?:\.git)?$`)
)

const (
	maxPackBytes      = 1 * 1024 * 1024
	packFetchTimeout  = 15 * time.Second
	defaultPackName   = "my-icebath-pack"
	packUsageRefusal  = "[red]Error:[/red] Usage: icebath plugins pack {install|export|show}"
	packExportComment = "Exported by `icebath plugins pack export`."
)

// Pack parse, validation or fetch failure the CLI exits non zero on
type PackError struct{ Msg string }

func (e *PackError) Error() string { return e.Msg }

func packErrorf(format string, args ...any) error {
	return &PackError{Msg: fmt.Sprintf(format, args...)}
}

// One pinned plugin of a pack, empty name, repo or subdir standing for None
type PackPluginEntry struct {
	Ref, Name, Repo, Subdir string
}

// Name or repo with the subdir of a repo entry, a question mark when neither is set
func (e PackPluginEntry) Display() string {
	base := e.Name
	if base == "" {
		base = e.Repo
	}
	if base == "" {
		base = "?"
	}
	if e.Repo != "" && e.Subdir != "" {
		return base + "/" + e.Subdir
	}
	return base
}

// Install identifier of a repo entry, empty for a bare name the catalog resolves
func (e PackPluginEntry) InstallIdentifier() string {
	if e.Repo == "" {
		return ""
	}
	if e.Subdir != "" {
		return e.Repo + "/" + e.Subdir
	}
	return e.Repo
}

// Plugin id and its validated non secret config seed in pack order
type PackSeed struct {
	ID   string
	Seed map[string]any
}

// A parsed and validated pack manifest
type PluginPack struct {
	Name, Description, Author, Version string
	Plugins                            []PackPluginEntry
	Config                             []PackSeed
	Skills                             []string
}

// Seed of a plugin id, nil when the pack carries none
func (p *PluginPack) seedFor(id string) map[string]any {
	for _, s := range p.Config {
		if s.ID == id {
			return s.Seed
		}
	}
	return nil
}

// Python typed keys of a loaded pack document for the keys that were not YAML strings
type packKeys map[uintptr]map[string]any

func mappingID(m map[string]any) uintptr { return reflect.ValueOf(m).Pointer() }

// Typed key of a mapping entry and whether it was a YAML string
func (k packKeys) typed(m map[string]any, key string) (any, bool) {
	if v, ok := config.MappingPyKey(m, key); ok {
		return v, false
	}
	if v, ok := k[mappingID(m)][key]; ok {
		return v, false
	}
	return key, true
}

// Python str of a mapping key
func (k packKeys) str(m map[string]any, key string) string {
	v, _ := k.typed(m, key)
	return pyrepr.Str(v)
}

// Records the Python type of every non string key by walking the node tree beside the loaded value
func (k packKeys) walk(n *yaml.Node, v any) {
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) > 0 {
			k.walk(n.Content[0], v)
		}
	case yaml.AliasNode:
		k.walk(n.Alias, v)
	case yaml.SequenceNode:
		if list, ok := v.([]any); ok {
			for i, c := range n.Content {
				if i < len(list) {
					k.walk(c, list[i])
				}
			}
		}
	case yaml.MappingNode:
		m, ok := v.(map[string]any)
		if !ok {
			return
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			keyNode, valNode := n.Content[i], n.Content[i+1]
			var loaded any
			if keyNode.Decode(&loaded) != nil {
				continue
			}
			loadedKey, isStr := loaded.(string)
			if !isStr {
				loadedKey = fmt.Sprint(loaded)
			}
			if typed, ok := yaml11Key(keyNode); ok {
				if _, s := typed.(string); !s {
					if k[mappingID(m)] == nil {
						k[mappingID(m)] = map[string]any{}
					}
					k[mappingID(m)][loadedKey] = typed
				}
			}
			k.walk(valNode, m[loadedKey])
		}
	}
}

// Value a YAML 1.1 safe loader gives a mapping key node, found by loading it as a sequence item
func yaml11Key(n *yaml.Node) (any, bool) {
	if n.Kind == yaml.ScalarNode && n.Style != 0 && n.Style&yaml.TaggedStyle == 0 {
		return n.Value, true
	}
	if n.Kind == yaml.ScalarNode && n.Style == 0 {
		if v, ok := yaml11TimestampKey(n.Value); ok {
			return v, true
		}
	}
	text, err := yaml.Marshal(&yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{n}})
	if err != nil {
		return nil, false
	}
	doc, err := config.FastSafeLoad(text)
	if err != nil {
		return nil, false
	}
	list, ok := doc.([]any)
	if !ok || len(list) != 1 {
		return nil, false
	}
	if _, isTime := list[0].(time.Time); isTime {
		return n.Value, true
	}
	return list[0], true
}

// Date or datetime a YAML 1.1 safe loader builds from a plain timestamp mapping key
func yaml11TimestampKey(text string) (any, bool) {
	v, ok, err := pydatetime.YAMLTimestamp(text)
	return v, ok && err == nil
}

// First ValueError building a date or datetime from a plain timestamp scalar, raised past the YAML error wrap
func yaml11TimestampValueError(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		if n.Style != 0 {
			return nil
		}
		_, _, err := pydatetime.YAMLTimestamp(n.Value)
		return err
	}
	for _, c := range n.Content {
		if err := yaml11TimestampValueError(c); err != nil {
			return err
		}
	}
	return nil
}

// Quoted name or repo of a plugins item, else its one based position
func entryLabel(item any, index int) string {
	if m, ok := item.(map[string]any); ok {
		label := m["name"]
		if !pyrepr.Truthy(label) {
			label = m["repo"]
		}
		if s, ok := label.(string); ok && pystr.Strip(s) != "" {
			return "'" + pystr.Strip(s) + "'"
		}
	}
	return fmt.Sprintf("#%d", index+1)
}

// Reserved or secret when a config key may never travel in a pack, empty otherwise
func forbiddenKeyReason(key string) string {
	if key == "granted_capabilities" || key == "capabilities_consent" || strings.HasPrefix(key, "allow_") {
		return "reserved"
	}
	if packSecretKeyRe.MatchString(key) {
		return "secret"
	}
	return ""
}

// Dotted key and reason of the first forbidden key at any depth, mappings inside lists included
func firstForbiddenKey(value any, path string, keys packKeys) (string, string, bool) {
	switch x := value.(type) {
	case map[string]any:
		for _, key := range config.MappingKeys(x) {
			if _, isStr := keys.typed(x, key); isStr {
				if reason := forbiddenKeyReason(key); reason != "" {
					return path + key, reason, true
				}
			}
			if found, reason, ok := firstForbiddenKey(x[key], path+keys.str(x, key)+".", keys); ok {
				return found, reason, true
			}
		}
	case []any:
		for _, child := range x {
			if found, reason, ok := firstForbiddenKey(child, path, keys); ok {
				return found, reason, true
			}
		}
	}
	return "", "", false
}

// Copy with forbidden keys and non YAML scalar leaves removed at every depth
func stripForbiddenKeys(value any) any {
	switch x := value.(type) {
	case map[string]any:
		out := map[string]any{}
		var order []string
		for _, key := range config.MappingKeys(x) {
			child := x[key]
			if forbiddenKeyReason(key) != "" || !yamlScalarOrContainer(child) {
				continue
			}
			out[key] = stripForbiddenKeys(child)
			order = append(order, key)
		}
		config.SetMappingKeys(out, order)
		return out
	case []any:
		out := make([]any, len(x))
		for i, child := range x {
			out[i] = stripForbiddenKeys(child)
		}
		return out
	}
	return value
}

// None, str, int, float, bool, list or dict as a loaded config holds them
func yamlScalarOrContainer(v any) bool {
	switch v.(type) {
	case nil, string, bool, int, int64, uint64, float64, []any, map[string]any:
		return true
	}
	_, isInt := pyrepr.Int(v)
	return isInt
}

// Validated copy of one plugin's config seed, refusing reserved, trust gate and secret shaped keys at any depth
func ValidateConfigSeed(pluginID string, seed any) (map[string]any, error) {
	return validateConfigSeed(pluginID, seed, nil)
}

func validateConfigSeed(pluginID string, seed any, keys packKeys) (map[string]any, error) {
	m, ok := seed.(map[string]any)
	if !ok {
		return nil, packErrorf("Pack config for plugin '%s' must be a mapping of plugins.entries.%s keys.", pluginID, pluginID)
	}
	order := config.MappingKeys(m)
	for _, key := range order {
		typed, isStr := keys.typed(m, key)
		if !isStr || pystr.Strip(key) == "" {
			return nil, packErrorf("Pack config for plugin '%s' has an invalid key: %s.", pluginID, pyrepr.Repr(typed))
		}
	}
	if key, reason, found := firstForbiddenKey(m, "", keys); found {
		if reason == "reserved" {
			return nil, packErrorf("Pack config for plugin '%s' sets reserved key '%s': packs cannot pre-grant capabilities or trust gates. Capability consent happens interactively at install time.", pluginID, key)
		}
		return nil, packErrorf("Pack config for plugin '%s' sets secret-shaped key '%s': secrets never travel in packs. Declare the secret in the plugin's requires_env instead — it is prompted at install.", pluginID, key)
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	config.SetMappingKeys(out, order)
	return out, nil
}

// Parses and validates a pack YAML document
func ParsePack(text, source string) (*PluginPack, error) {
	if source == "" {
		source = "<pack>"
	}
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(strings.TrimPrefix(text, "\ufeff")), &node); err != nil {
		return nil, packErrorf("Pack %s is not valid YAML: %s", source, err)
	}
	if err := yaml11TimestampValueError(&node); err != nil {
		return nil, err
	}
	loaded, err := config.FastSafeLoad([]byte(text))
	if err != nil {
		return nil, packErrorf("Pack %s is not valid YAML: %s", source, err)
	}
	raw, ok := loaded.(map[string]any)
	if !ok {
		return nil, packErrorf("Pack %s must be a YAML mapping.", source)
	}
	keys := packKeys{}
	keys.walk(&node, raw)
	meta := raw
	if nested, ok := raw["pack"].(map[string]any); ok {
		meta = nested
	}
	name, ok := meta["name"].(string)
	if !ok || pystr.Strip(name) == "" {
		return nil, packErrorf("Pack %s is missing a 'name'.", source)
	}
	pluginsRaw, ok := raw["plugins"].([]any)
	if !ok || len(pluginsRaw) == 0 {
		return nil, packErrorf("Pack %s must declare a non-empty 'plugins' list.", source)
	}
	pack := &PluginPack{Name: pystr.Strip(name), Description: packStrOr(meta["description"]), Author: packStrOr(meta["author"]), Version: packStrOr(meta["version"])}
	for i, item := range pluginsRaw {
		entry, err := parsePackEntry(item, entryLabel(item, i))
		if err != nil {
			return nil, err
		}
		pack.Plugins = append(pack.Plugins, entry)
	}
	configRaw := raw["config"]
	if !pyrepr.Truthy(configRaw) {
		configRaw = map[string]any{}
	}
	configMap, ok := configRaw.(map[string]any)
	if !ok {
		return nil, packErrorf("Pack %s 'config' must be a mapping of plugin id → settings.", source)
	}
	for _, key := range config.MappingKeys(configMap) {
		id := keys.str(configMap, key)
		seed, err := validateConfigSeed(id, configMap[key], keys)
		if err != nil {
			return nil, err
		}
		pack.setSeed(id, seed)
	}
	skillsRaw := raw["skills"]
	if !pyrepr.Truthy(skillsRaw) {
		skillsRaw = []any{}
	}
	skills, ok := skillsRaw.([]any)
	if !ok {
		return nil, packErrorf("Pack %s 'skills' must be a list of skill ids.", source)
	}
	for _, s := range skills {
		if text := pystr.Strip(pyrepr.Str(s)); text != "" {
			pack.Skills = append(pack.Skills, text)
		}
	}
	return pack, nil
}

// Replaces the seed of an id in place or appends it, the dict comprehension keeping first position
func (p *PluginPack) setSeed(id string, seed map[string]any) {
	for i := range p.Config {
		if p.Config[i].ID == id {
			p.Config[i].Seed = seed
			return
		}
	}
	p.Config = append(p.Config, PackSeed{ID: id, Seed: seed})
}

// Python str of a value, empty when the value is falsy
func packStrOr(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// Validates one plugins item with name or repo, an exact SHA ref and an optional subdir
func parsePackEntry(item any, label string) (PackPluginEntry, error) {
	m, ok := item.(map[string]any)
	if !ok {
		return PackPluginEntry{}, packErrorf("Pack plugin entry %s must be a mapping.", label)
	}
	repoRaw := m["repo"]
	if !pyrepr.Truthy(repoRaw) {
		repoRaw = m["source"]
	}
	repo := ""
	if s, ok := repoRaw.(string); ok {
		repo = pystr.Strip(strings.TrimPrefix(s, "github:"))
	} else if pyrepr.Truthy(repoRaw) {
		repo = pyrepr.Str(repoRaw)
	}
	nameRaw, hasName := m["name"]
	name := ""
	if hasName && nameRaw != nil {
		s, ok := nameRaw.(string)
		if !ok || pystr.Strip(s) == "" {
			return PackPluginEntry{}, packErrorf("Pack plugin entry %s has an invalid 'name'.", label)
		}
		name = pystr.Strip(s)
	}
	if name == "" && repo == "" {
		return PackPluginEntry{}, packErrorf("Pack plugin entry %s needs either 'name' (community index) or 'repo' (owner/repo or git URL).", label)
	}
	refRaw := m["ref"]
	if !pyrepr.Truthy(refRaw) {
		refRaw = m["version"]
	}
	ref, ok := refRaw.(string)
	if !ok || !exactSHARe.MatchString(pystr.Strip(ref)) {
		return PackPluginEntry{}, packErrorf("Pack plugin entry %s has ref %s: refs must be exact 40-character commit SHAs (tags and branch names are rejected — pin the commit for reproducible installs).", label, pyrepr.Repr(refRaw))
	}
	subdir := ""
	if subdirRaw, has := m["subdir"]; has && subdirRaw != nil {
		s, ok := subdirRaw.(string)
		if !ok || strings.Trim(s, "/") == "" {
			return PackPluginEntry{}, packErrorf("Pack plugin entry %s has an invalid 'subdir'.", label)
		}
		subdir = strings.Trim(s, "/")
	}
	return PackPluginEntry{Ref: strings.ToLower(pystr.Strip(ref)), Name: name, Repo: repo, Subdir: subdir}, nil
}

// Client the https pack fetch uses, a seam for tests
var packHTTPClient = func() *http.Client { return pyhttpx.PlainClient(true) }

// Loads a pack from a local file path or an https URL
func LoadPack(ctx context.Context, pathOrURL string) (*PluginPack, error) {
	if strings.HasPrefix(pathOrURL, "https://") {
		text, err := fetchPackText(ctx, pathOrURL)
		if err != nil {
			return nil, packErrorf("Could not fetch pack from %s: %s", pathOrURL, err)
		}
		if len(strings.ToValidUTF8(text, "")) > maxPackBytes {
			return nil, packErrorf("Pack payload exceeds the 1 MiB size limit.")
		}
		return ParsePack(text, pathOrURL)
	}
	for _, scheme := range []string{"http://", "file://", "ftp://"} {
		if strings.HasPrefix(pathOrURL, scheme) {
			return nil, packErrorf("Pack URLs must use https:// (or pass a local file path).")
		}
	}
	path := pypath.PathStr(pypath.Expanduser(pathOrURL))
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return nil, packErrorf("Pack file not found: %s", path)
	}
	if st.Size() > maxPackBytes {
		return nil, packErrorf("Pack file exceeds the 1 MiB size limit.")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := strictUTF8(raw); err != nil {
		return nil, err
	}
	return ParsePack(strings.TrimPrefix(string(raw), "\ufeff"), path)
}

// Body of an https pack after redirects, a non success status raising like raise_for_status
func fetchPackText(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := pyhttpx.Do(packHTTPClient(), req, packFetchTimeout)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("%s", pyhttpx.RaiseForStatusMessage(resp.StatusCode, resp.URL, resp.Header.Get("Location")))
	}
	return resp.Text(), nil
}

// A pack entry resolved to an installable identifier, empty when catalog resolution failed
type ResolvedPackPlugin struct {
	Entry             PackPluginEntry
	Identifier        string
	IndexCapabilities []string
	ResolveError      string
}

// Live catalog the pack resolver reads, a seam for tests
var packCatalog = LoadCatalogLive

// Resolves every entry, bare names through the curated catalog, failures carried per entry
func ResolvePackPlugins(ctx context.Context, pack *PluginPack) []ResolvedPackPlugin {
	var resolved []ResolvedPackPlugin
	var entries []*PluginCatalogEntry
	loaded := false
	for _, entry := range pack.Plugins {
		if id := entry.InstallIdentifier(); id != "" {
			resolved = append(resolved, ResolvedPackPlugin{Entry: entry, Identifier: id})
			continue
		}
		if !loaded {
			entries, loaded = packCatalog(ctx), true
		}
		var match *PluginCatalogEntry
		for _, e := range entries {
			if e.Name == entry.Name {
				match = e
				break
			}
		}
		if match == nil {
			resolved = append(resolved, ResolvedPackPlugin{Entry: entry, ResolveError: "not found in the plugin catalog"})
			continue
		}
		caps := match.Capabilities
		resolved = append(resolved, ResolvedPackPlugin{Entry: entry, Identifier: match.InstallIdentifier(),
			IndexCapabilities: append(append(append([]string{}, caps.ProvidesTools...), caps.ProvidesHooks...), caps.ProvidesMiddleware...)})
	}
	return resolved
}

// Prints the mandatory pack review screen
func renderPackReview(console *Console, pack *PluginPack, resolved []ResolvedPackPlugin) {
	header := "[bold]" + pack.Name + "[/bold]"
	if pack.Version != "" {
		header += " v" + pack.Version
	}
	if pack.Author != "" {
		header += " — by " + pack.Author
	}
	console.Print("\n" + header)
	if pack.Description != "" {
		console.Print("[dim]" + pack.Description + "[/dim]")
	}
	table := newPluginTable("", [][2]string{{"Plugin", ""}, {"Source", ""}, {"Pinned ref", ""}, {"Capabilities (declared)", ""}})
	for _, rp := range resolved {
		caps := "(shown at install)"
		if len(rp.IndexCapabilities) > 0 {
			caps = strings.Join(rp.IndexCapabilities, ", ")
		}
		source := rp.Identifier
		if source == "" {
			source = "[red]unresolved: " + rp.ResolveError + "[/red]"
		}
		table.AddRow(rp.Entry.Display(), source, pystr.Slice(rp.Entry.Ref, 0, 12), caps)
	}
	console.printTable(table)
	if len(pack.Config) > 0 {
		console.Print("[bold]Config seeds[/bold] (plugins.entries.<id>, non-secret):")
		for _, s := range pack.Config {
			for _, key := range config.MappingKeys(s.Seed) {
				console.Print(fmt.Sprintf("  %s.%s = %s", s.ID, key, pyrepr.Repr(s.Seed[key])))
			}
		}
	}
	if len(pack.Skills) > 0 {
		console.Print("[yellow]Pack lists skills (NOT auto-installed yet):[/yellow] " + strings.Join(pack.Skills, ", "))
		console.Print("[dim]Install them manually, e.g. `icebath skills install <id>`.[/dim]")
	}
	console.Print(fmt.Sprintf("\n[dim]Installing a pack runs third-party code × %d plugins. Each plugin's declared capabilities still require individual consent after install — a pack never bulk-grants.[/dim]", len(resolved)))
}

// Outcome of one plugin install within a pack
type PackInstallResult struct {
	Display       string
	OK            bool
	InstalledName string
	Error         string
}

// Seeds plugins.entries keys of a plugin that are not set yet, user values always winning
func seedPluginConfig(ctx context.Context, h, pluginID string, seed map[string]any, console *Console) error {
	seed, err := ValidateConfigSeed(pluginID, seed)
	if err != nil {
		return err
	}
	configWriteMu.Lock()
	defer configWriteMu.Unlock()
	doc, err := readUserConfig(h)
	if err != nil {
		return err
	}
	parent := doc
	for _, key := range []string{"plugins", "entries"} {
		if _, set := parent[key]; !set {
			parent[key] = map[string]any{}
			config.AppendMappingKey(parent, key)
		}
		child, ok := parent[key].(map[string]any)
		if !ok {
			return fmt.Errorf("'%s' object has no attribute 'setdefault'", pyTypeName(parent[key]))
		}
		parent = child
	}
	if _, set := parent[pluginID]; !set {
		parent[pluginID] = map[string]any{}
		config.AppendMappingKey(parent, pluginID)
	}
	entry, ok := parent[pluginID].(map[string]any)
	if !ok {
		console.Print(fmt.Sprintf("[yellow]Warning:[/yellow] plugins.entries.%s is not a mapping; skipping pack config seed.", pluginID))
		return nil
	}
	wrote := false
	for _, key := range config.MappingKeys(seed) {
		if _, set := entry[key]; set {
			console.Print(fmt.Sprintf("[dim]  plugins.entries.%s.%s already set — keeping your value.[/dim]", pluginID, key))
			continue
		}
		entry[key] = seed[key]
		config.AppendMappingKey(entry, key)
		wrote = true
	}
	if !wrote {
		return nil
	}
	if err := saveUserConfig(ctx, h, doc); err != nil {
		return err
	}
	console.Print(fmt.Sprintf("[dim]  Seeded plugins.entries.%s from pack.[/dim]", pluginID))
	return nil
}

// Install collaborators the pack fan out calls, seams the tests replace
var (
	packInstallCore  = installPluginCore
	packPromptEnv    = promptPluginEnvVars
	packSetEnabled   = setPluginEnabled
	packConsent      = runCapabilityConsent
	packDeclaredCaps = declaredCapabilitiesFromManifest
)

// Fans a pack out to pinned installs with per plugin consent, enabling and seeding each success
func InstallPackPlugins(ctx context.Context, pack *PluginPack, resolved []ResolvedPackPlugin, console *Console, force bool) ([]PackInstallResult, error) {
	h := home.Dir(ctx)
	var results []PackInstallResult
	failed := func(display, msg string) {
		results = append(results, PackInstallResult{Display: display, Error: msg})
		console.Print(fmt.Sprintf("[red]✗[/red] %s: %s", display, msg))
	}
	for _, rp := range resolved {
		display := rp.Entry.Display()
		if rp.Identifier == "" {
			failed(display, rp.ResolveError)
			continue
		}
		console.Print(fmt.Sprintf("[dim]Installing %s @ %s...[/dim]", display, pystr.Slice(rp.Entry.Ref, 0, 12)))
		catalogName := rp.Entry.Name
		if rp.Entry.Repo != "" {
			catalogName = ""
		}
		identifier, ref := rp.Identifier, rp.Entry.Ref
		_, manifest, installedName, err := recordedInstall(ctx, h, func() (string, map[string]any, string, error) {
			return packInstallCore(ctx, identifier, installCoreOptions{Force: force, Ref: ref})
		}, catalogName, identifier)
		if err != nil {
			if _, isOp := err.(*PluginOperationError); !isOp {
				logError("pack install failed for "+display, "error", err)
			}
			failed(display, pyErrorText(err))
			continue
		}
		if err := packPromptEnv(ctx, h, manifest, console); err != nil {
			logDebug("requires_env prompt failed for "+installedName, "error", err)
		}
		if err := packSetEnabled(ctx, h, installedName, true, nil, nil); err != nil {
			if refused, ok := err.(*AdmissionRefused); ok {
				failed(display, refused.Error())
				continue
			}
			return results, err
		}
		if declared := packDeclaredCaps(manifest, installedName); len(declared) > 0 {
			if err := packConsent(ctx, console, installedName, declared, "install"); err != nil {
				return results, err
			}
		}
		seed := pack.seedFor(installedName)
		if seed == nil && rp.Entry.Name != "" && rp.Entry.Name != installedName {
			seed = pack.seedFor(rp.Entry.Name)
		}
		if len(seed) > 0 {
			if err := seedPluginConfig(ctx, h, installedName, seed, console); err != nil {
				pe, ok := err.(*PackError)
				if !ok {
					return results, err
				}
				console.Print("[yellow]Warning:[/yellow] " + pe.Msg)
			}
		}
		console.Print(fmt.Sprintf("[green]✓[/green] %s installed as [bold]%s[/bold].", display, installedName))
		results = append(results, PackInstallResult{Display: display, OK: true, InstalledName: installedName})
	}
	return results, nil
}

// Repo or URL and subdir of a recorded install source
func sourceToRepoSubdir(source string) (string, string) {
	if source == "" {
		return "", ""
	}
	base, subdir, _ := strings.Cut(source, "#")
	subdir = strings.Trim(subdir, "/")
	if m := packGithubHTTPSRe.FindStringSubmatch(pystr.Strip(base)); m != nil {
		return m[1] + "/" + m[2], subdir
	}
	return pystr.Strip(base), subdir
}

// Exportable plugins.entries keys of a plugin with reserved and secret shaped keys stripped at every depth
var sanitizedEntryConfig = func(h, pluginID string) map[string]any {
	cfg, err := loadConfig(h)
	if err != nil {
		return map[string]any{}
	}
	plugins, _ := cfg["plugins"].(map[string]any)
	entries, _ := plugins["entries"].(map[string]any)
	entry, ok := entries[pluginID].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return stripForbiddenKeys(entry).(map[string]any)
}

// Pack YAML of the current install and the warnings for plugins without Git provenance
func ExportPack(ctx context.Context, enabledOnly bool, packName string) (string, []string, error) {
	h := home.Dir(ctx)
	metadata, err := readInstallMetadata(h)
	if err != nil {
		return "", nil, err
	}
	enabled := getEnabledSet(h)
	dir, err := pluginsDir(h)
	if err != nil {
		return "", nil, err
	}
	children, err := os.ReadDir(dir)
	if err != nil {
		return "", nil, err
	}
	var installed []string
	for _, c := range children {
		if strings.HasPrefix(c.Name(), ".") {
			continue
		}
		if st, err := os.Stat(dir + string(os.PathSeparator) + c.Name()); err == nil && st.IsDir() && (!enabledOnly || enabled[c.Name()]) {
			installed = append(installed, c.Name())
		}
	}
	sort.Strings(installed)
	var entries []any
	configOut := map[string]any{}
	var configOrder, warnings []string
	for _, id := range installed {
		record, _ := metadata[id].(map[string]any)
		source := ""
		if pyrepr.Truthy(record["source"]) {
			source = pyrepr.Str(record["source"])
		}
		revision, isStr := record["revision"].(string)
		repo, subdir := sourceToRepoSubdir(source)
		if repo == "" || !isStr || !exactSHARe.MatchString(revision) {
			warnings = append(warnings, id+": no Git provenance (local-only or pre-metadata install) — listed as a comment, not installable from this pack.")
			continue
		}
		entry := map[string]any{"repo": repo, "ref": strings.ToLower(revision)}
		order := []string{"repo", "ref"}
		if subdir != "" {
			entry["subdir"] = subdir
			order = append(order, "subdir")
		}
		config.SetMappingKeys(entry, order)
		entries = append(entries, entry)
		if seed := sanitizedEntryConfig(h, id); len(seed) > 0 {
			configOut[id] = seed
			configOrder = append(configOrder, id)
		}
	}
	if entries == nil {
		entries = []any{}
	}
	doc := map[string]any{"name": packName, "description": packExportComment, "version": "1.0.0", "plugins": entries}
	docOrder := []string{"name", "description", "version", "plugins"}
	if len(configOut) > 0 {
		config.SetMappingKeys(configOut, configOrder)
		doc["config"] = configOut
		docOrder = append(docOrder, "config")
	}
	config.SetMappingKeys(doc, docOrder)
	raw, err := config.SafeDump(doc)
	if err != nil {
		return "", nil, err
	}
	text := string(raw)
	if len(warnings) > 0 {
		lines := make([]string, len(warnings))
		for i, w := range warnings {
			lines[i] = "# WARNING (not exported): " + w
		}
		text = strings.Join(lines, "\n") + "\n" + text
	}
	return text, warnings, nil
}

// Loads a source, exiting 1 on a pack error, resolves it and prints the review screen
func loadAndReview(ctx context.Context, console *Console, source string) (*PluginPack, []ResolvedPackPlugin, error) {
	pack, err := LoadPack(ctx, source)
	if err != nil {
		if pe, ok := err.(*PackError); ok {
			return nil, nil, fail(console, "[red]Error:[/red] "+pe.Msg)
		}
		return nil, nil, err
	}
	resolved := packResolve(ctx, pack)
	renderPackReview(console, pack, resolved)
	return pack, resolved, nil
}

// Resolver the pack commands call, a seam for tests
var packResolve = ResolvePackPlugins

// Dry run review of a pack
func CmdPackShow(ctx context.Context, source string, out io.Writer) error {
	console := NewConsole(out)
	_, resolved, err := loadAndReview(ctx, console, source)
	if err != nil {
		return err
	}
	unresolved := 0
	for _, rp := range resolved {
		if rp.Identifier == "" {
			unresolved++
		}
	}
	if unresolved > 0 {
		noun := "ies"
		if unresolved == 1 {
			noun = "y"
		}
		console.Print(fmt.Sprintf("\n[yellow]%d entr%s could not be resolved — install would skip them and exit non-zero.[/yellow]", unresolved, noun))
	}
	console.Print("\n[dim]Dry run only. Install with `icebath plugins pack install ...`.[/dim]")
	return nil
}

// Review screen, one pack level consent, pinned fan out installs and per plugin consent, exit 1 on any failure
func CmdPackInstall(ctx context.Context, source string, force bool, out io.Writer) error {
	console := NewConsole(out)
	pack, resolved, err := loadAndReview(ctx, console, source)
	if err != nil {
		return err
	}
	if !isTTY() {
		return fail(console, "[red]Error:[/red] Pack install requires an interactive terminal to review and confirm the pack contents (no --yes in v1).")
	}
	if !askYes(fmt.Sprintf("\nInstall %d plugin(s) from pack '%s'? [y/N] ", len(resolved), pack.Name), nil) {
		return fail(console, "[dim]Aborted — nothing installed.[/dim]")
	}
	results, err := InstallPackPlugins(ctx, pack, resolved, console, force)
	if err != nil {
		return err
	}
	var ok, failed []PackInstallResult
	for _, r := range results {
		if r.OK {
			ok = append(ok, r)
		} else {
			failed = append(failed, r)
		}
	}
	console.Print(fmt.Sprintf("\n[bold]Pack '%s':[/bold] %d installed, %d failed.", pack.Name, len(ok), len(failed)))
	for _, r := range failed {
		console.Print(fmt.Sprintf("  [red]✗[/red] %s: %s", r.Display, r.Error))
	}
	if len(ok) > 0 {
		console.Print("[dim]Restart the gateway for the plugins to take effect:[/dim]")
		console.Print("[dim]  icebath gateway restart[/dim]")
	}
	if len(failed) > 0 {
		return &CommandExit{Code: 1}
	}
	return nil
}

// Pack YAML of the current install on out, warnings and errors on errOut
func CmdPackExport(ctx context.Context, enabledOnly bool, name string, out, errOut io.Writer) error {
	console := NewConsole(errOut)
	text, warnings, err := ExportPack(ctx, enabledOnly, name)
	if err != nil {
		console.Print("[red]Error:[/red] Could not export pack: " + pyErrorText(err))
		return &CommandExit{Code: 1}
	}
	for _, w := range warnings {
		console.Print("[yellow]Warning:[/yellow] " + w)
	}
	_, err = io.WriteString(out, text)
	return err
}

// Parsed icebath plugins pack command line
type PackArgs struct {
	Action, Source, Name string
	Force, EnabledOnly   bool
}

// Dispatches icebath plugins pack install, export or show
func PackCommand(ctx context.Context, args PackArgs, out, errOut io.Writer) error {
	switch args.Action {
	case "install":
		return CmdPackInstall(ctx, args.Source, args.Force, out)
	case "export":
		name := args.Name
		if name == "" {
			name = defaultPackName
		}
		return CmdPackExport(ctx, args.EnabledOnly, name, out, errOut)
	case "show":
		return CmdPackShow(ctx, args.Source, out)
	}
	return fail(NewConsole(out), packUsageRefusal)
}
