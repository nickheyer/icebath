package approval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/tool"
)

var (
	summaryOutcomes = set("denied", "timeout", "notify_failed", "cancelled", "blocked")
	summaryNouns    = set("command", "code", "action")
)

func (g *Gate) userSummary(ctx context.Context, outcome, noun string) string {
	window, _ := FormatWindow(approvalTimeout(ctx, g.o.Config), g.o.Translate)
	key := "approval.summary.default"
	if summaryOutcomes[outcome] {
		key = "approval.summary." + outcome
	}
	nounText := noun
	if summaryNouns[noun] {
		nounText = g.o.Translate("approval.noun."+noun, nil)
	}
	return g.o.Translate(key, map[string]string{"noun": nounText, "window": window})
}

func (g *Gate) denied(ctx context.Context, message, key, description, outcome, noun string, reasonAsked bool, reason string) tool.Decision {
	no := false
	return tool.Decision{Message: message, PatternKey: key, Description: description, Outcome: outcome,
		UserConsent: &no, UserSummary: g.userSummary(ctx, outcome, noun), ReasonAsked: reasonAsked, DenyReason: reason}
}

func blocked(message, key, description string) tool.Decision {
	return tool.Decision{Message: message, PatternKey: key, Description: description}
}

func approved() tool.Decision { return tool.Decision{Approved: true} }

func (g *Gate) userApproved(sessionKey, description string) tool.Decision {
	g.resetDenials(sessionKey)
	return tool.Decision{Approved: true, UserApproved: true, Description: description}
}

type gateSpec struct {
	noun            string
	transport       bool
	userApproved    bool
	redactCLI       bool
	pendingKeys     bool
	notifyFailed    string
	gatewayRefused  string
	transportDenied string
	cliTimeout      string
	cliDenied       string
	smartLog        string
}

const (
	stopCommand = " The user has NOT consented to this action. Do NOT retry this command, do NOT rephrase it, and do NOT " +
		"attempt the same outcome via a different command. Stop the current workflow and wait for the user to respond " +
		"before taking any further destructive or irreversible action."
	stopAction = " The user has NOT consented to this action. Do NOT retry it, do NOT rephrase it, and do NOT attempt " +
		"the same outcome via a different path."
	executeCodeDescription = "execute_code script execution. The script can spawn subprocesses or mutate files without " +
		"passing through terminal command approval; approval is one-shot for this run."
)

var (
	commandGate = gateSpec{
		noun: "command", transport: true, userApproved: true, pendingKeys: true,
		notifyFailed:   "BLOCKED: Failed to send approval request to user. Do NOT retry.",
		gatewayRefused: "BLOCKED: Command {reason}.{reason_addendum}" + stopCommand + "{timeout_addendum}{breaker}",
		transportDenied: "BLOCKED: User denied this command through the selected approval transport. The user has NOT " +
			"consented to this action. Do NOT retry or attempt the same outcome through another route.{breaker}",
		cliTimeout: "BLOCKED: Command timed out without user response." + stopCommand + " Silence is not consent.{breaker}",
		cliDenied:  "BLOCKED: User denied this command." + stopCommand + "{breaker}",
		smartLog:   "Smart approval: auto-approved '{command}' ({description})",
	}
	executeCodeGate = gateSpec{
		noun: "code", transport: true, userApproved: true, redactCLI: true, pendingKeys: true,
		notifyFailed: "BLOCKED: Failed to send execute_code approval request to user. Do NOT retry.",
		gatewayRefused: "BLOCKED: execute_code script {reason}.{reason_addendum} The user has NOT consented to running " +
			"this code. Do NOT retry, do NOT rephrase the script, and do NOT attempt the same outcome via a different " +
			"tool.{timeout_addendum}{breaker}",
		transportDenied: "BLOCKED: User denied execute_code through the selected approval transport. The user has NOT consented.",
		cliTimeout:      "BLOCKED: Action timed out without user response." + stopAction + " Silence is not consent.{breaker}",
		cliDenied: "BLOCKED: User denied execute_code script execution (matched '{description}'). Do NOT retry — the " +
			"user has explicitly rejected it.{breaker}",
		smartLog: "Smart approval: auto-approved execute_code for session {session_key}",
	}
	actionGate = gateSpec{
		noun:           "action",
		notifyFailed:   "BLOCKED: Failed to send approval request to user. Do NOT retry.",
		gatewayRefused: "BLOCKED: Action {reason}.{reason_addendum}" + stopAction + "{timeout_addendum}",
		cliTimeout:     "BLOCKED: Action timed out without user response." + stopAction + " Silence is not consent.",
		cliDenied: "BLOCKED: User denied this potentially dangerous action (matched '{description}'). Do NOT retry — " +
			"the user has explicitly rejected it.",
	}
)

type unattendedCtx struct {
	name, cfgKey, clause, scope, trust string
	approves                           func(ctx context.Context, st *config.Store) bool
}

func (u unattendedCtx) blockMessage(subject, noun, advice string) string {
	return "BLOCKED: " + subject + " but " + u.clause + ". " + advice + " To allow " + noun + " " + u.scope +
		", set approvals." + u.cfgKey + ": approve in config.yaml."
}

func (u unattendedCtx) execTail() string {
	return strings.ToUpper(u.clause[:1]) + u.clause[1:] + ". Use normal tools instead, or set approvals." + u.cfgKey +
		": approve only if " + u.trust + "."
}

var (
	singleQueryCtx = unattendedCtx{"single_query", "single_query_mode",
		"single-query mode (-q) runs without a user present to approve it",
		"in single-query mode", "this single-query run is intentionally trusted", singleQueryApproves}
	cronCtx = unattendedCtx{"cron", "cron_mode", "cron jobs run without a user present to approve it",
		"in cron jobs", "this cron profile is intentionally trusted", cronApproves}
)

func unattendedContexts(c *tool.Call) []unattendedCtx {
	var out []unattendedCtx
	if c.Presence.SingleQuery {
		out = append(out, singleQueryCtx)
	}
	switch {
	case c.Presence.Cron:
		out = append(out, cronCtx)
	case unattendedPlatform(c.Presence, c.Origin):
		out = append(out, unattendedCtx{"unattended", "unattended_mode",
			"this session runs on an unattended platform (" + c.Origin.Platform + ") with no user present to approve it",
			"on unattended platforms", "sessions on this surface are intentionally trusted", unattendedApproves})
	}
	return out
}

type findings struct {
	keys        []string
	description string
}

func (g *Gate) unapproved(d *Detector, command string, approvedKey func(string) bool) (findings, bool) {
	dangerous, key, description := d.Dangerous(command)
	if !dangerous {
		return findings{}, false
	}
	type item struct{ key, text string }
	all := []item{{key, description}}
	for _, k := range d.ContentRuleKeys(command) {
		if k != key {
			all = append(all, item{k, k})
		}
	}
	var out findings
	var texts []string
	for _, it := range all {
		if !approvedKey(it.key) {
			out.keys = append(out.keys, it.key)
			texts = append(texts, it.text)
		}
	}
	if len(out.keys) == 0 {
		return findings{}, false
	}
	out.description = strings.Join(texts, "; ")
	return out, true
}

func hasNotifier(c *tool.Call) bool { return c.Surface != nil && c.Surface.Kind() != "cli" }

type presence struct{ cli, gateway, ask bool }

func presenceOf(c *tool.Call) presence {
	p := presence{cli: c.Presence.Interactive, gateway: gatewayContext(c), ask: c.Presence.Ask}
	switch {
	case c.Presence.SingleQuery || c.Presence.Cron:
		p = presence{}
	case p.ask && unattendedPlatform(c.Presence, c.Origin) && (!hasNotifier(c) || notifierBlind(c.Origin)):
		p.ask = false
	}
	return p
}

type decisionInput struct {
	command, description, key string
	keys                      []string
	sessionKey                string
	presence                  presence
	smart                     bool
	pendingBody               func() string
}

func format(template string, fields map[string]string) string {
	pairs := make([]string, 0, 2*len(fields))
	for k, v := range fields {
		pairs = append(pairs, "{"+k+"}", v)
	}
	return strings.NewReplacer(pairs...).Replace(template)
}

func firstRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) > n {
		rs = rs[:n]
	}
	return string(rs)
}

func (g *Gate) smartGate(ctx context.Context, c *tool.Call, spec gateSpec, in decisionInput, humanPresent bool) (*tool.Decision, bool) {
	v := g.guardian.verdict(ctx, c, in.command, in.description, in.key, in.keys)
	switch v {
	case verdictApprove:
		g.resetDenials(in.sessionKey)
		slog.DebugContext(ctx, format(spec.smartLog, map[string]string{"command": firstRunes(in.command, 60),
			"description": in.description, "session_key": in.sessionKey}))
		d := tool.Decision{Approved: true, SmartApproved: true, Description: in.description}
		return &d, false
	case verdictDeny:
	default:
		return nil, false
	}
	g.recordDenial(in.sessionKey)
	if humanPresent {
		return nil, true
	}
	d := tool.Decision{SmartDenied: true, Message: "BLOCKED by smart approval: " + in.description +
		". The command was assessed as genuinely dangerous. Do NOT retry." + g.breakerAddendum(ctx, in.sessionKey)}
	return &d, true
}

func (g *Gate) pendingResult(spec gateSpec, in decisionInput, command, description, body string, smartDenied bool) tool.Decision {
	if !spec.pendingKeys {
		return tool.Decision{PatternKey: in.key, Status: "approval_required", Command: command, Description: description,
			Message: "⚠️ This action is potentially dangerous (" + description + "). Asking the user for approval.\n\n" +
				"**Target:**\n```\n" + command + "\n```"}
	}
	if body == "" {
		body = "**Command:**\n```\n" + command + "\n```"
	}
	d := tool.Decision{PatternKey: in.key, Status: "pending_approval", Command: command, Description: description,
		Message: "⚠️ " + description + ". Asking the user for approval.\n\n" + body + "\n\n" +
			"STOP: do NOT re-run, rephrase, or re-issue this " + spec.noun + " — each variant sends the user ANOTHER " +
			"approval card. Wait for the user's decision; if this turn must end, report that approval is pending."}
	if smartDenied {
		d.SmartDenied = true
		no := false
		d.AllowPermanent = &no
	}
	return d
}

func (g *Gate) prompt(ctx context.Context, c *tool.Call, p tool.ApprovalPrompt) tool.Choice {
	var choice tool.Choice
	g.humanInput(ctx, c, "approval", p.Command, c.SessionKey, func() string {
		done := g.humanWait(ctx, sessionKey(c))
		defer done()
		choice = g.ask(ctx, c, p)
		return choice.Answer
	})
	return choice
}

func (g *Gate) ask(ctx context.Context, c *tool.Call, p tool.ApprovalPrompt) tool.Choice {
	if c.Surface == nil {
		return tool.Choice{Answer: "cancelled", Reason: "no approval surface is attached to this turn, so the prompt could not be shown"}
	}
	p.Command = g.o.Redactor.Approval(ctx, p.Command, false, false)
	p.Description = g.o.Redactor.Approval(ctx, p.Description, false, false)
	p.Timeout = time.Duration(approvalTimeout(ctx, g.o.Config)) * time.Second
	choice, ok, err := c.Surface.PromptApproval(ctx, p)
	switch {
	case err != nil:
		slog.ErrorContext(ctx, "Approval callback failed", "error", err)
		return tool.Choice{Answer: "cancelled", Reason: fmt.Sprintf("the approval callback failed: %T", err)}
	case !ok:
		return tool.Choice{Answer: "cancelled", Reason: "the approval surface cannot show a blocking prompt on this turn"}
	}
	if choice.Answer == "cancelled" && choice.Reason == "" {
		choice.Reason = "no answer"
	}
	return choice
}

func (g *Gate) humanDecision(ctx context.Context, c *tool.Call, spec gateSpec, in decisionInput) tool.Decision {
	smartDenied := false
	if in.smart {
		if d, overridable := g.smartGate(ctx, c, spec, in, in.presence.cli || in.presence.gateway || in.presence.ask); d != nil {
			return *d
		} else {
			smartDenied = overridable
		}
	}
	body := ""
	if in.pendingBody != nil {
		body = in.pendingBody()
	}
	allowPermanent := !smartDenied
	deny := func(template, outcome string, fields map[string]string, reasonAsked bool, reason string) tool.Decision {
		breaker := ""
		if strings.Contains(template, "{breaker}") {
			breaker = g.breakerAddendum(ctx, in.sessionKey)
		}
		values := map[string]string{"description": in.description, "breaker": breaker}
		for k, v := range fields {
			values[k] = v
		}
		return g.denied(ctx, format(template, values), in.key, in.description, outcome, spec.noun, reasonAsked, reason)
	}
	grant := func(choice string) tool.Decision {
		if !smartDenied {
			g.persistChoice(ctx, in.sessionKey, choice, in.keys)
		}
		if spec.userApproved {
			return g.userApproved(in.sessionKey, in.description)
		}
		return approved()
	}
	if spec.transport {
		surface := "cli"
		if in.presence.gateway || in.presence.ask {
			surface = gatewaySurface(c.Origin)
		}
		attempt := g.presentWithTransport(ctx, c, in, !smartDenied, allowPermanent, surface)
		choice, refusal := g.transportChoice(ctx, c, attempt, in.key, in.description)
		if refusal != nil {
			return *refusal
		}
		if choice != "" {
			if choice == "deny" {
				g.recordDenial(in.sessionKey)
				return deny(spec.transportDenied, "denied", nil, false, "")
			}
			return grant(choice)
		}
	}
	if in.presence.gateway || in.presence.ask {
		displayCommand := g.o.Redactor.Approval(ctx, in.command, false, false)
		displayDescription := g.o.Redactor.Approval(ctx, in.description, false, false)
		if hasNotifier(c) {
			data := tool.ApprovalPrompt{Command: displayCommand, PatternKey: in.key, PatternKeys: in.keys,
				Description: displayDescription, AllowPermanent: !smartDenied, AllowSession: !smartDenied, SmartDenied: smartDenied}
			decision := g.awaitGateway(ctx, c, data, "gateway")
			if decision.notifyFailed {
				return g.denied(ctx, spec.notifyFailed, in.key, in.description, "notify_failed", spec.noun, false, "")
			}
			if decision.cancelled != "" {
				return deny(spec.gatewayRefused, "cancelled", map[string]string{
					"reason":          "approval was withdrawn before the user answered (" + decision.cancelled + ")",
					"reason_addendum": "", "timeout_addendum": ""}, true, "")
			}
			if !decision.resolved {
				return deny(spec.gatewayRefused, "timeout", map[string]string{"reason": "timed out without user response",
					"reason_addendum": "", "timeout_addendum": " Silence is not consent."}, true, decision.reason)
			}
			if decision.choice == "" || decision.choice == "deny" {
				addendum := ""
				if decision.reason != "" {
					addendum = " Reason given by the user: \"" + decision.reason + "\"."
				}
				return deny(spec.gatewayRefused, "denied", map[string]string{"reason": "denied by user",
					"reason_addendum": addendum, "timeout_addendum": ""}, true, decision.reason)
			}
			return grant(decision.choice)
		}
		if !fallThroughToCLI(in.presence.cli, c.Surface != nil, false) {
			if !spec.pendingKeys {
				displayCommand, displayDescription = in.command, in.description
			}
			return g.pendingResult(spec, in, displayCommand, displayDescription, body, smartDenied)
		}
	}
	promptCommand, promptDescription := in.command, in.description
	if spec.redactCLI {
		promptCommand = g.o.Redactor.Approval(ctx, in.command, false, false)
		promptDescription = g.o.Redactor.Approval(ctx, in.description, false, false)
	}
	hookPrompt := tool.ApprovalPrompt{Command: promptCommand, Description: promptDescription, PatternKey: in.key,
		PatternKeys: append([]string(nil), in.keys...), SessionKey: in.sessionKey, Surface: "cli"}
	g.o.Hooks.Approval(ctx, "pre_approval_request", approvalHook(c, hookPrompt, ""))
	choice := g.prompt(ctx, c, tool.ApprovalPrompt{ID: newRequestID(), SessionKey: in.sessionKey, Command: promptCommand,
		Description: promptDescription, PatternKey: in.key, PatternKeys: append([]string(nil), in.keys...),
		AllowSession: true, AllowPermanent: allowPermanent, SmartDenied: smartDenied, Surface: "cli"})
	g.o.Hooks.Approval(ctx, "post_approval_response", approvalHook(c, hookPrompt, choice.Answer))
	switch choice.Answer {
	case "timeout":
		return deny(spec.cliTimeout, "timeout", nil, false, "")
	case "cancelled":
		return deny(spec.gatewayRefused, "cancelled", map[string]string{
			"reason":          "was not approved: the approval prompt could not be delivered or was not answered (" + choice.Reason + ")",
			"reason_addendum": "", "timeout_addendum": " Silence is not consent."}, true, "")
	case "deny":
		return deny(spec.cliDenied, "denied", nil, false, "")
	}
	return grant(choice.Answer)
}

func (g *Gate) skipContainerGuards(envKind string, hostAccess bool) bool {
	switch envKind {
	case "docker":
		return !hostAccess
	case "modal", "daytona", "vercel_sandbox":
		return true
	}
	return g.o.SkipGuards(envKind)
}

func (g *Gate) userDenyBlock(ctx context.Context, d *Detector, command string) (tool.Decision, bool) {
	v := readApprovals(ctx, g.o.Config)
	pattern, ok := d.DenyRule(command, v.cfg.Deny)
	if !ok {
		return tool.Decision{}, false
	}
	slog.WarnContext(ctx, "User deny rule blocked command", "rule", pattern, "command", firstRunes(command, 200))
	return DenyBlock(pattern), true
}

func (g *Gate) sudoPassword(ctx context.Context, c *tool.Call) bool {
	if c.Secrets == nil {
		return false
	}
	_, ok, err := c.Secrets.Lookup(ctx, "SUDO_PASSWORD")
	return err == nil && ok
}

func (g *Gate) floorBlock(ctx context.Context, c *tool.Call, d *Detector, command string, sudoGuard bool) (tool.Decision, bool) {
	profileHome, _ := g.profileHome(ctx, c)
	if hit, desc := d.Hardline(command); hit {
		slog.WarnContext(ctx, "Hardline block", "description", desc, "command", firstRunes(command, 200))
		return HardlineBlock(ctx, profileHome, desc, command), true
	}
	if target, ok := g.protector.Command(command); ok {
		slog.WarnContext(ctx, "Runtime self-delete block", "target", target, "command", firstRunes(command, 200))
		return HardlineBlock(ctx, profileHome, "recursive/any delete of "+target, command), true
	}
	if sudoGuard {
		if hit, desc := d.SudoStdin(command, g.sudoPassword(ctx, c)); hit {
			slog.WarnContext(ctx, "Sudo stdin guard block", "description", desc, "command", firstRunes(command, 200))
			return SudoBlock(desc), true
		}
	}
	return g.userDenyBlock(ctx, d, command)
}

func detectorFailure(err error) tool.Decision {
	return tool.Decision{Message: "BLOCKED: the approval gate could not start its command detector: " + err.Error()}
}

func (g *Gate) Command(ctx context.Context, c *tool.Call, cmd tool.CommandCheck) tool.Decision {
	d, err := g.Detector(ctx, c)
	if err != nil {
		return detectorFailure(err)
	}
	if g.skipContainerGuards(cmd.EnvKind, cmd.HostAccess) {
		if block, ok := g.userDenyBlock(ctx, d, cmd.Command); ok {
			return block
		}
		return approved()
	}
	if !cmd.SudoGuard {
		return g.dangerousOnly(ctx, c, d, cmd.Command)
	}
	if block, ok := g.floorBlock(ctx, c, d, cmd.Command, true); ok {
		return block
	}
	if slot := batchSlot(ctx); slot != nil {
		if prepared, ok := slot.Consume(cmd.Command, cmd.EnvKind, cmd.HostAccess); ok {
			return prepared
		}
	}
	mode := Mode(ctx, g.o.Config, c.Origin)
	if g.yoloActive(c) || mode == config.ApprovalOff {
		return approved()
	}
	if AllowlistMatch(cmd.Command, g.Permanent(ctx)) {
		return approved()
	}
	p := presenceOf(c)
	if !p.cli && !p.gateway && !p.ask {
		for _, u := range unattendedContexts(c) {
			if block, ok := g.unattendedDeny(ctx, d, cmd.Command, u); ok {
				return block
			}
		}
		return approved()
	}
	key := sessionKey(c)
	f, pending := g.unapproved(d, cmd.Command, func(k string) bool { return g.Approved(ctx, c, k) })
	if !pending {
		return approved()
	}
	return g.humanDecision(ctx, c, commandGate, decisionInput{command: cmd.Command, description: f.description,
		key: f.keys[0], keys: f.keys, sessionKey: key, presence: p, smart: mode == config.ApprovalSmart})
}

func (g *Gate) unattendedDeny(ctx context.Context, d *Detector, command string, u unattendedCtx) (tool.Decision, bool) {
	if u.approves(ctx, g.o.Config) {
		return tool.Decision{}, false
	}
	f, pending := g.unapproved(d, command, func(k string) bool { return g.permanentlyApproved(ctx, k) })
	if !pending {
		return tool.Decision{}, false
	}
	result := tool.Decision{Message: u.blockMessage("Command flagged as dangerous ("+f.description+")",
		"dangerous commands", "Find an alternative approach that avoids this command.")}
	if u.name == "single_query" {
		result.PatternKey, result.Description = f.keys[0], f.description
	}
	return result, true
}

func (g *Gate) dangerousOnly(ctx context.Context, c *tool.Call, d *Detector, command string) tool.Decision {
	if block, ok := g.floorBlock(ctx, c, d, command, false); ok {
		return block
	}
	if g.yoloActive(c) {
		return approved()
	}
	if AllowlistMatch(command, g.Permanent(ctx)) {
		return approved()
	}
	f, pending := g.unapproved(d, command, func(k string) bool { return g.Approved(ctx, c, k) })
	if !pending {
		return approved()
	}
	return g.runGate(ctx, c, gateRequest{key: f.keys[0], keys: f.keys, description: f.description, target: command,
		subject: "Command flagged as dangerous (" + f.description + ")", noun: "dangerous commands",
		advice:    "Find an alternative approach that avoids this command.",
		logPrefix: "AUTO-APPROVED dangerous command in non-interactive non-gateway context"})
}

type gateRequest struct {
	key, description, target, subject, noun, advice string
	denyCron, denySingleQuery, denyUnattended       string
	logPrefix                                       string
	failClosed                                      bool
	noHuman                                         string
	keys                                            []string
}

func (g *Gate) runGate(ctx context.Context, c *tool.Call, r gateRequest) tool.Decision {
	if g.yoloActive(c) || Mode(ctx, g.o.Config, c.Origin) == config.ApprovalOff {
		return approved()
	}
	key := sessionKey(c)
	if g.Approved(ctx, c, r.key) {
		return approved()
	}
	if r.noun == "" {
		r.noun = "flagged actions"
	}
	if r.advice == "" {
		r.advice = "Find an alternative approach that avoids this action."
	}
	p := presenceOf(c)
	if !p.cli && !p.gateway && !p.ask {
		contexts := unattendedContexts(c)
		messages := map[string]string{"single_query": r.denySingleQuery, "cron": r.denyCron, "unattended": r.denyUnattended}
		for _, u := range contexts {
			if !u.approves(ctx, g.o.Config) {
				message := messages[u.name]
				switch {
				case message == "" && u.name == "unattended":
					message = u.blockMessage("approval required ("+r.description+")", "flagged actions",
						"Find an alternative approach that avoids this action.")
				case message == "":
					message = u.blockMessage(r.subject, r.noun, r.advice)
				}
				return blocked(message, r.key, r.description)
			}
			if u.name == "single_query" {
				slog.WarnContext(ctx, r.logPrefix+" — single-query auto-approve (approvals.single_query_mode: approve).",
					"pattern", r.key, "description", r.description)
				return approved()
			}
			break
		}
		if len(contexts) == 0 && r.failClosed {
			slog.WarnContext(ctx, r.logPrefix+" — no interactive user/gateway present; BLOCKED (fail-closed). Set "+
				"ICEBATH_INTERACTIVE or ICEBATH_GATEWAY_SESSION to answer the prompt.", "pattern", r.key, "description", r.description)
			message := r.noHuman
			if message == "" {
				message = "BLOCKED: approval required (" + r.description + ") but no interactive user or gateway is present to approve it."
			}
			return blocked(message, r.key, r.description)
		}
		slog.WarnContext(ctx, r.logPrefix+" — set ICEBATH_INTERACTIVE or ICEBATH_GATEWAY_SESSION to require approval.",
			"pattern", r.key, "description", r.description)
		return approved()
	}
	keys := r.keys
	if len(keys) == 0 {
		keys = []string{r.key}
	}
	return g.humanDecision(ctx, c, actionGate, decisionInput{command: r.target, description: r.description, key: r.key,
		keys: keys, sessionKey: key, presence: p})
}

func ToolApprovalRequest(toolName, reason, ruleKey string) tool.ActionRequest {
	description := reason
	if description == "" {
		description = "Plugin requires approval for " + toolName
	}
	if ruleKey == "" {
		sum := sha256.Sum256([]byte(description))
		ruleKey = toolName + ":" + hex.EncodeToString(sum[:])[:12]
	}
	subject := "Tool '" + toolName + "' requires approval (" + description + ")"
	return tool.ActionRequest{
		Tool:        toolName,
		PatternKey:  "plugin_rule:" + ruleKey,
		Description: description,
		Target:      "<" + toolName + "> (plugin approval rule)",
		Subject:     subject,
		Advice:      "Find an alternative approach.",
		FailClosed:  true,
		NoHuman:     "BLOCKED: " + subject + " but no interactive user or gateway is present to approve it. A plugin flagged this action for human confirmation.",
	}
}

func (g *Gate) Action(ctx context.Context, c *tool.Call, a tool.ActionRequest) tool.Decision {
	return g.runGate(ctx, c, gateRequest{key: a.PatternKey, keys: a.PatternKeys, description: a.Description,
		target: a.Target, subject: a.Subject, noun: a.Noun, advice: a.Advice, denyCron: a.DenyCron,
		denySingleQuery: a.DenySingleQuery, denyUnattended: a.DenyUnattended, failClosed: a.FailClosed, noHuman: a.NoHuman,
		logPrefix: "flagged action '" + a.Tool + "' in non-interactive non-gateway context"})
}

func (g *Gate) localViewerTurn(c *tool.Call) bool {
	return localInteractive(c.Origin) && c.Presence.Viewer && !c.Origin.DelegatedChild && hasNotifier(c)
}

func (g *Gate) Code(ctx context.Context, c *tool.Call, code tool.CodeCheck) tool.Decision {
	const key = "execute_code"
	if code.EnvKind == "vercel_sandbox" || g.skipContainerGuards(code.EnvKind, code.HostAccess) {
		return approved()
	}
	mode := Mode(ctx, g.o.Config, c.Origin)
	if g.yoloActive(c) || mode == config.ApprovalOff {
		return approved()
	}
	p := presenceOf(c)
	for _, u := range unattendedContexts(c) {
		if !u.approves(ctx, g.o.Config) {
			return g.denied(ctx, "BLOCKED: execute_code runs arbitrary local Python (including subprocess calls that bypass "+
				"shell-string approval checks). "+u.execTail(), key, executeCodeDescription, "blocked", "code", false, "")
		}
		return approved()
	}
	if (!p.gateway && !p.ask) || g.localViewerTurn(c) {
		return approved()
	}
	command := "execute_code <<'PY'\n" + code.Code + "\nPY"
	if g.Approved(ctx, c, key) {
		return approved()
	}
	return g.humanDecision(ctx, c, executeCodeGate, decisionInput{command: command, description: executeCodeDescription,
		key: key, keys: []string{key}, sessionKey: sessionKey(c), presence: p, smart: mode == config.ApprovalSmart,
		pendingBody: func() string {
			return "**Code:**\n```python\n" + g.o.Redactor.Approval(ctx, code.Code, false, false) + "\n```"
		}})
}

func consent(choice, unresolved string) tool.Decision {
	switch choice {
	case "once", "session", "always":
		return tool.Decision{Approved: true}
	case "timeout", "cancelled":
		return tool.Decision{Outcome: unresolved}
	}
	return tool.Decision{Outcome: "denied"}
}

func (g *Gate) rememberedConsent(ctx context.Context, sessionKey, choice, rememberKey, unresolved string) tool.Decision {
	if rememberKey != "" {
		g.persistChoice(ctx, sessionKey, choice, []string{rememberKey})
	}
	return consent(choice, unresolved)
}

func (g *Gate) Elicit(ctx context.Context, c *tool.Call, e tool.ElicitRequest) tool.Decision {
	key := sessionKey(c)
	declined := tool.Decision{Outcome: "denied"}
	if c.Presence.SingleQuery {
		return declined
	}
	surface := e.Surface
	if surface == "" {
		surface = "mcp-elicitation"
	}
	title := e.Title
	if title == "" {
		title = "Confirm this action?"
	}
	scoped := e.RememberKey != ""
	apiRun := c.Origin.Platform == "api_server" && !c.Presence.Cron && !c.Presence.SingleQuery
	if gatewayContext(c) || apiRun {
		if !hasNotifier(c) {
			slog.WarnContext(ctx, "Elicitation requested in gateway session but no notifier is registered — failing closed", "session", key)
			return declined
		}
		decision := g.awaitGateway(ctx, c, tool.ApprovalPrompt{Command: e.Message, Description: e.Description,
			PatternKey: "mcp_elicitation", PatternKeys: []string{"mcp_elicitation"}, AllowSession: scoped, AllowPermanent: scoped}, surface)
		if decision.notifyFailed {
			return declined
		}
		if !decision.resolved || decision.cancelled != "" {
			return tool.Decision{Outcome: "cancelled"}
		}
		return g.rememberedConsent(ctx, key, decision.choice, e.RememberKey, "denied")
	}
	if NoUserCanAnswer(c) {
		slog.InfoContext(ctx, "Elicitation consent declined: no user can answer in this session", "surface", surface)
		return declined
	}
	hookPrompt := tool.ApprovalPrompt{Command: e.Message, Description: e.Description, PatternKey: "mcp_elicitation",
		PatternKeys: []string{"mcp_elicitation"}, SessionKey: key, Surface: surface}
	g.o.Hooks.Approval(ctx, "pre_approval_request", approvalHook(c, hookPrompt, ""))
	choice := g.prompt(ctx, c, tool.ApprovalPrompt{ID: newRequestID(), SessionKey: key, Title: title, Command: e.Message,
		Description: e.Description, PatternKey: "mcp_elicitation", PatternKeys: []string{"mcp_elicitation"},
		AllowSession: scoped, AllowPermanent: scoped, Surface: surface})
	g.o.Hooks.Approval(ctx, "post_approval_response", approvalHook(c, hookPrompt, choice.Answer))
	return g.rememberedConsent(ctx, key, choice.Answer, e.RememberKey, "cancelled")
}

type Confirmation struct {
	Gateway      bool
	Choice       string
	Resolved     bool
	NotifyFailed bool
	Withdrawn    string
	NoHuman      bool
	Cause        string
}

func (g *Gate) ApproveOnce(ctx context.Context, c *tool.Call, p tool.ApprovalPrompt) Confirmation {
	key := sessionKey(c)
	if hasNotifier(c) {
		surface := p.Surface
		if surface == "" {
			surface = "gateway"
		}
		decision := g.awaitGateway(ctx, c, tool.ApprovalPrompt{Command: p.Command, Description: p.Description,
			PatternKey: p.PatternKey, PatternKeys: p.PatternKeys}, surface)
		return Confirmation{Gateway: true, Choice: decision.choice, Resolved: decision.resolved,
			NotifyFailed: decision.notifyFailed, Withdrawn: decision.cancelled}
	}
	if c.Surface == nil || NoUserCanAnswer(c) {
		return Confirmation{NoHuman: true}
	}
	hookPrompt := tool.ApprovalPrompt{Command: p.Command, Description: p.Description, PatternKey: p.PatternKey,
		PatternKeys: append([]string(nil), p.PatternKeys...), SessionKey: key, Surface: "cli"}
	g.o.Hooks.Approval(ctx, "pre_approval_request", approvalHook(c, hookPrompt, ""))
	choice := g.prompt(ctx, c, tool.ApprovalPrompt{ID: newRequestID(), SessionKey: key, Command: p.Command,
		Description: p.Description, PatternKey: p.PatternKey, PatternKeys: append([]string(nil), p.PatternKeys...),
		Surface: "cli"})
	g.o.Hooks.Approval(ctx, "post_approval_response", approvalHook(c, hookPrompt, choice.Answer))
	out := Confirmation{Choice: choice.Answer}
	if choice.Answer == "cancelled" {
		out.Cause = choice.Reason
	}
	return out
}

var _ tool.Approver = (*Gate)(nil)
