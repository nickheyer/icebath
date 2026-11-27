package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/contexts"
	contextsconfig "github.com/nickheyer/icebath/internal/contexts/config"
	agentprompt "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/prompt"
	"github.com/nickheyer/icebath/internal/prompt"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/tools"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Fewest messages a compression asked for by the user works on
const manualMinimum = 4

// Compression the user asked for, run as the session's next piece of work
type compactJob struct {
	focus      string
	aggressive bool
	// Exchanges at the end kept whole, 0 for the whole chat
	keep int
}

// Arguments of /compress: whether to preview, whether to keep only the last exchange under the shortest summary, the exchanges kept whole
// when only the start of the chat is compressed, and what the summary should keep above all
func compressArgs(argument string) (preview, aggressive bool, keep int, focus string) {
	var rest []string
	for _, word := range strings.Fields(argument) {
		switch strings.ToLower(word) {
		case "--preview", "--dry-run", "--dryrun":
			preview = true
		case "--aggressive":
			aggressive = true
		default:
			rest = append(rest, word)
		}
	}
	text := strings.Join(rest, " ")
	lower := strings.ToLower(text)
	if strings.HasPrefix(lower, "up to here") {
		lower, text = lower[len("up to "):], text[len("up to "):]
	}
	words := strings.Fields(lower)
	switch {
	case len(words) == 0:
		return preview, aggressive, 0, ""
	case words[0] == "here":
		keep = keepLast
		if len(words) >= 2 {
			keep = keepCount(words[1])
		}
		return preview, aggressive, keep, ""
	case (words[0] == "--keep" || words[0] == "-k") && len(words) >= 2:
		return preview, aggressive, keepCount(words[1]), ""
	case strings.HasPrefix(words[0], "--keep="):
		return preview, aggressive, keepCount(strings.TrimPrefix(words[0], "--keep=")), ""
	}
	return preview, aggressive, 0, text
}

// Keep count of the argument, between one and the ceiling, digits of any script read as their values; the default when it is no number
func keepCount(value string) int {
	digits := strings.Map(func(r rune) rune {
		if r > unicode.MaxASCII && unicode.IsDigit(r) {
			for _, table := range unicode.Nd.R16 {
				if lo, hi := rune(table.Lo), rune(table.Hi); r >= lo && r <= hi {
					return '0' + (r-lo)/rune(table.Stride)%10
				}
			}
			for _, table := range unicode.Nd.R32 {
				if lo, hi := rune(table.Lo), rune(table.Hi); r >= lo && r <= hi {
					return '0' + (r-lo)/rune(table.Stride)%10
				}
			}
		}
		return r
	}, value)
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return keepLast
	}
	if errors.Is(err, strconv.ErrRange) && strings.HasPrefix(digits, "-") || n < 1 {
		return 1
	}
	return int(min(n, maxKeepLast))
}

// Compresses the session's conversation now, as the session's next piece of work, or shows what that would do
func (a *Agent) Compact(ctx context.Context, id, argument string) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	preview, aggressive, keep, focus := compressArgs(argument)
	if preview {
		return a.preview(ctx, r, keep, focus)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cx.running {
		return "⏳ Compression already in progress for this session. Please wait for it to finish.", nil
	}
	r.queue = append(r.queue, queued{compact: &compactJob{focus: focus, aggressive: aggressive, keep: keep}})
	r.life.admissions = append(r.life.admissions, "")
	if r.busy {
		return "Queued. Compression runs when the current reply finishes.", nil
	}
	r.mu.Unlock()
	err = a.start(r)
	r.mu.Lock()
	return "", err
}

// What a compression would cover, without changing anything
func (a *Agent) preview(ctx context.Context, r *run, keep int, focus string) (string, error) {
	store, err := a.store()
	if err != nil {
		return "", err
	}
	history, err := store.Messages(ctx, r.sessionID(), sessions.Model)
	if err != nil {
		return "", err
	}
	var shown []*sessionsv1.Message
	for _, m := range history {
		if role := m.GetRole(); (role == sessionsv1.Role_ROLE_USER || role == sessionsv1.Role_ROLE_ASSISTANT) && strings.TrimSpace(contexts.Text(m)) != "" {
			shown = append(shown, m)
		}
	}
	head, tail := shown, []*sessionsv1.Message(nil)
	if keep > 0 {
		head, tail = splitKeep(shown, keep)
	}
	lines := []string{"Preview — no changes made.", fmt.Sprintf("Would compress %d of %d message(s) (~%s tokens currently in context).", len(head),
		len(shown), strs.Thousands(requestTokens(nil, sent(shown), nil)))}
	switch {
	case keep > 0 && len(tail) > 0:
		lines = append(lines, fmt.Sprintf("Boundary: keeping the last %d exchange(s) (%d message(s)) verbatim.", keep, len(tail)))
	case keep > 0:
		lines = append(lines, "Boundary: 'here' split would keep everything — falling back to full compression.")
	}
	if focus != "" {
		lines = append(lines, fmt.Sprintf("Focus topic: %q", focus))
	}
	lines = append(lines, "Run the command again without --preview to apply.")
	for i, line := range lines {
		lines[i] = "🗜️ " + line
	}
	return strings.Join(lines, "\n"), nil
}

// Runs a compression the user asked for under the session's turn, telling them what came of it
func (a *Agent) compactNow(ctx context.Context, r *run, job *compactJob) error {
	store, err := a.store()
	if err != nil {
		return err
	}
	a.restore(r)
	settings := a.settings(r)
	instance, route, err := a.current(r)
	if err != nil {
		return errors.New("No provider configured -- cannot compress.")
	}
	aux := a.helpers(r, settings, route)
	env := a.contextEnv(ctx, r, settings, instance, route, aux)
	engine := a.engine(r, env)
	if engine == nil {
		return errors.New("No context engine is registered, so the conversation can't be compressed.")
	}
	history, err := store.Messages(ctx, env.Session, sessions.Model)
	if err != nil {
		return err
	}
	if len(history) < manualMinimum {
		a.tell(r, "Not enough conversation to compress (need at least 4 messages).")
		return nil
	}
	if !engine.Compressible(history) && job.keep == 0 && !job.aggressive {
		a.tell(r, "Nothing to compress yet (the transcript is still all protected context).")
		return nil
	}
	before := requestTokens(nil, sent(history), nil)
	a.tell(r, fmt.Sprintf("🗜️ Compressing %d messages (~%s tokens)...", len(history), strs.Thousands(before)))
	done, err := a.compress(ctx, r, settings, env, engine, compressing{trigger: "manual", tokens: before, focus: job.focus, force: true, bypass: true,
		aggressive: job.aggressive, keep: job.keep})
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		return fmt.Errorf("Compression failed: %s", maskedFailure(err.Error()))
	}
	switch done.reason {
	case "lock":
		r.mu.Lock()
		holder := strings.TrimPrefix(r.cx.deferred, "lock:")
		r.mu.Unlock()
		if holder != "" && holder != "unconfirmed" {
			a.tell(r, "⏳ Compression already in progress for this session (holder: "+holder+"). Please wait for it to finish.")
		} else {
			a.tell(r, "⏳ Compression skipped: could not acquire this session's compression lock. Another compression may still be running, or the "+
				"lock check failed — try again shortly.")
		}
		return nil
	case "saturated":
		a.tell(r, "⏳ Compression skipped: too many compressions are running right now. Try again shortly.")
		return nil
	case "timeout", "rotated":
		return nil
	}
	after := requestTokens(nil, sent(done.after), nil)
	a.settle(r, settings)
	a.tell(r, a.feedback(done, before, after, job.focus))
	return nil
}

// Headline, size and notes shown after a compression the user asked for
func (a *Agent) feedback(done compressed, before, after int, focus string) string {
	result := done.result
	if result == nil {
		result = &contexts.Result{}
	}
	countBefore, countAfter := len(done.before), len(done.after)
	unchanged := !done.stored
	var headline, note string
	switch {
	case done.reason == "refused":
		headline = fmt.Sprintf("Compression refused (summary would grow the conversation): %d messages preserved", countBefore)
		note = "The generated summary was larger than what it would replace; no messages were removed."
	case result.Aborted:
		headline = fmt.Sprintf("Compression aborted: %d messages preserved", countBefore)
		note = "Summary generation failed; no messages were removed."
	case result.Fallback && done.stored:
		headline = fmt.Sprintf("Compressed with fallback: %d → %d messages", countBefore, countAfter)
		dropped := cmp.Or(result.Dropped, max(countBefore-countAfter, 0))
		note = fmt.Sprintf("Summary generation failed; Icebath used limited fallback context and removed %d message(s).", dropped)
	case unchanged:
		headline = fmt.Sprintf("No changes from compression: %d messages", countBefore)
	default:
		headline = fmt.Sprintf("Compressed: %d → %d messages", countBefore, countAfter)
		if countAfter < countBefore && after > before {
			note = "Note: fewer messages can still raise this estimate when compression rewrites the transcript into denser summaries."
		}
	}
	tokens := "Approx request size: ~" + strs.Thousands(before) + " → ~" + strs.Thousands(after) + " tokens"
	if unchanged && after == before || done.reason == "refused" {
		tokens = "Approx request size: ~" + strs.Thousands(before) + " tokens (unchanged)"
	}
	if failure := strings.TrimSpace(result.Failure); failure != "" && (result.Aborted || result.Fallback) {
		note = strings.TrimSpace(note + " Reason: " + maskedFailure(failure))
	}
	lines := []string{"🗜️ " + headline}
	if focus != "" {
		lines = append(lines, fmt.Sprintf("Focus: %q", focus))
	}
	lines = append(lines, tokens)
	if note != "" {
		lines = append(lines, note)
	}
	switch {
	case result.Aborted:
		lines = append(lines, fmt.Sprintf("⚠️ Compression aborted (%s). No messages were dropped — conversation is unchanged. Run /compress to retry, /new "+
			"for a clean session, or check your auxiliary.compression model configuration.", maskedFailure(cmp.Or(result.Failure, "unknown error"))))
	case result.AuxModel != "":
		lines = append(lines, fmt.Sprintf("ℹ️ Configured compression model `%s` failed (%s). Recovered using your main model — context is intact — but you "+
			"may want to check `auxiliary.compression.model` in config.yaml.", result.AuxModel, maskedFailure(cmp.Or(result.AuxFailure, "unknown error"))))
	}
	return strings.Join(lines, "\n")
}

// Glyphs of the categories of the context view, in their order, and of the free part of the window
var categories = []struct{ id, label, glyph string }{
	{"system_prompt", "System prompt", "■"},
	{"tool_definitions", "Tool definitions", "▣"},
	{"rules", "Rules", "▩"},
	{"skills", "Skills", "▤"},
	{"mcp", "MCP", "▥"},
	{"subagent_definitions", "Subagent definitions", "▦"},
	{"memory", "Memory", "▧"},
	{"conversation", "Conversation", "▨"},
}

const (
	freeGlyph   = "·"
	gridColumns = 20
	gridRows    = 5
	tableLimit  = 15
)

// The context window of the session in detail: how full it is, when it compresses, what compressions did, what the calls took and what
// takes up the window; with all, what each toolset and skill costs
func (a *Agent) Context(ctx context.Context, id, argument string) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	expanded := strings.EqualFold(strings.TrimSpace(argument), "all")
	store, err := a.store()
	if err != nil {
		return "", err
	}
	session, err := store.Get(ctx, id)
	if err != nil {
		return "", err
	}
	history, err := store.Messages(ctx, id, sessions.Model)
	if err != nil {
		return "", err
	}
	settings := a.settings(r)
	instance, route, routeErr := a.current(r)
	if routeErr != nil && len(history) == 0 {
		return "No context data available yet. Send a message to start a session.", nil
	}
	env := a.contextEnv(ctx, r, settings, instance, route, a.helpers(r, settings, route))
	var status contexts.Status
	engine := a.engine(r, env)
	if engine != nil {
		status = engine.Status()
	}
	window := cmp.Or(env.Window, status.Window)
	surface := platformOf(session)
	toolEnv := &tools.Env{Cwd: session.GetDirectory(), Config: settings, OneShot: r.oneshot, Client: r.shell(), Platform: surface}
	c := r.subagent()
	if c != nil {
		toolEnv.Platform, toolEnv.Delegated = c.platform, c.delegated()
	}
	focus := agentprompt.Focused(settings, surface, toolEnv.Cwd)
	offered := a.offered(toolEnv, focus, c)
	definitions := tools.Definitions(offered, toolEnv)
	breakdown := a.breakdown(ctx, r, settings, session, instance, route, offered, definitions, history, surface, focus)
	estimated := 0
	for _, part := range breakdown {
		estimated += part.tokens
	}
	a.restoreAnchor(ctx, r, history)
	r.mu.Lock()
	anchor, spent := r.cx.usage, r.spent
	r.mu.Unlock()
	used, source := estimated, "local estimate"
	if tokens, held, more := anchor.Tokens(history); held {
		used, source = tokens, "provider usage"
		if more {
			source = "provider usage + estimated new messages"
		}
	}
	mark := "~"
	if source == "provider usage" {
		mark = ""
	}
	if used <= 0 || window <= 0 {
		if len(history) == 0 {
			return "No context data available yet. Send a message to start a session.", nil
		}
		return strings.Join([]string{"🧠 **Context Window**", "", fmt.Sprintf("Estimated context: ~%s tokens across %d messages", strs.Thousands(estimated),
			len(history)), "_(Full compression and throughput stats available after the first agent response)_"}, "\n"), nil
	}
	share := float64(used) / float64(window) * 100
	filled := int(math.RoundToEven(share / 100 * 24))
	filled = min(max(filled, 0), 24)
	lines := []string{"🧠 **Context Window**", "", "Model: `" + cmp.Or(route.Model, "?") + "`", "Window: " + strs.Thousands(window) + " tokens",
		fmt.Sprintf("In use: %s%s / %s (%s%.0f%%)", mark, strs.Thousands(used), strs.Thousands(window), mark, share),
		strings.Repeat("█", filled) + strings.Repeat("░", 24-filled), "Headroom to limit: " + mark + strs.Thousands(max(0, window-used)) + " tokens", ""}
	if engine != nil {
		if threshold := status.Threshold; threshold > 0 {
			percent := fmt.Sprintf("%.0f", status.Percent*100)
			if used >= threshold {
				lines = append(lines, fmt.Sprintf("⚠️ **Over auto-compression threshold (%s, %s%%)**", strs.Thousands(threshold), percent))
			} else {
				lines = append(lines, fmt.Sprintf("Auto-compresses at: %s (%s%%) — %s%s to go", strs.Thousands(threshold), percent, mark,
					strs.Thousands(threshold-used)))
			}
		}
		lines = append(lines, fmt.Sprintf("Compressions this session: %d", status.Compressions))
		if status.Compressions > 0 {
			lines = append(lines, fmt.Sprintf("Last compression freed: %.0f%% of context", status.Savings))
		}
		if status.Blocked != "" {
			lines = append(lines, "Compression held off: "+status.Blocked)
		}
		lines = append(lines, status.Lines...)
	}
	if total := session.GetUsage(); total != nil && total.GetApiCalls() > 0 {
		input := total.GetInput() + total.GetCacheRead() + total.GetCacheWrite()
		lines = append(lines, "", fmt.Sprintf("Session totals (cumulative across %d API calls)", total.GetApiCalls()),
			fmt.Sprintf("Input %s · Output %s · Reasoning %s", strs.Thousands(input), strs.Thousands(total.GetOutput()), strs.Thousands(total.GetReasoning())),
			"Total billed: "+strs.Thousands(input+total.GetOutput()), "_Throughput, not context size — each call re-sends the window above._")
	} else if spent.calls > 0 {
		lines = append(lines, "", fmt.Sprintf("Session totals (cumulative across %d API calls)", spent.calls),
			fmt.Sprintf("Input %s · Output %s · Reasoning %s", strs.Thousands(spent.Prompt()), strs.Thousands(spent.Output), strs.Thousands(spent.Reasoning)),
			"Total billed: "+strs.Thousands(spent.Prompt()+spent.Output), "_Throughput, not context size — each call re-sends the window above._")
	}
	lines = append(lines, "")
	lines = append(lines, grid(breakdown, window)...)
	lines = append(lines, "", "Estimated usage by category")
	width := utf8.RuneCountInString("Free space")
	for _, part := range breakdown {
		width = max(width, utf8.RuneCountInString(part.label))
	}
	row := func(glyph, label string, tokens int, share float64) string {
		return fmt.Sprintf("%s %-*s ~%9s tokens ~%5.1f%%", glyph, width, label, strs.Thousands(tokens), share)
	}
	for _, part := range breakdown {
		lines = append(lines, row(part.glyph, part.label, part.tokens, float64(part.tokens)/float64(window)*100))
	}
	free := max(0, window-estimated)
	lines = append(lines, row(freeGlyph, "Free space", free, float64(free)/float64(window)*100), "",
		fmt.Sprintf("Context window: %s%s / %s tokens (%s%.0f%%)", mark, strs.Thousands(used), strs.Thousands(window), mark, share),
		"Source: "+source+"; category counts are local estimates.")
	if !expanded {
		return strings.Join(append(lines, "", "Use /context all for per-skill and per-toolset costs."), "\n"), nil
	}
	return strings.Join(append(lines, a.costTables(settings, session, offered, definitions)...), "\n"), nil
}

// One category of what takes up the window
type category struct {
	id, label, glyph string
	tokens           int
}

// What takes up the window by category: the system prompt split by what its sections are, the tool definitions by kind, the conversation
func (a *Agent) breakdown(ctx context.Context, r *run, settings *config.Config, session *sessionsv1.Session, instance provider.Instance, route provider.Route,
	offered []tools.Def, definitions []map[string]any, history []*sessionsv1.Message, surface string, focus bool) []category {
	sizes := map[string]int{}
	env := a.promptEnv(settings, session.GetId(), session.GetDirectory(), surface, instance, route, definitions, session.GetCreated().AsTime())
	env.Focus, env.OneShot = focus, r.oneshot
	if sections, err := a.prompts.Sections(ctx, env); err == nil {
		for _, section := range sections {
			sizes[sectionCategory(section)] += contexts.Tokens(section.Text)
		}
	} else {
		r.mu.Lock()
		if built := r.system; built != nil {
			sizes["system_prompt"] += contexts.Tokens(built.text)
		}
		r.mu.Unlock()
	}
	for i, def := range offered {
		if i >= len(definitions) {
			break
		}
		kind := "tool_definitions"
		switch {
		case strings.HasPrefix(def.Name, "mcp_"):
			kind = "mcp"
		case def.Name == "delegate_task":
			kind = "subagent_definitions"
		}
		sizes[kind] += contexts.Definitions(definitions[i : i+1])
	}
	sizes["conversation"] = contexts.Estimate(sent(history))
	var out []category
	for _, c := range categories {
		if tokens := sizes[c.id]; tokens > 0 {
			out = append(out, category{id: c.id, label: c.label, glyph: c.glyph, tokens: tokens})
		}
	}
	return out
}

// Category of the window a section of the system prompt counts under
func sectionCategory(section prompt.Section) string {
	name := strings.ToLower(section.Name)
	switch {
	case name == "context files" || strings.Contains(name, "rules") || name == "coding instructions":
		return "rules"
	case strings.Contains(name, "skill"):
		return "skills"
	case strings.Contains(name, "memory") || strings.Contains(name, "user profile"):
		return "memory"
	}
	return "system_prompt"
}

// Grid of the window, each cell a twentieth of a row, filled by category in order and free after
func grid(parts []category, window int) []string {
	total := gridColumns * gridRows
	var cells []string
	for _, part := range parts {
		n := int(math.RoundToEven(float64(part.tokens) / float64(window) * float64(total)))
		if n == 0 && part.tokens > 0 {
			n = 1
		}
		for range n {
			cells = append(cells, part.glyph)
		}
	}
	cells = cells[:min(len(cells), total)]
	for len(cells) < total {
		cells = append(cells, freeGlyph)
	}
	rows := make([]string, gridRows)
	for i := range gridRows {
		rows[i] = strings.Join(cells[i*gridColumns:(i+1)*gridColumns], " ")
	}
	return rows
}

// Tables of what each toolset's schemas and each skill cost, largest first
func (a *Agent) costTables(settings *config.Config, session *sessionsv1.Session, offered []tools.Def, definitions []map[string]any) []string {
	type toolset struct {
		name          string
		count, tokens int
	}
	byName := map[string]*toolset{}
	for i, def := range offered {
		if i >= len(definitions) {
			break
		}
		name := cmp.Or(def.Toolset, def.Name)
		if byName[name] == nil {
			byName[name] = &toolset{name: name}
		}
		byName[name].count++
		byName[name].tokens += contexts.Definitions(definitions[i : i+1])
	}
	sets := make([]*toolset, 0, len(byName))
	for _, set := range byName {
		sets = append(sets, set)
	}
	slices.SortFunc(sets, func(x, y *toolset) int { return cmp.Or(y.tokens-x.tokens, cmp.Compare(x.name, y.name)) })
	var lines []string
	table := func(title string, rows []string) {
		if len(rows) == 0 {
			return
		}
		lines = append(lines, "", title)
		lines = append(lines, rows[:min(len(rows), tableLimit)]...)
		if len(rows) > tableLimit {
			lines = append(lines, fmt.Sprintf("  … and %d more", len(rows)-tableLimit))
		}
	}
	var rows []string
	for _, set := range sets {
		rows = append(rows, fmt.Sprintf("  %-24s %3d tools ~%8s tokens", set.name, set.count, strs.Thousands(set.tokens)))
	}
	table("Toolsets by schema cost (largest first)", rows)
	if a.skills == nil {
		return lines
	}
	type skill struct {
		name         string
		index, whole int
		known        bool
	}
	var skills []skill
	for _, entry := range a.skills.Scope(settings, session.GetDirectory()).Loadable(false) {
		s := skill{name: entry.Name, index: contexts.Tokens("- " + entry.Name + ": " + entry.Description)}
		if info, err := os.Stat(entry.Path); err == nil {
			s.whole, s.known = int(info.Size())/4, true
		}
		skills = append(skills, s)
	}
	slices.SortFunc(skills, func(x, y skill) int { return cmp.Or(y.index+y.whole-x.index-x.whole, cmp.Compare(x.name, y.name)) })
	rows = nil
	for _, s := range skills {
		name := s.name
		if runes := []rune(name); len(runes) > 28 {
			name = string(runes[:27]) + "…"
		}
		whole := fmt.Sprintf("%8s", "n/a")
		if s.known {
			whole = "~" + fmt.Sprintf("%8s", strs.Thousands(s.whole))
		}
		rows = append(rows, fmt.Sprintf("  %-28s index ~%6s  SKILL.md %s tokens", name, strs.Thousands(s.index), whole))
	}
	table("Skills by cost (index = always-on; SKILL.md = cost when loaded)", rows)
	return lines
}

// Whether a compression the user asked for is waiting in the queue, for /status
func (r *run) compactQueued() bool {
	return slices.ContainsFunc(r.queue, func(q queued) bool { return q.compact != nil })
}

// Lines of /status about the context: the window, the trigger and the compressions so far
func (a *Agent) contextLines(r *run) string {
	engine := a.contexts.Engine(r.sessionID())
	if engine == nil {
		return ""
	}
	status := engine.Status()
	var out strings.Builder
	if status.Window > 0 && status.Threshold > 0 {
		fmt.Fprintf(&out, "- **Context window** %s tokens, compresses at %s (%.0f%%)\n", strs.Thousands(status.Window), strs.Thousands(status.Threshold),
			status.Percent*100)
	}
	if status.Compressions > 0 {
		fmt.Fprintf(&out, "- **Compressions** %d, the last freed %.0f%%\n", status.Compressions, status.Savings)
	}
	if status.Blocked != "" {
		fmt.Fprintf(&out, "- **Compression held off** %s\n", status.Blocked)
	}
	r.mu.Lock()
	queued := r.compactQueued()
	running := r.cx.running
	r.mu.Unlock()
	switch {
	case running:
		out.WriteString("- **Compressing** now\n")
	case queued:
		out.WriteString("- **Compression** queued\n")
	}
	if !a.settings(r).GetBool(contextsconfig.Enabled) {
		out.WriteString("- **Automatic compression** off\n")
	}
	return out.String()
}
