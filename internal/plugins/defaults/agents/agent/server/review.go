package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/internal/auxiliary"
	auxiliaryconfig "github.com/nickheyer/icebath/internal/auxiliary/config"
	"github.com/nickheyer/icebath/internal/delegation"
	"github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/tools"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Helper model task the reviewer of /review runs on, its own provider, model or endpoint unset leaving it on the session's model
const reviewTask = "review"

type reviews struct{}

func (reviews) Tasks() []auxiliary.Task {
	return []auxiliary.Task{{Name: reviewTask, Title: "Reviews", About: "The model that reviews work when you use /review", Order: 60}}
}

const (
	// Messages of the conversation the reviewer is shown, and the characters of each
	reviewMessages = 10
	reviewChars    = 12000
	// Skills the parent worked under the reviewer is told to load
	reviewSkills = 8
	reviewGoal   = "Act as an independent senior reviewer. Thoroughly review the work presented in the conversation excerpt provided in your context: " +
		"investigate any code, pull request, branch, commit, documentation, design, or other artifact it references (open the PR, read the diff, run " +
		"the code or tests where feasible) rather than judging from the excerpt alone. Produce a full, structured review: what the work does, whether " +
		"it is correct and complete, concrete defects or risks found (with file/line references where possible), what was verified vs. only read, and " +
		"a clear final verdict with recommended next steps."
)

// Starts an independent subagent reviewing the work the conversation just discussed, its review re-entering the conversation once done;
// what the user is told
func (a *Agent) Review(ctx context.Context, id, instructions string) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	store, err := a.store()
	if err != nil {
		return "", err
	}
	history, err := store.Messages(ctx, r.sessionID(), sessions.Model)
	if err != nil {
		return "", err
	}
	snapshot := recentConversation(history)
	if len(snapshot) == 0 {
		return "", errors.New("There is nothing to review yet, because this session has no messages.")
	}
	r.mu.Lock()
	launched := append([]string{}, r.skills...)
	r.mu.Unlock()
	goal, brief := reviewBrief(snapshot, instructions, loadedSkills(launched, history))
	session, err := store.Get(ctx, r.sessionID())
	if err != nil {
		return "", err
	}
	settings := a.settings(r)
	env := &tools.Env{Cwd: session.GetDirectory(), Config: settings, OneShot: r.oneshot, Client: r.shell(), Platform: platformOf(session),
		Notify: func(notice tools.Notice) error { return a.deliver(r, notice) }}
	_, route, _ := a.current(r)
	env.Aux = a.helpers(r, settings, route)
	task := auxiliary.Task{Name: reviewTask}
	pinned := &delegation.Route{Provider: strings.TrimSpace(settings.GetString(task.Key(auxiliaryconfig.Provider))),
		Model: strings.TrimSpace(settings.GetString(task.Key(auxiliaryconfig.Model))), BaseURL: strings.TrimSpace(settings.GetString(task.Key(auxiliaryconfig.BaseURL))),
		APIKey: strings.TrimSpace(settings.GetString(task.Key(auxiliaryconfig.APIKey))), APIMode: strings.TrimSpace(settings.GetString(task.Key(auxiliaryconfig.APIMode)))}
	if p := strings.ToLower(pinned.Provider); p == auxiliaryconfig.Auto || p == auxiliaryconfig.Main {
		pinned.Provider = ""
	}
	if pinned.Provider == "" && pinned.Model == "" && pinned.BaseURL == "" {
		pinned = nil
	}
	call := &tools.Call{Name: "delegate_task", SessionID: r.sessionID(), Env: env, Args: map[string]any{"goal": goal, "context": brief}}
	raw := a.delegateWith(ctx, r, call, pinned, true)
	var result map[string]any
	if json.Unmarshal([]byte(raw), &result) != nil {
		return "", fmt.Errorf("The review could not start. The subagent answered: %s", strs.Head(strings.TrimSpace(raw), 300))
	}
	if reason, _ := result["error"].(string); reason != "" {
		return "", errors.New(reason)
	}
	if result["status"] == "dispatched" {
		return "The review started. Its findings come back to this session when it is done.", nil
	}
	model := ""
	if pinned != nil {
		model = pinned.Model
	}
	note := ""
	if model != "" {
		note = " on " + model
	}
	if focus := strings.TrimSpace(instructions); focus != "" {
		note += ", focused on “" + focus + "”"
	}
	body := any(result)
	if results, ok := result["results"]; ok {
		body = results
	}
	dumped, _ := json.Marshal(body)
	return "The review is done" + note + ".\n\n```json\n" + strs.Head(string(dumped), 4000) + "\n```", nil
}

// Last user and assistant messages with text, oldest first, each cut to the characters a reviewer gets
func recentConversation(history []*sessionsv1.Message) [][2]string {
	var out [][2]string
	for i := len(history) - 1; i >= 0 && len(out) < reviewMessages; i-- {
		m := history[i]
		role := ""
		switch {
		case m.GetRole() == sessionsv1.Role_ROLE_USER && m.GetDisplay() != "hidden":
			role = "user"
		case m.GetRole() == sessionsv1.Role_ROLE_ASSISTANT:
			role = "assistant"
		default:
			continue
		}
		text := strings.TrimSpace(m.GetText())
		if len(m.GetMedia()) > 0 {
			text = strings.TrimSpace(text + "\n[image]")
		}
		if text == "" {
			continue
		}
		if len([]rune(text)) > reviewChars {
			text = strs.Head(text, reviewChars) + "\n[... truncated ...]"
		}
		out = append(out, [2]string{role, text})
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

var preloadedSkill = regexp.MustCompile(`with the "([^"]+)" skill\s+preloaded`)

// Skills the parent worked under: those its launch loaded first, then those it viewed, each once, at most the cap
func loadedSkills(launched []string, history []*sessionsv1.Message) []string {
	candidates := append([]string{}, launched...)
	for _, m := range history {
		if m.GetRole() == sessionsv1.Role_ROLE_USER {
			for _, found := range preloadedSkill.FindAllStringSubmatch(m.GetSent(), -1) {
				candidates = append(candidates, found[1])
			}
		}
		if m.GetRole() != sessionsv1.Role_ROLE_ASSISTANT {
			continue
		}
		for _, call := range m.GetToolCalls() {
			if call.GetName() != "skill_view" {
				continue
			}
			var args struct {
				Name     string `json:"name"`
				FilePath string `json:"file_path"`
			}
			if json.Unmarshal([]byte(cmp.Or(call.GetArguments(), "{}")), &args) == nil && args.FilePath == "" {
				candidates = append(candidates, args.Name)
			}
		}
	}
	var names []string
	for _, name := range candidates {
		if name = strings.TrimSpace(name); name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names[:min(len(names), reviewSkills)]
}

// Goal a viewer shows for the review, and the whole briefing of the reviewer
func reviewBrief(snapshot [][2]string, instructions string, skills []string) (string, string) {
	focus := strings.Join(strings.Fields(instructions), " ")
	goal := "Review recent work"
	if focus != "" {
		goal = "Review: " + focus
	}
	if runes := []rune(goal); len(runes) > 80 {
		goal = strings.TrimRightFunc(string(runes[:79]), unicode.IsSpace) + "…"
	}
	lines := []string{reviewGoal, "", "You were spawned by the /review command. The following is an excerpt of the most recent conversation between the user " +
		"and their primary agent. It is your starting evidence — the work to review is referenced in it.", "", "--- Recent conversation (oldest first) ---"}
	for _, m := range snapshot {
		label := "[PRIMARY AGENT]"
		if m[0] == "user" {
			label = "[USER]"
		}
		lines = append(lines, label, m[1], "")
	}
	lines = append(lines, "--- End of conversation excerpt ---")
	if len(skills) > 0 {
		lines = append(lines, "", "The primary agent was operating under these loaded skills: "+strings.Join(skills, ", ")+". Before reviewing, load each "+
			"with skill_view(name=...) and treat their conventions, invariants, and review standards as binding for your assessment — the work was "+
			"produced under them and must be judged against them.")
	}
	if strings.TrimSpace(instructions) != "" {
		lines = append(lines, "", "Additional review instructions from the user:", strings.TrimSpace(instructions))
	}
	lines = append(lines, "", "Your review is delivered back into that conversation, addressed to the primary agent and its user. Be direct and specific; "+
		"do not soften findings.")
	return goal, strings.Join(lines, "\n")
}
