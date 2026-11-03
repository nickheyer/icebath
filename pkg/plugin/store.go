package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Directories and files of the plugin store under its root
const (
	cacheDir      = "cache"
	dataDir       = "data"
	marketsDir    = "marketplaces"
	knownFile     = "known_marketplaces.json"
	installedFile = "installed_plugins.json"
	flaggedFile   = "flagged-plugins.json"
	orphanMarker  = ".orphaned_at"
	orphanAge     = 14 * 24 * time.Hour
	nodeTimeout   = 60 * time.Second
)

// Scopes an install or an enablement is recorded under
const (
	ScopeUser    = "user"
	ScopeProject = "project"
	ScopeLocal   = "local"
	ScopeManaged = "managed"
)

// Marketplace the person registered, with where its catalog sits
type Known struct {
	Source          MarketSource `json:"source"`
	InstallLocation string       `json:"installLocation"`
	LastUpdated     time.Time    `json:"lastUpdated"`
	AutoUpdate      *bool        `json:"autoUpdate,omitempty"`
}

// One install of a plugin
type Install struct {
	Scope       string    `json:"scope"`
	InstallPath string    `json:"installPath"`
	Version     string    `json:"version"`
	Marketplace string    `json:"marketplace"`
	Entry       string    `json:"entry"`
	Source      Source    `json:"source"`
	InstalledAt time.Time `json:"installedAt"`
	LastUpdated time.Time `json:"lastUpdated"`
	// Loaded in place from the source rather than copied into the cache
	InPlace bool `json:"isLocal,omitempty"`
	// Commit the plugin was taken from, for git sources
	Commit string `json:"gitCommitSha,omitempty"`
}

// Marketplaces registered and plugins installed under one directory
type Store struct {
	Dir string
	// Reports progress of a fetch, nothing when nil
	Progress func(text string)
}

// Store under a configuration directory
func NewStore(configDir string) *Store {
	return &Store{Dir: filepath.Join(configDir, "plugins")}
}

func (s *Store) say(format string, args ...any) {
	if s.Progress != nil {
		s.Progress(fmt.Sprintf(format, args...))
	}
}

// Directory a plugin keeps data in across updates
func (s *Store) DataDir(id string) string { return filepath.Join(s.Dir, dataDir, DataID(id)) }

func readJSON[T any](path string) (T, error) {
	var out T
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("%s: %w", path, err)
	}
	return out, nil
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Marketplaces registered, by name
func (s *Store) Known() (map[string]Known, error) {
	out, err := readJSON[map[string]Known](filepath.Join(s.Dir, knownFile))
	if out == nil {
		out = map[string]Known{}
	}
	return out, err
}

func (s *Store) saveKnown(m map[string]Known) error {
	return writeJSON(filepath.Join(s.Dir, knownFile), m)
}

// Plugins installed, by id
func (s *Store) Installed() (map[string]Install, error) {
	doc, err := readJSON[struct {
		Version int                `json:"version"`
		Plugins map[string]Install `json:"plugins"`
	}](filepath.Join(s.Dir, installedFile))
	if doc.Plugins == nil {
		doc.Plugins = map[string]Install{}
	}
	return doc.Plugins, err
}

func (s *Store) saveInstalled(m map[string]Install) error {
	return writeJSON(filepath.Join(s.Dir, installedFile), map[string]any{"version": 1, "plugins": m})
}

// Registers a marketplace from a source, fetching its catalog, refusing a reserved or taken name
func (s *Store) AddMarketplace(ctx context.Context, src MarketSource, name string) (*Marketplace, error) {
	if err := src.Validate(); err != nil {
		return nil, err
	}
	known, err := s.Known()
	if err != nil {
		return nil, err
	}
	location, m, err := s.fetchMarketplace(ctx, src, name)
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = m.Name
	}
	if ReservedMarket(name, src) {
		return nil, fmt.Errorf("marketplace name %q is reserved", name)
	}
	if have, ok := known[name]; ok && jsonText(have.Source) != jsonText(src) {
		return nil, fmt.Errorf("marketplace %s is already registered from %s", name, have.Source)
	}
	known[name] = Known{Source: src, InstallLocation: location, LastUpdated: time.Now()}
	if err := s.saveKnown(known); err != nil {
		return nil, err
	}
	m.Name = name
	return m, nil
}

// Fetches the catalog of a source into the store, returning where it sits
func (s *Store) fetchMarketplace(ctx context.Context, src MarketSource, name string) (string, *Marketplace, error) {
	switch src.Kind {
	case MarketFile:
		m, err := ReadMarketplace(src.Path)
		return src.Path, m, err
	case MarketDirectory:
		path, err := findCatalog(src.Path)
		if err != nil {
			return "", nil, err
		}
		m, err := ReadMarketplace(path)
		return src.Path, m, err
	case MarketURL:
		s.say("fetching %s", src.URL)
		data, err := download(ctx, src.URL, src.Headers, 8<<20)
		if err != nil {
			if strings.HasPrefix(src.URL, "http://") {
				return "", nil, fmt.Errorf("%s: a marketplace url must be https", src.URL)
			}
			return "", nil, err
		}
		m, err := ParseMarketplace(data)
		if err != nil {
			return "", nil, err
		}
		if name == "" {
			name = m.Name
		}
		dir := filepath.Join(s.Dir, marketsDir, name)
		path := filepath.Join(dir, ".claude-plugin", "marketplace.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", nil, err
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return "", nil, err
		}
		m.File, m.Root = path, dir
		return dir, m, nil
	case MarketGitHub, MarketGit:
		repoURL, folder := src.URL, ""
		if src.Kind == MarketGitHub {
			repoURL = githubURL(src.Repo)
			folder = strings.ReplaceAll(src.Repo, "/", "-")
		} else {
			folder = strings.TrimSuffix(filepath.Base(strings.TrimSuffix(src.URL, "/")), ".git")
		}
		if name != "" {
			folder = name
		}
		dir := filepath.Join(s.Dir, marketsDir, folder)
		s.say("cloning %s", repoURL)
		if _, err := cloneOrUpdate(ctx, repoURL, src.Ref, "", dir, src.SparsePaths); err != nil {
			return "", nil, err
		}
		path := filepath.Join(dir, filepath.FromSlash(src.Path))
		if src.Path == "" {
			var err error
			if path, err = findCatalog(dir); err != nil {
				return "", nil, err
			}
		}
		m, err := ReadMarketplace(path)
		if err != nil {
			return "", nil, err
		}
		m.Root = dir
		return dir, m, nil
	case MarketSettings:
		m := &Marketplace{Name: src.Name, Owner: *src.Owner, Plugins: src.Plugins}
		return "", m, nil
	}
	return "", nil, fmt.Errorf("unknown marketplace source %q", src.Kind)
}

// Catalog file under a marketplace root, the Claude one before the Codex one
func findCatalog(root string) (string, error) {
	for _, file := range MarketplaceFiles {
		path := filepath.Join(root, filepath.FromSlash(file))
		if exists(path) {
			return path, nil
		}
	}
	return "", fmt.Errorf("%s holds no %s", root, strings.Join(MarketplaceFiles, " or "))
}

// Forgets a marketplace, deleting its copy
func (s *Store) RemoveMarketplace(name string) error {
	known, err := s.Known()
	if err != nil {
		return err
	}
	k, ok := known[name]
	if !ok {
		return fmt.Errorf("marketplace %s is not registered", name)
	}
	installed, err := s.Installed()
	if err != nil {
		return err
	}
	for id, in := range installed {
		if in.Marketplace == name {
			return fmt.Errorf("plugin %s is installed from %s, uninstall it first", id, name)
		}
	}
	if k.Source.Kind == MarketGitHub || k.Source.Kind == MarketGit || k.Source.Kind == MarketURL {
		os.RemoveAll(k.InstallLocation)
	}
	delete(known, name)
	return s.saveKnown(known)
}

// Fetches a registered marketplace again
func (s *Store) UpdateMarketplace(ctx context.Context, name string) (*Marketplace, error) {
	known, err := s.Known()
	if err != nil {
		return nil, err
	}
	k, ok := known[name]
	if !ok {
		return nil, fmt.Errorf("marketplace %s is not registered", name)
	}
	location, m, err := s.fetchMarketplace(ctx, k.Source, name)
	if err != nil {
		return nil, err
	}
	k.InstallLocation, k.LastUpdated = location, time.Now()
	known[name] = k
	if err := s.saveKnown(known); err != nil {
		return nil, err
	}
	m.Name = name
	if m.ForceRemove {
		if err := s.flagRemoved(name, m); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Uninstalls plugins the catalog no longer lists, recording them as flagged
func (s *Store) flagRemoved(name string, m *Marketplace) error {
	installed, err := s.Installed()
	if err != nil {
		return err
	}
	flagged, err := readJSON[map[string]time.Time](filepath.Join(s.Dir, flaggedFile))
	if err != nil {
		return err
	}
	if flagged == nil {
		flagged = map[string]time.Time{}
	}
	for id, in := range installed {
		if in.Marketplace != name {
			continue
		}
		if _, ok := m.Entry(in.Entry); ok {
			continue
		}
		if err := s.Uninstall(id, false); err != nil {
			return err
		}
		flagged[id] = time.Now()
	}
	return writeJSON(filepath.Join(s.Dir, flaggedFile), flagged)
}

// Catalog of a registered marketplace as it sits in the store
func (s *Store) Marketplace(name string) (*Marketplace, error) {
	known, err := s.Known()
	if err != nil {
		return nil, err
	}
	k, ok := known[name]
	if !ok {
		return nil, fmt.Errorf("marketplace %s is not registered", name)
	}
	switch k.Source.Kind {
	case MarketSettings:
		return &Marketplace{Name: name, Owner: *k.Source.Owner, Plugins: k.Source.Plugins}, nil
	case MarketFile:
		m, err := ReadMarketplace(k.InstallLocation)
		if err != nil {
			return nil, err
		}
		m.Name = name
		return m, nil
	}
	root := k.InstallLocation
	path := filepath.Join(root, filepath.FromSlash(k.Source.Path))
	if k.Source.Path == "" || k.Source.Kind == MarketDirectory {
		if path, err = findCatalog(root); err != nil {
			return nil, err
		}
	}
	m, err := ReadMarketplace(path)
	if err != nil {
		return nil, err
	}
	m.Name, m.Root = name, root
	return m, nil
}

// Every registered marketplace with its catalog, by name order, a catalog that fails to read reported in its place
func (s *Store) Marketplaces() ([]*Marketplace, map[string]error, error) {
	known, err := s.Known()
	if err != nil {
		return nil, nil, err
	}
	names := make([]string, 0, len(known))
	for name := range known {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []*Marketplace
	errs := map[string]error{}
	for _, name := range names {
		m, err := s.Marketplace(name)
		if err != nil {
			errs[name] = err
			continue
		}
		out = append(out, m)
	}
	return out, errs, nil
}

// Splits name@marketplace into its parts
func SplitID(id string) (string, string) {
	name, market, _ := strings.Cut(id, "@")
	return name, market
}

// Finds the entry a name points at, across every marketplace when none is named, refusing an ambiguous one
func (s *Store) Resolve(spec string) (*Marketplace, Entry, error) {
	name, market := SplitID(spec)
	if market != "" {
		m, err := s.Marketplace(market)
		if err != nil {
			return nil, Entry{}, err
		}
		e, ok := m.Entry(name)
		if !ok {
			return nil, Entry{}, fmt.Errorf("marketplace %s offers no plugin %s, it has %s", market, name, strings.Join(m.Names(), ", "))
		}
		return m, e, nil
	}
	markets, _, err := s.Marketplaces()
	if err != nil {
		return nil, Entry{}, err
	}
	var found []*Marketplace
	var entry Entry
	for _, m := range markets {
		if e, ok := m.Entry(name); ok {
			found = append(found, m)
			entry = e
		}
	}
	switch len(found) {
	case 0:
		return nil, Entry{}, fmt.Errorf("no registered marketplace offers %s", name)
	case 1:
		return found[0], entry, nil
	}
	var names []string
	for _, m := range found {
		names = append(names, name+"@"+m.Name)
	}
	return nil, Entry{}, fmt.Errorf("%s is offered by several marketplaces, pick one of %s", name, strings.Join(names, ", "))
}

// Installs a plugin from a marketplace under the scope, fetching its source into the cache, and its dependencies
func (s *Store) Install(ctx context.Context, spec, scope string) (string, Install, error) {
	m, e, err := s.Resolve(spec)
	if err != nil {
		return "", Install{}, err
	}
	return s.installEntry(ctx, m, e, scope, map[string]bool{})
}

func (s *Store) installEntry(ctx context.Context, m *Marketplace, e Entry, scope string, seen map[string]bool) (string, Install, error) {
	id := e.Name + "@" + m.Name
	if seen[id] {
		return id, Install{}, nil
	}
	seen[id] = true
	installed, err := s.Installed()
	if err != nil {
		return id, Install{}, err
	}
	in, err := s.fetch(ctx, m, e)
	if err != nil {
		return id, Install{}, fmt.Errorf("install %s: %w", id, err)
	}
	in.Scope = scope
	if old, ok := installed[id]; ok {
		in.InstalledAt = old.InstalledAt
		if old.InstallPath != in.InstallPath && !old.InPlace {
			s.orphan(old.InstallPath)
		}
	}
	installed[id] = in
	if err := s.saveInstalled(installed); err != nil {
		return id, in, err
	}
	deps, err := s.dependencies(m, e, in.InstallPath)
	if err != nil {
		return id, in, err
	}
	for _, d := range deps {
		dm := m
		if d.Marketplace != "" && d.Marketplace != m.Name {
			allowed := false
			for _, x := range m.CrossDeps {
				allowed = allowed || x == d.Marketplace
			}
			if !allowed {
				return id, in, fmt.Errorf("install %s: dependency %s@%s is on a marketplace %s does not allow", id, d.Name, d.Marketplace, m.Name)
			}
			if dm, err = s.Marketplace(d.Marketplace); err != nil {
				return id, in, fmt.Errorf("install %s: dependency %s: %w", id, d.Name, err)
			}
		}
		de, ok := dm.Entry(d.Name)
		if !ok {
			return id, in, fmt.Errorf("install %s: dependency %s is not offered by %s", id, d.Name, dm.Name)
		}
		if _, _, err := s.installEntry(ctx, dm, de, scope, seen); err != nil {
			return id, in, err
		}
	}
	return id, in, nil
}

// Dependencies of an installed plugin, from its manifest and its entry
func (s *Store) dependencies(m *Marketplace, e Entry, root string) ([]Dependency, error) {
	manifest, err := ReadManifest(root)
	if err != nil {
		return nil, err
	}
	deps := append([]Dependency(nil), manifest.Dependencies...)
	deps = append(deps, e.Dependencies...)
	return deps, nil
}

// Fetches the source of an entry into the cache, or notes where it sits when loaded in place
func (s *Store) fetch(ctx context.Context, m *Marketplace, e Entry) (Install, error) {
	src := e.Source
	in := Install{Marketplace: m.Name, Entry: e.Name, Source: src, InstalledAt: time.Now(), LastUpdated: time.Now()}
	known, _ := s.Known()
	k := known[m.Name]
	headers := map[string]string{}
	for key, v := range k.Source.Headers {
		headers[key] = v
	}
	for key, v := range e.Headers {
		headers[key] = v
	}
	switch src.Kind {
	case SourceRelative, SourceLocal:
		if !k.Source.HasRoot() && k.Source.Kind != "" {
			return in, fmt.Errorf("a relative source cannot resolve under a %s marketplace", k.Source.Kind)
		}
		if m.Root == "" {
			return in, errors.New("the marketplace has no root for a relative source")
		}
		root, err := inside(m.Root, src.Path)
		if err != nil {
			return in, err
		}
		if k.Source.Kind == MarketFile || k.Source.Kind == MarketDirectory {
			in.InstallPath, in.InPlace = root, true
			in.Version = s.version(ctx, root, e, m.Root, src.Path)
			return in, nil
		}
		in.Commit = commitOf(ctx, m.Root, src.Path)
		in.Version = s.version(ctx, root, e, m.Root, src.Path)
		dst := filepath.Join(s.Dir, cacheDir, m.Name, e.Name, in.Version)
		s.say("copying %s", root)
		if err := copyTree(root, dst); err != nil {
			return in, err
		}
		in.InstallPath = dst
	case SourceGitHub, SourceURL, SourceGitSub:
		repoURL := src.URL
		if src.Kind == SourceGitHub {
			repoURL = githubURL(src.Repo)
		} else if src.Kind == SourceGitSub && githubRepo.MatchString(src.URL) {
			repoURL = githubURL(src.URL)
		}
		tmp := filepath.Join(s.Dir, cacheDir, ".fetch", m.Name, e.Name)
		var sparse []string
		if src.Kind == SourceGitSub {
			sparse = []string{strings.TrimPrefix(src.Path, "./")}
		}
		s.say("cloning %s", repoURL)
		commit, err := cloneOrUpdate(ctx, repoURL, src.Ref, src.SHA, tmp, sparse)
		if err != nil {
			return in, err
		}
		root := tmp
		if src.Kind == SourceGitSub {
			if root, err = inside(tmp, "./"+strings.TrimPrefix(src.Path, "./")); err != nil {
				return in, err
			}
		}
		in.Commit = commit
		in.Version = s.version(ctx, root, e, "", "")
		if in.Version == "" {
			in.Version = commit[:12]
			if src.Kind == SourceGitSub {
				in.Version += "-" + shortHash(src.Path)
			}
		}
		dst := filepath.Join(s.Dir, cacheDir, m.Name, e.Name, in.Version)
		if err := copyTree(root, dst); err != nil {
			return in, err
		}
		in.InstallPath = dst
	case SourceNPM:
		tmp := filepath.Join(s.Dir, cacheDir, ".fetch", m.Name, e.Name)
		s.say("fetching npm package %s", src.Package)
		root, version, err := fetchNPM(ctx, src, tmp)
		if err != nil {
			return in, err
		}
		in.Version = s.version(ctx, root, e, "", "")
		if in.Version == "" {
			in.Version = version
		}
		if in.Version == "" {
			in.Version = "unknown"
		}
		dst := filepath.Join(s.Dir, cacheDir, m.Name, e.Name, in.Version)
		if err := copyTree(root, dst); err != nil {
			return in, err
		}
		in.InstallPath = dst
	case SourceArchive:
		tmp := filepath.Join(s.Dir, cacheDir, ".fetch", m.Name, e.Name)
		s.say("downloading %s", src.URL)
		if e.HeadersHelper != "" {
			extra, err := helperHeaders(ctx, e.HeadersHelper)
			if err != nil {
				return in, err
			}
			for key, v := range extra {
				headers[key] = v
			}
		}
		root, err := fetchArchive(ctx, src, tmp, headers)
		if err != nil {
			return in, err
		}
		in.Version = s.version(ctx, root, e, "", "")
		if in.Version == "" {
			in.Version = strings.ToLower(src.SHA256[:12])
		}
		dst := filepath.Join(s.Dir, cacheDir, m.Name, e.Name, in.Version)
		if err := copyTree(root, dst); err != nil {
			return in, err
		}
		in.InstallPath = dst
	case SourceCommand:
		s.say("running the command source of %s", e.Name)
		root, err := runCommandSource(ctx, src)
		if err != nil {
			return in, err
		}
		manifest, err := ReadManifest(root)
		if err != nil {
			return in, err
		}
		hash := shortHash(root + "\x00" + manifest.Version + "\x00" + time.Now().Format(time.RFC3339Nano))
		in.Version = hash
		if manifest.Version != "" {
			in.Version = manifest.Version + "-" + hash
		}
		if src.Mode == "link" {
			in.InstallPath, in.InPlace = root, true
			return in, nil
		}
		dst := filepath.Join(s.Dir, cacheDir, m.Name, e.Name, in.Version)
		if err := copyTree(root, dst); err != nil {
			return in, err
		}
		in.InstallPath = dst
	default:
		return in, fmt.Errorf("unknown source kind %q", src.Kind)
	}
	if err := installNode(ctx, in.InstallPath, s.say); err != nil {
		return in, err
	}
	return in, nil
}

// Version of a plugin, the manifest version, the entry version, the commit of its directory, else unknown
func (s *Store) version(ctx context.Context, root string, e Entry, marketRoot, rel string) string {
	if m, err := ReadManifest(root); err == nil && m.Version != "" {
		return m.Version
	}
	if e.Version != "" {
		return e.Version
	}
	if marketRoot != "" {
		if c := commitOf(ctx, marketRoot, strings.TrimPrefix(rel, "./")); c != "" {
			return c[:12]
		}
		return "unknown"
	}
	return ""
}

// Runs a headers helper and reads the JSON object it prints
func helperHeaders(ctx context.Context, command string) (map[string]string, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "sh", "-c", command).Output()
	if err != nil {
		return nil, fmt.Errorf("headersHelper: %w", err)
	}
	var headers map[string]string
	if err := json.Unmarshal(out, &headers); err != nil {
		return nil, fmt.Errorf("headersHelper must print one JSON object of headers: %w", err)
	}
	return headers, nil
}

// Installs the Node dependencies of a copied plugin when it ships a package.json with a supported lockfile, scripts never run
func installNode(ctx context.Context, root string, say func(string, ...any)) error {
	if !exists(filepath.Join(root, "package.json")) {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return err
	}
	var pkg struct {
		Overrides           map[string]any `json:"overrides"`
		PatchedDependencies map[string]any `json:"patchedDependencies"`
	}
	json.Unmarshal(data, &pkg)
	if len(pkg.Overrides) > 0 || len(pkg.PatchedDependencies) > 0 {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, nodeTimeout)
	defer cancel()
	var cmd *exec.Cmd
	switch {
	case exists(filepath.Join(root, "bun.lock")):
		if _, err := exec.LookPath("bun"); err != nil {
			return nil
		}
		cmd = exec.CommandContext(cctx, "bun", "install", "--frozen-lockfile", "--ignore-scripts", "--production")
	case exists(filepath.Join(root, "npm-shrinkwrap.json")) || exists(filepath.Join(root, "package-lock.json")):
		if _, err := exec.LookPath("npm"); err != nil {
			return nil
		}
		cmd = exec.CommandContext(cctx, "npm", "ci", "--ignore-scripts", "--omit=dev", "--no-audit", "--no-fund")
	default:
		return nil
	}
	say("installing node dependencies of %s", filepath.Base(root))
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "npm_config_ignore_scripts=true")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("node dependencies of %s: %v: %s", root, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Marks a cached version directory for deletion later
func (s *Store) orphan(path string) {
	if path == "" || !strings.HasPrefix(path, filepath.Join(s.Dir, cacheDir)) {
		return
	}
	os.WriteFile(filepath.Join(path, orphanMarker), []byte(time.Now().UTC().Format(time.RFC3339)), 0o644)
}

// Deletes cached version directories orphaned longer than the age, while any install record exists
func (s *Store) Sweep() error {
	installed, err := s.Installed()
	if err != nil || len(installed) == 0 {
		return err
	}
	return filepath.WalkDir(filepath.Join(s.Dir, cacheDir), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.SkipAll
			}
			return nil
		}
		if d.IsDir() || d.Name() != orphanMarker {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
		if err != nil || time.Since(at) < orphanAge {
			return nil
		}
		os.RemoveAll(filepath.Dir(path))
		return filepath.SkipDir
	})
}

// Removes an install, its cached copy and its data unless kept
func (s *Store) Uninstall(id string, keepData bool) error {
	installed, err := s.Installed()
	if err != nil {
		return err
	}
	in, ok := installed[id]
	if !ok {
		return fmt.Errorf("plugin %s is not installed", id)
	}
	if !in.InPlace {
		s.orphan(in.InstallPath)
	}
	delete(installed, id)
	if err := s.saveInstalled(installed); err != nil {
		return err
	}
	if !keepData {
		os.RemoveAll(s.DataDir(id))
	}
	return nil
}

// Fetches an installed plugin again from its marketplace
func (s *Store) Update(ctx context.Context, id string) (Install, bool, error) {
	installed, err := s.Installed()
	if err != nil {
		return Install{}, false, err
	}
	in, ok := installed[id]
	if !ok {
		return Install{}, false, fmt.Errorf("plugin %s is not installed", id)
	}
	m, err := s.Marketplace(in.Marketplace)
	if err != nil {
		return Install{}, false, err
	}
	e, ok := m.Entry(in.Entry)
	if !ok {
		return Install{}, false, fmt.Errorf("marketplace %s no longer offers %s", in.Marketplace, in.Entry)
	}
	_, fresh, err := s.installEntry(ctx, m, e, in.Scope, map[string]bool{})
	if err != nil {
		return Install{}, false, err
	}
	return fresh, fresh.Version != in.Version || fresh.Commit != in.Commit, nil
}

// Plugins no installed plugin depends on that were installed only as dependencies, by id
func (s *Store) Prunable() ([]string, error) {
	installed, err := s.Installed()
	if err != nil {
		return nil, err
	}
	needed := map[string]bool{}
	for id, in := range installed {
		m, err := ReadManifest(in.InstallPath)
		if err != nil {
			continue
		}
		for _, d := range m.Dependencies {
			market := d.Marketplace
			if market == "" {
				_, market = SplitID(id)
			}
			needed[d.Name+"@"+market] = true
		}
	}
	var out []string
	for id := range installed {
		if !needed[id] && installed[id].Scope == "dependency" {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Loads every installed plugin under its id, with the options and enablement given
func (s *Store) Load(enabled map[string]bool, options map[string]map[string]string, projectDir string) ([]*Plugin, map[string]error, error) {
	installed, err := s.Installed()
	if err != nil {
		return nil, nil, err
	}
	ids := make([]string, 0, len(installed))
	for id := range installed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []*Plugin
	errs := map[string]error{}
	catalogs := map[string]*Marketplace{}
	for _, id := range ids {
		in := installed[id]
		p, err := Load(in.InstallPath, LoadOptions{ID: id, Entry: in.Entry, Marketplace: in.Marketplace, Data: s.DataDir(id), From: FromMarketplace, Options: options[id], ProjectDir: projectDir, Entry_: s.entryOf(in, catalogs)})
		if err != nil {
			errs[id] = err
			continue
		}
		p.Version = in.Version
		if on, set := enabled[id]; set {
			p.Enabled = on
		} else {
			p.Enabled = p.DefaultEnabled()
		}
		out = append(out, p)
	}
	return out, errs, nil
}

// Marketplace entry an install came from, nil when the catalog no longer offers it
func (s *Store) entryOf(in Install, catalogs map[string]*Marketplace) *Entry {
	m, ok := catalogs[in.Marketplace]
	if !ok {
		var err error
		if m, err = s.Marketplace(in.Marketplace); err != nil {
			catalogs[in.Marketplace] = nil
			return nil
		}
		catalogs[in.Marketplace] = m
	}
	if m == nil {
		return nil
	}
	e, ok := m.Entry(in.Entry)
	if !ok {
		return nil
	}
	return &e
}

// Entry an install came from, read from its marketplace
func (s *Store) EntryOf(in Install) *Entry {
	return s.entryOf(in, map[string]*Marketplace{})
}

// Creates the data directory of an installed plugin
func (s *Store) EnsureData(id string) error {
	return os.MkdirAll(s.DataDir(id), 0o755)
}

func shortHash(text string) string {
	sum := sha256Sum(text)
	return sum[:12]
}

// File under the store holding sensitive option values, readable by the owner alone
const secretsFile = "secrets.json"

// Sensitive option values of every plugin, by id and key
func (s *Store) Secrets() (map[string]map[string]string, error) {
	out, err := readJSON[map[string]map[string]string](filepath.Join(s.Dir, secretsFile))
	if out == nil {
		out = map[string]map[string]string{}
	}
	return out, err
}

// Writes sensitive option values of a plugin into the secrets file, an empty value removing the key
func (s *Store) SaveSecrets(id string, values map[string]string) error {
	all, err := s.Secrets()
	if err != nil {
		return err
	}
	if all[id] == nil {
		all[id] = map[string]string{}
	}
	for k, v := range values {
		if v == "" {
			delete(all[id], k)
			continue
		}
		all[id][k] = v
	}
	if len(all[id]) == 0 {
		delete(all, id)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.Dir, secretsFile)
	if err := os.WriteFile(path+".tmp", append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// Option values with the secrets laid over the plain ones
func (s *Store) OptionsWithSecrets(plain map[string]map[string]string) (map[string]map[string]string, error) {
	secrets, err := s.Secrets()
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]string, len(plain)+len(secrets))
	for id, values := range plain {
		copied := make(map[string]string, len(values))
		for k, v := range values {
			copied[k] = v
		}
		out[id] = copied
	}
	for id, values := range secrets {
		if out[id] == nil {
			out[id] = map[string]string{}
		}
		for k, v := range values {
			out[id][k] = v
		}
	}
	return out, nil
}
