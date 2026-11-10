package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"
	"weak"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func titleLog() *slog.Logger { return slog.Default().With("logger", "agent.title_generator") }

const (
	maxTitleInputChars       = 1000
	pastePreviewLabel        = "\n\nPasted content:\n"
	maxDerivedTitleChars     = 48
	maxTitleWords            = 12
	titleMaxTokens           = 512
	promptVagueExample       = "Code changes"
	provisionalGreetingTitle = "friendly greeting"
	languageRuleMatchUser    = "- Write the title in the same language as the user's message."
	languageRulePinned       = "- Write the title in {language}."
)

var (
	attachmentRefRe          = mustPyRe(`@(?:file|folder):\S+`)
	contextFooterRe          = mustPyRe(`(?s)\n+--- (?:Context Warnings|Attached Context) ---\n.*`)
	contextReferenceTokenRe  = mustPyRe(`(?<![\w/])@(?:file|folder):(?:` + "(?:`[^`\\n]+`|\"[^\"\\n]+\"|'[^'\\n]+')" + `(?::\d+(?:-\d+)?)?|\S+)`)
	fencedTitleRe            = mustPyRe("(?s)^```(?:json)?\\s*(.*?)\\s*```$")
	looseJSONTitleRe         = mustPyRe(`"title\"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	titleEdgeRe              = mustPyRe(`^[\W_]+|[\W_]+$`)
	promptGoodExamples       = []string{"Fix login button on mobile", "Postgres connection pool exhaustion", "Friendly greeting"}
	exampleEchoReject        = buildExampleEchoReject()
	titlePromptTemplate      = buildTitlePromptTemplate()
	titleControlWrapperTags  = []string{"command-message", "command-name", "command-args", "local-command-caveat", "local-command-stderr", "local-command-stdout", "task-notification", "system-reminder", "ide_opened_file", "ide_selection"}
	titleMachinePrefixes     = []string{"[CONTEXT COMPACTION", state.LegacySummaryPrefix, "[Runtime note:", "[System note:", "[SYSTEM]", "[System: The active model for this chat has changed to "}
	titleUpgradeMu           sync.Mutex
	titleUpgradeThreads      = map[weak.Pointer[memory.ContextThread]]struct{}{}
	titleCallLLM             = provider.CallLLM
	autoTitleSessionFn       = autoTitleSession
	generateTitleFn          = generateTitle
	maybeAutoTitleFn         = maybeAutoTitle
	titleLoadConfig          = loadTitleConfigTree
	titleConfigFn            = titleConfig
	titleGreetingTitleChecks = []string{provisionalGreetingTitle, provisionalGreetingTitle + " in chat"}
)

// Lowered prompt examples a model echo is rejected for, the greeting example excepted
func buildExampleEchoReject() map[string]bool {
	out := map[string]bool{pystr.Lower(promptVagueExample): true}
	for _, t := range promptGoodExamples {
		if t != "Friendly greeting" {
			out[pystr.Lower(t)] = true
		}
	}
	return out
}

// Title prompt with the language rule slot and the example lines rendered from the example constants
func buildTitlePromptTemplate() string {
	var b strings.Builder
	b.WriteString("You name chat sessions. Given the user's opening message, write a title " +
		"that lets them find this conversation again in a list.\n\n" +
		"Rules:\n" +
		"- 3 to 7 words, sentence case (capitalize only the first word and proper nouns).\n" +
		"- Name what the user wants DONE, not that they asked a question.\n" +
		"- Keep technical terms, filenames, numbers, and error codes exact.\n" +
		"- Drop filler words: the, this, my, a, an.\n" +
		"- No trailing punctuation, no quotes, no tool names, no 'Title:' prefix.\n" +
		"- Never answer the message. Name it.\n" +
		"- Always produce something, even for a bare greeting.\n" +
		"__LANGUAGE_RULE__\n")
	for _, t := range promptGoodExamples {
		b.WriteString(`Good: {"title": "` + t + "\"}\n")
	}
	b.WriteString(`Too vague: {"title": "` + promptVagueExample + "\"}\n" +
		`Too long: {"title": "Investigate and fix the issue where the login button ` +
		"does not respond on mobile devices\"}\n\n" +
		`Reply with JSON only: {"title": "..."}`)
	return b.String()
}

// Response format constraining the reply to one title field
func titleResponseFormat() map[string]any {
	return map[string]any{
		"type": "json_schema",
		"json_schema": map[string]any{"name": "session_title", "strict": true, "schema": map[string]any{
			"type": "object", "properties": map[string]any{"title": map[string]any{"type": "string"}}, "required": []any{"title"}, "additionalProperties": false}},
	}
}

// Read only config of the active profile
func loadTitleConfigTree(ctx context.Context) (map[string]any, error) {
	tree, err := config.LoadReadonly(ctx, home.Dir(ctx), os.LookupEnv)
	if err != nil {
		return nil, err
	}
	return tree, nil
}

// Mapping view of a config node, false for any other value
func titleConfigMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case config.Tree:
		return m, true
	}
	return nil, false
}

// AttributeError Python raises calling get on a non mapping value
func titleNoGetError(v any) error {
	return &pyint.Error{Kind: "AttributeError", Msg: "'" + pyrepr.TypeName(v) + "' object has no attribute 'get'"}
}

// auxiliary.title_generation mapping, empty when unset, an error where a level is a non mapping value
func titleConfig(ctx context.Context) (map[string]any, error) {
	cfg, err := titleLoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	node := map[string]any(cfg)
	for _, key := range []string{"auxiliary", "title_generation"} {
		v := node[key]
		if !pyrepr.Truthy(v) {
			return map[string]any{}, nil
		}
		m, ok := titleConfigMap(v)
		if !ok {
			return nil, titleNoGetError(v)
		}
		node = m
	}
	return node, nil
}

// Configured title language, empty to match the user
func titleLanguage(ctx context.Context) string {
	cfg, err := titleConfigFn(ctx)
	if err != nil {
		return ""
	}
	v, ok := cfg["language"]
	if !ok {
		v = ""
	}
	return pystr.Strip(pyrepr.Str(v))
}

// Whether automatic titling is on, true when the config cannot be read
func autoTitleEnabled(ctx context.Context) bool {
	cfg, err := titleConfigFn(ctx)
	if err != nil {
		titleLog().Debug("Failed to read title_generation.enabled", "error", err)
		return true
	}
	return isTruthyValue(cfg["enabled"], true)
}

// Whether the background model upgrade of the instant title runs, true when the config cannot be read
func modelTitleUpgradeEnabled(ctx context.Context) bool {
	cfg, err := titleConfigFn(ctx)
	if err != nil {
		titleLog().Debug("Failed to read title_generation.model_upgrade_enabled", "error", err)
		return true
	}
	return isTruthyValue(cfg["model_upgrade_enabled"], true)
}

// Python str of a value after an or empty string fallback
func titleStrOr(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// True when the title call would share the turn's own self hosted endpoint and must wait for the turn
func titleUpgradeMustWaitForTurn(ctx context.Context, mainRuntime map[string]any) bool {
	providerName := pystr.Lower(pystr.Strip(titleStrOr(mainRuntime["provider"])))
	if !titleIsSelfHostedProvider(ctx, providerName) {
		return false
	}
	cfg, err := titleConfigFn(ctx)
	if err != nil {
		return true
	}
	pinnedProvider := pystr.Lower(pystr.Strip(titleStrOr(cfg["provider"])))
	mainBaseURL := pystr.RstripChars(pystr.Strip(titleStrOr(mainRuntime["base_url"])), "/")
	if pinnedProvider != "" && pinnedProvider != "auto" {
		shares, err := titlePinMayShareEndpoint(ctx, pinnedProvider, providerName, mainBaseURL)
		if err != nil {
			return true
		}
		if !shares {
			return false
		}
	}
	pinnedBaseURL := pystr.RstripChars(pystr.Strip(titleStrOr(cfg["base_url"])), "/")
	return pinnedBaseURL == "" || pinnedBaseURL == mainBaseURL
}

// Custom, a named custom route, LM Studio or local after alias normalisation
func titleIsSelfHostedProvider(ctx context.Context, name string) bool {
	name = provider.NormalizeProviderDef(ctx, name)
	return name == "custom" || name == "lmstudio" || name == "local" || strings.HasPrefix(name, "custom:")
}

// True when a title pin can land on the turn's own self hosted server
func titlePinMayShareEndpoint(ctx context.Context, pinnedProvider, mainProvider, mainBaseURL string) (bool, error) {
	if titleIsSelfHostedProvider(ctx, pinnedProvider) {
		return true, nil
	}
	mainAliases := provider.CustomProviderAliases(mainProvider, "")
	for alias := range provider.CustomProviderAliases(pinnedProvider, "") {
		if mainAliases[alias] {
			return true, nil
		}
	}
	cfg, err := titleLoadConfig(ctx)
	if err != nil {
		return false, err
	}
	compatible := provider.CompatibleCustomProviders(cfg)
	entries := make([]any, len(compatible))
	for i, e := range compatible {
		entries[i] = e
	}
	pdef := provider.ResolveCustomProvider(pinnedProvider, entries)
	return pdef != nil && mainBaseURL != "" && pystr.RstripChars(pystr.Strip(pdef.BaseURL), "/") == mainBaseURL, nil
}

// Registers a started upgrade weakly, false when it was already started
func registerTitleUpgrade(t *memory.ContextThread) bool {
	wp := weak.Make(t)
	titleUpgradeMu.Lock()
	defer titleUpgradeMu.Unlock()
	if _, ok := titleUpgradeThreads[wp]; ok {
		return false
	}
	titleUpgradeThreads[wp] = struct{}{}
	runtime.AddCleanup(t, func(wp weak.Pointer[memory.ContextThread]) {
		titleUpgradeMu.Lock()
		delete(titleUpgradeThreads, wp)
		titleUpgradeMu.Unlock()
	}, wp)
	return true
}

// Whether the upgrade went through start and is still referenced
func titleUpgradeStarted(t *memory.ContextThread) bool {
	titleUpgradeMu.Lock()
	defer titleUpgradeMu.Unlock()
	_, ok := titleUpgradeThreads[weak.Make(t)]
	return ok
}

// Joins the title upgrades still running within one shared deadline
func waitForTitleUpgrades(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	titleUpgradeMu.Lock()
	threads := make([]*memory.ContextThread, 0, len(titleUpgradeThreads))
	for wp := range titleUpgradeThreads {
		if t := wp.Value(); t != nil {
			threads = append(threads, t)
		}
	}
	titleUpgradeMu.Unlock()
	for _, t := range threads {
		t.Join(max(0, time.Until(deadline)))
	}
}

// Starts a deferred title upgrade once, making it joinable by the upgrade waiter
func startTitleUpgrade(upgrade any) {
	t, _ := upgrade.(*memory.ContextThread)
	if t == nil || !registerTitleUpgrade(t) {
		return
	}
	t.Start()
}

// Code points of s from start to the end
func titleFrom(s string, start int) string { return pystr.Slice(s, start, math.MaxInt) }

// Python re.sub of every match with an empty string
func titleReSub(re *regexp2.Regexp, s string) string {
	out, _ := re.Replace(s, "", -1, -1)
	return out
}

// Removes leading control wrappers including nested ones so a slash command turn keeps its typed prose
func stripControlWrappers(text string) string {
	current := pystr.Strip(text)
	for range len(titleControlWrapperTags) * 2 {
		stripped := stripOneWrapper(current)
		if stripped == current {
			break
		}
		current = stripped
	}
	return current
}

// Text with one leading control wrapper removed, trailing prose preferred over the wrapper body
func stripOneWrapper(text string) string {
	lowered := pystr.Lower(text)
	for _, tag := range titleControlWrapperTags {
		openTag, closeTag := "<"+tag+">", "</"+tag+">"
		if !strings.HasPrefix(lowered, openTag) {
			continue
		}
		at := strings.Index(lowered, closeTag)
		if at == -1 {
			return pystr.Strip(titleFrom(text, len(openTag)))
		}
		end := pystr.Len(lowered[:at])
		kept := pystr.Strip(titleFrom(text, end+len(closeTag)))
		if kept == "" {
			kept = pystr.Strip(pystr.Slice(text, len(openTag), end))
		}
		return pystr.Strip(kept)
	}
	return text
}

// True when only context references and their expansion footer remain in the message
func attachmentOnlyOpener(message string) bool {
	residual := titleReSub(contextReferenceTokenRe, titleReSub(contextFooterRe, message))
	return pystr.Strip(residual) == ""
}

// Text worth titling, a skill invocation described as typed and control wrappers stripped
func summarizeUserMessage(userMessage string) string {
	if userMessage == "" {
		return ""
	}
	if described, ok := skills.DescribeSkillInvocation(userMessage, " — "); ok {
		return stripControlWrappers(described)
	}
	return stripControlWrappers(userMessage)
}

// Opening text combined with a bounded paste preview inside the title input budget
func buildTitleInput(userMessage string, titlePreview *string) string {
	message := summarizeUserMessage(userMessage)
	preview := ""
	if titlePreview != nil {
		preview = pystr.Strip(*titlePreview)
	}
	if preview == "" {
		return pystr.Slice(message, 0, maxTitleInputChars)
	}
	message = pystr.Strip(titleReSub(contextFooterRe, message))
	if pystr.Strip(titleReSub(attachmentRefRe, message)) == "" {
		return pystr.Slice(preview, 0, maxTitleInputChars)
	}
	messageBudget := min(pystr.Len(message), maxTitleInputChars/2)
	previewBudget := maxTitleInputChars - messageBudget - pystr.Len(pastePreviewLabel)
	if previewBudget <= 0 {
		return pystr.Slice(message, 0, maxTitleInputChars)
	}
	return pystr.Slice(message, 0, messageBudget) + pastePreviewLabel + pystr.Slice(preview, 0, previewBudget)
}

// False for machine authored openers, scaffolding only turns and attachment only openers
func isTitleableUserMessage(userMessage string) bool {
	if pystr.Strip(userMessage) == "" {
		return false
	}
	trimmed := pystr.Lstrip(userMessage)
	for _, prefix := range titleMachinePrefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return false
		}
	}
	return pystr.Strip(summarizeUserMessage(userMessage)) != "" && !attachmentOnlyOpener(userMessage)
}

// Instant title from the first meaningful line cut at a word boundary, empty when nothing is titleable
func deriveTitle(userMessage string, titlePreview *string) string {
	if (titlePreview == nil || *titlePreview == "") && attachmentOnlyOpener(userMessage) {
		return ""
	}
	line := strings.Join(pystr.SplitWhitespace(titleFirstLine(buildTitleInput(userMessage, titlePreview)), -1), " ")
	if pystr.Len(line) > maxDerivedTitleChars {
		cut := pystr.Slice(line, 0, maxDerivedTitleChars)
		space := -1
		if at := strings.LastIndex(cut, " "); at >= 0 {
			space = pystr.Len(cut[:at])
		}
		if space > maxDerivedTitleChars/2 {
			cut = pystr.Slice(cut, 0, space)
		}
		line = pystr.RstripChars(cut, " ,.;:—-") + "…"
	}
	return line
}

// Text without a leading case insensitive Title prefix
func stripTitlePrefix(text string) string {
	if strings.HasPrefix(pystr.Lower(text), "title:") {
		return pystr.Strip(titleFrom(text, 6))
	}
	return text
}

// First non blank line stripped, empty when there is none
func titleFirstLine(text string) string {
	for _, ln := range pystr.Splitlines(text, false) {
		if s := pystr.Strip(ln); s != "" {
			return s
		}
	}
	return ""
}

// Title of a title payload by strict parse then a loose field scan, false when absent
func extractJSONTitle(raw string) (string, bool) {
	if parsed, err := pyjson.Loads(raw); err == nil {
		if obj, ok := parsed.(*pyjson.Object); ok {
			if v, ok := obj.Get("title"); ok {
				if s, ok := v.(string); ok {
					return pystr.Strip(s), true
				}
			}
		}
	}
	m, _ := looseJSONTitleRe.FindStringMatch(raw)
	if m == nil {
		return "", false
	}
	group := m.GroupByNumber(1).String()
	if v, err := pyjson.Loads(`"` + group + `"`); err == nil {
		if s, ok := v.(string); ok {
			return pystr.Strip(s), true
		}
	}
	return pystr.Strip(group), true
}

// True for structured output cut before its closing quote, brace or fence
func isTruncatedStructuredOutput(raw string) bool {
	for _, p := range []string{`{"`, `["`, "[{"} {
		if strings.HasPrefix(raw, p) {
			return true
		}
	}
	return strings.HasPrefix(raw, "```") && strings.Count(raw, "```")%2 == 1
}

// Title text by strict JSON, loose JSON, then first line prose, truncated structured output dropped
func extractTitleText(content string) string {
	if content == "" {
		return ""
	}
	raw := pystr.Strip(content)
	if m, _ := fencedTitleRe.FindStringMatch(raw); m != nil {
		raw = pystr.Strip(m.GroupByNumber(1).String())
	}
	if title, ok := extractJSONTitle(raw); ok {
		return title
	}
	if isTruncatedStructuredOutput(raw) {
		return ""
	}
	raw = pystr.Strip((*AIAgent)(nil).stripThinkBlocks(raw))
	return pystr.Strip(pystr.StripChars(stripTitlePrefix(titleFirstLine(raw)), "\"'"))
}

// Title payload a reasoning model left in its reasoning fields, never reasoning prose
func titleFromReasoning(message any) string {
	for _, field := range []string{"reasoning_content", "reasoning"} {
		text, ok := provider.MessageField(message, field).(string)
		if ok && pystr.Strip(text) != "" {
			if title, found := extractJSONTitle(pystr.Strip(text)); found && title != "" {
				return title
			}
		}
	}
	return ""
}

// Normalized model title capped at 80 characters, empty when nothing usable remains
func cleanTitle(text string) string {
	collapsed := strings.Join(pystr.SplitWhitespace(text, -1), " ")
	title := pystr.RstripChars(stripTitlePrefix(pystr.Strip(pystr.StripChars(collapsed, "\"'"))), ".!,;:")
	if pystr.Len(title) > 80 {
		title = pystr.Rstrip(pystr.Slice(title, 0, 77)) + "..."
	}
	return title
}

// Calls an optional consumer callback with args, logging any panic or unusable callback instead
func titleSafeCallback(callback any, args []any, logFmt, label string) {
	defer func() {
		if r := recover(); r != nil {
			titleLog().Debug(fmt.Sprintf(logFmt, label), "error", fmt.Sprint(r))
		}
	}()
	if callback == nil {
		return
	}
	fn := reflect.ValueOf(callback)
	if fn.Kind() != reflect.Func {
		panic(fmt.Sprintf("'%s' object is not callable", pyrepr.TypeName(callback)))
	}
	if fn.IsNil() {
		return
	}
	in := make([]reflect.Value, len(args))
	for i, a := range args {
		in[i] = reflect.ValueOf(a)
	}
	fn.Call(in)
}

// Hands a titling failure to the failure callback
func reportFailure(failureCallback any, err error, label string) {
	titleSafeCallback(failureCallback, []any{"title generation", err}, "%s failure_callback raised", label)
}

// Hands a persisted title and its provenance to the title callback
func notifyTitle(titleCallback any, title, source, label string) {
	titleSafeCallback(titleCallback, []any{title, source}, "%s callback failed", label)
}

// Lowered title with leading and trailing non word runs removed
func normalizeTitleEdges(title string) string {
	return pystr.Lower(titleReSub(titleEdgeRe, pystr.Strip(title)))
}

// True for the provisional greeting title the prompt asks for on a topicless opener
func isProvisionalGreetingTitle(title string) bool {
	normalized := normalizeTitleEdges(title)
	for _, g := range titleGreetingTitleChecks {
		if normalized == g {
			return true
		}
	}
	return false
}

// True when the title is one of the prompt's own example titles echoed back
func isPromptExampleEcho(title string) bool {
	return exampleEchoReject[normalizeTitleEdges(title)]
}

// Runs fn and turns a panic into its returned error the way an except Exception clause catches
func titleTry(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = e
			} else {
				err = fmt.Errorf("%v", r)
			}
		}
	}()
	return fn()
}

type generateTitleOptions struct {
	Timeout          *float64
	FailureCallback  any
	MainRuntime      map[string]any
	RuntimeValidator func() bool
	TitlePreview     *string
}

// Runtime validator verdict, proceeding when it panics
func titleRuntimeAllows(validator func() bool) (allowed bool) {
	defer func() {
		if r := recover(); r != nil {
			titleLog().Debug("Title runtime validator raised; proceeding", "error", fmt.Sprint(r))
			allowed = true
		}
	}()
	if validator() {
		return true
	}
	titleLog().Debug("Title generation skipped: runtime validator returned False")
	return false
}

// First choice message of an auxiliary response, an error where Python indexing would raise
func titleResponseMessage(response any) (any, error) { return provider.FirstChoiceMessage(response) }

// One title model call with the answer shaped and example echo guards, empty when rejected
func requestTitle(ctx context.Context, prompt, snippet string, o generateTitleOptions) (string, error) {
	var timeout any
	if o.Timeout != nil {
		timeout = *o.Timeout
	}
	response, err := titleCallLLM(ctx, map[string]any{
		"task":       "title_generation",
		"messages":   []any{map[string]any{"role": "system", "content": prompt}, map[string]any{"role": "user", "content": snippet}},
		"max_tokens": titleMaxTokens, "temperature": nil, "timeout": timeout, "main_runtime": o.MainRuntime,
		"extra_body":       map[string]any{"response_format": titleResponseFormat()},
		"reasoning_config": map[string]any{"enabled": false},
	})
	if err != nil {
		return "", err
	}
	message, err := titleResponseMessage(response)
	if err != nil {
		return "", err
	}
	content := ""
	if v := provider.MessageField(message, "content"); pyrepr.Truthy(v) {
		s, ok := v.(string)
		if !ok {
			return "", &pyint.Error{Kind: "AttributeError", Msg: "'" + pyrepr.TypeName(v) + "' object has no attribute 'strip'"}
		}
		content = s
	}
	text := extractTitleText(content)
	if text == "" {
		text = titleFromReasoning(message)
	}
	title := cleanTitle(text)
	if words := len(pystr.SplitWhitespace(title, -1)); title != "" && words > maxTitleWords {
		titleLog().Debug(fmt.Sprintf("Rejecting answer-shaped title output (%d words > %d)", words, maxTitleWords))
		return "", nil
	}
	if title != "" && isPromptExampleEcho(title) {
		titleLog().Debug("Rejecting prompt-example echo title: " + pyrepr.StrRepr(title))
		return "", nil
	}
	return title, nil
}

// Model title from the opening message alone, empty when disabled, skipped, rejected or failed
func generateTitle(ctx context.Context, userMessage string, o generateTitleOptions) string {
	if !autoTitleEnabled(ctx) {
		titleLog().Debug("Auto-title skipped: auxiliary.title_generation.enabled=false")
		return ""
	}
	if o.RuntimeValidator != nil && !titleRuntimeAllows(o.RuntimeValidator) {
		return ""
	}
	snippet := buildTitleInput(userMessage, o.TitlePreview)
	noPreview := o.TitlePreview == nil || *o.TitlePreview == ""
	if pystr.Strip(snippet) == "" || (noPreview && attachmentOnlyOpener(snippet)) {
		return ""
	}
	rule := languageRuleMatchUser
	if language := titleLanguage(ctx); language != "" {
		rule = strings.ReplaceAll(languageRulePinned, "{language}", language)
	}
	prompt := strings.ReplaceAll(titlePromptTemplate, "__LANGUAGE_RULE__", rule)
	var title string
	err := titleTry(func() error {
		var err error
		title, err = requestTitle(ctx, prompt, snippet, o)
		return err
	})
	if err != nil {
		titleLog().Warn("Title generation failed: " + err.Error())
		titleLog().Debug("Title generation traceback", "error", err)
		reportFailure(o.FailureCallback, err, "Title generation")
		return ""
	}
	return title
}

type titleSourceReader interface {
	GetSessionTitleSource(ctx context.Context, sessionID string) (string, bool, error)
}

type titleReader interface {
	GetSessionTitle(ctx context.Context, sessionID string) (string, bool, error)
}

type autoTitleWriter interface {
	SetAutoTitleContext(ctx context.Context, sessionID, title, source string) (bool, error)
}

type legacyAutoTitleWriter interface {
	SetAutoTitleIfEmpty(ctx context.Context, sessionID, title string) (bool, error)
}

type sessionTitleSetter interface {
	SetSessionTitle(ctx context.Context, sessionID, title string) (bool, error)
}

type titleLineage interface {
	GetNextTitleInLineage(ctx context.Context, baseTitle string) (string, error)
}

type conversationRootReader interface {
	GetConversationRoot(sessionID string) (string, error)
}

// Session store as an interface value, a nil state DB pointer standing for no store
func titleStore(db any) any {
	if d, ok := db.(*state.DB); ok && d == nil {
		return nil
	}
	return db
}

// True when the session already carries an llm or user title, or the check fails
func hasUpgradedTitle(ctx context.Context, db any, sessionID string) (upgraded bool) {
	defer func() {
		if r := recover(); r != nil {
			upgraded = true
		}
	}()
	if r, ok := db.(titleSourceReader); ok {
		source, has, err := r.GetSessionTitleSource(ctx, sessionID)
		if err != nil {
			return true
		}
		return has && source != state.TitleSourceDerived
	}
	r, ok := db.(titleReader)
	if !ok {
		return true
	}
	title, _, err := r.GetSessionTitle(ctx, sessionID)
	if err != nil {
		return true
	}
	return title != ""
}

// True for a ValueError raised by the session store
func isTitleValueError(err error) bool {
	var pe *pyint.Error
	return errors.As(err, &pe) && pe.Kind == "ValueError"
}

// Stored title at the source authority, empty when outranked, a name collision retried with a lineage suffix
func persistSessionTitle(ctx context.Context, db any, sessionID, title, source string, dedupe bool) (string, error) {
	set := func(candidate string) (string, error) {
		if w, ok := db.(autoTitleWriter); ok {
			done, err := w.SetAutoTitleContext(ctx, sessionID, candidate, source)
			if err != nil {
				return "", err
			}
			if done {
				return candidate, nil
			}
			titleLog().Debug(fmt.Sprintf("Skipping %s title: a higher-authority title already holds session %s", source, sessionID))
			return "", nil
		}
		if w, ok := db.(legacyAutoTitleWriter); ok {
			done, err := w.SetAutoTitleIfEmpty(ctx, sessionID, candidate)
			if err != nil || !done {
				return "", err
			}
			return candidate, nil
		}
		w, ok := db.(sessionTitleSetter)
		if !ok {
			return "", &pyint.Error{Kind: "AttributeError", Msg: "'" + pyrepr.TypeName(db) + "' object has no attribute 'set_session_title'"}
		}
		done, err := w.SetSessionTitle(ctx, sessionID, candidate)
		if err != nil {
			return "", err
		}
		if !done {
			return "", fmt.Errorf("session %s not found when storing title", sessionID)
		}
		return candidate, nil
	}
	persisted, err := set(title)
	if err == nil || !isTitleValueError(err) {
		return persisted, err
	}
	deduped := ""
	if l, ok := db.(titleLineage); ok && dedupe {
		next, lerr := l.GetNextTitleInLineage(ctx, title)
		if lerr != nil {
			return "", lerr
		}
		deduped = next
	}
	if deduped == "" || deduped == title {
		return "", err
	}
	return set(deduped)
}

// Titles a delegate run with a Subagent prefix at derived authority without a model call, never failing
func applySubagentTitle(ctx context.Context, db any, sessionID, goal string) string {
	db = titleStore(db)
	var persisted string
	err := titleTry(func() error {
		derived := ""
		if isTitleableUserMessage(goal) {
			derived = deriveTitle(goal, nil)
		}
		if derived == "" {
			return nil
		}
		var err error
		persisted, err = persistSessionTitle(ctx, db, sessionID, "Subagent: "+derived, state.TitleSourceDerived, true)
		return err
	})
	if err != nil {
		titleLog().Debug(fmt.Sprintf("Subagent title failed for %s", sessionID), "error", err)
		return ""
	}
	return persisted
}

// Writes the derived title inline and notifies it, empty when nothing usable or a title already exists
func applyInstantTitle(ctx context.Context, db any, sessionID, userMessage string, titleCallback any, titlePreview *string) string {
	db = titleStore(db)
	if db == nil || sessionID == "" {
		return ""
	}
	var persisted string
	err := titleTry(func() error {
		title := ""
		if isTitleableUserMessage(userMessage) {
			title = deriveTitle(userMessage, titlePreview)
		}
		if title != "" {
			var err error
			if persisted, err = persistSessionTitle(ctx, db, sessionID, title, state.TitleSourceDerived, false); err != nil {
				return err
			}
		}
		if persisted != "" {
			notifyTitle(titleCallback, persisted, state.TitleSourceDerived, "Instant-title")
		}
		return nil
	})
	if err != nil {
		titleLog().Debug("Instant title failed", "error", err)
		return ""
	}
	return persisted
}

type autoTitleSessionOptions struct {
	FailureCallback  any
	MainRuntime      map[string]any
	TitleCallback    any
	RuntimeValidator func() bool
	TitlePreview     *string
}

// Generates and stores the model title in the background, upgrading only a derived title and never failing
func autoTitleSession(ctx context.Context, db any, sessionID, userMessage string, o autoTitleSessionOptions) {
	db = titleStore(db)
	err := titleTry(func() error {
		if db == nil || sessionID == "" || hasUpgradedTitle(ctx, db, sessionID) {
			return nil
		}
		conversationID := sessionID
		if r, ok := db.(conversationRootReader); ok {
			_ = titleTry(func() error {
				root, err := r.GetConversationRoot(sessionID)
				if err == nil && root != "" {
					conversationID = root
				}
				return err
			})
		}
		ctx = provider.WithPortalConversationContext(ctx, conversationID)
		recorder, _ := db.(provider.AuxUsageRecorder)
		ctx = provider.SetAccountingContext(ctx, recorder, sessionID)
		title, source := generateTitleFn(ctx, userMessage, generateTitleOptions{FailureCallback: o.FailureCallback, MainRuntime: o.MainRuntime,
			RuntimeValidator: o.RuntimeValidator, TitlePreview: o.TitlePreview}), state.TitleSourceLLM
		if title != "" && isProvisionalGreetingTitle(title) {
			source = state.TitleSourceDerived
		}
		if title == "" {
			title, source = deriveTitle(userMessage, o.TitlePreview), state.TitleSourceDerived
		}
		if title == "" {
			return nil
		}
		persisted, err := persistSessionTitle(ctx, db, sessionID, title, source, true)
		if err != nil {
			titleLog().Debug("Failed to set auto-generated title: " + err.Error())
			return nil
		}
		if persisted != "" {
			titleLog().Debug("Auto-generated session title: " + persisted)
			notifyTitle(o.TitleCallback, persisted, source, "Auto-title")
		}
		return nil
	})
	if err != nil {
		titleLog().Warn("Auto-title failed (harmless; if this started after an update, restart the running icebath process): " + err.Error())
		titleLog().Debug("Auto-title traceback", "error", err)
		reportFailure(o.FailureCallback, err, "Auto-title")
	}
}

// True for a question a person actually asked rather than machinery persisted as a user row
func titleIsRealUserTurn(message *state.Message) bool {
	if message == nil || message.Role != "user" {
		return false
	}
	if s, ok := message.Content.(string); ok {
		return isTitleableUserMessage(s)
	}
	return isTitleableUserMessage(state.FlattenMessageText(message.Content, "\n"))
}

// True when the session has no title of any provenance, false when that cannot be told
func sessionIsUntitled(ctx context.Context, db any, sessionID string) bool {
	r, ok := db.(titleReader)
	if !ok {
		return false
	}
	var title string
	err := titleTry(func() error {
		var err error
		title, _, err = r.GetSessionTitle(ctx, sessionID)
		return err
	})
	if err != nil {
		titleLog().Debug(fmt.Sprintf("Untitled check failed for %s", sessionID), "error", err)
		return false
	}
	return pystr.Strip(title) == ""
}

// Kanban worker card title or Kanban task id when the board cannot be read, empty outside a dispatched worker
func kanbanTaskTitle(ctx context.Context, env *tools.Env) string {
	taskID := pystr.Strip(os.Getenv("ICEBATH_KANBAN_TASK"))
	if taskID == "" || !tools.IsDispatcherOwnedWorkerContext(env) {
		return ""
	}
	title := ""
	err := titleTry(func() error {
		return kanban.ConnectClosing(ctx, "", nil, func(conn state.SQLiteConn) error {
			task, err := kanban.GetTask(ctx, conn, taskID)
			if err != nil {
				return err
			}
			if task != nil {
				title = strings.Join(pystr.SplitWhitespace(task.Title, -1), " ")
			}
			return nil
		})
	})
	if err != nil {
		titleLog().Debug(fmt.Sprintf("Kanban task %s unreadable; naming the session after its id", taskID), "error", err)
		title = ""
	} else if limit := state.MaxTitleLength - 4; pystr.Len(title) > limit {
		title = pystr.Rstrip(pystr.Slice(title, 0, limit-1)) + "…"
	}
	if title == "" {
		return "Kanban task " + taskID
	}
	return title
}

type autoTitleOptions struct {
	ConversationHistory []*state.Message
	FailureCallback     any
	MainRuntime         map[string]any
	TitleCallback       any
	RuntimeValidator    func() bool
	TitlePreview        *string
	Env                 *tools.Env
}

// Instant inline title then a background model upgrade, the upgrade returned unstarted when it must wait for the turn
func maybeAutoTitle(ctx context.Context, db any, sessionID, userMessage string, o autoTitleOptions) (any, bool) {
	db = titleStore(db)
	if db == nil || sessionID == "" || userMessage == "" {
		return nil, false
	}
	userTurns := 0
	for _, m := range o.ConversationHistory {
		if titleIsRealUserTurn(m) {
			userTurns++
		}
	}
	if userTurns > 1 && (hasUpgradedTitle(ctx, db, sessionID) || (userTurns > 3 && !sessionIsUntitled(ctx, db, sessionID))) {
		return nil, false
	}
	if cardTitle := kanbanTaskTitle(ctx, o.Env); cardTitle != "" {
		_ = titleTry(func() error {
			persisted, err := persistSessionTitle(ctx, db, sessionID, cardTitle, state.TitleSourceLLM, true)
			if err == nil && persisted != "" {
				notifyTitle(o.TitleCallback, persisted, state.TitleSourceLLM, "Kanban task title")
			}
			return err
		})
		return nil, false
	}
	if !isTitleableUserMessage(userMessage) {
		return nil, false
	}
	if !autoTitleEnabled(ctx) {
		titleLog().Debug("Auto-title skipped: auxiliary.title_generation.enabled=false")
		return nil, false
	}
	applyInstantTitle(ctx, db, sessionID, userMessage, o.TitleCallback, o.TitlePreview)
	if !modelTitleUpgradeEnabled(ctx) {
		titleLog().Debug("Instant title persisted; model upgrade disabled by auxiliary.title_generation.model_upgrade_enabled=false")
		return nil, false
	}
	upgradeOpts := autoTitleSessionOptions{FailureCallback: o.FailureCallback, MainRuntime: o.MainRuntime, TitleCallback: o.TitleCallback,
		RuntimeValidator: o.RuntimeValidator}
	if o.TitlePreview != nil && pystr.Strip(*o.TitlePreview) != "" {
		upgradeOpts.TitlePreview = o.TitlePreview
	}
	target := autoTitleSessionFn
	upgrade := memory.SpawnContextThread(ctx, "auto-title", func(ctx context.Context) {
		target(ctx, db, sessionID, userMessage, upgradeOpts)
	})
	if titleUpgradeMustWaitForTurn(ctx, o.MainRuntime) {
		titleLog().Debug("Auto-title upgrade deferred past the turn: shares the self-hosted endpoint with the main request")
		return upgrade, false
	}
	startTitleUpgrade(upgrade)
	return upgrade, true
}
