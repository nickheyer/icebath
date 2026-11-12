package observability

import (
	"context"
	"errors"
	"net/url"
	"reflect"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// LongSessionTurns is the turn count that marks real use rather than a one shot try out
const LongSessionTurns = 6

const minuteMS = 60_000

const hourS = 3_600

var (
	sessionDurationThresholds = []threshold{{minuteMS, "lt_1m"}, {5 * minuteMS, "1m_to_5m"}, {30 * minuteMS, "5m_to_30m"},
		{120 * minuteMS, "30m_to_2h"}, {480 * minuteMS, "2h_to_8h"}}
	installAgeThresholds = []threshold{{hourS, "lt_1h"}, {24 * hourS, "1h_to_1d"}, {7 * 24 * hourS, "1d_to_7d"},
		{30 * 24 * hourS, "7d_to_30d"}, {90 * 24 * hourS, "30d_to_90d"}}
	ttftThresholds        = []threshold{{0.5, "lt_500ms"}, {1.0, "500ms_to_1s"}, {2.0, "1s_to_2s"}, {5.0, "2s_to_5s"}, {15.0, "5s_to_15s"}}
	contextFillThresholds = []threshold{{50, "lt_50"}, {75, "50_to_75"}, {90, "75_to_90"}, {100, "90_to_100"}}
	usageKeys             = [][2]string{{"input", "input_tokens"}, {"output", "output_tokens"}, {"cache_read", "cache_read_tokens"},
		{"cache_write", "cache_write_tokens"}, {"reasoning", "reasoning_tokens"}}
	compressionTriggers = fromKeys(map[string][]string{
		"auto": {"auto", "threshold", "preflight", "auto_threshold", "idle", "turn_start_threshold", "engine_preflight",
			"pre_api", "post_tool", "gateway_hygiene"},
		"overflow": {"overflow", "context_overflow", "overflow_error", "error"},
		"manual":   {"manual", "user", "command", "slash"},
	})
	compressionOutcomes = fromKeys(map[string][]string{
		"success": {"success", "ok", "compressed", "completed"}, "skipped": {"skipped", "noop", "not_needed"},
	})
	compressionFailureClassAliases = map[string]string{"summary_model_benched": "feasibility_skip"}
	networkPackages                = []string{"net", "crypto/tls", "crypto/x509", "golang.org/x/net", "github.com/gorilla/websocket", "nhooyr.io/websocket", "github.com/coder/websocket"}
)

// Inverts a value to keys table into a key to value lookup
func fromKeys(groups map[string][]string) map[string]string {
	out := map[string]string{}
	for value, keys := range groups {
		for _, k := range keys {
			out[k] = value
		}
	}
	return out
}

// ProviderIdentifier is the public provider name of a value
func ProviderIdentifier(value any) string { return ProviderMetricName(value) }

// SessionFields is one closed session's surface, turn counts, active time and ending
func SessionFields(startFields map[string]string, turns, failedTurns int, lastOutcome string, activeMS int64) map[string]string {
	get := func(key, missing string) string {
		if v, ok := startFields[key]; ok {
			return v
		}
		return missing
	}
	if !TaskOutcomes.Has(lastOutcome) {
		lastOutcome = "unknown"
	}
	return map[string]string{
		"active_duration_bucket": bucket(float64(max(0, activeMS)), sessionDurationThresholds, "gte_8h"),
		"entrypoint":             get("entrypoint", "unknown"), "execution_surface": get("execution_surface", "unknown"),
		"failed_turn_count_bucket": CountBucket(failedTurns), "last_outcome": lastOutcome,
		"platform": get("platform", "none"), "turn_count_bucket": SizeBucket(turns),
	}
}

// InstallAgeBucket buckets an install age in seconds, unknown when not a non negative number
func InstallAgeBucket(ageSeconds any) string {
	age, ok := nonNegativeNumber(ageSeconds)
	if !ok {
		return "unknown"
	}
	return bucket(age, installAgeThresholds, "gte_90d")
}

// TtftBucket is the time to the first streamed chunk of a primary call, not_streamed when none streamed
func TtftBucket(event map[string]any) string {
	started, ok := nonNegativeNumber(event["started_at"])
	if !ok {
		return "unknown"
	}
	first, ok := nonNegativeNumber(event["first_chunk_at"])
	if !ok {
		return "not_streamed"
	}
	return bucket(max(0.0, first-started), ttftThresholds, "gte_15s")
}

// ContextFillBucket is the percentage of the context window the tokens filled
func ContextFillBucket(tokensBefore, contextLength any) string {
	tokens, ok := nonNegativeNumber(tokensBefore)
	length, lok := nonNegativeNumber(contextLength)
	if !ok || !lok || length == 0 {
		return "unknown"
	}
	return bucket(100*tokens/length, contextFillThresholds, "gte_100")
}

// ModelTokenFields is the token usage mark of one call with positive integer amounts, nil without usage
func ModelTokenFields(usage any, model, provider any, callRole string, auxTask any) map[string]any {
	u, ok := asDict(usage)
	if !ok {
		return nil
	}
	out := map[string]any{}
	anyAmount := false
	for _, k := range usageKeys {
		out[k[0]] = 0
		if n, isInt := pyInt(u[k[1]]); isInt && n.Sign() > 0 {
			out[k[0]] = u[k[1]]
			anyAmount = true
		}
	}
	if !anyAmount {
		return nil
	}
	aux := "none"
	if callRole == "auxiliary" {
		aux = AuxTaskMetricName(auxTask)
	}
	out["aux_task"], out["call_role"] = aux, callRole
	out["model"] = ModelMetricName(model, ProviderIdentifier(provider), ModelIdentifierMaxLength)
	out["provider"] = ProviderIdentifier(provider)
	return out
}

// SetupCompletedFields names the configured provider and the setup surface
func SetupCompletedFields(surface, provider any) map[string]string {
	value := norm(surface)
	if !SetupSurfaces.Has(value) {
		value = "other"
	}
	p := "none"
	if pyrepr.Truthy(provider) {
		p = ProviderIdentifier(provider)
	}
	return map[string]string{"provider": p, "surface": value}
}

// CompressionFailureClass collapses an attempt class onto the closed set, a fallback commit keeping its class
func CompressionFailureClass(outcome string, failureClass any) string {
	var raw any
	if s, ok := failureClass.(string); ok {
		raw = s
	}
	value, _, _ := strings.Cut(norm(raw), ":")
	if outcome == "success" {
		if alias, ok := compressionFailureClassAliases[value]; ok {
			value = alias
		}
		if value == "" || value == "none" {
			return "none"
		}
	}
	if value == "" {
		return "unknown"
	}
	if value != "none" && value != "unknown" && CompressionFailureClasses.Has(value) {
		return value
	}
	return "other"
}

// CompressionFields is one compression attempt's fill, class, outcome and trigger
func CompressionFields(trigger, outcome, tokensBefore, contextLength, failureClass any) map[string]string {
	outcomeValue, ok := compressionOutcomes[norm(outcome)]
	if !ok {
		outcomeValue = "failed"
	}
	triggerValue, ok := compressionTriggers[norm(trigger)]
	if !ok {
		triggerValue = "other"
	}
	return map[string]string{
		"context_fill_bucket": ContextFillBucket(tokensBefore, contextLength),
		"failure_class":       CompressionFailureClass(outcomeValue, failureClass), "outcome": outcomeValue, "trigger": triggerValue,
	}
}

// ModelSwitchFields is a model switch's surface and both providers
func ModelSwitchFields(ctx context.Context, fromProvider, toProvider, surface any) map[string]string {
	return map[string]string{
		"execution_surface": ExecutionSurface(ctx, map[string]any{"execution_surface": surface}),
		"from_provider":     ProviderIdentifier(fromProvider), "to_provider": ProviderIdentifier(toProvider),
	}
}

// Enum value of a reason, a typed string reading as its plain string
func enumValue(reason any) any {
	if v, ok := reason.(interface{ Value() any }); ok {
		return v.Value()
	}
	if reason != nil {
		if rv := reflect.ValueOf(reason); rv.Kind() == reflect.String {
			return rv.String()
		}
	}
	return reason
}

// FallbackFields is a provider fallback's error class and both providers
func FallbackFields(fromProvider, toProvider, reason any) map[string]string {
	return map[string]string{
		"error_class":   ModelErrorClass(map[string]any{"reason": enumValue(reason)}),
		"from_provider": ProviderIdentifier(fromProvider), "to_provider": ProviderIdentifier(toProvider),
	}
}

// SlashCommandFields is a slash command's public name and surface
func SlashCommandFields(ctx context.Context, command, surface any) map[string]string {
	return map[string]string{
		"command":           SlashCommandMetricName(ctx, command),
		"execution_surface": ExecutionSurface(ctx, map[string]any{"execution_surface": surface}),
	}
}

// ExtensionInstallFields is one extension install row, nil for a kind outside the contract
func ExtensionInstallFields(kind, source, name, outcome, failureClass, registry any, err error) map[string]string {
	kindValue := norm(kind)
	if !ExtensionKinds.Has(kindValue) {
		return nil
	}
	sourceValue := norm(source)
	if !ExtensionSources.Has(sourceValue) {
		sourceValue = "other"
	}
	outcomeValue := "failed"
	if o := norm(outcome); o == "success" || o == "ok" || o == "installed" {
		outcomeValue = "success"
	}
	return map[string]string{
		"kind": kindValue, "name": ExtensionMetricName(kindValue, name), "outcome": outcomeValue, "source": sourceValue,
		"failure_class": ExtensionFailureClass(kindValue, outcomeValue, failureClass, err),
		"registry":      ExtensionRegistry(kindValue, registry),
	}
}

// TaggedFailureClass is the non empty class a raise site tagged on the error
func TaggedFailureClass(err error) (class string, ok bool) {
	tagged, implements := err.(interface{ FailureClass() string })
	if !implements {
		return "", false
	}
	defer func() {
		if recover() != nil {
			class, ok = "", false
		}
	}()
	class = tagged.FailureClass()
	return class, class != ""
}

// Whether an error in the chain is a type of a network package
func fromNetworkPackage(err error) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		t := reflect.TypeOf(e)
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		for _, p := range networkPackages {
			if t.PkgPath() == p || strings.HasPrefix(t.PkgPath(), p+"/") {
				return true
			}
		}
	}
	return false
}

// ExceptionFailureClass is why a raised install failed from its tag or type, never its message
func ExceptionFailureClass(err error) (class string) {
	if tagged, ok := TaggedFailureClass(err); ok {
		return tagged
	}
	defer func() {
		if recover() != nil {
			class = "exception:" + ownTypeName(err)
		}
	}()
	mro := pyExceptionMRO(err)
	has := func(name string) bool {
		for _, n := range mro {
			if n == name {
				return true
			}
		}
		return false
	}
	var urlErr *url.Error
	switch {
	case has("PermissionError"):
		return "permission"
	case has("ConnectionError") || has("TimeoutError") || errors.As(err, &urlErr) || fromNetworkPackage(err):
		return "network"
	case has("OSError"):
		return "filesystem_error"
	}
	return "exception:" + mro[0]
}

// Name of the error's own dynamic type, read without running any method of the error
func ownTypeName(err error) string {
	t := reflect.TypeOf(err)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Name()
}

// ExtensionFailureClass is none on success, else the stated or raised class collapsed onto the kind's set
func ExtensionFailureClass(kind, outcome string, failureClass any, err error) string {
	if outcome == "success" {
		return "none"
	}
	var value any
	if s, ok := failureClass.(string); ok && s != "" {
		value = s
	}
	if value == nil && err != nil {
		value = ExceptionFailureClass(err)
	}
	head, _, _ := strings.Cut(norm(value), ":")
	if head != "none" && ExtensionKindFailureClasses[kind].Has(head) {
		return head
	}
	return "other"
}

// ExtensionRegistry is the skills hub adapter id of a skill row, none for plugins and MCP servers
func ExtensionRegistry(kind string, registry any) string {
	if kind != "skill" {
		return "none"
	}
	if SkillSourceIDAliasesHook == nil {
		panic("skills hub source alias table not installed")
	}
	value := norm(registry)
	if value == "" {
		value = "none"
	}
	if alias, ok := SkillSourceIDAliasesHook()[value]; ok {
		value = alias
	}
	if ExtensionRegistries.Has(value) {
		return value
	}
	return "other"
}

// Whether a session ran past the short try out turn buckets
func longSession(d map[string]string) bool {
	v, ok := d["turn_count_bucket"]
	return !ok || !Set("0", "1", "2", "3_to_5").Has(v)
}

// milestoneRule is one install milestone and the dimensions that reach it
type milestoneRule struct {
	name    string
	reached func(map[string]string) bool
}

var milestoneRules = map[string][]milestoneRule{
	TaskStartedMetric: {{"first_task_started", func(map[string]string) bool { return true }},
		{"first_gateway_message", func(d map[string]string) bool { v, ok := d["platform"]; return ok && v != "none" }},
		{"first_scheduled_task", func(d map[string]string) bool { return d["entrypoint"] == "scheduled_task" }}},
	TaskFinishedMetric: {{"first_task_success", func(d map[string]string) bool { return d["outcome"] == "success" }}},
	ToolUsageMetric: {{"first_tool_success", func(d map[string]string) bool { return d["outcome"] == "success" }},
		{"first_mcp_tool_success", func(d map[string]string) bool { return d["outcome"] == "success" && d["tool_name"] == "mcp" }},
		{"first_delegation", func(d map[string]string) bool { return d["outcome"] == "success" && d["tool_name"] == "delegate_task" }}},
	SkillLifecycleMetric: {{"first_skill_created", UserCreatedSkill}},
	SkillLoadMetric:      {{"first_skill_reused", func(d map[string]string) bool { return d["reuse_state"] == "reused" }}},
	SessionMetric:        {{"first_long_session", longSession}},
	SetupCompletedMetric: {{"setup_completed", func(map[string]string) bool { return true }}},
}

// MilestonesFor lists the install milestones a recorded counter reaches
func MilestonesFor(metricName string, dimensions map[string]string) []string {
	var out []string
	for _, rule := range milestoneRules[metricName] {
		if rule.reached(dimensions) {
			out = append(out, rule.name)
		}
	}
	return out
}
