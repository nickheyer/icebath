package slash

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/approvals"
	"github.com/nickheyer/icebath/internal/slash"
)

type Agent interface {
	Title(sessionID, title string) (string, error)
	Retry(sessionID string) error
	Undo(sessionID string, count int) (string, error)
	Approve(sessionID string, answer approvals.Answer) (int, error)
	Deny(sessionID string, answer approvals.Answer) (int, error)
	Yolo(sessionID string) (string, error)
	Confirm(ctx context.Context, sessionID, command, detail string) (bool, string, error)
	Model(ctx context.Context, sessionID, model, provider, effort string, reset, global bool) (string, error)
	Reasoning(sessionID, level string, global bool) (string, error)
	Status(sessionID string) (string, error)
	Approvals(sessionID, argument string) (string, error)
	Profile(sessionID, name string) (string, error)
	Diff(ctx context.Context, sessionID, argument string) (string, error)
	Personality(sessionID, name string) (string, error)
	Usage(ctx context.Context, sessionID, argument string) (string, error)
	RefreshModels(ctx context.Context, sessionID string) error
	Compact(ctx context.Context, sessionID, argument string) (string, error)
	Context(ctx context.Context, sessionID, argument string) (string, error)
	Agents(ctx context.Context, sessionID, argument string) (string, error)
	Delegation(sessionID, argument string) (string, error)
	Review(ctx context.Context, sessionID, instructions string) (string, error)
	SteerMessage(ctx context.Context, sessionID, text string) (string, error)
	Busy(ctx context.Context, sessionID, argument string) (string, error)
	Login(ctx context.Context, sessionID, provider, login string) (string, error)
}

type commands struct {
	registry *slash.Registry
	agent    Agent
}

func Register(registry *slash.Registry, agent Agent) {
	registry.Register(commands{registry: registry, agent: agent})
}

func (c commands) Commands() []slash.Command {
	return []slash.Command{
		{Name: "help", Usage: "[skills|filter]", Help: "List commands; /help skills lists skill commands, /help <text> filters", Run: c.help},
		{Name: "title", Usage: "[title]", Help: "Show or set the session title", Run: c.title},
		{Name: "retry", Help: "Regenerate the last reply", Run: c.retry},
		{Name: "undo", Usage: "[n]", Help: "Remove the last message, or last n, and the replies, to edit and resend", Run: c.undo},
		{Name: "approve", Usage: "[session|always] [all]", Help: "Approve a pending dangerous command once, for this session or always; all answers every one waiting", Run: c.approve},
		{Name: "deny", Usage: "[all] [reason]", Help: "Deny a pending dangerous command (optionally with a reason); all denies every one waiting", Run: c.deny},
		{Name: "yolo", Help: "Approvals off for this session, or on again; /approvals sets the lasting mode", Run: c.yolo},
		{Name: "status", Help: "Show session info", Run: c.status},
		{Name: "reasoning", Usage: "[level|reset] [--global]", Help: "Show or set the reasoning effort", Run: c.reasoning},
		{Name: "approvals", Usage: "[manual|smart|off] [--session]", Help: "Whether the agent asks before risky commands, for good or --session", Run: c.approvals},
		{Name: "diff", Usage: "[staged|all|session] [--stat] [path...]",
			Help: "Show git changes in the working directory; session shows everything changed since the oldest checkpoint", Run: c.diff},
		{Name: "version", Aliases: []string{"v"}, Help: "Show the icebath version", Run: c.version},
		{Name: "profile", Usage: "[name]", Help: "Show the profile this session runs on, or switch it to another", Run: c.profile},
		{Name: "model", Usage: "[model|reset] [--provider name] [--reasoning level] [--login name] [--global|--session] [--refresh]",
			Help: "Show or switch the model; --login holds the session to one login of the provider, or @rotate to its rotation", Run: c.model},
		{Name: "personality", Usage: "[name|none]", Help: "List the personalities, or set the one every session of this profile speaks with", Run: c.personality},
		{Name: "usage", Usage: "[reset [--force]]", Help: "Show token usage and rate limits; reset redeems a banked Codex limit reset", Run: c.usage},
		{Name: "compress", Aliases: []string{"compact"}, Usage: "[here [N] | focus topic] [--preview] [--aggressive]",
			Help: "Compress conversation context (add 'here [N]' to keep recent N turns; --aggressive keeps only the last exchange under the shortest " +
				"summary; --preview shows what would happen)", Run: c.compress},
		{Name: "context", Aliases: []string{"ctx"}, Usage: "[all]",
			Help: "Show detailed context window view with usage gauge, category breakdown, compression stats, and throughput", Run: c.context},
		{Name: "agents", Aliases: []string{"tasks"}, Usage: "[pause|resume|status]",
			Help: "Show active agents and running tasks; pause or resume new subagents, or show how delegation stands", Run: c.agents},
		{Name: "delegation", Usage: "[backend]", Help: "List the delegation backends, or set the one delegate_task runs subagents on", Run: c.delegation},
		{Name: "review", Usage: "[review instructions]", Help: "Spawn an independent subagent to review the work just discussed (PR, code, docs)", Run: c.review},
		{Name: "steer", Aliases: []string{"s"}, Usage: "<prompt>", Help: "Inject a message after the next tool call without interrupting", Run: c.steer},
		{Name: "busy", Usage: "[queue|steer|interrupt|status]", Help: "Control what enter does while a turn runs", Run: c.busy},
	}
}

func (c commands) agents(ctx context.Context, call *slash.Call) (string, error) {
	return c.agent.Agents(ctx, call.SessionID, call.Argument)
}

func (c commands) delegation(_ context.Context, call *slash.Call) (string, error) {
	return c.agent.Delegation(call.SessionID, call.Argument)
}

func (c commands) review(ctx context.Context, call *slash.Call) (string, error) {
	return c.agent.Review(ctx, call.SessionID, call.Argument)
}

func (c commands) steer(ctx context.Context, call *slash.Call) (string, error) {
	return c.agent.SteerMessage(ctx, call.SessionID, call.Argument)
}

func (c commands) busy(ctx context.Context, call *slash.Call) (string, error) {
	return c.agent.Busy(ctx, call.SessionID, call.Argument)
}

func (c commands) usage(ctx context.Context, call *slash.Call) (string, error) {
	return c.agent.Usage(ctx, call.SessionID, call.Argument)
}

func (c commands) personality(_ context.Context, call *slash.Call) (string, error) {
	return c.agent.Personality(call.SessionID, call.Argument)
}

func (c commands) profile(_ context.Context, call *slash.Call) (string, error) {
	return c.agent.Profile(call.SessionID, call.Argument)
}

// Skill commands /help lists before it points at /help skills for the rest
const listedSkills = 10

func (c commands) help(_ context.Context, call *slash.Call) (string, error) {
	filter := strings.TrimSpace(call.Argument)
	var out strings.Builder
	line := func(command slash.Command) string {
		usage := strings.TrimSpace("/" + command.Name + " " + command.Usage)
		return fmt.Sprintf("- `%s` %s\n", usage, command.Help)
	}
	var core, skills []slash.Command
	for _, command := range c.registry.Commands() {
		if command.Skill {
			skills = append(skills, command)
		} else {
			core = append(core, command)
		}
	}
	switch {
	case strings.EqualFold(filter, "skills"):
		if len(skills) == 0 {
			return "No skill commands are installed.", nil
		}
		fmt.Fprintf(&out, "⚡ **Skill Commands** (%d active):\n\n", len(skills))
		for _, command := range skills {
			out.WriteString(line(command))
		}
		return out.String(), nil
	case filter != "":
		for _, command := range append(core, skills...) {
			if text := line(command); strings.Contains(text, filter) {
				out.WriteString(text)
			}
		}
		if out.Len() == 0 {
			return "No commands match " + filter, nil
		}
		return out.String(), nil
	}
	for _, command := range core {
		out.WriteString(line(command))
	}
	if len(skills) > 0 {
		fmt.Fprintf(&out, "\n⚡ **Skill Commands** (%d active):\n\n", len(skills))
		for _, command := range skills[:min(listedSkills, len(skills))] {
			out.WriteString(line(command))
		}
		if len(skills) > listedSkills {
			fmt.Fprintf(&out, "\n... and %d more. Use `/help skills` for the full list.\n", len(skills)-listedSkills)
		}
	}
	return out.String(), nil
}

func (c commands) title(_ context.Context, call *slash.Call) (string, error) {
	return c.agent.Title(call.SessionID, call.Argument)
}

func (c commands) retry(_ context.Context, call *slash.Call) (string, error) {
	return "", c.agent.Retry(call.SessionID)
}

func (c commands) undo(ctx context.Context, call *slash.Call) (string, error) {
	count := 1
	if call.Argument != "" {
		var err error
		if count, err = strconv.Atoi(call.Argument); err != nil || count < 1 {
			return "", errors.New("Usage: /undo [n]")
		}
	}
	detail := "This removes the last user/assistant exchange from history."
	if count != 1 {
		detail = fmt.Sprintf("This removes the last %d user turns from history.", count)
	}
	ok, note, err := c.agent.Confirm(ctx, call.SessionID, "undo", detail)
	switch {
	case err != nil:
		return "", err
	case !ok:
		return note, nil
	}
	hint, err := c.agent.Undo(call.SessionID, count)
	if err != nil {
		return "", err
	}
	removed := "Removed the last message."
	if count != 1 {
		removed = fmt.Sprintf("Removed the last %d messages.", count)
	}
	if hint != "" {
		removed += "\n\n" + hint
	}
	return removed + note, nil
}

// Words /approve takes and the answer each one gives
var scopes = map[string]string{"always": approvals.Always, "permanent": approvals.Always, "permanently": approvals.Always, "session": approvals.Session,
	"ses": approvals.Session}

func (c commands) approve(_ context.Context, call *slash.Call) (string, error) {
	answer := approvals.Answer{Choice: approvals.Once}
	for _, word := range strings.Fields(strings.ToLower(call.Argument)) {
		switch scope := scopes[word]; {
		case word == "all":
			answer.All = true
		case scope == approvals.Always || scope == approvals.Session && answer.Choice != approvals.Always:
			answer.Choice = scope
		}
	}
	count, err := c.agent.Approve(call.SessionID, answer)
	if err != nil {
		return "", err
	}
	scope := map[string]string{approvals.Once: "", approvals.Session: " (pattern approved for this session)", approvals.Always: " (pattern approved permanently)"}[answer.Choice]
	if count == 1 {
		return "✅ Command approved" + scope + ". The agent is resuming...", nil
	}
	return fmt.Sprintf("✅ Commands approved%s (%d commands). The agent is resuming...", scope, count), nil
}

func (c commands) deny(_ context.Context, call *slash.Call) (string, error) {
	reason := strings.TrimSpace(call.Argument)
	words := strings.Fields(reason)
	all := len(words) > 0 && strings.ToLower(words[0]) == "all"
	if all {
		reason = strings.TrimSpace(reason[len(words[0]):])
	}
	count, err := c.agent.Deny(call.SessionID, approvals.Answer{All: all, Reason: reason})
	switch {
	case err != nil:
		return "", err
	case count == 1 && reason != "":
		return fmt.Sprintf("❌ Command denied. Reason relayed to the agent: \"%s\"", reason), nil
	case reason != "":
		return fmt.Sprintf("❌ Commands denied (%d commands). Reason relayed to the agent: \"%s\"", count, reason), nil
	case count == 1:
		return "❌ Command denied.", nil
	}
	return fmt.Sprintf("❌ Commands denied (%d commands).", count), nil
}

func (c commands) yolo(_ context.Context, call *slash.Call) (string, error) {
	return c.agent.Yolo(call.SessionID)
}

func (c commands) status(_ context.Context, call *slash.Call) (string, error) {
	return c.agent.Status(call.SessionID)
}

func (c commands) reasoning(_ context.Context, call *slash.Call) (string, error) {
	words := strings.Fields(call.Argument)
	global := slices.Contains(words, "--global")
	if words = slices.DeleteFunc(words, func(word string) bool { return word == "--global" }); len(words) > 1 {
		return "", errors.New("Usage: /reasoning [level|reset] [--global]")
	}
	return c.agent.Reasoning(call.SessionID, strings.Join(words, ""), global)
}

func (c commands) approvals(_ context.Context, call *slash.Call) (string, error) {
	return c.agent.Approvals(call.SessionID, call.Argument)
}

func (c commands) diff(ctx context.Context, call *slash.Call) (string, error) {
	return c.agent.Diff(ctx, call.SessionID, call.Argument)
}

func (c commands) version(context.Context, *slash.Call) (string, error) {
	version := "(devel)"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		version = info.Main.Version
	}
	return "icebath " + version + " " + runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH, nil
}

func (c commands) model(ctx context.Context, call *slash.Call) (string, error) {
	const usage = "Usage: /model [model|reset] [--provider name] [--reasoning level] [--login name] [--global|--session] [--refresh]"
	var model, provider, effort, login string
	var reset, global, refresh bool
	words := strings.Fields(call.Argument)
	for i := 0; i < len(words); i++ {
		switch word := words[i]; {
		case word == "--global":
			global = true
		case word == "--session":
			global = false
		case word == "--refresh":
			refresh = true
		case word == "--provider" || word == "--reasoning" || word == "--login":
			if i++; i == len(words) {
				return "", errors.New(usage)
			}
			switch word {
			case "--provider":
				provider = words[i]
			case "--login":
				login = words[i]
			default:
				effort = words[i]
			}
		case strings.HasPrefix(word, "--") || model != "" || reset:
			return "", errors.New(usage)
		case word == "reset":
			reset = true
		default:
			model = word
		}
	}
	if refresh {
		if err := c.agent.RefreshModels(ctx, call.SessionID); err != nil {
			return "", err
		}
	}
	if login != "" && model == "" && effort == "" && !reset {
		// Only the login changes: on the provider named, else the session's own
		return c.agent.Login(ctx, call.SessionID, provider, login)
	}
	out, err := c.agent.Model(ctx, call.SessionID, model, provider, effort, reset, global)
	if err != nil || login == "" {
		return out, err
	}
	note, err := c.agent.Login(ctx, call.SessionID, provider, login)
	if err != nil {
		return "", err
	}
	return out + "\n\n" + note, nil
}

func (c commands) compress(ctx context.Context, call *slash.Call) (string, error) {
	return c.agent.Compact(ctx, call.SessionID, call.Argument)
}

func (c commands) context(ctx context.Context, call *slash.Call) (string, error) {
	return c.agent.Context(ctx, call.SessionID, call.Argument)
}
