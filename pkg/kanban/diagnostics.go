package kanban

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// SeverityOrder lists diagnostic severities from least to most urgent
var SeverityOrder = []string{"warning", "error", "critical"}

// Whether severity meets or exceeds threshold, true for no threshold and false for unknown names
func SeverityAtOrAbove(severity, threshold *string) bool {
	if threshold == nil {
		return true
	}
	if severity == nil || !slices.Contains(SeverityOrder, *severity) || !slices.Contains(SeverityOrder, *threshold) {
		return false
	}
	return slices.Index(SeverityOrder, *severity) >= slices.Index(SeverityOrder, *threshold)
}

// DiagnosticAction is one recovery action the dashboard renders as a button and the CLI as a hint
type DiagnosticAction struct {
	Kind      string
	Label     string
	Payload   *pyjson.Object
	Suggested bool
}

// Ordered mapping of the action fields
func (a *DiagnosticAction) ToDict() *pyjson.Object {
	payload := a.Payload
	if payload == nil {
		payload = pyjson.NewObject()
	}
	return pyjson.ObjectOf("kind", a.Kind, "label", a.Label, "payload", pyjson.Clone(payload), "suggested", a.Suggested)
}

// Diagnostic is one active distress signal on a task
type Diagnostic struct {
	Kind        string
	Severity    string
	Title       string
	Detail      string
	Actions     []*DiagnosticAction
	FirstSeenAt int64
	LastSeenAt  int64
	Count       int64
	RunID       *int64
	Data        *pyjson.Object
}

// Ordered mapping of the diagnostic fields with every action expanded
func (d *Diagnostic) ToDict() *pyjson.Object {
	actions := make([]any, 0, len(d.Actions))
	for _, a := range d.Actions {
		actions = append(actions, a.ToDict())
	}
	data := d.Data
	if data == nil {
		data = pyjson.NewObject()
	}
	return pyjson.ObjectOf("kind", d.Kind, "severity", d.Severity, "title", d.Title, "detail", d.Detail,
		"actions", actions, "first_seen_at", d.FirstSeenAt, "last_seen_at", d.LastSeenAt, "count", d.Count,
		"run_id", optIntValue(d.RunID), "data", pyjson.Clone(data))
}

// diagRuleError is a Python exception raised inside a rule, which drops that rule only
type diagRuleError struct{ msg string }

// Raises a rule level exception the rule runner absorbs
func diagFail(format string, args ...any) {
	panic(&diagRuleError{fmt.Sprintf(format, args...)})
}

// Attribute value read through a panicking accessor, false when the accessor has no such attribute
func attrOK(f func() any) (v any, ok bool) {
	defer func() {
		if recover() != nil {
			v, ok = nil, false
		}
	}()
	return f(), true
}

// Field of a row, a task, run or event, or a mapping, the default when absent
func diagField(obj any, name string, def any) any {
	var v any
	ok := false
	switch o := obj.(type) {
	case nil:
	case *Row:
		if o != nil {
			v, ok = o.Lookup(name)
		}
	case *pyjson.Object:
		if o != nil {
			v, ok = o.Get(name)
		}
	case map[string]any:
		v, ok = o[name]
	case *Task:
		if o != nil {
			v, ok = attrOK(func() any { return taskAttr(o, name) })
		}
	case *Run:
		if o != nil {
			v, ok = attrOK(func() any { return runAttr(o, name) })
		}
	case *Event:
		if o != nil {
			v, ok = attrOK(func() any { return eventAttr(o, name) })
		}
	}
	if !ok {
		return def
	}
	return v
}

// Python attribute of an event by field name
func eventAttr(e *Event, name string) any {
	switch name {
	case "id":
		return e.ID
	case "task_id":
		return e.TaskID
	case "kind":
		return e.Kind
	case "payload":
		return e.Payload
	case "created_at":
		return e.CreatedAt
	case "run_id":
		return optIntValue(e.RunID)
	}
	panic("kanban: Event has no attribute " + pyrepr.Repr(name))
}

// dict.get on a value that must be a dict, raising AttributeError otherwise
func mustDictGet(d any, key string, def any) any {
	if !isDict(d) {
		diagFail("'%s' object has no attribute 'get'", pyrepr.TypeName(d))
	}
	if v, ok := dictGet(d, key); ok {
		return v
	}
	return def
}

// Python int of a value, raising inside the rule on failure
func diagInt(v any) int64 {
	if b, ok := v.(bool); ok {
		if b {
			return 1
		}
		return 0
	}
	n, err := pyint.Of(v)
	if err != nil {
		diagFail("%s", err.Error())
	}
	if !n.IsInt64() {
		if n.Sign() < 0 {
			return math.MinInt64
		}
		return math.MaxInt64
	}
	return n.Int64()
}

// Python float of a value, raising inside the rule on failure
func diagFloat(v any) float64 {
	f, err := pyint.Float(v)
	if err != nil {
		diagFail("%s", err.Error())
	}
	return f
}

// Event payload as a dict whether stored decoded or as JSON text, empty otherwise
func parsePayload(ev any) any {
	p := diagField(ev, "payload", nil)
	if isDict(p) {
		return p
	}
	if s, ok := p.(string); ok {
		decoded, err := pyjson.Loads(s)
		if err != nil || !pyrepr.Truthy(decoded) {
			return pyjson.NewObject()
		}
		return decoded
	}
	return pyjson.NewObject()
}

// Kind of an event, empty when absent
func eventKind(ev any) string {
	v := diagField(ev, "kind", "")
	if !pyrepr.Truthy(v) {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return pyrepr.Str(v)
	}
	return s
}

// Creation epoch of an event, zero when absent
func eventTS(ev any) int64 {
	v := diagField(ev, "created_at", 0)
	if !pyrepr.Truthy(v) {
		return 0
	}
	return diagInt(v)
}

// Primary field unless it is None, else the legacy column of old rows
func firstField(task any, primary, legacy string, def any) any {
	if v := diagField(task, primary, nil); v != nil {
		return v
	}
	return diagField(task, legacy, def)
}

// Newest creation epoch over events whose kind is in kinds, zero when none
func latestEventTS(events []any, kinds ...string) int64 {
	var latest int64
	for _, ev := range events {
		if slices.Contains(kinds, eventKind(ev)) {
			latest = max(latest, eventTS(ev))
		}
	}
	return latest
}

// Whether the latest breaker trip was a terminal provider error that nothing resumed since
func latestGaveUpIsTerminalProvider(events []any) bool {
	for i := len(events) - 1; i >= 0; i-- {
		switch eventKind(events[i]) {
		case "gave_up":
			return pyrepr.Truthy(mustDictGet(parsePayload(events[i]), "terminal_provider", nil))
		case "unblocked", "promoted", "completed", "claimed":
			return false
		}
	}
	return false
}

// Action showing a CLI command
func cliHint(label, command string, suggested bool) *DiagnosticAction {
	return &DiagnosticAction{Kind: "cli_hint", Label: label, Payload: pyjson.ObjectOf("command", command), Suggested: suggested}
}

// Suggested hint to read the task's worker log
func logHintAction(taskID string) *DiagnosticAction {
	cmd := "icebath kanban log " + taskID
	return cliHint("Check logs: "+cmd, cmd, true)
}

// First 500 characters of an error with an ellipsis, empty when absent
func errorSnippet(lastErr any) string {
	if !pyrepr.Truthy(lastErr) {
		return ""
	}
	s, ok := lastErr.(string)
	if !ok {
		diagFail("'%s' object has no attribute 'strip'", pyrepr.TypeName(lastErr))
	}
	text := pystr.Strip(s)
	if text == "" {
		return ""
	}
	if pystr.Len(text) > 500 {
		return pystr.Slice(text, 0, 500) + "…"
	}
	return text
}

// First line of a snippet cut to 160 characters
func snippetHeadline(snippet string) string {
	lines := pystr.Splitlines(snippet, false)
	if len(lines) == 0 {
		diagFail("list index out of range")
	}
	return pystr.Slice(lines[0], 0, 160)
}

// Events of kind with no completed or edited event after them, in arrival order
func activeHallucinationEvents(events []any, kind string) []any {
	var active []any
	for _, ev := range events {
		switch k := eventKind(ev); {
		case k == "completed" || k == "edited":
			active = active[:0]
		case k == kind:
			active = append(active, ev)
		}
	}
	return active
}

// Ordered deduplicated payload entries under key across hits
func uniquePayloadIDs(hits []any, key string) []any {
	out := []any{}
	for _, ev := range hits {
		v := mustDictGet(parsePayload(ev), key, []any{})
		if !pyrepr.Truthy(v) {
			continue
		}
		items, ok := v.([]any)
		if !ok {
			diagFail("'%s' object is not iterable", pyrepr.TypeName(v))
		}
		for _, pid := range items {
			if !slices.ContainsFunc(out, func(x any) bool { return pyjson.Equal(x, pid) }) {
				out = append(out, pid)
			}
		}
	}
	return out
}

// Baseline reclaim and reassign actions every diagnostic falls back on
func genericRecoveryActions(running bool) []*DiagnosticAction {
	var out []*DiagnosticAction
	if running {
		out = append(out, &DiagnosticAction{Kind: "reclaim", Label: "Reclaim task", Payload: pyjson.NewObject()})
	}
	return append(out, &DiagnosticAction{Kind: "reassign", Label: "Reassign to different profile", Payload: pyjson.ObjectOf("reclaim_first", running)})
}

// Whether the task is running
func isRunningTask(task any) bool {
	return diagField(task, "status", nil) == "running"
}

// Runs by id descending, equal ids keeping the last listed run first
func runsNewestFirst(runs []any) []any {
	sorted := slices.Clone(runs)
	sort.SliceStable(sorted, func(i, j int) bool {
		return diagInt(diagField(sorted[i], "id", 0)) < diagInt(diagField(sorted[j], "id", 0))
	})
	slices.Reverse(sorted)
	return sorted
}

// Whether an aux slot was user configured, a non auto provider or any model, base_url or api_key
func auxSlotExplicit(slot any) bool {
	if !isDict(slot) {
		return false
	}
	text := func(key string) string {
		v, _ := dictGet(slot, key)
		if !pyrepr.Truthy(v) {
			return ""
		}
		return pystr.Strip(pyrepr.Str(v))
	}
	if provider := pystr.Lower(text("provider")); provider != "" && provider != "auto" {
		return true
	}
	for _, key := range []string{"model", "base_url", "api_key"} {
		if text(key) != "" {
			return true
		}
	}
	return false
}

// Whether a main model is visible in the raw config, false when unprovable
func mainModelVisible(rawConfig any) bool {
	if !isDict(rawConfig) {
		return false
	}
	modelCfg, _ := dictGet(rawConfig, "model")
	strOf := func(v any) string {
		if !pyrepr.Truthy(v) {
			return ""
		}
		return pystr.Strip(pyrepr.Str(v))
	}
	if isDict(modelCfg) {
		provider, _ := dictGet(modelCfg, "provider")
		var model any
		for _, key := range []string{"default", "model", "name"} {
			if v, _ := dictGet(modelCfg, key); pyrepr.Truthy(v) {
				model = v
				break
			}
		}
		return strOf(provider) != "" && strOf(model) != ""
	}
	return strOf(modelCfg) != ""
}

// Whether the triage aux paths look configured, nil when no config context is present
func TriageAuxStatus(config any) any {
	if !isDict(config) {
		return nil
	}
	if explicit, _ := dictGet(config, "triage_aux_status"); isDict(explicit) {
		return explicit
	}
	aux, _ := dictGet(config, "auxiliary")
	kanbanCfg, _ := dictGet(config, "kanban")
	if !isDict(kanbanCfg) {
		kanbanCfg = map[string]any{}
	}
	_, hasModel := dictGet(config, "model")
	if !isDict(aux) && !pyrepr.Truthy(kanbanCfg) && !hasModel {
		return nil
	}
	if !isDict(aux) {
		aux = map[string]any{}
	}
	autoDecompose := true
	if v, ok := dictGet(kanbanCfg, "auto_decompose"); ok {
		autoDecompose = pyrepr.Truthy(v)
	}
	decomposer, _ := dictGet(aux, "kanban_decomposer")
	specifier, _ := dictGet(aux, "triage_specifier")
	return pyjson.ObjectOf("auto_decompose", autoDecompose, "decomposer_explicit", auxSlotExplicit(decomposer),
		"specifier_explicit", auxSlotExplicit(specifier), "main_model_visible", mainModelVisible(config))
}

// Python int of a value at or above one, the default otherwise
func diagPositiveInt(value any, def int64) int64 {
	if _, isBool := value.(bool); !isBool {
		if n, err := pyint.Of(value); err == nil {
			if n.Sign() >= 1 {
				if n.IsInt64() {
					return n.Int64()
				}
				return math.MaxInt64
			}
			return def
		}
		return def
	}
	if value.(bool) {
		return 1
	}
	return def
}

type diagRule func(task any, events, runs []any, now int64, cfg map[string]any) []*Diagnostic

// Completion blocked for created cards that do not exist, cleared by a later completion or edit
func ruleHallucinatedCards(task any, events, runs []any, now int64, cfg map[string]any) []*Diagnostic {
	hits := activeHallucinationEvents(events, "completion_blocked_hallucination")
	if len(hits) == 0 {
		return nil
	}
	actions := append([]*DiagnosticAction{{Kind: "comment", Label: "Add a comment explaining what to do", Payload: pyjson.NewObject()}},
		genericRecoveryActions(isRunningTask(task))...)
	return []*Diagnostic{{
		Kind: "hallucinated_cards", Severity: "error",
		Title: "Worker claimed cards that don't exist",
		Detail: "The completing worker declared created_cards that either didn't exist or weren't " +
			"created by its profile. The completion was blocked and the task stayed in its prior " +
			"state. Usually means the worker hallucinated ids instead of capturing return values " +
			"from kanban_create.",
		Actions:     actions,
		FirstSeenAt: eventTS(hits[0]), LastSeenAt: eventTS(hits[len(hits)-1]), Count: int64(len(hits)),
		Data: pyjson.ObjectOf("phantom_ids", uniquePayloadIDs(hits, "phantom_cards")),
	}}
}

// triageSlot is the primary and fallback aux slot pair of one auto decompose setting
type triageSlot struct{ primary, fallback, desc, detailPath string }

var triageSlots = map[bool]triageSlot{
	true: {"auxiliary.kanban_decomposer", "auxiliary.triage_specifier", "decomposer",
		"Auto-decompose is on, so the dispatcher needs auxiliary.kanban_decomposer (with " +
			"auxiliary.triage_specifier as a fallback for non-fan-out tasks)."},
	false: {"auxiliary.triage_specifier", "auxiliary.kanban_decomposer", "specifier",
		"Auto-decompose is off, so triage tasks need " +
			"`icebath kanban specify`, which uses auxiliary.triage_specifier."},
}

// Triage task with no usable aux model on its primary slot and no main model to fall back to
func ruleTriageAuxUnavailable(task any, events, runs []any, now int64, cfg map[string]any) []*Diagnostic {
	if diagField(task, "status", nil) != "triage" {
		return nil
	}
	status := TriageAuxStatus(cfg)
	if status == nil {
		return nil
	}
	get := func(key string) bool { return pyrepr.Truthy(mustDictGet(status, key, nil)) }
	autoDecompose, mainVisible := get("auto_decompose"), get("main_model_visible")
	decomposerExplicit, specifierExplicit := get("decomposer_explicit"), get("specifier_explicit")
	slot := triageSlots[autoDecompose]
	primaryExplicit, fallbackExplicit := specifierExplicit, decomposerExplicit
	if autoDecompose {
		primaryExplicit, fallbackExplicit = decomposerExplicit, specifierExplicit
	}
	if primaryExplicit || mainVisible {
		return nil
	}
	taskIDValue := diagField(task, "id", nil)
	taskID := "<task_id>"
	if pyrepr.Truthy(taskIDValue) {
		taskID = pyrepr.Str(taskIDValue)
	}
	actions := []*DiagnosticAction{cliHint("Configure "+slot.primary, "icebath config set "+slot.primary+".provider auto", true)}
	if !fallbackExplicit && !mainVisible {
		actions = append(actions, cliHint("Or configure fallback "+slot.fallback, "icebath config set "+slot.fallback+".provider auto", false))
	}
	if !autoDecompose {
		cmd := "icebath kanban specify " + taskID
		actions = append(actions, cliHint("Specify manually: "+cmd, cmd, false))
	}
	var dataTaskID any = "<task_id>"
	if pyrepr.Truthy(taskIDValue) {
		dataTaskID = taskIDValue
	}
	return []*Diagnostic{{
		Kind: "triage_aux_unavailable", Severity: "warning",
		Title: "Triage " + slot.desc + " has no usable model",
		Detail: "This task is still in triage and no working auxiliary model is visible to the " +
			"dispatcher. " + slot.detailPath + " The default slot uses `provider: auto` which falls back to " +
			"the main model, but no main model is configured either. Configure the slot directly " +
			"or set a main model so the auto fallback can take over.",
		Actions:     actions,
		FirstSeenAt: now, LastSeenAt: now, Count: 1,
		Data: pyjson.ObjectOf("task_id", dataTaskID, "auto_decompose", autoDecompose,
			"primary_slot", slot.primary, "main_model_visible", mainVisible),
	}}
}

// Advisory for completion summaries naming task ids that do not resolve
func ruleProsePhantomRefs(task any, events, runs []any, now int64, cfg map[string]any) []*Diagnostic {
	hits := activeHallucinationEvents(events, "suspected_hallucinated_references")
	if len(hits) == 0 {
		return nil
	}
	return []*Diagnostic{{
		Kind: "prose_phantom_refs", Severity: "warning",
		Title: "Completion summary references unknown task ids",
		Detail: "The completion summary mentions task ids that don't resolve in this board's database. " +
			"The completion itself succeeded, but downstream consumers parsing the summary may be " +
			"pointed at cards that never existed.",
		Actions:     genericRecoveryActions(isRunningTask(task)),
		FirstSeenAt: eventTS(hits[0]), LastSeenAt: eventTS(hits[len(hits)-1]), Count: int64(len(hits)),
		Data: pyjson.ObjectOf("phantom_refs", uniquePayloadIDs(hits, "phantom_refs")),
	}}
}

// failure_threshold with the legacy spawn_failure_threshold alias
func failureThreshold(cfg map[string]any) any {
	if v, ok := cfg["failure_threshold"]; ok {
		return v
	}
	if v, ok := cfg["spawn_failure_threshold"]; ok {
		return v
	}
	return 3
}

var outcomeLabels = map[string]string{"spawn_failed": "spawn", "timed_out": "timeout", "crashed": "crash"}

// Python less than between a failure count and an integer, raising for unorderable values
func diagLess(v any, threshold int64) bool {
	if _, isStr := v.(string); isStr {
		diagFail("'<' not supported between instances of 'str' and 'int'")
	}
	if f, ok := v.(float64); ok {
		return f < float64(threshold)
	}
	return diagInt(v) < threshold
}

// Consecutive failures at or past the threshold, or a terminal provider trip after one attempt
func ruleRepeatedFailures(task any, events, runs []any, now int64, cfg map[string]any) []*Diagnostic {
	switch diagField(task, "status", nil) {
	case "done", "archived", "running":
		return nil
	}
	threshold := diagPositiveInt(failureThreshold(cfg), 3)
	failureLimit := diagPositiveInt(cfg["failure_limit"], threshold)
	failuresValue := firstField(task, "consecutive_failures", "spawn_failures", 0)
	terminalTrip := latestGaveUpIsTerminalProvider(events)
	if !terminalTrip && (failuresValue == nil || diagLess(failuresValue, threshold)) {
		return nil
	}
	if !pyrepr.Truthy(failuresValue) {
		failuresValue = int64(0)
	}
	failures := diagInt(failuresValue)
	lastErr := firstField(task, "last_failure_error", "last_spawn_error", nil)
	assignee := diagField(task, "assignee", nil)
	var mostRecentOutcome any
	for _, r := range runsNewestFirst(runs) {
		if oc, ok := diagField(r, "outcome", nil).(string); ok && (oc == "spawn_failed" || oc == "timed_out" || oc == "crashed") {
			mostRecentOutcome = oc
			break
		}
	}
	var actions []*DiagnosticAction
	if mostRecentOutcome == "spawn_failed" && pyrepr.Truthy(assignee) && assignee != "default" {
		who := pyrepr.Str(assignee)
		doctor, auth := "icebath -p "+who+" doctor", "icebath -p "+who+" auth"
		actions = append(actions, cliHint("Verify profile: "+doctor, doctor, true), cliHint("Fix profile auth: "+auth, auth, false))
	} else if mostRecentOutcome == "timed_out" || mostRecentOutcome == "crashed" {
		if taskID := diagField(task, "id", nil); pyrepr.Truthy(taskID) {
			actions = append(actions, logHintAction(pyrepr.Str(taskID)))
		}
	}
	actions = append(actions, genericRecoveryActions(isRunningTask(task))...)
	severity := "error"
	if failures >= threshold*2 {
		severity = "critical"
	}
	errSnippet := errorSnippet(lastErr)
	outcomeKey, _ := mostRecentOutcome.(string)
	outcomeLabel, ok := outcomeLabels[outcomeKey]
	if !ok {
		outcomeLabel = "failure"
	}
	var title, detail string
	switch {
	case terminalTrip:
		title = "Provider rejected this profile's credential or model — blocked after one attempt"
		detail = fmt.Sprintf("The worker's provider call failed with an error a retry cannot fix (revoked or invalid "+
			"API key, model not found), so the dispatcher blocked the task instead of spending the "+
			"%d-attempt retry budget on it. Full last error:\n\n%s\n\n"+
			"Fix the assignee profile's provider credentials/model, then unblock the task.", failureLimit, errSnippet)
	case errSnippet != "":
		title = fmt.Sprintf("Agent %s x%d: %s", outcomeLabel, failures, snippetHeadline(errSnippet))
		detail = fmt.Sprintf("This task has failed %d times in a row (most recent: %s). Full "+
			"last error:\n\n%s\n\nThe dispatcher circuit breaker is configured for "+
			"%d consecutive non-success attempts. Fix the root cause and reclaim or "+
			"unblock the task to retry.", failures, outcomeLabel, errSnippet, failureLimit)
	default:
		title = fmt.Sprintf("Agent %s x%d (no error recorded)", outcomeLabel, failures)
		detail = fmt.Sprintf("This task has failed %d times in a row (most recent: %s) but no "+
			"error text was captured. Check the suggested command or the worker log.", failures, outcomeLabel)
	}
	return []*Diagnostic{{
		Kind: "repeated_failures", Severity: severity, Title: title, Detail: detail, Actions: actions,
		FirstSeenAt: now, LastSeenAt: now, Count: failures,
		Data: pyjson.ObjectOf("consecutive_failures", failures, "most_recent_outcome", mostRecentOutcome,
			"last_error", lastErr, "failure_threshold", threshold, "failure_limit", failureLimit),
	}}
}

// Trailing crashed run outcomes at or past the crash threshold, deferring to the unified rule
func ruleRepeatedCrashes(task any, events, runs []any, now int64, cfg map[string]any) []*Diagnostic {
	switch diagField(task, "status", nil) {
	case "done", "archived", "running":
		return nil
	}
	current := diagField(task, "consecutive_failures", 0)
	if !pyrepr.Truthy(current) {
		current = int64(0)
	}
	if !diagLess(current, diagInt(failureThreshold(cfg))) {
		return nil
	}
	var crashThreshold any = 2
	if v, ok := cfg["crash_threshold"]; ok {
		crashThreshold = v
	}
	threshold := diagInt(crashThreshold)
	var consecutive int64
	var lastErr any
	for _, r := range runsNewestFirst(runs) {
		outcome := diagField(r, "outcome", nil)
		if outcome == "crashed" {
			consecutive++
			if lastErr == nil {
				lastErr = diagField(r, "error", nil)
			}
		} else if outcome == "completed" || outcome == "reclaimed" {
			break
		}
	}
	if consecutive < threshold {
		return nil
	}
	var actions []*DiagnosticAction
	if taskID := diagField(task, "id", nil); pyrepr.Truthy(taskID) {
		actions = append(actions, logHintAction(pyrepr.Str(taskID)))
	}
	actions = append(actions, genericRecoveryActions(isRunningTask(task))...)
	severity := "error"
	if consecutive >= threshold*2 {
		severity = "critical"
	}
	errSnippet := errorSnippet(lastErr)
	var title, detail string
	if errSnippet != "" {
		title = fmt.Sprintf("Agent crashed %dx: %s", consecutive, snippetHeadline(errSnippet))
		detail = fmt.Sprintf("The last %d runs ended with outcome=crashed. Full last error:\n\n%s", consecutive, errSnippet)
	} else {
		title = fmt.Sprintf("Agent crashed %dx (no error recorded)", consecutive)
		detail = fmt.Sprintf("The last %d runs ended with outcome=crashed but "+
			"no error text was captured. Check the worker log for more.", consecutive)
	}
	return []*Diagnostic{{
		Kind: "repeated_crashes", Severity: severity, Title: title, Detail: detail, Actions: actions,
		FirstSeenAt: now, LastSeenAt: now, Count: consecutive,
		Data: pyjson.ObjectOf("consecutive_crashes", consecutive, "last_error", lastErr),
	}}
}

// List of a graph key, empty when absent or falsy
func graphList(graph any, key string) []any {
	v, _ := dictGet(graph, key)
	if !pyrepr.Truthy(v) {
		return nil
	}
	items, ok := v.([]any)
	if !ok {
		diagFail("'%s' object is not iterable", pyrepr.TypeName(v))
	}
	return items
}

// Review required sticky block whose waiting children depend on the blocked implementation
func ruleReviewDependencyDeadlock(task any, events, runs []any, now int64, cfg map[string]any) []*Diagnostic {
	if diagField(task, "status", nil) != "blocked" {
		return nil
	}
	var latestBlock any
	for i := len(events) - 1; i >= 0; i-- {
		if eventKind(events[i]) == "blocked" {
			latestBlock = events[i]
			break
		}
	}
	if latestBlock == nil {
		return nil
	}
	reasonValue := mustDictGet(parsePayload(latestBlock), "reason", nil)
	if !pyrepr.Truthy(reasonValue) {
		reasonValue = ""
	}
	reason := pystr.Strip(pyrepr.Str(reasonValue))
	if !strings.HasPrefix(pystr.Lower(reason), "review-required:") {
		return nil
	}
	graph := cfg["_graph"]
	if !isDict(graph) {
		return nil
	}
	var waiting []any
	for _, child := range graphList(graph, "children") {
		if st, _ := dictGet(child, "status"); isDict(child) && st == "todo" {
			waiting = append(waiting, child)
		}
	}
	if len(waiting) == 0 {
		return nil
	}
	taskID := ""
	if v := diagField(task, "id", nil); pyrepr.Truthy(v) {
		taskID = pyrepr.Str(v)
	}
	childIDs := []any{}
	for _, child := range waiting {
		if id, _ := dictGet(child, "id"); pyrepr.Truthy(id) {
			childIDs = append(childIDs, pyrepr.Str(id))
		}
	}
	var actions []*DiagnosticAction
	if taskID != "" {
		actions = append(actions, cliHint("Complete the finished implementation phase", "icebath kanban complete "+taskID, true))
	}
	if taskID != "" && len(childIDs) > 0 {
		actions = append(actions, cliHint("Or unlink the incorrectly gated reviewer", fmt.Sprintf("icebath kanban unlink %s %s", taskID, childIDs[0]), false))
	}
	blockedAt := eventTS(latestBlock)
	if blockedAt == 0 {
		blockedAt = now
	}
	return []*Diagnostic{{
		Kind: "review_dependency_deadlock", Severity: "error",
		Title: fmt.Sprintf("Review handoff blocks %d dependent task(s)", len(childIDs)),
		Detail: "This implementation is sticky-blocked for review while its downstream task(s) require " +
			"the implementation to be done or archived before they can run. Complete the finished " +
			"phase, unlink the incorrect dependency, or migrate this workflow to the first-class " +
			"review lifecycle.",
		Actions:     actions,
		FirstSeenAt: blockedAt, LastSeenAt: blockedAt, Count: int64(len(childIDs)),
		Data: pyjson.ObjectOf("blocked_parent_id", taskID, "waiting_child_ids", childIDs, "block_reason", reason),
	}}
}

// Running card with a direct parent that is not done or archived
func ruleRunningWithOpenParents(task any, events, runs []any, now int64, cfg map[string]any) []*Diagnostic {
	if diagField(task, "status", nil) != "running" {
		return nil
	}
	graph := cfg["_graph"]
	if !isDict(graph) {
		return nil
	}
	var open []any
	for _, parent := range graphList(graph, "parents") {
		if !isDict(parent) {
			continue
		}
		id, _ := dictGet(parent, "id")
		st, _ := dictGet(parent, "status")
		if pyrepr.Truthy(id) && st != "done" && st != "archived" {
			open = append(open, parent)
		}
	}
	if len(open) == 0 {
		return nil
	}
	taskID := ""
	if v := diagField(task, "id", nil); pyrepr.Truthy(v) {
		taskID = pyrepr.Str(v)
	}
	parentIDs := make([]string, 0, len(open))
	openParents := make([]any, 0, len(open))
	for _, p := range open {
		id, _ := dictGet(p, "id")
		st, _ := dictGet(p, "status")
		parentIDs = append(parentIDs, pyrepr.Str(id))
		openParents = append(openParents, pyjson.ObjectOf("id", id, "status", st))
	}
	seenAt := int64(0)
	if v := diagField(task, "started_at", 0); pyrepr.Truthy(v) {
		seenAt = diagInt(v)
	}
	if seenAt == 0 {
		seenAt = now
	}
	return []*Diagnostic{{
		Kind: "running_with_open_parents", Severity: "warning",
		Title: fmt.Sprintf("Running while %d parent(s) are not done", len(parentIDs)),
		Detail: "This card is running concurrently with a parent it declares a dependency on, so the " +
			"parent's work is not serialised ahead of it and completion will be refused until every " +
			"parent is done or archived. Finish the parent, or unlink the edge if it was never meant " +
			"to gate this run.",
		Actions:     []*DiagnosticAction{cliHint("Unlink the parent that should not gate this run", fmt.Sprintf("icebath kanban unlink %s %s", parentIDs[0], taskID), false)},
		FirstSeenAt: seenAt, LastSeenAt: now, Count: int64(len(parentIDs)),
		Data: pyjson.ObjectOf("open_parents", openParents),
	}}
}

// Config value of key, the default when absent
func cfgOr(cfg map[string]any, key string, def any) any {
	if v, ok := cfg[key]; ok {
		return v
	}
	return def
}

// Blocked past the stale hours with no comment or unblock since the last block
func ruleStuckInBlocked(task any, events, runs []any, now int64, cfg map[string]any) []*Diagnostic {
	hours := diagFloat(cfgOr(cfg, "blocked_stale_hours", 24))
	if diagField(task, "status", nil) != "blocked" {
		return nil
	}
	lastBlocked := latestEventTS(events, "blocked")
	if lastBlocked == 0 {
		return nil
	}
	ageHours := float64(now-lastBlocked) / 3600.0
	if ageHours < hours {
		return nil
	}
	for _, ev := range events {
		if k := eventKind(ev); (k == "commented" || k == "unblocked") && eventTS(ev) > lastBlocked {
			return nil
		}
	}
	return []*Diagnostic{{
		Kind: "stuck_in_blocked", Severity: "warning",
		Title: fmt.Sprintf("Task has been blocked for %dh", int64(ageHours)),
		Detail: fmt.Sprintf("This task transitioned to blocked %dh ago and has had no comments or "+
			"unblock attempts since. Blocked tasks are waiting for human input — check the block "+
			"reason and either unblock with feedback or answer with a comment.", int64(ageHours)),
		Actions:     []*DiagnosticAction{{Kind: "comment", Label: "Add a comment / unblock the task", Payload: pyjson.NewObject(), Suggested: true}},
		FirstSeenAt: lastBlocked, LastSeenAt: lastBlocked, Count: 1,
		Data: pyjson.ObjectOf("blocked_at", lastBlocked, "age_hours", pyint.Round(ageHours, 1)),
	}}
}

// Blocked after unblocked cycles at or past the threshold within the sliding window
func ruleBlockUnblockCycling(task any, events, runs []any, now int64, cfg map[string]any) []*Diagnostic {
	threshold := diagPositiveInt(cfg["block_cycle_threshold"], 3)
	windowSeconds := diagFloat(cfgOr(cfg, "block_cycle_window_seconds", 24*3600))
	cutoff := float64(now) - windowSeconds
	var cycles, initialBlocked, lastCycleBlocked int64
	seenUnblock := false
	for _, ev := range events {
		ts := eventTS(ev)
		if float64(ts) < cutoff {
			continue
		}
		switch eventKind(ev) {
		case "blocked":
			if initialBlocked == 0 {
				initialBlocked = ts
			}
			if seenUnblock {
				cycles++
				lastCycleBlocked = ts
				seenUnblock = false
			}
		case "unblocked":
			seenUnblock = true
		}
	}
	if cycles < threshold {
		return nil
	}
	var actions []*DiagnosticAction
	if taskID := diagField(task, "id", nil); pyrepr.Truthy(taskID) {
		cmd := "icebath kanban events " + pyrepr.Str(taskID)
		actions = append(actions, cliHint("Check block reasons: "+cmd, cmd, true))
	}
	first, last := initialBlocked, lastCycleBlocked
	if first == 0 {
		first = now
	}
	if last == 0 {
		last = now
	}
	return []*Diagnostic{{
		Kind: "block_unblock_cycling", Severity: "warning",
		Title: fmt.Sprintf("Task block→unblock cycled %dx in %dh", cycles, int64(windowSeconds/3600)),
		Detail: fmt.Sprintf("This task has been blocked %d times after being unblocked, suggesting the "+
			"unblock is not addressing the root cause and the worker keeps hitting the same wall. "+
			"Review the block reasons in the event history; a different intervention (reassign, "+
			"change scope, archive) may be needed.", cycles),
		Actions:     actions,
		FirstSeenAt: first, LastSeenAt: last, Count: cycles,
		Data: pyjson.ObjectOf("cycles", cycles, "window_seconds", int64(windowSeconds)),
	}}
}

// Age text in hours past one hour, else whole minutes
func ageText(seconds float64) string {
	if seconds >= 3600 {
		return fmt.Sprintf("%.1fh", seconds/3600)
	}
	return fmt.Sprintf("%dm", int64(seconds/60))
}

// Severity escalating with age past two and six times the threshold
func ageSeverity(age, threshold float64) string {
	switch {
	case age >= threshold*6:
		return "critical"
	case age >= threshold*2:
		return "error"
	}
	return "warning"
}

// Assigned unclaimed ready task left waiting past the stranded threshold
func ruleStrandedInReady(task any, events, runs []any, now int64, cfg map[string]any) []*Diagnostic {
	thresholdSeconds := diagFloat(cfgOr(cfg, "stranded_threshold_seconds", 30*60))
	if diagField(task, "status", nil) != "ready" {
		return nil
	}
	if pyrepr.Truthy(diagField(task, "claim_lock", nil)) {
		return nil
	}
	assigneeValue := diagField(task, "assignee", nil)
	if !pyrepr.Truthy(assigneeValue) {
		assigneeValue = ""
	}
	assignee, ok := assigneeValue.(string)
	if !ok {
		diagFail("'%s' object has no attribute 'strip'", pyrepr.TypeName(assigneeValue))
	}
	if pystr.Strip(assignee) == "" {
		return nil
	}
	lastReady := latestEventTS(events, "created", "promoted", "reclaimed", "unblocked")
	if lastReady == 0 {
		if v := diagField(task, "created_at", 0); pyrepr.Truthy(v) {
			lastReady = diagInt(v)
		}
	}
	if lastReady == 0 {
		return nil
	}
	age := float64(now - lastReady)
	if age < thresholdSeconds {
		return nil
	}
	ageStr := ageText(age)
	return []*Diagnostic{{
		Kind: "stranded_in_ready", Severity: ageSeverity(age, thresholdSeconds),
		Title: fmt.Sprintf("Ready for %s with no worker", ageStr),
		Detail: fmt.Sprintf("This task has been ready for %s but nothing has claimed it. Common causes: "+
			"assignee %s is misspelled, the profile was deleted, or the external worker "+
			"pool for this lane is down. Confirm the assignee is correct and that a worker is "+
			"actually polling for it.", ageStr, pyrepr.Repr(assignee)),
		Actions: []*DiagnosticAction{
			{Kind: "reassign", Label: "Reassign to a different worker", Payload: pyjson.ObjectOf("current_assignee", assignee)},
			cliHint("Check dispatcher status", "icebath kanban diagnostics", false),
		},
		FirstSeenAt: lastReady, LastSeenAt: lastReady, Count: 1,
		Data: pyjson.ObjectOf("ready_since", lastReady, "age_seconds", int64(age), "assignee", assignee,
			"threshold_seconds", int64(thresholdSeconds)),
	}}
}

// Running card whose claim expired past the grace window with no live recorded worker
func ruleStaleRunning(task any, events, runs []any, now int64, cfg map[string]any) []*Diagnostic {
	grace := diagFloat(cfgOr(cfg, "stale_running_grace_seconds", 60*60))
	if diagField(task, "status", nil) != "running" {
		return nil
	}
	expires := toEpoch(diagField(task, "claim_expires", nil))
	if expires == nil || float64(now-*expires) < grace {
		return nil
	}
	lockValue := diagField(task, "claim_lock", "")
	lock := ""
	if pyrepr.Truthy(lockValue) {
		lock = pyrepr.Str(lockValue)
	}
	if !strings.HasPrefix(lock, hostPrefix()) {
		return nil
	}
	hb := toEpoch(diagField(task, "last_heartbeat_at", nil))
	if hb != nil && now-*hb <= DefaultClaimHeartbeatMaxStaleSeconds {
		return nil
	}
	type recordedPid struct{ pid, startedAt any }
	recorded := []recordedPid{{diagField(task, "worker_pid", nil), diagField(task, "worker_started_at", nil)}}
	runID := diagField(task, "current_run_id", nil)
	for _, ev := range events {
		if runID != nil && !pyjson.Equal(diagField(ev, "run_id", nil), runID) {
			continue
		}
		if k := eventKind(ev); k == "spawned" || k == "worker_bound" {
			p := parsePayload(ev)
			recorded = append(recorded, recordedPid{mustDictGet(p, "pid", nil), mustDictGet(p, "started_at", nil)})
		}
	}
	pidSet := map[int64]bool{}
	for _, r := range recorded {
		if pyrepr.Truthy(r.pid) {
			pidSet[diagInt(r.pid)] = true
		}
	}
	pids := make([]int64, 0, len(pidSet))
	for p := range pidSet {
		pids = append(pids, p)
	}
	slices.Sort(pids)
	for _, r := range recorded {
		if pyrepr.Truthy(r.pid) && workerAlive(int(diagInt(r.pid)), r.startedAt) {
			return nil
		}
	}
	expired := float64(now - *expires)
	ageStr := ageText(expired)
	taskID := ""
	if v := diagField(task, "id", nil); pyrepr.Truthy(v) {
		taskID = pyrepr.Str(v)
	}
	pidTexts := make([]string, 0, len(pids))
	pidList := make([]any, 0, len(pids))
	for _, p := range pids {
		pidTexts = append(pidTexts, fmt.Sprint(p))
		pidList = append(pidList, p)
	}
	pidJoined := strings.Join(pidTexts, ", ")
	if pidJoined == "" {
		pidJoined = "none"
	}
	return []*Diagnostic{{
		Kind: "stale_running", Severity: ageSeverity(expired, grace),
		Title: fmt.Sprintf("Running %s past its expired claim with no live worker", ageStr),
		Detail: fmt.Sprintf("Nothing is executing this card and nothing has reclaimed it: the claim expired "+
			"%s ago, no heartbeat arrived inside the last hour, and no pid recorded for "+
			"the run (%s) is alive. The dispatcher "+
			"declines a reclaim when the run's owner receipt never resolved, so the card can "+
			"hold its profile's worker slot indefinitely while every card queued behind it "+
			"stays deferred. Check the card's log, then reclaim it.", ageStr, pidJoined),
		Actions: []*DiagnosticAction{
			{Kind: "reclaim", Label: "Reclaim the dead claim", Payload: pyjson.NewObject(), Suggested: true},
			cliHint("Reclaim from the CLI", "icebath kanban reclaim "+taskID, false),
		},
		FirstSeenAt: *expires, LastSeenAt: *expires, Count: 1,
		Data: pyjson.ObjectOf("claim_expires", *expires, "expired_seconds", int64(expired), "recorded_pids", pidList,
			"last_heartbeat_at", optIntValue(hb), "grace_seconds", int64(grace)),
	}}
}

var diagRules = []diagRule{
	ruleHallucinatedCards,
	ruleTriageAuxUnavailable,
	ruleProsePhantomRefs,
	ruleRepeatedFailures,
	ruleRepeatedCrashes,
	ruleReviewDependencyDeadlock,
	ruleRunningWithOpenParents,
	ruleStuckInBlocked,
	ruleBlockUnblockCycling,
	ruleStrandedInReady,
	ruleStaleRunning,
}

// DiagnosticsDefaultConfig is the rule configuration every computation starts from
func DiagnosticsDefaultConfig() map[string]any {
	return map[string]any{
		"failure_threshold":           int64(2),
		"spawn_failure_threshold":     int64(2),
		"crash_threshold":             int64(2),
		"blocked_stale_hours":         int64(24),
		"stranded_threshold_seconds":  int64(30 * 60),
		"stale_running_grace_seconds": int64(60 * 60),
	}
}

// Whether a config names failure_threshold or its legacy alias
func hasExplicitThreshold(cfg map[string]any) bool {
	_, a := cfg["failure_threshold"]
	_, b := cfg["spawn_failure_threshold"]
	return a || b
}

// Mapping of a config value as a plain dict, nil when it is not a dict
func dictOf(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case *pyjson.Object:
		if m == nil {
			return nil
		}
		out := make(map[string]any, m.Len())
		for _, k := range m.Keys {
			out[k], _ = m.Get(k)
		}
		return out
	}
	return nil
}

// Python dict of a value, raising the ValueError or TypeError dict raises for non mappings
func pyDictOf(v any) (map[string]any, error) {
	if m := dictOf(v); m != nil {
		return m, nil
	}
	switch x := v.(type) {
	case string:
		return nil, &ValueError{fmt.Sprintf("dictionary update sequence element #0 has length %d; 2 is required", pystr.Len(pystr.Slice(x, 0, 1)))}
	case []any:
		out := map[string]any{}
		for i, item := range x {
			pair, ok := item.([]any)
			if !ok {
				return nil, &TypeError{fmt.Sprintf("cannot convert dictionary update sequence element #%d to a sequence", i)}
			}
			if len(pair) != 2 {
				return nil, &ValueError{fmt.Sprintf("dictionary update sequence element #%d has length %d; 2 is required", i, len(pair))}
			}
			out[pyrepr.Str(pair[0])] = pair[1]
		}
		return out, nil
	}
	return nil, &TypeError{fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(v))}
}

// Diagnostics config from the kanban section, the threshold following kanban.failure_limit unless overridden
func ConfigFromKanbanConfig(kanbanCfg map[string]any) (map[string]any, error) {
	if kanbanCfg == nil {
		kanbanCfg = map[string]any{}
	}
	diagCfg := map[string]any{}
	if raw := kanbanCfg["diagnostics"]; pyrepr.Truthy(raw) {
		m, err := pyDictOf(raw)
		if err != nil {
			return nil, err
		}
		for k, v := range m {
			diagCfg[k] = v
		}
	}
	if _, ok := diagCfg["failure_limit"]; !ok {
		diagCfg["failure_limit"] = cfgOr(kanbanCfg, "failure_limit", DiagnosticsDefaultConfig()["failure_threshold"])
	}
	if !hasExplicitThreshold(diagCfg) {
		diagCfg["failure_threshold"] = diagCfg["failure_limit"]
	}
	return diagCfg, nil
}

// Diagnostics config from the full runtime config, carrying kanban, auxiliary and model for the triage rules
func ConfigFromRuntimeConfig(rawConfig any) (map[string]any, error) {
	if !pyrepr.Truthy(rawConfig) {
		rawConfig = map[string]any{}
	}
	raw := dictOf(rawConfig)
	if raw == nil {
		return map[string]any{}, nil
	}
	cfg := map[string]any{}
	if kanbanCfg := dictOf(raw["kanban"]); kanbanCfg != nil {
		folded, err := ConfigFromKanbanConfig(kanbanCfg)
		if err != nil {
			return nil, err
		}
		for k, v := range folded {
			cfg[k] = v
		}
		cfg["kanban"] = raw["kanban"]
	}
	for _, key := range []string{"auxiliary", "model"} {
		if v, ok := raw[key]; ok && v != nil {
			cfg[key] = v
		}
	}
	return cfg, nil
}

// Every rule for one task ordered critical first, ties broken by the most recent last seen
func ComputeTaskDiagnostics(task any, events, runs []any, now *int64, config map[string]any, graph any) []*Diagnostic {
	nowTS := time.Now().Unix()
	if now != nil {
		nowTS = *now
	}
	if config == nil {
		config = map[string]any{}
	}
	cfg := DiagnosticsDefaultConfig()
	for k, v := range config {
		cfg[k] = v
	}
	if graph != nil {
		cfg["_graph"] = graph
	}
	if _, ok := config["failure_limit"]; ok && !hasExplicitThreshold(config) {
		cfg["failure_threshold"] = diagPositiveInt(config["failure_limit"], 2)
	}
	var out []*Diagnostic
	for _, rule := range diagRules {
		func() {
			defer func() { _ = recover() }()
			out = append(out, rule(task, events, runs, nowTS, cfg)...)
		}()
	}
	severityIdx := func(s string) int { return slices.Index(SeverityOrder, s) }
	sort.SliceStable(out, func(i, j int) bool {
		si, sj := severityIdx(out[i].Severity), severityIdx(out[j].Severity)
		if si != sj {
			return si > sj
		}
		return out[i].LastSeenAt > out[j].LastSeenAt
	})
	return out
}

// TypeError is the Python TypeError a malformed value raises
type TypeError struct{ Msg string }

func (e *TypeError) Error() string { return e.Msg }
