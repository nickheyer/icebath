package approvals

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"

	approvalsconfig "github.com/nickheyer/icebath/internal/approvals/config"
	"github.com/nickheyer/icebath/internal/auxiliary"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/hooks"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/tools/redact"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Session a decision is for, and what it can reach
type Context struct {
	// Session the answers are remembered for and the prompts shown in
	Session string
	// Settings of its profile
	Config *config.Config
	// Surface the session came from, such as "tui", "cron" or a messaging platform
	Source string
	// The turn's client waits for it alone and leaves once it ends, as a query run of the command line: nobody answers its prompts
	Finite bool
	// Helper models of the session, which the smart reviewer runs on; nil where there are none
	Aux *auxiliary.Client
	// Says what the session is doing while a prompt waits, nil for nothing
	Activity func(text string)
	// Answers the prompts of the session in place of a user, once or deny, for a session nobody is asked in such as a subagent's; nil asks
	// the user
	Answer func(target, description string) string
}

// Surfaces where nobody can answer a prompt, and those where a window to show it in still means nobody answers
var (
	unattendedPlatforms = strs.Set("webhook", "msgraph_webhook", "api_server")
	blindPlatforms      = strs.Set("webhook", "msgraph_webhook")
)

// Surface the session runs on, as the process environment names it or else its source
func (c Context) platform() string {
	return cmp.Or(os.Getenv("ICEBATH_SESSION_PLATFORM"), c.Source)
}

func (c Context) singleQuery() bool {
	return c.Finite || config.Yes(os.Getenv("ICEBATH_SINGLE_QUERY_SESSION"))
}

func (c Context) cron() bool {
	return tools.Scheduled(c.Source)
}

func (c Context) unattendedPlatform() bool {
	return unattendedPlatforms[c.platform()]
}

// Whether no user can answer a prompt of the session: a one-shot run, a scheduled job, a surface nobody answers
func (c Context) unanswerable() bool {
	return c.singleQuery() || c.cron() || c.unattendedPlatform()
}

// Whether someone can be asked: a window shows the session's prompts and it is no one-shot run or scheduled job; on a surface nobody
// answers only when ICEBATH_EXEC_ASK says to ask anyway and the surface can show prompts
func (a *Approvals) present(c Context) bool {
	if c.Answer != nil {
		return true
	}
	if c.singleQuery() || c.cron() {
		return false
	}
	watched := a.watched(c.Session)
	if c.unattendedPlatform() {
		return config.Yes(os.Getenv("ICEBATH_EXEC_ASK")) && watched && !blindPlatforms[c.platform()]
	}
	return watched
}

// A context where nobody watches, and how its block messages name it
type unattended struct {
	name, key, clause, scope, trust string
}

func (u unattended) mode(cfg *config.Config) string {
	return cfg.GetString(u.key)
}

// BLOCKED sentence naming the context and the setting that would allow it
func (u unattended) block(subject, noun, advice string) string {
	return fmt.Sprintf("BLOCKED: %s but %s. %s To allow %s %s, set %s: approve in config.yaml.", subject, u.clause, advice, noun, u.scope, u.key)
}

// Sentence after a refused script naming the context and the setting that would let it run
func (u unattended) tail() string {
	clause := []rune(u.clause)
	return fmt.Sprintf("%s%s. Use normal tools instead, or set %s: approve only if %s.", strings.ToUpper(string(clause[:1])), string(clause[1:]), u.key,
		u.trust)
}

// Contexts where nobody watches that apply, a one-shot run first, a scheduled job before an unattended surface
func (c Context) unattended() []unattended {
	var out []unattended
	if c.singleQuery() {
		out = append(out, unattended{"single_query", approvalsconfig.SingleQueryMode, "single-query mode (-q) runs without a user present to approve it",
			"in single-query mode", "this single-query run is intentionally trusted"})
	}
	switch {
	case c.cron():
		out = append(out, unattended{"cron", approvalsconfig.CronMode, "cron jobs run without a user present to approve it", "in cron jobs",
			"this cron profile is intentionally trusted"})
	case c.unattendedPlatform():
		out = append(out, unattended{"unattended", approvalsconfig.UnattendedMode,
			fmt.Sprintf("this session runs on an unattended platform (%s) with no user present to approve it", c.platform()), "on unattended platforms",
			"sessions on this surface are intentionally trusted"})
	}
	return out
}

// Reader of commands for the profile, its icebath folders folded
func reading(cfg *config.Config) *reader {
	dirs := []string{cfg.Dir(), cfg.ProfileDir(cfg.Profile())}
	for _, dir := range dirs[:2] {
		dirs = append(dirs, resolve(dir))
	}
	return newReader(cfg.Profile(), dirs)
}

// Text shown to the user to decide on: secrets masked, anything that could hide part of it made visible
func display(text string) string {
	return tools.Visible(redact.File(text, true))
}

// Surface a prompt of the session is asked on as the approval hooks name it: cli where the person works on this machine, gateway elsewhere
func (c Context) surface() string {
	if localSurfaces[c.platform()] {
		return "cli"
	}
	return "gateway"
}

// The approval as its hooks read it: what is asked, its keys, the session, where it is asked, and the turn and tool call that asked
func approval(ctx context.Context, c Context, p Prompt, surface string) hooks.Approval {
	return hooks.Approval{Command: p.Command, Description: p.Description, Key: strs.First(p.Keys), Keys: strs.List(slices.Clone(p.Keys)), SessionKey: c.Session,
		Surface: surface, Session: c.Session, Turn: hooks.TurnOf(ctx).ID, Call: hooks.CallOf(ctx)}
}

// The wait for the user as the human input hooks read it
func humanInput(ctx context.Context, c Context, p Prompt) hooks.HumanInput {
	return hooks.HumanInput{Kind: "approval", ID: strs.Random(16), Session: c.Session, SessionKey: c.Session, Platform: cmp.Or(c.platform(), "cli"),
		Prompt: display(p.Command), Turn: hooks.TurnOf(ctx).ID, Call: hooks.CallOf(ctx)}
}

// Answer an approval came to as its hooks name it: the choice, cancelled when nobody could answer, timeout when nobody did
func choiceOf(out outcome) string {
	switch {
	case out.cancelled != "":
		return "cancelled"
	case out.choice != "":
		return out.choice
	}
	return "timeout"
}

// Wording of one kind of gate
type spec struct {
	noun string
	// Template of a refusal after the prompt, filled with reason, reason_addendum, timeout_addendum and breaker
	refused string
	// Refusal after a transport's deny, empty where transports aren't used
	transportDenied string
	// Log line of a smart approval
	smartLog string
	// The user's answer counts as theirs, resetting the denial count
	user bool
}

const stopCommand = " The user has NOT consented to this action. Do NOT retry this command, do NOT rephrase it, and do NOT attempt the same " +
	"outcome via a different command. Stop the current workflow and wait for the user to respond before taking any further destructive or " +
	"irreversible action."

const stopAction = " The user has NOT consented to this action. Do NOT retry it, do NOT rephrase it, and do NOT attempt the same outcome via a " +
	"different path."

var commandSpec = spec{noun: "command", user: true,
	refused: "BLOCKED: Command {reason}.{reason_addendum}" + stopCommand + "{timeout_addendum}{breaker}",
	transportDenied: "BLOCKED: User denied this command through the selected approval transport. The user has NOT consented to this action. Do NOT " +
		"retry or attempt the same outcome through another route.{breaker}",
	smartLog: "Smart approval: auto-approved '{command}' ({description})"}

var actionSpec = spec{noun: "action", refused: "BLOCKED: Action {reason}.{reason_addendum}" + stopAction + "{timeout_addendum}"}

// Template filled with the values by field name
func fill(template string, fields map[string]string) string {
	for k, v := range fields {
		template = strings.ReplaceAll(template, "{"+k+"}", v)
	}
	return template
}

// One plain sentence for the user on what became of what they were asked about
func summary(cfg *config.Config, outcome, noun string) string {
	switch outcome {
	case "denied":
		return fmt.Sprintf("You denied this %s, so it did not run.", noun)
	case "timeout":
		return fmt.Sprintf("No answer came within %s, so the %s did not run.", window(timeout(cfg)), noun)
	case "notify_failed":
		return fmt.Sprintf("The question could not reach you, so the %s did not run.", noun)
	case "cancelled":
		return fmt.Sprintf("The question was withdrawn or never reached you, so the %s did not run.", noun)
	case "blocked":
		return fmt.Sprintf("This %s is not allowed where nobody can answer, so it did not run.", noun)
	}
	return fmt.Sprintf("This %s did not run.", noun)
}

type refusal = tools.Refusal

func refused(cfg *config.Config, message, outcome, noun, reason string) *refusal {
	return &refusal{Message: message, Outcome: outcome, Summary: summary(cfg, outcome, noun), Reason: reason}
}

// What is asked: the text shown, what it was flagged as, the keys its answer is remembered under
type ask struct {
	target, description string
	keys                []string
	// The smart reviewer checks it first
	smart bool
	// Answers that last past this one may be given
	lasting bool
}

// What a decision came to: let through by the user or the reviewer, or refused
type decided struct {
	refusal *refusal
	user    bool
	smart   bool
}

// Asks the user, after the smart reviewer when it is on, and turns the answer into the decision
func (a *Approvals) decide(ctx context.Context, c Context, s spec, q ask) decided {
	cfg, id := c.Config, c.Session
	smartDenied := false
	if q.smart {
		// The approval hooks hear of the reviewer's look, and of its verdict when it gave one
		asked := approval(ctx, c, Prompt{Command: display(q.target), Description: display(q.description), Keys: q.keys}, "smart")
		a.hooks.Fire(ctx, cfg, asked)
		verdict := a.review(ctx, c, q.target, q.description)
		if verdict == approve || verdict == reject {
			a.hooks.Fire(ctx, cfg, hooks.Approved{Approval: asked, Choice: "smart_" + verdict, DecidedBy: "aux_llm"})
		}
		switch verdict {
		case approve:
			a.reset(id)
			slog.Debug(fill(s.smartLog, map[string]string{"command": strs.Head(q.target, 60), "description": q.description, "session_key": id}))
			return decided{smart: true}
		case reject:
			a.deny(id)
			smartDenied = true
		}
	}
	lasting := q.lasting && !smartDenied
	deny := func(template, outcome, reason string, fields map[string]string) decided {
		all := map[string]string{"description": q.description, "breaker": "", "reason": "", "reason_addendum": "", "timeout_addendum": ""}
		if strings.Contains(template, "{breaker}") {
			all["breaker"] = a.breaker(cfg, id)
		}
		for k, v := range fields {
			all[k] = v
		}
		return decided{refusal: refused(cfg, fill(template, all), outcome, s.noun, reason)}
	}
	grant := func(choice string) decided {
		if lasting {
			if err := a.remember(cfg, id, choice, q.keys); err != nil {
				slog.Warn("The always answer is in effect but could not be saved to allowed commands", "keys", q.keys, "error", err)
			}
		}
		if s.user {
			a.reset(id)
		}
		return decided{user: s.user}
	}
	p := Prompt{Command: display(q.target), Description: display(q.description), Keys: q.keys, Choices: choices(lasting, lasting), SmartDenied: smartDenied}
	if s.transportDenied != "" && c.Answer == nil {
		choice, failed := a.transported(c, a.send(ctx, c, p))
		if failed != nil {
			return decided{refusal: failed}
		}
		switch choice {
		case "":
		case Deny:
			a.deny(id)
			return deny(s.transportDenied, "denied", "", nil)
		default:
			return grant(choice)
		}
	}
	out := a.answered(ctx, c, p)
	switch {
	case out.cancelled != "":
		return deny(s.refused, "cancelled", "", map[string]string{"reason": fmt.Sprintf("approval was withdrawn before the user answered (%s)", out.cancelled)})
	case out.choice == "":
		return deny(s.refused, "timeout", out.reason, map[string]string{"reason": "timed out without user response", "timeout_addendum": " Silence is not consent."})
	case out.choice == Deny:
		addendum := ""
		if out.reason != "" {
			addendum = fmt.Sprintf(" Reason given by the user: \"%s\".", out.reason)
		}
		return deny(s.refused, "denied", out.reason, map[string]string{"reason": "denied by user", "reason_addendum": addendum})
	}
	return grant(out.choice)
}

// Checks a shell command against every rule before it runs, asking the user when they flag it; on an isolated backend only the
// always-deny patterns apply
func (a *Approvals) Command(ctx context.Context, c Context, command string, isolated bool) tools.Verdict {
	cfg := c.Config
	r := reading(cfg)
	if isolated {
		if pattern := r.userDeny(cfg, command); pattern != "" {
			slog.Warn("User deny rule blocked command", "rule", pattern, "command", preview(command))
			return tools.Verdict{Refusal: userDenyRefusal(pattern)}
		}
		return tools.Verdict{}
	}
	if blocked := r.floor(cfg, command, cfg.GetString(terminalconfig.SudoPassword) != ""); blocked != nil {
		return tools.Verdict{Refusal: blocked}
	}
	mode := Mode(cfg)
	if a.skips(c.Session) || mode == approvalsconfig.Off || allowlisted(cfg, command) {
		return tools.Verdict{}
	}
	if !a.present(c) {
		for _, u := range c.unattended() {
			if u.mode(cfg) != approvalsconfig.Refuse {
				continue
			}
			_, description, pending := r.unapproved(command, func(key string) bool { return a.approved(cfg, "", key) })
			if !pending {
				continue
			}
			return tools.Verdict{Refusal: &refusal{Outcome: "blocked", Message: u.block(fmt.Sprintf("Command flagged as dangerous (%s)", description),
				"dangerous commands", "Find an alternative approach that avoids this command.")}}
		}
		return tools.Verdict{}
	}
	keys, description, pending := r.unapproved(command, func(key string) bool { return a.approved(cfg, c.Session, key) })
	if !pending {
		return tools.Verdict{}
	}
	d := a.decide(ctx, c, commandSpec, ask{target: command, description: description, keys: keys, smart: mode == approvalsconfig.Smart, lasting: true})
	switch {
	case d.refusal != nil:
		return tools.Verdict{Refusal: d.refusal}
	case d.user:
		return tools.Verdict{User: true, Note: fmt.Sprintf("Command required approval (%s) and was approved by the user.", description)}
	case d.smart:
		return tools.Verdict{Note: fmt.Sprintf("Command was flagged (%s) and auto-approved by smart approval.", description)}
	}
	return tools.Verdict{}
}

// Asks the user to allow an action a tool flagged, unless yolo, approvals off or an earlier answer let it through; nil when it may go
// ahead. A strict gate asks every time and takes no answer that lasts
func (a *Approvals) Gate(ctx context.Context, c Context, g tools.Gate) error {
	cfg := c.Config
	key, description := cmp.Or(g.Key, g.Description), cmp.Or(g.Description, g.Key)
	noHuman := cmp.Or(g.Unanswered, "BLOCKED: approval required ("+description+") but no interactive user or gateway is present to approve it.")
	if g.Code {
		return a.code(ctx, c, g, key, description, noHuman)
	}
	if g.Strict {
		if c.unanswerable() || !a.present(c) {
			return refused(cfg, noHuman, "blocked", "action", "")
		}
		if d := a.decide(ctx, c, actionSpec, ask{target: g.Target, description: description, keys: []string{key}}); d.refusal != nil {
			return d.refusal
		}
		return nil
	}
	if a.skips(c.Session) || Mode(cfg) == approvalsconfig.Off || a.approved(cfg, c.Session, key) {
		return nil
	}
	if !a.present(c) {
		for _, u := range c.unattended() {
			if u.mode(cfg) == approvalsconfig.Refuse {
				subject, noun, advice := fmt.Sprintf("approval required (%s)", description), "flagged actions", "Find an alternative approach that avoids this action."
				if g.Subject != "" && u.name != "unattended" {
					subject, noun, advice = g.Subject, cmp.Or(g.Noun, noun), cmp.Or(g.Advice, advice)
				}
				return &refusal{Outcome: "blocked", Message: cmp.Or(g.Blocked, u.block(subject, noun, advice))}
			}
			if u.name == "single_query" {
				slog.Warn(fmt.Sprintf("flagged action (pattern: %s): %s — single-query auto-approve (approvals.single_query_mode: approve).", key, description))
				return nil
			}
			slog.Warn(fmt.Sprintf("flagged action (pattern: %s): %s — %s approve mode.", key, description, u.name))
			return nil
		}
		slog.Warn(fmt.Sprintf("flagged action (pattern: %s): %s — no interactive user or gateway present; BLOCKED (fail-closed).", key, description))
		return &refusal{Outcome: "blocked", Message: noHuman}
	}
	if d := a.decide(ctx, c, actionSpec, ask{target: g.Target, description: description, keys: []string{key}, lasting: true}); d.refusal != nil {
		return d.refusal
	}
	return nil
}

// Surfaces the user drives the session from directly, on this machine, where code the agent runs needs no answer of theirs
var localSurfaces = strs.Set("", "tui", "cli", "local", "desktop")

// Wording of a code gate, the name of the tool running the code in place of {tool}
var codeSpec = spec{noun: "code", user: true,
	refused: "BLOCKED: {tool} script {reason}.{reason_addendum} The user has NOT consented to running this code. Do NOT retry, do NOT rephrase the " +
		"script, and do NOT attempt the same outcome via a different tool.{timeout_addendum}{breaker}",
	transportDenied: "BLOCKED: User denied {tool} through the selected approval transport. The user has NOT consented.",
	smartLog:        "Smart approval: auto-approved {tool} for session {session_key}"}

// Asks before code runs that can reach past the command checks: let through under yolo, approvals off, in a context nobody watches that
// trusts it and on the user's own window; refused in one that does not trust it; else asked, the smart reviewer first
func (a *Approvals) code(ctx context.Context, c Context, g tools.Gate, key, description, noHuman string) error {
	cfg := c.Config
	if a.skips(c.Session) || Mode(cfg) == approvalsconfig.Off {
		return nil
	}
	if contexts := c.unattended(); len(contexts) > 0 {
		if u := contexts[0]; u.mode(cfg) == approvalsconfig.Refuse {
			return refused(cfg, "BLOCKED: "+g.Subject+". "+u.tail(), "blocked", codeSpec.noun, "")
		}
		return nil
	}
	if localSurfaces[c.platform()] && !config.Yes(os.Getenv("ICEBATH_EXEC_ASK")) || a.approved(cfg, c.Session, key) {
		return nil
	}
	if !a.present(c) {
		return refused(cfg, noHuman, "blocked", codeSpec.noun, "")
	}
	s := codeSpec
	for _, text := range []*string{&s.refused, &s.transportDenied, &s.smartLog} {
		*text = strings.ReplaceAll(*text, "{tool}", key)
	}
	q := ask{target: g.Target, description: description, keys: []string{key}, smart: Mode(cfg) == approvalsconfig.Smart, lasting: true}
	if d := a.decide(ctx, c, s, q); d.refusal != nil {
		return d.refusal
	}
	return nil
}

// Asks before a slash command that deletes or resets something, unless confirmations are off: go ahead or not, and what to add to the
// command's reply, such as that it won't ask again
func (a *Approvals) Confirm(ctx context.Context, c Context, command, detail string) (bool, string) {
	if !Confirms(c.Config) || !a.present(c) {
		return true, ""
	}
	p := Prompt{Title: "Run /" + command + "?", Command: "/" + command, Description: detail, Keys: []string{"destructive_slash:" + command},
		Choices: []Choice{{Once, "Yes"}, {Always, "Yes, stop asking"}, {Deny, "No"}}, quiet: true}
	out := a.answered(ctx, c, p)
	switch out.choice {
	case Once:
		return true, ""
	case Always:
		if err := c.Config.Set(approvalsconfig.DestructiveConfirm, false); err != nil {
			slog.Warn("Could not persist destructive_slash_confirm=false", "session", c.Session, "error", err)
			return true, "\n\n⚠ That choice could not be saved, so /" + command + " and the other commands that delete or reset things ask again next " +
				"time. To stop the questions, turn off Confirm before deleting in /settings under Approvals."
		}
		slog.Info("User opted out of destructive slash confirm", "session", c.Session)
		return true, "\n\n/" + command + " and the other commands that delete or reset things now run without asking. To be asked again, turn on " +
			"Confirm before deleting in /settings under Approvals."
	}
	return false, "Cancelled /" + command + ", so nothing changed."
}

// The answer to a prompt: the one the context gives in place of a user, else the user's own, waited for
func (a *Approvals) answered(ctx context.Context, c Context, p Prompt) outcome {
	if c.Answer != nil {
		asked := approval(ctx, c, p, c.surface())
		a.heard(p).Fire(ctx, c.Config, asked)
		out := outcome{choice: c.Answer(p.Command, p.Description)}
		a.heard(p).Fire(context.WithoutCancel(ctx), c.Config, hooks.Approved{Approval: asked, Choice: choiceOf(out)})
		return out
	}
	return a.await(ctx, c, p)
}

// Binds the session's approvals into the environment of a turn
func (a *Approvals) Bind(env *tools.Env, c Context) {
	env.Gate = func(ctx context.Context, g tools.Gate) error { return a.Gate(ctx, c, g) }
	env.Approve = func(ctx context.Context, action, reason string) error {
		return a.Gate(ctx, c, tools.Gate{Key: reason, Target: action})
	}
	env.Command = func(ctx context.Context, command string, isolated bool) tools.Verdict {
		return a.Command(ctx, c, command, isolated)
	}
}

// Everything the rules key a decision on for the command, the same for two commands the rules treat alike
func Identity(cfg *config.Config, command string) string {
	r := reading(cfg)
	key, _ := json.Marshal([]any{r.hardline(command), r.dangerous(command), r.contents(command),
		r.sudoGuess(command, cfg.GetString(terminalconfig.SudoPassword) != ""), deletesRuntime(command), r.userDeny(cfg, command),
		allowlisted(cfg, command)})
	return string(key)
}

// Dry run of the rules for a command
type Evaluation struct {
	Command string
	// Backend it would run on
	Backend string
	// allow, ask-approval, hardline-deny or user-deny
	Verdict string
	// Exit code the check gives: 0 allow, 2 ask, 3 deny
	Code   int
	Rule   string
	Detail string
	// Spellings of the command the rules read
	Variants []string
}

// Verdicts of a dry run and their exit codes
const (
	Allowed  = "allow"
	Asks     = "ask-approval"
	Hardline = "hardline-deny"
	UserDeny = "user-deny"
)

var exits = map[string]int{Allowed: 0, Asks: 2, Hardline: 3, UserDeny: 3}

// What the rules would do with the command on the backend named, in the order they run, without running, asking or remembering anything
func (a *Approvals) Evaluate(cfg *config.Config, session, command, backend string, isolated bool) Evaluation {
	r := reading(cfg)
	result := func(verdict, rule, detail string) Evaluation {
		return Evaluation{Command: command, Backend: backend, Verdict: verdict, Code: exits[verdict], Rule: rule, Detail: detail, Variants: r.variants(command)}
	}
	if isolated {
		if pattern := r.userDeny(cfg, command); pattern != "" {
			return result(UserDeny, pattern, "It matches one of your Blocked commands, which stay blocked even in an isolated sandbox, with --yolo, or when "+
				"When to ask is Never ask.")
		}
		return result(Allowed, "", fmt.Sprintf("It runs in the isolated %q sandbox, so only your Blocked commands are checked.", backend))
	}
	if description := r.hardline(command); description != "" {
		return result(Hardline, description, "It is on the list of commands that are always blocked, even with --yolo or when When to ask is Never ask.")
	}
	if description := r.sudoGuess(command, cfg.GetString(terminalconfig.SudoPassword) != ""); description != "" {
		return result(Hardline, description, "It pipes a password into sudo, which is always blocked.")
	}
	if target := deletesRuntime(command); target != "" {
		return result(Hardline, "deletes "+target, "It deletes icebath's own program, which is always blocked.")
	}
	if pattern := r.userDeny(cfg, command); pattern != "" {
		return result(UserDeny, pattern, "It matches one of your Blocked commands, which stay blocked even with --yolo or when When to ask is Never ask.")
	}
	if a.skips(session) || Mode(cfg) == approvalsconfig.Off {
		return result(Allowed, "", "Asking is off, by --yolo or by When to ask set to Never ask, so only the always-blocked commands and your Blocked "+
			"commands would stop it.")
	}
	if allowlisted(cfg, command) {
		return result(Allowed, "", "It matches one of your Allowed commands, so it runs without asking.")
	}
	if description := r.dangerous(command); description != "" {
		return result(Asks, description, fmt.Sprintf("It looks risky, as %s, so the agent would ask you first.", description))
	}
	return result(Allowed, "", "No check matched, so it would run without asking.")
}

// Refusal of a script run inside the server whose text would stop, restart or uninstall the gateway or kill the program hosting it, which
// would kill the script before it could finish; empty when it may run or this process is no server
func (a *Approvals) Script(cfg *config.Config, code string) string {
	if !a.serving.Load() || !containsLifecycle(code, cfg.Profile()) {
		return ""
	}
	if withinBudget(code) && containsHostKill(code) {
		return hostKillRefusal
	}
	return "Blocked: cannot restart or stop the gateway from inside the gateway process. The gateway would kill this script before it could " +
		"complete (SIGTERM propagates to child processes). Run the lifecycle command from a shell outside the gateway."
}

// Refusal of a command run inside the server that would stop, restart or uninstall the gateway or kill the program hosting it, or that runs
// a script that can't be read to make sure; empty when it may run or this process is no server. read fetches a script through the
// session's backend when it is not on this machine
func (a *Approvals) Lifecycle(cfg *config.Config, command, cwd string, read func(path string) (string, bool)) string {
	if !a.serving.Load() {
		return ""
	}
	if withinBudget(command) && containsLaunchctlSubmit(command) {
		return "Blocked: launchctl submit/bootstrap is restricted inside a supervised gateway regardless of the job label, to prevent indirect " +
			"gateway restart loops. This guard does not inspect the job's KeepAlive settings or determine whether it is independent of icebath. " +
			"Perform authorized LaunchAgent maintenance from a separate shell outside the gateway, not by switching launchctl verbs to bypass " +
			"this rejection."
	}
	unsafe, why := scanLifecycle(command, cwd, cfg.Profile(), read)
	switch {
	case unsafe && why != "":
		return fmt.Sprintf("Blocked: the lifecycle guard could not scan this command or referenced script: %s. ", why) +
			"Nothing in the command is known to contain a gateway lifecycle command, but a script the command executes must be scannable (a " +
			"regular text file under 1 MiB) before it can run inside the gateway process."
	case unsafe && withinBudget(command) && containsHostKill(command):
		return hostKillRefusal
	case unsafe:
		return "Blocked: command or referenced script cannot restart, stop, or uninstall the gateway from inside the gateway process. The " +
			"gateway would kill this command before it could complete (SIGTERM propagates to child processes). Run `icebath gateway restart` " +
			"from a separate shell outside the running gateway."
	}
	return ""
}
