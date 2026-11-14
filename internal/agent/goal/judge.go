package goal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/tool"
	"github.com/nickheyer/icebath/internal/tool/process"
	"gopkg.in/yaml.v3"
)

const (
	defaultJudgeTimeout       = 30 * time.Second
	defaultJudgeMaxTokens     = 4096
	judgeResponseSnippetChars = 4000
	judgeTask                 = "goal_judge"
)

const continuationPrompt = "[Continuing toward your standing goal]\n" +
	"Goal: {goal}\n\n" +
	"Continue working toward this goal. Take the next concrete step. " +
	"If you believe the goal is complete, state so explicitly and stop. " +
	"If you are blocked and need input from the user, say so clearly and stop."

const continuationWithContract = "[Continuing toward your standing goal]\n" +
	"Goal: {goal}\n\n" +
	"Completion contract:\n" +
	"{contract_block}\n\n" +
	"Continue working toward the outcome above. Take the next concrete step. " +
	"Stay within the stated boundaries and do not violate the constraints. " +
	"Before claiming the goal is done, satisfy the Verification criterion and " +
	"show the concrete evidence (command output, file contents, test result). " +
	"If you hit the stated stop condition or are otherwise blocked and need " +
	"user input, say so clearly and stop."

const continuationWithSubgoals = "[Continuing toward your standing goal]\n" +
	"Goal: {goal}\n\n" +
	"Additional criteria the user added mid-loop:\n" +
	"{subgoals_block}\n\n" +
	"Continue working toward the goal AND all additional criteria. Take " +
	"the next concrete step. If you believe the goal and every " +
	"additional criterion are complete, state so explicitly and stop. " +
	"If you are blocked and need input from the user, say so clearly " +
	"and stop."

const continuationGateFailed = "[Continuing toward your standing goal — a quality gate failed]\n" +
	"Goal: {goal}\n\n" +
	"The quality gate command below must pass before this goal can be " +
	"declared done, and it just failed (attempt {attempt}/{max_retries}):\n" +
	"  $ {command}\n" +
	"Exit code: {exit_code}\n" +
	"Output (tail):\n" +
	"```\n" +
	"{output}\n" +
	"```\n\n" +
	"Fix the underlying problem so this gate passes, then re-run it to " +
	"confirm. Do not declare the goal complete while any gate fails. If the " +
	"gate itself is wrong or cannot pass, say so clearly and stop."

const judgeSystemPrompt = "You are a strict judge evaluating whether an autonomous agent has " +
	"achieved a user's stated goal. You receive the goal text, the agent's " +
	"most recent response, and — when present — a list of background " +
	"processes the agent has running. Decide one of four verdicts.\n\n" +
	"DONE — the goal is fully satisfied:\n" +
	"- The response explicitly confirms the goal was completed, OR\n" +
	"- The response clearly shows the final deliverable was produced.\n" +
	"DONE requires the deliverable to actually exist. If the response only " +
	"explains why the goal cannot be reached, the verdict is BLOCKED, not " +
	"DONE.\n\n" +
	"BLOCKED — the goal cannot be satisfied as stated:\n" +
	"- The response explains the goal is genuinely unachievable (impossible, " +
	"out of scope, no valid path to the deliverable), or refuses to " +
	"fabricate a deliverable that cannot exist, OR\n" +
	"- The response explains progress is blocked and the next step needs " +
	"user input to proceed.\n" +
	"Return BLOCKED with the reason describing what is blocking. BLOCKED is " +
	"a refusal, not a completion — never return BLOCKED for a goal that " +
	"was achieved.\n" +
	"When the block is an error the agent hit (an HTTP status, an API, " +
	"sign-in or token failure), quote the error text verbatim in the reason " +
	"and attribute it only to a provider, service or credential the response " +
	"itself names. Never infer one the response does not name — an unnamed " +
	"401 belongs to the model provider the agent was calling, not to some " +
	"other service's token.\n\n" +
	"WAIT — the goal is NOT done, but the next step is to wait for async " +
	"work to finish rather than act again. Choose this ONLY when the agent's " +
	"progress is genuinely gated on something running on its own:\n" +
	"- A background process listed below is still running AND the response " +
	"shows the agent is waiting on its result (e.g. a CI poller, build, " +
	"test run, deploy). If the process has a session id, return it in " +
	"``wait_on_session`` — that releases when the process exits OR its " +
	"watch_patterns trigger fires (use this for a long-lived watcher that " +
	"signals mid-run and may never exit). Otherwise return its pid in " +
	"``wait_on_pid`` (releases on exit only).\n" +
	"- The agent says it is rate-limited / backing off / must wait a fixed " +
	"period — return seconds in ``wait_for_seconds``.\n" +
	"- The agent has delegated subagents still running (stated below as " +
	"active delegations) and the response says it is waiting on them with " +
	"nothing else dispatchable — return ``wait_for_seconds`` between 600 and " +
	"1800. Their results wake the agent on their own; re-poking it now only " +
	"produces a status recap.\n" +
	"Picking WAIT parks the loop without burning a turn; it resumes " +
	"automatically when the pid exits or the time elapses. Do NOT pick WAIT " +
	"just because work remains — only when re-poking now would be pure " +
	"busy-work because the agent can't progress until the async thing " +
	"finishes.\n\n" +
	"CONTINUE — not done, and there is a concrete next step the agent can " +
	"take right now. This is the default when in doubt.\n\n" +
	"Reply ONLY with a single JSON object on one line. Shapes:\n" +
	`{"verdict": "done", "reason": "<one sentence>"}` + "\n" +
	`{"verdict": "blocked", "reason": "<one sentence>"}` + "\n" +
	`{"verdict": "continue", "reason": "<one sentence>"}` + "\n" +
	`{"verdict": "wait", "wait_on_session": "<id>", "reason": "<one sentence>"}` + "\n" +
	`{"verdict": "wait", "wait_on_pid": <int>, "reason": "<one sentence>"}` + "\n" +
	`{"verdict": "wait", "wait_for_seconds": <int>, "reason": "<one sentence>"}` + "\n" +
	`The legacy shape {"done": <true|false>, "reason": "..."} is still ` +
	"accepted (true=done, false=continue)."

const judgeDelegationsBlock = "Active delegations: the agent has {count} delegated subagent batch(es) still running; " +
	"their results are delivered to it automatically when they finish.\n\n"

const judgeBackgroundBlock = "Background processes the agent currently has running (it may be waiting " +
	"on one of these):\n{background_lines}\n\n"

const judgeUserPrompt = "Goal:\n{goal}\n\n" +
	"Agent's most recent response:\n{response}\n\n" +
	"{background_block}" +
	"Current time: {current_time}\n\n" +
	"Is the goal satisfied — done, blocked, continue, or wait?"

const judgeUserPromptWithSubgoals = "Goal:\n{goal}\n\n" +
	"Additional criteria the user added mid-loop (all must also be " +
	"satisfied for the goal to be DONE):\n{subgoals_block}\n\n" +
	"Agent's most recent response:\n{response}\n\n" +
	"{background_block}" +
	"Current time: {current_time}\n\n" +
	"Decision: For each numbered criterion above, find concrete " +
	"evidence in the agent's response that the criterion is " +
	"satisfied. Do not accept generic phrases like 'all requirements " +
	"met' or 'implying it was done' — require specific evidence (a " +
	"file contents excerpt, an output line, a command result). If " +
	"ANY criterion lacks specific evidence in the response, the goal " +
	"is NOT done — return CONTINUE (or WAIT if blocked on a listed " +
	"background process).\n\n" +
	"Is the goal AND every additional criterion satisfied?"

const judgeUserPromptWithContract = "Goal:\n{goal}\n\n" +
	"Completion contract (the authoritative definition of done):\n" +
	"{contract_block}\n\n" +
	"Agent's most recent response:\n{response}\n\n" +
	"{background_block}" +
	"Current time: {current_time}\n\n" +
	"Decision rules:\n" +
	"- The goal is DONE only when the Verification criterion is satisfied AND " +
	"the response shows concrete evidence of it (a command result, file " +
	"contents excerpt, test/benchmark output) — not a claim like 'done' or " +
	"'all tests pass' without evidence.\n" +
	"- If any stated Constraint was violated, the goal is NOT done — CONTINUE.\n" +
	"- If the response shows the agent is waiting on a listed background " +
	"process to satisfy the Verification criterion (e.g. CI is the " +
	"verification and it's still running), return WAIT on that process " +
	"instead of re-poking — re-poking now would be pure busy-work.\n" +
	"- If the response explains the work is genuinely unachievable or hits " +
	"the stated Stop condition and needs user input, the goal is NOT done — " +
	"return BLOCKED with the reason describing the block.\n" +
	"- Otherwise the goal is NOT done — CONTINUE.\n\n" +
	"Is the goal satisfied per its completion contract — done, blocked, continue, or wait?"

const draftContractSystemPrompt = "You turn a user's plain-language objective into a structured completion " +
	"contract for an autonomous coding agent. The contract has five fields:\n" +
	"- outcome: the single end state that must be true when done\n" +
	"- verification: the specific test / command / artifact that PROVES the " +
	"outcome (must be concrete and checkable)\n" +
	"- constraints: what must NOT change or regress\n" +
	"- boundaries: which files, dirs, tools, or systems are in scope\n" +
	"- stop_when: the condition under which the agent should stop and ask " +
	"for human input instead of pushing on\n\n" +
	"Infer sensible, specific values from the objective and any project " +
	"context implied by it. Prefer concrete verification (a named test " +
	"command, a build, a benchmark) over vague phrases. Keep each field to " +
	"one or two sentences. If a field genuinely cannot be inferred, use an " +
	"empty string for it.\n\n" +
	"Reply ONLY with a single JSON object on one line:\n" +
	`{"outcome": "...", "verification": "...", "constraints": "...", ` +
	`"boundaries": "...", "stop_when": "..."}`

var jsonObjectRe = regexp.MustCompile(`(?s)\{.*?\}`)

func fill(template string, pairs ...string) string {
	return strings.NewReplacer(pairs...).Replace(template)
}

type JudgeInput struct {
	Goal        string
	Response    string
	Subgoals    []string
	Background  []*process.Reply
	Contract    *Contract
	Delegations int
	Timeout     time.Duration
}

type WaitDirective struct {
	SessionID string
	PID       int
	Seconds   int
}

type Judgment struct {
	Verdict         string
	Reason          string
	ParseFailed     bool
	Wait            *WaitDirective
	TransportFailed bool
}

type sideUnavailable interface{ Unavailable() bool }

func (m *Managers) Judge(ctx context.Context, in JudgeInput) (Judgment, error) {
	if trimSpace(in.Goal) == "" {
		return Judgment{Verdict: "skipped", Reason: "empty goal"}, nil
	}
	if trimSpace(in.Response) == "" {
		return Judgment{Verdict: "continue", Reason: "empty response (nothing to evaluate)"}, nil
	}
	if m.o.Side == nil {
		return Judgment{}, &tool.UnfilledError{Name: "goal.Options.Side"}
	}
	maxTokens, timeout := m.judgeSettings(ctx)
	if in.Timeout > 0 {
		timeout = in.Timeout
	}
	var subgoals []string
	for _, s := range in.Subgoals {
		if t := trimSpace(s); t != "" {
			subgoals = append(subgoals, t)
		}
	}
	background := renderBackground(in.Background)
	if in.Delegations > 0 {
		background += fill(judgeDelegationsBlock, "{count}", strconv.Itoa(in.Delegations))
	}
	common := []string{
		"{goal}", truncate(in.Goal, 2000),
		"{response}", truncate(in.Response, judgeResponseSnippetChars),
		"{background_block}", background,
		"{current_time}", time.Now().Format("2006-01-02 15:04:05 MST"),
	}
	var prompt string
	switch {
	case in.Contract != nil && !in.Contract.Empty():
		block := in.Contract.Render()
		if len(subgoals) > 0 {
			block += "\n" + renderExtraCriteria(subgoals)
		}
		prompt = fill(judgeUserPromptWithContract, append(common, "{contract_block}", truncate(block, 2500))...)
	case len(subgoals) > 0:
		lines := make([]string, len(subgoals))
		for i, s := range subgoals {
			lines[i] = fmt.Sprintf("- %d. %s", i+1, s)
		}
		prompt = fill(judgeUserPromptWithSubgoals, append(common, "{subgoals_block}", truncate(strings.Join(lines, "\n"), 2000))...)
	default:
		prompt = fill(judgeUserPrompt, common...)
	}
	raw, err := m.callJudge(ctx, judgeSystemPrompt, prompt, maxTokens, timeout)
	if err != nil {
		var u sideUnavailable
		if errors.As(err, &u) && u.Unavailable() {
			slog.InfoContext(ctx, fmt.Sprintf("goal judge: auxiliary client unavailable (%s) — falling through to continue", err))
			return Judgment{Verdict: "continue", Reason: "goal_judge auxiliary client unavailable: " + err.Error(), TransportFailed: true}, nil
		}
		slog.InfoContext(ctx, fmt.Sprintf("goal judge: API call failed (%s) — falling through to continue", err))
		return Judgment{Verdict: "continue", Reason: "judge error: " + truncate(err.Error(), 200), TransportFailed: true}, nil
	}
	j := parseJudgeResponse(raw)
	wait := ""
	if j.Wait != nil {
		wait = " wait=" + j.Wait.String()
	}
	slog.InfoContext(ctx, fmt.Sprintf("goal judge: verdict=%s reason=%s%s", j.Verdict, truncate(j.Reason, 120), wait))
	return j, nil
}

func (w WaitDirective) String() string {
	switch {
	case w.SessionID != "":
		return "{'session_id': " + quoteText(w.SessionID) + "}"
	case w.PID > 0:
		return "{'pid': " + strconv.Itoa(w.PID) + "}"
	}
	return "{'seconds': " + strconv.Itoa(w.Seconds) + "}"
}

func (m *Managers) callJudge(ctx context.Context, system, user string, maxTokens int, timeout time.Duration) (string, error) {
	zero := 0.0
	resp, err := m.o.Side.Complete(ctx, judgeTask, wire.Request{
		Messages: []wire.Message{
			{Role: wire.RoleSystem, Text: system},
			{Role: wire.RoleUser, Text: user},
		},
		Temperature: &zero,
		MaxTokens:   maxTokens,
		Timeout:     timeout,
	})
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", nil
	}
	return resp.Message.FlatText(""), nil
}

func (m *Managers) judgeSettings(ctx context.Context) (int, time.Duration) {
	maxTokens, timeout := defaultJudgeMaxTokens, defaultJudgeTimeout
	if cfg, err := m.o.Config.LoadReadonly(ctx); err == nil && cfg.Auxiliary.GoalJudge.Timeout > 0 {
		timeout = time.Duration(cfg.Auxiliary.GoalJudge.Timeout * float64(time.Second))
	}
	ev, ok, err := m.o.Config.Effective(ctx, "auxiliary.goal_judge.max_tokens")
	if err != nil || !ok || ev.Value == nil {
		return maxTokens, timeout
	}
	var v any
	if ev.Value.Kind != yaml.ScalarNode || ev.Value.Decode(&v) != nil {
		return maxTokens, timeout
	}
	var n int
	switch t := v.(type) {
	case int:
		n = t
	case float64:
		n = int(t)
	case bool:
		if t {
			n = 1
		}
	case string:
		n, _ = strconv.Atoi(trimSpace(t))
	}
	if n > 0 {
		maxTokens = n
	}
	return maxTokens, timeout
}

func extractJSONObject(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	text := trimSpace(raw)
	if strings.HasPrefix(text, "```") {
		text = strings.Trim(text, "`")
		if nl := strings.IndexByte(text, '\n'); nl >= 0 {
			text = text[nl+1:]
		}
	}
	var data any
	if err := json.Unmarshal([]byte(text), &data); err != nil {
		match := jsonObjectRe.FindString(text)
		if match == "" {
			return nil
		}
		data = nil
		if json.Unmarshal([]byte(match), &data) != nil {
			return nil
		}
	}
	obj, _ := data.(map[string]any)
	return obj
}

func parseJudgeResponse(raw string) Judgment {
	if raw == "" {
		return Judgment{Verdict: "continue", Reason: "judge returned empty response", ParseFailed: true}
	}
	data := extractJSONObject(raw)
	if data == nil {
		return Judgment{Verdict: "continue", Reason: "judge reply was not JSON: " + quoteText(truncate(raw, 200)), ParseFailed: true}
	}
	reason := ""
	if truthy(data["reason"]) {
		reason = trimSpace(textOf(data["reason"]))
	}
	if reason == "" {
		reason = "no reason provided"
	}
	var verdict string
	if v, ok := data["verdict"].(string); ok {
		verdict = strings.ToLower(trimSpace(v))
	} else {
		done := truthy(data["done"])
		if s, ok := data["done"].(string); ok {
			switch strings.ToLower(trimSpace(s)) {
			case "true", "yes", "1", "done":
				done = true
			default:
				done = false
			}
		}
		verdict = "continue"
		if done {
			verdict = "done"
		}
	}
	switch verdict {
	case "done", "blocked", "continue", "wait":
	default:
		verdict = "continue"
	}
	if verdict != "wait" {
		return Judgment{Verdict: verdict, Reason: reason}
	}
	var sess any
	for _, key := range []string{"wait_on_session", "session_id", "wait_session"} {
		if truthy(data[key]) {
			sess = data[key]
			break
		}
	}
	if s, ok := sess.(string); ok && trimSpace(s) != "" {
		return Judgment{Verdict: "wait", Reason: reason, Wait: &WaitDirective{SessionID: trimSpace(s)}}
	}
	if pid, ok := firstPositive(data, "wait_on_pid", "pid", "wait_pid"); ok {
		return Judgment{Verdict: "wait", Reason: reason, Wait: &WaitDirective{PID: pid}}
	}
	if seconds, ok := firstPositive(data, "wait_for_seconds", "seconds", "wait_seconds"); ok {
		return Judgment{Verdict: "wait", Reason: reason, Wait: &WaitDirective{Seconds: seconds}}
	}
	return Judgment{Verdict: "continue", Reason: reason + " (wait verdict had no target — continuing)"}
}

func firstPositive(data map[string]any, keys ...string) (int, bool) {
	for _, k := range keys {
		v := data[k]
		if v == nil {
			continue
		}
		n, err := intOf(v)
		if err != nil {
			continue
		}
		if n > 0 {
			return n, true
		}
	}
	return 0, false
}

func renderBackground(procs []*process.Reply) string {
	var lines []string
	for _, p := range procs {
		if p == nil || replyText(p, "status") == "exited" {
			continue
		}
		pid, ok := replyInt(p, "pid")
		if !ok || pid == 0 {
			continue
		}
		cmd := truncate(trimSpace(strings.ReplaceAll(replyText(p, "command"), "\n", " ")), 120)
		out := truncate(trimSpace(strings.ReplaceAll(replyText(p, "output_preview"), "\n", " ")), 120)
		line := "- pid " + strconv.Itoa(pid)
		if sid := replyText(p, "session_id"); sid != "" {
			line += " / session " + sid
		}
		line += ": " + cmd
		if up, ok := replyInt(p, "uptime_seconds"); ok {
			line += " (running " + strconv.Itoa(up) + "s)"
		}
		if patterns := replyStrings(p, "watch_patterns"); len(patterns) > 0 {
			hit := ""
			if replyBool(p, "watch_hit") {
				hit = " [already matched]"
			}
			line += " | watch_patterns=" + quoteList(patterns) + hit
		} else if replyBool(p, "notify_on_complete") {
			line += " | notify_on_complete"
		}
		if out != "" {
			line += " | recent output: " + out
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}
	return fill(judgeBackgroundBlock, "{background_lines}", strings.Join(lines, "\n"))
}

func replyText(r *process.Reply, key string) string {
	v, ok := r.Get(key)
	if !ok {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case *string:
		if t != nil {
			return *t
		}
		return ""
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

func replyInt(r *process.Reply, key string) (int, bool) {
	v, ok := r.Get(key)
	if !ok {
		return 0, false
	}
	switch t := v.(type) {
	case int:
		return t, true
	case *int:
		if t == nil {
			return 0, false
		}
		return *t, true
	case int64:
		return int(t), true
	case float64:
		return int(t), true
	}
	return 0, false
}

func replyBool(r *process.Reply, key string) bool {
	v, ok := r.Get(key)
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

func replyStrings(r *process.Reply, key string) []string {
	v, ok := r.Get(key)
	if !ok {
		return nil
	}
	s, _ := v.([]string)
	return s
}

func quoteList(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = quoteText(s)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func quoteText(s string) string {
	quote := '\''
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteRune(quote)
	for _, r := range s {
		switch {
		case r == quote || r == '\\':
			b.WriteRune('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case unicode.IsPrint(r):
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteRune(quote)
	return b.String()
}

func (m *Managers) DraftContract(ctx context.Context, objective string) (Contract, bool, error) {
	objective = trimSpace(objective)
	if objective == "" {
		return Contract{}, false, nil
	}
	if m.o.Side == nil {
		return Contract{}, false, &tool.UnfilledError{Name: "goal.Options.Side"}
	}
	maxTokens, timeout := m.judgeSettings(ctx)
	raw, err := m.callJudge(ctx, draftContractSystemPrompt, "Objective:\n"+truncate(objective, 4000), maxTokens, timeout)
	if err != nil {
		slog.InfoContext(ctx, fmt.Sprintf("goal draft: API call failed (%s)", err))
		return Contract{}, false, nil
	}
	data := extractJSONObject(raw)
	if data == nil {
		slog.DebugContext(ctx, "goal draft: reply was not JSON: "+quoteText(truncate(raw, 200)))
		return Contract{}, false, nil
	}
	c := ContractFrom(data)
	if c.Empty() {
		return Contract{}, false, nil
	}
	return c, true, nil
}
