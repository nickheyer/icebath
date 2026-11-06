package securityguidance

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

const maxScanBytes = 256 * 1024

func init() { plugins.RegisterBundled(Plugin{}) }

// One security rule, a path check fires on the path alone, a path filter gates the content triggers
type Rule struct {
	RuleName   string
	Reminder   string
	PathCheck  func(string) bool
	PathFilter func(string) bool
	Substrings []string
	Regex      string
}

// Rule with its regex compiled once
type compiledRule struct {
	Rule
	re *regexp2.Regexp
}

// Path argument and scanned content arguments of each targeted tool
var targetTools = map[string]struct {
	path    string
	content []string
}{
	"write_file":   {"path", []string{"content"}},
	"patch":        {"path", []string{"new_string", "patch"}},
	"skill_manage": {"file_path", []string{"file_content", "new_string"}},
}

var compiled = compileRules(SecurityPatterns)

// Whether an environment variable holds 1, true, yes or on ignoring case
func envFlag(name string) bool {
	switch pystr.Lower(os.Getenv(name)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Compiles every rule regex, skipping a rule whose regex is invalid with a warning
func compileRules(rules []Rule) []compiledRule {
	out := make([]compiledRule, 0, len(rules))
	for _, r := range rules {
		c := compiledRule{Rule: r}
		if r.Regex != "" {
			re, err := pyre.Compile(r.Regex)
			if err != nil {
				slog.Warn(fmt.Sprintf("security-guidance: skipping rule %s — invalid regex %s: %v", r.RuleName, pyrepr.Repr(r.Regex), err))
				continue
			}
			c.re = re
		}
		out = append(out, c)
	}
	return out
}

// Whether one rule fires for one write, a panicking path predicate counting as a miss
func ruleMatches(e compiledRule, path, content string) (hit bool) {
	gate := func() (ok, decided bool) {
		defer func() {
			if recover() != nil {
				ok, decided = false, true
			}
		}()
		if e.PathCheck != nil {
			return e.PathCheck(path), true
		}
		if e.PathFilter != nil && !e.PathFilter(path) {
			return false, true
		}
		return false, false
	}
	if ok, decided := gate(); decided {
		return ok
	}
	for _, sub := range e.Substrings {
		if strings.Contains(content, sub) {
			return true
		}
	}
	if e.re != nil {
		m, err := e.re.MatchString(content)
		return err == nil && m
	}
	return false
}

// UTF-8 length of a WTF-8 string with lone surrogates dropped like errors=ignore
func utf8LenIgnoringSurrogates(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == 0xED && i+2 < len(s) && s[i+1] >= 0xA0 {
			i += 2
			continue
		}
		n++
	}
	return n
}

// Name and reminder of every rule matching the content, each at most once
type finding struct{ name, reminder string }

// Findings for one path and content, none for empty or oversized content
func scanContent(path, content string) []finding {
	if content == "" || utf8LenIgnoringSurrogates(content) > maxScanBytes {
		return nil
	}
	var out []finding
	for _, e := range compiled {
		if ruleMatches(e, path, content) {
			out = append(out, finding{e.RuleName, e.Reminder})
		}
	}
	return out
}

// Findings over every populated content argument of a targeted tool call
func scanArgs(toolName string, args any) []finding {
	spec, ok := targetTools[toolName]
	m, isMap := args.(map[string]any)
	if envFlag("SECURITY_GUIDANCE_DISABLE") || !ok || !isMap {
		return nil
	}
	path, _ := m[spec.path].(string)
	var out []finding
	for _, key := range spec.content {
		if v, ok := m[key].(string); ok && v != "" {
			out = append(out, scanContent(path, v)...)
		}
	}
	return out
}

// Markdown warning block appended to a tool result
func formatWarningBlock(findings []finding) string {
	names := make([]string, len(findings))
	for i, f := range findings {
		names[i] = f.name
	}
	plural := "s"
	if len(findings) == 1 {
		plural = ""
	}
	lines := []string{"", "---", fmt.Sprintf("⚠️ Security guidance — %d pattern%s matched (%s)", len(findings), plural, strings.Join(names, ", ")), ""}
	for _, f := range findings {
		lines = append(lines, f.reminder, "")
	}
	lines = append(lines, "Pattern matches can be false positives. If the construct is safe in this "+
		"context, briefly document why in a code comment and continue. Otherwise, "+
		"fix the code before moving on.")
	return strings.Join(lines, "\n")
}

// Refuses the write in block mode when any pattern matches, nil lets it through
func onPreToolCall(_ context.Context, kwargs map[string]any) (any, error) {
	if !envFlag("SECURITY_GUIDANCE_BLOCK") {
		return nil, nil
	}
	tool, _ := kwargs["tool_name"].(string)
	findings := scanArgs(tool, kwargs["args"])
	if len(findings) == 0 {
		return nil, nil
	}
	return map[string]any{
		"action":  "block",
		"message": "security-guidance refused this write: " + formatWarningBlock(findings) + "\n\nTo override, unset SECURITY_GUIDANCE_BLOCK and retry.",
	}, nil
}

// Appends the warning block to a string result in warn mode, leaving error results alone
func onTransformToolResult(_ context.Context, kwargs map[string]any) (any, error) {
	result, isStr := kwargs["result"].(string)
	if envFlag("SECURITY_GUIDANCE_BLOCK") || !isStr {
		return nil, nil
	}
	tool, _ := kwargs["tool_name"].(string)
	findings := scanArgs(tool, kwargs["args"])
	if len(findings) == 0 {
		return nil, nil
	}
	if parsed, err := pyjson.Loads(result); err == nil {
		if o, ok := parsed.(*pyjson.Object); ok {
			if _, has := o.Get("error"); has && o.Len() <= 2 {
				return nil, nil
			}
		}
	}
	return result + "\n\n" + formatWarningBlock(findings), nil
}

// Compiled-in security-guidance plugin
type Plugin struct{}

// Manifest of the security-guidance plugin from its plugin.yaml
func (Plugin) Manifest() plugins.Manifest {
	return plugins.Manifest{
		Name:          "security-guidance",
		Version:       "0.1.0",
		Description:   "Append security warnings to file-write tool results when the new content contains known-dangerous patterns (pickle.load, yaml.load, eval(, os.system, dangerouslySetInnerHTML, verify=False, ECB, XXE, GitHub Actions injection, ...). 25 regex/substring rules forked from Anthropic's claude-plugins-official under Apache-2.0. Non-blocking — the file is written and the warning rides back to the model in the next turn so it can self-correct.",
		Author:        "Anthropic (patterns, Apache-2.0) / NousResearch (icebath plugin port)",
		ProvidesHooks: []string{"transform_tool_result", "pre_tool_call"},
	}
}

// Registers the block mode pre_tool_call hook and the warn mode transform_tool_result hook
func (Plugin) Register(ctx *plugins.Context) error {
	ctx.RegisterHook("pre_tool_call", onPreToolCall, false)
	ctx.RegisterHook("transform_tool_result", onTransformToolResult, false)
	return nil
}
