package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/auxiliary"
	"github.com/nickheyer/icebath/internal/contexts"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// Failed merges of one exchange after which it is skipped
const microMaxFailures = 3

// Rolling summary the oldest exchanges are folded into one at a time, with where folding stands
type microState struct {
	cursor  int
	rolling string
	// Failures in a row at the cursor they happened at
	failures    int
	failedAt    int
	passes      int
	saved       int
	sincePasses int
}

func (m *microState) resetCursor() {
	m.cursor, m.rolling, m.failures, m.failedAt = 0, "", 0, -1
}

// Text of the marker that stands in for the folded exchanges
func microMarker(summary string) string {
	return contexts.Handoff + "\n\n" + contexts.TaskHeading + "\n" + strings.TrimSpace(summary) + "\n\n" + contexts.EndMarker
}

// Whether a message is the marker of a rolling summary
func isMicroMarker(m *sessionsv1.Message) bool {
	return m.GetSummary() && isAssistant(m) && strings.HasPrefix(contexts.Text(m), contexts.Handoff+"\n\n"+contexts.TaskHeading)
}

// Rolling summary read back from a marker's text
func rollingFrom(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	body := text
	if i := strings.LastIndex(text, contexts.TaskHeading); i >= 0 {
		body = text[i+len(contexts.TaskHeading):]
	}
	if end := strings.Index(body, contexts.EndMarker); end >= 0 {
		body = body[:end]
	}
	return strings.TrimSpace(body)
}

// First index of the window not yet folded, the rolling summary read back from a marker that survived when it was lost
func (c *compressor) microCursor(messages []*sessionsv1.Message, headEnd, tailStart int) int {
	if headEnd < c.micro.cursor && c.micro.cursor < tailStart {
		return c.micro.cursor
	}
	last := -1
	for i := headEnd; i < tailStart; i++ {
		if contexts.IsSummary(messages[i]) {
			last = i
		}
	}
	cursor := headEnd
	if last >= headEnd {
		cursor = last + 1
		if strings.TrimSpace(c.micro.rolling) == "" {
			if recovered := rollingFrom(contexts.Text(messages[last])); recovered != "" {
				c.micro.rolling = recovered
				slog.Info(fmt.Sprintf("Micro-compaction: recovered rolling summary from transcript (%d chars)", utf8.RuneCountInString(recovered)))
			}
		}
	}
	c.micro.cursor = cursor
	return cursor
}

// Next whole exchange from the start, a reply and its tool rounds closed by a message of another role; false when none is whole
func oneExchange(messages []*sessionsv1.Message, start, tailStart int) (int, int, bool) {
	limit := min(tailStart, len(messages))
	turn := func(i int, tool bool) bool {
		m := messages[i]
		return (isAssistant(m) || tool && isTool(m)) && !contexts.IsSummary(m)
	}
	i := start
	for i < limit && !turn(i, false) {
		i++
	}
	if i >= limit {
		return 0, 0, false
	}
	first := i
	for i++; i < limit && turn(i, true); i++ {
	}
	if i >= len(messages) || isAssistant(messages[i]) || isTool(messages[i]) {
		return 0, 0, false
	}
	return first, i, true
}

// Next exchange of the compressible window not folded yet
func (c *compressor) nextExchange(messages []*sessionsv1.Message) (int, int, bool) {
	start := alignForward(messages, c.headSize(messages))
	end := c.tailCut(messages, start, c.tailBudget, false)
	if start >= end {
		return 0, 0, false
	}
	cursor := c.microCursor(messages, start, end)
	if cursor >= end {
		return 0, 0, false
	}
	return oneExchange(messages, cursor, end)
}

// Prompt that merges one exchange into the rolling summary
func microPrompt(existing, exchange string) []*sessionsv1.Message {
	block := existing
	if strings.TrimSpace(existing) == "" {
		block = "(No previous summary yet.)"
	}
	prompt := "You are a summarization agent creating a compact record of an " +
		"ongoing conversation.  You are given a running summary and the " +
		"next exchange from the conversation.  Merge the exchange's key " +
		"decisions, requirements, file paths, and open questions into the " +
		"summary.  Preserve the summary's structure.  Drop resolved details " +
		"that are no longer relevant.  Add new decisions, file paths, and " +
		"open questions.\n\n" +
		"NEVER include API keys, tokens, passwords, secrets, credentials, " +
		"or connection strings in the summary — replace any that appear " +
		"with [REDACTED].\n\n" +
		"## Current Running Summary\n" + block + "\n\n" +
		"## Next Exchange to Merge\n" + exchange + "\n\n" +
		"Return ONLY the updated summary text, no preamble or explanation. " +
		"Do not include this instruction block in your output."
	return auxiliary.Prompt("You are a conversation summarization assistant.", prompt)
}

// Rolling summary with one exchange merged in by the summarising model, empty for any answer that can't serve
func (c *compressor) microMerge(ctx context.Context, env *contexts.Env, exchange string) string {
	if env.Aux == nil {
		return ""
	}
	temperature := 0.1
	answer, err := env.Aux.Ask(ctx, auxiliary.Request{Task: Task, Messages: microPrompt(c.micro.rolling, exchange), MaxTokens: min(1500, cmp.Or(c.maxSummary, 1500)),
		Temperature: &temperature, Main: c.fallenBack})
	switch {
	case err != nil:
		slog.Info("micro-summarization call failed: " + err.Error())
		return ""
	case answer.Finish == "length":
		slog.Warn("micro-summarization output hit the token cap (finish_reason=length) — discarding partial summary")
		return ""
	}
	content := strings.TrimSpace(thinkBlocks.ReplaceAllString(strings.TrimSpace(answer.Text), ""))
	switch {
	case content == "":
		slog.Info("micro-summarization returned empty content")
		return ""
	case refused(content):
		slog.Warn("micro-summarization returned refusal content — discarding unusable summary")
		return ""
	}
	return content
}

// Folds the oldest exchange not yet folded into the rolling summary after a turn, or summarises the rolling summary again once it grew;
// nil when the turn left nothing to do or the merge failed
func (c *compressor) Finished(ctx context.Context, env *contexts.Env, messages []*sessionsv1.Message) (*contexts.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.s.micro {
		return nil, nil
	}
	if c.s.microEvery > 1 {
		c.micro.sincePasses++
		if c.micro.sincePasses < c.s.microEvery {
			return nil, nil
		}
		c.micro.sincePasses = 0
	}
	if len(messages) < 4 {
		return nil, nil
	}
	start, end, found := c.nextExchange(messages)
	if !found {
		return nil, nil
	}
	began := time.Now()
	before := contexts.Estimate(messages)
	report := func(outcome string, result []*sessionsv1.Message, exchange int) {
		after := contexts.Estimate(result)
		c.microTelemetry(outcome, len(messages), len(result), before, after, exchange, time.Since(began))
	}
	if contexts.Tokens(c.micro.rolling) >= c.s.microDefrag {
		old := c.micro.rolling
		c.micro.rolling = ""
		fresh := c.microMerge(ctx, env, old)
		if fresh == "" {
			c.micro.rolling = old
			report("defrag_failed", messages, 0)
			return nil, nil
		}
		c.micro.rolling = fresh
		result := slices.Clone(messages)
		for i := len(result) - 1; i >= 0; i-- {
			if isMicroMarker(result[i]) {
				marker := contexts.Copy(result[i])
				marker.Text, marker.Sent, marker.Carriers = microMarker(fresh), "", nil
				result[i] = marker
				break
			}
		}
		slog.Info(fmt.Sprintf("Micro-compaction defrag: rolling summary re-summarized (%d -> %d chars)", utf8.RuneCountInString(old), utf8.RuneCountInString(fresh)))
		c.micro.failures, c.micro.failedAt = 0, -1
		report("defrag", result, 0)
		return &contexts.Result{Messages: result, Record: contexts.Record{Method: "micro_defrag", Before: before, After: contexts.Estimate(result)}}, nil
	}
	cumulative := strings.TrimSpace(c.micro.rolling) != ""
	exchange := strings.Join(records(messages[start:end]), "\n\n")
	cost := contexts.Tokens(exchange)
	updated := c.microMerge(ctx, env, exchange)
	if updated == "" {
		report(c.microFailure(start, end), messages, cost)
		return nil, nil
	}
	c.micro.rolling = updated
	c.micro.cursor = end
	c.micro.failures, c.micro.failedAt = 0, -1
	result := c.splice(messages, start, end, cumulative)
	c.micro.cursor = start + 1
	for i := len(result) - 1; i >= 0; i-- {
		if result[i].GetSummary() {
			c.micro.cursor = i + 1
			break
		}
	}
	report("absorbed", result, cost)
	return &contexts.Result{Messages: result, Record: contexts.Record{Method: "micro_absorbed", Before: before, After: contexts.Estimate(result), Messages: len(messages),
		Left: len(result)}}, nil
}

// Counts a failed merge at the cursor, skipping the exchange after too many in a row
func (c *compressor) microFailure(start, end int) string {
	if start == c.micro.failedAt {
		c.micro.failures++
	} else {
		c.micro.failures = 1
	}
	c.micro.failedAt = start
	if c.micro.failures < microMaxFailures {
		return "summarize_failed"
	}
	slog.Info(fmt.Sprintf("Micro-compaction: skipping exchange at cursor %d after %d consecutive failures", start, c.micro.failures))
	c.micro.cursor = end
	c.micro.failures, c.micro.failedAt = 0, -1
	return "exchange_skipped"
}

// Exchange replaced by the marker of the rolling summary, older markers it supersedes dropped and the messages of the person that drop left
// side by side merged
func (c *compressor) splice(messages []*sessionsv1.Message, start, end int, supersede bool) []*sessionsv1.Message {
	if strings.TrimSpace(c.micro.rolling) == "" {
		return messages
	}
	marker := &sessionsv1.Message{Role: sessionsv1.Role_ROLE_ASSISTANT, Text: microMarker(c.micro.rolling), Summary: true, ModelOnly: true}
	for _, m := range messages[start:end] {
		marker.Absorbed = append(marker.Absorbed, m.GetUid())
	}
	result := append(append(append([]*sessionsv1.Message{}, messages[:start]...), marker), messages[end:]...)
	if !supersede {
		return result
	}
	var markers []int
	for i, m := range result {
		if isMicroMarker(m) {
			markers = append(markers, i)
		}
	}
	if len(markers) <= 1 {
		return result
	}
	stale := map[int]bool{}
	for _, i := range markers[:len(markers)-1] {
		stale[i] = true
	}
	var kept []*sessionsv1.Message
	for i, m := range result {
		if !stale[i] {
			kept = append(kept, m)
		}
	}
	return mergeUsers(kept)
}

// Messages of the person left side by side merged into one
func mergeUsers(messages []*sessionsv1.Message) []*sessionsv1.Message {
	plain := func(m *sessionsv1.Message) bool {
		return m != nil && isUser(m) && !m.GetSummary() && len(m.GetMedia()) == 0
	}
	var out []*sessionsv1.Message
	for _, m := range messages {
		if len(out) == 0 || !plain(m) || !plain(out[len(out)-1]) {
			out = append(out, m)
			continue
		}
		previous := out[len(out)-1]
		var parts []string
		for _, s := range []string{contexts.Text(previous), contexts.Text(m)} {
			if s != "" {
				parts = append(parts, s)
			}
		}
		joined := contexts.Rewritten(previous, strings.Join(parts, "\n\n"))
		joined.ModelOnly = true
		joined.Absorbed = append(joined.Absorbed, append([]string{m.GetUid()}, m.GetAbsorbed()...)...)
		out[len(out)-1] = joined
	}
	return out
}

// Logs one content free line of a micro compaction pass
func (c *compressor) microTelemetry(outcome string, messagesBefore, messagesAfter, tokensBefore, tokensAfter, exchange int, took time.Duration) {
	c.micro.saved -= tokensAfter - tokensBefore
	c.micro.passes++
	var occupancy any
	if c.threshold > 0 {
		occupancy = math.Round(float64(tokensAfter)/float64(c.threshold)*1000) / 10
	}
	line, _ := json.Marshal(map[string]any{
		"event": "micro_compaction", "session_id": c.session, "outcome": outcome, "messages_before": messagesBefore, "messages_after": messagesAfter,
		"tokens_before": tokensBefore, "tokens_after": tokensAfter, "tokens_delta": tokensAfter - tokensBefore, "exchange_tokens": exchange,
		"rolling_summary_tokens": contexts.Tokens(c.micro.rolling), "cursor": c.micro.cursor, "passes_total": c.micro.passes,
		"tokens_saved_total": c.micro.saved, "duration_ms": took.Milliseconds(), "threshold_tokens": c.threshold, "context_limit": c.window,
		"occupancy_pct": occupancy, "main_model": c.route.Model, "aux_model": c.lastAuxModel,
	})
	slog.Info("micro compaction telemetry: " + string(line))
}
