package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// ---- events ----

// Field of a decoded event or item, nil when absent
func codexEventField(event any, name string) any {
	m, ok := event.(map[string]any)
	if !ok {
		return nil
	}
	return m[name]
}

var codexProgressDeltaTypes = map[string]bool{
	"response.output_text.delta": true, "response.reasoning_summary_text.delta": true, "response.text.delta": true,
	"response.audio.delta": true, "response.function_call_arguments.delta": true, "response.reasoning_text.delta": true,
	"response.refusal.delta": true,
}

// Reports whether an event carries substantive forward progress rather than lifecycle liveness
func codexEventHasContent(event map[string]any) bool {
	eventType := stringOf(event["type"])
	if codexProgressDeltaTypes[eventType] {
		return pyTruthy(event["delta"])
	}
	if eventType == "response.output_item.added" {
		item := event["item"]
		if !strings.Contains(stringOf(codexEventField(item, "type")), "function_call") {
			return false
		}
		for _, field := range []string{"id", "call_id", "name", "arguments"} {
			if pyTruthy(codexEventField(item, field)) {
				return true
			}
		}
	}
	return false
}

// CodexStreamErrorEvent is a provider error synthesized from a standalone type=error SSE frame
type CodexStreamErrorEvent struct {
	Message    string
	Code       string
	Param      string
	StatusCode int
	Body       map[string]any
}

func (e *CodexStreamErrorEvent) Error() string { return e.Message }

// BodyText is the OpenAI shaped error body as JSON for the classifier
func (e *CodexStreamErrorEvent) BodyText() string {
	raw, _ := json.Marshal(e.Body)
	return string(raw)
}

// Error event from a type=error frame reading top level fields first then the error envelope
func codexStreamError(event map[string]any) *CodexStreamErrorEvent {
	nested := event["error"]
	field := func(name string) any {
		if v := event[name]; v != nil {
			return v
		}
		if nested != nil {
			return codexEventField(nested, name)
		}
		return nil
	}
	message := "stream emitted error event"
	if raw := field("message"); raw != nil {
		if s := strings.TrimSpace(strOrEmpty(raw)); s != "" {
			message = s
		}
	}
	code := strOrEmpty(field("code"))
	param := strOrEmpty(field("param"))
	var codeVal, paramVal any
	if field("code") != nil {
		codeVal = code
	}
	if field("param") != nil {
		paramVal = param
	}
	return &CodexStreamErrorEvent{Message: message, Code: code, Param: param,
		Body: map[string]any{"error": map[string]any{"message": message, "code": codeVal, "param": paramVal, "type": "error"}}}
}

func codexMessagePhase(item any) string {
	phase, ok := codexEventField(item, "phase").(string)
	if !ok {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(phase))
}

// Concatenated output_text parts of a message item
func codexOutputTextOf(item any) string {
	var b strings.Builder
	for _, p := range codexList(codexEventField(item, "content")) {
		if codexEventField(p, "type") == "output_text" {
			b.WriteString(strOrEmpty(codexEventField(p, "text")))
		}
	}
	return strings.TrimSpace(b.String())
}

// ---- assembler ----

type codexPendingCall struct {
	item        map[string]any
	arguments   string
	outputIndex any
	sequence    int
}

// Assembles a Responses object from raw SSE events with announced order and pending call settlement
type codexAssembler struct {
	model        string
	textFn       func(string) error
	reasoningFn  func(string) error
	commentaryFn func(string) error
	firstDeltaFn func() error
	toolStartFn  func(string) error

	hasToolCalls          bool
	firstDeltaFired       bool
	sawTerminal           bool
	nextOutputSequence    int
	activeMessagePhase    string
	activeSummaryIndex    any
	terminalStatus        string
	terminalUsage         map[string]any
	terminalResponseID    string
	terminalIncomplete    any
	terminalError         any
	terminalServiceTier   any
	sawResponseCompleted  bool
	sawAnswerItemAdded    bool
	outputItems           []map[string]any
	outputIndexes         []any
	outputSequences       []int
	textDeltas            []string
	commentaryTextDeltas  []string
	pendingFunctionCalls  map[string]*codexPendingCall
	pendingOrder          []string
	announcedOutputOrder  map[string][2]any
	messagePhases         map[string]string
	hasActiveMessagePhase bool
	toolStartErr          error
}

func newCodexAssembler(model string) *codexAssembler {
	return &codexAssembler{model: model, terminalStatus: "completed", pendingFunctionCalls: map[string]*codexPendingCall{},
		announcedOutputOrder: map[string][2]any{}, messagePhases: map[string]string{}}
}

func (a *codexAssembler) onItemAdded(event map[string]any) {
	item, _ := event["item"].(map[string]any)
	itemType := stringOf(codexEventField(item, "type"))
	a.activeMessagePhase = ""
	a.hasActiveMessagePhase = false
	if itemType == "message" {
		a.activeMessagePhase = codexMessagePhase(item)
		a.hasActiveMessagePhase = true
		if a.activeMessagePhase != "commentary" && a.activeMessagePhase != "analysis" {
			a.sawAnswerItemAdded = true
		}
	}
	if a.activeMessagePhase == "commentary" {
		a.commentaryTextDeltas = nil
	}
	itemID := stringOf(codexEventField(item, "id"))
	if itemID != "" && itemType == "message" {
		a.messagePhases[itemID] = a.activeMessagePhase
	}
	if itemID != "" {
		if _, seen := a.announcedOutputOrder[itemID]; !seen {
			a.announcedOutputOrder[itemID] = [2]any{a.nextOutputSequence, event["output_index"]}
			a.nextOutputSequence++
		}
	}
	if strings.Contains(itemType, "function_call") {
		a.hasToolCalls = true
		if name := stringOf(codexEventField(item, "name")); name != "" && a.toolStartFn != nil {
			a.toolStartErr = a.toolStartFn(name)
		}
		if itemID != "" {
			order := a.announcedOutputOrder[itemID]
			if _, exists := a.pendingFunctionCalls[itemID]; !exists {
				a.pendingOrder = append(a.pendingOrder, itemID)
			}
			a.pendingFunctionCalls[itemID] = &codexPendingCall{item: item, arguments: strOrEmpty(codexEventField(item, "arguments")),
				outputIndex: order[1], sequence: order[0].(int)}
		}
	}
}

func (a *codexAssembler) phaseFor(event map[string]any) string {
	itemID := stringOf(event["item_id"])
	if phase, ok := a.messagePhases[itemID]; ok && itemID != "" {
		return phase
	}
	return a.activeMessagePhase
}

func (a *codexAssembler) onTextDelta(event map[string]any) error {
	delta := strOrEmpty(event["delta"])
	if delta == "" {
		return nil
	}
	switch a.phaseFor(event) {
	case "commentary":
		a.commentaryTextDeltas = append(a.commentaryTextDeltas, delta)
		if a.commentaryFn == nil && a.reasoningFn != nil {
			return a.reasoningFn(delta)
		}
	case "analysis":
		if a.reasoningFn != nil {
			return a.reasoningFn(delta)
		}
	default:
		a.textDeltas = append(a.textDeltas, delta)
		if a.hasToolCalls {
			return nil
		}
		if !a.firstDeltaFired {
			a.firstDeltaFired = true
			if a.firstDeltaFn != nil {
				if err := a.firstDeltaFn(); err != nil {
					return err
				}
			}
		}
		if a.textFn != nil {
			return a.textFn(delta)
		}
	}
	return nil
}

func (a *codexAssembler) onRefusalDelta(event map[string]any) {
	if refusal, ok := event["delta"].(string); ok && refusal != "" {
		a.textDeltas = append(a.textDeltas, refusal)
	}
}

// Pending call key by item id, else by the response local output index
func (a *codexAssembler) pendingFunctionKey(itemID string, outputIndex any) (string, bool) {
	if _, ok := a.pendingFunctionCalls[itemID]; ok {
		return itemID, true
	}
	if outputIndex != nil {
		for _, key := range a.pendingOrder {
			pending, ok := a.pendingFunctionCalls[key]
			if ok && codexIndexEqual(pending.outputIndex, outputIndex) {
				return key, true
			}
		}
	}
	return "", false
}

func codexIndexEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	fa, aNum := codexIndexFloat(a)
	fb, bNum := codexIndexFloat(b)
	if aNum && bNum {
		return fa == fb
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func codexIndexFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}

func (a *codexAssembler) onFunctionCall(event map[string]any, eventType string) {
	a.hasToolCalls = true
	key, ok := a.pendingFunctionKey(stringOf(event["item_id"]), event["output_index"])
	if !ok {
		return
	}
	pending := a.pendingFunctionCalls[key]
	if strings.Contains(eventType, "delta") {
		pending.arguments += strOrEmpty(event["delta"])
	} else if strings.HasSuffix(eventType, "function_call_arguments.done") {
		if done, has := event["arguments"]; has && done != nil {
			pending.arguments = strOrEmpty(done)
		}
	}
}

func (a *codexAssembler) onReasoningDelta(event map[string]any) error {
	text := strOrEmpty(event["delta"])
	if text == "" || a.reasoningFn == nil {
		return nil
	}
	if idx, has := event["summary_index"]; has && idx != nil {
		if a.activeSummaryIndex != nil && !codexIndexEqual(idx, a.activeSummaryIndex) {
			text = "\n\n" + text
		}
		a.activeSummaryIndex = idx
	}
	return a.reasoningFn(text)
}

func (a *codexAssembler) onItemDone(event map[string]any) error {
	doneItem, _ := event["item"].(map[string]any)
	if doneItem == nil {
		return nil
	}
	a.outputItems = append(a.outputItems, doneItem)
	doneID := stringOf(doneItem["id"])
	var announcedSequence any
	var announcedIndex any
	if order, ok := a.announcedOutputOrder[doneID]; ok {
		announcedSequence, announcedIndex = order[0], order[1]
	}
	var pendingKeys []string
	if strings.Contains(stringOf(doneItem["type"]), "function_call") {
		doneCallID, _ := doneItem["call_id"].(string)
		if strings.TrimSpace(doneCallID) != "" {
			for _, key := range a.pendingOrder {
				pending, ok := a.pendingFunctionCalls[key]
				if ok && codexEventField(pending.item, "call_id") == doneCallID {
					pendingKeys = append(pendingKeys, key)
				}
			}
			if len(pendingKeys) == 0 {
				if key, ok := a.pendingFunctionKey(doneID, event["output_index"]); ok && !pyTruthy(codexEventField(a.pendingFunctionCalls[key].item, "call_id")) {
					pendingKeys = []string{key}
				}
			}
			announcedSequence, announcedIndex = nil, nil
		} else if key, ok := a.pendingFunctionKey(doneID, event["output_index"]); ok {
			pendingKeys = []string{key}
		}
		if len(pendingKeys) > 0 {
			alias := a.pendingFunctionCalls[pendingKeys[0]]
			for _, key := range pendingKeys[1:] {
				if p := a.pendingFunctionCalls[key]; p.sequence < alias.sequence {
					alias = p
				}
			}
			announcedSequence, announcedIndex = alias.sequence, alias.outputIndex
		}
	}
	if announcedSequence == nil {
		announcedSequence = a.nextOutputSequence
		a.nextOutputSequence++
	}
	index := announcedIndex
	if v, has := event["output_index"]; has && v != nil {
		index = v
	}
	a.outputIndexes = append(a.outputIndexes, index)
	a.outputSequences = append(a.outputSequences, announcedSequence.(int))
	for _, key := range pendingKeys {
		a.removePending(key)
	}
	if codexMessagePhase(doneItem) == "commentary" && a.commentaryFn != nil {
		commentary := strings.TrimSpace(strings.Join(a.commentaryTextDeltas, ""))
		if commentary == "" {
			commentary = codexOutputTextOf(doneItem)
		}
		a.commentaryTextDeltas = nil
		if commentary != "" {
			return a.commentaryFn(commentary)
		}
	}
	return nil
}

func (a *codexAssembler) removePending(key string) {
	if _, ok := a.pendingFunctionCalls[key]; !ok {
		return
	}
	delete(a.pendingFunctionCalls, key)
	for i, k := range a.pendingOrder {
		if k == key {
			a.pendingOrder = append(a.pendingOrder[:i:i], a.pendingOrder[i+1:]...)
			break
		}
	}
}

func (a *codexAssembler) onTerminal(event map[string]any, eventType string) bool {
	a.sawTerminal = true
	if resp, ok := event["response"].(map[string]any); ok {
		a.terminalUsage, _ = resp["usage"].(map[string]any)
		a.terminalResponseID = stringOf(resp["id"])
		a.terminalServiceTier = resp["service_tier"]
		if status, ok := resp["status"].(string); ok {
			a.terminalStatus = status
		}
		switch eventType {
		case "response.incomplete":
			a.terminalIncomplete = resp["incomplete_details"]
		case "response.failed":
			a.terminalError = resp["error"]
		}
	}
	a.sawResponseCompleted = a.sawResponseCompleted || eventType == "response.completed"
	if a.terminalStatus == "" {
		a.terminalStatus = strings.TrimPrefix(eventType, "response.")
	}
	return true
}

// Processes one event, true on a terminal frame, an error for error frames or an aborting callback
func (a *codexAssembler) feed(event map[string]any) (bool, error) {
	eventType, _ := event["type"].(string)
	switch eventType {
	case "error":
		return false, codexStreamError(event)
	case "response.output_item.added":
		a.onItemAdded(event)
		return false, a.toolStartErr
	case "response.output_item.done":
		return false, a.onItemDone(event)
	case "response.completed", "response.incomplete", "response.failed":
		return a.onTerminal(event, eventType), nil
	case "response.refusal.delta":
		a.onRefusalDelta(event)
		return false, nil
	}
	switch {
	case strings.Contains(eventType, "output_text.delta"):
		return false, a.onTextDelta(event)
	case strings.Contains(eventType, "function_call"):
		a.onFunctionCall(event, eventType)
	case strings.Contains(eventType, "reasoning") && strings.Contains(eventType, "delta"):
		return false, a.onReasoningDelta(event)
	}
	return false, nil
}

type codexIndexedItem struct {
	index    any
	sequence int
	item     map[string]any
}

// Done items merged with settled pending calls in stream order
func (a *codexAssembler) settledOutput() []map[string]any {
	indexed := make([]codexIndexedItem, 0, len(a.outputItems)+len(a.pendingOrder))
	for i, item := range a.outputItems {
		indexed = append(indexed, codexIndexedItem{index: a.outputIndexes[i], sequence: a.outputSequences[i], item: item})
	}
	for _, key := range a.pendingOrder {
		pending := a.pendingFunctionCalls[key]
		args := strings.TrimSpace(pending.arguments)
		if args == "" {
			args = "{}"
		}
		indexed = append(indexed, codexIndexedItem{index: pending.outputIndex, sequence: pending.sequence, item: map[string]any{
			"type": "function_call", "id": pending.item["id"], "call_id": pending.item["call_id"], "name": pending.item["name"],
			"status": "completed", "arguments": args,
		}})
	}
	allIndexed, comparable := true, true
	for _, entry := range indexed {
		if entry.index == nil {
			allIndexed = false
			break
		}
		if _, ok := codexIndexFloat(entry.index); !ok {
			comparable = false
		}
	}
	if allIndexed && comparable {
		sort.SliceStable(indexed, func(i, j int) bool {
			fi, _ := codexIndexFloat(indexed[i].index)
			fj, _ := codexIndexFloat(indexed[j].index)
			return fi < fj
		})
	} else if !allIndexed {
		sort.SliceStable(indexed, func(i, j int) bool { return indexed[i].sequence < indexed[j].sequence })
	}
	out := make([]map[string]any, len(indexed))
	for i, entry := range indexed {
		out[i] = entry.item
	}
	return out
}

var errCodexNoTerminal = errors.New("Codex Responses stream did not emit a terminal response")

// Assembled Responses object, erroring when neither a terminal frame nor output arrived
func (a *codexAssembler) result() (map[string]any, error) {
	var output []map[string]any
	if a.sawResponseCompleted {
		output = a.settledOutput()
	} else {
		output = append([]map[string]any{}, a.outputItems...)
	}
	answerPresent := false
	for _, item := range output {
		phase := codexMessagePhase(item)
		if stringOf(item["type"]) == "message" && phase != "commentary" && phase != "analysis" {
			answerPresent = true
		}
	}
	if len(a.textDeltas) > 0 && ((len(output) == 0 && !a.hasToolCalls) || (a.sawTerminal && a.sawAnswerItemAdded && !answerPresent)) {
		message := map[string]any{"type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": strings.Join(a.textDeltas, "")}}}
		firstCall := len(output)
		for i, item := range output {
			if strings.Contains(stringOf(item["type"]), "function_call") {
				firstCall = i
				break
			}
		}
		output = append(output[:firstCall:firstCall], append([]map[string]any{message}, output[firstCall:]...)...)
	}
	if !a.sawTerminal && len(output) == 0 {
		return nil, errCodexNoTerminal
	}
	outList := make([]any, len(output))
	for i, item := range output {
		outList[i] = item
	}
	resp := map[string]any{"output": outList, "output_text": strings.Join(a.textDeltas, ""), "status": a.terminalStatus,
		"id": a.terminalResponseID, "model": a.model, "incomplete_details": a.terminalIncomplete, "error": a.terminalError,
		"service_tier": a.terminalServiceTier}
	if a.terminalUsage != nil {
		resp["usage"] = a.terminalUsage
	}
	return resp, nil
}

// ---- usage ----

// Nested numeric usage field as a non negative int
func codexUsageField(u map[string]any, path ...string) int {
	var cur any = u
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return 0
		}
		cur = m[key]
	}
	switch t := cur.(type) {
	case bool:
		return 0
	case float64:
		return maxInt(int(t), 0)
	case int:
		return maxInt(t, 0)
	case int64:
		return maxInt(int(t), 0)
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return maxInt(int(n), 0)
		}
	case string:
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(t), "%d", &n); err == nil {
			return maxInt(n, 0)
		}
	}
	return 0
}

func codexFirstNonzero(u map[string]any, paths ...[]string) int {
	for _, p := range paths {
		if v := codexUsageField(u, p...); v != 0 {
			return v
		}
	}
	return 0
}

// Usage of a Responses usage object with cache buckets subtracted from the input total
func codexUsageFromMap(u map[string]any) Usage {
	if u == nil {
		return Usage{}
	}
	promptTotal := codexFirstNonzero(u, []string{"input_tokens"})
	output := codexFirstNonzero(u, []string{"output_tokens"})
	cacheRead := codexFirstNonzero(u, []string{"input_tokens_details", "cached_tokens"})
	cacheWrite := codexFirstNonzero(u, []string{"input_tokens_details", "cache_write_tokens"}, []string{"input_tokens_details", "cache_creation_tokens"})
	reasoning := codexFirstNonzero(u, []string{"output_tokens_details", "reasoning_tokens"}, []string{"completion_tokens_details", "reasoning_tokens"})
	total := codexUsageField(u, "total_tokens")
	if total == 0 {
		total = promptTotal + output
	}
	return Usage{InputTokens: maxInt(0, promptTotal-cacheRead-cacheWrite), OutputTokens: output, CacheReadTokens: cacheRead,
		CacheWriteTokens: cacheWrite, ReasoningTokens: reasoning, TotalTokens: total}
}

// ---- zero event retry spillover ----

const (
	codexSpillResultSize = 100000
	codexSpillTurnBudget = 200000
	codexSpillPreview    = 1500
	codexSpillSubdir     = "cache/spillover"
	codexSpillMaxStem    = 120
	codexPersistedTag    = "<persisted-output>"
	codexPersistedClose  = "</persisted-output>"
)

var codexUnsafeFilenameRe = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// Spillover file name of a tool use id, hashed when sanitized or over the stem cap
func codexSpillFilename(toolUseID string) string {
	rawID := toolUseID
	if rawID == "" {
		rawID = "tool_result"
	}
	stem := strings.Trim(codexUnsafeFilenameRe.ReplaceAllString(rawID, "_"), "._-")
	changed := stem != rawID
	if stem == "" {
		stem = "tool_result"
	}
	if changed || len(stem) > codexSpillMaxStem {
		sum := sha256.Sum256([]byte(rawID))
		digest := hex.EncodeToString(sum[:])[:12]
		if len(stem) > codexSpillMaxStem {
			stem = stem[:codexSpillMaxStem]
		}
		stem = strings.TrimRight(stem, "._-")
		if stem == "" {
			stem = "tool_result"
		}
		stem += "_" + digest
	}
	return stem + ".txt"
}

// Preview cut at the last newline within the cap and whether more follows
func codexPreview(content string, maxChars int) (string, bool) {
	if len(content) <= maxChars {
		return content, false
	}
	if lastNL := strings.LastIndex(content[:maxChars], "\n"); lastNL > maxChars/2 {
		return content[:lastNL+1], true
	}
	return content[:maxChars], true
}

// Persisted output block with the recovery guidance the tool result storage emits
func codexPersistedMessage(preview string, hasMore bool, originalSize int, path string) string {
	sizeKB := float64(originalSize) / 1024
	sizeStr := fmt.Sprintf("%.1f KB", sizeKB)
	if sizeKB >= 1024 {
		sizeStr = fmt.Sprintf("%.1f MB", sizeKB/1024)
	}
	more := ""
	if hasMore {
		more = "\n..."
	}
	return codexPersistedTag + "\n" +
		fmt.Sprintf("This tool result was too large (%s characters, %s).\n", commaInt(originalSize), sizeStr) +
		fmt.Sprintf("Full output saved to: %s\n", path) +
		"Use the read_file tool with offset and limit to access specific sections of this output.\n" +
		"Recovery: page through the saved file with read_file (offset/limit) or " +
		"process it with execute_code — do NOT re-request the same data from the " +
		"remote API; the archive is kept while this session's transcript references " +
		"it. If it is missing, inspect the current state before retrying — the call " +
		"that produced it may already have taken effect.\n\n" +
		fmt.Sprintf("Preview (first %d chars):\n", len(preview)) +
		preview + more + "\n" + codexPersistedClose
}

// Spills an oversized tool output under the home cache and returns the preview block, the content when it fits
func codexSpillToolOutput(ctx context.Context, content, toolUseID string) string {
	if len(content) <= codexSpillResultSize {
		return content
	}
	dir := filepath.Join(home.Dir(ctx), filepath.FromSlash(codexSpillSubdir))
	filename := codexSpillFilename(toolUseID)
	preview, hasMore := codexPreview(content, codexSpillPreview)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		codexLog.Warn("Spillover write failed", "file", filename, "error", err)
		return fmt.Sprintf("%s\n\n[Truncated: tool response was %s chars. Full output could not be saved to sandbox.]", preview, commaInt(len(content)))
	}
	path := filepath.Join(dir, filename)
	data := []byte(content)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		codexLog.Warn("Spillover write failed", "file", filename, "error", err)
		return fmt.Sprintf("%s\n\n[Truncated: tool response was %s chars. Full output could not be saved to sandbox.]", preview, commaInt(len(content)))
	}
	if st, err := os.Stat(path); err != nil || st.Size() != int64(len(data)) {
		os.Remove(path)
		codexLog.Warn("Spillover write is not lossless, discarding archive", "file", filename)
		return fmt.Sprintf("%s\n\n[Truncated: tool response was %s chars. Full output could not be saved to sandbox.]", preview, commaInt(len(content)))
	}
	codexLog.Info("Persisted large tool result", "tool", "codex_zero_event_retry", "id", toolUseID, "chars", len(content), "path", path)
	return codexPersistedMessage(preview, hasMore, len(content), path)
}

// Retry payload with oversized inline function_call_output strings spilled before a zero event reconnect
func pruneZeroEventRetryPayload(ctx context.Context, kwargs map[string]any, attempt, attempts int) map[string]any {
	var items []map[string]any
	switch t := kwargs["input"].(type) {
	case []map[string]any:
		items = t
	case []any:
		items = mapsOfList(t)
		if len(items) != len(t) {
			return kwargs
		}
	default:
		return kwargs
	}
	beforeRaw, _ := json.Marshal(items)
	before := len(beforeRaw)
	if before <= codexSpillTurnBudget {
		return kwargs
	}
	pruned := 0
	prunedItems := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if item["type"] == "function_call_output" {
			if output, ok := item["output"].(string); ok {
				callID := strOrEmpty(item["call_id"])
				if callID == "" {
					callID = "call"
				}
				if replaced := codexSpillToolOutput(ctx, output, callID); replaced != output {
					cp := make(map[string]any, len(item))
					for k, v := range item {
						cp[k] = v
					}
					cp["output"] = replaced
					item = cp
					pruned++
				}
			}
		}
		prunedItems = append(prunedItems, item)
	}
	if pruned == 0 {
		codexLog.Warn(fmt.Sprintf("Codex zero-event retry (attempt %d/%d): no prunable tool output; resending payload unchanged (serialized_input_bytes=%d, model=%s)", attempt, attempts, before, strOrEmpty(kwargs["model"])))
		return kwargs
	}
	afterRaw, _ := json.Marshal(prunedItems)
	codexLog.Warn(fmt.Sprintf("Codex zero-event retry (attempt %d/%d): spilled %d oversized tool output(s) before reconnect, serialized_input_bytes=%d -> %d (model=%s)", attempt, attempts, pruned, before, len(afterRaw), strOrEmpty(kwargs["model"])))
	out := make(map[string]any, len(kwargs))
	for k, v := range kwargs {
		out[k] = v
	}
	out["input"] = prunedItems
	return out
}

// ---- watchdogs ----

// Environment knobs of the Codex stream watchdogs
const (
	CodexTTFBTimeoutEnv       = "ICEBATH_CODEX_TTFB_TIMEOUT_SECONDS"
	CodexEventStaleTimeoutEnv = "ICEBATH_CODEX_EVENT_STALE_TIMEOUT_SECONDS"
	CodexTTFBDisableAboveEnv  = "ICEBATH_CODEX_TTFB_DISABLE_ABOVE_TOKENS"
	CodexTTFBStrictEnv        = "ICEBATH_CODEX_TTFB_STRICT"
	CodexTTFBMaxEnv           = "ICEBATH_CODEX_TTFB_MAX_SECONDS"
	CodexHardTimeoutEnv       = "ICEBATH_CODEX_HARD_TIMEOUT_SECONDS"
	APICallStaleTimeoutEnv    = "ICEBATH_API_CALL_STALE_TIMEOUT"
)

// Silence floor for high effort reasoning and the first progress grace of large Codex requests
const (
	HighEffortSilenceFloorSeconds    = 300.0
	CodexFirstProgressTimeoutSeconds = 300.0
)

// Tokens one unary long decode hop may produce
const longDecodeHopTokens = 32768

// Most hops a request with this output budget can take
func longDecodeHops(maxOutputTokens any) int {
	budget := 0
	switch t := maxOutputTokens.(type) {
	case int:
		budget = t
	case int64:
		budget = int(t)
	case float64:
		if t == math.Trunc(t) {
			budget = int(t)
		}
	}
	return maxInt(1, int(math.Ceil(float64(budget)/longDecodeHopTokens)))
}

// Minimum stale timeout for openai-codex by estimated context, zero below 10k tokens
func CodexStaleTimeoutFloor(estTokens int) float64 {
	for _, tier := range [][2]float64{{100000, 1200}, {50000, 900}, {10000, 600}} {
		if float64(estTokens) > tier[0] {
			return tier[1]
		}
	}
	return 0
}

// Stale timeout raised to the codex floor then clamped to the hard ceiling
func boundCodexStaleTimeout(stale float64, estTokens int) float64 {
	if floor := CodexStaleTimeoutFloor(estTokens); floor > 0 && stale < floor {
		stale = floor
	}
	hard := envFloat(CodexHardTimeoutEnv, 1500)
	if hard > 0 && stale > hard {
		return hard
	}
	return stale
}

// Silence floor when the wire reasoning effort is high or stronger
func highEffortSilenceFloor(r reasoningCfg) float64 {
	if r.disabled {
		return 0
	}
	effort := r.effort
	if effort == "" {
		return 0
	}
	if idx := ladderIndex(effort); idx < 0 || idx < ladderIndex("high") {
		return 0
	}
	return HighEffortSilenceFloorSeconds
}

// Poll loop thresholds of one Codex Responses request
type codexWatchdogs struct {
	stale                float64
	estTokens            int
	ttfbEnabled          bool
	ttfb                 float64
	idleEnabled          bool
	idle                 float64
	idleRequiresProgress bool
	progress             float64
}

// Base non stream stale timeout and whether it is the implicit default
func (c *codexResponsesClient) staleTimeoutBase(req *Request, settings wireSettings) (float64, bool) {
	return nonStreamStaleTimeoutBase(settings, c.cc.provider, req.Model)
}

// Effective stale timeout scaled by context, floored for high effort, and capped by the context deadline
func (c *codexResponsesClient) computeStaleTimeout(ctx context.Context, req *Request, settings wireSettings, estTokens int, kwargs map[string]any) float64 {
	return computeNonStreamStaleTimeout(ctx, settings, c.cc.provider, req, c.cc.local, true, estTokens, kwargs)
}

// Base non streaming stale timeout, per model or provider config then the env then the reasoning floor then 90s
func nonStreamStaleTimeoutBase(settings wireSettings, provider, model string) (float64, bool) {
	if d, ok := configuredProviderTimeout(settings.providers, provider, model, "stale_timeout_seconds", "stale_timeout_seconds"); ok {
		return d.Seconds(), false
	}
	if _, explicit := os.LookupEnv(APICallStaleTimeoutEnv); explicit {
		return envFloat(APICallStaleTimeoutEnv, 90), false
	}
	if floor, ok := ReasoningStaleTimeoutFloor(model); ok {
		return floor.Seconds(), false
	}
	return 90, true
}

// Non streaming stale timeout scaled by context, floored for high effort codex, multiplied by long decode hops and capped by the deadline
func computeNonStreamStaleTimeout(ctx context.Context, settings wireSettings, provider string, req *Request, local, codex bool, estTokens int, payload map[string]any) float64 {
	base, implicit := nonStreamStaleTimeoutBase(settings, provider, req.Model)
	if implicit && local {
		return math.Inf(1)
	}
	timeout := base
	switch {
	case estTokens > 100000:
		timeout = math.Max(base, 240)
	case estTokens > 50000:
		timeout = math.Max(base, 150)
	}
	_, envSet := os.LookupEnv(APICallStaleTimeoutEnv)
	_, configured := configuredProviderTimeout(settings.providers, provider, req.Model, "stale_timeout_seconds", "stale_timeout_seconds")
	if explicit := envSet || configured; !explicit {
		if codex {
			timeout = math.Max(timeout, highEffortSilenceFloor(reasoningFromRequest(req.Reasoning)))
		}
		timeout *= float64(longDecodeStaleMultiplier(payload))
		timeout = capToContextDeadline(ctx, timeout)
	}
	return timeout
}

// Implicit stale timeout capped at half the remaining context deadline, never below 60s
func capToContextDeadline(ctx context.Context, timeout float64) float64 {
	deadline, ok := ctx.Deadline()
	if !ok {
		return timeout
	}
	remaining := time.Until(deadline).Seconds()
	return math.Min(timeout, math.Max(60, remaining*0.5))
}

// Stale timeout plus the TTFB, progress and idle watchdogs of a Codex request
func (c *codexResponsesClient) resolveWatchdogs(ctx context.Context, req *Request, settings wireSettings, estTokens int, kwargs map[string]any) codexWatchdogs {
	stale := c.computeStaleTimeout(ctx, req, settings, estTokens, kwargs)
	local := c.cc.local
	codexBackend := c.flags.isCodexBackend
	effortFloor := highEffortSilenceFloor(reasoningFromRequest(req.Reasoning))
	codexFloor := 0.0
	if !local {
		codexFloor = CodexStaleTimeoutFloor(estTokens)
		stale = boundCodexStaleTimeout(stale, estTokens)
	}
	idleDefault := 12.0
	switch {
	case estTokens > 100000:
		idleDefault = 180
	case estTokens > 50000:
		idleDefault = 120
	case estTokens > 10000:
		idleDefault = 60
	}
	idleDefault = math.Max(effortFloor, idleDefault)
	ttfbEnabled := true
	ttfbExplicit := envFloat(CodexTTFBTimeoutEnv, -1) != -1
	ttfb := envFloat(CodexTTFBTimeoutEnv, 120)
	if ttfb <= 0 {
		ttfbEnabled = false
	} else if !local {
		disableAbove := envFloat(CodexTTFBDisableAboveEnv, 10000)
		if !envTruthy(CodexTTFBStrictEnv) && disableAbove > 0 && float64(estTokens) >= disableAbove && ttfb < idleDefault {
			codexLog.Info(fmt.Sprintf("Scaling codex-responses no-event TTFB watchdog from %.0fs to %.0fs for large request (context=~%s tokens >= %.0f). Set %s=1 to keep the smaller cutoff.", ttfb, idleDefault, commaInt(estTokens), disableAbove, CodexTTFBStrictEnv))
			ttfb = idleDefault
		}
		if cap := envFloat(CodexTTFBMaxEnv, 0); cap > 0 && ttfb > cap {
			codexLog.Info(fmt.Sprintf("Capping codex-responses no-event TTFB timeout from %.0fs to %.0fs (context=~%s tokens) per %s.", ttfb, cap, commaInt(estTokens), CodexTTFBMaxEnv))
			ttfb = cap
		}
	} else if !ttfbExplicit {
		localCeiling := settings.localStreamStale
		if localCeiling <= 0 {
			localCeiling = 900
		}
		localCeiling = envFloat(LocalStreamStaleTimeoutEnv, localCeiling)
		if localCeiling > ttfb {
			codexLog.Info(fmt.Sprintf("Local provider detected (%s) — no-event TTFB watchdog raised from %.0fs to %.0fs (agent.local_stream_stale_timeout); set %s for an explicit cutoff.", c.route.BaseURL, ttfb, localCeiling, CodexTTFBTimeoutEnv))
			ttfb = localCeiling
		}
	}
	if ttfbEnabled && !ttfbExplicit {
		ttfb = math.Max(ttfb, effortFloor)
	}
	idleExplicit := envFloat(CodexEventStaleTimeoutEnv, -1) != -1
	idle := envFloat(CodexEventStaleTimeoutEnv, idleDefault)
	progressGated := codexBackend && codexFloor > 0 && !idleExplicit
	w := codexWatchdogs{stale: stale, estTokens: estTokens, ttfbEnabled: ttfbEnabled, ttfb: ttfb, idleEnabled: idle > 0, idle: idle,
		idleRequiresProgress: progressGated}
	if progressGated {
		w.progress = CodexFirstProgressTimeoutSeconds
	}
	return w
}

var codexSilentRejectModelRe = regexp.MustCompile(`(?:^|[/\-_])gpt-5\.5(?:$|[\-_])`)

// Actionable hint when the request matches the known Codex silent reject shape
func codexSilentHangHint(isCodexBackend bool, model string) string {
	if !isCodexBackend || !codexSilentRejectModelRe.MatchString(strings.ToLower(model)) {
		return ""
	}
	return fmt.Sprintf("Codex backend appears to be silently rejecting %s on chatgpt.com/backend-api/codex (no stream events, no error). "+
		"This is a known backend-side pattern that has affected ChatGPT Plus accounts intermittently. "+
		"Workaround: try `gpt-5.4` on the same OAuth profile, or switch to a different model/provider in your fallback chain. "+
		"Some ChatGPT Codex accounts do not support `gpt-5.4-codex`. See hermes-agent#21444 for symptom history.", pyRepr(model))
}
