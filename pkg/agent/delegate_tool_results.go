package agent

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Text of tool result content with content block lists flattened and dicts serialized
func stringifyToolContent(content any) string {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case *tools.ParentCopy:
		return c.Text
	case []any:
		parts := make([]string, len(c))
		for i, item := range c {
			parts[i] = stringifyContentBlock(item)
		}
		return strings.Join(parts, "\n")
	case []map[string]any:
		parts := make([]string, len(c))
		for i, item := range c {
			parts[i] = stringifyContentBlock(item)
		}
		return strings.Join(parts, "\n")
	}
	if obj, ok := pyjson.AsObject(content); ok {
		return dumpsDefaultStr(obj)
	}
	return pyrepr.Str(content)
}

// One content block as its text field, its JSON form when a dict, otherwise its str form
func stringifyContentBlock(item any) string {
	obj, ok := pyjson.AsObject(item)
	if !ok {
		return pyrepr.Str(item)
	}
	if text, has := obj.Get("text"); has {
		switch t := text.(type) {
		case string:
			return t
		case *tools.ParentCopy:
			return t.Text
		}
	}
	return dumpsDefaultStr(obj)
}

// JSON text with non ASCII kept and unserializable values rendered through str
func dumpsDefaultStr(v any) string {
	text, _ := pyjson.Dumps(v, pyjson.EnsureASCII(false), pyjson.Default(func(x any) (any, error) { return pyrepr.Str(x), nil }))
	return text
}

var errorStatuses = map[string]bool{"error": true, "failed": true, "failure": true, "timeout": true}

// True for JSON with an error key or failed status, or a first line opening with a classic error marker
func looksLikeErrorOutput(content any) bool {
	text := stringifyToolContent(content)
	if text == "" {
		return false
	}
	if trimmed := pystr.Lstrip(text); strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		if parsed, err := pyjson.Loads(text); err == nil {
			if obj, ok := pyjson.AsObject(parsed); ok {
				errVal, _ := obj.Get("error")
				status, _ := obj.Get("status")
				statusText := ""
				if pyrepr.Truthy(status) {
					statusText = pyrepr.Str(status)
				}
				if pyrepr.Truthy(errVal) || errorStatuses[pystr.Lower(pystr.Strip(statusText))] {
					return true
				}
			}
		}
	}
	first := ""
	if lines := pystr.Splitlines(text, false); len(lines) > 0 {
		first = pystr.Lower(pystr.Strip(lines[0]))
	}
	for _, marker := range []string{"error:", "failed:", "traceback ", "exception:"} {
		if strings.HasPrefix(first, marker) {
			return true
		}
	}
	return false
}

// Hashable scalar form of a tool call id, false for values a dict cannot key on
func callIDKey(v any) (any, bool) {
	switch v.(type) {
	case string, int, int64, float64, bool, nil:
		return v, true
	}
	return nil, false
}

// Last tool results of a child conversation as tool, preview and error flag rows in chronological order
func extractOutputTail(env *tools.Env, result map[string]any, maxEntries, maxChars int) []any {
	switch result["messages"].(type) {
	case []any, []map[string]any:
	default:
		return []any{}
	}
	messages := resultMessages(result["messages"])
	nameByCallID := map[any]string{}
	for _, msg := range messages {
		if msg["role"] != "assistant" {
			continue
		}
		calls, _ := msg["tool_calls"].([]any)
		if typedCalls, ok := msg["tool_calls"].([]map[string]any); ok {
			for _, c := range typedCalls {
				calls = append(calls, c)
			}
		}
		for _, rawCall := range calls {
			tc, ok := pyjson.AsObject(rawCall)
			if !ok {
				continue
			}
			id, _ := tc.Get("id")
			key, hashable := callIDKey(id)
			if !pyrepr.Truthy(id) || !hashable {
				continue
			}
			name := "tool"
			if fnRaw, _ := tc.Get("function"); pyrepr.Truthy(fnRaw) {
				if fn, ok := pyjson.AsObject(fnRaw); ok {
					if n, _ := fn.Get("name"); pyrepr.Truthy(n) {
						name = pyrepr.Str(n)
					}
				}
			}
			nameByCallID[key] = name
		}
	}
	tail := []any{}
	for i := len(messages) - 1; i >= 0; i-- {
		if len(tail) >= maxEntries {
			break
		}
		msg := messages[i]
		if msg["role"] != "tool" {
			continue
		}
		var body any = ""
		if pyrepr.Truthy(msg["content"]) {
			body = msg["content"]
		}
		content := tools.RedactChildText(env, stringifyToolContent(body), false)
		var callID any = ""
		if pyrepr.Truthy(msg["tool_call_id"]) {
			callID = msg["tool_call_id"]
		}
		toolName := "tool"
		if key, ok := callIDKey(callID); ok {
			if n, found := nameByCallID[key]; found {
				toolName = n
			}
		}
		row := pyjson.NewObject()
		row.Set("tool", toolName)
		row.Set("preview", pystr.Slice(content, 0, maxChars))
		row.Set("is_error", looksLikeErrorOutput(content))
		tail = append(tail, row)
	}
	slices.Reverse(tail)
	return tail
}

var toolInputTargetKeys = map[string]bool{
	"cwd": true, "destination_path": true, "directory": true, "dst": true, "endpoint": true, "file_path": true, "new_path": true,
	"old_path": true, "path": true, "source_path": true, "src": true, "target_path": true, "url": true, "urls": true,
}

var toolInputURLKeys = map[string]bool{"endpoint": true, "url": true, "urls": true}

// Bounded side effect target with URL credentials, query and fragment dropped, nil when unusable
func sanitizeToolTarget(env *tools.Env, key string, value any, force bool) any {
	if list, ok := value.([]any); ok {
		cleaned := []any{}
		for _, item := range list[:min(16, len(list))] {
			if c := sanitizeToolTarget(env, key, item, force); c != nil {
				cleaned = append(cleaned, c)
			}
		}
		if len(cleaned) == 0 {
			return nil
		}
		return cleaned
	}
	s, ok := value.(string)
	if !ok || s == "" {
		return nil
	}
	bounded := pystr.Slice(tools.RedactChildText(env, s, force), 0, 1024)
	if toolInputURLKeys[key] {
		parsed, err := pyurl.Split(bounded)
		if err != nil {
			return nil
		}
		if parsed.Scheme != "" && parsed.Netloc != "" {
			hostname := parsed.Hostname()
			if hostname == "" {
				return nil
			}
			host := hostname
			if strings.Contains(hostname, ":") {
				host = "[" + hostname + "]"
			}
			port, hasPort, err := parsed.Port()
			if err != nil {
				return nil
			}
			netloc := host
			if hasPort {
				netloc = fmt.Sprintf("%s:%d", host, port)
			}
			return pyurl.Unsplit(pyurl.URL{Scheme: parsed.Scheme, Netloc: netloc, Path: parsed.Path})
		}
	}
	return bounded
}

// Known side effect target keys of a mapping, each sanitized with URL secrets dropped
func sanitizeTargets(env *tools.Env, mapping *pyjson.Object, force bool) *pyjson.Object {
	targets := pyjson.NewObject()
	for _, rawKey := range mapping.Keys {
		key := pystr.Lower(rawKey)
		if !toolInputTargetKeys[key] {
			continue
		}
		if cleaned := sanitizeToolTarget(env, key, mapping.Vals[rawKey], force); cleaned != nil {
			targets.Set(key, cleaned)
		}
	}
	return targets
}

// Argument keys and targets with bounded sanitized contents, empty parts for malformed shapes
func inputSummary(env *tools.Env, keys any, targets any, force bool) *pyjson.Object {
	argumentKeys := []any{}
	var keyList []any
	switch k := keys.(type) {
	case []any:
		keyList = k
	case []string:
		for _, s := range k {
			keyList = append(keyList, s)
		}
	}
	for _, key := range keyList[:min(64, len(keyList))] {
		argumentKeys = append(argumentKeys, pystr.Slice(tools.RedactChildText(env, pyrepr.Str(key), force), 0, 128))
	}
	out := pyjson.NewObject()
	out.Set("argument_keys", argumentKeys)
	if mapping, ok := pyjson.AsObject(targets); ok {
		out.Set("targets", sanitizeTargets(env, mapping, force))
	} else {
		out.Set("targets", pyjson.NewObject())
	}
	return out
}

// Argument names and side effect targets of a tool call without its raw payload
func summarizeToolArguments(env *tools.Env, arguments any) *pyjson.Object {
	var parsed any
	if s, ok := arguments.(string); ok {
		if v, err := pyjson.Loads(s); err == nil {
			parsed = v
		}
	}
	obj, ok := pyjson.AsObject(parsed)
	if !ok {
		return inputSummary(env, []any{}, pyjson.NewObject(), true)
	}
	keys := slices.Clone(obj.Keys)
	slices.Sort(keys)
	return inputSummary(env, keys, obj, false)
}

// Non negative integer byte count of a trace field, zero for absent, boolean or non numeric values
func traceByteCount(item *pyjson.Object, key string) (int, error) {
	value, ok := item.Get(key)
	if !ok {
		return 0, nil
	}
	if _, isBool := value.(bool); isBool {
		return 0, nil
	}
	switch value.(type) {
	case float32, float64:
	default:
		if _, isInt := pyint.Instance(value); !isInt {
			return 0, nil
		}
	}
	n, err := pyint.Of(value)
	if err != nil {
		return 0, err
	}
	if n.Sign() < 0 {
		return 0, nil
	}
	if !n.IsInt64() {
		return math.MaxInt, nil
	}
	return int(n.Int64()), nil
}

// Metadata only tool history for lifecycle hooks with input summaries sanitized again under force
func subagentStopToolCallHistory(env *tools.Env, toolTrace any) ([]any, error) {
	trace, ok := toolTrace.([]any)
	if !ok {
		return []any{}, nil
	}
	history := []any{}
	for _, raw := range trace {
		item, ok := pyjson.AsObject(raw)
		if !ok {
			continue
		}
		statusVal, _ := item.Get("status")
		status := "unknown"
		if pyrepr.Truthy(statusVal) {
			status = pyrepr.Str(statusVal)
		}
		status = pystr.Lower(status)
		summaryRaw, _ := item.Get("input_summary")
		summary, ok := pyjson.AsObject(summaryRaw)
		if !ok {
			summary = pyjson.NewObject()
		}
		toolVal, _ := item.Get("tool")
		toolName := "unknown"
		if pyrepr.Truthy(toolVal) {
			toolName = pyrepr.Str(toolVal)
		}
		argKeys, _ := summary.Get("argument_keys")
		targets, _ := summary.Get("targets")
		inBytes, err := traceByteCount(item, "args_bytes")
		if err != nil {
			return nil, err
		}
		outBytes, err := traceByteCount(item, "result_bytes")
		if err != nil {
			return nil, err
		}
		if status != "ok" && status != "error" {
			status = "unknown"
		}
		row := pyjson.NewObject()
		row.Set("tool_name", pystr.Slice(toolName, 0, 256))
		row.Set("tool_input", inputSummary(env, argKeys, targets, true))
		row.Set("input_bytes", inBytes)
		row.Set("output_bytes", outBytes)
		row.Set("status", status)
		history = append(history, row)
	}
	return history, nil
}

// Hard per summary character ceiling layered over the dynamic headroom budget, zero disabling it
const DefaultMaxSummaryChars = 24000

const (
	summaryHeadroomFraction = 0.5
	minSummaryChars         = 2000
)

// Home whose cache receives delegation spills, the agent's own home before the context home
func delegationSpillHome(ctx context.Context, env *tools.Env) string {
	if env != nil && env.Home != "" {
		return env.Home
	}
	return home.Dir(ctx)
}

// Writes the full summary under cache delegation and returns its path, empty when the write fails
func spillSummaryToFile(ctx context.Context, env *tools.Env, taskIndex any, summary string) string {
	cacheDir := home.HermesDir(delegationSpillHome(ctx, env), "cache/delegation", "delegation_cache")
	if err := os.MkdirAll(cacheDir, 0o777); err != nil {
		delegateLog().Debug(fmt.Sprintf("Failed to spill subagent summary to file: %s", err))
		return ""
	}
	now := time.Now()
	stamp := fmt.Sprintf("%s_%06d", now.Format("20060102_150405"), now.Nanosecond()/1000)
	path := filepath.Join(cacheDir, fmt.Sprintf("subagent-summary-%s-%s.txt", pyrepr.Str(taskIndex), stamp))
	if err := tools.WriteTextExclusive(path, summary, false, false); err != nil {
		delegateLog().Debug(fmt.Sprintf("Failed to spill subagent summary to file: %s", err))
		return ""
	}
	return path
}

// Head and tail window of an over budget summary with the full forced projection spilled to disk
func trimSummaryWithFooter(ctx context.Context, env *tools.Env, summary any, cap int, taskIndex any) (any, string) {
	var forced, text string
	if pc, ok := summary.(*tools.ParentCopy); ok {
		forced, text = tools.RedactChildText(env, pc, true), pc.Text
	} else {
		text = pyrepr.Str(summary)
		projected := tools.RedactSummary(env, summary, true, true)
		if s, ok := projected.(string); ok {
			forced = s
		} else {
			forced = pyrepr.Str(projected)
		}
	}
	spillPath := spillSummaryToFile(ctx, env, taskIndex, forced)
	if spillPath != "" {
		spillPath = tools.ToAgentVisibleCachePath(env, spillPath)
	}
	modelText := headTailWithFooter(text, cap, spillPath)
	return tools.NewParentCopy(modelText, headTailWithFooter(forced, cap, spillPath)), spillPath
}

// Line snapped three quarter head and quarter tail of a summary with a footer locating the omitted middle
func headTailWithFooter(summary string, cap int, spillPath string) string {
	runes := pystr.Runes(summary)
	originalLen := len(runes)
	headBudget := int(float64(cap) * 0.75)
	tailBudget := cap - headBudget
	head := runes[:min(headBudget, originalLen)]
	tail := runes
	if tailBudget > 0 && tailBudget < originalLen {
		tail = runes[originalLen-tailBudget:]
	}
	if nl := lastRuneIndex(head, '\n'); float64(nl) > float64(headBudget)*0.5 {
		head = head[:nl]
	}
	if nl := slices.Index(tail, '\n'); nl >= 0 && float64(nl) < float64(tailBudget)*0.5 {
		tail = tail[nl+1:]
	}
	headText, tailText := pystr.FromRunes(head), pystr.FromRunes(tail)
	footer := []string{
		"", strings.Repeat("─", 8) + " [SUMMARY TRUNCATED] " + strings.Repeat("─", 8),
		fmt.Sprintf("Showing %s chars (head) + %s chars (tail) of %s total — trimmed to protect the parent's context window.",
			pystr.GroupThousands(int64(len(head))), pystr.GroupThousands(int64(len(tail))), pystr.GroupThousands(int64(originalLen))),
	}
	if spillPath != "" {
		footer = append(footer, "Full subagent output saved to: "+spillPath,
			fmt.Sprintf("To read the omitted middle: read_file path=\"%s\" offset=%d limit=200  (the file is the complete summary; raise/lower offset to page through it).",
				spillPath, strings.Count(headText, "\n")+2))
	} else {
		footer = append(footer, "Full output could not be stored to disk; the head+tail above is all that was preserved.")
	}
	footer = append(footer, strings.Repeat("─", 37))
	return headText + "\n\n[... middle omitted — see footer ...]\n\n" + tailText + strings.Join(footer, "\n")
}

// Index of the last occurrence of r, minus one when absent
func lastRuneIndex(rs []rune, r rune) int {
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i] == r {
			return i
		}
	}
	return -1
}

// Positive integer of a real number, the error int raises for NaN or infinity
func positiveTokens(v any) (int, bool, error) {
	switch f := v.(type) {
	case float64:
		if !(f > 0) {
			return 0, false, nil
		}
	case float32:
		if !(f > 0) {
			return 0, false, nil
		}
	default:
		n, ok := pyint.Instance(v)
		if !ok || n.Sign() <= 0 {
			return 0, false, nil
		}
	}
	n, err := pyint.Of(v)
	if err != nil {
		return 0, false, err
	}
	if !n.IsInt64() {
		return math.MaxInt, true, nil
	}
	return int(n.Int64()), true, nil
}

// Current prompt size of the parent from its own last prompt size then its last provider usage
func parentPromptSizeTokens(parent *AIAgent) (int, bool, error) {
	if parent == nil {
		return 0, false, nil
	}
	if parent.lastPromptSizeTokens > 0 {
		return parent.lastPromptSizeTokens, true, nil
	}
	if parent.lastTurnUsage == nil {
		return 0, false, nil
	}
	return positiveTokens(parent.lastTurnUsage["prompt_tokens"])
}

// Output reserve of a context engine, zero when the engine declares none
func engineMaxTokens(engine ContextEngine) (int, error) {
	var raw any
	switch e := engine.(type) {
	case *ContextCompressor:
		if e.maxTokens != nil {
			raw = *e.maxTokens
		}
	case *hostedContextEngine:
		if v, err := e.obj.Get(e.ctx, "max_tokens"); err == nil {
			raw = v
		}
	}
	if !pyrepr.Truthy(raw) {
		return 0, nil
	}
	n, err := pyint.Of(raw)
	if err != nil {
		return 0, err
	}
	if !n.IsInt64() {
		if n.Sign() > 0 {
			return math.MaxInt, nil
		}
		return math.MinInt, nil
	}
	return int(n.Int64()), nil
}

// Per summary character budget from the parent's remaining context headroom, false when its context state is unknown
func parentSummaryCharBudget(parent *AIAgent, nSummaries int) (budget int, known bool) {
	defer func() {
		if r := recover(); r != nil {
			if isUserInterruptPanic(r) {
				panic(r)
			}
			delegateLog().Debug("Summary budget computation failed", "error", r)
			budget, known = 0, false
		}
	}()
	if parent == nil || parent.contextCompressor == nil {
		return 0, false
	}
	contextLength := engineContextLength(parent.contextCompressor)
	if contextLength <= 0 {
		return 0, false
	}
	used, ok, err := parentPromptSizeTokens(parent)
	if err != nil {
		delegateLog().Debug("Summary budget computation failed", "error", err)
		return 0, false
	}
	if !ok {
		return 0, false
	}
	reserve, err := engineMaxTokens(parent.contextCompressor)
	if err != nil {
		delegateLog().Debug("Summary budget computation failed", "error", err)
		return 0, false
	}
	headroom := contextLength - used - reserve
	if headroom <= 0 {
		return minSummaryChars, true
	}
	perSummaryTokens := int(float64(headroom)*summaryHeadroomFraction) / max(1, nSummaries)
	return max(minSummaryChars, perSummaryTokens*4), true
}

// Text of a string or parent copy summary, false for any other value
func summaryText(v any) (string, bool) {
	switch s := v.(type) {
	case string:
		return s, true
	case *tools.ParentCopy:
		return s.Text, true
	}
	return "", false
}

// Static summary ceiling from the delegation config, the default when unparsable
func staticSummaryCeiling(ctx context.Context) int {
	raw, ok := tools.LoadDelegationConfig(ctx)["max_summary_chars"]
	if !ok {
		return DefaultMaxSummaryChars
	}
	n, err := pyint.Of(raw)
	if err != nil {
		return DefaultMaxSummaryChars
	}
	if !n.IsInt64() {
		if n.Sign() > 0 {
			return math.MaxInt
		}
		return 0
	}
	return int(n.Int64())
}

// Trims summaries in place so a batch cannot overflow the parent's window, spilling each full text to disk
func applySummaryBudget(ctx context.Context, results []*pyjson.Object, parent *AIAgent) {
	var summaries []*pyjson.Object
	for _, r := range results {
		if r == nil {
			continue
		}
		v, _ := r.Get("summary")
		if s, ok := summaryText(v); ok && s != "" {
			summaries = append(summaries, r)
		}
	}
	if len(summaries) == 0 {
		return
	}
	var candidates []int
	if c := staticSummaryCeiling(ctx); c > 0 {
		candidates = append(candidates, c)
	}
	if c, ok := parentSummaryCharBudget(parent, len(summaries)); ok && c > 0 {
		candidates = append(candidates, c)
	}
	if len(candidates) == 0 {
		return
	}
	limit := slices.Min(candidates)
	env := finalizeEnv(parent)
	for _, entry := range summaries {
		summary, _ := entry.Get("summary")
		text, _ := summaryText(summary)
		length := pystr.Len(text)
		if length <= limit {
			continue
		}
		taskIndex, ok := entry.Get("task_index")
		if !ok {
			taskIndex = -1
		}
		modelText, spillPath := trimSummaryWithFooter(ctx, env, summary, limit, taskIndex)
		entry.Set("summary", modelText)
		entry.Set("summary_truncated", true)
		if spillPath != "" {
			entry.Set("summary_full_path", spillPath)
		}
		logIndex, ok := entry.Get("task_index")
		if !ok {
			logIndex = "?"
		}
		spill := spillPath
		if spill == "" {
			spill = "none"
		}
		delegateLog().Debug(fmt.Sprintf("[subagent-%s] summary trimmed %d → ~%d chars (spill=%s)", pyrepr.Str(logIndex), length, limit, spill))
	}
}

var parentFinalizationFallbackMu sync.Mutex

type finalizationHeldKey struct{}

// Takes the per parent lock serializing lifecycle side effects, reentrant along one call chain
func parentFinalizationLock(ctx context.Context, parent *AIAgent) (context.Context, func()) {
	mu := &parentFinalizationFallbackMu
	if parent != nil {
		mu = &parent.subagentFinalizationMu
	}
	held, _ := ctx.Value(finalizationHeldKey{}).([]*sync.Mutex)
	if slices.Contains(held, mu) {
		return ctx, func() {}
	}
	mu.Lock()
	next := append(slices.Clone(held), mu)
	return context.WithValue(ctx, finalizationHeldKey{}, next), mu.Unlock
}

// Integer dict key a task index value hashes to, bools and integral floats included
func childIndexKey(v any) (int, bool) {
	if n, ok := pyint.Instance(v); ok {
		if n.IsInt64() {
			return int(n.Int64()), true
		}
		return 0, false
	}
	switch f := v.(type) {
	case float64:
		if f == math.Trunc(f) && !math.IsInf(f, 0) {
			return int(f), true
		}
	case float32:
		return childIndexKey(float64(f))
	}
	return 0, false
}

// Child of the batch keyed by a result's task index, nil when absent
func childAt(childByIndex map[int]*AIAgent, taskIndex any) *AIAgent {
	if k, ok := childIndexKey(taskIndex); ok {
		return childByIndex[k]
	}
	return nil
}

// Redaction environment of the parent, nil without a parent
func finalizeEnv(parent *AIAgent) *tools.Env {
	if parent == nil {
		return nil
	}
	return parent.env
}

// Feeds each forced redacted child summary to the parent's memory providers
func notifyMemoryManager(ctx context.Context, results []*pyjson.Object, taskList []map[string]any, childByIndex map[int]*AIAgent, parent *AIAgent) {
	if parent == nil || parent.memoryManager == nil {
		return
	}
	mm, env := parent.memoryManager, parent.env
	for _, entry := range results {
		delegateQuiet("", func() {
			taskIndex, ok := entry.Get("task_index")
			if !ok {
				taskIndex = -1
			}
			task := ""
			if n, isInt := pyint.Instance(taskIndex); isInt && n.Sign() >= 0 && n.Cmp(big.NewInt(int64(len(taskList)))) < 0 {
				goal, has := taskList[int(n.Int64())]["goal"]
				if !has {
					return
				}
				if s, ok := goal.(string); ok {
					task = s
				} else {
					task = pyrepr.Str(goal)
				}
			}
			summary, _ := entry.Get("summary")
			childSessionID := ""
			if child := childAt(childByIndex, taskIndex); child != nil {
				childSessionID = child.sessionID
			}
			mm.OnDelegation(ctx, task, tools.RedactChildText(env, summary, true), memory.DelegationOptions{ChildSessionID: childSessionID})
		})
	}
}

// Duration in whole milliseconds the way int of seconds times one thousand truncates
func durationMillis(v any) (int, error) {
	if !pyrepr.Truthy(v) {
		return 0, nil
	}
	if n, ok := pyint.Instance(v); ok {
		ms := new(big.Int).Mul(n, big.NewInt(1000))
		if !ms.IsInt64() {
			return 0, fmt.Errorf("duration out of range")
		}
		return int(ms.Int64()), nil
	}
	f, ok := v.(float64)
	if !ok {
		if f32, is32 := v.(float32); is32 {
			f, ok = float64(f32), true
		}
	}
	if !ok {
		return 0, fmt.Errorf("unsupported operand type(s) for *: '%s' and 'int'", pyrepr.TypeName(v))
	}
	n, err := pyint.Of(f * 1000)
	if err != nil {
		return 0, err
	}
	return int(n.Int64()), nil
}

// Pops the hidden role and cost fields, fires subagent_stop per child and returns the summed child cost
func fireSubagentStopHooks(ctx context.Context, results []*pyjson.Object, childByIndex map[int]*AIAgent, parent *AIAgent) float64 {
	env := finalizeEnv(parent)
	total := 0.0
	for _, entry := range results {
		childRole, _ := entry.Pop("_child_role")
		childCost, hasCost := entry.Pop("_child_cost_usd")
		if !hasCost {
			childCost = 0.0
		}
		if pyrepr.Truthy(childCost) {
			if f, err := pyint.Float(childCost); err == nil {
				total += f
			}
		}
		delegateQuiet("subagent_stop hook invocation failed", func() {
			taskIndex, ok := entry.Get("task_index")
			if !ok {
				taskIndex = -1
			}
			child := childAt(childByIndex, taskIndex)
			summary, _ := entry.Get("summary")
			status, _ := entry.Get("status")
			trace, _ := entry.Get("tool_trace")
			history, err := subagentStopToolCallHistory(env, trace)
			if err != nil {
				panic(err)
			}
			duration, _ := entry.Get("duration_seconds")
			ms, err := durationMillis(duration)
			if err != nil {
				panic(err)
			}
			var parentSessionID, childSessionID any
			parentTurnID := ""
			if parent != nil {
				parentSessionID, parentTurnID = parent.sessionID, parent.currentTurnID
			}
			if child != nil {
				childSessionID = child.sessionID
			}
			plugins.InvokeHook(ctx, "subagent_stop", map[string]any{
				"parent_session_id": parentSessionID, "parent_turn_id": parentTurnID, "child_session_id": childSessionID,
				"child_role": childRole, "child_summary": tools.RedactChildValue(env, summary, true), "child_status": status,
				"tool_call_history": history, "duration_ms": ms,
			})
		})
	}
	return total
}

// Folds the children's spend into the parent's session cost, marking source and status when unset
func rollupChildrenCost(parent *AIAgent, childrenCostTotal float64) {
	if childrenCostTotal <= 0.0 || parent == nil {
		return
	}
	parent.sessionEstimatedCostUSD += childrenCostTotal
	if parent.sessionCostSource == "" || parent.sessionCostSource == "none" {
		parent.sessionCostSource = "subagent"
	}
	if parent.sessionCostStatus == "" || parent.sessionCostStatus == "unknown" {
		parent.sessionCostStatus = "estimated"
	}
}

// True for a dict shaped output schema
func isSchemaDict(v any) bool {
	_, ok := pyjson.AsObject(v)
	return ok
}

// Applies the host owned summary, memory, hook and cost contracts to a finished batch once
func finalizeChildResults(ctx context.Context, results []*pyjson.Object, taskList []map[string]any, children []delegateChild, parent *AIAgent) {
	ctx, unlock := parentFinalizationLock(ctx, parent)
	defer unlock()
	childByIndex := make(map[int]*AIAgent, len(children))
	for _, c := range children {
		childByIndex[c.Index] = c.Child
	}
	env := finalizeEnv(parent)
	for _, entry := range results {
		taskIndex, _ := entry.Get("task_index")
		var schema any
		if child := childAt(childByIndex, taskIndex); child != nil {
			schema = child.delegateOutputSchema
		}
		summary, _ := entry.Get("summary")
		_, hasValid := entry.Get("schema_valid")
		forced := tools.RedactSummary(env, summary, isSchemaDict(schema) || hasValid, true)
		if cleaned, ok := tools.RedactChildResult(env, entry, schema, false).(*pyjson.Object); ok {
			entry.Update(cleaned)
		}
		if _, isStr := summaryText(summary); !isStr {
			continue
		}
		current, _ := entry.Get("summary")
		text, isStr := summaryText(current)
		forcedText, forcedIsStr := summaryText(forced)
		if isStr && forcedIsStr {
			entry.Set("summary", tools.NewParentCopy(text, forcedText))
		}
	}
	applySummaryBudget(ctx, results, parent)
	notifyMemoryManager(ctx, results, taskList, childByIndex, parent)
	rollupChildrenCost(parent, fireSubagentStopHooks(ctx, results, childByIndex, parent))
}

// Child runner and child builder of delegate_task, swapped by tests the way the Python facade is patched
var (
	runSingleChildFn                  = runSingleChild
	buildChildAgentFn                 = buildChildAgent
	buildChildPreservingParentToolsFn = buildChildPreservingParentTools
)

// Runs one child and applies the host lifecycle delegate_task uses
func runChildLifecycle(ctx context.Context, taskIndex int, goal string, child, parent *AIAgent) *pyjson.Object {
	result := runSingleChildFn(ctx, taskIndex, goal, child, parent, "", nil, nil)
	if _, ok := result.Get("task_index"); !ok {
		result.Set("task_index", taskIndex)
	}
	task := map[string]any{"goal": goal}
	taskList := make([]map[string]any, 0, max(taskIndex, 0)+1)
	for range taskIndex {
		taskList = append(taskList, map[string]any{"goal": ""})
	}
	taskList = append(taskList, task)
	finalizeChildResults(ctx, []*pyjson.Object{result}, taskList, []delegateChild{{Index: taskIndex, Task: task, Child: child}}, parent)
	return result
}

// Serializes child construction so one build's resolved tool names never leak into another
var childConstructionMu sync.Mutex

// Builds a child and restores the parent's resolved tool names the build replaced
func buildChildPreservingParentTools(ctx context.Context, spec childBuildSpec) (*AIAgent, error) {
	childConstructionMu.Lock()
	parentToolNames := tools.LastResolvedToolNames()
	if parentToolNames == nil {
		parentToolNames = []string{}
	}
	child, err := func() (*AIAgent, error) {
		defer tools.RestoreLastResolvedToolNames(parentToolNames)
		return buildChildAgentFn(ctx, spec)
	}()
	childConstructionMu.Unlock()
	if err != nil {
		return nil, err
	}
	child.delegateSavedToolNames = parentToolNames
	return child, nil
}
