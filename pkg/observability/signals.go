package observability

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

var daysSinceInstallThresholds = []threshold{
	{daySeconds, "same_day"}, {7 * daySeconds, "1d_to_7d"}, {30 * daySeconds, "7d_to_30d"}, {90 * daySeconds, "30d_to_90d"},
}

// ToolAvailabilityAgent is the agent state that decides whether a missed built in counts, implemented by pkg/agent
type ToolAvailabilityAgent interface {
	DelegateDepth() int
	Platform() string
	BackgroundReview() bool
	ToolSearchScopedNames() map[string]bool
}

// Fields for a call to a shipped built in the session disabled, nil for anything else
func ToolUnavailableFields(agent ToolAvailabilityAgent, name any, issue string, route map[string]string) map[string]string {
	tool, ok := name.(string)
	if issue != "unknown_tool" || !ok || !tools.BuiltinToolNames[tool] {
		return nil
	}
	if agent.DelegateDepth() != 0 || agent.Platform() == "cron" {
		return nil
	}
	if agent.BackgroundReview() || agent.ToolSearchScopedNames()[tool] {
		return nil
	}
	fields := make(map[string]string, len(route)+1)
	for key, value := range route {
		fields[key] = value
	}
	fields["tool_name"] = tool
	return fields
}

func success(d map[string]string) bool { return d["outcome"] == "success" }

func toolUsed(prefix string, names ...string) func(map[string]string) bool {
	return func(d map[string]string) bool {
		tool := d["tool_name"]
		if !success(d) {
			return false
		}
		for _, name := range names {
			if tool == name {
				return true
			}
		}
		return prefix != "" && strings.HasPrefix(tool, prefix)
	}
}

// UserCreatedSkill reports a skill created at the user's request, not by the background review
func UserCreatedSkill(d map[string]string) bool {
	provenance, ok := d["provenance"]
	return d["action"] == "created" && (!ok || provenance != "agent_created")
}

type featureRule struct {
	feature string
	used    func(map[string]string) bool
}

func dimensionIn(d map[string]string, key string, values ...string) bool {
	value, ok := d[key]
	if !ok {
		return false
	}
	for _, v := range values {
		if value == v {
			return true
		}
	}
	return false
}

var taskSurface = map[string]string{"desktop": "desktop", "tui": "tui"}

var featureRules = func() map[string][]featureRule {
	rules := map[string][]featureRule{
		MemoryOpMetric:       {{"memory", func(d map[string]string) bool { return success(d) && dimensionIn(d, "origin", "foreground") }}},
		SkillLifecycleMetric: {{"skills_created", UserCreatedSkill}},
		DelegationRunMetric:  {{"delegation", func(map[string]string) bool { return true }}},
		CronRunMetric:        {{"cron", func(d map[string]string) bool { return dimensionIn(d, "outcome", "success", "failed") }}},
		CuratorRunMetric:     {{"curator", func(d map[string]string) bool { return success(d) && dimensionIn(d, "trigger", "manual") }}},
		TaskStartedMetric: {
			{"gateway_platform", func(d map[string]string) bool { p, ok := d["platform"]; return ok && p != "none" }},
			{"desktop", func(d map[string]string) bool { return dimensionIn(d, "execution_surface", taskSurface["desktop"]) }},
			{"tui", func(d map[string]string) bool { return dimensionIn(d, "execution_surface", taskSurface["tui"]) }},
		},
		ToolUsageMetric: {
			{"mcp", toolUsed("", "mcp")}, {"plugins", toolUsed("", "plugin")}, {"browser", toolUsed("browser_")},
			{"voice", toolUsed("", "text_to_speech")}, {"kanban", toolUsed("kanban_")}, {"projects", toolUsed("", "desktop_project")},
		},
		SlashCommandMetric: {
			{"voice", func(d map[string]string) bool { return dimensionIn(d, "command", "voice") }},
			{"kanban", func(d map[string]string) bool { return dimensionIn(d, "command", "kanban") }},
		},
	}
	for _, feature := range Features.Sorted() {
		rules[FeatureUsedMark] = append(rules[FeatureUsedMark], featureRule{feature, func(d map[string]string) bool {
			return dimensionIn(d, "feature", feature)
		}})
	}
	return rules
}()

// Features a recorded counter proves were used, in rule order
func FeaturesFor(metricName string, dimensions map[string]string) []string {
	var out []string
	for _, rule := range featureRules[metricName] {
		if rule.used(dimensions) {
			out = append(out, rule.feature)
		}
	}
	return out
}

// Bucketed age of the owning profile's first session, unknown when state.db is unreadable
func DaysSinceInstallBucket(ctx context.Context, homeDir string) string {
	first, err := firstSessionStartedAt(ctx, homeDir)
	if err != nil {
		return "unknown"
	}
	age, ok := nonNegativeNumber(pyTime() - first)
	if !ok {
		return "unknown"
	}
	return bucket(age, daysSinceInstallThresholds, "gte_90d")
}

// First use fact for a feature no counter sees, never stored as a counter row itself
func FeatureUsedCounter(event *RelayEvent) (Counter, bool) {
	if !validShape(event, markShape...) || eventText(event, "name") != FeatureUsedMark {
		return Counter{}, false
	}
	data, ok := asDict(eventAttr(event, "data"))
	if !ok || len(data) != 1 {
		return Counter{}, false
	}
	feature, isText := data["feature"].(string)
	if !isText || !Features.Has(feature) {
		return Counter{}, false
	}
	return Counter{Metric: FeatureUsedMark, Dimensions: map[string]string{"feature": feature}}, true
}

// Reports one real use of a feature under the given profile home, never panicking
func RecordFeatureUsed(ctx context.Context, feature string, homeDir string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Shared-metrics feature use not recorded", "error", fmt.Sprint(r))
		}
	}()
	if homeDir != "" {
		ctx = home.WithOverride(ctx, homeDir)
	}
	if Features.Has(feature) && relayEnabled(ctx) {
		recordProcessMark(ctx, FeatureUsedMark, map[string]string{"feature": feature})
	}
}
