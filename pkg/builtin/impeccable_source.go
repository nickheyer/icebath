package builtin

import (
	"fmt"
	"io/fs"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Number carried through frontmatter that prints the way ECMAScript prints numbers
type jsNumber float64

// One file of the skill scripts directory with its permission bits
type impeccableScript struct {
	Name    string
	Content string
	Mode    fs.FileMode
}

// One named text file, a reference by base name or a provider payload file
type impeccableFile struct {
	Name    string
	Content string
}

// One subagent definition read from skill/agents
type impeccableAgent struct {
	Name               string
	CodexName          string
	ClaudeName         string
	Description        any
	Tools              any
	Model              any
	Effort             any
	MaxTurns           any
	NicknameCandidates any
	Providers          []string
	Body               string
	FilePath           string
}

// The skill source as the build reads it from skill/
type impeccableSkill struct {
	Name          string
	Description   any
	License       any
	Compatibility any
	Metadata      any
	AllowedTools  any
	UserInvocable bool
	ArgumentHint  any
	Context       any
	Body          string
	FilePath      string
	References    []impeccableFile
	Scripts       []impeccableScript
	Agents        []impeccableAgent
	Payloads      map[string][]impeccableFile
}

// Token values of every provider substituted into skill text
var providerTokenTable = map[string]map[string]string{
	"claude-code": {"model": "Claude", "config_file": "CLAUDE.md", "ask_instruction": "Ask with the AskUserQuestion tool and wait for the answer before continuing.", "command_prefix": "/"},
	"cursor":      {"model": "the model", "config_file": ".cursorrules", "ask_instruction": "Ask the user directly to clarify what you cannot infer.", "command_prefix": "/"},
	"dsh":         {"model": "DeepSeek", "config_file": "AGENTS.md", "ask_instruction": "Ask with the ask_user_question tool and wait for the answer before continuing.", "command_prefix": "/"},
	"gemini":      {"model": "Gemini", "config_file": "GEMINI.md", "ask_instruction": "Ask the user directly to clarify what you cannot infer.", "command_prefix": "/"},
	"codex":       {"model": "GPT", "config_file": "AGENTS.md", "ask_instruction": "Ask with Codex's structured user-input/question tool when available, or directly in chat if it is not, and wait for the answer before continuing.", "command_prefix": "$"},
	"agents":      {"model": "the model", "config_file": ".github/copilot-instructions.md", "ask_instruction": "Ask the user directly to clarify what you cannot infer.", "command_prefix": "/"},
	"kiro":        {"model": "Claude", "config_file": ".kiro/settings.json", "ask_instruction": "Ask the user directly to clarify what you cannot infer.", "command_prefix": "/"},
	"opencode":    {"model": "Claude", "config_file": "AGENTS.md", "ask_instruction": "Ask with the `question` tool and wait for the answer before continuing.", "command_prefix": "/"},
	"pi":          {"model": "the model", "config_file": "AGENTS.md", "ask_instruction": "Ask the user directly to clarify what you cannot infer.", "command_prefix": "/"},
	"omp":         {"model": "the model", "config_file": "AGENTS.md", "ask_instruction": "Ask the user directly to clarify what you cannot infer.", "command_prefix": "/"},
	"qoder":       {"model": "the model", "config_file": "AGENTS.md", "ask_instruction": "Ask the user directly to clarify what you cannot infer.", "command_prefix": "/"},
	"trae":        {"model": "the model", "config_file": "RULES.md", "ask_instruction": "Ask the user directly to clarify what you cannot infer.", "command_prefix": "/"},
	"rovo-dev":    {"model": "Rovo Dev", "config_file": "AGENTS.md", "ask_instruction": "Ask the user directly to clarify what you cannot infer.", "command_prefix": "/"},
	"veto":        {"model": "the selected model", "config_file": "~/.veto/config.json", "ask_instruction": "Ask the user directly to clarify what you cannot infer.", "command_prefix": "/"},
	"vibe":        {"model": "Mistral", "config_file": "AGENTS.md", "ask_instruction": "Ask the user directly to clarify what you cannot infer.", "command_prefix": "/"},
	"grok":        {"model": "Grok", "config_file": "AGENTS.md", "ask_instruction": "Ask with the AskUserQuestion tool and wait for the answer before continuing.", "command_prefix": "/"},
	"antigravity": {"model": "Gemini", "config_file": "AGENTS.md", "ask_instruction": "Ask the user directly to clarify what you cannot infer.", "command_prefix": "/"},
	"hermes":      impeccableProviderTokens,
	"devin":       {"model": "Devin", "config_file": "AGENTS.md", "ask_instruction": "Ask with the ask_user_question tool when available, or directly in chat if it is not, and wait for the answer before continuing.", "command_prefix": "/"},
}

var (
	batchFileRe     = regexp.MustCompile(`(?i)\.(cmd|bat)$`)
	jsDecimalRe     = regexp.MustCompile(`^[+-]?(?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)
	jsRadixRe       = regexp.MustCompile(`^0(?:[xX][0-9a-fA-F]+|[oO][0-7]+|[bB][01]+)$`)
	jsSpaceCutset   = "\t\n\v\f\r \u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff"
	perProjectFiles = map[string]bool{"config.json": true}
)

const skillBinaryDir = "bin"

// Text of a file the way a WHATWG UTF-8 decoder reads it, one U+FFFD per malformed sequence
func decodeUTF8Lossy(data []byte) string {
	var b strings.Builder
	b.Grow(len(data))
	needed, seen := 0, 0
	var cp rune
	lower, upper := byte(0x80), byte(0xBF)
	for i := 0; i < len(data); i++ {
		c := data[i]
		if needed == 0 {
			switch {
			case c <= 0x7F:
				b.WriteByte(c)
			case c >= 0xC2 && c <= 0xDF:
				needed, cp = 1, rune(c&0x1F)
			case c >= 0xE0 && c <= 0xEF:
				if c == 0xE0 {
					lower = 0xA0
				}
				if c == 0xED {
					upper = 0x9F
				}
				needed, cp = 2, rune(c&0x0F)
			case c >= 0xF0 && c <= 0xF4:
				if c == 0xF0 {
					lower = 0x90
				}
				if c == 0xF4 {
					upper = 0x8F
				}
				needed, cp = 3, rune(c&0x07)
			default:
				b.WriteRune('\uFFFD')
			}
			continue
		}
		if c < lower || c > upper {
			needed, seen, cp, lower, upper = 0, 0, 0, 0x80, 0xBF
			b.WriteRune('\uFFFD')
			i--
			continue
		}
		lower, upper = 0x80, 0xBF
		cp = cp<<6 | rune(c&0x3F)
		seen++
		if seen == needed {
			b.WriteRune(cp)
			needed, seen, cp = 0, 0, 0
		}
	}
	if needed != 0 {
		b.WriteRune('\uFFFD')
	}
	return b.String()
}

// Reads a file as the lossy UTF-8 text the build works on
func readTextFile(p string) (string, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return decodeUTF8Lossy(data), nil
}

// ECMAScript truthiness of a frontmatter value
func jsTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case bool:
		return x
	case jsNumber:
		return x != 0 && !math.IsNaN(float64(x))
	}
	return true
}

// ECMAScript String() of a frontmatter value
func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return "undefined"
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case jsNumber:
		return x.String()
	case []any:
		parts := make([]string, len(x))
		for i, item := range x {
			if item != nil {
				parts[i] = jsString(item)
			}
		}
		return strings.Join(parts, ",")
	}
	return "[object Object]"
}

// ECMAScript Number() of a frontmatter value
func jsToNumber(v any) jsNumber {
	switch x := v.(type) {
	case jsNumber:
		return x
	case bool:
		if x {
			return 1
		}
		return 0
	case []any:
		return jsToNumber(jsString(x))
	case string:
		s := strings.Trim(x, jsSpaceCutset)
		switch {
		case s == "":
			return 0
		case s == "Infinity" || s == "+Infinity":
			return jsNumber(math.Inf(1))
		case s == "-Infinity":
			return jsNumber(math.Inf(-1))
		case jsRadixRe.MatchString(s):
			base := map[byte]int{'x': 16, 'X': 16, 'o': 8, 'O': 8, 'b': 2, 'B': 2}[s[1]]
			n, ok := new(big.Int).SetString(s[2:], base)
			if !ok {
				return jsNumber(math.NaN())
			}
			f, _ := new(big.Float).SetInt(n).Float64()
			return jsNumber(f)
		case jsDecimalRe.MatchString(s):
			f, err := strconv.ParseFloat(s, 64)
			if err != nil && !math.IsInf(f, 0) {
				return jsNumber(math.NaN())
			}
			return jsNumber(f)
		}
	}
	return jsNumber(math.NaN())
}

// Text ECMAScript Number.prototype.toString produces
func (n jsNumber) String() string {
	f := float64(n)
	switch {
	case math.IsNaN(f):
		return "NaN"
	case f == 0:
		return "0"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	mant, expText, _ := strings.Cut(strconv.FormatFloat(f, 'e', -1, 64), "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(expText)
	k, pos := len(digits), exp+1
	switch {
	case k <= pos && pos <= 21:
		return sign + digits + strings.Repeat("0", pos-k)
	case 0 < pos && pos <= 21:
		return sign + digits[:pos] + "." + digits[pos:]
	case -6 < pos && pos <= 0:
		return sign + "0." + strings.Repeat("0", -pos) + digits
	}
	e := strconv.Itoa(pos - 1)
	if pos-1 > 0 {
		e = "+" + e
	}
	if k == 1 {
		return sign + digits + "e" + e
	}
	return sign + digits[:1] + "." + digits[1:] + "e" + e
}

// JSON.stringify of a string
func jsonQuote(s string) string {
	out, err := pyjson.Dumps(s, pyjson.EnsureASCII(false))
	if err != nil {
		panic(err)
	}
	return out
}

// Slice of a string by UTF-16 code units, the unit ECMAScript string indexes count
func utf16Units(s string) []uint16 {
	return utf16.Encode([]rune(s))
}

func fmHas(pairs []fmPair, key string) bool {
	for _, p := range pairs {
		if p.Key == key {
			return true
		}
	}
	return false
}

// First truthy value of a key, else the fallback, the || default the build applies
func fmOr(pairs []fmPair, key string, fallback any) any {
	if v := fmLookup(pairs, key); jsTruthy(v) {
		return v
	}
	return fallback
}

func isBatchFile(name string) bool {
	return batchFileRe.MatchString(name)
}

// Text with every line ending as CRLF
func toCrlf(text string) string {
	return crlfRe.ReplaceAllString(text, "\r\n")
}

// Paths of every batch file under the directories that holds a bare LF, missing directories skipped
func findBatchFilesWithBareLf(dirs []string) ([]string, error) {
	var bad []string
	var walk func(dir string) error
	walk = func(dir string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}
		for _, entry := range entries {
			entryPath := filepath.Join(dir, entry.Name())
			if entry.IsDir() {
				if entry.Name() == "node_modules" || entry.Name() == ".git" {
					continue
				}
				if err := walk(entryPath); err != nil {
					return err
				}
				continue
			}
			if !entry.Type().IsRegular() || !isBatchFile(entry.Name()) {
				continue
			}
			data, err := os.ReadFile(entryPath)
			if err != nil {
				return err
			}
			for i, c := range data {
				if c == '\n' && (i == 0 || data[i-1] != '\r') {
					bad = append(bad, entryPath)
					break
				}
			}
		}
		return nil
	}
	for _, dir := range dirs {
		if err := walk(dir); err != nil {
			return nil, err
		}
	}
	return bad, nil
}

// Files under skill/scripts with batch files as CRLF, the binary directory and project artifacts left out
func readSkillScripts(scriptsDir string) ([]impeccableScript, error) {
	var scripts []impeccableScript
	var walk func(dir string) error
	walk = func(dir string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			entryPath := filepath.Join(dir, entry.Name())
			if entry.IsDir() {
				if dir == scriptsDir && entry.Name() == skillBinaryDir {
					continue
				}
				if err := walk(entryPath); err != nil {
					return err
				}
				continue
			}
			if !entry.Type().IsRegular() || perProjectFiles[entry.Name()] {
				continue
			}
			rel, err := filepath.Rel(scriptsDir, entryPath)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			raw, err := readTextFile(entryPath)
			if err != nil {
				return err
			}
			info, err := os.Stat(entryPath)
			if err != nil {
				return err
			}
			if isBatchFile(rel) {
				raw = toCrlf(raw)
			}
			scripts = append(scripts, impeccableScript{Name: rel, Content: raw, Mode: info.Mode() & 0o777})
		}
		return nil
	}
	if err := walk(scriptsDir); err != nil {
		return nil, err
	}
	return scripts, nil
}

// Names of the markdown files directly inside a directory
func markdownNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".md") {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}

// Provider list of an agent, nil when the agent ships to every provider
func agentProviders(raw any) []string {
	var parts []string
	switch x := raw.(type) {
	case []any:
		for _, p := range x {
			parts = append(parts, jsString(p))
		}
	case string:
		if strings.Trim(x, jsSpaceCutset) == "" {
			return nil
		}
		parts = strings.Split(x, ",")
	default:
		return nil
	}
	out := []string{}
	for _, p := range parts {
		if p = strings.Trim(p, jsSpaceCutset); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// The single impeccable skill read from root/skill, no skill when SKILL.src.md is absent
func readImpeccableSource(rootDir string) ([]impeccableSkill, error) {
	skillDir := filepath.Join(rootDir, "skill")
	skillMdPath := filepath.Join(skillDir, "SKILL.src.md")
	if !pathExists(skillMdPath) {
		return nil, nil
	}
	content, err := readTextFile(skillMdPath)
	if err != nil {
		return nil, err
	}
	fm, body := parseImpeccableFrontmatter(content)
	skill := impeccableSkill{
		Description:   fmOr(fm, "description", ""),
		License:       fmOr(fm, "license", ""),
		Compatibility: fmOr(fm, "compatibility", ""),
		Metadata:      fmOr(fm, "metadata", nil),
		AllowedTools:  fmOr(fm, "allowed-tools", ""),
		ArgumentHint:  fmOr(fm, "argument-hint", ""),
		Context:       fmOr(fm, "context", nil),
		Body:          body,
		FilePath:      skillMdPath,
		Payloads:      map[string][]impeccableFile{},
	}
	inv := fmLookup(fm, "user-invocable")
	skill.UserInvocable = inv == true || inv == "true"
	name, ok := fmOr(fm, "name", "impeccable").(string)
	if !ok {
		return nil, fmt.Errorf("%s: skill name must be a string", skillMdPath)
	}
	skill.Name = name

	referenceDir := filepath.Join(skillDir, "reference")
	if pathExists(referenceDir) {
		names, err := markdownNames(referenceDir)
		if err != nil {
			return nil, err
		}
		for _, refFile := range names {
			text, err := readTextFile(filepath.Join(referenceDir, refFile))
			if err != nil {
				return nil, err
			}
			skill.References = append(skill.References, impeccableFile{Name: strings.TrimSuffix(refFile, ".md"), Content: text})
		}
	}

	scriptsDir := filepath.Join(skillDir, "scripts")
	if pathExists(scriptsDir) {
		if skill.Scripts, err = readSkillScripts(scriptsDir); err != nil {
			return nil, err
		}
	}

	for _, config := range impeccableProviders {
		if config.PluginPayload == nil {
			continue
		}
		source := config.PluginPayload.Source
		dir := filepath.Join(skillDir, source)
		if _, done := skill.Payloads[source]; done || !pathExists(dir) {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		files := []impeccableFile{}
		for _, entry := range entries {
			if !entry.Type().IsRegular() {
				continue
			}
			text, err := readTextFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				return nil, err
			}
			files = append(files, impeccableFile{Name: entry.Name(), Content: text})
		}
		sort.SliceStable(files, func(i, j int) bool { return files[i].Name < files[j].Name })
		skill.Payloads[source] = files
	}

	agentsDir := filepath.Join(skillDir, "agents")
	if pathExists(agentsDir) {
		names, err := markdownNames(agentsDir)
		if err != nil {
			return nil, err
		}
		for _, agentFile := range names {
			agentPath := filepath.Join(agentsDir, agentFile)
			text, err := readTextFile(agentPath)
			if err != nil {
				return nil, err
			}
			afm, abody := parseImpeccableFrontmatter(text)
			name, ok := fmOr(afm, "name", strings.TrimSuffix(agentFile, ".md")).(string)
			if !ok {
				return nil, fmt.Errorf("%s: agent name must be a string", agentPath)
			}
			agent := impeccableAgent{
				Name:               name,
				CodexName:          jsString(fmOr(afm, "codex-name", strings.ReplaceAll(name, "-", "_"))),
				ClaudeName:         jsString(fmOr(afm, "claude-name", name)),
				Description:        fmOr(afm, "description", ""),
				Tools:              fmOr(afm, "tools", ""),
				Model:              fmOr(afm, "model", ""),
				Effort:             fmOr(afm, "effort", ""),
				MaxTurns:           "",
				NicknameCandidates: fmOr(afm, "nickname-candidates", []any{}),
				Providers:          agentProviders(fmLookup(afm, "providers")),
				Body:               abody,
				FilePath:           agentPath,
			}
			if raw := fmLookup(afm, "max-turns"); jsTruthy(raw) {
				agent.MaxTurns = jsToNumber(raw)
			}
			skill.Agents = append(skill.Agents, agent)
		}
	}
	return []impeccableSkill{skill}, nil
}

// Provider tokens substituted into skill text, slash invocations respelled for a non slash prefix
func replaceProviderTokens(content, provider string, commandNames, allSkillNames []string) string {
	tokens, ok := providerTokenTable[provider]
	if !ok {
		tokens = providerTokenTable["cursor"]
	}
	cmdPrefix := tokens["command_prefix"]
	if cmdPrefix == "" {
		cmdPrefix = "/"
	}
	var list []string
	for _, n := range commandNames {
		if !excludedFromSuggestions[n] {
			list = append(list, cmdPrefix+n)
		}
	}
	if len(list) == 0 {
		for _, n := range impeccableSubCommands {
			list = append(list, cmdPrefix+"impeccable "+n)
		}
	}
	result := strings.ReplaceAll(content, "{{model}}", tokens["model"])
	result = strings.ReplaceAll(result, "{{config_file}}", tokens["config_file"])
	result = strings.ReplaceAll(result, "{{ask_instruction}}", tokens["ask_instruction"])
	result = strings.ReplaceAll(result, "{{command_prefix}}", cmdPrefix)
	result = strings.ReplaceAll(result, "{{available_commands}}", strings.Join(list, ", "))
	if cmdPrefix != "/" && len(allSkillNames) > 0 {
		sorted := append([]string(nil), allSkillNames...)
		sort.SliceStable(sorted, func(i, j int) bool { return len(utf16Units(sorted[i])) > len(utf16Units(sorted[j])) })
		for _, name := range sorted {
			re := regexp2.MustCompile(`(?<![a-zA-Z0-9_./}>-])/(?=`+regexp2.Escape(name)+`(?:[^a-zA-Z0-9_-]|\z))`, regexp2.None)
			replaced, err := re.ReplaceFunc(result, func(regexp2.Match) string { return cmdPrefix }, -1, -1)
			if err != nil {
				panic(err)
			}
			result = replaced
		}
	}
	return result
}

// Script source with the two provider marker lines rendered for a build
func replaceProviderScriptMarker(content, provider, buildProvider string) string {
	tokens, ok := providerTokenTable[provider]
	if !ok {
		tokens = providerTokenTable["cursor"]
	}
	commandPrefix := tokens["command_prefix"]
	if commandPrefix == "" {
		commandPrefix = "/"
	}
	content = strings.Replace(content, "export const IMPECCABLE_COMMAND_PREFIX = '/'; // @impeccable-provider-command-prefix",
		"export const IMPECCABLE_COMMAND_PREFIX = "+jsonQuote(commandPrefix)+";", 1)
	return strings.Replace(content, "export const IMPECCABLE_PROVIDER_ID = 'source'; // @impeccable-provider-id",
		"export const IMPECCABLE_PROVIDER_ID = "+jsonQuote(buildProvider)+";", 1)
}

// One curated category of the homepage antidote list with its do and dont lines
type curatedCategory struct {
	Name string
	Do   []string
	Dont []string
}

// One named list of pattern lines
type patternCategory struct {
	Name  string
	Items []string
}

// The curated do lists as patterns and the dont lists as antipatterns
type impeccablePatterns struct {
	Patterns     []patternCategory
	Antipatterns []patternCategory
}

var curatedCategories = []curatedCategory{
	{
		Name: "Typography",
		Do: []string{
			"Pair a distinctive display face with a restrained body face; vary across projects.",
			"Use a \u22651.25 scale ratio between hierarchy steps. Flat scales read as bland.",
			"Cap body line length at 65\u201375ch. Wider is fatiguing.",
		},
		Dont: []string{
			"Inter, Roboto, Plex, Fraunces, or any other reflex default. Look further.",
			"Monospace as lazy shorthand for \"technical.\"",
			"Long passages in uppercase. Reserve all-caps for short labels.",
		},
	},
	{
		Name: "Color & Contrast",
		Do: []string{
			"Use OKLCH. Reduce chroma near lightness extremes.",
			"Tint neutrals toward the brand hue. Chroma 0.005\u20130.01 is enough.",
			"Pick a color strategy before picking colors (Restrained, Committed, Full, Drenched).",
		},
		Dont: []string{
			"Pure #000 or #fff. Always tint.",
			"Dark mode + purple-to-cyan gradients. The AI tell.",
			"Gradient text via background-clip. Use weight or size for emphasis.",
		},
	},
	{
		Name: "Layout & Space",
		Do: []string{
			"Vary spacing for rhythm. Tight groupings, generous separations.",
			"Use the simplest tool: Flexbox for 1D, Grid for 2D, plain flow often enough.",
			"Let whitespace carry hierarchy before reaching for color or scale.",
		},
		Dont: []string{
			"Wrap everything in cards. Nested cards are always wrong.",
			"Identical card grids of icon + heading + text, repeated endlessly.",
			"The hero-metric template: big number, small label, supporting stats, gradient accent.",
		},
	},
	{
		Name: "Visual Details",
		Do: []string{
			"Commit to an aesthetic direction and execute it with precision.",
			"Use ornament only where it earns its place.",
		},
		Dont: []string{
			"Side-stripe borders (border-left/-right > 1px). The dashboard tell.",
			"Glassmorphism everywhere. Rare and purposeful or nothing.",
			"Rounded rectangles with generic drop shadows. \"Could be any AI output.\"",
		},
	},
	{
		Name: "Motion",
		Do: []string{
			"Use transform and opacity. Animate the composited properties only.",
			"Ease out with exponential curves (quart / quint / expo).",
			"Respect prefers-reduced-motion on every transition.",
		},
		Dont: []string{
			"Animate layout (width, height, padding, margin).",
			"Bounce or elastic easing. Feels dated and tacky.",
			"Decorative motion for its own sake. Motion should signal state.",
		},
	},
	{
		Name: "Interaction",
		Do: []string{
			"Use optimistic UI: update immediately, sync later.",
			"Design empty states that teach the interface, not just say \"nothing here.\"",
			"Progressive disclosure: start simple, reveal sophistication on demand.",
		},
		Dont: []string{
			"Make every button primary. Hierarchy matters.",
			"Default to a modal. Exhaust inline alternatives first.",
			"Repeat information the user can already see.",
		},
	},
}

// The curated pattern catalog, whatever root or path is passed
func readPatterns(_ ...string) impeccablePatterns {
	var out impeccablePatterns
	for _, c := range curatedCategories {
		out.Patterns = append(out.Patterns, patternCategory{c.Name, c.Do})
		out.Antipatterns = append(out.Antipatterns, patternCategory{c.Name, c.Dont})
	}
	return out
}

// Paths of every markdown file under a directory, none when the directory is missing
func readFilesRecursive(dir string) ([]string, error) {
	fileList := []string{}
	if !pathExists(dir) {
		return fileList, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		filePath := filepath.Join(dir, entry.Name())
		info, err := os.Stat(filePath)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			nested, err := readFilesRecursive(filePath)
			if err != nil {
				return nil, err
			}
			fileList = append(fileList, nested...)
		} else if strings.HasSuffix(entry.Name(), ".md") {
			fileList = append(fileList, filePath)
		}
	}
	return fileList, nil
}
