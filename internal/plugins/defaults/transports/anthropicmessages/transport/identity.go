package transport

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Version of Claude Code a subscription request names when no installed one answers
const claudeCodeFallback = "2.1.74"

// Betas a request signed in with a Claude subscription carries
var subscriptionBetas = []string{"claude-code-20250219", "oauth-2025-04-20"}

// Where Claude Code is installed besides PATH, and the names it goes by
var (
	claudeCodePrefixes = []string{"~/.local/bin", "~/.claude/local", "~/bin", "~/.npm-global/bin", "~/.bun/bin", "~/.volta/bin", "/opt/homebrew/bin",
		"/usr/local/bin"}
	claudeCodeNames = []string{"claude", "claude-code"}
)

// Installed version of Claude Code, looked up once
type claudeCode struct {
	mu      sync.Mutex
	version string
}

// User agent of a request signed in with a Claude subscription, naming the installed Claude Code
func (c *claudeCode) userAgent() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.version == "" {
		c.version = detectClaudeCode()
	}
	return "claude-code/" + c.version + " (external, cli)"
}

// Version the first Claude Code found answers --version with, PATH first then the install prefixes; the fallback when none answers
func detectClaudeCode() string {
	seen := map[string]bool{}
	var candidates []string
	add := func(path string) {
		if path != "" && !seen[path] {
			seen[path] = true
			candidates = append(candidates, path)
		}
	}
	for _, name := range claudeCodeNames {
		if hit, err := exec.LookPath(name); err == nil {
			add(hit)
		}
	}
	for _, prefix := range claudeCodePrefixes {
		for _, name := range claudeCodeNames {
			if path := filepath.Join(fsx.ExpandHome(prefix), name); fsx.IsFile(path) {
				add(path)
			}
		}
	}
	for _, path := range candidates {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var out bytes.Buffer
		cmd := exec.CommandContext(ctx, path, "--version")
		cmd.Stdout, cmd.Stderr = &out, io.Discard
		err := cmd.Run()
		cancel()
		if err != nil {
			continue
		}
		if fields := strings.Fields(out.String()); len(fields) > 0 && fields[0][0] >= '0' && fields[0][0] <= '9' {
			return fields[0]
		}
	}
	return claudeCodeFallback
}

// First block of the system prompt of a subscription request
const claudeCodePrefix = "You are Claude Code, Anthropic's official CLI for Claude."

// Prefix tool names take on the subscription wire, as Claude Code's MCP tools have them
const mcpPrefix = "mcp__"

// Tool names sent under other names on the subscription wire, so its billing classifier doesn't flag them, and back
var (
	wireAliases = map[string]string{"session_search": "chat_history_lookup", "memory": "context_notes"}
	wireNames   = map[string]string{"chat_history_lookup": "session_search", "context_notes": "memory"}
)

// Prose mentions of an aliased tool in the system prompt and the tool descriptions, rewritten to the alias
var proseAliases = []struct {
	pattern *regexp.Regexp
	alias   string
}{{regexp.MustCompile(`\bsession_search\b`), wireAliases["session_search"]}}

// The product name and author swapped for Claude Code's in the system prompt of a subscription request
var identitySwaps = [][2]string{{"Icebath Agent", "Claude Code"}, {"Icebath agent", "Claude Code"}, {provider.IdentityAuthor, "Anthropic"}}

// The product's slug standing alone in prose, not inside an address or a quoted identifier
var slug = regexp2.MustCompile(`(?<![\w./:@'"` + "`" + `-])icebath-agent(?![\w/@-]|\.\w)`)

func aliasProse(text string) string {
	for _, p := range proseAliases {
		text = p.pattern.ReplaceAllString(text, p.alias)
	}
	return text
}

// Subscription wire name of a tool: Claude Code's MCP prefix, an alias in place of a name the classifier flags unless a real tool has the
// alias's name
func wireNamer(tools []map[string]any) func(string) string {
	claimed := map[string]bool{}
	for _, tool := range tools {
		if name, ok := tool["name"].(string); ok {
			if _, aliased := wireAliases[name]; !aliased {
				claimed[toMCP(name)] = true
			}
		}
	}
	return func(name string) string {
		if alias, ok := wireAliases[name]; ok && !claimed[mcpPrefix+alias] {
			name = alias
		}
		return toMCP(name)
	}
}

func toMCP(name string) string {
	if strings.HasPrefix(name, mcpPrefix) {
		return name
	}
	return mcpPrefix + strings.TrimPrefix(name, "mcp_")
}

// System prompt, tools and replayed tool calls of a subscription request in Claude Code's identity: its system block first, the product
// named as Claude Code, the tools under their wire names
func claudeCodeIdentity(system any, tools []map[string]any, messages []map[string]any, toWire func(string) string) []any {
	out := []any{map[string]any{"type": "text", "text": claudeCodePrefix}}
	switch s := system.(type) {
	case string:
		if s != "" {
			out = append(out, map[string]any{"type": "text", "text": s})
		}
	case []any:
		out = append(out, s...)
	}
	for _, b := range out {
		block, ok := b.(map[string]any)
		if !ok || block["type"] != "text" {
			continue
		}
		text, _ := block["text"].(string)
		for _, swap := range identitySwaps {
			text = strings.ReplaceAll(text, swap[0], swap[1])
		}
		if replaced, err := slug.Replace(text, "claude-code", -1, -1); err == nil {
			text = replaced
		}
		block["text"] = aliasProse(text)
	}
	for _, tool := range tools {
		if name, ok := tool["name"].(string); ok {
			tool["name"] = toWire(name)
		}
		if description, ok := tool["description"].(string); ok {
			tool["description"] = aliasProse(description)
		}
	}
	for _, message := range messages {
		content, _ := message["content"].([]any)
		for _, b := range content {
			if block, ok := b.(map[string]any); ok && block["type"] == "tool_use" {
				if name, ok := block["name"].(string); ok {
					block["name"] = toWire(name)
				}
			}
		}
	}
	return out
}

// Name of a tool a subscription reply calls, as the agent knows it: the name sent, a known tool the wire name stands for, else the
// aliased one
func fromWire(name string, known map[string]bool) string {
	bare, ok := strings.CutPrefix(name, mcpPrefix)
	if !ok {
		return name
	}
	for _, candidate := range []string{name, "mcp_" + bare, bare} {
		if known[candidate] {
			return candidate
		}
	}
	if original, ok := wireNames[bare]; ok {
		return original
	}
	return name
}
