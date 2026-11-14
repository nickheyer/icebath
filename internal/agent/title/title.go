// Names sessions from their opening message with an instant derived title and a side-model upgrade
package title

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model"
	"github.com/nickheyer/icebath/internal/model/catalog"
	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/state"
	"github.com/nickheyer/icebath/internal/tool"
)

const (
	maxInputChars       = 1000
	maxDerivedChars     = 48
	maxTitleWords       = 12
	titleMaxTokens      = 512
	maxCleanChars       = 80
	pastePreviewLabel   = "\n\nPasted content:\n"
	provisionalGreeting = "friendly greeting"
	legacySummaryPrefix = "[CONTEXT SUMMARY]:"
	upgradeJoinTimeout  = 10 * time.Second
	failureTask         = "title generation"
	sideTask            = "title_generation"
	languageRuleMatch   = "- Write the title in the same language as the user's message."
	languageRuleMarker  = "__LANGUAGE_RULE__"
	responseFormat      = `{"type": "json_schema", "json_schema": {"name": "session_title", "strict": true, "schema": {"type": "object", "properties": {"title": {"type": "string"}}, "required": ["title"], "additionalProperties": false}}}`
)

var (
	promptGoodExamples = []string{"Fix login button on mobile", "Postgres connection pool exhaustion", "Friendly greeting"}
	promptVagueExample = "Code changes"
	promptTemplate     = buildPromptTemplate()
	exampleEchoReject  = buildEchoReject()

	controlWrapperTags = []string{"command-message", "command-name", "command-args", "local-command-caveat", "local-command-stderr",
		"local-command-stdout", "task-notification", "system-reminder", "ide_opened_file", "ide_selection"}

	machinePrefixes = []string{"[CONTEXT COMPACTION", legacySummaryPrefix, "[Runtime note:", "[System note:", "[SYSTEM]",
		"[System: The active model for this chat has changed to "}
)

const (
	spaceClass = `\t\n\v\f\r \x{1c}-\x{1f}\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`
	spaceRun   = `[` + spaceClass + `]*`
	nonSpace   = `[^` + spaceClass + `]`
	quotedRef  = "(?:`[^`\\n]+`|\"[^\"\\n]+\"|'[^'\\n]+')"
)

var (
	attachmentRefRe = regexp.MustCompile(`@(?:file|folder):` + nonSpace + `+`)
	contextFooterRe = regexp.MustCompile(`(?s)\n+--- (?:Context Warnings|Attached Context) ---\n.*`)
	referenceTokRe  = regexp.MustCompile(`^@(?:file|folder):(?:` + quotedRef + `(?::\p{Nd}+(?:-\p{Nd}+)?)?|` + nonSpace + `+)`)
	fencedRe        = regexp.MustCompile("(?s)^```(?:json)?" + spaceRun + "(.*?)" + spaceRun + "```$")
	looseTitleRe    = regexp.MustCompile(`"title"` + spaceRun + `:` + spaceRun + `"((?:[^"\\]|\\.)*)"`)
	errEmptyReply   = errors.New("title generation returned no response")
)

func buildPromptTemplate() string {
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
		languageRuleMarker + "\n")
	for _, t := range promptGoodExamples {
		b.WriteString(`Good: {"title": "` + t + `"}` + "\n")
	}
	b.WriteString(`Too vague: {"title": "` + promptVagueExample + `"}` + "\n")
	b.WriteString(`Too long: {"title": "Investigate and fix the issue where the login button ` +
		`does not respond on mobile devices"}` + "\n\n")
	b.WriteString(`Reply with JSON only: {"title": "..."}`)
	return b.String()
}

func buildEchoReject() []string {
	var out []string
	for _, t := range promptGoodExamples {
		if t != "Friendly greeting" {
			out = append(out, strings.ToLower(t))
		}
	}
	return append(out, strings.ToLower(promptVagueExample))
}

type Skills interface {
	DescribeInvocation(content string) (string, bool)
}

type Tasks interface {
	TaskTitle(ctx context.Context, taskID string) (string, bool, error)
}

type Options struct {
	Store          *state.Store
	Config         *config.Store
	Catalog        *catalog.Catalog
	Side           tool.SideModel
	Skills         Skills
	StripReasoning func(text string) string
	Tasks          Tasks
}

type Service struct {
	o       Options
	mu      sync.Mutex
	running map[*Upgrade]struct{}
}

func New(o Options) (*Service, error) {
	switch {
	case o.Store == nil:
		return nil, &tool.UnfilledError{Name: "title.Options.Store"}
	case o.Config == nil:
		return nil, &tool.UnfilledError{Name: "title.Options.Config"}
	case o.Catalog == nil:
		return nil, &tool.UnfilledError{Name: "title.Options.Catalog"}
	case o.Side == nil:
		return nil, &tool.UnfilledError{Name: "title.Options.Side"}
	case o.Skills == nil:
		return nil, &tool.UnfilledError{Name: "title.Options.Skills"}
	case o.StripReasoning == nil:
		return nil, &tool.UnfilledError{Name: "title.Options.StripReasoning"}
	}
	return &Service{o: o, running: map[*Upgrade]struct{}{}}, nil
}

type Turn struct {
	SessionID  string
	UserText   string
	Preview    string
	History    []wire.Message
	KanbanTask string
	Main       *model.Route
	Current    func() bool
	OnTitle    func(title, source string)
	OnFailure  func(task string, err error)
}

type Upgrade struct {
	s       *Service
	ctx     context.Context
	t       Turn
	once    sync.Once
	started atomic.Bool
	done    chan struct{}
}

func (u *Upgrade) Start() {
	if u == nil {
		return
	}
	u.once.Do(func() {
		u.started.Store(true)
		u.s.mu.Lock()
		u.s.running[u] = struct{}{}
		u.s.mu.Unlock()
		go func() {
			defer func() {
				u.s.mu.Lock()
				delete(u.s.running, u)
				u.s.mu.Unlock()
				close(u.done)
			}()
			u.s.upgrade(u.ctx, u.t)
		}()
	})
}

func (u *Upgrade) Started() bool { return u != nil && u.started.Load() }

func (s *Service) Begin(ctx context.Context, t Turn) (*Upgrade, error) {
	if t.SessionID == "" || t.UserText == "" {
		return nil, nil
	}
	turns := 0
	for i := range t.History {
		if t.History[i].Role == wire.RoleUser && s.Titleable(t.History[i].FlatText("\n")) {
			turns++
		}
	}
	if turns > 1 && (s.hasUpgradedTitle(ctx, t.SessionID) || (turns > 3 && !s.untitled(ctx, t.SessionID))) {
		return nil, nil
	}
	if task := trimSpace(t.KanbanTask); task != "" {
		return nil, s.kanbanTitle(ctx, t, task)
	}
	if !s.Titleable(t.UserText) {
		return nil, nil
	}
	cfg := s.titleConfig(ctx)
	if !cfg.Enabled {
		slog.DebugContext(ctx, "Auto-title skipped: auxiliary.title_generation.enabled=false")
		return nil, nil
	}
	if _, _, err := s.Instant(ctx, t.SessionID, t.UserText, t.Preview, t.OnTitle); err != nil {
		slog.DebugContext(ctx, "Instant title failed", "session", t.SessionID, "err", err)
	}
	if !cfg.ModelUpgradeEnabled {
		slog.DebugContext(ctx, "Instant title persisted; model upgrade disabled by auxiliary.title_generation.model_upgrade_enabled=false")
		return nil, nil
	}
	up := t
	if trimSpace(up.Preview) == "" {
		up.Preview = ""
	}
	up.History = nil
	u := &Upgrade{s: s, ctx: context.WithoutCancel(ctx), t: up, done: make(chan struct{})}
	if s.MustWaitForTurn(ctx, t.Main) {
		slog.DebugContext(ctx, "Auto-title upgrade deferred past the turn: shares the self-hosted endpoint with the main request")
		return u, nil
	}
	u.Start()
	return u, nil
}

func (s *Service) Wait(ctx context.Context) error {
	bounded, cancel := context.WithTimeout(ctx, upgradeJoinTimeout)
	defer cancel()
	s.mu.Lock()
	pending := make([]chan struct{}, 0, len(s.running))
	for u := range s.running {
		pending = append(pending, u.done)
	}
	s.mu.Unlock()
	for _, done := range pending {
		select {
		case <-done:
		case <-bounded.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (s *Service) Instant(ctx context.Context, sessionID, userText, preview string, onTitle func(title, source string)) (string, bool, error) {
	if sessionID == "" || !s.Titleable(userText) {
		return "", false, nil
	}
	title, ok := s.Derive(userText, preview)
	if !ok {
		return "", false, nil
	}
	persisted, ok, err := s.persist(ctx, sessionID, title, state.TitleSourceDerived, false)
	if err != nil || !ok {
		return "", false, err
	}
	if onTitle != nil {
		onTitle(persisted, state.TitleSourceDerived)
	}
	return persisted, true, nil
}

func (s *Service) Subagent(ctx context.Context, sessionID, goal string) (string, bool, error) {
	if !s.Titleable(goal) {
		return "", false, nil
	}
	derived, ok := s.Derive(goal, "")
	if !ok {
		return "", false, nil
	}
	return s.persist(ctx, sessionID, "Subagent: "+derived, state.TitleSourceDerived, true)
}

func (s *Service) Generate(ctx context.Context, userText string) (string, bool) {
	return s.generate(ctx, userText, "", nil, nil)
}

func (s *Service) MustWaitForTurn(ctx context.Context, main *model.Route) bool {
	provider, baseURL := "", ""
	if main != nil {
		provider = strings.ToLower(trimSpace(main.Provider))
		baseURL = strings.TrimRight(trimSpace(main.BaseURL), "/")
	}
	if !s.selfHosted(ctx, provider) {
		return false
	}
	cfg, err := s.o.Config.LoadReadonly(ctx)
	if err != nil {
		return true
	}
	tg := cfg.Auxiliary.TitleGeneration
	pinned := strings.ToLower(trimSpace(tg.Provider))
	if pinned != "" && pinned != "auto" {
		share, err := s.pinMayShare(ctx, pinned, provider, baseURL)
		if err != nil {
			return true
		}
		if !share {
			return false
		}
	}
	pinnedBase := strings.TrimRight(trimSpace(tg.BaseURL), "/")
	return pinnedBase == "" || pinnedBase == baseURL
}

func (s *Service) selfHosted(ctx context.Context, provider string) bool {
	p := s.o.Catalog.NormalizeProvider(ctx, provider)
	return p == "custom" || p == "lmstudio" || p == "local" || strings.HasPrefix(p, "custom:")
}

func (s *Service) pinMayShare(ctx context.Context, pinned, mainProvider, mainBaseURL string) (bool, error) {
	if s.selfHosted(ctx, pinned) {
		return true, nil
	}
	mainAliases := catalog.CustomProviderAliases(mainProvider, "")
	for _, a := range catalog.CustomProviderAliases(pinned, "") {
		if slices.Contains(mainAliases, a) {
			return true, nil
		}
	}
	custom, err := s.o.Config.CustomEndpoints(ctx)
	if err != nil {
		return false, err
	}
	def, ok := catalog.ResolveCustomProvider(pinned, custom)
	return ok && mainBaseURL != "" && strings.TrimRight(trimSpace(def.BaseURL), "/") == mainBaseURL, nil
}

func (s *Service) Titleable(text string) bool {
	if trimSpace(text) == "" {
		return false
	}
	lead := strings.TrimLeftFunc(text, isSpace)
	for _, p := range machinePrefixes {
		if strings.HasPrefix(lead, p) {
			return false
		}
	}
	return trimSpace(s.summarize(text)) != "" && !attachmentOnly(text)
}

func (s *Service) Derive(text, preview string) (string, bool) {
	if preview == "" && attachmentOnly(text) {
		return "", false
	}
	line := strings.Join(fields(firstLine(s.buildInput(text, preview))), " ")
	if r := []rune(line); len(r) > maxDerivedChars {
		cut := r[:maxDerivedChars]
		if space := lastSpace(cut); space > maxDerivedChars/2 {
			cut = cut[:space]
		}
		line = strings.TrimRight(string(cut), " ,.;:—-") + "…"
	}
	return line, line != ""
}

func (s *Service) summarize(text string) string {
	if text == "" {
		return ""
	}
	if described, ok := s.o.Skills.DescribeInvocation(text); ok {
		text = described
	}
	return stripControlWrappers(text)
}

func (s *Service) buildInput(text, preview string) string {
	message := s.summarize(text)
	preview = trimSpace(preview)
	if preview == "" {
		return head(message, maxInputChars)
	}
	message = trimSpace(contextFooterRe.ReplaceAllString(message, ""))
	if trimSpace(attachmentRefRe.ReplaceAllString(message, "")) == "" {
		return head(preview, maxInputChars)
	}
	messageBudget := min(utf8.RuneCountInString(message), maxInputChars/2)
	previewBudget := maxInputChars - messageBudget - utf8.RuneCountInString(pastePreviewLabel)
	if previewBudget <= 0 {
		return head(message, maxInputChars)
	}
	return head(message, messageBudget) + pastePreviewLabel + head(preview, previewBudget)
}

func stripControlWrappers(text string) string {
	current := trimSpace(text)
	for range len(controlWrapperTags) * 2 {
		stripped := stripOneWrapper(current)
		if stripped == current {
			break
		}
		current = stripped
	}
	return current
}

func stripOneWrapper(text string) string {
	runes := []rune(text)
	lowered := make([]rune, len(runes))
	for i, r := range runes {
		lowered[i] = unicode.ToLower(r)
	}
	low := string(lowered)
	for _, tag := range controlWrapperTags {
		open, closing := "<"+tag+">", "</"+tag+">"
		if !strings.HasPrefix(low, open) {
			continue
		}
		openLen := len(open)
		end := strings.Index(low, closing)
		if end < 0 {
			return trimSpace(string(runes[openLen:]))
		}
		endRune := utf8.RuneCountInString(low[:end])
		if rest := trimSpace(string(runes[endRune+len(closing):])); rest != "" {
			return rest
		}
		return trimSpace(string(runes[openLen:endRune]))
	}
	return text
}

func attachmentOnly(message string) bool {
	return trimSpace(removeReferenceTokens(contextFooterRe.ReplaceAllString(message, ""))) == ""
}

func removeReferenceTokens(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		j := strings.IndexByte(s[i:], '@')
		if j < 0 {
			break
		}
		at := i + j
		if !wordOrSlashBefore(s, at) {
			if loc := referenceTokRe.FindStringIndex(s[at:]); loc != nil {
				b.WriteString(s[i:at])
				i = at + loc[1]
				continue
			}
		}
		b.WriteString(s[i : at+1])
		i = at + 1
	}
	b.WriteString(s[i:])
	return b.String()
}

func wordOrSlashBefore(s string, at int) bool {
	if at == 0 {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(s[:at])
	return r == '/' || isWord(r)
}

func (s *Service) titleConfig(ctx context.Context) config.AuxTitleGeneration {
	cfg, err := s.o.Config.LoadReadonly(ctx)
	if err != nil {
		slog.DebugContext(ctx, "Failed to read auxiliary.title_generation", "err", err)
		return config.AuxTitleGeneration{Enabled: true, ModelUpgradeEnabled: true}
	}
	return cfg.Auxiliary.TitleGeneration
}

func (s *Service) generate(ctx context.Context, userText, preview string, current func() bool, onFailure func(string, error)) (string, bool) {
	cfg := s.titleConfig(ctx)
	if !cfg.Enabled {
		slog.DebugContext(ctx, "Auto-title skipped: auxiliary.title_generation.enabled=false")
		return "", false
	}
	if current != nil && !current() {
		slog.DebugContext(ctx, "Title generation skipped: runtime validator returned False")
		return "", false
	}
	snippet := s.buildInput(userText, preview)
	if trimSpace(snippet) == "" || (preview == "" && attachmentOnly(snippet)) {
		return "", false
	}
	rule := languageRuleMatch
	if language := trimSpace(cfg.Language); language != "" {
		rule = "- Write the title in " + language + "."
	}
	resp, err := s.o.Side.Complete(ctx, sideTask, wire.Request{
		Messages: []wire.Message{
			{Role: wire.RoleSystem, Text: strings.ReplaceAll(promptTemplate, languageRuleMarker, rule)},
			{Role: wire.RoleUser, Text: snippet},
		},
		MaxTokens: titleMaxTokens,
		Reasoning: &wire.Reasoning{Enabled: false},
		ExtraBody: map[string]json.RawMessage{"response_format": json.RawMessage(responseFormat)},
	})
	if err == nil && resp == nil {
		err = errEmptyReply
	}
	if err != nil {
		slog.WarnContext(ctx, "Title generation failed: "+err.Error())
		if onFailure != nil {
			onFailure(failureTask, err)
		}
		return "", false
	}
	extracted := s.extractTitleText(resp.Message.FlatText(""))
	if extracted == "" {
		extracted = titleFromReasoning(&resp.Message)
	}
	title, ok := cleanTitle(extracted)
	if !ok {
		return "", false
	}
	if n := len(fields(title)); n > maxTitleWords {
		slog.DebugContext(ctx, "Rejecting answer-shaped title output", "words", n, "max", maxTitleWords)
		return "", false
	}
	if exampleEcho(title) {
		slog.DebugContext(ctx, "Rejecting prompt-example echo title", "title", title)
		return "", false
	}
	return title, true
}

func (s *Service) extractTitleText(content string) string {
	if content == "" {
		return ""
	}
	raw := trimSpace(content)
	if m := fencedRe.FindStringSubmatch(raw); m != nil {
		raw = trimSpace(m[1])
	}
	if title, ok := jsonTitle(raw); ok {
		return title
	}
	if truncatedStructured(raw) {
		return ""
	}
	raw = trimSpace(s.o.StripReasoning(raw))
	return trimSpace(strings.Trim(stripTitlePrefix(firstLine(raw)), "\"'"))
}

func jsonTitle(raw string) (string, bool) {
	var parsed any
	if json.Unmarshal([]byte(raw), &parsed) == nil {
		if m, ok := parsed.(map[string]any); ok {
			if t, ok := m["title"].(string); ok {
				return trimSpace(t), true
			}
		}
	}
	m := looseTitleRe.FindStringSubmatch(raw)
	if m == nil {
		return "", false
	}
	var decoded string
	if json.Unmarshal([]byte(`"`+m[1]+`"`), &decoded) == nil {
		return trimSpace(decoded), true
	}
	return trimSpace(m[1]), true
}

func truncatedStructured(raw string) bool {
	if strings.HasPrefix(raw, `{"`) || strings.HasPrefix(raw, `["`) || strings.HasPrefix(raw, "[{") {
		return true
	}
	return strings.HasPrefix(raw, "```") && strings.Count(raw, "```")%2 == 1
}

func titleFromReasoning(m *wire.Message) string {
	candidates := []string{"", m.Reasoning}
	if m.ReasoningContent != nil {
		candidates[0] = *m.ReasoningContent
	}
	for _, text := range candidates {
		if t := trimSpace(text); t != "" {
			if title, ok := jsonTitle(t); ok && title != "" {
				return title
			}
		}
	}
	return ""
}

func cleanTitle(text string) (string, bool) {
	t := trimSpace(strings.Trim(strings.Join(fields(text), " "), "\"'"))
	t = strings.TrimRight(stripTitlePrefix(t), ".!,;:")
	if r := []rune(t); len(r) > maxCleanChars {
		t = strings.TrimRightFunc(string(r[:maxCleanChars-3]), isSpace) + "..."
	}
	return t, t != ""
}

func stripTitlePrefix(text string) string {
	r := []rune(text)
	if len(r) >= 6 && strings.EqualFold(string(r[:6]), "title:") {
		return trimSpace(string(r[6:]))
	}
	return text
}

func normalizedTitle(title string) string {
	return strings.ToLower(strings.TrimFunc(trimSpace(title), func(r rune) bool { return !isAlnum(r) }))
}

func provisionalGreetingTitle(title string) bool {
	n := normalizedTitle(title)
	return n == provisionalGreeting || n == provisionalGreeting+" in chat"
}

func exampleEcho(title string) bool {
	return slices.Contains(exampleEchoReject, normalizedTitle(title))
}

func (s *Service) hasUpgradedTitle(ctx context.Context, sessionID string) bool {
	source, ok, err := s.o.Store.GetSessionTitleSource(ctx, sessionID)
	if err != nil {
		slog.DebugContext(ctx, "Title source check failed", "session", sessionID, "err", err)
		return true
	}
	return ok && source != state.TitleSourceDerived
}

func (s *Service) untitled(ctx context.Context, sessionID string) bool {
	title, _, err := s.o.Store.GetSessionTitle(ctx, sessionID)
	if err != nil {
		slog.DebugContext(ctx, "Untitled check failed", "session", sessionID, "err", err)
		return false
	}
	return trimSpace(title) == ""
}

func (s *Service) persist(ctx context.Context, sessionID, title, source string, dedupe bool) (string, bool, error) {
	persisted, ok, err := s.setTitle(ctx, sessionID, title, source)
	if err == nil || !titleRejected(err) || !dedupe {
		return persisted, ok, err
	}
	next, nerr := s.o.Store.GetNextTitleInLineage(ctx, title)
	if nerr != nil {
		return "", false, nerr
	}
	if next == "" || next == title {
		return "", false, err
	}
	return s.setTitle(ctx, sessionID, next, source)
}

func (s *Service) setTitle(ctx context.Context, sessionID, candidate, source string) (string, bool, error) {
	ok, err := s.o.Store.SetAutoTitle(ctx, sessionID, candidate, source)
	if err != nil {
		return "", false, err
	}
	if !ok {
		slog.DebugContext(ctx, "Skipping title: a higher-authority title already holds the session", "source", source, "session", sessionID)
		return "", false, nil
	}
	return candidate, true, nil
}

func titleRejected(err error) bool {
	var conflict *state.TitleConflictError
	var tooLong *state.TitleTooLongError
	return errors.As(err, &conflict) || errors.As(err, &tooLong)
}

func (s *Service) kanbanTitle(ctx context.Context, t Turn, task string) error {
	if s.o.Tasks == nil {
		return &tool.UnfilledError{Name: "title.Options.Tasks"}
	}
	title := ""
	raw, found, err := s.o.Tasks.TaskTitle(ctx, task)
	switch {
	case err != nil:
		slog.DebugContext(ctx, "Kanban task unreadable; naming the session after its id", "task", task, "err", err)
	case found:
		title = strings.Join(fields(raw), " ")
		limit := state.MaxTitleLength - 4
		if r := []rune(title); len(r) > limit {
			title = strings.TrimRightFunc(string(r[:limit-1]), isSpace) + "…"
		}
	}
	if title == "" {
		title = "Kanban task " + task
	}
	persisted, ok, err := s.persist(ctx, t.SessionID, title, state.TitleSourceLLM, true)
	if err != nil {
		slog.DebugContext(ctx, "Kanban task title failed", "session", t.SessionID, "err", err)
		return nil
	}
	if ok && t.OnTitle != nil {
		t.OnTitle(persisted, state.TitleSourceLLM)
	}
	return nil
}

func (s *Service) upgrade(ctx context.Context, t Turn) {
	if s.hasUpgradedTitle(ctx, t.SessionID) {
		return
	}
	source := state.TitleSourceLLM
	title, ok := s.generate(ctx, t.UserText, t.Preview, t.Current, t.OnFailure)
	if ok && provisionalGreetingTitle(title) {
		source = state.TitleSourceDerived
	}
	if !ok {
		title, ok = s.Derive(t.UserText, t.Preview)
		source = state.TitleSourceDerived
	}
	if !ok {
		return
	}
	persisted, ok, err := s.persist(ctx, t.SessionID, title, source, true)
	if err != nil {
		slog.DebugContext(ctx, "Failed to set auto-generated title", "err", err)
		return
	}
	if ok {
		slog.DebugContext(ctx, "Auto-generated session title", "title", persisted)
		if t.OnTitle != nil {
			t.OnTitle(persisted, source)
		}
	}
}

func isSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

func isLineBreak(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}

func isAlnum(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }

func isWord(r rune) bool { return r == '_' || isAlnum(r) }

func trimSpace(s string) string { return strings.TrimFunc(s, isSpace) }

func fields(s string) []string { return strings.FieldsFunc(s, isSpace) }

func firstLine(text string) string {
	for _, ln := range strings.FieldsFunc(text, isLineBreak) {
		if t := trimSpace(ln); t != "" {
			return t
		}
	}
	return ""
}

func head(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func lastSpace(r []rune) int {
	for i := len(r) - 1; i >= 0; i-- {
		if r[i] == ' ' {
			return i
		}
	}
	return -1
}
