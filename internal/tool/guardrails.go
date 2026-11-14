package tool

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
)

const (
	stallIdenticalThreshold = 3
	stallMaxCyclePeriod     = 4
	stallCycleHistory       = 64
	identicalStubMinChars   = 512
	stubArgsPreviewChars    = 120
)

var (
	failureTolerantTools = map[string]bool{
		"terminal": true, "execute_code": true, "process_manage": true, "process": true, "browser_navigate": true, "web_extract": true,
	}
	progressResetTools = map[string]bool{
		"write_file": true, "patch": true, "terminal": true, "execute_code": true, "browser_click": true, "browser_type": true,
		"browser_press": true, "browser_navigate": true, "process_manage": true, "process": true, "delegate_task": true,
		"send_message": true, "cronjob": true, "cronjob_manage": true, "todo": true, "todo_list": true, "memory": true, "skill_manage": true,
	}
	attendedPlatforms = map[string]bool{"cli": true, "tui": true, "desktop": true, "acp": true, "subagent": true, "api_server": true}
)

func stallRepeatable(name string) bool {
	return name == "process_manage" || strings.HasSuffix(name, "_get_result") || strings.HasSuffix(name, "_poll")
}

type GuardSignature struct {
	ToolName string
	ArgsHash string
}

type GuardDecision struct {
	Action    string
	Code      string
	Message   string
	ToolName  string
	Count     int
	Signature *GuardSignature
}

func (d GuardDecision) Allows() bool {
	return d.Action == "" || d.Action == "allow" || d.Action == "warn"
}

func (d GuardDecision) ShouldHalt() bool { return d.Action == "block" || d.Action == "halt" }

func (d GuardDecision) metadata() *jnode {
	action, code := d.Action, d.Code
	if action == "" {
		action, code = "allow", "allow"
	}
	n := newObj().with("action", newStr(action)).with("code", newStr(code)).with("message", newStr(d.Message)).
		with("tool_name", newStr(d.ToolName)).with("count", newNum(strconv.Itoa(d.Count)))
	if d.Signature != nil {
		n.set("signature", newObj().with("tool_name", newStr(d.Signature.ToolName)).with("args_hash", newStr(d.Signature.ArgsHash)))
	}
	return n
}

func (d GuardDecision) Metadata() json.RawMessage { return d.metadata().compact() }

func GuardBlockText(d GuardDecision) string {
	return newObj().with("error", newStr(d.Message)).with("guardrail", d.metadata()).spaced()
}

func GuardGuidance(result string, d GuardDecision) string {
	if (d.Action != "warn" && d.Action != "halt") || d.Message == "" {
		return result
	}
	label := "Tool loop warning"
	if d.Action == "halt" {
		label = "Tool loop hard stop"
	}
	return result + fmt.Sprintf("\n\n[%s: %s; count=%d; %s]", label, d.Code, d.Count, d.Message)
}

type guardConfig struct {
	warnings, hardStop              bool
	exactWarn, exactBlock           int
	sameWarn, sameHalt              int
	noProgressWarn, noProgressBlock int
	maxWebSearches, maxSubagents    int
}

func newGuardConfig(c config.ToolLoopGuardrails, platform string) guardConfig {
	atLeast := func(v, def, minimum int) int {
		if v < minimum {
			return def
		}
		return v
	}
	g := guardConfig{
		warnings:        c.WarningsEnabled,
		hardStop:        c.HardStopEnabled,
		exactWarn:       atLeast(c.WarnAfter.ExactFailure, 2, 1),
		sameWarn:        atLeast(c.WarnAfter.SameToolFailure, 3, 1),
		noProgressWarn:  atLeast(c.WarnAfter.IdempotentNoProgress, 2, 1),
		exactBlock:      atLeast(c.HardStopAfter.ExactFailure, 5, 1),
		sameHalt:        atLeast(c.HardStopAfter.SameToolFailure, 8, 1),
		noProgressBlock: atLeast(c.HardStopAfter.IdempotentNoProgress, 5, 1),
		maxWebSearches:  atLeast(c.LoopCaps.MaxWebSearches, 50, 0),
		maxSubagents:    atLeast(c.LoopCaps.MaxSubagents, 50, 0),
	}
	if p := strings.ToLower(trimWide(platform)); c.NonInteractiveHardStopEnabled && p != "" && !attendedPlatforms[p] {
		g.hardStop = true
	}
	return g
}

type progressRecord struct {
	hash  string
	count int
}

type historyEntry struct {
	sig        GuardSignature
	hash       string
	repeatable bool
}

type Guardrails struct {
	mu    sync.Mutex
	cfg   guardConfig
	stall bool

	exactFailures    map[GuardSignature]int
	sameToolFailures map[string]int
	progress         map[GuardSignature]bool
	noProgress       map[GuardSignature]progressRecord
	halt             *GuardDecision
	turnHalt         *GuardDecision

	streakSig     *GuardSignature
	streakHash    string
	streakCount   int
	streakFirstID string
	history       []historyEntry
	persisted     map[string]string
	webSearches   int
	subagents     int
}

func NewGuardrails(c config.ToolLoopGuardrails, platform string, stallGuards bool) *Guardrails {
	g := &Guardrails{cfg: newGuardConfig(c, platform), stall: stallGuards}
	g.resetLocked()
	return g
}

func (g *Guardrails) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resetLocked()
}

func (g *Guardrails) resetLocked() {
	g.exactFailures = map[GuardSignature]int{}
	g.sameToolFailures = map[string]int{}
	g.progress = map[GuardSignature]bool{}
	g.noProgress = map[GuardSignature]progressRecord{}
	g.halt, g.turnHalt = nil, nil
	g.streakSig, g.streakHash, g.streakCount, g.streakFirstID = nil, "", 0, ""
	g.history = nil
	g.persisted = map[string]string{}
	g.webSearches, g.subagents = 0, 0
}

func (g *Guardrails) Halt() (GuardDecision, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.turnHalt == nil {
		return GuardDecision{}, false
	}
	return *g.turnHalt, true
}

func HaltResponse(d GuardDecision) string {
	name := d.ToolName
	if name == "" {
		name = "the same tool"
	}
	return fmt.Sprintf("I stopped retrying because I kept running %s %d times without making progress. The last result above shows what blocked it. Tell me how you'd like to proceed, or send `continue` and I'll try a different approach.", name, d.Count)
}

func (g *Guardrails) setTurnHaltLocked(d GuardDecision) {
	if d.ShouldHalt() && g.turnHalt == nil {
		dd := d
		g.turnHalt = &dd
	}
}

func guardSignature(name string, args Args) GuardSignature {
	return GuardSignature{ToolName: name, ArgsHash: sha256Hex(canonicalJSON(nodeOf(map[string]any(args))))}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func canonicalJSON(n *jnode) string {
	return string(sortedNode(n).compact())
}

func sortedNode(n *jnode) *jnode {
	if n == nil {
		return n
	}
	switch n.kind {
	case jArr:
		items := make([]*jnode, len(n.arr))
		for i, v := range n.arr {
			items[i] = sortedNode(v)
		}
		return newArr(items...)
	case jObj:
		idx := make([]int, len(n.keys))
		for i := range idx {
			idx[i] = i
		}
		sort.SliceStable(idx, func(a, b int) bool { return n.keys[idx[a]] < n.keys[idx[b]] })
		out := newObj()
		for _, i := range idx {
			out.keys = append(out.keys, n.keys[i])
			out.vals = append(out.vals, sortedNode(n.vals[i]))
		}
		return out
	}
	return n
}

func resultHash(result, tool string) string {
	n, err := parseNode([]byte(result))
	if err != nil || n == nil || n.kind == jNull {
		return sha256Hex(result)
	}
	if tool == "execute_code" && n.isObj() {
		c := n.shallow()
		c.del("duration_seconds")
		if k := c.get("kernel"); k.isObj() {
			kc := k.shallow()
			kc.del("execution_count")
			c.set("kernel", kc)
		}
		n = c
	}
	return sha256Hex(canonicalJSON(n))
}

func (g *Guardrails) decideLocked(action, code, tool string, count int, sig GuardSignature, message string) GuardDecision {
	if message == "" {
		message = decisionMessage(code, tool, count, 0, 0)
	}
	s := sig
	d := GuardDecision{Action: action, Code: code, Message: message, ToolName: tool, Count: count, Signature: &s}
	if d.ShouldHalt() {
		dd := d
		g.halt = &dd
	}
	return d
}

func decisionMessage(code, tool string, count, period, limit int) string {
	switch code {
	case "repeated_exact_failure_block":
		return fmt.Sprintf("Blocked %s: the same tool call failed %d times with identical arguments. Stop retrying it unchanged; change strategy or explain the blocker.", tool, count)
	case "idempotent_no_progress_block":
		return fmt.Sprintf("Blocked %s: this read-only call returned the same result %d times. Stop repeating it unchanged; use the result already provided or try a different query.", tool, count)
	case "same_tool_failure_halt":
		return fmt.Sprintf("Stopped %s: it failed %d times this turn. Stop retrying the same failing tool path and choose a different approach.", tool, count)
	case "repeated_exact_failure_warning":
		return fmt.Sprintf("%s has failed %d times with identical arguments. This looks like a loop; inspect the error and change strategy instead of retrying it unchanged.", tool, count)
	case "idempotent_no_progress_warning":
		return fmt.Sprintf("%s returned the same result %d times. Use the result already provided or change the query instead of repeating it unchanged.", tool, count)
	case "identical_call_streak_halt":
		return fmt.Sprintf("Stopped %s: the same call with identical arguments returned the same result %d times in a row. Stop repeating it unchanged; use the result already provided or change strategy.", tool, count)
	case "identical_cycle_halt":
		return fmt.Sprintf("Stopped %s: the same repeating cycle of tool calls (period %d) with identical arguments and identical results has run %d times. Repeating the batch unchanged is not progress; use the results already provided or change strategy.", tool, period, count)
	case "loop_web_search_cap":
		return fmt.Sprintf("Blocked web_search: this turn has already made %d web searches, the per-turn limit. This looks like a runaway search loop. Work with the results you already have and give the user your answer.", limit)
	case "loop_subagent_cap":
		return fmt.Sprintf("Blocked delegate_task: this turn has already spawned %d subagents (limit %d). This looks like a runaway delegation loop. Finish the work with the results you have and answer the user.", count, limit)
	}
	return ""
}

func failureRecoveryHint(tool string, count int) string {
	common := fmt.Sprintf("%s has failed %d times this turn. This looks like a loop. Do not switch to text-only replies; keep using tools, but diagnose before retrying. First inspect the latest error/output and verify your assumptions. ", tool, count)
	if tool == "terminal" {
		return common + "For terminal failures, run a small diagnostic such as `pwd && ls -la` in the same tool, then try an absolute path, a simpler command, a different working directory, or a different tool such as read_file/write_file/patch."
	}
	return common + "Try different arguments, a narrower query/path, an absolute path when relevant, or a different tool that can make progress. If the blocker is external, report the blocker after one diagnostic attempt instead of repeating the same failing path."
}

func (g *Guardrails) Before(name string, effects Effects, args Args) GuardDecision {
	g.mu.Lock()
	defer g.mu.Unlock()
	sig := guardSignature(name, args)
	allow := GuardDecision{Action: "allow", Code: "allow", ToolName: name, Signature: &sig}
	if d, blocked := g.loopCapLocked(name, args, sig); blocked {
		return d
	}
	if !g.cfg.hardStop {
		return allow
	}
	exact := g.exactFailures[sig]
	if g.progress[sig] {
		exact = 0
	}
	if exact >= g.cfg.exactBlock {
		return g.decideLocked("block", "repeated_exact_failure_block", name, exact, sig, "")
	}
	if effects&Idempotent != 0 {
		if rec, ok := g.noProgress[sig]; ok && rec.count >= g.cfg.noProgressBlock {
			return g.decideLocked("block", "idempotent_no_progress_block", name, rec.count, sig, "")
		}
	}
	return allow
}

func subagentSpawnCount(args Args) int {
	action := ""
	switch v := args["action"].(type) {
	case nil:
	case string:
		action = v
	default:
		action = fmt.Sprint(v)
	}
	switch strings.ToLower(trimWide(action)) {
	case "list", "steer", "stop":
		return 0
	}
	if tasks, ok := args["tasks"].([]any); ok && len(tasks) > 0 {
		return len(tasks)
	}
	return 1
}

func (g *Guardrails) loopCapLocked(name string, args Args, sig GuardSignature) (GuardDecision, bool) {
	var limit int
	var counter *int
	var code string
	switch name {
	case "web_search":
		limit, counter, code = g.cfg.maxWebSearches, &g.webSearches, "loop_web_search_cap"
	case "delegate_task":
		limit, counter, code = g.cfg.maxSubagents, &g.subagents, "loop_subagent_cap"
	default:
		return GuardDecision{}, false
	}
	increment := 1
	if name != "web_search" {
		increment = 0
		if limit != 0 {
			increment = subagentSpawnCount(args)
		}
	}
	if increment != 0 && limit != 0 && *counter >= limit {
		return g.decideLocked("block", code, name, *counter, sig, decisionMessage(code, name, *counter, 0, limit)), true
	}
	*counter += increment
	return GuardDecision{}, false
}

func (g *Guardrails) afterLocked(name string, effects Effects, args Args, result string, failed bool) GuardDecision {
	sig := guardSignature(name, args)
	warnings := g.cfg.warnings
	if failed {
		if g.progress[sig] {
			delete(g.progress, sig)
			delete(g.exactFailures, sig)
		}
		throttled := RateLimited(result)
		exact := g.exactFailures[sig]
		if !throttled {
			exact++
			g.exactFailures[sig] = exact
		}
		same := g.sameToolFailures[name] + 1
		g.sameToolFailures[name] = same
		delete(g.noProgress, sig)
		if g.cfg.hardStop && !failureTolerantTools[name] && same >= g.cfg.sameHalt {
			return g.decideLocked("halt", "same_tool_failure_halt", name, same, sig, "")
		}
		if warnings && !throttled && exact >= g.cfg.exactWarn {
			return g.decideLocked("warn", "repeated_exact_failure_warning", name, exact, sig, "")
		}
		if warnings && same >= g.cfg.sameWarn {
			return g.decideLocked("warn", "same_tool_failure_warning", name, same, sig, failureRecoveryHint(name, same))
		}
		return GuardDecision{Action: "allow", Code: "allow", ToolName: name, Count: exact, Signature: &sig}
	}
	delete(g.exactFailures, sig)
	delete(g.sameToolFailures, name)
	if progressResetTools[name] || MutationLanded(name, result) {
		for k := range g.exactFailures {
			g.progress[k] = true
		}
		clear(g.sameToolFailures)
	}
	if effects&Idempotent == 0 {
		delete(g.noProgress, sig)
		return GuardDecision{Action: "allow", Code: "allow", ToolName: name, Signature: &sig}
	}
	hash := resultHash(result, name)
	repeat := 1
	if prev, ok := g.noProgress[sig]; ok && prev.hash == hash {
		repeat = prev.count + 1
	}
	g.noProgress[sig] = progressRecord{hash: hash, count: repeat}
	if warnings && repeat >= g.cfg.noProgressWarn {
		return g.decideLocked("warn", "idempotent_no_progress_warning", name, repeat, sig, "")
	}
	return GuardDecision{Action: "allow", Code: "allow", ToolName: name, Count: repeat, Signature: &sig}
}

func ordinal(n int) string {
	suffix := "th"
	if m := n % 100; m < 11 || m > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return strconv.Itoa(n) + suffix
}

type stallObservation struct {
	notice, stub, kind string
}

func (g *Guardrails) observeLocked(name string, args Args, result string, plain bool, callID string, failed bool) stallObservation {
	sig := guardSignature(name, args)
	hash := ""
	if plain {
		hash = resultHash(result, name)
	}
	if plain && g.streakSig != nil && *g.streakSig == sig && g.streakHash == hash {
		g.streakCount++
	} else {
		g.streakSig = nil
		g.streakCount = 0
		if plain {
			s := sig
			g.streakSig = &s
			g.streakCount = 1
		}
		g.streakHash = hash
		g.streakFirstID = callID
	}
	count := g.streakCount
	var obs stallObservation
	if !stallRepeatable(name) && count >= stallIdenticalThreshold {
		obs.notice = fmt.Sprintf("[icebath note: this is the %s consecutive identical call to %s with identical arguments returning the same result. Do not repeat it — change arguments, use a different tool, or proceed with what you have.]", ordinal(count), name)
		obs.kind = "identical_call_streak"
		if g.cfg.hardStop && count >= g.cfg.noProgressBlock && g.halt == nil {
			g.decideLocked("halt", "identical_call_streak_halt", name, count, sig, "")
		}
	}
	if plain {
		g.history = append(g.history, historyEntry{sig: sig, hash: hash, repeatable: stallRepeatable(name)})
		if len(g.history) > stallCycleHistory {
			g.history = slices.Clone(g.history[len(g.history)-stallCycleHistory:])
		}
	} else {
		g.history = nil
	}
	if obs.notice == "" && plain {
		if period, laps, ok := g.identicalCycleLocked(); ok {
			obs.notice = fmt.Sprintf("[icebath note: the last %d rounds repeated the same cycle of %d tool calls (ending with %s) with identical arguments and identical results. Do not repeat the batch — change arguments, use a different tool, or proceed with what you have.]", laps, period, name)
			obs.kind = "identical_cycle"
			if g.cfg.hardStop && laps >= g.cfg.noProgressBlock && g.halt == nil {
				g.decideLocked("halt", "identical_cycle_halt", name, laps, sig, decisionMessage("identical_cycle_halt", name, laps, period, 0))
			}
		}
	}
	if plain && count >= 2 && !failed && utf8.RuneCountInString(result) >= identicalStubMinChars {
		obs.stub = g.resultStubLocked(name, args)
	}
	return obs
}

func (g *Guardrails) identicalCycleLocked() (int, int, bool) {
	h := g.history
	for period := 2; period <= stallMaxCyclePeriod; period++ {
		if len(h) < period*stallIdenticalThreshold {
			continue
		}
		laps := 1
		for {
			base := len(h) - period*(laps+1)
			if base < 0 {
				break
			}
			equal := true
			for i := 0; i < period; i++ {
				a, b := h[base+i], h[len(h)-period+i]
				if a.sig != b.sig || a.hash != b.hash {
					equal = false
					break
				}
			}
			if !equal {
				break
			}
			laps++
		}
		if laps >= stallIdenticalThreshold {
			allRepeatable := true
			for i := 0; i < period; i++ {
				if !h[len(h)-period+i].repeatable {
					allRepeatable = false
					break
				}
			}
			if allRepeatable {
				continue
			}
			return period, laps, true
		}
	}
	return 0, 0, false
}

func (g *Guardrails) RecordPersisted(callID, path string) {
	if callID == "" || path == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.persisted[callID] = path
}

func (g *Guardrails) resultStubLocked(name string, args Args) string {
	preview := canonicalJSON(nodeOf(map[string]any(args)))
	if utf8.RuneCountInString(preview) > stubArgsPreviewChars {
		preview = runePrefix(preview, stubArgsPreviewChars) + "…"
	}
	first := g.streakFirstID
	ref := ""
	if first != "" {
		ref = " (tool_call_id " + first + ")"
	}
	stub := fmt.Sprintf("[icebath note: this result is byte-identical to the %s result earlier this turn%s. Refer to that result; it has not changed. Args: %s]", name, ref, preview)
	if first != "" {
		if path := g.persisted[first]; path != "" {
			stub += "\n[The referenced result was persisted to: " + path + " — page through it with read_file if you need the full content.]"
		}
	}
	return stub
}

type guardObservation struct {
	result    Result
	decision  GuardDecision
	stallKind string
	halt      *GuardDecision
}

func appendGuardText(r Result, text string) Result {
	if r.Parts == nil {
		r.Text += text
		return r
	}
	return AppendHint(r, text)
}

func (g *Guardrails) observe(name string, effects Effects, args Args, r Result, failed bool, callID string) guardObservation {
	g.mu.Lock()
	defer g.mu.Unlock()
	plain := r.Parts == nil
	decision := g.afterLocked(name, effects, args, r.Plain(), failed)
	var obs stallObservation
	if g.stall {
		obs = g.observeLocked(name, args, r.Text, plain, callID, failed)
	}
	if obs.stub != "" && plain {
		r.Text = obs.stub
	}
	if decision.Action == "warn" || decision.Action == "halt" {
		r = appendGuardText(r, GuardGuidance("", decision))
	}
	out := guardObservation{decision: decision}
	if obs.notice != "" {
		out.stallKind = obs.kind
	}
	if decision.ShouldHalt() {
		g.setTurnHaltLocked(decision)
		d := decision
		out.halt = &d
	} else if g.halt != nil && (g.halt.Code == "identical_call_streak_halt" || g.halt.Code == "identical_cycle_halt") {
		streak := *g.halt
		r = appendGuardText(r, GuardGuidance("", streak))
		g.setTurnHaltLocked(streak)
		out.halt = &streak
	}
	if obs.notice != "" {
		r = appendGuardText(r, "\n\n"+obs.notice)
	}
	out.result = r
	return out
}

func (g *Guardrails) blockResult(d GuardDecision) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.setTurnHaltLocked(d)
	return GuardBlockText(d)
}
