package observability

import (
	"context"
	"math/big"
	"reflect"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// lazySet is a vocabulary rebuilt on every lookup because a hook another package installs feeds it
type lazySet func() ValueSet

// Reports whether the value belongs to the vocabulary as it stands now
func (l lazySet) Has(value string) bool { return l().Has(value) }

// CatalogValues is a closed enum backed by public catalogs and loaded on each use
type CatalogValues struct {
	loaders  []string
	extra    ValueSet
	extraFn  func() ValueSet
	adapters bool
}

// Static extras plus every catalog loader's names, adapter vocabularies adding the gateway Platform members
func (c *CatalogValues) Values() ValueSet {
	out := c.staticExtra()
	for _, name := range c.loaders {
		out = out.Union(catalogSafe(name))
	}
	if c.adapters {
		out = out.Union(gatewayAdapterPlatforms())
	}
	return out
}

// Extra values the enum always holds
func (c *CatalogValues) staticExtra() ValueSet {
	if c.extraFn != nil {
		return c.extraFn()
	}
	return c.extra
}

// Reports whether the value is an extra or a current catalog member
func (c *CatalogValues) Has(value string) bool {
	return c.staticExtra().Has(value) || c.Values().Has(value)
}

// Keys of the platform registry pkg/tools serves, hermes_cli.platforms PLATFORMS
func platformRegistryNames() ValueSet {
	out := ValueSet{}
	for _, p := range tools.Platforms {
		out[p.Key] = struct{}{}
	}
	return out
}

// Maps a platform system name onto the shared metrics OS taxonomy
func ClientOSFamily(value any) string {
	if v, ok := osFamilies[norm(value)]; ok {
		return v
	}
	return "unknown"
}

var (
	architectureAliases = map[string]string{
		"amd64": "x86_64", "x64": "x86_64", "x86_64": "x86_64", "aarch64": "arm64", "arm64": "arm64",
		"i386": "x86", "i486": "x86", "i586": "x86", "i686": "x86", "x86": "x86",
	}
	osFamilies = map[string]string{"darwin": "macos", "linux": "linux", "macos": "macos", "windows": "windows"}
)

// Maps a machine architecture onto the shared metrics taxonomy
func ClientArchitecture(value any) string {
	normalized := strings.ReplaceAll(norm(value), "-", "_")
	if v, ok := architectureAliases[normalized]; ok {
		return v
	}
	if strings.HasPrefix(normalized, "armv") {
		return "arm"
	}
	return "unknown"
}

// Allowlisted installation method, nix reading nixos
func ClientInstallMethod(value any) string {
	normalized := norm(value)
	if normalized == "nix" {
		normalized = "nixos"
	}
	return allowlisted(normalized, ClientInstallMethods)
}

// Bounded client resource attached to aggregate packages
func ClientResource(version, osName, architecture, installMethod any) map[string]string {
	v := pystr.Strip(pyText(version))
	if n := pystr.Len(v); n == 0 || n > 64 {
		v = "unknown"
	}
	return map[string]string{
		"architecture":    ClientArchitecture(architecture),
		"icebath_version": v,
		"install_method":  ClientInstallMethod(installMethod),
		"os_family":       ClientOSFamily(osName),
	}
}

// Reports whether a package resource matches the bounded contract exactly
func ClientResourceIsValid(resource any) bool {
	d, ok := asDict(resource)
	if !ok || !keysOf(d).Equal(ClientResourceKeys) {
		return false
	}
	version, ok := d["icebath_version"].(string)
	if !ok || pystr.Len(version) == 0 || pystr.Len(version) > 64 {
		return false
	}
	return inSet(d["os_family"], ClientOSFamilies) && inSet(d["architecture"], ClientArchitectures) &&
		inSet(d["install_method"], ClientInstallMethods)
}

// Python value in allowed for a value that must be a string member
func inSet(v any, allowed allowedValues) bool {
	s, ok := v.(string)
	return ok && allowed.Has(s)
}

// Reports whether dimensions match one closed shared metric contract, current or legacy
func CounterDimensionsAreValid(metricName string, dimensions map[string]any) bool {
	fields := keysOf(dimensions)
	current, known := metricFields[metricName]
	if !known || !fields.Equal(current) {
		legacy := false
		for _, l := range legacyMetricFields[metricName] {
			if fields.Equal(l) {
				legacy = true
				break
			}
		}
		if !legacy {
			return false
		}
	}
	identifiers := identifierFields[metricName]
	contract := counterDimensionValues[metricName]
	for field := range fields {
		value, ok := dimensions[field].(string)
		if !ok {
			return false
		}
		if maxLength, isIdentifier := identifiers[field]; isIdentifier {
			if value != metricIdentifier(value, maxLength) {
				return false
			}
		} else if !contract[field].Has(value) {
			return false
		}
	}
	return extensionInstallFieldsAgree(metricName, dimensions)
}

// String dimensions as the loosely typed map the validator reads
func anyDims(d map[string]string) map[string]any {
	out := make(map[string]any, len(d))
	for k, v := range d {
		out[k] = v
	}
	return out
}

// An install row's failure class sits in its kind's set, is none exactly on success, and only skills name a registry
func extensionInstallFieldsAgree(metricName string, dimensions map[string]any) bool {
	if _, has := dimensions["failure_class"]; metricName != ExtensionInstallMetric || !has {
		return true
	}
	kind, _ := dimensions["kind"].(string)
	failureClass, _ := dimensions["failure_class"].(string)
	return ExtensionKindFailureClasses[kind].Has(failureClass) &&
		(failureClass == "none") == pyrepr.Equal(dimensions["outcome"], "success") &&
		(kind == "skill" || pyrepr.Equal(dimensions["registry"], "none"))
}

// Event metadata when it carries the schema stamp and only allowlisted Relay keys
func relayMetadata(event *RelayEvent, schemaKey, schemaVersion string, extraKeys ...string) map[string]any {
	if event == nil {
		return nil
	}
	metadata, ok := asDict(event.Metadata)
	if !ok {
		return nil
	}
	if v, ok := metadata[schemaKey].(string); !ok || v != schemaVersion {
		return nil
	}
	allowed := Set(append([]string{schemaKey, relayRuntimeInstanceKey, CommitTicketKey, "otel.status_code"}, extraKeys...)...)
	for k := range metadata {
		if !allowed.Has(k) {
			return nil
		}
	}
	if status, has := metadata["otel.status_code"]; has && !pyrepr.Equal(status, "OK") && !pyrepr.Equal(status, "ERROR") {
		return nil
	}
	return metadata
}

// One Relay event attribute by its Python name, nil when absent
func eventAttr(event *RelayEvent, attr string) any {
	if event == nil {
		return nil
	}
	switch attr {
	case "kind":
		return event.Kind
	case "name":
		return event.Name
	case "category":
		return event.Category
	case "scope_category":
		return event.ScopeCategory
	case "category_profile":
		return event.CategoryProfile
	case "metadata":
		return event.Metadata
	case "data":
		return event.Data
	}
	return nil
}

// Python str(getattr(event, attr, "") or "")
func eventText(event *RelayEvent, attr string) string {
	return pyText(eventAttr(event, attr))
}

// shapeField is one expected Relay attribute, a string compared as text, nil requiring None, anything else by equality
type shapeField struct {
	attr  string
	value any
}

// Matches the coarse Relay event shape on every mentioned attribute
func eventShapeMatches(event *RelayEvent, expected ...shapeField) bool {
	for _, e := range expected {
		switch v := e.value.(type) {
		case string:
			if eventText(event, e.attr) != v {
				return false
			}
		case nil:
			if eventAttr(event, e.attr) != nil {
				return false
			}
		default:
			if !pyrepr.Equal(eventAttr(event, e.attr), v) {
				return false
			}
		}
	}
	return true
}

// Projects data onto the metric's closed field set, false when it does not fit
func boundedDimensions(metricName string, data any) (map[string]string, bool) {
	d, ok := asDict(data)
	if !ok || !keysOf(d).Equal(metricFields[metricName]) {
		return nil, false
	}
	if !CounterDimensionsAreValid(metricName, d) {
		return nil, false
	}
	out := make(map[string]string, len(d))
	for k, v := range d {
		out[k] = v.(string)
	}
	if !catalogHolds(metricName, out) {
		return nil, false
	}
	return out, true
}

// Reruns the producer's catalog pass on provider and model fields so a rewritten identifier never reaches a counter
func catalogHolds(metricName string, dimensions map[string]string) bool {
	for field, maxLength := range identifierFields[metricName] {
		value := dimensions[field]
		var expected string
		switch {
		case strings.HasSuffix(field, "provider"):
			expected = value
			if value != "none" {
				expected = ProviderMetricName(value)
			}
		case strings.HasSuffix(field, "model"):
			expected = ModelMetricName(value, dimensions[field[:len(field)-5]+"provider"], maxLength)
		default:
			continue
		}
		if value != expected {
			return false
		}
	}
	return true
}

// Metadata allowlist check plus the event shape match
func validShape(event *RelayEvent, shape ...shapeField) bool {
	return relayMetadata(event, SchemaKey, SchemaVersion) != nil && eventShapeMatches(event, shape...)
}

// Counter is one validated metric row, its name and closed dimensions
type Counter struct {
	Metric     string
	Dimensions map[string]string
}

// Bounded counter of an event's data for a metric, false when the metric is unset or the data does not fit
func boundedCounter(metricName string, event *RelayEvent) (Counter, bool) {
	if metricName == "" {
		return Counter{}, false
	}
	d, ok := boundedDimensions(metricName, eventAttr(event, "data"))
	if !ok {
		return Counter{}, false
	}
	return Counter{Metric: metricName, Dimensions: d}, true
}

// Bounded data of a scope end event of the given shape
func scopedDimensions(event *RelayEvent, metricName string, shape ...shapeField) (map[string]string, bool) {
	if !validShape(event, append([]shapeField{{"kind", "scope"}, {"scope_category", "end"}}, shape...)...) {
		return nil, false
	}
	return boundedDimensions(metricName, eventAttr(event, "data"))
}

var markShape = []shapeField{{"kind", "mark"}, {"category", nil}, {"scope_category", nil}, {"category_profile", nil}}

// Bounded counter of a safe Relay mark whose name maps to a metric
func markCounter(event *RelayEvent, metricsByMark map[string]string) (Counter, bool) {
	if !validShape(event, markShape...) {
		return Counter{}, false
	}
	return boundedCounter(metricsByMark[eventText(event, "name")], event)
}

// Active install counter of one empty allowlisted mark
func ClientActiveCounter(event *RelayEvent) (Counter, bool) {
	return markCounter(event, map[string]string{ClientActiveMark: ClientActiveMetric})
}

// Package dimensions of one valid logical model call end event
func ModelCallDimensions(event *RelayEvent) (map[string]string, bool) {
	if aux, ok := auxiliaryModelCallDimensions(event); ok {
		return aux, true
	}
	return scopedDimensions(event, ModelRouteMetric, shapeField{"category", "llm"}, shapeField{"name", ModelCallScope},
		shapeField{"category_profile", map[string]any{"model_name": ModelCallProfileModel}})
}

// Terminal auxiliary route projected from the logical LLM scope
func auxiliaryModelCallDimensions(event *RelayEvent) (map[string]string, bool) {
	metadata := relayMetadata(event, relayRuntimeSchemaKey, relayRuntimeSchemaVersion, "icebath.call_role")
	callRole, _ := metadata["icebath.call_role"].(string)
	data, isDict := asDict(eventAttr(event, "data"))
	if callRole == "" || !strings.HasPrefix(callRole, "auxiliary:") ||
		!eventShapeMatches(event, shapeField{"kind", "scope"}, shapeField{"category", "function"},
			shapeField{"name", relayLogicalLLMScope}, shapeField{"scope_category", "end"}, shapeField{"category_profile", nil}) ||
		!isDict || !keysOf(data).Minus("response_model", "error_class").Equal(Set("model", "outcome", "provider")) ||
		!inSet(data["outcome"], legacyModelOutcomes) {
		return nil, false
	}
	outcome := data["outcome"].(string)
	var errorClass any = "none"
	if outcome != "cancelled" {
		if pyrepr.Truthy(data["error_class"]) {
			errorClass = data["error_class"]
		}
	}
	errorText, _ := errorClass.(string)
	if outcome == "failed" && pyrepr.Equal(errorClass, "none") {
		errorText = "unknown"
	} else if _, isStr := errorClass.(string); !isStr {
		errorText = "unknown"
	}
	d := ModelRouteFields(data, "auxiliary", outcome, errorText, "unknown")
	if !CounterDimensionsAreValid(ModelRouteMetric, anyDims(d)) {
		return nil, false
	}
	return d, true
}

// Validated task counter of a task scope start or end event
func TaskCounter(event *RelayEvent) (Counter, bool) {
	if !validShape(event, shapeField{"kind", "scope"}, shapeField{"category", "function"}, shapeField{"name", TaskScope},
		shapeField{"category_profile", nil}) {
		return Counter{}, false
	}
	switch eventText(event, "scope_category") {
	case "start":
		return boundedCounter(TaskStartedMetric, event)
	case "end":
		return taskEndCounter(event, TaskFinishedMetric)
	}
	return Counter{}, false
}

// Duration and retry counter carried by the same task end event
func TaskDurationCounter(event *RelayEvent) (Counter, bool) {
	if !validShape(event, shapeField{"kind", "scope"}, shapeField{"scope_category", "end"},
		shapeField{"category", "function"}, shapeField{"name", TaskScope}, shapeField{"category_profile", nil}) {
		return Counter{}, false
	}
	return taskEndCounter(event, TaskDurationMetric)
}

var taskEndFields = metricFields[TaskFinishedMetric].Union(metricFields[TaskDurationMetric],
	Set("model_call_count_bucket", "tool_call_count_bucket"))

// Projects a task end event onto one counter once both projections validate
func taskEndCounter(event *RelayEvent, metricName string) (Counter, bool) {
	data, ok := asDict(eventAttr(event, "data"))
	if !ok || !keysOf(data).Equal(taskEndFields) {
		return Counter{}, false
	}
	projections := map[string]map[string]string{}
	for _, name := range []string{TaskFinishedMetric, TaskDurationMetric} {
		d, ok := boundedDimensions(name, projectFields(data, metricFields[name]))
		if !ok || len(d) == 0 {
			return Counter{}, false
		}
		projections[name] = d
	}
	return Counter{Metric: metricName, Dimensions: projections[metricName]}, true
}

// The data entries whose keys are in fields
func projectFields(data map[string]any, fields ValueSet) map[string]any {
	out := make(map[string]any, len(fields))
	for f := range fields {
		out[f] = data[f]
	}
	return out
}

// Package dimensions of one allowlisted tool lifecycle end event
func ToolCallDimensions(event *RelayEvent) (map[string]string, bool) {
	return toolEndProjection(event, ToolCallMetric)
}

// Per tool usage dimensions carried by the same tool lifecycle end event
func ToolUsageDimensions(event *RelayEvent) (map[string]string, bool) {
	return toolEndProjection(event, ToolUsageMetric)
}

// Per category latency and retry dimensions carried by the same tool end event
func ToolLatencyDimensions(event *RelayEvent) (map[string]string, bool) {
	return toolEndProjection(event, ToolLatencyMetric)
}

var (
	toolEndMetrics = []string{ToolCallMetric, ToolUsageMetric, ToolLatencyMetric}
	toolEndFields  = metricFields[ToolCallMetric].Union(metricFields[ToolUsageMetric], metricFields[ToolLatencyMetric])
)

// Projects a tool end event onto one counter once every projection validates
func toolEndProjection(event *RelayEvent, metricName string) (map[string]string, bool) {
	if !validShape(event, shapeField{"kind", "scope"}, shapeField{"scope_category", "end"}, shapeField{"category", "tool"},
		shapeField{"name", ToolCallScope}, shapeField{"category_profile", map[string]any{}}) {
		return nil, false
	}
	data, ok := asDict(eventAttr(event, "data"))
	if !ok || !keysOf(data).Equal(toolEndFields) {
		return nil, false
	}
	projections := map[string]map[string]string{}
	for _, name := range toolEndMetrics {
		d, ok := boundedDimensions(name, projectFields(data, metricFields[name]))
		if !ok || len(d) == 0 {
			return nil, false
		}
		projections[name] = d
	}
	return projections[metricName], true
}

// Daily install configuration snapshot from a safe mark
func InstallSnapshotCounter(event *RelayEvent) (Counter, bool) {
	return markCounter(event, map[string]string{InstallSnapshotMark: InstallSnapshotMetric})
}

// Validated approval counter from a safe Relay mark
func ToolApprovalCounter(event *RelayEvent) (Counter, bool) {
	return markCounter(event, map[string]string{ToolApprovalMark: ToolApprovalMetric})
}

// Validated skill lifecycle or load counter from a safe mark
func SkillCounter(event *RelayEvent) (Counter, bool) {
	return markCounter(event, skillMarkMetrics)
}

// Counter of one session, setup, compression, switch, fallback, command, install or other decision mark
func DecisionCounter(event *RelayEvent) (Counter, bool) {
	return markCounter(event, decisionMarkMetrics)
}

var tokenMarkDimensions = Set("aux_task", "call_role", "model", "provider")

// TokenSum is one model token sum row, its dimensions and amount
type TokenSum struct {
	Metric     string
	Dimensions map[string]string
	Amount     int64
}

// Expands one token usage mark into one sum per non zero token type, nothing when any part is invalid
func ModelTokenCounters(event *RelayEvent) []TokenSum {
	if !validShape(event, markShape...) || eventText(event, "name") != ModelTokensMark {
		return nil
	}
	data, ok := asDict(eventAttr(event, "data"))
	if !ok || !keysOf(data).Equal(tokenMarkDimensions.Union(TokenTypes)) {
		return nil
	}
	var counters []TokenSum
	for _, tokenType := range TokenTypes.Sorted() {
		amount, ok := pyInt(data[tokenType])
		if !ok || amount.Sign() < 0 || !amount.IsInt64() {
			return nil
		}
		d := map[string]any{"token_type": tokenType}
		for f := range tokenMarkDimensions {
			d[f] = data[f]
		}
		if !CounterDimensionsAreValid(ModelTokensMetric, d) {
			return nil
		}
		dimensions := map[string]string{}
		for k, v := range d {
			dimensions[k] = v.(string)
		}
		if !catalogHolds(ModelTokensMetric, dimensions) {
			return nil
		}
		if amount.Sign() != 0 {
			counters = append(counters, TokenSum{Metric: ModelTokensMetric, Dimensions: dimensions, Amount: amount.Int64()})
		}
	}
	return counters
}

// Bounded fields of one successful non load skill transition
func SkillLifecycleFields(kwargs map[string]any) (map[string]string, bool) {
	action := norm(kwargs["action"])
	if !SkillLifecycleActions.Has(action) {
		return nil, false
	}
	return map[string]string{"action": action, "provenance": SkillProvenance(kwargs["provenance"])}, true
}

// Bounded skill use fields without exporting local skill identity
func SkillLoadFields(kwargs map[string]any) (map[string]string, bool) {
	useCount, isInt := pyInt(kwargs["use_count"])
	reused, reusedOK := kwargs["reused"].(bool)
	afterPatch, afterOK := kwargs["reuse_after_patch"].(bool)
	if !isInt || useCount.Cmp(big.NewInt(1)) < 0 || !reusedOK || !afterOK || (afterPatch && !reused) {
		return nil, false
	}
	state := "no_new_patch"
	if !reused {
		state = "not_applicable"
	} else if afterPatch {
		state = "reused_after_patch"
	}
	reuse := "first_use"
	if reused {
		reuse = "reused"
	}
	return map[string]string{
		"post_patch_state": state,
		"provenance":       SkillProvenance(kwargs["provenance"]),
		"reuse_state":      reuse,
		"skill_name":       SkillMetricName(kwargs["skill_name"]),
		"use_count_bucket": CountBucket(bigCount(useCount)),
	}, true
}

// Int count of a Python int, saturating beyond the int range
func bigCount(i *big.Int) int {
	if !i.IsInt64() {
		if i.Sign() < 0 {
			return 0
		}
		return int(^uint(0) >> 1)
	}
	return int(i.Int64())
}

// Producer provenance normalized onto the closed taxonomy
func SkillProvenance(value any) string {
	return allowlisted(norm(value), SkillProvenances)
}

var (
	surfaceAliases = map[string]string{
		"api_server": "api", "relay": "gateway", "cron": "scheduled_task", "scheduler": "scheduled_task",
		"scheduled": "scheduled_task",
	}
	knownGatewayPlatforms = Set("discord", "email", "slack", "telegram", "teams", "whatsapp")
)

// Safe session surface carried by the parent Relay scope
func ExecutionSurface(kwargs map[string]any) string {
	raw := kwargs["execution_surface"]
	if !pyrepr.Truthy(raw) {
		raw = kwargs["platform"]
	}
	if !pyrepr.Truthy(raw) {
		raw = "unknown"
	}
	value := norm(raw)
	if ExecutionSurfaces.Has(value) {
		return value
	}
	if alias, ok := surfaceAliases[value]; ok {
		return alias
	}
	if allPlatformNames().Has(value) || knownGatewayPlatforms.Has(value) {
		return "gateway"
	}
	return "other"
}

// Platforms the registry serves plus plugin registered platforms, hermes get_all_platforms
func allPlatformNames() ValueSet {
	return platformRegistryNames().Union(Set(pluginPlatformNames()...))
}

// Bounded fields recorded on a task scope start event
func TaskStartFields(ctx context.Context, kwargs map[string]any) map[string]string {
	surface := ExecutionSurface(kwargs)
	return map[string]string{
		"entrypoint": TaskEntrypoint(kwargs, surface), "execution_surface": surface,
		"platform": GatewayPlatform(ctx, kwargs, surface),
	}
}

// Messaging platform of a gateway task, unproven plugin platforms reading plugin and other surfaces none
func GatewayPlatform(ctx context.Context, kwargs map[string]any, surface string) string {
	if surface == "" {
		surface = ExecutionSurface(kwargs)
	}
	if surface != "gateway" {
		return "none"
	}
	name := AdapterPlatform(ctx, kwargs["platform"])
	if GatewayPlatforms.Has(name) {
		return name
	}
	return "plugin"
}

// Public name of a gateway adapter's platform, a Platform enum value or its text
func AdapterPlatform(ctx context.Context, value any) string {
	return PlatformMetricName(ctx, value, coreGatewayPlatforms.Union(gatewayAdapterPlatforms()))
}

var surfaceEntrypoints = map[string]string{
	"acp": "interactive", "cli": "interactive", "desktop": "interactive", "tui": "interactive",
	"api": "api", "batch": "batch", "python": "python", "scheduled_task": "scheduled_task", "unknown": "unknown",
	"gateway": "gateway_message",
}

// Task dispatch owner without exporting source strings
func TaskEntrypoint(kwargs map[string]any, surface string) string {
	declared := norm(kwargs["entrypoint"])
	if TaskEntrypoints.Has(declared) {
		return declared
	}
	if pyrepr.Truthy(kwargs["parent_task_id"]) || pyrepr.Truthy(kwargs["parent_session_id"]) {
		return "delegated"
	}
	if surface == "" {
		surface = ExecutionSurface(kwargs)
	}
	if e, ok := surfaceEntrypoints[surface]; ok {
		return e
	}
	return "other"
}

// Bounded terminal payload of one task scope
func TaskTerminalFields(ctx context.Context, kwargs map[string]any, durationMS, modelCallCount, toolCallCount, retryCount int) map[string]string {
	outcome, endReason, termination := TaskTerminalState(kwargs)
	out := TaskStartFields(ctx, kwargs)
	out["duration_bucket"] = DurationBucket(durationMS)
	out["end_reason"] = endReason
	out["failure_class"] = TaskFailureClass(kwargs, outcome)
	out["model_call_count_bucket"] = CountBucket(modelCallCount)
	out["outcome"] = outcome
	out["retry_count_bucket"] = CountBucket(retryCount)
	out["termination"] = termination
	out["tool_call_count_bucket"] = CountBucket(toolCallCount)
	return out
}

var (
	localFailureClasses      = map[string]string{"interpreter_shutdown": "shutdown", "session_busy": "session_busy"}
	exitReasonFailureClasses = [][2]string{
		{"empty_response", "empty_response"}, {"all_retries_exhausted", "empty_response"},
		{"context_compression", "context_compression"}, {"compaction_", "context_compression"},
		{"ollama_runtime_context", "context_compression"},
		{"local_processing_error", "local_error"}, {"repeated_outer_errors", "repeated_errors"},
		{"error_near_max_iterations", "repeated_errors"}, {"session_persistence", "persistence"},
		{"redirect_restart_limit", "restart_limit"}, {"rebuilt_restart_limit", "restart_limit"},
	}
)

// Closed class of why a failed task failed, none for any other outcome
func TaskFailureClass(kwargs map[string]any, outcome string) string {
	if outcome == "" {
		outcome, _, _ = TaskTerminalState(kwargs)
	}
	if outcome != "failed" {
		return "none"
	}
	if declared := norm(kwargs["failure_class"]); declared != "none" && TaskFailureClasses.Has(declared) {
		return declared
	}
	failureReason := norm(kwargs["failure_reason"])
	if failureReason != "none" && ModelErrorClasses.Has(failureReason) {
		return failureReason
	}
	if c, ok := localFailureClasses[failureReason]; ok {
		return c
	}
	reason := norm(kwargs["turn_exit_reason"])
	for _, pair := range exitReasonFailureClasses {
		if strings.HasPrefix(reason, pair[0]) {
			return pair[1]
		}
	}
	return "other"
}

// Terminal state mapped onto bounded outcome, end reason and termination
func TaskTerminalState(kwargs map[string]any) (string, string, string) {
	reason := norm(kwargs["turn_exit_reason"])
	has := func(s string) bool { return strings.Contains(reason, s) }
	switch {
	case pyrepr.Truthy(kwargs["interrupted"]) || has("interrupt") || has("cancel"):
		return "cancelled", "user_cancelled", "user_cancelled"
	case has("timeout") || has("timed_out"):
		return "timed_out", "timed_out", "timed_out"
	case has("max_iterations") || has("budget_exhausted"):
		return "failed", "iteration_limit", "system_aborted"
	case has("approval") && (has("denied") || has("rejected")):
		return "failed", "approval_denied", "none"
	case has("guardrail"):
		return "failed", "guardrail_blocked", "system_aborted"
	case reason == "system_aborted":
		return "failed", "system_aborted", "system_aborted"
	case kwargs["completed"] == true:
		return "success", "completed", "none"
	case kwargs["failed"] == true || (reason != "" && reason != "unknown"):
		return "failed", "failed", "none"
	}
	return "unknown", "unknown", "unknown"
}

var (
	durationThresholds = []threshold{{1_000, "lt_1s"}, {5_000, "1s_to_5s"}, {30_000, "5s_to_30s"}, {120_000, "30s_to_2m"}, {600_000, "2m_to_10m"}}
	countThresholds    = []threshold{{1, "0"}, {2, "1"}, {3, "2"}, {6, "3_to_5"}, {11, "6_to_10"}}
	latencyThresholds  = []threshold{{100, "lt_100ms"}, {250, "100ms_to_250ms"}, {500, "250ms_to_500ms"}, {1_000, "500ms_to_1s"},
		{2_000, "1s_to_2s"}, {5_000, "2s_to_5s"}, {10_000, "5s_to_10s"}, {30_000, "10s_to_30s"}}
	sizeThresholds = []threshold{{1, "0"}, {2, "1"}, {3, "2"}, {6, "3_to_5"}, {11, "6_to_10"}, {26, "11_to_25"}, {101, "26_to_100"}, {251, "101_to_250"}}
)

// Bucket of a non negative task duration in milliseconds
func DurationBucket(durationMS int) string {
	return bucket(clampCount(durationMS), durationThresholds, "gte_10m")
}

// Bucket of a non negative per task count
func CountBucket(count int) string {
	return bucket(clampCount(count), countThresholds, "gte_11")
}

var (
	toolCategoryExact = func() map[string]string {
		out := map[string]string{"clarify": "planning", "kanban": "planning", "todo": "planning",
			"session_search": "memory", "cronjob": "scheduler", "skills": "skill", "x_search": "web"}
		for c := range ToolCategories {
			out[c] = c
		}
		return out
	}()
	toolCategoryPrefixes = []struct {
		prefixes []string
		category string
	}{
		{[]string{"mcp"}, "mcp"},
		{[]string{"browser"}, "browser"},
		{[]string{"image", "tts", "video", "vision"}, "media"},
		{[]string{"homeassistant"}, "home_automation"},
		{[]string{"discord", "email", "feishu", "icebath-yuanbao", "slack", "sms"}, "communication"},
	}
)

// Low cardinality category of a tool from its registry toolset
func ToolCategory(kwargs map[string]any) string {
	if strings.HasPrefix(norm(kwargs["tool_name"]), tools.ConnectorNamePrefix) {
		return "connector"
	}
	toolset := norm(kwargs["toolset"])
	if toolset == "" {
		return "unknown"
	}
	if c, ok := toolCategoryExact[toolset]; ok {
		return c
	}
	for _, p := range toolCategoryPrefixes {
		for _, prefix := range p.prefixes {
			if strings.HasPrefix(toolset, prefix) {
				return p.category
			}
		}
	}
	return "other"
}

var toolStatusOutcomes = map[string]string{
	"blocked": "blocked", "cancelled": "cancelled", "failed": "failed", "success": "success", "timed_out": "timed_out",
	"error": "failed", "ok": "success", "timeout": "timed_out",
}

// Terminal tool status normalized without inspecting its result
func ToolOutcome(kwargs map[string]any) string {
	if o, ok := toolStatusOutcomes[norm(kwargs["status"])]; ok {
		return o
	}
	return "unknown"
}

var approvalChoices = map[string]string{
	"always": "approved", "approve": "approved", "approved": "approved", "once": "approved", "session": "approved",
	"smart_approve": "approved", "deny": "denied", "denied": "denied", "smart_deny": "denied",
	"timed_out": "timed_out", "timeout": "timed_out", "cancelled": "cancelled",
}

// Terminal approval choice normalized to a bounded outcome
func ToolApprovalOutcome(kwargs map[string]any) string {
	if o, ok := approvalChoices[norm(kwargs["choice"])]; ok {
		return o
	}
	return "unknown"
}

// ToolTerminalOptions carries the optional keyword arguments of the tool terminal payload, nil reading as None
type ToolTerminalOptions struct {
	Category           string
	ApprovalOutcome    *string
	FallbackDurationMS any
	ToolName           *string
}

// Bounded tool call terminal payload feeding the category and per tool counters
func ToolTerminalFields(kwargs map[string]any, opts ToolTerminalOptions) map[string]string {
	outcome := ToolOutcome(kwargs)
	approval := "not_required"
	if opts.ApprovalOutcome != nil {
		approval = *opts.ApprovalOutcome
	}
	category := opts.Category
	if !ToolCategories.Has(category) {
		category = ToolCategory(kwargs)
	}
	toolName := ""
	if opts.ToolName != nil && liveToolNames.Has(*opts.ToolName) {
		toolName = *opts.ToolName
	} else {
		toolName = ToolMetricName(kwargs)
	}
	return map[string]string{
		"approval_outcome":   allowlisted(approval, ToolApprovalOutcomes),
		"error_class":        ToolErrorClass(kwargs, outcome),
		"latency_bucket":     ToolLatencyBucket(kwargs["duration_ms"], opts.FallbackDurationMS),
		"outcome":            outcome,
		"retry_count_bucket": ToolRetryBucket(kwargs["retry_count"]),
		"tool_category":      category,
		"tool_name":          toolName,
	}
}

// A built in or bridge tool's own name, connector, MCP and plugin tools collapsing to their kind
func ToolMetricName(kwargs map[string]any) string {
	name := norm(kwargs["tool_name"])
	if tools.BuiltinToolNames[name] || BridgeToolNames.Has(name) {
		return name
	}
	if name == "" {
		return "unknown"
	}
	if strings.HasPrefix(name, tools.ConnectorNamePrefix) {
		return "connector"
	}
	if strings.HasPrefix(name, "mcp_") || ToolCategory(kwargs) == "mcp" {
		return "mcp"
	}
	return "plugin"
}

var (
	toolStatusErrorClasses = map[string]string{"blocked": "blocked", "cancelled": "interrupted", "success": "none", "timed_out": "timeout"}
	toolErrorTypes         = map[string]string{
		"keyboard_interrupt": "interrupted", "tool_interrupted": "interrupted", "user_interrupt": "interrupted",
		"invalid_tool_arguments": "invalid_arguments", "thread_missing_result": "exception",
		"tool_error": "tool_error", "tool_result_contract": "contract_violation", "tool_timeout": "timeout",
	}
)

// Closed failure class from the tool's own error type, connector codes naming their class
func ToolErrorClass(kwargs map[string]any, outcome string) string {
	if outcome == "" {
		outcome = ToolOutcome(kwargs)
	}
	if c, ok := toolStatusErrorClasses[outcome]; ok {
		return c
	}
	if outcome != "failed" {
		return "unknown"
	}
	errorType := norm(kwargs["error_type"])
	if errorType == "tool_error" {
		if c, ok := ConnectorErrorClass(kwargs); ok && c != "" {
			return c
		}
	}
	if c, ok := toolErrorTypes[errorType]; ok {
		return c
	}
	if errorType != "" {
		return "exception"
	}
	return "unknown"
}

var (
	connectorCodeClasses = map[string]string{
		"CONNECTION_REQUIRED": "connection_required", "CONNECTION_EXPIRED": "connection_required",
		"TOOL_NOT_ALLOWED": "not_allowed", "ALIAS_REQUIRED": "alias", "ALIAS_UNKNOWN": "alias",
		"INVALID_ARGUMENTS": "invalid_arguments", "FILE_NOT_ALLOWED": "not_allowed",
		"FILE_NOT_FOUND": "invalid_arguments", "FILE_TOO_LARGE": "invalid_arguments",
	}
	connectorFamilyClasses = map[string]string{"backend": "connector_backend"}
)

// Class of a failed connector entry from its result's error code, false when not a connector call or no code
func ConnectorErrorClass(kwargs map[string]any) (string, bool) {
	if !strings.HasPrefix(norm(kwargs["tool_name"]), tools.ConnectorNamePrefix) {
		return "", false
	}
	errObj := resultError(kwargs["result"])
	code := strings.ToUpper(pystr.Strip(pyText(errObj["code"])))
	if code == "" {
		return "", false
	}
	family, _ := errObj["family"].(string)
	if c := connectorFamilyClasses[tools.ExecuteErrorFamily(code, family)]; c != "" {
		return c, true
	}
	c, ok := connectorCodeClasses[code]
	return c, ok
}

// The error object of a tool result, decoded from JSON text when needed
func resultError(result any) map[string]any {
	if s, ok := result.(string); ok {
		decoded, err := pyjson.Loads(s)
		if err != nil {
			return map[string]any{}
		}
		result = decoded
	}
	if d, ok := asDict(result); ok {
		if e, ok := asDict(d["error"]); ok {
			return e
		}
	}
	return map[string]any{}
}

// Bucket of a tool duration in milliseconds, falling back to the measured duration
func ToolLatencyBucket(value any, fallbackDurationMS any) string {
	durationMS, ok := nonNegativeNumber(value)
	if !ok {
		durationMS, ok = nonNegativeNumber(fallbackDurationMS)
	}
	if !ok {
		return "unknown"
	}
	return bucket(durationMS, latencyThresholds, "gte_30s")
}

// Bucket of explicit tool retries only, missing relationships staying unknown
func ToolRetryBucket(value any) string {
	n, ok := pyInt(value)
	if !ok || n.Sign() < 0 {
		return "unknown"
	}
	return CountBucket(bigCount(n))
}

// Terminal model identity and provider route
func ModelCallFields(kwargs map[string]any) map[string]string {
	provider := ProviderMetricName(kwargs["provider"])
	model := ModelMetricName(kwargs["response_model"], provider, ModelIdentifierMaxLength)
	if model == "unknown" {
		model = ModelMetricName(kwargs["model"], provider, ModelIdentifierMaxLength)
	}
	return map[string]string{"model": model, "provider": provider}
}

// Terminal route plus how the logical call ended, the error class being the last classified attempt error
func ModelRouteFields(kwargs map[string]any, callRole, outcome, errorClass, ttftBucket string) map[string]string {
	out := ModelCallFields(kwargs)
	out["call_role"] = allowlisted(callRole, ModelCallRoles)
	out["error_class"] = errorClass
	if !ModelErrorClasses.Has(errorClass) {
		out["error_class"] = "unknown"
	}
	out["outcome"] = outcome
	if !ModelOutcomes.Has(outcome) {
		out["outcome"] = "failed"
	}
	out["ttft_bucket"] = ttftBucket
	if !TTFTBuckets.Has(ttftBucket) {
		out["ttft_bucket"] = "unknown"
	}
	return out
}

// The classifier's failover reason of one failed provider attempt
func ModelErrorClass(kwargs map[string]any) string {
	reason := norm(kwargs["reason"])
	if reason != "none" && ModelErrorClasses.Has(reason) {
		return reason
	}
	return "unknown"
}

// Bucket of a non negative install scale count
func SizeBucket(count int) string {
	return bucket(clampCount(count), sizeThresholds, "gte_251")
}

// InstallSnapshotInput is the keyword argument set of the daily configuration snapshot
type InstallSnapshotInput struct {
	MemoryProvider     any
	MCPServers         int
	Plugins            int
	Skills             int
	CronJobs           int
	Profiles           int
	MessagingPlatforms int
	InstallAgeBucket   string
	MainProvider       any
	TerminalBackend    any
	DisplayLanguage    any
}

// Bounded daily configuration snapshot of counts, closed enums and public names only
func InstallSnapshotFields(in InstallSnapshotInput) map[string]string {
	provider := norm(in.MemoryProvider)
	backend := norm(in.TerminalBackend)
	if backend == "" {
		backend = "local"
	}
	age := in.InstallAgeBucket
	if !InstallAgeBuckets.Has(age) {
		age = "unknown"
	}
	mainProvider := "none"
	if pyrepr.Truthy(in.MainProvider) {
		mainProvider = ProviderMetricName(in.MainProvider)
	}
	memory := "plugin"
	if provider == "" {
		memory = "builtin"
	} else if MemoryProviders.Has(provider) {
		memory = provider
	}
	if !TerminalBackends.Has(backend) {
		backend = "other"
	}
	return map[string]string{
		"cron_job_count_bucket":           SizeBucket(in.CronJobs),
		"display_language":                DisplayLanguageMetricName(in.DisplayLanguage),
		"install_age_bucket":              age,
		"main_provider":                   mainProvider,
		"mcp_server_count_bucket":         SizeBucket(in.MCPServers),
		"memory_provider":                 memory,
		"messaging_platform_count_bucket": SizeBucket(in.MessagingPlatforms),
		"plugin_count_bucket":             SizeBucket(in.Plugins),
		"profile_count_bucket":            SizeBucket(in.Profiles),
		"skill_count_bucket":              SizeBucket(in.Skills),
		"terminal_backend":                backend,
	}
}

// One structurally safe identifier without a product catalog, unknown when unsafe or too long
func metricIdentifier(value any, maxLength int) string {
	s, ok := value.(string)
	if !ok {
		return "unknown"
	}
	identifier := pystr.Lower(pystr.Strip(s))
	if identifier == "" || pystr.Len(identifier) > maxLength ||
		!strings.ContainsRune(metricIdentifierStartCharacters, []rune(identifier)[0]) {
		return "unknown"
	}
	for _, r := range identifier {
		if !strings.ContainsRune(metricIdentifierCharacters, r) {
			return "unknown"
		}
	}
	return identifier
}

// Sorted dimension keys, the Python dict order a bounded projection builds
func SortedKeys(d map[string]string) []string {
	out := make([]string, 0, len(d))
	for k := range d {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Text of a value or its string kind, the getattr(raw, "value", raw) unwrap of an enum
func enumText(raw any) any {
	if raw == nil {
		return nil
	}
	if _, ok := raw.(string); ok {
		return raw
	}
	v := reflect.ValueOf(raw)
	if v.Kind() == reflect.String {
		return v.String()
	}
	return raw
}
