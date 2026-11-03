package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Formats a plugin manifest may be written in
const (
	FormatClaude   = "claude"
	FormatCodex    = "codex"
	FormatPortable = "portable"
	FormatBare     = "bare"
)

// Files a manifest is read from, in the order they are tried
const (
	ClaudeManifest    = ".claude-plugin/plugin.json"
	CodexManifest     = ".codex-plugin/plugin.json"
	PortableManifest  = "plugin.json"
	PortableSchema    = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"
	PortableMCPSchema = "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json"
)

// Who wrote a plugin
type Author struct {
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
	URL   string `json:"url,omitempty"`
}

// Plugin another plugin needs enabled
type Dependency struct {
	Name        string `json:"name"`
	Marketplace string `json:"marketplace,omitempty"`
	Version     string `json:"version,omitempty"`
}

// Setting a plugin asks the person for
type Option struct {
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Required    bool     `json:"required,omitempty"`
	Default     any      `json:"default,omitempty"`
	Options     []string `json:"options,omitempty"`
	Multiple    bool     `json:"multiple,omitempty"`
	Sensitive   bool     `json:"sensitive,omitempty"`
	Min         *float64 `json:"min,omitempty"`
	Max         *float64 `json:"max,omitempty"`
}

// Checks the kind, choices and default of an option
func (o Option) Validate(key string) error {
	if !optionKey.MatchString(key) {
		return fmt.Errorf("userConfig key %q must be letters, digits and underscores not starting with a digit", key)
	}
	switch o.Type {
	case "string", "number", "boolean", "directory", "file":
	default:
		return fmt.Errorf("userConfig %s: type %q is not string, number, boolean, directory or file", key, o.Type)
	}
	if o.Title == "" || o.Description == "" {
		return fmt.Errorf("userConfig %s: title and description are required", key)
	}
	if len(o.Options) > 0 {
		if o.Type != "string" || o.Multiple || o.Sensitive {
			return fmt.Errorf("userConfig %s: options need a plain string field", key)
		}
		for _, c := range o.Options {
			if c == "" || len(c) > 64 {
				return fmt.Errorf("userConfig %s: each option is 1 to 64 characters", key)
			}
		}
		if s, ok := o.Default.(string); ok && s != "" && !o.Required {
			found := false
			for _, c := range o.Options {
				found = found || c == s
			}
			if !found {
				return fmt.Errorf("userConfig %s: default %q is not one of the options", key, s)
			}
		}
	}
	return nil
}

// Text of the default of an option, empty when it has none
func (o Option) DefaultText() string {
	switch v := o.Default.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			parts = append(parts, scalar(item))
		}
		return strings.Join(parts, ",")
	}
	return scalar(o.Default)
}

var optionKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Command declared inline in a manifest
type CommandSpec struct {
	Source       string   `json:"source,omitempty"`
	Content      string   `json:"content,omitempty"`
	Description  string   `json:"description,omitempty"`
	ArgumentHint string   `json:"argumentHint,omitempty"`
	Model        string   `json:"model,omitempty"`
	AllowedTools []string `json:"allowedTools,omitempty"`
}

// Background process a plugin runs while enabled
type Monitor struct {
	Name        string `json:"name"`
	Command     string `json:"command"`
	Description string `json:"description"`
	// Always, or on-skill-invoke:<skill>
	When string `json:"when,omitempty"`
}

// How a plugin shows in a directory, from the Codex and portable formats
type Interface struct {
	DisplayName      string   `json:"displayName,omitempty"`
	ShortDescription string   `json:"shortDescription,omitempty"`
	LongDescription  string   `json:"longDescription,omitempty"`
	DeveloperName    string   `json:"developerName,omitempty"`
	Category         string   `json:"category,omitempty"`
	Capabilities     []string `json:"capabilities,omitempty"`
	WebsiteURL       string   `json:"websiteURL,omitempty"`
	SupportURL       string   `json:"supportURL,omitempty"`
	BrandColor       string   `json:"brandColor,omitempty"`
}

// Identity and component declarations of a plugin, read from any of the manifest formats
type Manifest struct {
	Name        string
	DisplayName string
	Version     string
	Description string
	Author      Author
	Homepage    string
	Repository  string
	License     string
	Keywords    []string
	// Starts enabled unless the person says otherwise, true when unset
	DefaultEnabled *bool
	Dependencies   []Dependency
	UserConfig     map[string]Option
	// Skill directories beside the default one
	Skills []string
	// Command paths or inline commands, replacing the default directory when set
	Commands    []string
	CommandMap  map[string]CommandSpec
	CommandsSet bool
	// Agent files, replacing the default directory when set
	Agents    []string
	AgentsSet bool
	// Hook files and inline hook maps, merged with the default file
	HookFiles  []string
	HookInline []json.RawMessage
	// Server files, bundles and inline maps, merged with the default file
	ServerFiles  []string
	ServerInline []json.RawMessage
	// Style paths, replacing the default directory when set
	Styles    []string
	StylesSet bool
	Monitors  []Monitor
	// Agent the main session runs as while the plugin is enabled
	Agent string
	// Skill run once after install, the portable and Codex onboarding skill
	OnboardingSkill string
	Interface       Interface
	// Format the manifest was read in
	Format string
	File   string
	// Fields the manifest set that the format does not know
	Unknown []string
	// Problems found that did not stop the read
	Warnings []string
}

// Reads whichever manifest the directory holds, bare when it holds none
func ReadManifest(root string) (Manifest, error) {
	for _, file := range []string{ClaudeManifest, PortableManifest, CodexManifest} {
		path := filepath.Join(root, file)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return Manifest{}, err
		}
		m, err := ParseManifest(data, path)
		if err != nil {
			return Manifest{}, fmt.Errorf("%s: %w", path, err)
		}
		if m.Format == FormatPortable {
			if overlay, err := codexOverlay(root, data); err != nil {
				return Manifest{}, err
			} else if overlay != nil {
				m.merge(*overlay)
			}
		}
		return m, nil
	}
	return Manifest{Format: FormatBare, Name: filepath.Base(root)}, nil
}

// Codex settings of a portable manifest, the inline extension or the compatibility overlay file
func codexOverlay(root string, data []byte) (*Manifest, error) {
	var doc struct {
		Extensions map[string]json.RawMessage `json:"extensions"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, nil
	}
	if raw, ok := doc.Extensions["com.openai"]; ok && len(raw) > 0 && raw[0] == '{' {
		m, err := parseCodex(raw, filepath.Join(root, PortableManifest))
		if err != nil {
			return nil, err
		}
		return &m, nil
	}
	path := filepath.Join(root, CodexManifest)
	overlay, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	m, err := parseCodex(overlay, path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &m, nil
}

// Takes the hook, interface and onboarding settings of an overlay
func (m *Manifest) merge(o Manifest) {
	if len(o.HookFiles) > 0 || len(o.HookInline) > 0 {
		m.HookFiles = append(m.HookFiles, o.HookFiles...)
		m.HookInline = append(m.HookInline, o.HookInline...)
	}
	if o.OnboardingSkill != "" {
		m.OnboardingSkill = o.OnboardingSkill
	}
	if o.Interface.DisplayName != "" {
		m.Interface = o.Interface
	}
	m.Warnings = append(m.Warnings, o.Warnings...)
}

// Fields the Claude manifest format knows
var claudeKeys = []string{"$schema", "name", "displayName", "version", "description", "author", "homepage", "repository", "license", "keywords", "metadata", "icon", "documentationUrl", "supportUrl", "privacyPolicyUrl", "termsOfServiceUrl", "defaultEnabled", "dependencies", "settings", "userConfig", "types", "channels", "skills", "commands", "agents", "hooks", "mcpServers", "lspServers", "outputStyles", "workflows", "experimental", "themes", "monitors"}

// Fields the Codex manifest format knows
var codexKeys = []string{"$schema", "name", "version", "description", "author", "homepage", "repository", "license", "keywords", "skills", "mcpServers", "apps", "hooks", "interface", "extensions", "requires_local_executor", "capabilities", "composerIcon", "logo"}

// Fields the portable manifest format knows
var portableKeys = []string{"$schema", "name", "version", "description", "author", "homepage", "repository", "license", "keywords", "extensions"}

// Parses manifest JSON, telling the format from the schema and the fields it sets
func ParseManifest(data []byte, path string) (Manifest, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	var schema string
	json.Unmarshal(raw["$schema"], &schema)
	switch {
	case schema == PortableSchema || strings.HasPrefix(schema, "https://agent-plugins.org/"):
		return parsePortable(data, path, schema)
	case strings.Contains(filepath.ToSlash(path), ".codex-plugin/"):
		return parseCodex(data, path)
	case raw["extensions"] != nil && raw["skills"] == nil && raw["commands"] == nil && raw["agents"] == nil && raw["hooks"] == nil:
		return parsePortable(data, path, "")
	}
	return parseClaude(data, path)
}

// Checks the name of a plugin, no spaces, at signs, colons, separators or control characters
func checkName(name string) error {
	if name == "" {
		return errors.New("name is required")
	}
	for _, r := range name {
		switch {
		case r == ' ' || r == '@' || r == ':' || r == '/' || r == '\\':
			return fmt.Errorf("name %q must not contain spaces, @, :, / or \\", name)
		case r < 0x20 || r == 0x7f:
			return fmt.Errorf("name %q holds a control character", name)
		case r == '‪' || r == '‫' || r == '‬' || r == '‭' || r == '‮' || r == '⁦' || r == '⁧' || r == '⁨' || r == '⁩':
			return fmt.Errorf("name %q holds a bidi formatting character", name)
		}
	}
	return nil
}

// Common identity fields of every format
type identity struct {
	Name        string          `json:"name"`
	DisplayName string          `json:"displayName"`
	Version     string          `json:"version"`
	Description string          `json:"description"`
	Author      json.RawMessage `json:"author"`
	Homepage    string          `json:"homepage"`
	Repository  string          `json:"repository"`
	License     string          `json:"license"`
	Keywords    []string        `json:"keywords"`
}

func (m *Manifest) identity(id identity, path string) error {
	m.Name, m.DisplayName, m.Version, m.Description = id.Name, id.DisplayName, id.Version, id.Description
	m.Homepage, m.Repository, m.License, m.Keywords, m.File = id.Homepage, id.Repository, id.License, id.Keywords, path
	if err := checkName(m.Name); err != nil {
		return err
	}
	if m.Homepage != "" {
		if _, err := url.ParseRequestURI(m.Homepage); err != nil {
			return fmt.Errorf("homepage %q is not a url", m.Homepage)
		}
	}
	if len(id.Author) > 0 {
		var a Author
		if err := json.Unmarshal(id.Author, &a); err != nil {
			var name string
			if json.Unmarshal(id.Author, &name) != nil {
				return errors.New("author must be an object with a name")
			}
			a.Name = name
		}
		m.Author = a
	}
	return nil
}

// Parses the Claude format, unknown top level keys stripped with a warning
func parseClaude(data []byte, path string) (Manifest, error) {
	var doc struct {
		identity
		DefaultEnabled *bool                      `json:"defaultEnabled"`
		Dependencies   []json.RawMessage          `json:"dependencies"`
		Settings       map[string]json.RawMessage `json:"settings"`
		UserConfig     map[string]Option          `json:"userConfig"`
		Skills         json.RawMessage            `json:"skills"`
		Commands       json.RawMessage            `json:"commands"`
		Agents         json.RawMessage            `json:"agents"`
		Hooks          json.RawMessage            `json:"hooks"`
		MCPServers     json.RawMessage            `json:"mcpServers"`
		OutputStyles   json.RawMessage            `json:"outputStyles"`
		Experimental   struct {
			Monitors json.RawMessage `json:"monitors"`
		} `json:"experimental"`
		Monitors json.RawMessage `json:"monitors"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	m := Manifest{Format: FormatClaude, DefaultEnabled: doc.DefaultEnabled, UserConfig: doc.UserConfig}
	if err := m.identity(doc.identity, path); err != nil {
		return Manifest{}, err
	}
	for key, opt := range doc.UserConfig {
		if err := opt.Validate(key); err != nil {
			return Manifest{}, err
		}
	}
	for _, raw := range doc.Dependencies {
		d, err := parseDependency(raw)
		if err != nil {
			return Manifest{}, err
		}
		m.Dependencies = append(m.Dependencies, d)
	}
	if raw, ok := doc.Settings["agent"]; ok {
		json.Unmarshal(raw, &m.Agent)
	}
	var err error
	if m.Skills, err = stringList(doc.Skills); err != nil {
		return Manifest{}, fmt.Errorf("skills: %w", err)
	}
	if len(doc.Commands) > 0 {
		m.CommandsSet = true
		if m.Commands, err = stringList(doc.Commands); err != nil {
			var cmds map[string]CommandSpec
			if err := json.Unmarshal(doc.Commands, &cmds); err != nil {
				return Manifest{}, errors.New("commands: expected a path, a list of paths or a map of commands")
			}
			for key, c := range cmds {
				if (c.Source == "") == (c.Content == "") {
					return Manifest{}, fmt.Errorf("commands: %s must set exactly one of source and content", key)
				}
			}
			m.CommandMap = cmds
		}
	}
	if len(doc.Agents) > 0 {
		m.AgentsSet = true
		if m.Agents, err = stringList(doc.Agents); err != nil {
			return Manifest{}, fmt.Errorf("agents: %w", err)
		}
	}
	if m.HookFiles, m.HookInline, err = pathsOrObjects(doc.Hooks); err != nil {
		return Manifest{}, fmt.Errorf("hooks: %w", err)
	}
	if m.ServerFiles, m.ServerInline, err = pathsOrObjects(doc.MCPServers); err != nil {
		return Manifest{}, fmt.Errorf("mcpServers: %w", err)
	}
	if len(doc.OutputStyles) > 0 {
		m.StylesSet = true
		if m.Styles, err = stringList(doc.OutputStyles); err != nil {
			return Manifest{}, fmt.Errorf("outputStyles: %w", err)
		}
	}
	monitors := doc.Experimental.Monitors
	if len(monitors) == 0 && len(doc.Monitors) > 0 {
		monitors = doc.Monitors
		m.Warnings = append(m.Warnings, "monitors sits at the top level, experimental.monitors is its place")
	}
	if len(monitors) > 0 {
		var list []Monitor
		if err := json.Unmarshal(monitors, &list); err != nil {
			var one string
			if json.Unmarshal(monitors, &one) != nil {
				return Manifest{}, errors.New("monitors: expected a path or a list of monitors")
			}
			m.Warnings = append(m.Warnings, "monitors path "+one+" is read with the default file")
		}
		for _, mon := range list {
			if mon.Name == "" || mon.Command == "" || mon.Description == "" {
				return Manifest{}, errors.New("monitors: each needs a name, a command and a description")
			}
			if mon.When != "" && mon.When != "always" && !strings.HasPrefix(mon.When, "on-skill-invoke:") {
				return Manifest{}, fmt.Errorf("monitors: %s when %q is not always or on-skill-invoke:<skill>", mon.Name, mon.When)
			}
		}
		m.Monitors = list
	}
	m.Unknown = unknownKeys(data, claudeKeys)
	for _, k := range m.Unknown {
		m.Warnings = append(m.Warnings, "unknown field "+k+" was dropped")
	}
	return m, nil
}

// Parses the Codex compatibility format, skills and servers by path and the interface at the root
func parseCodex(data []byte, path string) (Manifest, error) {
	var doc struct {
		identity
		Skills     json.RawMessage            `json:"skills"`
		MCPServers json.RawMessage            `json:"mcpServers"`
		Hooks      json.RawMessage            `json:"hooks"`
		Interface  Interface                  `json:"interface"`
		Extensions map[string]json.RawMessage `json:"extensions"`
		Onboarding string                     `json:"onboardingSkill"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	m := Manifest{Format: FormatCodex, Interface: doc.Interface, OnboardingSkill: doc.Onboarding}
	if doc.Name != "" {
		if err := m.identity(doc.identity, path); err != nil {
			return Manifest{}, err
		}
	} else {
		m.File = path
	}
	var err error
	if m.Skills, err = stringList(doc.Skills); err != nil {
		return Manifest{}, fmt.Errorf("skills: %w", err)
	}
	if m.ServerFiles, m.ServerInline, err = pathsOrObjects(doc.MCPServers); err != nil {
		return Manifest{}, fmt.Errorf("mcpServers: %w", err)
	}
	if m.HookFiles, m.HookInline, err = pathsOrObjects(doc.Hooks); err != nil {
		return Manifest{}, fmt.Errorf("hooks: %w", err)
	}
	if raw, ok := doc.Extensions["com.openai"]; ok {
		var ext struct {
			OnboardingSkill string `json:"onboardingSkill"`
		}
		if json.Unmarshal(raw, &ext) == nil && ext.OnboardingSkill != "" {
			m.OnboardingSkill = ext.OnboardingSkill
		}
	}
	m.Unknown = unknownKeys(data, codexKeys)
	return m, nil
}

// Parses the portable format, components at fixed places and the OpenAI extension read as an overlay
func parsePortable(data []byte, path, schema string) (Manifest, error) {
	if schema != "" && schema != PortableSchema {
		return Manifest{}, fmt.Errorf("manifest schema %s is not the supported %s", schema, PortableSchema)
	}
	var doc struct {
		identity
		Extensions json.RawMessage `json:"extensions"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	m := Manifest{Format: FormatPortable}
	if err := m.identity(doc.identity, path); err != nil {
		return Manifest{}, err
	}
	if !portableName.MatchString(m.Name) || strings.Contains(m.Name, "--") || strings.Contains(m.Name, "..") {
		return Manifest{}, fmt.Errorf("name %q must be lower case letters, digits, dashes and dots starting and ending alphanumeric", m.Name)
	}
	if len(doc.Extensions) > 0 && doc.Extensions[0] != '{' {
		m.Warnings = append(m.Warnings, "extensions is not an object and was ignored")
	}
	m.Unknown = unknownKeys(data, portableKeys)
	for _, k := range m.Unknown {
		m.Warnings = append(m.Warnings, "unknown field "+k+" was ignored")
	}
	return m, nil
}

var portableName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$`)

// Reads a dependency given as name, name@marketplace or an object
func parseDependency(raw json.RawMessage) (Dependency, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		name, market, _ := strings.Cut(s, "@")
		if name == "" {
			return Dependency{}, errors.New("dependencies: empty name")
		}
		return Dependency{Name: name, Marketplace: market}, nil
	}
	var d Dependency
	if err := json.Unmarshal(raw, &d); err != nil || d.Name == "" {
		return Dependency{}, errors.New("dependencies: each is a name, name@marketplace or an object with a name")
	}
	return d, nil
}

// Paths and inline objects of a field that takes a path, an object, or a list mixing them
func pathsOrObjects(raw json.RawMessage) ([]string, []json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil, nil
	}
	var items []json.RawMessage
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, nil, err
		}
	} else {
		items = []json.RawMessage{raw}
	}
	var paths []string
	var objects []json.RawMessage
	for _, item := range items {
		switch {
		case len(item) == 0:
		case item[0] == '"':
			var s string
			if err := json.Unmarshal(item, &s); err != nil {
				return nil, nil, err
			}
			paths = append(paths, s)
		case item[0] == '{':
			objects = append(objects, item)
		default:
			return nil, nil, errors.New("expected a path or an object")
		}
	}
	return paths, objects, nil
}

// Top level keys of the JSON that the known list does not name, sorted
func unknownKeys(data []byte, known []string) []string {
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	var out []string
	for k := range raw {
		found := false
		for _, name := range known {
			found = found || name == k
		}
		if !found {
			out = append(out, k)
		}
	}
	sortStrings(out)
	return out
}
