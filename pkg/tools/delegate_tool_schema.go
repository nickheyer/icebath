package tools

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
)

const (
	delegationDeliveryIndependent = "each ungrouped task / `group` returns on its own"
	delegationDeliveryJoined      = "one message per call"
	oneshotDescriptionDelivery    = "Parallel children are joined and their results return in this tool call.\n\n"
	delegateDescriptionDelivery   = "Sessions without a later-result consumer (including one-shot CLI and cron) join parallel children " +
		"and return results in this tool call. " +
		"Otherwise runs in the background: dispatch returns live transcript paths and results re-enter " +
		"as a new message when subagents finish (%s). Background results are delivered only " +
		"BETWEEN your turns: finish whatever does not depend on them, then give a one-line status and END YOUR TURN. Never " +
		"wait or poll on transcripts, artifact files, or CI for a child. " +
		"While children run, `action` (list/steer/stop) controls them live.\n\n"
	delegateDescriptionHead = "Spawn isolated subagents for parallel or context-heavy work. Each gets its own conversation, terminal " +
		"session, and toolset, and only its final summary returns to you. Pass every task in `tasks` — one entry spawns one subagent, several run in parallel " +
		"(limit in the tasks description).\n\n" +
		delegateDescriptionDelivery +
		"USE FOR: reasoning-heavy subtasks, work that would flood your context, or independent parallel workstreams.\n" +
		"DO NOT USE FOR (use these instead):\n" +
		"- Mechanical multi-step work with no reasoning needed -> execute_code\n" +
		"- A single tool call -> call the tool directly\n" +
		"- Tasks needing user interaction -> subagents cannot ask questions\n" +
		"- Durable work that must survive this session -> cronjob or terminal(background=True, notify=True); /stop, /new, " +
		"or process exit halts running subagents (whole tree); each returns an 'interrupted' completion with partial output.\n\n" +
		"RULES:\n" +
		"- Children know nothing of this conversation: pass everything needed via 'context', including any required " +
		"output language, tone, or style (e.g. \"respond in Chinese\").\n" +
		"- Child summaries are SELF-REPORTS, not verified facts: a child claiming \"uploaded successfully\" or " +
		"\"file written\" may be wrong. For external side effects (uploads, remote writes, publishing), require a " +
		"verifiable handle (URL, ID, absolute path) and verify it yourself before telling the user the operation " +
		"succeeded.\n" +
		"- Children cannot close tracked work: a child asked to close it returns findings instead; " +
		"the parent applies the transition.\n"
	delegateDescriptionTail = "- Children inherit the parent model unless pinned via delegation.provider / delegation.model in config.yaml."
)

// OneshotDescriptionDelivery exports the joined delivery paragraph of a one shot delegate_task
const OneshotDescriptionDelivery = oneshotDescriptionDelivery

// Arguments of tools/delegate_tool.py delegate_task, nil fields standing for Python None
type DelegateTaskRequest struct {
	Goal, Context, Tasks, MaxIterations, Role         any
	Background                                        *bool
	OutputSchema, Images, Action, SubagentID, Message any
	ParentAgent                                       any
	CredentialsCfg                                    map[string]any
}

// delegate_task entry point the tools delegation slice seven assigns at init
var DelegateTask func(ctx context.Context, env *Env, req DelegateTaskRequest) string

// Delegation has no external requirements
func checkDelegateRequirements(*Env) bool { return true }

func delegateParam(typ, description string, extra map[string]any) map[string]any {
	out := map[string]any{"type": typ}
	for k, v := range extra {
		out[k] = v
	}
	out["description"] = description
	return out
}

// Static delegate_task schema whose descriptions every definition build rewrites to the user limits
var DelegateTaskSchema = map[string]any{
	"name": "delegate_task",
	"description": "Spawn one or more subagents in isolated contexts. " +
		"Description is rebuilt at every get_definitions() call to reflect the user's current delegation limits.",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tasks": map[string]any{
				"type":     "array",
				"minItems": 1,
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"goal": delegateParam("string",
							"What this subagent should accomplish. Be specific and self-contained — it knows "+
								"nothing about your conversation history.", nil),
						"context": delegateParam("string",
							"Background THIS child needs: file paths, error messages, constraints. Each child "+
								"sees only its own context — repeat shared background in every task that needs it.", nil),
						"output_schema": delegateParam("object",
							"Optional JSON Schema this child's final answer must validate against (told to the "+
								"child up front; parent validates with one bounded correction retry; result gains "+
								"schema_valid, plus schema_errors on failure — the child's raw text is still returned "+
								"as summary, never discarded). Keep it forgiving — require only fields you will read.", nil),
						"images": delegateParam("array",
							"Optional images this child must SEE (max 8): local file paths or http(s) URLs — e.g. a "+
								"screenshot the user sent, a design mock, a chart. Vision-capable children receive the "+
								"pixels on their first turn; non-vision children get path hints for vision_analyze. Text "+
								"files do NOT belong here — put paths in 'context' instead.",
							map[string]any{"items": map[string]any{"type": "string"}}),
						"group": delegateParam("string",
							"Optional result-delivery bucket within this call (only when delegation.independent_completions "+
								"is enabled; otherwise the whole call returns as one message). Tasks sharing a group return "+
								"together in ONE message; ungrouped tasks return individually as each finishes. This does not "+
								"order execution; if B needs A's output, dispatch B after A returns.", nil),
					},
					"required": []any{"goal"},
				},
				"description": "(rebuilt at get_definitions() time)",
			},
			"action": delegateParam("string",
				"Default 'spawn'. Live control of running children: "+
					"'list' = ids/goals/status/transcripts; 'steer' = queue "+
					"course-correction text into one child (subagent_id + "+
					"message) without stopping it; 'stop' = end one child "+
					"early (subagent_id; partial result still returns). "+
					"Control actions return immediately; goal/tasks are ignored unless spawning.",
				map[string]any{"enum": []any{"spawn", "list", "steer", "stop"}}),
			"subagent_id": delegateParam("string", "Target for action='steer'/'stop' (ids from the spawn response or action='list').", nil),
			"message": delegateParam("string",
				"For action='steer': the course correction, appended to "+
					"the child's next tool result mid-run. Be directive and specific.", nil),
		},
		"required": []any{},
	},
}

// Description holding only guidance stated nowhere else in the schema
func buildDelegateTopLevelDescription(ctx context.Context, independentCompletions *bool) string {
	restrictions := "- Children cannot call delegate_task, clarify, memory, or cronjob.\n"
	if depth := MaxSpawnDepth(ctx); depth >= 2 && OrchestratorEnabled(ctx) {
		restrictions = "- Children cannot call clarify, memory, or cronjob.\n" +
			fmt.Sprintf("- Children can themselves delegate while depth remains (max_spawn_depth=%d); the ", MaxSpawnDepth(ctx)) +
			"runtime derives this from depth automatically.\n"
	}
	independent := false
	if independentCompletions != nil {
		independent = *independentCompletions
	} else {
		independent = IndependentCompletions(ctx)
	}
	delivery := delegationDeliveryJoined
	if independent {
		delivery = delegationDeliveryIndependent
	}
	return fmt.Sprintf(delegateDescriptionHead, delivery) + restrictions + delegateDescriptionTail
}

// Tasks parameter description carrying the current concurrency limit
func buildDelegateTasksParamDescription(ctx context.Context) string {
	return fmt.Sprintf("The task(s), up to %d in parallel for this user (set "+
		"via delegation.max_concurrent_children). Each entry spawns one "+
		"subagent with isolated context and terminal session; a single task "+
		"is a one-entry array. Required when spawning.", MaxConcurrentChildren(ctx))
}

// Per build description and parameters reflecting the user limits and the active provider parameters
func buildDelegateDynamicSchemaOverrides(env *Env) map[string]any {
	ctx := delegationEnvContext(env)
	independent := IndependentCompletions(ctx)
	static := DelegateTaskSchema["parameters"].(map[string]any)
	params := make(map[string]any, len(static))
	for k, v := range static {
		params[k] = v
	}
	props := map[string]any{}
	for k, v := range static["properties"].(map[string]any) {
		cp := map[string]any{}
		for kk, vv := range v.(map[string]any) {
			cp[kk] = vv
		}
		props[k] = cp
	}
	params["properties"] = props
	tasks := props["tasks"].(map[string]any)
	tasks["description"] = buildDelegateTasksParamDescription(ctx)
	if !independent {
		items := maps.Clone(tasks["items"].(map[string]any))
		itemProps := map[string]any{}
		for k, v := range items["properties"].(map[string]any) {
			if k != "group" {
				itemProps[k] = v
			}
		}
		items["properties"] = itemProps
		tasks["items"] = items
	}
	extra, addendum := SchemaExtension(ctx, LoadDelegationConfig(ctx))
	if len(extra) > 0 {
		items := maps.Clone(tasks["items"].(map[string]any))
		itemProps := maps.Clone(items["properties"].(map[string]any))
		for k, v := range extra {
			itemProps[k] = v
		}
		items["properties"] = itemProps
		tasks["items"] = items
	}
	description := buildDelegateTopLevelDescription(ctx, &independent)
	if addendum != "" {
		description += "\n" + addendum
	}
	return map[string]any{"description": description, "parameters": params}
}

// Exported buildDelegateDynamicSchemaOverrides
func BuildDelegateDynamicSchemaOverrides(env *Env) map[string]any {
	return buildDelegateDynamicSchemaOverrides(env)
}

var liveControlParams = map[string]bool{"action": true, "subagent_id": true, "message": true}

// Built delegate_task definition without live control for a one shot session whose children join in the call
func OneshotDelegateFunction(function map[string]any) map[string]any {
	description, _ := function["description"].(string)
	for _, delivery := range []string{delegationDeliveryIndependent, delegationDeliveryJoined} {
		description = strings.ReplaceAll(description, fmt.Sprintf(delegateDescriptionDelivery, delivery), oneshotDescriptionDelivery)
	}
	params := function["parameters"].(map[string]any)
	props := map[string]any{}
	for k, v := range params["properties"].(map[string]any) {
		if !liveControlParams[k] {
			props[k] = v
		}
	}
	out := maps.Clone(function)
	out["description"] = description
	newParams := maps.Clone(params)
	newParams["properties"] = props
	out["parameters"] = newParams
	return out
}

// Agent depth a parent exposes so an orchestrator child keeps its workers in its own turn
type delegateDepthAgent interface{ DelegateDepth() int }

// Background for the model facing registry path, false only under an orchestrator subagent
func modelBackgroundValue(parent any) bool {
	if a, ok := parent.(delegateDepthAgent); ok {
		return !(a.DelegateDepth() > 0)
	}
	return true
}

var modelHiddenTaskFields = []string{"acp_command", "acp_args"}

// Model supplied tasks without trusted config only fields, the same list back when nothing changed
func stripModelHiddenTaskFields(tasks any) any {
	list, ok := tasks.([]any)
	if !ok {
		return tasks
	}
	hidden := slices.ContainsFunc(list, func(t any) bool {
		m, ok := t.(map[string]any)
		return ok && slices.ContainsFunc(modelHiddenTaskFields, func(k string) bool { _, has := m[k]; return has })
	})
	if !hidden {
		return tasks
	}
	out := make([]any, len(list))
	for i, t := range list {
		m, ok := t.(map[string]any)
		if !ok {
			out[i] = t
			continue
		}
		cp := map[string]any{}
		for k, v := range m {
			if !slices.Contains(modelHiddenTaskFields, k) {
				cp[k] = v
			}
		}
		out[i] = cp
	}
	return out
}

// StripModelHiddenTaskFields exports stripModelHiddenTaskFields
func StripModelHiddenTaskFields(tasks any) any { return stripModelHiddenTaskFields(tasks) }

func init() {
	Register(Def{
		Name:    "delegate_task",
		Toolset: "delegation",
		Schema:  DelegateTaskSchema,
		Handler: func(ctx context.Context, call *Call) string {
			args := call.Args
			background := modelBackgroundValue(nil)
			return DelegateTask(ctx, call.Env, DelegateTaskRequest{
				Goal: args["goal"], Context: args["context"], Tasks: stripModelHiddenTaskFields(args["tasks"]),
				MaxIterations: args["max_iterations"], Role: args["role"], Background: &background,
				OutputSchema: args["output_schema"], Images: args["images"], Action: args["action"],
				SubagentID: args["subagent_id"], Message: args["message"], ParentAgent: ParentAgentFrom(ctx),
			})
		},
		Check:         checkDelegateRequirements,
		Emoji:         "\U0001F500",
		DynamicSchema: buildDelegateDynamicSchemaOverrides,
	})
}
