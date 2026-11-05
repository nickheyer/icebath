package provider

import "strings"

const planModeRules = "For this turn, you are in PLAN MODE — planning only.\n" +
	"\n" +
	"- Do not implement code.\n" +
	"- Do not edit project files except the plan markdown file itself.\n" +
	"- Do not run mutating terminal commands, commit, push, or perform external\n" +
	"  actions.\n" +
	"- You may inspect the repo or other context with read-only commands/tools\n" +
	"  when needed.\n" +
	"- Your deliverable is a markdown plan saved inside the active workspace under\n" +
	"  `.icebath/plans/YYYY-MM-DD_HHMMSS-<slug>.md` (create the directory if\n" +
	"  needed; Hermes file tools are backend-aware, so this relative path keeps\n" +
	"  the plan with the workspace on local, docker, ssh, modal, and daytona\n" +
	"  backends). If the runtime provides a specific target path, use that exact\n" +
	"  path instead.\n"

const planCraft = "Write the plan for an implementer with zero context for the codebase and\n" +
	"questionable taste. A good plan makes implementation obvious — if someone has\n" +
	"to guess, the plan is incomplete.\n" +
	"\n" +
	"Structure (include the sections that are relevant):\n" +
	"- Goal — one sentence.\n" +
	"- Current context / assumptions.\n" +
	"- Architecture / proposed approach — 2-3 sentences.\n" +
	"- Step-by-step tasks. Each task is bite-sized (2-5 minutes of focused work),\n" +
	"  names exact file paths (`src/models/user.py`, not \"the model file\"),\n" +
	"  includes complete copy-pasteable code where code is needed, and exact\n" +
	"  commands with expected output for verification.\n" +
	"- Tests / validation — for code tasks, follow the TDD cycle per task: write\n" +
	"  the failing test, run it to verify failure, implement minimally, run to\n" +
	"  verify pass, commit.\n" +
	"- Risks, tradeoffs, and open questions.\n" +
	"\n" +
	"Principles: DRY, YAGNI, TDD, frequent commits. Avoid vague tasks (\"add\n" +
	"authentication\"), incomplete code (\"add validation here\"), and unverifiable\n" +
	"steps (\"test it works\" — instead: the exact command and its expected output).\n" +
	"\n" +
	"Interaction style:\n" +
	"- If the request is clear enough, write the plan directly.\n" +
	"- If it is genuinely underspecified, ask a brief clarifying question instead\n" +
	"  of guessing.\n" +
	"- After saving the plan, reply briefly with what you planned and the saved\n" +
	"  path, and offer to execute it (e.g. via subagent-driven development) —\n" +
	"  but do not start executing in this turn.\n"

// BuildPlanPrompt builds the plan mode turn prompt, an empty task asking the agent to infer it from context
func BuildPlanPrompt(task string) string {
	task = strings.TrimSpace(task)
	block := "No explicit task was given with /plan — infer the task from the " +
		"current conversation context (the thing we have been discussing " +
		"or working toward). If the conversation does not imply a task, ask a brief clarifying question.\n"
	if task != "" {
		block = "Task to plan:\n" + task + "\n"
	}
	return "[/plan — plan mode]\n\n" + planModeRules + "\n" + block + "\n" + planCraft
}
