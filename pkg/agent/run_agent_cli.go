package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

var basicToolsets = map[string]bool{"web": true, "terminal": true, "vision": true, "creative": true, "reasoning": true}

var compositeToolsets = map[string]bool{"research": true, "development": true, "analysis": true, "content_creation": true, "full_stack": true}

const listToolsUsage = `
💡 Usage Examples:
  icebath --enabled_toolsets=research --query='search for Python news'
  icebath --enabled_toolsets=development --query='debug this code'
  icebath --enabled_toolsets=safe --query='analyze without terminal'
  icebath --enabled_toolsets=web,vision --query='analyze website'
  icebath --disabled_toolsets=terminal --query='no command execution'
  icebath --save_trajectories --query='your question here'`

type toolsetListing struct {
	name, description string
	resolved, include []string
}

// Prints basic, composite, scenario and legacy toolsets, every tool and the usage examples
func printToolListing(ctx context.Context, w io.Writer, r *tools.Registry, env *tools.Env) {
	fmt.Fprintln(w, "📋 Available Tools & Toolsets:")
	fmt.Fprintln(w, strings.Repeat("-", 50))
	fmt.Fprintln(w, "\n🎯 Predefined Toolsets (New System):")
	fmt.Fprintln(w, strings.Repeat("-", 40))
	var basic, composite, scenario []toolsetListing
	for _, name := range r.OrderedToolsetNames(ctx, "") {
		ts, ok := r.GetToolset(ctx, "", name, true)
		if !ok {
			continue
		}
		info := toolsetListing{name: name, description: ts.Description, resolved: r.ResolveToolset(ctx, "", name), include: ts.Includes}
		switch {
		case basicToolsets[name]:
			basic = append(basic, info)
		case compositeToolsets[name]:
			composite = append(composite, info)
		default:
			scenario = append(scenario, info)
		}
	}
	joinOrNone := func(items []string) string {
		if len(items) == 0 {
			return "none"
		}
		return strings.Join(items, ", ")
	}
	fmt.Fprintln(w, "\n📌 Basic Toolsets:")
	for _, t := range basic {
		fmt.Fprintf(w, "  • %-15s - %s\n", t.name, t.description)
		fmt.Fprintf(w, "    Tools: %s\n", joinOrNone(t.resolved))
	}
	fmt.Fprintln(w, "\n📂 Composite Toolsets (built from other toolsets):")
	for _, t := range composite {
		fmt.Fprintf(w, "  • %-15s - %s\n", t.name, t.description)
		fmt.Fprintf(w, "    Includes: %s\n", joinOrNone(t.include))
		fmt.Fprintf(w, "    Total tools: %d\n", len(t.resolved))
	}
	fmt.Fprintln(w, "\n🎭 Scenario-Specific Toolsets:")
	for _, t := range scenario {
		fmt.Fprintf(w, "  • %-20s - %s\n", t.name, t.description)
		fmt.Fprintf(w, "    Total tools: %d\n", len(t.resolved))
	}
	fmt.Fprintln(w, "\n📦 Legacy Toolsets (for backward compatibility):")
	available := r.AvailableToolsets(ctx, env)
	for _, name := range r.AvailableToolsetNames(ctx, env) {
		info := available[name]
		mark := "❌"
		if info.Available {
			mark = "✅"
		}
		fmt.Fprintf(w, "  %s %s: %s\n", mark, name, info.Description)
		if !info.Available {
			fmt.Fprintf(w, "    Requirements: %s\n", strings.Join(info.Requirements, ", "))
		}
	}
	all := append([]string(nil), r.AllToolNames(ctx, "")...)
	fmt.Fprintf(w, "\n🔧 Individual Tools (%d available):\n", len(all))
	sort.Strings(all)
	for _, name := range all {
		ts := r.ToolsetForTool(ctx, "", name)
		if ts == "" {
			ts = "None"
		}
		fmt.Fprintf(w, "  📌 %s (from %s)\n", name, ts)
	}
	fmt.Fprintln(w, listToolsUsage)
}

// Comma separated toolset argument as a list echoed under the label, nil when absent
func parseToolsetArg(w io.Writer, raw, label string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	names := make([]string, len(parts))
	for i, p := range parts {
		names[i] = strings.TrimSpace(p)
	}
	fmt.Fprintf(w, "%s: %s\n", label, pyStrListRepr(names))
	return names
}

// Python repr of a list of str
func pyStrListRepr(items []string) string {
	reprs := make([]string, len(items))
	for i, s := range items {
		reprs[i] = pyStrRepr(s)
	}
	return "[" + strings.Join(reprs, ", ") + "]"
}

// Writes one trajectory in the batch runner format to a UUID named sample file
func saveSampleTrajectory(w io.Writer, a *AIAgent, result map[string]any, userQuery, model string) {
	filename := "sample_" + uuid.NewString()[:8] + ".json"
	messages, _ := result["messages"].([]map[string]any)
	completed, _ := result["completed"].(bool)
	entry := pyjson.NewObject()
	entry.Set("conversations", a.convertToTrajectoryFormat(messages, userQuery, completed))
	entry.Set("timestamp", naiveISOFormat(time.Now()))
	entry.Set("model", model)
	entry.Set("completed", result["completed"])
	entry.Set("query", userQuery)
	text, err := pyjson.Dumps(entry, pyjson.EnsureASCII(false), pyjson.Indent(2))
	if err == nil {
		err = os.WriteFile(filename, []byte(text), 0o644)
	}
	if err != nil {
		fmt.Fprintf(w, "\n⚠️ Failed to save sample: %v\n", err)
		return
	}
	fmt.Fprintf(w, "\n💾 Sample trajectory saved to: %s\n", filename)
}

// MainOptions are the direct runner flags, Query nil for the built in example
type MainOptions struct {
	Query                             *string
	Model, APIKey, BaseURL            string
	MaxTurns                          int
	EnabledToolsets, DisabledToolsets string
	ListTools, SaveTrajectories       bool
	SaveSample, Verbose               bool
	LogPrefixChars                    int
	Registry                          *tools.Registry
	Env                               *tools.Env
}

// Runs the agent directly on one query and prints the conversation summary
func Main(ctx context.Context, w io.Writer, o MainOptions) {
	fmt.Fprintln(w, "🤖 AI Agent with Tool Calling")
	fmt.Fprintln(w, strings.Repeat("=", 50))
	if o.ListTools {
		printToolListing(ctx, w, o.Registry, o.Env)
		return
	}
	provider.InstallTrustStore()
	enabled := parseToolsetArg(w, o.EnabledToolsets, "🎯 Enabled toolsets")
	disabled := parseToolsetArg(w, o.DisabledToolsets, "🚫 Disabled toolsets")
	if o.SaveTrajectories {
		fmt.Fprintln(w, "💾 Trajectory saving: ENABLED")
		fmt.Fprintln(w, "   - Successful conversations → trajectory_samples.jsonl")
		fmt.Fprintln(w, "   - Failed conversations → failed_trajectories.jsonl")
	}
	a, err := NewAIAgent(ctx, AIAgentOptions{
		BaseURL: o.BaseURL, Model: o.Model, APIKey: o.APIKey, MaxIterations: o.MaxTurns,
		EnabledToolsets: enabled, DisabledToolsets: disabled, SaveTrajectories: o.SaveTrajectories,
		VerboseLogging: o.Verbose, LogPrefixChars: o.LogPrefixChars,
	})
	if err != nil {
		fmt.Fprintf(w, "❌ Failed to initialize agent: %v\n", err)
		return
	}
	userQuery := "Tell me about the latest developments in Python 3.13 and what new features " +
		"developers should know about. Please search for current information and try it out."
	if o.Query != nil {
		userQuery = *o.Query
	}
	fmt.Fprintf(w, "\n📝 User Query: %s\n", userQuery)
	fmt.Fprintln(w, "\n"+strings.Repeat("=", 50))
	result, err := a.RunConversation(ctx, userQuery, RunConversationOptions{})
	if err != nil {
		panic(err)
	}
	fmt.Fprintln(w, "\n"+strings.Repeat("=", 50)+"\n📋 CONVERSATION SUMMARY\n"+strings.Repeat("=", 50))
	messages, _ := result["messages"].([]map[string]any)
	fmt.Fprintf(w, "✅ Completed: %s\n📞 API Calls: %v\n💬 Messages: %d\n", pyBoolStr(result["completed"]), result["api_calls"], len(messages))
	if final, _ := result["final_response"].(string); final != "" {
		fmt.Fprintln(w, "\n🎯 FINAL RESPONSE:\n"+strings.Repeat("-", 30)+"\n"+final)
	}
	if o.SaveSample {
		saveSampleTrajectory(w, a, result, userQuery, o.Model)
	}
	fmt.Fprintln(w, "\n👋 Agent execution completed!")
}

// Python str of a bool value
func pyBoolStr(v any) string {
	switch v {
	case true:
		return "True"
	case false:
		return "False"
	}
	return fmt.Sprint(v)
}
