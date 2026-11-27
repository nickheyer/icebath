package engine

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/internal/contexts"
	"github.com/nickheyer/icebath/internal/provider"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

const (
	// Messages after the kept head looked through for a summary that says the session was compressed before
	headProbeExtra = 4
	// Most messages the tail keeps by count alone
	maxTailFloor = 8
)

func isTool(m *sessionsv1.Message) bool { return m.GetRole() == sessionsv1.Role_ROLE_TOOL }

func isAssistant(m *sessionsv1.Message) bool { return m.GetRole() == sessionsv1.Role_ROLE_ASSISTANT }

func isUser(m *sessionsv1.Message) bool { return m.GetRole() == sessionsv1.Role_ROLE_USER }

// Whether the message is a reply that calls tools
func calls(m *sessionsv1.Message) bool { return isAssistant(m) && len(m.GetToolCalls()) > 0 }

// Start of a region moved past tool results whose call is before it
func alignForward(messages []*sessionsv1.Message, i int) int {
	for i < len(messages) && isTool(messages[i]) {
		i++
	}
	return i
}

// End of a region pulled back to the reply whose tool results it would split
func alignBackward(messages []*sessionsv1.Message, i int) int {
	if i <= 0 || i >= len(messages) {
		return i
	}
	check := i - 1
	for check >= 0 && isTool(messages[check]) {
		check--
	}
	if check >= 0 && calls(messages[check]) {
		return check
	}
	return i
}

// Opening messages kept as they are: none once the session was compressed before, as a summary near the start shows
func (c *compressor) headSize(messages []*sessionsv1.Message) int {
	if c.aggressive || c.compressions >= 1 || c.previous != "" || c.s.protectFirst <= 0 {
		return 0
	}
	for _, m := range messages[:min(len(messages), c.s.protectFirst+headProbeExtra)] {
		if contexts.IsSummary(m) {
			return 0
		}
	}
	return min(c.s.protectFirst, len(messages))
}

// Newest message of the person at or after the head, -1 when there is none
func lastUser(messages []*sessionsv1.Message, headEnd int) int {
	for i := len(messages) - 1; i >= max(headEnd, 0); i-- {
		if contexts.RealUser(messages[i]) {
			return i
		}
	}
	return -1
}

// Newest reply at or after the head that says something, else the newest reply, summaries left out; -1 when there is none
func lastReply(messages []*sessionsv1.Message, headEnd int) int {
	found := -1
	for i := len(messages) - 1; i >= max(headEnd, 0); i-- {
		m := messages[i]
		if !isAssistant(m) || contexts.IsSummary(m) {
			continue
		}
		if found < 0 {
			found = i
		}
		if strings.TrimSpace(contexts.Text(m)) != "" {
			return i
		}
	}
	return found
}

// Index after the person's message at the index, the reply to it and that reply's tool results
func pairEnd(messages []*sessionsv1.Message, user int) int {
	i := user + 1
	if i >= len(messages) || !isAssistant(messages[i]) {
		return i
	}
	return alignForward(messages, i+1)
}

// Cut moved so the newest reply that said something stays in the tail, its tool round whole
func (c *compressor) keepReply(messages []*sessionsv1.Message, cut, headEnd int) int {
	last := lastReply(messages, headEnd)
	if last < 0 || last >= cut {
		return cut
	}
	moved := alignBackward(messages, last)
	slog.Debug(fmt.Sprintf("Anchoring tail cut to last assistant message at index %d (was %d, aligned to %d) to keep the previously-visible reply out of "+
		"the compaction summary (#29824)", last, cut, moved))
	return max(moved, headEnd+1)
}

// Cut moved so the newest message of the person stays in the tail, pushed past its reply when the head would split them
func (c *compressor) keepUser(messages []*sessionsv1.Message, cut, headEnd int) int {
	last := lastUser(messages, headEnd)
	if last < 0 || last >= cut {
		return cut
	}
	slog.Debug(fmt.Sprintf("Anchoring tail cut to last user message at index %d (was %d) to prevent active-task loss after compression", last, cut))
	if moved := max(last, headEnd+1); moved > last {
		end := pairEnd(messages, last)
		slog.Debug(fmt.Sprintf("Causal Coupling: cut would split turn-pair at user %d; pushing cut forward to pair_end %d so the completed pair is "+
			"summarised together (#22523)", last, end))
		return max(end, headEnd+1)
	}
	return last
}

// Cut moved so the newest n messages of the person stay in the tail
func (c *compressor) keepUsers(messages []*sessionsv1.Message, cut, headEnd, n int) int {
	if n <= 1 {
		return c.keepUser(messages, cut, headEnd)
	}
	var users []int
	for i := len(messages) - 1; i >= headEnd; i-- {
		if contexts.RealUser(messages[i]) {
			users = append(users, i)
		}
	}
	if len(users) == 0 || users[min(n, len(users))-1] >= cut {
		return cut
	}
	return max(users[min(n, len(users))-1], headEnd+1)
}

// Whether the reasoning of every reply reaches the wire on the route, as Anthropic replays signed thinking with each reply
func (c *compressor) thinkingOnWire() bool {
	return c.route.Mode == provider.ModeAnthropicMessages
}

// Tokens one message costs the tail: its text, a fixed overhead, its tool calls, its pictures and its reasoning when charged
func budgetTokens(m *sessionsv1.Message, reasoning bool) int {
	tokens := contexts.Tokens(contexts.Text(m)) + 10
	for _, call := range m.GetToolCalls() {
		data, _ := json.Marshal(map[string]any{"id": call.GetId(), "type": "function",
			"function": map[string]any{"name": call.GetName(), "arguments": call.GetArguments()}})
		tokens += contexts.Tokens(string(data))
	}
	tokens += contexts.ImageTokens * len(m.GetMedia())
	if reasoning {
		tokens += contexts.Tokens(m.GetReasoning())
	}
	return tokens
}

// Price of each message for the tail walk, the reasoning of the newest reply charged, of every reply where it reaches the wire
func (c *compressor) pricer(messages []*sessionsv1.Message) func(int) int {
	newest := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if isAssistant(messages[i]) {
			newest = i
			break
		}
	}
	all := c.thinkingOnWire()
	return func(i int) int {
		return budgetTokens(messages[i], all || i == newest)
	}
}

// Newest summary at or after the head, -1 when there is none
func newestSummary(messages []*sessionsv1.Message, headEnd int) int {
	for i := len(messages) - 1; i >= max(headEnd, 0); i-- {
		if contexts.IsSummary(messages[i]) {
			return i
		}
	}
	return -1
}

// Tail walk price in which messages at or before the newest summary cost nothing, already being in it
func (c *compressor) cutPricer(messages []*sessionsv1.Message, headEnd int) func(int) int {
	price := c.pricer(messages)
	newest := newestSummary(messages, headEnd)
	if newest < 0 {
		return price
	}
	return func(i int) int {
		if i <= newest {
			return 0
		}
		return price(i)
	}
}

// Lowest cut that leaves the summary window a message the scan of earlier summaries keeps
func progressFloor(messages []*sessionsv1.Message, headEnd, cut int) int {
	floor := headEnd + 1
	newest := newestSummary(messages, headEnd)
	if newest < 0 {
		return floor
	}
	for _, m := range span(messages, headEnd, max(cut, newest+1)) {
		if !contexts.IsSummary(m) {
			return floor
		}
	}
	raised := alignForward(messages, newest+2)
	user := lastUser(messages, newest+1)
	if raised >= len(messages) || 0 <= user && user < raised {
		return floor
	}
	return raised
}

// Messages lo to hi with the bounds clamped to the slice
func span(messages []*sessionsv1.Message, lo, hi int) []*sessionsv1.Message {
	lo, hi = min(max(lo, 0), len(messages)), min(max(hi, 0), len(messages))
	if lo >= hi {
		return nil
	}
	return messages[lo:hi]
}

// Tokens the tail may overrun its budget to: half again, at most its share of the window and never under the budget
func (c *compressor) softCeiling(budget int) int {
	ceiling := int(float64(budget) * 1.5)
	if c.window > 0 {
		ceiling = min(ceiling, int(float64(c.window)*tailMaxContextFraction))
	}
	return max(ceiling, budget)
}

// Walks back from the newest message adding prices until the ceiling: the first index kept and what was counted; with cutAtBreak the
// index that broke the ceiling
func walk(messages []*sessionsv1.Message, headEnd, ceiling, minTail int, cutAtBreak bool, price func(int) int) (int, int) {
	n := len(messages)
	total, cut := 0, n
	for i := n - 1; i >= headEnd; i-- {
		cost := price(i)
		if total+cost > ceiling && n-i >= minTail {
			if cutAtBreak {
				return i, total
			}
			return cut, total
		}
		total += cost
		cut = i
	}
	return cut, total
}

// First index of the protected tail: a walk back under the token budget, aligned so no tool round is split, holding the newest message
// of the person and the newest reply, and the newest few messages of the person when asked; a turn too large for the tail may be split
// at a tool round when allowed
func (c *compressor) tailCut(messages []*sessionsv1.Message, headEnd, budget int, allowSplit bool) int {
	n := len(messages)
	if c.aggressive {
		return n
	}
	available := max(0, n-headEnd-1)
	minTail := 0
	if available > 1 {
		minTail = min(max(3, min(c.s.protectLast, maxTailFloor)), max(3, available-2), available)
	}
	soft := c.softCeiling(budget)
	floor := minTail
	if soft >= minTail*budgetTokens(&sessionsv1.Message{}, true) {
		floor = 0
	}
	price := c.cutPricer(messages, headEnd)
	cut, total := walk(messages, headEnd, soft, floor, false, price)
	if cut <= headEnd && total > 0 && total <= soft {
		cut, _ = walk(messages, headEnd, budget, minTail, true, price)
	}
	cut = min(cut, n-max(floor, 1))
	if cut <= headEnd {
		cut = max(n-minTail, headEnd+1)
	}
	cut = alignBackward(messages, cut)
	user := lastUser(messages, headEnd)
	anchored := c.keepUser(messages, cut, headEnd)
	split := false
	if allowSplit && anchored < cut && user >= 0 && len(messages[user].GetMedia()) == 0 && budgetTokens(messages[user], true) <= soft &&
		callsBetween(messages, user, cut) {
		if walked, _ := walk(messages, anchored, soft, 0, false, c.pricer(messages)); walked > anchored {
			split = true
			slog.Debug(fmt.Sprintf("Active turn exceeds protected-tail soft ceiling; keeping tool-group-aligned mid-turn cut at index %d instead of anchoring "+
				"user message %d (#80449)", cut, user))
		}
	}
	if !split {
		cut = anchored
		reply := c.keepReply(messages, cut, headEnd)
		over := false
		if reply < cut && allowSplit && callsBetween(messages, reply, cut) {
			price, total := c.pricer(messages), 0
			for i := reply; i < cut; i++ {
				total += price(i)
			}
			over = total > soft
		}
		if over {
			slog.Debug(fmt.Sprintf("Assistant reply anchor would retain an over-ceiling region; keeping tool-group-aligned cut at index %d instead of "+
				"anchoring to %d (#131412)", cut, reply))
		} else {
			cut = reply
		}
	}
	if c.s.minTailUsers > 1 {
		cut = c.keepUsers(messages, cut, headEnd, c.s.minTailUsers)
	}
	return min(n, alignForward(messages, max(cut, progressFloor(messages, headEnd, cut))))
}

// Whether any reply from lo up to hi calls tools
func callsBetween(messages []*sessionsv1.Message, lo, hi int) bool {
	for i := max(lo, 0); i < hi && i < len(messages); i++ {
		if len(messages[i].GetToolCalls()) > 0 {
			return true
		}
	}
	return false
}

// Bounds of the middle to summarise: past the head, up to the tail, a reply or tool round before the newest message of the person moved
// out of the middle's last place so that message is not merged into the summary
func (c *compressor) middle(messages []*sessionsv1.Message) (int, int) {
	start := alignForward(messages, c.headSize(messages))
	end := c.tailCut(messages, start, c.tailBudget, true)
	latest := lastUser(messages, 0)
	if end == latest && latest >= 0 {
		bridge := latest - 1
		switch {
		case bridge >= 0 && isTool(messages[bridge]):
			bridge = alignBackward(messages, latest)
		case bridge < 0 || !isAssistant(messages[bridge]):
			bridge = -1
		}
		if bridge >= progressFloor(messages, start, bridge) {
			end = bridge
		}
	}
	return start, end
}

// Blank messages of the person right after their newest one, dropped when a reply follows them
func dropBlankEchoes(messages []*sessionsv1.Message) []*sessionsv1.Message {
	user := lastUser(messages, 0)
	if user < 0 {
		return messages
	}
	i := user + 1
	for i < len(messages) && contexts.Blank(messages[i]) {
		i++
	}
	if i == user+1 || i >= len(messages) || !isAssistant(messages[i]) {
		return messages
	}
	return append(append([]*sessionsv1.Message{}, messages[:user+1]...), messages[i:]...)
}
