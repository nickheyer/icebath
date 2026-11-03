package harness

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/plugin"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Names of the agents every app offers
const (
	KindGeneral = "general-purpose"
	KindExplore = "Explore"
	KindPlan    = "Plan"
)

// Tools that only read, offered to the exploring and planning agents
var readOnlyTools = []string{"Read", "Glob", "Grep", "Outline", "WebFetch", "ShellOutput", "Record"}

// Tools a subagent never gets, the ones that put questions to the person or steer the main session
var subagentDenied = []string{"AskUser", "AskUserQuestion", "EndConversation", "EnterPlanMode", "ExitPlanMode"}

// Kinds every app starts with, standing in for the built in agents of other harnesses
func builtinKinds() []Kind {
	return []Kind{
		{Name: KindGeneral, Description: "General-purpose agent for researching complex questions, searching for code, and executing multi-step tasks. Use when a task needs several tools and its own transcript", Source: "builtin"},
		{Name: KindExplore, Description: "Read-only search agent for broad fan-out searches across many files, directories and naming conventions, returning the conclusion rather than file dumps. Give it a thoroughness of quick, medium or very thorough", Tools: readOnlyTools, NoInstructions: true, Source: "builtin", Append: "You only read. Locate what was asked and report where it is and what it says, in a short answer."},
		{Name: KindPlan, Description: "Software architect agent for designing implementation plans. Returns step by step plans, the files that matter and the trade offs", Tools: readOnlyTools, NoInstructions: true, Source: "builtin", Append: "You only read. Produce an implementation plan with the ordered steps, the files each touches and the trade offs considered."},
	}
}

// Kind an agent definition makes, its prompt, tools, model, limits and hooks carried over
func kindOfAgent(a *plugin.Agent) Kind {
	k := Kind{Name: a.Name, Description: a.Description, System: a.Prompt, Deny: append([]string(nil), a.DisallowedTools...), MaxRounds: a.MaxTurns, Source: a.File}
	if a.Plugin != nil {
		k.Source = "plugin " + a.Plugin.ID
	}
	for _, t := range a.Tools {
		name, rest, pattern := strings.Cut(t, "(")
		if pattern {
			k.Grants = append(k.Grants, name+"("+rest)
		}
		if name == "Task" {
			name = agent.SubagentTool
		}
		k.Tools = append(k.Tools, name)
	}
	for i, d := range k.Deny {
		name, _, _ := strings.Cut(d, "(")
		if name == "Task" {
			name = agent.SubagentTool
		}
		k.Deny[i] = name
	}
	if len(k.Tools) > 0 {
		k.Deny = nil
		for _, d := range a.DisallowedTools {
			name, _, _ := strings.Cut(d, "(")
			k.Tools = without(k.Tools, name)
		}
	}
	k.Tools = without(k.Tools, agent.SubagentTool+"(")
	k.Model = modelOfAgent(a.Model)
	k.Mode = a.PermissionMode
	if k.Mode == "auto" {
		k.Mode = ""
	}
	k.CompactWindow = a.AutoCompactWindow
	k.Skills = append([]string(nil), a.Skills...)
	k.Memory = a.Memory
	k.Background = a.Background
	k.NoInstructions = a.OmitInstructions
	k.Effort = a.Effort
	k.Isolation = a.Isolation
	k.Color = a.Color
	k.InitialPrompt = a.InitialPrompt
	k.HookConfig = a.Hooks
	k.Servers = a.MCPServers
	k.Problems = append([]string(nil), a.Problems...)
	return k
}

// Names with the dropped one left out
func without(names []string, dropped string) []string {
	out := names[:0]
	for _, n := range names {
		if n != dropped {
			out = append(out, n)
		}
	}
	return out
}

// Target an agent model field names, nil for inherit or none, the alias kept for the endpoint to resolve
func modelOfAgent(model string) *Target {
	model = strings.TrimSpace(model)
	if model == "" || model == "inherit" {
		return nil
	}
	return &Target{Model: model}
}

// Aliases a model field may name in place of an id
var modelAliases = []string{"sonnet", "opus", "haiku", "fable"}

// Model id an alias or id stands for on the endpoint, the newest catalog model of the vendor carrying the alias
func resolveModel(name string, ep Endpoint) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "inherit" {
		return ep.Model, nil
	}
	alias := false
	for _, a := range modelAliases {
		alias = alias || name == a
	}
	if !alias {
		return name, nil
	}
	if strings.Contains(strings.ToLower(ep.Model), name) {
		return ep.Model, nil
	}
	var best profiles.Model
	for _, m := range profiles.Catalog() {
		if !strings.Contains(strings.ToLower(m.ID), name) {
			continue
		}
		if ep.Catalog.Vendor != "" && m.Vendor != "" && m.Vendor != ep.Catalog.Vendor {
			continue
		}
		if best.ID == "" || m.Created > best.Created || m.Created == best.Created && m.ID > best.ID {
			best = m
		}
	}
	if best.ID == "" {
		return "", fmt.Errorf("no model of %s matches the alias %s", ep.Profile.Name, name)
	}
	return best.ID, nil
}
