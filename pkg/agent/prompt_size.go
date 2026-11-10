package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	promptSkillsBlockRe   = regexp.MustCompile(`(?s)<available_skills>.*?</available_skills>`)
	promptNamesOnlyLineRe = regexp.MustCompile(`^  .+ \[names only\]: (?P<names>.+)$`)
)

const (
	promptSkillLinePrefix  = "- "
	promptSkillsTableLimit = 20
)

// PromptTextSize holds the code point and UTF-8 byte counts of one prompt block
type PromptTextSize struct{ Chars, Bytes int }

// PromptSection is one cache tier row of the breakdown
type PromptSection struct {
	Label        string
	Chars, Bytes int
}

// SkillBreakdownEntry is the attributed index cost and on disk SKILL.md size of one skill
type SkillBreakdownEntry struct {
	Name                 string
	IndexLineBytes       int
	IndexLineTotalBytes  int
	IndexLineSharedBytes int
	IndexLineSkillCount  int
	SkillMDBytes         *int64
	Path                 string
}

// ToolsetBreakdownEntry is the tool count and schema JSON bytes of one canonical toolset
type ToolsetBreakdownEntry struct {
	Toolset   string
	ToolCount int
	JSONBytes int
}

// PromptBreakdown holds the prompt size measurements of a fresh offline session
type PromptBreakdown struct {
	Platform          string
	Model             string
	SystemPrompt      PromptTextSize
	SkillsIndex       PromptTextSize
	Memory            PromptTextSize
	UserProfile       PromptTextSize
	ToolCount         int
	ToolJSONBytes     int
	Sections          []PromptSection
	SkillsBreakdown   []SkillBreakdownEntry
	ToolsetsBreakdown []ToolsetBreakdownEntry
}

// UTF-8 byte length of text, the strict encode error when it holds a lone surrogate
func promptBytes(s string) (int, error) {
	if err := pystr.StrictUTF8EncodeError(s); err != nil {
		return 0, err
	}
	return len(s), nil
}

// Code point and byte counts of text
func promptSize(text string) (PromptTextSize, error) {
	n, err := promptBytes(text)
	if err != nil {
		return PromptTextSize{}, err
	}
	return PromptTextSize{Chars: pystr.Len(text), Bytes: n}, nil
}

// Byte count as kilobytes with one decimal and the KB suffix
func promptFmtKB(n int) string {
	return strconv.FormatFloat(float64(n)/1024, 'f', 1, 64) + " KB"
}

// Key lookup over a Python dict value, a Go map or an ordered object, false for any other type
func promptDictLookup(v any) (func(string) (any, bool), bool) {
	switch d := v.(type) {
	case map[string]any:
		return func(k string) (any, bool) { x, ok := d[k]; return x, ok }, true
	case *pyjson.Object:
		if d == nil {
			return nil, false
		}
		return d.Get, true
	}
	return nil, false
}

// Callable name of an OpenAI function shaped tool schema, empty for a non dict
func promptToolName(tool any) string {
	get, ok := promptDictLookup(tool)
	if !ok {
		return ""
	}
	if fnv, _ := get("function"); fnv != nil {
		if fget, isDict := promptDictLookup(fnv); isDict {
			if name, _ := fget("name"); pyrepr.Truthy(name) {
				return pyrepr.Str(name)
			}
		}
	}
	name, ok := get("name")
	if !ok {
		return ""
	}
	return pyrepr.Str(name)
}

// Offline agent with an inspect only key and the platform toolsets the gateway resolves
func buildInspectionAgent(ctx context.Context, env *tools.Env, platform string) (*AIAgent, error) {
	cfg, err := config.LoadContext(ctx, home.Dir(ctx), nil)
	if err != nil {
		return nil, err
	}
	modelCfg := map[string]any{}
	if m, ok := cfg["model"].(map[string]any); ok {
		modelCfg = m
	}
	agentCfg := map[string]any{}
	if raw := cfg["agent"]; pyrepr.Truthy(raw) {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(raw))
		}
		agentCfg = m
	}
	model := ""
	if v := modelCfg["default"]; pyrepr.Truthy(v) {
		model = pyrepr.Str(v)
	} else if v := modelCfg["model"]; pyrepr.Truthy(v) {
		model = pyrepr.Str(v)
	}
	platformTools := tools.Default.PlatformTools(ctx, tools.ScopeKey(home.Dir(ctx)), cfg, platform, tools.PlatformToolsOptions{IncludeDefaultMCPServers: true})
	enabled := make([]string, 0, len(platformTools))
	for name := range platformTools {
		enabled = append(enabled, name)
	}
	sort.Strings(enabled)
	disabled := tools.ParseConfigStringList(agentCfg["disabled_toolsets"])
	if len(disabled) == 0 {
		disabled = nil
	}
	return NewAIAgent(ctx, AIAgentOptions{
		Model: model, APIKey: "inspect-only", BaseURL: "https://openrouter.ai/api/v1", QuietMode: true, SaveTrajectories: false,
		Platform: platform, EnabledToolsets: enabled, DisabledToolsets: disabled, Env: env,
	})
}

// Frontmatter name and directory name of every installed skill to its SKILL.md, local dirs first
func skillMDPathsByName(env *tools.Env) map[string]string {
	mapping := map[string]string{}
	setDefault := func(k, v string) {
		if _, ok := mapping[k]; !ok {
			mapping[k] = v
		}
	}
	for _, skillsDir := range skills.ScopeFromEnv(env).AllSkillsDirs() {
		if _, err := os.Stat(skillsDir); err != nil {
			continue
		}
		for _, skillFile := range skills.IterSkillIndexFiles(skillsDir, "SKILL.md") {
			dirName := filepath.Base(filepath.Dir(skillFile))
			frontmatterName := dirName
			if raw, err := os.ReadFile(skillFile); err == nil {
				if text, decErr := pystr.DecodeUTF8Sig(raw); decErr == nil {
					frontmatter, _ := skills.ParseFrontmatter(text)
					if v := frontmatter["name"]; pyrepr.Truthy(v) {
						frontmatterName = pyrepr.Str(v)
					}
				}
			}
			setDefault(frontmatterName, skillFile)
			setDefault(dirName, skillFile)
		}
	}
	return mapping
}

// Floor division and modulo of a by b like divmod
func promptDivmod(a, b int) (int, int) {
	q, r := a/b, a%b
	if r != 0 && (r < 0) != (b < 0) {
		q, r = q-1, r+b
	}
	return q, r
}

// Per skill byte breakdown of the rendered available_skills block, largest SKILL.md first
func computeSkillsBreakdown(env *tools.Env, skillsBlock string) ([]SkillBreakdownEntry, error) {
	nameToPath := skillMDPathsByName(env)
	entries := []SkillBreakdownEntry{}
	appendEntry := func(e SkillBreakdownEntry) {
		path, ok := nameToPath[e.Name]
		if ok {
			if st, err := os.Stat(path); err == nil {
				size := st.Size()
				e.SkillMDBytes = &size
			}
			e.Path = path
		}
		entries = append(entries, e)
	}
	for _, line := range pystr.Splitlines(skillsBlock, false) {
		lineBytes, err := promptBytes(line)
		if err != nil {
			return nil, err
		}
		if m := promptNamesOnlyLineRe.FindStringSubmatch(line); m != nil {
			var names []string
			for _, n := range strings.Split(m[promptNamesOnlyLineRe.SubexpIndex("names")], ",") {
				if s := pystr.Strip(n); s != "" {
					names = append(names, s)
				}
			}
			nameBytes := make([]int, len(names))
			sum := 0
			for i, name := range names {
				if nameBytes[i], err = promptBytes(name); err != nil {
					return nil, err
				}
				sum += nameBytes[i]
			}
			sharedBase, sharedRemainder := 0, 0
			if len(names) > 0 {
				sharedBase, sharedRemainder = promptDivmod(lineBytes-sum, len(names))
			}
			for i, name := range names {
				shared := sharedBase
				if i < sharedRemainder {
					shared++
				}
				appendEntry(SkillBreakdownEntry{Name: name, IndexLineBytes: nameBytes[i] + shared, IndexLineTotalBytes: lineBytes,
					IndexLineSharedBytes: shared, IndexLineSkillCount: len(names)})
			}
		} else if row := pystr.Lstrip(line); strings.HasPrefix(row, promptSkillLinePrefix) {
			rest := row[len(promptSkillLinePrefix):]
			if i := strings.Index(rest, ": "); i >= 0 {
				rest = rest[:i]
			}
			if name := pystr.Strip(rest); name != "" {
				appendEntry(SkillBreakdownEntry{Name: name, IndexLineBytes: lineBytes, IndexLineTotalBytes: lineBytes,
					IndexLineSharedBytes: 0, IndexLineSkillCount: 1})
			}
		}
	}
	mdOrZero := func(e SkillBreakdownEntry) int64 {
		if e.SkillMDBytes == nil {
			return 0
		}
		return *e.SkillMDBytes
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if a, b := mdOrZero(entries[i]), mdOrZero(entries[j]); a != b {
			return a > b
		}
		return entries[i].Name < entries[j].Name
	})
	return entries, nil
}

// Per toolset schema byte totals with each tool under its canonical toolset, largest first then by name
func computeToolsetsBreakdown(ctx context.Context, toolList []any) ([]ToolsetBreakdownEntry, error) {
	toolToToolset := tools.Default.ToolToToolsetMap(ctx, tools.ScopeKey(home.Dir(ctx)))
	groups := map[string]*ToolsetBreakdownEntry{}
	for _, tool := range toolList {
		toolset := toolToToolset[promptToolName(tool)]
		if toolset == "" {
			toolset = "(unknown)"
		}
		group, ok := groups[toolset]
		if !ok {
			group = &ToolsetBreakdownEntry{Toolset: toolset}
			groups[toolset] = group
		}
		group.ToolCount++
		text, err := pyjson.Dumps(tool, pyjson.EnsureASCII(false))
		if err != nil {
			return nil, err
		}
		n, err := promptBytes(text)
		if err != nil {
			return nil, err
		}
		group.JSONBytes += n
	}
	out := make([]ToolsetBreakdownEntry, 0, len(groups))
	for _, g := range groups {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].JSONBytes != out[j].JSONBytes {
			return out[i].JSONBytes > out[j].JSONBytes
		}
		return out[i].Toolset < out[j].Toolset
	})
	return out, nil
}

// Prompt size measurements of a fresh session on platform, its tiers and per skill and toolset costs
func ComputePromptBreakdown(ctx context.Context, env *tools.Env, platform string) (*PromptBreakdown, error) {
	if env != nil && env.Home != "" {
		ctx = home.WithOverride(ctx, env.Home)
	}
	ctx = tools.WithToolEnv(ctx, env)
	a, err := buildInspectionAgent(ctx, env, platform)
	if err != nil {
		return nil, err
	}
	parts, err := BuildSystemPromptParts(ctx, a, nil)
	if err != nil {
		return nil, err
	}
	full, err := BuildSystemPrompt(ctx, a, nil)
	if err != nil {
		return nil, err
	}
	skillsIndex := ""
	if loc := promptSkillsBlockRe.FindStringIndex(parts.Volatile); loc != nil {
		skillsIndex = parts.Volatile[loc[0]:loc[1]]
	} else if loc := promptSkillsBlockRe.FindStringIndex(parts.Stable); loc != nil {
		skillsIndex = parts.Stable[loc[0]:loc[1]]
	}
	memoryBlock, userBlock := "", ""
	if a.memoryStore != nil {
		if a.memoryEnabled {
			memoryBlock = a.memoryStore.FormatForSystemPrompt("memory")
		}
		if a.userProfileEnabled {
			userBlock = a.memoryStore.FormatForSystemPrompt("user")
		}
	}
	toolList := a.tools
	if toolList == nil {
		toolList = []any{}
	}
	out := &PromptBreakdown{Platform: platform, Model: a.model}
	for _, tier := range []struct{ label, text string }{
		{"stable (identity/guidance/skills)", parts.Stable}, {"context (AGENTS.md/cwd files)", parts.Context},
		{"volatile (memory/profile/timestamp)", parts.Volatile},
	} {
		n, err := promptBytes(tier.text)
		if err != nil {
			return nil, err
		}
		out.Sections = append(out.Sections, PromptSection{Label: tier.label, Chars: pystr.Len(tier.text), Bytes: n})
	}
	for _, s := range []struct {
		dst  *PromptTextSize
		text string
	}{{&out.SystemPrompt, full}, {&out.SkillsIndex, skillsIndex}, {&out.Memory, memoryBlock}, {&out.UserProfile, userBlock}} {
		if *s.dst, err = promptSize(s.text); err != nil {
			return nil, err
		}
	}
	toolsJSON, err := pyjson.Dumps(toolList, pyjson.EnsureASCII(false))
	if err != nil {
		return nil, err
	}
	out.ToolCount = len(toolList)
	if out.ToolJSONBytes, err = promptBytes(toolsJSON); err != nil {
		return nil, err
	}
	if out.SkillsBreakdown, err = computeSkillsBreakdown(env, skillsIndex); err != nil {
		return nil, err
	}
	if out.ToolsetsBreakdown, err = computeToolsetsBreakdown(ctx, toolList); err != nil {
		return nil, err
	}
	return out, nil
}

// Ordered JSON object of the skill entry in the key order the breakdown emits
func (e SkillBreakdownEntry) Object() *pyjson.Object {
	o := pyjson.NewObject()
	o.Set("name", e.Name)
	o.Set("index_line_bytes", e.IndexLineBytes)
	o.Set("index_line_total_bytes", e.IndexLineTotalBytes)
	o.Set("index_line_shared_bytes", e.IndexLineSharedBytes)
	o.Set("index_line_skill_count", e.IndexLineSkillCount)
	if e.SkillMDBytes != nil {
		o.Set("skill_md_bytes", *e.SkillMDBytes)
	} else {
		o.Set("skill_md_bytes", nil)
	}
	o.Set("path", e.Path)
	return o
}

// Ordered JSON object of the whole breakdown in the key order the json flag prints
func (b *PromptBreakdown) Object() *pyjson.Object {
	size := func(s PromptTextSize) *pyjson.Object {
		o := pyjson.NewObject()
		o.Set("chars", s.Chars)
		o.Set("bytes", s.Bytes)
		return o
	}
	o := pyjson.NewObject()
	o.Set("platform", b.Platform)
	o.Set("model", b.Model)
	o.Set("system_prompt", size(b.SystemPrompt))
	o.Set("skills_index", size(b.SkillsIndex))
	o.Set("memory", size(b.Memory))
	o.Set("user_profile", size(b.UserProfile))
	toolsObj := pyjson.NewObject()
	toolsObj.Set("count", b.ToolCount)
	toolsObj.Set("json_bytes", b.ToolJSONBytes)
	o.Set("tools", toolsObj)
	sections := make([]any, 0, len(b.Sections))
	for _, s := range b.Sections {
		sections = append(sections, []any{s.Label, s.Chars, s.Bytes})
	}
	o.Set("sections", sections)
	skillRows := make([]any, 0, len(b.SkillsBreakdown))
	for _, e := range b.SkillsBreakdown {
		skillRows = append(skillRows, e.Object())
	}
	o.Set("skills_breakdown", skillRows)
	toolsetRows := make([]any, 0, len(b.ToolsetsBreakdown))
	for _, ts := range b.ToolsetsBreakdown {
		row := pyjson.NewObject()
		row.Set("toolset", ts.Toolset)
		row.Set("tool_count", ts.ToolCount)
		row.Set("json_bytes", ts.JSONBytes)
		toolsetRows = append(toolsetRows, row)
	}
	o.Set("toolsets_breakdown", toolsetRows)
	return o
}

// Byte count right aligned in eight columns with comma thousands separators
func promptGrouped(n int64) string {
	return pystr.Rjust(pystr.GroupThousands(n), 8)
}

// Plain text rendering of the breakdown for a terminal
func RenderBreakdown(data *PromptBreakdown) string {
	sp := data.SystemPrompt
	model := data.Model
	if model == "" {
		model = "unset"
	}
	lines := []string{
		"Prompt-size breakdown (platform=" + data.Platform + ", model=" + model + ")", "",
		"  System prompt total : " + promptGrouped(int64(sp.Bytes)) + " B  (" + promptFmtKB(sp.Bytes) + ", " + pystr.GroupThousands(int64(sp.Chars)) + " chars)", "",
		"  Major blocks:",
	}
	for _, blk := range []struct {
		label string
		size  PromptTextSize
	}{{"skills index", data.SkillsIndex}, {"memory", data.Memory}, {"user profile", data.UserProfile}} {
		lines = append(lines, "    "+pystr.Ljust(blk.label, 19)+": "+promptGrouped(int64(blk.size.Bytes))+" B  ("+promptFmtKB(blk.size.Bytes)+")")
	}
	lines = append(lines, "", "  Prompt tiers:")
	for _, s := range data.Sections {
		lines = append(lines, "    "+pystr.Ljust(s.Label, 36)+": "+promptGrouped(int64(s.Bytes))+" B  ("+promptFmtKB(s.Bytes)+")")
	}
	lines = append(lines, "", "  Tool schemas         : "+promptGrouped(int64(data.ToolJSONBytes))+" B  ("+promptFmtKB(data.ToolJSONBytes)+", "+strconv.Itoa(data.ToolCount)+" tools)")
	if toolsets := data.ToolsetsBreakdown; len(toolsets) > 0 {
		lines = append(lines, "", "  Toolsets by size (tool-schema JSON, largest first):",
			"    "+pystr.Ljust("toolset", 22)+" "+pystr.Rjust("tools", 5)+"  "+pystr.Rjust("schema", 10))
		for _, ts := range toolsets {
			lines = append(lines, "    "+pystr.Ljust(ts.Toolset, 22)+" "+pystr.Rjust(strconv.Itoa(ts.ToolCount), 5)+"  "+promptGrouped(int64(ts.JSONBytes))+" B  ("+promptFmtKB(ts.JSONBytes)+")")
		}
	}
	if skillRows := data.SkillsBreakdown; len(skillRows) > 0 {
		lines = append(lines, "", "  Skills by size (SKILL.md on-disk = read cost; index cost = attributed always-on bytes, largest first):",
			"    "+pystr.Ljust("skill", 28)+" "+pystr.Rjust("SKILL.md", 10)+"  "+pystr.Rjust("index cost", 10))
		shown := skillRows[:min(len(skillRows), promptSkillsTableLimit)]
		for _, sk := range shown {
			mdStr := pystr.Rjust("n/a", 10)
			if sk.SkillMDBytes != nil {
				mdStr = promptGrouped(*sk.SkillMDBytes) + " B"
			}
			name := sk.Name
			if pystr.Len(name) > 28 {
				name = pystr.Slice(name, 0, 27) + "…"
			}
			lines = append(lines, "    "+pystr.Ljust(name, 28)+" "+mdStr+"  "+promptGrouped(int64(sk.IndexLineBytes))+" B")
		}
		if remaining := len(skillRows) - len(shown); remaining > 0 {
			lines = append(lines, "    … and "+strconv.Itoa(remaining)+" more (use --json for the full list)")
		}
	}
	return strings.Join(lines, "\n")
}

// Prints the prompt size breakdown as text or JSON, a failure reported as one line with exit 0
func CmdPromptSize(ctx context.Context, env *tools.Env, ns map[string]any, w io.Writer) (int, error) {
	platform, _ := ns["platform"].(string)
	if platform == "" {
		platform = "cli"
	}
	data, err := ComputePromptBreakdown(ctx, env, platform)
	if err != nil {
		_, _ = fmt.Fprintln(w, "Could not compute prompt-size breakdown: "+pyrepr.ExceptionMessage(err))
		return 0, nil
	}
	if pyrepr.Truthy(ns["json"]) {
		text, err := pyjson.Dumps(data.Object(), pyjson.EnsureASCII(false), pyjson.Indent(2))
		if err != nil {
			return 1, err
		}
		_, _ = fmt.Fprintln(w, text)
		return 0, nil
	}
	_, _ = fmt.Fprintln(w, RenderBreakdown(data))
	return 0, nil
}
