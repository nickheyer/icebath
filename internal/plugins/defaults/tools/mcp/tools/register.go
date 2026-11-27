package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/mcp"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Prefix of every MCP tool name
const Prefix = "mcp__"

// Longest function name providers take, and the length of the hash a longer one ends in
const (
	nameLimit = 64
	hashChars = 8
)

var nameChars = regexp.MustCompile(`[^A-Za-z0-9_]`)

// Name part with every character outside letters, digits and underscore turned into an underscore
func sanitize(value string) string { return nameChars.ReplaceAllString(value, "_") }

var clamped sync.Map

// Name of a server's tool the model calls, mcp__server__tool, a longer one cut to 64 characters ending in a stable hash
func toolName(server, tool string) string {
	full := Prefix + sanitize(server) + "__" + sanitize(tool)
	if len(full) <= nameLimit {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	suffix := "_" + hex.EncodeToString(sum[:])[:hashChars]
	if _, warned := clamped.LoadOrStore(full, true); !warned {
		slog.Warn("MCP tool name is longer than providers take; shortened to a hash suffixed name", "name", full, "length", len(full))
	}
	return full[:nameLimit-len(suffix)] + suffix
}

// Utility tool every server may get for a capability it advertises
type utility struct {
	key, family, description string
	// Name the chat shows and what it lets the agent do, for a person, the server's name in place of %s
	title, about string
	properties   map[string]any
	required     []string
}

var utilities = []utility{
	{key: "list_resources", family: "resources", description: "List available resources from MCP server '%s'", title: "List resources",
		about: "List what the %s server offers to read", properties: map[string]any{}},
	{key: "read_resource", family: "resources", description: "Read a resource by URI from MCP server '%s'", title: "Read resource",
		about:      "Read something that the %s server offers",
		properties: map[string]any{"uri": map[string]any{"type": "string", "description": "URI of the resource to read"}}, required: []string{"uri"}},
	{key: "list_prompts", family: "prompts", description: "List available prompts from MCP server '%s'", title: "List prompts",
		about: "List the ready-made prompts of the %s server", properties: map[string]any{}},
	{key: "get_prompt", family: "prompts", description: "Get a prompt by name from MCP server '%s'", title: "Get prompt",
		about: "Get a ready-made prompt from the %s server", properties: map[string]any{
			"name":      map[string]any{"type": "string", "description": "Name of the prompt to retrieve"},
			"arguments": map[string]any{"type": "object", "description": "Optional arguments to pass to the prompt", "properties": map[string]any{}, "additionalProperties": true},
		}, required: []string{"name"}},
}

// Utility tools a server gets: those its settings leave on and, once it answered the handshake, it advertises
func utilityKeys(s *spec, result *mcp.InitializeResult) []string {
	var keys []string
	for _, u := range utilities {
		on := u.family == "resources" && s.Resources || u.family == "prompts" && s.Prompts
		if on && (result == nil || result.Capabilities == nil || result.Capabilities.Has(u.family)) {
			keys = append(keys, u.key)
		}
	}
	return keys
}

// Prompt injection signs in a tool description, logged and never blocking
var injections = []struct {
	re     *regexp.Regexp
	reason string
}{
	{regexp.MustCompile(`(?i)ignore\s+(all\s+)?previous\s+instructions`), "prompt override attempt ('ignore previous instructions')"},
	{regexp.MustCompile(`(?i)you\s+are\s+now\s+a`), "identity override attempt ('you are now a...')"},
	{regexp.MustCompile(`(?i)your\s+new\s+(task|role|instructions?)\s+(is|are)`), "task override attempt"},
	{regexp.MustCompile(`(?i)system\s*:\s*`), "system prompt injection attempt"},
	{regexp.MustCompile(`(?i)<\s*(system|human|assistant)\s*>`), "role tag injection attempt"},
	{regexp.MustCompile(`(?i)do\s+not\s+(tell|inform|mention|reveal)`), "concealment instruction"},
	{regexp.MustCompile(`(?i)(curl|wget|fetch)\s+https?://`), "network command in description"},
	{regexp.MustCompile(`(?i)base64\.(b64decode|decodebytes)`), "base64 decode reference"},
	{regexp.MustCompile(`(?i)exec\s*\(|eval\s*\(`), "code execution reference"},
	{regexp.MustCompile(`(?i)import\s+(subprocess|os|shutil|socket)`), "dangerous import reference"},
}

var scanned sync.Map

// Logs the injection signs of a tool's description once
func scan(server, tool, description string) {
	if description == "" {
		return
	}
	if _, seen := scanned.LoadOrStore(server+"\x00"+tool+"\x00"+description, true); seen {
		return
	}
	var findings []string
	for _, p := range injections {
		if p.re.MatchString(description) {
			findings = append(findings, p.reason)
		}
	}
	if len(findings) > 0 {
		shown := []rune(description)
		slog.Warn("MCP tool description has suspicious content", "server", server, "tool", tool, "findings", strings.Join(findings, "; "),
			"description", string(shown[:min(len(shown), 200)]))
	}
}

// Unicode tag characters that can hide text, a flag's tag sequence kept
var tagChars = regexp.MustCompile("(\U0001F3F4[\U000E0020-\U000E007E]+\U000E007F)|[\U000E0000-\U000E007F]")

func stripTags(text string) string {
	return tagChars.ReplaceAllStringFunc(text, func(m string) string {
		if strings.HasPrefix(m, "\U0001F3F4") {
			return m
		}
		return ""
	})
}

// One registration a server asks for: a tool of its own or a utility tool
type candidate struct {
	name    string
	origin  string
	utility bool
	def     tools.Def
}

// Tools of every server of the profile, built again when something changed; a name two servers map to goes to the first by name
func (p *pool) definitions() []tools.Def {
	version := p.version.Load()
	p.defsMu.Lock()
	defer p.defsMu.Unlock()
	if p.defs != nil && p.built == version {
		return p.defs
	}
	p.mu.Lock()
	servers := maps.Clone(p.servers)
	p.mu.Unlock()
	defs := []tools.Def{}
	owners := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		for _, def := range p.serverDefs(servers[name]) {
			if owner, taken := owners[def.Name]; taken {
				slog.Error("MCP tool name already taken by another server; skipping it to keep the first owner", "server", name, "tool", def.Name, "owner", owner)
				continue
			}
			owners[def.Name] = name
			defs = append(defs, def)
		}
	}
	p.defs, p.built = defs, version
	return defs
}

// Tools of one server: its own that its filter lets through, then its utility tools, a utility its own tool shadows dropped and a name two
// of its own tools map to skipped altogether
func (p *pool) serverDefs(srv *server) []tools.Def {
	s := srv.currentSpec()
	listed := srv.offered()
	srv.mu.Lock()
	result, state, cachedUtility := srv.result, srv.state, srv.cachedUtility
	srv.mu.Unlock()
	var candidates []candidate
	for _, t := range listed {
		if t == nil || !s.allows(t.Name) {
			continue
		}
		scan(s.Name, t.Name, t.Description)
		candidates = append(candidates, candidate{name: toolName(s.Name, t.Name), origin: fmt.Sprintf("tool %q", t.Name), def: p.toolDef(srv, s, t)})
	}
	keys := cachedUtility
	if state != stateLazy {
		keys = utilityKeys(s, result)
	}
	for _, u := range utilities {
		if slices.Contains(keys, u.key) {
			candidates = append(candidates, candidate{name: toolName(s.Name, u.key), origin: fmt.Sprintf("utility %q", u.key), utility: true, def: p.utilityDef(srv, s, u)})
		}
	}
	byName := map[string][]candidate{}
	for _, c := range candidates {
		byName[c.name] = append(byName[c.name], c)
	}
	var defs []tools.Def
	for _, c := range candidates {
		group := byName[c.name]
		natives := slices.DeleteFunc(slices.Clone(group), func(x candidate) bool { return x.utility })
		switch {
		case len(group) == 1:
			defs = append(defs, c.def)
		case len(natives) == 1 && c.utility:
			slog.Info("MCP generated utility shadowed by the server's own tool of that name; keeping the server's", "server", s.Name, "tool", c.name)
		case len(natives) == 1:
			defs = append(defs, c.def)
		case c.origin == group[0].origin:
			origins := make([]string, len(group))
			for i, x := range group {
				origins[i] = x.origin
			}
			slog.Error("MCP tool names collide once normalized; skipping every one of them rather than picking one", "server", s.Name, "tool", c.name,
				"from", strings.Join(origins, ", "))
		}
	}
	return defs
}

// Tool of a server as the model sees it: its prefixed name, its description without hidden tags, and its input schema made valid for
// every provider
func (p *pool) toolDef(srv *server, s *spec, t *mcp.Tool) tools.Def {
	description := t.Description
	if description == "" {
		description = fmt.Sprintf("MCP tool %s from %s", t.Name, s.Name)
	}
	def := tools.Def{
		Name:    toolName(s.Name, t.Name),
		Toolset: "mcp-" + s.Name,
		Title:   s.Name + " · " + toolTitle(t),
		About:   toolAbout(s, t),
		Group:   s.Name,
		Icon:    tools.IconServer,
		Schema:  map[string]any{"description": stripTags(description), "parameters": normalize(t.InputSchema)},
		Handler: func(ctx context.Context, call *tools.Call) string { return p.call(ctx, call, srv.name, t) },
		Check:   p.check(srv.name),
		// What a server returns is outside content, its calls run alongside others only when its settings say they may, and a call runs as
		// long as the server's own timeout allows
		Untrusted: true,
		Parallel:  s.Parallel,
		Timeout:   p.timeout(s),
	}
	if t.ReadOnly() {
		def.Effect = tools.ReadOnly
	}
	if s.Trust == TrustUntrusted && !t.ReadOnly() {
		def.Risks = []string{trustKey(s.Name, t.Name)}
	}
	return def
}

func (p *pool) utilityDef(srv *server, s *spec, u utility) tools.Def {
	parameters := map[string]any{"type": "object", "properties": u.properties}
	if u.required != nil {
		parameters["required"] = u.required
	}
	return tools.Def{
		Name:    toolName(s.Name, u.key),
		Toolset: "mcp-" + s.Name,
		Title:   s.Name + " · " + u.title,
		About:   fmt.Sprintf(u.about, s.Name),
		Group:   s.Name,
		Icon:    tools.IconServer,
		Subject: strs.First(u.required),
		Schema:  map[string]any{"description": fmt.Sprintf(u.description, s.Name), "parameters": parameters},
		Handler: p.utilityHandler(srv.name, u.key),
		Check:   p.check(srv.name),
		// Listing and reading a server's resources and prompts changes nothing
		Effect:    tools.ReadOnly,
		Untrusted: true,
		Parallel:  s.Parallel,
		Timeout:   p.timeout(s),
	}
}

// Name of a server's tool a person reads: the title the server gives it, else its name with its underscores and dashes as spaces and a
// capital first letter
func toolTitle(t *mcp.Tool) string {
	if label := t.Label(); label != t.Name {
		return label
	}
	text := strings.Join(strings.Fields(strings.NewReplacer("_", " ", "-", " ").Replace(t.Name)), " ")
	if text == "" {
		return t.Name
	}
	runes := []rune(text)
	return strings.ToUpper(string(runes[:1])) + string(runes[1:])
}

// What a server's tool lets the agent do, as the server itself says it: the first line of its description without hidden tags, its
// trailing period dropped; for a tool the server describes not at all, its name and the server's
func toolAbout(s *spec, t *mcp.Tool) string {
	first, _, _ := strings.Cut(strings.TrimSpace(stripTags(t.Description)), "\n")
	if first = strings.TrimSuffix(strings.Join(strings.Fields(first), " "), "."); first != "" {
		return first
	}
	return "Use " + toolTitle(t) + " on the " + s.Name + " server"
}

// Whether a server's tools are offered to a session: one of this profile, the server serving or able to start on use
func (p *pool) check(name string) func(env *tools.Env) bool {
	return func(env *tools.Env) bool {
		if env == nil || env.Config == nil || env.Config.Profile() != p.profile || safe(env.Config) {
			return false
		}
		p.mu.Lock()
		srv := p.servers[name]
		p.mu.Unlock()
		return srv != nil && srv.available()
	}
}

// Approval key of a write capable tool of an untrusted server, under which a yes for the session or always is kept
func trustKey(server, tool string) string {
	return fmt.Sprintf("mcp:%s/%s", server, tool)
}
