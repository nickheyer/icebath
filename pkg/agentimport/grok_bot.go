package agentimport

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	fsutil "github.com/nickheyer/icebath/pkg/utils/fs"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	regexutil "github.com/nickheyer/icebath/pkg/utils/regex"
)

const (
	grokAgent         = "grok-bot"
	exportFormat      = "grok-bot-export"
	grokSkillCategory = "grok-bot-imports"
)

// Local Grok Bot app dirs per OS, only used to tell an import is possible
var grokAppDirs = []string{"Library/Application Support/Grok Bot", "Applications/Grok Bot.app",
	".config/Grok Bot", "AppData/Roaming/Grok Bot", "AppData/Local/Programs/Grok Bot"}

var grokSystemAppDirs = []string{"/Applications/Grok Bot.app"}

var exportGlobs = []string{"grok-bot-export*.json", "grok-bot-export*.md", "grok-bot-export*.txt"}

var (
	fenceRe         = regexutil.MustCompile("(?s)```(?:json)?\\s*\\n(.*?)\\n```")
	slugRe          = regexp.MustCompile(`[^a-z0-9]+`)
	secretValueRe   = regexutil.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{16,}|xox[abprs]-[A-Za-z0-9-]{10,}|gh[pousr]_[A-Za-z0-9]{20,}|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,})|(?i:bearer\s+[A-Za-z0-9._~+/-]{20,})`)
	frontNameRe     = regexutil.MustCompile(`(?m)\A(---\s*\n(?:.*\n)*?)name:[^\n]*\n`)
	scheduleTrigger = map[string]bool{"schedule": true, "scheduled": true, "time": true, "cron": true, "interval": true}
	smartQuotes     = strings.NewReplacer("“", `"`, "”", `"`, "‘", "'", "’", "'")
	listKeys        = []string{"routines", "skills", "apps", "memories", "user_facts"}
)

// ExportPrompt is the message a user pastes into a Grok Bot chat so the Bot writes its own export
const ExportPrompt = "I'm moving to another assistant and need a complete, machine-readable copy of your setup. Do not " +
	"change, pause or delete anything.\n" +
	"\n" +
	"Write one JSON document with exactly this shape, then save it as grok-bot-export-<your name>.json " +
	"(your name in lowercase, spaces as hyphens): in my Downloads folder if you can write to my computer, " +
	"otherwise in your workspace, and attach it to your reply. If you cannot attach files, put the whole " +
	"document in a single ```json code block instead.\n" +
	"\n" +
	"{\n" +
	"  \"format\": \"grok-bot-export\",\n" +
	"  \"version\": 1,\n" +
	"  \"bots\": [\n" +
	"    {\n" +
	"      \"name\": \"your name\",\n" +
	"      \"title\": \"your label/job title, or empty\",\n" +
	"      \"instructions\": \"your full role, job description and lasting rules, verbatim where you can\",\n" +
	"      \"memories\": [\"each thing you remember about the work, one string per fact\"],\n" +
	"      \"user_facts\": [\"each fact you know about me (name, time zone, preferences), one per string\"],\n" +
	"      \"skills\": [\n" +
	"        {\"name\": \"skill name\", \"description\": \"when to use it\",\n" +
	"         \"instructions\": \"the COMPLETE skill text, every step, rule and output format, verbatim\"}\n" +
	"      ],\n" +
	"      \"routines\": [\n" +
	"        {\"name\": \"routine name\",\n" +
	"         \"instruction\": \"the COMPLETE instruction the routine runs, verbatim\",\n" +
	"         \"trigger\": \"schedule | slack | github | linear | sentry | pagerduty | email | webhook | other\",\n" +
	"         \"schedule_cron\": \"5-field cron expression in the routine's time zone, e.g. 0 8 * * 1-5 " +
	"(empty when not on a schedule)\",\n" +
	"         \"schedule_text\": \"When to run, in words, as the app shows it\",\n" +
	"         \"timezone\": \"IANA time zone, e.g. America/New_York\",\n" +
	"         \"paused\": false,\n" +
	"         \"skills\": [\"names of skills this routine uses\"],\n" +
	"         \"trigger_details\": \"for event triggers: channel, repository, phrase, senders, etc.\"}\n" +
	"      ],\n" +
	"      \"apps\": [{\"name\": \"connected app or MCP server name\", \"mcp_url\": \"remote MCP URL if any, else empty\"}]\n" +
	"    }\n" +
	"  ]\n" +
	"}\n" +
	"\n" +
	"Rules: include EVERY routine you own (paused ones too) and EVERY skill enabled for you, with full " +
	"text, not summaries. If you cannot read something (a routine's schedule, a skill's text), leave " +
	"it out and tell me in your reply what you left out; never guess or fill it in from memory. Never include passwords, API keys, tokens, webhook keys, cookies or card " +
	"numbers: leave those out entirely. Output valid JSON only (no comments).\n" +
	"If it is too long for one message or file, split it: send several documents, each complete and " +
	"valid in this same shape with the same bot name, each holding the part the others do not. They " +
	"are imported together.\n"

// Pasted stands for the export read from standard input, the text riding on the importer
const Pasted = "<pasted export>"

// Folder name of a bot, routine or skill, lowercase with hyphens, at most 64 chars
func slug(name string) string {
	s := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-"), "-")
	if len(s) > 64 {
		s = s[:64]
	}
	if s == "" {
		return "grok-bot"
	}
	return s
}

// DetectGrokBot is the Grok Bot app dir or an export file found on this computer, empty when none
func DetectGrokBot() string {
	h := userHome()
	var candidates []string
	for _, d := range grokAppDirs {
		candidates = append(candidates, filepath.Join(h, d))
	}
	candidates = append(candidates, grokSystemAppDirs...)
	for _, c := range candidates {
		if exists(c) {
			return c
		}
	}
	if found := FindExportFiles(""); len(found) > 0 {
		return found[0]
	}
	return ""
}

// FindExportFiles lists the exports to read, the source file, the export files in a source dir, or those in ~/Downloads
func FindExportFiles(source string) []string {
	if source != "" && isFile(source) {
		return []string{source}
	}
	folder := source
	if folder == "" {
		folder = filepath.Join(userHome(), "Downloads")
	}
	if !isDir(folder) {
		return nil
	}
	patterns := exportGlobs
	if source != "" {
		patterns = []string{"*.json", "*.md", "*.txt"}
	}
	seen := map[string]bool{}
	var found []string
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(filepath.Join(globEscape(folder), pattern))
		for _, p := range matches {
			if !seen[p] && isFile(p) && mentionsExportFormat(p) {
				seen[p] = true
				found = append(found, p)
			}
		}
	}
	mtime := func(p string) float64 {
		st, err := os.Stat(p)
		if err != nil {
			return 0
		}
		return float64(st.ModTime().UnixNano()) / 1e9
	}
	sort.SliceStable(found, func(i, j int) bool {
		mi, mj := mtime(found[i]), mtime(found[j])
		if mi != mj {
			return mi < mj
		}
		return filepath.Base(found[i]) < filepath.Base(found[j])
	})
	return found
}

// Cheap filter so a folder scan skips unrelated files
func mentionsExportFormat(path string) bool {
	text, err := readText(path)
	return err == nil && strings.Contains(text, exportFormat)
}

// ExportFormatError is a document that is not a Grok Bot export this importer can read
type ExportFormatError struct{ Msg string }

func (e *ExportFormatError) Error() string { return e.Msg }

// RedactSecrets replaces token shaped strings with [redacted] and counts them
func RedactSecrets(value any) (any, int) {
	switch t := value.(type) {
	case string:
		n := 0
		out, err := secretValueRe.ReplaceFunc(t, func(regexp2.Match) string { n++; return "[redacted]" }, -1, -1)
		if err != nil {
			return t, 0
		}
		return out, n
	case []any:
		out := make([]any, len(t))
		total := 0
		for i, v := range t {
			var n int
			out[i], n = RedactSecrets(v)
			total += n
		}
		return out, total
	case *jsonutil.Object:
		out := jsonutil.NewObject()
		total := 0
		for _, k := range t.Keys() {
			v, n := RedactSecrets(t.Value(k))
			out.Set(k, v)
			total += n
		}
		return out, total
	}
	return value, 0
}

// All matches of group one of a pattern
func findAllGroup(re *regexp2.Regexp, text string) []string {
	var out []string
	m, _ := re.FindStringMatch(text)
	for m != nil {
		out = append(out, m.GroupByNumber(1).String())
		m, _ = re.FindNextMatch(m)
	}
	return out
}

// ParseExport returns the bots of an export document or of every json block in a pasted reply, merged per bot
func ParseExport(text string) ([]*jsonutil.Object, error) {
	blocks := findAllGroup(fenceRe, text)
	candidates := blocks
	if len(blocks) == 0 {
		candidates = []string{strings.TrimSpace(text)}
		if open := strings.Index(text, "{"); open >= 0 {
			if end := strings.LastIndex(text, "}"); end >= 0 {
				if end+1 > open {
					candidates = append(candidates, text[open:end+1])
				} else {
					candidates = append(candidates, "")
				}
			}
		}
	}
	var documents []*jsonutil.Object
	for _, c := range candidates {
		if d := loadDocument(c, len(blocks) == 0); d != nil {
			documents = append(documents, d)
		}
	}
	if len(blocks) > 0 && len(documents) == 0 {
		if d := loadDocument(strings.TrimSpace(text), false); d != nil {
			documents = append(documents, d)
		}
	}
	if len(documents) == 0 {
		if strings.Count(text, "{") > strings.Count(text, "}") {
			return nil, &ExportFormatError{"the export looks cut off (more { than }); ask the Bot to send the " +
				"rest as another document in the same shape, or one bot per message"}
		}
		return nil, &ExportFormatError{fmt.Sprintf("no Grok Bot export found (expected a JSON document with \"format\": \"%s\" and a \"bots\" list)", exportFormat)}
	}
	var bots []*jsonutil.Object
	for _, d := range documents {
		valid, err := validatedBots(d)
		if err != nil {
			return nil, err
		}
		bots = append(bots, valid...)
	}
	return MergeBots(bots), nil
}

// A candidate as an export document, or with bareOK a bare bot list or bot, else nil
func loadDocument(candidate string, bareOK bool) *jsonutil.Object {
	attempts := []string{candidate, dropTrailingCommas(candidate), dropTrailingCommas(smartQuotes.Replace(candidate))}
	for _, attempt := range attempts {
		data, err := jsonutil.Decode(attempt)
		if err != nil {
			continue
		}
		if o, ok := asObject(data); ok {
			if f, _ := o.Value("format").(string); f == exportFormat {
				return o
			}
			if _, has := o.Get("bots"); has {
				return o
			}
		}
		if !bareOK {
			continue
		}
		if list, ok := data.([]any); ok && len(list) > 0 {
			all := true
			for _, b := range list {
				o, ok := asObject(b)
				if !ok {
					all = false
					break
				}
				if _, has := o.Get("name"); !has {
					all = false
					break
				}
			}
			if all {
				return jsonutil.ObjectOf("format", exportFormat, "version", int64(1), "bots", list)
			}
		}
		if o, ok := asObject(data); ok {
			if _, has := o.Get("name"); has {
				shaped := false
				for _, k := range append([]string{"instructions"}, listKeys...) {
					if _, ok := o.Get(k); ok {
						shaped = true
					}
				}
				if shaped {
					return jsonutil.ObjectOf("format", exportFormat, "version", int64(1), "bots", []any{o})
				}
			}
		}
	}
	return nil
}

// Text without commas that directly precede a closing brace or bracket outside strings
func dropTrailingCommas(text string) string {
	rs := []rune(text)
	var b strings.Builder
	inString, escaped := false, false
	for i, ch := range rs {
		if inString {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
		} else if ch == '"' {
			inString = true
		} else if ch == ',' {
			j := i + 1
			for j < len(rs) && unicode.IsSpace(rs[j]) {
				j++
			}
			if j < len(rs) && (rs[j] == '}' || rs[j] == ']') {
				continue
			}
		}
		b.WriteRune(ch)
	}
	return b.String()
}

// Lowercased trimmed name key of a bot
func botKey(bot *jsonutil.Object) string {
	return strings.ToLower(strings.TrimSpace(pyStr(bot.Value("name"))))
}

// Python emptiness of the values a merge leaves in place
func emptyValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case *jsonutil.Object:
		return t == nil || t.Len() == 0
	}
	return false
}

func isListKey(k string) bool {
	for _, l := range listKeys {
		if l == k {
			return true
		}
	}
	return false
}

// MergeBots makes one bot per name, lists unioned and scalars keeping the last non empty value
func MergeBots(bots []*jsonutil.Object) []*jsonutil.Object {
	merged := jsonutil.NewObject()
	for _, bot := range bots {
		key := botKey(bot)
		into, ok := asObject(merged.Value(key))
		if !ok {
			into = jsonutil.NewObject()
			merged.Set(key, into)
		}
		for _, field := range bot.Keys() {
			value := bot.Value(field)
			if list, isList := value.([]any); isList && isListKey(field) {
				into.Set(field, union(pyIter(into.Value(field)), list))
				continue
			}
			if _, has := into.Get(field); !emptyValue(value) || !has {
				into.Set(field, value)
			}
		}
	}
	out := make([]*jsonutil.Object, 0, merged.Len())
	for _, k := range merged.Keys() {
		out = append(out, merged.Value(k).(*jsonutil.Object))
	}
	return out
}

// Items a Python for loop yields from a value, chars of text and keys of an object
func pyIter(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case string:
		out := make([]any, 0, len(t))
		for _, r := range t {
			out = append(out, string(r))
		}
		return out
	case *jsonutil.Object:
		out := make([]any, 0, t.Len())
		for _, k := range t.Keys() {
			out = append(out, k)
		}
		return out
	}
	return nil
}

// Identity of a list item, stripped text or sorted JSON
func unionIdent(item any) string {
	if s, ok := item.(string); ok {
		return strings.TrimSpace(s)
	}
	text, err := jsonutil.Encode(item, jsonutil.SortKeys())
	if err != nil {
		return anyutil.Quote(item)
	}
	return text
}

// Union of two lists dropping only exact repeats, a repeat in current keeping its first position and last value
func union(current, incoming []any) []any {
	order := jsonutil.NewObject()
	for _, item := range current {
		order.Set(unionIdent(item), item)
	}
	for _, item := range incoming {
		if _, has := order.Get(unionIdent(item)); !has {
			order.Set(unionIdent(item), item)
		}
	}
	out := make([]any, 0, order.Len())
	for _, k := range order.Keys() {
		out = append(out, order.Value(k))
	}
	return out
}

// Dotted paths of secret looking keys anywhere in a value
func secretKeys(value any, path string) []string {
	var found []string
	switch t := value.(type) {
	case *jsonutil.Object:
		for _, key := range t.Keys() {
			here := key
			if path != "" {
				here = path + "." + key
			}
			if IsSecretKey(key) || strings.Contains(strings.ToLower(key), "password") {
				found = append(found, here)
			}
			found = append(found, secretKeys(t.Value(key), here)...)
		}
	case []any:
		for i, child := range t {
			found = append(found, secretKeys(child, fmt.Sprintf("%s[%d]", path, i))...)
		}
	}
	return found
}

// Reports Python equality with the integer one
func equalsOne(v any) bool {
	switch t := v.(type) {
	case int64:
		return t == 1
	case int:
		return t == 1
	case float64:
		return t == 1
	case bool:
		return t
	case *big.Int:
		return t.IsInt64() && t.Int64() == 1
	}
	return false
}

// Bots of a validated export document, refusing other versions, credential keys and nameless bots
func validatedBots(data *jsonutil.Object) ([]*jsonutil.Object, error) {
	if version, has := data.Get("version"); has && !equalsOne(version) {
		return nil, &ExportFormatError{"unsupported export version " + errchain.Show(version)}
	}
	if secrets := secretKeys(data, ""); len(secrets) > 0 {
		shown := secrets
		if len(shown) > 5 {
			shown = shown[:5]
		}
		return nil, &ExportFormatError{"the export carries credential-looking keys, so it was not read: " +
			strings.Join(shown, ", ") + ". Ask the Bot to export again without them."}
	}
	list, ok := data.Value("bots").([]any)
	if !ok || len(list) == 0 {
		return nil, &ExportFormatError{"the export has no bots"}
	}
	bots := make([]*jsonutil.Object, 0, len(list))
	for i, b := range list {
		o, ok := asObject(b)
		name := ""
		if ok && truthy(o.Value("name")) {
			name = strings.TrimSpace(pyStr(o.Value("name")))
		}
		if name == "" {
			return nil, &ExportFormatError{fmt.Sprintf("bots[%d] needs a name", i)}
		}
		bots = append(bots, o)
	}
	return bots, nil
}

// Stripped non empty strings of a list value
func stringsOf(value any) []string {
	list, _ := value.([]any)
	var out []string
	for _, v := range list {
		if s := strings.TrimSpace(pyStr(v)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Objects of a list value
func dictsOf(value any) []*jsonutil.Object {
	list, _ := value.([]any)
	var out []*jsonutil.Object
	for _, v := range list {
		if o, ok := asObject(v); ok {
			out = append(out, o)
		}
	}
	return out
}

// Sets the frontmatter name of a copied SKILL.md to its new folder name
func renameSkill(ctx context.Context, skillMD, name string) error {
	text, err := readText(skillMD)
	if err != nil {
		return err
	}
	renamed, err := frontNameRe.ReplaceFunc(text, func(m regexp2.Match) string {
		return m.GroupByNumber(1).String() + "name: " + name + "\n"
	}, -1, 1)
	if err != nil {
		return err
	}
	if renamed != text {
		return atomicWriteText(ctx, skillMD, renamed)
	}
	return nil
}

// SKILL.md text written for a skill the export carries inline
func skillMarkdown(skill *jsonutil.Object, name string) string {
	desc := skill.Value("description")
	description := ""
	if truthy(desc) {
		description = pyStr(desc)
	} else {
		description = "Imported Grok Bot skill: " + anyutil.String(skill.Value("name"))
	}
	description = strings.Join(strings.Fields(description), " ")
	if rs := []rune(description); len(rs) > 300 {
		description = string(rs[:300])
	}
	quoted, _ := jsonutil.Encode(description)
	body := ""
	if truthy(skill.Value("instructions")) {
		body = strings.TrimSpace(pyStr(skill.Value("instructions")))
	}
	if !strings.HasPrefix(body, "# ") {
		body = "# " + strings.TrimSpace(anyutil.String(skill.Value("name"))) + "\n\n" + body
	}
	return "---\nname: " + name + "\ndescription: " + quoted + "\n---\n\n" + body + "\n"
}

// A bot read from an export or a database with the file it came from
type sourcedBot struct {
	path string
	bot  *jsonutil.Object
}

// BotImporter plans and applies normalized bots into one home, the shared core of the Grok Bot and OpenBot importers
type BotImporter struct {
	base
	product          string
	skillCategory    string
	exportFiles      []string
	pasted           *string
	activateRoutines bool
	botNames         map[string]bool
	claimedSlugs     map[string]string
	exported         map[string]map[string]bool
	loadBots         func() []sourcedBot
	importShared     func() error
}

// BotOptions are the settings of a Grok Bot or OpenBot import
type BotOptions struct {
	Execute          bool
	Overwrite        bool
	ActivateRoutines bool
	BotNames         []string
}

func newBotImporter(ctx context.Context, agent, product, category string, exportFiles []string, targetRoot string, o BotOptions, pasted *string) *BotImporter {
	sourceRoot := userHome()
	switch {
	case pasted != nil:
		sourceRoot = Pasted
	case len(exportFiles) > 0:
		sourceRoot = filepath.Dir(exportFiles[0])
	}
	names := map[string]bool{}
	for _, n := range o.BotNames {
		if s := strings.TrimSpace(n); s != "" {
			names[strings.ToLower(s)] = true
		}
	}
	return &BotImporter{
		base: base{ctx: ctx, agent: agent, sourceRoot: sourceRoot, targetRoot: targetRoot, execute: o.Execute,
			overwrite: o.Overwrite, syncSkills: map[string]any{}},
		product: product, skillCategory: category, exportFiles: exportFiles, pasted: pasted,
		activateRoutines: o.ActivateRoutines, botNames: names, claimedSlugs: map[string]string{},
		exported: map[string]map[string]bool{},
	}
}

// NewGrokBotImporter builds the importer of Grok Bot export files, pasted holding a reply read from standard input
func NewGrokBotImporter(ctx context.Context, exportFiles []string, targetRoot string, o BotOptions, pasted *string) *BotImporter {
	g := newBotImporter(ctx, grokAgent, "Grok Bot", grokSkillCategory, exportFiles, targetRoot, o, pasted)
	g.loadBots = g.loadExportBots
	g.importShared = g.checkAgainstAppCache
	return g
}

// Run imports every bot the sources hold, only those --bot names when given
func (g *BotImporter) Run() (*Report, error) {
	bots := g.loadBots()
	if len(g.botNames) > 0 {
		var picked []sourcedBot
		for _, sb := range bots {
			if g.botNames[botKey(sb.bot)] {
				picked = append(picked, sb)
			}
		}
		bots = picked
		if len(bots) == 0 {
			g.record("export", "", "", "skipped", "No bot or agent matches --bot", nil)
			return g.buildReport(), nil
		}
	}
	for _, sb := range bots {
		if err := g.importBot(sb.path, sb.bot, len(bots) == 1); err != nil {
			return nil, err
		}
	}
	if err := g.importShared(); err != nil {
		return nil, err
	}
	return g.buildReport(), nil
}

// Flags Bots the Grok Bot app caches that no export covered and routines a Bot announced but left out
func (g *BotImporter) checkAgainstAppCache() error {
	cache := ReadGrokBotCache()
	var missing []string
	for _, b := range cache.Bots {
		if _, ok := g.exported[strings.ToLower(b)]; !ok {
			missing = append(missing, b)
		}
	}
	if len(missing) > 0 && len(g.botNames) == 0 {
		g.record("bots", "Grok Bot app", "", "skipped", "Not in the export, export these Bots too to bring them over: "+strings.Join(missing, ", "), nil)
	}
	for _, bot := range cache.RoutineBots {
		have, known := g.exported[strings.ToLower(bot)]
		var leftOut []string
		for _, r := range cache.Routines[bot] {
			if known && !have[slug(r)] {
				leftOut = append(leftOut, r)
			}
		}
		if len(leftOut) > 0 {
			g.record("routine-check", "Grok Bot app: "+bot, "", "skipped",
				fmt.Sprintf("%s created routine(s) its export did not include: %s. Ask the Bot to export again, naming them", bot, strings.Join(leftOut, ", ")), nil)
		}
	}
	return nil
}

// Bots of every export file, problems recorded, the same Bot across files imported once
func (g *BotImporter) loadExportBots() []sourcedBot {
	var found []sourcedBot
	for _, path := range g.exportFiles {
		var text string
		if g.pasted != nil && path == Pasted {
			text = *g.pasted
		} else {
			t, err := readText(path)
			if err != nil {
				g.record("export", path, "", "error", pyErrText(err), nil)
				continue
			}
			text = t
		}
		parsed, err := ParseExport(text)
		if err != nil {
			g.record("export", path, "", "error", err.Error(), nil)
			continue
		}
		total := 0
		for _, bot := range parsed {
			red, n := RedactSecrets(bot)
			total += n
			found = append(found, sourcedBot{path: path, bot: red.(*jsonutil.Object)})
		}
		if total > 0 {
			g.strippedSecrets = append(g.strippedSecrets, fmt.Sprintf("%s: %d token-like value(s) redacted", filepath.Base(path), total))
		}
	}
	origin := map[string]string{}
	all := make([]*jsonutil.Object, 0, len(found))
	for _, sb := range found {
		origin[botKey(sb.bot)] = sb.path
		all = append(all, sb.bot)
	}
	var out []sourcedBot
	for _, b := range MergeBots(all) {
		out = append(out, sourcedBot{path: origin[botKey(b)], bot: b})
	}
	return out
}

func (g *BotImporter) importBot(path string, bot *jsonutil.Object, single bool) error {
	name := strings.TrimSpace(pyStr(bot.Value("name")))
	slugs := map[string]bool{}
	for _, r := range dictsOf(bot.Value("routines")) {
		label := ""
		if truthy(r.Value("name")) {
			label = pyStr(r.Value("name"))
		}
		slugs[slug(label)] = true
	}
	g.exported[strings.ToLower(name)] = slugs
	source := filepath.Base(path) + ": " + name
	if err := g.importRole(source, bot, single); err != nil {
		return err
	}
	memories := filepath.Join(g.targetRoot, "memories")
	if err := g.mergeList("memories", source, filepath.Join(memories, "MEMORY.md"), stringsOf(bot.Value("memories"))); err != nil {
		return err
	}
	if err := g.mergeList("user-facts", source, filepath.Join(memories, "USER.md"), stringsOf(bot.Value("user_facts"))); err != nil {
		return err
	}
	skills, err := g.importSkills(source, dictsOf(bot.Value("skills")))
	if err != nil {
		return err
	}
	if err := g.importRoutines(source, name, dictsOf(bot.Value("routines")), skills); err != nil {
		return err
	}
	if err := g.importApps(source, dictsOf(bot.Value("apps"))); err != nil {
		return err
	}
	if servers, ok := asObject(bot.Value("mcp_servers")); ok && servers.Len() > 0 {
		return g.importMCPServers(servers, "mcp-servers")
	}
	return nil
}

func (g *BotImporter) mergeList(kind, source, destination string, entries []string) error {
	if len(entries) == 0 {
		g.record(kind, source, destination, "skipped", "No "+strings.ReplaceAll(kind, "-", " ")+" found", nil)
		return nil
	}
	return g.mergeMemoryEntries(kind, source, destination, entries)
}

// Reports whether SOUL.md still holds a seeded default nobody edited
func isSeededSoul(text string) bool {
	return strings.TrimSpace(text) == strings.TrimSpace(config.DefaultIdentity) || config.IsLegacyTemplateSoul(text)
}

// The role becomes SOUL.md while that is seeded and one Bot imports, else its lines join the memory entries
func (g *BotImporter) importRole(source string, bot *jsonutil.Object, single bool) error {
	name := strings.TrimSpace(pyStr(bot.Value("name")))
	title, instructions := "", ""
	if truthy(bot.Value("title")) {
		title = strings.TrimSpace(pyStr(bot.Value("title")))
	}
	if truthy(bot.Value("instructions")) {
		instructions = strings.TrimSpace(pyStr(bot.Value("instructions")))
	}
	if instructions == "" {
		g.record("role", source, "", "skipped", "No instructions found", nil)
		return nil
	}
	soul := filepath.Join(g.targetRoot, "SOUL.md")
	current := ""
	if isFile(soul) {
		text, err := readText(soul)
		if err != nil {
			return err
		}
		current = text
	}
	if strings.Contains(current, instructions) {
		g.record("role", source, soul, "skipped", "Already in SOUL.md", nil)
		return nil
	}
	if single && (strings.TrimSpace(current) == "" || isSeededSoul(current) || g.overwrite) {
		heading := "# " + name
		if title != "" {
			heading += " — " + title
		}
		write := func() (string, error) {
			if strings.TrimSpace(current) != "" {
				if err := atomicWriteText(g.ctx, soul+".bak."+g.agent, current); err != nil {
					return "", err
				}
			}
			return "", atomicWriteText(g.ctx, soul, heading+"\n\n"+instructions+"\n")
		}
		return g.apply("role", source, soul, "Would write the Bot's role to SOUL.md", write, nil)
	}
	var entries []string
	for _, e := range ExtractMarkdownEntries(instructions) {
		entries = append(entries, g.product+" "+name+": "+e)
	}
	return g.mergeMemoryEntries("role", source, filepath.Join(g.targetRoot, "memories", "MEMORY.md"), entries)
}

// Writes each skill and returns export skill name lowercased to the imported skill name
func (g *BotImporter) importSkills(source string, skills []*jsonutil.Object) (map[string]string, error) {
	root := filepath.Join(g.targetRoot, "skills", g.skillCategory)
	mapped := map[string]string{}
	if len(skills) == 0 {
		g.record("skills", source, root, "skipped", "No skills found", nil)
		return mapped, nil
	}
	for _, skill := range skills {
		label := ""
		if truthy(skill.Value("name")) {
			label = strings.TrimSpace(pyStr(skill.Value("name")))
		}
		folder := ""
		if truthy(skill.Value("path")) {
			folder = pyStr(skill.Value("path"))
		}
		if folder != "" && !isFile(filepath.Join(folder, "SKILL.md")) {
			g.record("skill", source+" / "+label, "", "skipped", "No SKILL.md in "+folder, nil)
			continue
		}
		hasText := truthy(skill.Value("instructions")) && strings.TrimSpace(pyStr(skill.Value("instructions"))) != ""
		if label == "" || (folder == "" && !hasText) {
			shown := label
			if shown == "" {
				shown = "?"
			}
			g.record("skill", source+" / "+shown, "", "skipped", "Skill has no name or no instructions", nil)
			continue
		}
		identity := folder
		if identity == "" {
			identity = anyutil.String(skill.Value("instructions"))
		}
		name := g.uniqueSlug(slug(label), strings.ToLower(label)+"\x00"+identity)
		destination := filepath.Join(root, name)
		if exists(destination) && !g.overwrite {
			g.record("skill", source+" / "+label, destination, "conflict", "Destination skill already exists", nil)
			mapped[strings.ToLower(label)] = name
			continue
		}
		dst, src, sk := destination, folder, skill
		write := func() (string, error) {
			if src != "" {
				if exists(dst) {
					if err := os.RemoveAll(dst); err != nil {
						return "", err
					}
				}
				ignore := func(_ string, names []string) map[string]bool {
					skip := map[string]bool{}
					for _, n := range names {
						if n == ".openbot-managed.json" {
							skip[n] = true
						}
					}
					return skip
				}
				if err := fsutil.CopyTree(src, dst, fsutil.CopyTreeOptions{Ignore: ignore}); err != nil {
					return "", err
				}
				return "", renameSkill(g.ctx, filepath.Join(dst, "SKILL.md"), filepath.Base(dst))
			}
			if err := os.MkdirAll(dst, 0o777); err != nil {
				return "", err
			}
			return "", atomicWriteText(g.ctx, filepath.Join(dst, "SKILL.md"), skillMarkdown(sk, filepath.Base(dst)))
		}
		would := "Would write SKILL.md"
		if folder != "" {
			would = "Would copy skill directory"
		}
		if err := g.apply("skill", source+" / "+label, destination, would, write, nil); err != nil {
			return nil, err
		}
		mapped[strings.ToLower(label)] = name
	}
	return mapped, nil
}

// Two different skills whose names slug alike get -2, -3 and the same skill seen twice keeps one name
func (g *BotImporter) uniqueSlug(s, identity string) string {
	candidate, n := s, 2
	for {
		owner, claimed := g.claimedSlugs[candidate]
		if !claimed || owner == identity {
			break
		}
		candidate = fmt.Sprintf("%s-%d", s, n)
		n++
	}
	g.claimedSlugs[candidate] = identity
	return candidate
}

// Scheduled routines become cron jobs, paused unless activating, event routines saved as notes
func (g *BotImporter) importRoutines(source, botName string, routines []*jsonutil.Object, skills map[string]string) error {
	if len(routines) == 0 {
		g.record("routines", source, "", "skipped", "No routines found", nil)
		return nil
	}
	jobs, err := cron.LoadJobsForHome(g.ctx, g.targetRoot)
	if err != nil {
		return err
	}
	existing := map[string]bool{}
	for _, j := range jobs {
		existing[j.Name] = true
	}
	planned := map[string]bool{}
	var unscheduled []*jsonutil.Object
	for _, routine := range routines {
		label := ""
		if truthy(routine.Value("name")) {
			label = strings.TrimSpace(pyStr(routine.Value("name")))
		}
		if label == "" {
			label = "routine"
		}
		jobName := botName + ": " + label
		if !existing[jobName] {
			baseName, n := jobName, 2
			for planned[jobName] {
				jobName = fmt.Sprintf("%s (%d)", baseName, n)
				n++
			}
		}
		where := "cron job '" + jobName + "'"
		instruction := ""
		if truthy(routine.Value("instruction")) {
			instruction = strings.TrimSpace(pyStr(routine.Value("instruction")))
		}
		if instruction == "" {
			g.record("routine", source+" / "+label, "", "skipped", "Routine has no instruction", nil)
			continue
		}
		schedule, ok := g.routineSchedule(routine)
		if !ok {
			unscheduled = append(unscheduled, routine)
			continue
		}
		if existing[jobName] {
			g.record("routine", source+" / "+label, where, "skipped", "A cron job with this name already exists (imported before?)", nil)
			continue
		}
		planned[jobName] = true
		jobSkills := []string{}
		for _, s := range stringsOf(routine.Value("skills")) {
			if mapped, ok := skills[strings.ToLower(s)]; ok {
				jobSkills = append(jobSkills, mapped)
			}
		}
		paused := !g.activateRoutines || truthy(routine.Value("paused"))
		zone := validZone(routine)
		details := map[string]any{"schedule": schedule, "paused": paused, "skills": jobSkills}
		if note := zoneNote(routine); note != "" {
			details["timezone_note"] = note
			where += " (" + note + ")"
		}
		opts := cron.CreateJobOptions{Prompt: &instruction, Schedule: schedule, Name: strPtr(jobName), Paused: paused}
		if len(jobSkills) > 0 {
			opts.Skills = append([]string(nil), jobSkills...)
		}
		if paused {
			opts.PausedReason = strPtr(fmt.Sprintf("Imported from %s: resume once the %s routine is off", g.product, g.product))
		}
		if zone != "" {
			opts.Timezone = zone
		}
		create := func() (string, error) {
			job, err := cron.CreateJob(g.ctx, g.targetRoot, opts)
			if err != nil {
				return "", err
			}
			details["job_id"] = job.ID
			return "", nil
		}
		would := fmt.Sprintf("Would create a cron job (%s)", schedule)
		if paused {
			would = fmt.Sprintf("Would create a paused cron job (%s)", schedule)
		}
		if err := g.apply("routine", source+" / "+label, where, would, create, details); err != nil {
			return err
		}
	}
	if len(unscheduled) > 0 {
		return g.noteEventRoutines(source, botName, unscheduled)
	}
	return nil
}

func strPtr(s string) *string { return &s }

// Schedule string cron accepts for a routine, false for an event triggered one
func (g *BotImporter) routineSchedule(routine *jsonutil.Object) (string, bool) {
	trigger := "schedule"
	if truthy(routine.Value("trigger")) {
		trigger = pyStr(routine.Value("trigger"))
	}
	if !scheduleTrigger[strings.ToLower(strings.TrimSpace(trigger))] {
		return "", false
	}
	for _, key := range []string{"schedule_cron", "schedule_text"} {
		candidate := ""
		if truthy(routine.Value(key)) {
			candidate = strings.TrimSpace(pyStr(routine.Value(key)))
		}
		if candidate == "" {
			continue
		}
		if _, err := cron.ParseSchedule(g.targetRoot, candidate); err != nil {
			continue
		}
		return candidate, true
	}
	return "", false
}

// Time zone of a routine when it names a real IANA zone, else empty
func validZone(routine *jsonutil.Object) string {
	tz := ""
	if truthy(routine.Value("timezone")) {
		tz = strings.TrimSpace(pyStr(routine.Value("timezone")))
	}
	if tz == "" {
		return ""
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return ""
	}
	return tz
}

// Says when the job runs on the profile clock because the routine zone is unknown
func zoneNote(routine *jsonutil.Object) string {
	tz := ""
	if truthy(routine.Value("timezone")) {
		tz = strings.TrimSpace(pyStr(routine.Value("timezone")))
	}
	if tz == "" || validZone(routine) != "" {
		return ""
	}
	return "unknown time zone " + tz + ", runs in this profile's time zone"
}

// Event triggered routines saved in full where the user can rebuild them
func (g *BotImporter) noteEventRoutines(source, botName string, routines []*jsonutil.Object) error {
	destination := filepath.Join(g.targetRoot, g.agent+"-import", slug(botName)+"-event-routines.md")
	lines := []string{"# " + botName + ": " + g.product + " routines that run on an event", "",
		"These start on something happening instead of a schedule, so they were not turned into " +
			"cron jobs. Recreate each in Icebath (ask Icebath to set it up from this file).", ""}
	for _, r := range routines {
		name := "routine"
		if truthy(r.Value("name")) {
			name = pyStr(r.Value("name"))
		}
		trigger := "unknown"
		if truthy(r.Value("trigger")) {
			trigger = pyStr(r.Value("trigger"))
		}
		detail := "n/a"
		switch {
		case truthy(r.Value("trigger_details")):
			detail = pyStr(r.Value("trigger_details"))
		case truthy(r.Value("schedule_text")):
			detail = pyStr(r.Value("schedule_text"))
		}
		instruction := ""
		if truthy(r.Value("instruction")) {
			instruction = pyStr(r.Value("instruction"))
		}
		lines = append(lines, "## "+strings.TrimSpace(name), "", "- Trigger: "+trigger, "- Details: "+detail, "",
			strings.TrimSpace(instruction), "")
	}
	text := strings.Join(lines, "\n")
	if isFile(destination) {
		current, err := readText(destination)
		if err != nil {
			return err
		}
		if current == text {
			g.record("event-routines", source, destination, "skipped", "Notes already saved", nil)
			return nil
		}
		if !g.overwrite {
			g.record("event-routines", source, destination, "conflict", "Notes file exists and was edited", nil)
			return nil
		}
	}
	write := func() (string, error) {
		if err := os.MkdirAll(filepath.Dir(destination), 0o777); err != nil {
			return "", err
		}
		return "", atomicWriteText(g.ctx, destination, text)
	}
	return g.apply("event-routines", source, destination,
		fmt.Sprintf("Would save %d routine(s) with no schedule as notes to recreate", len(routines)), write, nil)
}

// Apps with a remote MCP URL become MCP servers and the others are listed to connect by hand
func (g *BotImporter) importApps(source string, apps []*jsonutil.Object) error {
	if len(apps) == 0 {
		return nil
	}
	servers := jsonutil.NewObject()
	for _, a := range apps {
		url := ""
		if truthy(a.Value("mcp_url")) {
			url = pyStr(a.Value("mcp_url"))
		}
		if strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://") {
			servers.Set(slug(anyutil.String(a.Value("name"))), jsonutil.ObjectOf("url", strings.TrimSpace(pyStr(a.Value("mcp_url")))))
		}
	}
	if servers.Len() > 0 {
		if err := g.importMCPServers(servers, "mcp-servers"); err != nil {
			return err
		}
	}
	var others []string
	for _, a := range apps {
		name := ""
		if truthy(a.Value("name")) {
			name = strings.TrimSpace(pyStr(a.Value("name")))
		}
		if _, isServer := servers.Get(slug(anyutil.String(a.Value("name")))); !isServer && name != "" {
			others = append(others, name)
		}
	}
	if len(others) > 0 {
		g.record("apps", source, "", "skipped", fmt.Sprintf("Connect these in Icebath yourself (their logins stay in %s): ", g.product)+strings.Join(others, ", "), nil)
	}
	return nil
}
