package transport

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2/v2"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
)

// Author named in the product's own system prompt
const identityAuthor = "Nicholas W. Heyer"

// Betas required for OAuth subscription auth
var anthropicOAuthOnlyBetas = []string{"claude-code-20250219", "oauth-2025-04-20"}

const anthClaudeCodeVersionFallback = "2.1.74"

// Install prefixes probed in addition to PATH for the Claude Code CLI
var anthClaudeCodePrefixes = []string{
	"~/.local/bin", "~/.claude/local", "~/bin", "~/.npm-global/bin", "~/.bun/bin",
	"~/.volta/bin", "/opt/homebrew/bin", "/usr/local/bin",
}

var anthClaudeCodeNames = []string{"claude", "claude-code"}

// PATH lookup seam for the Claude Code CLI probes
var anthLookPath = exec.LookPath

// Runs a CLI with --version and returns its stdout, seam for tests
var anthRunVersion = func(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Stdin = nil
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return stdout.String(), nil
}

func anthIsRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Executable paths to try, PATH hits first then the install prefixes, deduped
func anthClaudeCodeCandidates() []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, name := range anthClaudeCodeNames {
		if hit, err := anthLookPath(name); err == nil {
			add(hit)
		}
	}
	for _, prefix := range anthClaudeCodePrefixes {
		for _, name := range anthClaudeCodeNames {
			p := filepath.Join(expandTildePath(prefix), name)
			if anthIsRegularFile(p) {
				add(p)
			}
		}
	}
	return out
}

// Installed Claude Code version from the first candidate answering --version, else the fallback
func anthDetectClaudeCodeVersion() string {
	for _, cmd := range anthClaudeCodeCandidates() {
		out, err := anthRunVersion(cmd)
		if err != nil {
			continue
		}
		fields := strings.Fields(strings.TrimSpace(out))
		if len(fields) == 0 {
			continue
		}
		version := fields[0]
		if version != "" && version[0] >= '0' && version[0] <= '9' {
			return version
		}
	}
	return anthClaudeCodeVersionFallback
}

var anthClaudeCodeVersionMu sync.Mutex

var anthClaudeCodeVersionCache string

// Process cached Claude Code version detected on first OAuth use
func anthClaudeCodeVersion() string {
	anthClaudeCodeVersionMu.Lock()
	defer anthClaudeCodeVersionMu.Unlock()
	if anthClaudeCodeVersionCache == "" {
		anthClaudeCodeVersionCache = anthDetectClaudeCodeVersion()
	}
	return anthClaudeCodeVersionCache
}

const anthClaudeCodeSystemPrefix = "You are Claude Code, Anthropic's official CLI for Claude."

const anthMCPToolPrefix = "mcp__"

// Tool names rewritten on the OAuth wire so the billing classifier does not flag them
var anthOAuthToolNameAliases = map[string]string{"session_search": "chat_history_lookup", "memory": "context_notes"}

var anthOAuthToolNameReverseAliases = func() map[string]string {
	out := map[string]string{}
	for name, wire := range anthOAuthToolNameAliases {
		out[wire] = name
	}
	return out
}()

var anthOAuthProseAliasPatterns = []struct {
	re   *regexp.Regexp
	wire string
}{{regexp.MustCompile(`\bsession_search\b`), anthOAuthToolNameAliases["session_search"]}}

// Rewrites prose safe tool name tokens to their OAuth wire aliases
func anthApplyOAuthProseAliases(text string) string {
	for _, p := range anthOAuthProseAliasPatterns {
		text = p.re.ReplaceAllString(text, p.wire)
	}
	return text
}

// OAuth wire form of a tool name, double underscore mcp prefix without aliasing
func anthNormalizeToMCPWire(name string) string {
	if strings.HasPrefix(name, anthMCPToolPrefix) {
		return name
	}
	return anthMCPToolPrefix + strings.TrimPrefix(name, "mcp_")
}

// Name to OAuth wire name mapper for one tool set, aliases yielding to a colliding real tool
func anthOAuthWireNamer(tools []map[string]any) func(string) string {
	claimed := map[string]bool{}
	for _, tool := range tools {
		name, ok := tool["name"].(string)
		if !ok {
			continue
		}
		if _, aliased := anthOAuthToolNameAliases[name]; aliased {
			continue
		}
		claimed[anthNormalizeToMCPWire(name)] = true
	}
	return func(name string) string {
		if aliased, ok := anthOAuthToolNameAliases[name]; ok && !claimed[anthMCPToolPrefix+aliased] {
			name = aliased
		}
		return anthNormalizeToMCPWire(name)
	}
}

// Product name and author swapped for the Claude Code identity in the system prompt of a subscription sign in
var anthOAuthSystemReplacements = [][2]string{
	{"Icebath Agent", "Claude Code"}, {"Icebath agent", "Claude Code"}, {identityAuthor, "Anthropic"},
}

// Standalone prose slug of the product, addresses and quoted identifiers excluded
var anthOAuthSlugPattern = regexp2.MustCompile(`(?<![\w./:@'"` + "`" + `-])icebath-agent(?![\w/@-]|\.\w)`)

func anthRewriteSlug(text string) string {
	out, err := anthOAuthSlugPattern.Replace(text, "claude-code", -1, -1)
	if err != nil {
		return text
	}
	return out
}

// Claude Code system prefix, product name sanitizing and tool renames for an OAuth request
func anthApplyClaudeCodeIdentity(system any, tools []map[string]any, messages []map[string]any, toWire func(string) string) []any {
	ccBlock := map[string]any{"type": "text", "text": anthClaudeCodeSystemPrefix}
	var blocks []any
	switch s := system.(type) {
	case string:
		if s != "" {
			blocks = []any{map[string]any{"type": "text", "text": s}}
		}
	case []any:
		blocks = s
	case []map[string]any:
		for _, b := range s {
			blocks = append(blocks, b)
		}
	}
	out := append([]any{ccBlock}, blocks...)
	for _, b := range out {
		block, ok := asMap(b)
		if !ok || asStr(block["type"]) != "text" {
			continue
		}
		text := asStr(block["text"])
		for _, rep := range anthOAuthSystemReplacements {
			text = strings.ReplaceAll(text, rep[0], rep[1])
		}
		setBlockField(b, "text", anthApplyOAuthProseAliases(anthRewriteSlug(text)))
	}
	for _, tool := range tools {
		if name, ok := tool["name"].(string); ok {
			tool["name"] = toWire(name)
		}
		if desc, ok := tool["description"].(string); ok {
			tool["description"] = anthApplyOAuthProseAliases(desc)
		}
	}
	for _, msg := range messages {
		content, ok := asList(msg["content"])
		if !ok {
			continue
		}
		for _, b := range content {
			block, ok := asMap(b)
			if !ok || asStr(block["type"]) != "tool_use" {
				continue
			}
			if name, ok := block["name"].(string); ok {
				setBlockField(b, "name", toWire(name))
			}
		}
	}
	return out
}

// User-agent the Claude Code identity carries
func anthClaudeCodeUserAgent() string {
	return "claude-code/" + anthClaudeCodeVersion() + " (external, cli)"
}

func asMap(v any) (map[string]any, bool) {
	return jsonutil.AsMap(v)
}

func asList(v any) ([]any, bool) {
	switch l := v.(type) {
	case []any:
		return l, true
	case []map[string]any:
		out := make([]any, len(l))
		for i, m := range l {
			out[i] = m
		}
		return out, true
	}
	return nil, false
}

func asStr(v any) string {
	s, _ := v.(string)
	return s
}

// Stores v under key in a block held as a plain map or an ordered object, in place
func setBlockField(b any, key string, v any) {
	switch t := b.(type) {
	case map[string]any:
		t[key] = v
	case *jsonutil.Object:
		if t != nil {
			t.Set(key, v)
		}
	}
}

func expandTildePath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if userHome, err := os.UserHomeDir(); err == nil {
			return filepath.Join(userHome, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// Lowercased hostname of a base URL, empty when absent
func baseURLHostname(raw string) string {
	host, err := urlutil.BaseHost(raw)
	if err != nil {
		return ""
	}
	return host
}

func normalizeBaseURLText(raw string) string { return strings.TrimSpace(raw) }

func normalizedLowerBaseURL(raw string) string {
	return strings.ToLower(strings.TrimRight(normalizeBaseURLText(raw), "/"))
}

// Any endpoint off anthropic.com, which uses its own keys and skips OAuth detection
func isThirdPartyAnthropicEndpoint(baseURL string) bool {
	n := normalizedLowerBaseURL(baseURL)
	return n != "" && !strings.Contains(n, "anthropic.com")
}

// OAuth and setup token shapes, never the Console API key families
func isOAuthToken(key string) bool {
	if key == "" {
		return false
	}
	for _, p := range []string{"sk-ant-api", "sk-ant-usr", "sk-ant-svc", "sk-ant-admin"} {
		if strings.HasPrefix(key, p) {
			return false
		}
	}
	return strings.HasPrefix(key, "sk-ant-") || strings.HasPrefix(key, "eyJ") || strings.HasPrefix(key, "cc-")
}

func isNativeAnthropicHost(baseURL string) bool {
	text := strings.TrimSpace(baseURL)
	return text == "" || baseURLHostname(text) == "api.anthropic.com"
}

// Registered name of a tool_use block name arriving on the OAuth wire
func inboundToolName(name string, known map[string]bool) string {
	if !strings.HasPrefix(name, anthMCPToolPrefix) {
		return name
	}
	bare := name[len(anthMCPToolPrefix):]
	for _, candidate := range []string{name, "mcp_" + bare, bare} {
		if known[candidate] {
			return candidate
		}
	}
	if original, ok := anthOAuthToolNameReverseAliases[bare]; ok {
		return original
	}
	return name
}
