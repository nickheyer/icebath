package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Files a marketplace catalog is read from, in the order they are tried under a root
var MarketplaceFiles = []string{".claude-plugin/marketplace.json", ".agents/plugins/marketplace.json"}

// Kinds of plugin source a catalog entry may name
const (
	SourceRelative = "relative"
	SourceLocal    = "local"
	SourceGitHub   = "github"
	SourceURL      = "url"
	SourceGitSub   = "git-subdir"
	SourceNPM      = "npm"
	SourceArchive  = "archive"
	SourceCommand  = "command"
)

// Kinds of marketplace source
const (
	MarketURL       = "url"
	MarketGitHub    = "github"
	MarketGit       = "git"
	MarketFile      = "file"
	MarketDirectory = "directory"
	MarketSettings  = "settings"
)

// Where a plugin comes from
type Source struct {
	Kind string `json:"source"`
	// Relative path of a plugin inside the marketplace, or the path of a local one
	Path string `json:"path,omitempty"`
	Repo string `json:"repo,omitempty"`
	URL  string `json:"url,omitempty"`
	Ref  string `json:"ref,omitempty"`
	SHA  string `json:"sha,omitempty"`
	// Package, version and registry of an npm source
	Package  string `json:"package,omitempty"`
	Version  string `json:"version,omitempty"`
	Registry string `json:"registry,omitempty"`
	// Digest of an archive
	SHA256 string `json:"sha256,omitempty"`
	// Command printing the plugin directory, with its timeout in seconds and copy or link mode
	Command string `json:"command,omitempty"`
	Timeout int    `json:"timeout,omitempty"`
	Mode    string `json:"mode,omitempty"`
	// Paths checked out of a git marketplace, every path when empty
	SparsePaths []string `json:"sparsePaths,omitempty"`
}

// Reads a source given as a path string or as an object
func (s *Source) UnmarshalJSON(data []byte) error {
	var path string
	if err := json.Unmarshal(data, &path); err == nil {
		*s = Source{Kind: SourceRelative, Path: path}
		return nil
	}
	type plain Source
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*s = Source(p)
	if s.Kind == "" {
		switch {
		case s.Repo != "":
			s.Kind = SourceGitHub
		case s.URL != "" && s.Path != "":
			s.Kind = SourceGitSub
		case s.URL != "":
			s.Kind = SourceURL
		case s.Package != "":
			s.Kind = SourceNPM
		case s.Command != "":
			s.Kind = SourceCommand
		case s.Path != "":
			s.Kind = SourceRelative
		}
	}
	return nil
}

// Writes a relative source as its path and every other as an object
func (s Source) MarshalJSON() ([]byte, error) {
	if s.Kind == SourceRelative {
		return json.Marshal(s.Path)
	}
	type plain Source
	return json.Marshal(plain(s))
}

// Checks the fields of a plugin source
func (s Source) Validate() error {
	switch s.Kind {
	case SourceRelative, SourceLocal:
		if s.Path == "" {
			return errors.New("a path is required")
		}
		if strings.Contains(s.Path, "..") {
			return fmt.Errorf("path %q must not contain ..", s.Path)
		}
	case SourceGitHub:
		if !githubRepo.MatchString(s.Repo) {
			return fmt.Errorf("repo %q is not owner/repo", s.Repo)
		}
	case SourceURL:
		if !gitURL(s.URL) {
			return fmt.Errorf("url %q is not a git url", s.URL)
		}
	case SourceGitSub:
		if s.URL == "" || s.Path == "" {
			return errors.New("git-subdir needs a url and a path")
		}
	case SourceNPM:
		if s.Package == "" {
			return errors.New("npm needs a package")
		}
		if s.Registry != "" && !strings.HasPrefix(s.Registry, "https://") {
			return fmt.Errorf("registry %q must be https", s.Registry)
		}
	case SourceArchive:
		if !strings.HasPrefix(s.URL, "https://") {
			return fmt.Errorf("archive url %q must be https", s.URL)
		}
		if !sha256Hex.MatchString(s.SHA256) {
			return errors.New("archive needs a 64 character sha256")
		}
	case SourceCommand:
		if s.Command == "" || len(s.Command) > 500 || strings.Contains(s.Command, "    ") {
			return errors.New("command must be printable text of at most 500 characters")
		}
		for _, r := range s.Command {
			if r < 0x20 || r > 0x7e {
				return errors.New("command must be printable ascii")
			}
		}
		if s.Timeout < 0 || s.Timeout > 600 {
			return errors.New("command timeout is 1 to 600 seconds")
		}
		if s.Mode != "" && s.Mode != "copy" && s.Mode != "link" {
			return fmt.Errorf("command mode %q is not copy or link", s.Mode)
		}
	default:
		return fmt.Errorf("unknown source kind %q", s.Kind)
	}
	if s.SHA != "" && !commitSHA.MatchString(s.SHA) {
		return errors.New("sha must be a full 40 character lower case commit")
	}
	return nil
}

var (
	githubRepo = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	sha256Hex  = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
	commitSHA  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	marketName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	entryName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// Reports whether a text is a git url
func gitURL(u string) bool {
	return strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "file://") || strings.HasPrefix(u, "git@") || strings.HasPrefix(u, "ssh://")
}

// One plugin a catalog offers, with any manifest fields it declares over the plugin's own
type Entry struct {
	Name           string            `json:"name"`
	Source         Source            `json:"source"`
	Description    string            `json:"description,omitempty"`
	Version        string            `json:"version,omitempty"`
	Category       string            `json:"category,omitempty"`
	Tags           []string          `json:"tags,omitempty"`
	StrictValue    *bool             `json:"strict,omitempty"`
	DefaultEnabled *bool             `json:"defaultEnabled,omitempty"`
	DisplayName    string            `json:"displayName,omitempty"`
	Author         *Author           `json:"author,omitempty"`
	Homepage       string            `json:"homepage,omitempty"`
	Repository     string            `json:"repository,omitempty"`
	License        string            `json:"license,omitempty"`
	Keywords       []string          `json:"keywords,omitempty"`
	Dependencies   []Dependency      `json:"-"`
	RawDeps        []json.RawMessage `json:"dependencies,omitempty"`
	Skills         []string          `json:"-"`
	RawSkills      json.RawMessage   `json:"skills,omitempty"`
	Commands       []string          `json:"-"`
	RawCommands    json.RawMessage   `json:"commands,omitempty"`
	Agents         []string          `json:"-"`
	RawAgents      json.RawMessage   `json:"agents,omitempty"`
	OutputStyles   []string          `json:"-"`
	RawStyles      json.RawMessage   `json:"outputStyles,omitempty"`
	Hooks          json.RawMessage   `json:"hooks,omitempty"`
	HookPaths      []string          `json:"-"`
	MCPServers     json.RawMessage   `json:"mcpServers,omitempty"`
	UserConfig     map[string]Option `json:"userConfig,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	HeadersHelper  string            `json:"headersHelper,omitempty"`
	// Codex policy, installation and authentication
	Policy map[string]json.RawMessage `json:"policy,omitempty"`
	// Codex display settings of the entry
	Interface *Interface `json:"interface,omitempty"`
}

// Reports whether the plugin manifest is the authority, the default
func (e Entry) Strict() bool { return e.StrictValue == nil || *e.StrictValue }

// Reports whether the entry declares components of its own
func (e Entry) hasComponents() bool {
	return len(e.Skills) > 0 || len(e.Commands) > 0 || len(e.Agents) > 0 || len(e.OutputStyles) > 0 || len(e.Hooks) > 0 || len(e.HookPaths) > 0 || len(e.MCPServers) > 0 || len(e.UserConfig) > 0
}

// Reads the list fields of the entry after the JSON came in
func (e *Entry) finish() error {
	var err error
	if e.Skills, err = stringList(e.RawSkills); err != nil {
		return fmt.Errorf("skills: %w", err)
	}
	if e.Commands, err = stringList(e.RawCommands); err != nil {
		return fmt.Errorf("commands: %w", err)
	}
	if e.Agents, err = stringList(e.RawAgents); err != nil {
		return fmt.Errorf("agents: %w", err)
	}
	if e.OutputStyles, err = stringList(e.RawStyles); err != nil {
		return fmt.Errorf("outputStyles: %w", err)
	}
	for _, raw := range e.RawDeps {
		d, err := parseDependency(raw)
		if err != nil {
			return err
		}
		e.Dependencies = append(e.Dependencies, d)
	}
	if len(e.Hooks) > 0 && e.Hooks[0] != '{' {
		paths, err := stringList(e.Hooks)
		if err != nil {
			return errors.New("hooks: expected an event map, a path or a list of paths")
		}
		e.HookPaths, e.Hooks = paths, nil
	}
	if e.HeadersHelper != "" && e.Strict() {
		return errors.New("headersHelper requires \"strict\": false")
	}
	return nil
}

// Checks the name and source of the entry
func (e Entry) Validate() error {
	if !entryName.MatchString(e.Name) {
		return fmt.Errorf("entry name %q must be letters, digits, dots, underscores and dashes starting alphanumeric", e.Name)
	}
	if err := e.Source.Validate(); err != nil {
		return fmt.Errorf("entry %s: %w", e.Name, err)
	}
	return nil
}

// Manifest an entry stands in for when the plugin has none, every field of the entry applying
func (e Entry) AsManifest(fallback string) Manifest {
	m := Manifest{Format: FormatBare, Name: e.Name, DisplayName: e.DisplayName, Version: e.Version, Description: e.Description, Homepage: e.Homepage, Repository: e.Repository, License: e.License, Keywords: e.Keywords, DefaultEnabled: e.DefaultEnabled, Dependencies: e.Dependencies, UserConfig: e.UserConfig}
	if m.Name == "" {
		m.Name = fallback
	}
	if e.Author != nil {
		m.Author = *e.Author
	}
	m.Skills = e.Skills
	if len(e.Commands) > 0 {
		m.Commands, m.CommandsSet = e.Commands, true
	}
	if len(e.Agents) > 0 {
		m.Agents, m.AgentsSet = e.Agents, true
	}
	if len(e.OutputStyles) > 0 {
		m.Styles, m.StylesSet = e.OutputStyles, true
	}
	if len(e.Hooks) > 0 {
		m.HookInline = []json.RawMessage{e.Hooks}
	}
	m.HookFiles = append(m.HookFiles, e.HookPaths...)
	if len(e.MCPServers) > 0 {
		m.ServerInline = []json.RawMessage{e.MCPServers}
	}
	return m
}

// Catalog of plugins and where each comes from
type Marketplace struct {
	Name        string  `json:"name"`
	Owner       Author  `json:"owner"`
	Description string  `json:"description,omitempty"`
	Version     string  `json:"version,omitempty"`
	Plugins     []Entry `json:"plugins"`
	// Base directory of bare name sources
	PluginRoot string `json:"-"`
	// Plugins removed from the catalog are uninstalled
	ForceRemove bool `json:"forceRemoveDeletedPlugins,omitempty"`
	// Marketplaces whose plugins may be installed as dependencies
	CrossDeps []string `json:"allowCrossMarketplaceDependenciesOn,omitempty"`
	// Old names of plugins mapped to new ones, or to nothing for a removed plugin
	Renames map[string]*string `json:"renames,omitempty"`
	// Codex display settings
	Interface *Interface `json:"interface,omitempty"`
	// File the catalog was read from
	File string `json:"-"`
	// Root every relative source resolves from
	Root string `json:"-"`
	// Entries that did not validate, by name
	Problems []string `json:"-"`
}

// Reads a catalog file, the root being the directory holding its .claude-plugin or .agents/plugins folder
func ReadMarketplace(path string) (*Marketplace, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m, err := ParseMarketplace(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	m.File = path
	m.Root = marketRoot(path)
	return m, nil
}

// Root of a catalog file, two directories up for one under .claude-plugin or .agents/plugins and the directory otherwise
func marketRoot(path string) string {
	dir := filepath.Dir(path)
	switch filepath.Base(dir) {
	case ".claude-plugin":
		return filepath.Dir(dir)
	case "plugins":
		if filepath.Base(filepath.Dir(dir)) == ".agents" {
			return filepath.Dir(filepath.Dir(dir))
		}
	}
	return dir
}

// Parses catalog JSON, each entry checked on its own so one bad entry leaves the rest usable
func ParseMarketplace(data []byte) (*Marketplace, error) {
	var doc struct {
		Marketplace
		Metadata struct {
			Description string `json:"description"`
			Version     string `json:"version"`
			PluginRoot  string `json:"pluginRoot"`
		} `json:"metadata"`
		Plugins []json.RawMessage `json:"plugins"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("marketplace: %w", err)
	}
	m := doc.Marketplace
	m.Plugins = nil
	if !marketName.MatchString(m.Name) || strings.Contains(m.Name, "..") {
		return nil, fmt.Errorf("marketplace name %q must be letters, digits, dots, underscores and dashes starting alphanumeric", m.Name)
	}
	if m.Owner.Name == "" {
		if m.Interface == nil || m.Interface.DisplayName == "" {
			return nil, errors.New("marketplace owner needs a name")
		}
		m.Owner.Name = m.Interface.DisplayName
	}
	if m.Description == "" {
		m.Description = doc.Metadata.Description
	}
	if m.Version == "" {
		m.Version = doc.Metadata.Version
	}
	m.PluginRoot = doc.Metadata.PluginRoot
	for _, raw := range doc.Plugins {
		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil {
			m.Problems = append(m.Problems, "entry does not parse: "+err.Error())
			continue
		}
		if err := e.finish(); err != nil {
			m.Problems = append(m.Problems, fmt.Sprintf("entry %s: %v", e.Name, err))
			continue
		}
		if e.Source.Kind == SourceRelative && !relativePath(e.Source.Path) && !strings.HasPrefix(e.Source.Path, "/") {
			if m.PluginRoot == "" || strings.Contains(e.Source.Path, "/") {
				m.Problems = append(m.Problems, fmt.Sprintf("entry %s: path %q must start with ./, a bare name needs metadata.pluginRoot", e.Name, e.Source.Path))
				continue
			}
			e.Source.Path = "./" + strings.TrimSuffix(m.PluginRoot, "/") + "/" + e.Source.Path
		}
		if err := e.Validate(); err != nil {
			m.Problems = append(m.Problems, err.Error())
			continue
		}
		m.Plugins = append(m.Plugins, e)
	}
	return &m, nil
}

// Entry by name, following renames
func (m *Marketplace) Entry(name string) (Entry, bool) {
	if m.Renames != nil {
		if to, ok := m.Renames[name]; ok {
			if to == nil {
				return Entry{}, false
			}
			name = *to
		}
	}
	for _, e := range m.Plugins {
		if e.Name == name {
			return e, true
		}
	}
	return Entry{}, false
}

// Names every entry, sorted
func (m *Marketplace) Names() []string {
	out := make([]string, 0, len(m.Plugins))
	for _, e := range m.Plugins {
		out = append(out, e.Name)
	}
	sort.Strings(out)
	return out
}

// Where a marketplace catalog comes from
type MarketSource struct {
	Kind string `json:"source"`
	URL  string `json:"url,omitempty"`
	Repo string `json:"repo,omitempty"`
	Ref  string `json:"ref,omitempty"`
	// Path of the catalog inside a repository, or the path of a file or directory source
	Path        string            `json:"path,omitempty"`
	SparsePaths []string          `json:"sparsePaths,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	// Catalog held inline, for a settings source
	Name    string  `json:"name,omitempty"`
	Owner   *Author `json:"owner,omitempty"`
	Plugins []Entry `json:"plugins,omitempty"`
}

// Reads the text a person gives marketplace add into a source
func ParseMarketSource(text string) (MarketSource, error) {
	text = strings.TrimSpace(text)
	switch {
	case text == "":
		return MarketSource{}, errors.New("a marketplace source is required")
	case strings.HasPrefix(text, "./") || strings.HasPrefix(text, "../") || strings.HasPrefix(text, "/") || strings.HasPrefix(text, "~/") || text == ".":
		path, err := filepath.Abs(expandHome(text))
		if err != nil {
			return MarketSource{}, err
		}
		if strings.HasSuffix(path, ".json") {
			return MarketSource{Kind: MarketFile, Path: path}, nil
		}
		return MarketSource{Kind: MarketDirectory, Path: path}, nil
	case strings.HasPrefix(text, "git@") || strings.HasPrefix(text, "ssh://"):
		url, ref, _ := strings.Cut(text, "#")
		return MarketSource{Kind: MarketGit, URL: url, Ref: ref}, nil
	case strings.HasPrefix(text, "http://") || strings.HasPrefix(text, "https://"):
		url, ref, _ := strings.Cut(text, "#")
		switch {
		case strings.HasSuffix(url, ".git") || strings.Contains(url, "/_git/"):
			return MarketSource{Kind: MarketGit, URL: url, Ref: ref}, nil
		case strings.HasPrefix(url, "https://github.com/") || strings.HasPrefix(url, "https://gitlab.com/"):
			return MarketSource{Kind: MarketGit, URL: strings.TrimSuffix(url, "/") + ".git", Ref: ref}, nil
		}
		return MarketSource{Kind: MarketURL, URL: text}, nil
	}
	repo := text
	ref := ""
	if i := strings.IndexAny(repo, "@#"); i >= 0 {
		repo, ref = repo[:i], repo[i+1:]
	}
	if !githubRepo.MatchString(repo) {
		if isDir(text) {
			path, _ := filepath.Abs(text)
			return MarketSource{Kind: MarketDirectory, Path: path}, nil
		}
		return MarketSource{}, fmt.Errorf("%q is not owner/repo, a git url, an http url or a path", text)
	}
	return MarketSource{Kind: MarketGitHub, Repo: repo, Ref: ref}, nil
}

// Checks the fields of a marketplace source
func (s MarketSource) Validate() error {
	switch s.Kind {
	case MarketURL:
		if !strings.HasPrefix(s.URL, "http://") && !strings.HasPrefix(s.URL, "https://") {
			return fmt.Errorf("marketplace url %q must be http or https", s.URL)
		}
	case MarketGitHub:
		if !githubRepo.MatchString(s.Repo) {
			return fmt.Errorf("marketplace repo %q is not owner/repo", s.Repo)
		}
	case MarketGit:
		if !gitURL(s.URL) {
			return fmt.Errorf("marketplace url %q is not a git url", s.URL)
		}
	case MarketFile, MarketDirectory:
		if s.Path == "" {
			return errors.New("a marketplace path is required")
		}
	case MarketSettings:
		if s.Name == "" || s.Owner == nil || s.Owner.Name == "" {
			return errors.New("a settings marketplace needs a name and an owner")
		}
		for _, e := range s.Plugins {
			if e.Source.Kind == SourceRelative {
				return fmt.Errorf("entry %s: a settings marketplace takes no relative path", e.Name)
			}
		}
	default:
		return fmt.Errorf("unknown marketplace source %q", s.Kind)
	}
	return nil
}

// Reports whether relative plugin sources can resolve against a copy of the marketplace
func (s MarketSource) HasRoot() bool {
	return s.Kind == MarketGitHub || s.Kind == MarketGit || s.Kind == MarketFile || s.Kind == MarketDirectory
}

// Names the terminal shows for the source
func (s MarketSource) String() string {
	switch s.Kind {
	case MarketURL, MarketGit:
		return s.URL
	case MarketGitHub:
		if s.Ref != "" {
			return s.Repo + "@" + s.Ref
		}
		return s.Repo
	case MarketFile, MarketDirectory:
		return s.Path
	case MarketSettings:
		return "settings"
	}
	return s.Kind
}

// Names that cannot be registered as a marketplace, the official and tool ones and their look alikes
var reservedMarkets = []string{
	"claude-code-marketplace", "claude-code-plugins", "claude-plugins-official", "anthropic-marketplace", "anthropic-plugins", "agent-skills", "anthropic-agent-skills", "life-sciences", "knowledge-work-plugins", "claude-for-legal", "claude-for-financial-services", "financial-services-plugins", "first-party-plugins", "claude-tag-plugins",
	"claude-community", "claude-plugins-community", "healthcare", "anthropic-plugin-directory", "claude-plugin-directory",
	"inline", "builtin", "skills-dir", "synced", "claude-plugin-test", "npm", "pip", "uv", "cargo", "github", "gh",
	"constructor", "hasownproperty", "isprototypeof", "propertyisenumerable", "tolocalestring", "tostring", "valueof", "__proto__",
}

// Reports whether a marketplace name is reserved, unless the source is under github.com/anthropics
func ReservedMarket(name string, src MarketSource) bool {
	lower := strings.ToLower(name)
	for _, r := range name {
		if r > 0x7e {
			return true
		}
	}
	if strings.HasPrefix(lower, "claudeai-") {
		return true
	}
	folded := strings.NewReplacer("_", "-", ".", "-").Replace(lower)
	for _, r := range reservedMarkets {
		if folded == strings.NewReplacer("_", "-", ".", "-").Replace(r) {
			official := src.Kind == MarketGitHub && strings.HasPrefix(strings.ToLower(src.Repo), "anthropics/")
			if official && strings.Contains(r, "claude") || official && strings.Contains(r, "anthropic") {
				return false
			}
			return true
		}
	}
	return false
}
