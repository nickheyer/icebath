package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/botmode"
	"github.com/nickheyer/icebath/pkg/botmode/dmdelivery"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Output tail a delivery runner's completion carries, a whole reply plus its attribution header
const replyCompletionChars = messageMaxChars + 2000

var (
	botPeerTargetRE  = regexp.MustCompile(`^([a-z0-9][a-z0-9_-]{0,63})/([a-zA-Z0-9][a-zA-Z0-9_-]{0,63})$`)
	botLocalTargetRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
)

// Terminal tool the delivery runner is launched through, the seam a test swaps
var botDMTerminalTool = tools.TerminalTool

func botModeDMLog() *slog.Logger { return slog.Default().With("logger", "tools.bot_mode_dm") }

func init() {
	tools.RegisterInlineToolExecutor(messageAgentToolName, agentInlineExecutor(messageAgentToolName,
		func(a *AIAgent, ctx context.Context, args map[string]any, ictx tools.InlineToolContext) (string, error) {
			return messageAgentTool(ctx, a, botDMArgText(args["target"]), botDMArgText(args["message"]), ictx.TaskID)
		}))
}

// Text of a tool argument, empty for a missing or falsy one
func botDMArgText(value any) string {
	if !pyrepr.Truthy(value) {
		return ""
	}
	return pyrepr.Str(value)
}

// Local profile folder id a target names, by handle, exact folder id, then one unambiguous friendly name
func resolveLocalName(target string, roster []string, root *string) (string, bool) {
	want := pystr.Lower(pystr.Strip(target))
	if want == "" {
		return "", false
	}
	want = botmode.ProfileForHandle(want, roster)
	for _, name := range roster {
		if pystr.Lower(name) == want {
			return name, true
		}
	}
	if root == nil {
		return "", false
	}
	aliases := botmode.LocalAliasMap(*root)
	forms := botmode.AliasForms(want)
	forms[want] = true
	hits := map[string]bool{}
	for form := range forms {
		for name := range aliases[form] {
			hits[name] = true
		}
	}
	if len(hits) != 1 {
		return "", false
	}
	for name := range hits {
		return name, true
	}
	return "", false
}

// JSON error answer with its classified reason, the teammates and peers listed when given
func botDMErr(message string, roster, peers *[]string) string {
	payload := pyjson.ObjectOf("error", message, "reason", tools.ClassifyAgentError(message))
	if roster != nil {
		payload.Set("teammates", botDMList(*roster))
	}
	if peers != nil {
		payload.Set("peers", botDMList(*peers))
	}
	return botDMDumps(payload)
}

// JSON list of names, an empty list for none
func botDMList(names []string) []any {
	out := make([]any, 0, len(names))
	for _, name := range names {
		out = append(out, name)
	}
	return out
}

// JSON text of an answer whose values are text, numbers, booleans and lists of text
func botDMDumps(payload *pyjson.Object) string {
	text, err := pyjson.Dumps(payload)
	if err != nil {
		panic(err)
	}
	return text
}

// Delivers a message to a teammate's Bot Chat, answering the JSON ack or error the model reads
func messageAgentTool(ctx context.Context, a *AIAgent, target, message, taskID string) (string, error) {
	h := botModeAgentHome(a)
	if refusal, refused := botModeGate(a, h); refused {
		return refusal, nil
	}
	observability.RecordFeatureUsed(ctx, "bot_mode", h)

	root, me := botmode.InstallRoot(h), botmode.ProfileName(h)
	rosterHomes := map[string]string{}
	var roster []string
	for _, profile := range botmode.Roster(root) {
		if _, seen := rosterHomes[profile.Name]; !seen {
			roster = append(roster, profile.Name)
		}
		rosterHomes[profile.Name] = profile.Dir
	}
	peers := botmode.Peers(root)
	teammates := []string{}
	for _, name := range roster {
		if name != me {
			teammates = append(teammates, botmode.Handle(name, roster))
		}
	}
	rosterErr := func(msg string) string { return botDMErr(msg, &teammates, &peers) }

	body := pystr.Strip(message)
	if body == "" {
		return botDMErr("message is required — compose what you want to say to that agent.", nil, nil), nil
	}
	if n := utf8.RuneCountInString(body); n > messageMaxChars {
		return botDMErr(fmt.Sprintf("message too long (%d chars > %d). "+
			"Send the essentials; share large content as a file path instead.", n, messageMaxChars), nil, nil), nil
	}
	rawTarget := strings.TrimLeft(pystr.Strip(target), "@")
	if rawTarget == "" {
		return rosterErr("target is required."), nil
	}
	meHome, ok := rosterHomes[me]
	if !ok {
		meHome = h
	}
	content := fmt.Sprintf("Message from 🤖 %s (@%s): ", botmode.DisplayName(me, meHome, roster), botmode.Handle(me, roster)) + body
	author := pyjson.ObjectOf("id", "bot:"+me, "name", botmode.Handle(me, roster), "is_bot", true)

	peerMatch := botPeerTargetRE.FindStringSubmatch(rawTarget)
	if peerMatch != nil || slices.Contains(peers, pystr.Lower(rawTarget)) {
		peerName, peerProfile := pystr.Lower(rawTarget), ""
		if peerMatch != nil {
			peerName, peerProfile = peerMatch[1], peerMatch[2]
		}
		if !slices.Contains(peers, peerName) {
			return rosterErr(fmt.Sprintf("No registered peer named '%s'.", peerName)), nil
		}
		dmTarget, who := peerName, peerName
		if peerProfile != "" {
			dmTarget, who = peerName+"/"+peerProfile, peerProfile
		}
		peerAuthor := pyjson.ObjectOf("id", provider.BotAuthorID(me, provider.LocalOrigin()), "name", botmode.Handle(me, roster), "is_bot", true)
		return startDelivery(ctx, []string{botmode.IcebathCLI(), "-p", botmode.ProfileName(root), "peer", "dm", dmTarget}, content,
			fmt.Sprintf("@%s on peer '%s'", who, peerName), true, taskID, a, "", peerAuthor)
	}

	if strings.Contains(rawTarget, "@") {
		if relayed, ok := tryRelayDelivery(ctx, h, rawTarget, content, me, taskID, a); ok {
			return relayed, nil
		}
	}
	resolved, found := resolveLocalName(rawTarget, roster, &root)
	if !found && !botLocalTargetRE.MatchString(rawTarget) && !strings.Contains(rawTarget, "@") {
		return rosterErr(fmt.Sprintf("Invalid target: %s.", pyrepr.StrRepr(rawTarget))), nil
	}
	if !found || resolved == me {
		if relayed, ok := tryRelayDelivery(ctx, h, rawTarget, content, me, taskID, a); ok {
			return relayed, nil
		}
		if found {
			return botDMErr("You can't message yourself. Pick a teammate from the roster.", nil, nil), nil
		}
		return rosterErr(fmt.Sprintf("No teammate named '%s' on this install, on a connected "+
			"machine, or on a registered peer. Pick a name from the roster "+
			"(roles are listed in your system prompt).", rawTarget)), nil
	}
	return startDelivery(ctx, []string{botmode.IcebathCLI(), "-p", resolved}, content, "@"+botmode.Handle(resolved, roster),
		false, taskID, a, rosterHomes[resolved], author)
}

// Refusal of a session that is not a Bot Chat of a Bot Mode managed install, a failed check refusing too
func botModeGate(a *AIAgent, h string) (refusal string, refused bool) {
	defer func() {
		if rec := recover(); rec != nil {
			refusal, refused = botDMErr(fmt.Sprintf("Bot Mode gate check failed: %v", rec), nil, nil), true
		}
	}()
	if botModeSessionTitle(a) != botmode.BotChatTitle {
		return botDMErr("message_agent is only available in a Bot Mode 'Bot Chat' session. "+
			"This session is not one; do not retry.", nil, nil), true
	}
	if !botmode.IsBotModeManaged(h) {
		return botDMErr("This install is not Bot-Mode-managed (no bot roster); "+
			"message_agent is unavailable. Do not retry.", nil, nil), true
	}
	return "", false
}

// Cross connection delivery through the Desktop relay, false when the target resolves against no relay roster row
func tryRelayDelivery(ctx context.Context, root, rawTarget, content, me, taskID string, a *AIAgent) (answer string, ok bool) {
	defer func() {
		if rec := recover(); rec != nil {
			botModeDMLog().Debug("relay delivery attempt failed", "error", rec)
			answer, ok = "", false
		}
	}()
	failed := func(err error) (string, bool) {
		botModeDMLog().Debug("relay delivery attempt failed", "error", err)
		return "", false
	}
	roster := botmode.ReadRemoteRoster(root)
	if len(roster) == 0 {
		return "", false
	}
	match, ambiguous := botmode.ResolveRemoteTarget(rawTarget, roster)
	if ambiguous {
		want, _, _ := strings.Cut(strings.TrimLeft(pystr.Strip(rawTarget), "@"), "@")
		want = pystr.Lower(want)
		taken, err := botmode.LocalTakenForms(root)
		if err != nil {
			return failed(err)
		}
		var forms []string
		for i, form := range botmode.RemoteTargetForms(roster, taken) {
			if i < len(roster) && botmode.TargetAliases(roster[i])[want] {
				forms = append(forms, form)
			}
		}
		return botDMErr(fmt.Sprintf("'%s' exists on several connected machines — disambiguate with one of: %s.",
			rawTarget, strings.Join(forms, ", ")), nil, nil), true
	}
	if match == nil {
		return "", false
	}
	var names []string
	for _, profile := range botmode.Roster(botmode.InstallRoot(root)) {
		names = append(names, profile.Name)
	}
	envelope, err := botmode.EnqueueEnvelope(ctx, root, *match, content, me, botmode.Handle(me, names))
	var refusedErr *botmode.EnvelopeRefusedError
	if errors.As(err, &refusedErr) {
		return botDMDumps(pyjson.ObjectOf("error", refusedErr.Error(), "reason", refusedErr.Reason)), true
	}
	if err != nil {
		return failed(err)
	}
	where := match.ConnectionLabel
	if where == "" {
		where = match.ConnectionID
	}
	label := fmt.Sprintf("@%s on %s", match.Handle, where)
	command, err := botmode.WaiterCommand(root, envelope)
	if err != nil {
		return failed(err)
	}
	envelopeID := pyrepr.Str(envelope.GetOr("id", ""))
	raw := spawnDelivery(ctx, command, label, "", envelopeID, taskID, a)
	parsed, err := pyjson.LoadsDict(raw)
	if err != nil {
		return failed(err)
	}
	waiterError := parsed.GetOr("error", nil)
	if !pyrepr.Truthy(waiterError) {
		return raw, true
	}
	return botDMDumps(pyjson.ObjectOf("status", "queued", "delivery_id", envelopeID, "to", label, "notification_error", waiterError,
		"detail", fmt.Sprintf("Message queued for %s; the relay delivers it on its own, but the reply "+
			"waiter did not start, so the reply will NOT wake you. Do NOT resend.", label))), true
}

// Shell command of the cleanup owning background runner for one DM file, every argv word quoted
func deliveryCommand(argv []string, dmFile string, stdinFile bool, profileHome string, author *pyjson.Object) (string, error) {
	executable, err := os.Executable()
	if err != nil || executable == "" {
		executable = "icebath"
	}
	mode := "query-file"
	if stdinFile {
		mode = "stdin"
	}
	runnerArgv := []string{executable, botmode.DMRunnerEntry, "--run-delivery", mode, dmFile}
	if profileHome != "" {
		resolved, err := pypath.Resolve(profileHome)
		if err != nil {
			return "", err
		}
		runnerArgv = append(runnerArgv, "--profile-home", resolved)
	}
	runnerArgv = append(runnerArgv, argv...)
	if runtime.GOOS == "windows" {
		for i, part := range runnerArgv {
			runnerArgv[i] = strings.ReplaceAll(part, "\\", "/")
		}
	}
	if author != nil && len(author.Keys) > 0 {
		text, err := pyjson.Dumps(author, pyjson.Separators(",", ":"))
		if err != nil {
			return "", err
		}
		runnerArgv = slices.Insert(runnerArgv, 3, "--author", text)
	}
	return pyshlex.Join(runnerArgv), nil
}

// Writes the DM file, admits a local delivery through the profile authority and hands the file to the runner
func startDelivery(ctx context.Context, argv []string, content, label string, stdinFile bool, taskID string, a *AIAgent, profileHome string, author *pyjson.Object) (string, error) {
	dmFile, err := dmdelivery.WriteDMFile(content)
	if err != nil {
		return "", err
	}
	if profileHome != "" {
		record, err := dmdelivery.AdmitLiveDM(ctx, profileHome, dmFile, author)
		if err != nil {
			var refusal dmdelivery.LiveTargetError
			if errors.As(err, &refusal) {
				return dmdelivery.SettledNoSend(dmFile, refusal)
			}
			deliveryID, idErr := dmdelivery.DMDeliveryID(dmFile)
			if idErr != nil {
				return "", idErr
			}
			return botDMDumps(pyjson.ObjectOf("status", "ambiguous", "delivery_id", deliveryID,
				"error", fmt.Sprintf("Live delivery admission could not be confirmed: %s. Do not resend.", dmdelivery.ExcText(err)),
				"evidence_file", dmFile)), nil
		}
		if record != nil {
			command, err := deliveryCommand(argv, dmFile, false, profileHome, author)
			if err != nil {
				return "", err
			}
			notification, err := pyjson.LoadsDict(spawnDelivery(ctx, command, label, "", "", taskID, a))
			if err != nil {
				return "", err
			}
			result := pyjson.ObjectOf("status", record["status"], "delivery_id", record["delivery_id"], "to", label,
				"detail", "Durably queued for the live Bot Chat owner. Do NOT wait or resend; finish your turn.")
			if notifyErr := notification.GetOr("error", nil); pyrepr.Truthy(notifyErr) {
				result.Set("notification_error", notifyErr)
			} else if procID := notification.GetOr("process_id", nil); pyrepr.Truthy(procID) {
				result.Set("process_id", procID)
				replyDelivery := notification.GetOr("reply_delivery", "notification")
				result.Set("reply_delivery", replyDelivery)
				if replyDelivery == "poll" {
					result.Set("detail", "Durably queued for the live Bot Chat owner. Do NOT resend. "+pyrepr.Str(notification.GetOr("detail", "")))
				}
			}
			return pyjson.Dumps(result)
		}
	}
	command, err := deliveryCommand(argv, dmFile, stdinFile, profileHome, author)
	if err != nil {
		dmdelivery.UnlinkDMFile(dmFile)
		return "", err
	}
	return spawnDelivery(ctx, command, label, dmFile, "", taskID, a), nil
}

// Launches the background runner and acks the hand off, the DM file removed unless the runner took it over
func spawnDelivery(ctx context.Context, command, label, dmFile, deliveryID, taskID string, a *AIAgent) (answer string) {
	transferred := false
	defer func() {
		if rec := recover(); rec != nil {
			botModeDMLog().Error(fmt.Sprintf("message_agent delivery spawn failed: %v", rec))
			answer = botDMErr(fmt.Sprintf("Delivery to %s could not be started: %v", label, rec), nil, nil)
		}
		if dmFile != "" && !transferred {
			dmdelivery.UnlinkDMFile(dmFile)
		}
	}()
	raw := botDMTerminalTool(ctx, a.env, tools.TerminalRequest{Command: command, Background: true, NotifyOnComplete: true,
		TaskID: taskID, Workdir: pm.RepoRoot(), HostLocal: true, CompletionOutputChars: replyCompletionChars})
	parsed := pyjson.NewObject()
	if decoded, err := pyjson.Loads(raw); err == nil {
		obj, isObject := decoded.(*pyjson.Object)
		if !isObject {
			panic(fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(decoded)))
		}
		parsed = obj
	}
	procID := ""
	if id := parsed.GetOr("session_id", nil); pyrepr.Truthy(id) {
		procID = pyrepr.Str(id)
	}
	if startErr := parsed.GetOr("error", nil); pyrepr.Truthy(startErr) {
		return botDMErr(fmt.Sprintf("Delivery to %s failed to start: %s", label, pyrepr.Str(startErr)), nil, nil)
	}
	if parsed.GetOr("status", nil) == "pending_approval" {
		tail := "."
		if dmFile != "" {
			tail = ", so nothing was sent. Approve it (or add it to command_allowlist) and send again."
		}
		return botDMErr(fmt.Sprintf("Delivery to %s failed to start: its command needs terminal approval that nobody "+
			"in this turn can grant", label)+tail, nil, nil)
	}
	if procID == "" {
		return botDMErr(fmt.Sprintf("Delivery to %s failed to start: no process id returned", label), nil, nil)
	}
	transferred = true
	pollOnly := parsed.GetOr("notify_on_complete", nil) == false
	var detail, replyDelivery string
	if pollOnly {
		replyDelivery = "poll"
		detail = fmt.Sprintf("Message handed to a background delivery process for %s, but THIS session "+
			"cannot receive completion notifications, so the reply will NOT arrive on its own. "+
			"Before ending your turn, retrieve the outcome with process(action='wait', "+
			"session_id='%s') — its output is the reply (relay it, attributed to that "+
			"agent) or the delivery failure (report it; the message was NOT delivered); "+
			"if wait returns status=timeout, call wait again until the process exits.", label, procID)
		if persistReplyWhenDone(ctx, procID, a) {
			detail += " Its outcome is also saved into this session's transcript as a delivery row " +
				"when the process exits, so it survives even if the turn ends first."
		}
	} else {
		replyDelivery = "notification"
		detail = fmt.Sprintf("Message queued for %s: this acknowledges the hand-off to a "+
			"background delivery process, not a delivery receipt — do NOT wait or poll. "+
			"Finish your turn now; that process's completion notification carries the "+
			"delivery outcome — the reply (relay it then, attributed to that agent) or "+
			"the delivery failure (report it; the message was NOT delivered).", label)
	}
	if deliveryID == "" && dmFile != "" {
		id, err := dmdelivery.DMDeliveryID(dmFile)
		if err != nil {
			panic(dmdelivery.ExcText(err))
		}
		deliveryID = id
	}
	return botDMDumps(pyjson.ObjectOf("status", "queued", "delivery_id", deliveryID, "to", label, "reply_delivery", replyDelivery,
		"detail", detail, "process_id", procID, "queued_at", time.Now().Unix()))
}

// Store that can append one transcript row, the session database of an agent that owns a transcript
type botDMMessageAppender interface {
	AppendMessage(ctx context.Context, sessionID string, fields map[string]any, guards state.AppendGuards) (int64, error)
}

// Saves an unread runner outcome as a delivery row of the sender's transcript, false when nothing was armed
func persistReplyWhenDone(ctx context.Context, procID string, a *AIAgent) bool {
	registry := tools.DefaultProcessRegistry()
	proc := registry.Get(a.env, procID)
	db, canAppend := sessionStoreOrNil(a.sessionDB).(botDMMessageAppender)
	sessionID := a.sessionID
	if proc == nil || sessionID == "" || !canAppend {
		return false
	}
	appendCtx := context.WithoutCancel(ctx)
	go func() {
		evt, unread := registry.UnreadCompletionEvent(procID, proc)
		if !unread {
			return
		}
		_, err := db.AppendMessage(appendCtx, sessionID, map[string]any{
			"role": "user", "content": tools.FormatProcessNotification(evt), "display_kind": tools.ProcessCompleteDisplayKind,
			"display_metadata": map[string]any{"display_text": tools.ProcessCompletionDisplayText([]map[string]any{evt})},
		}, state.AppendGuards{})
		if err != nil {
			botModeDMLog().Warn(fmt.Sprintf("message_agent: could not persist the reply of %s into session %s: %s",
				procID, sessionID, dmdelivery.ExcText(err)))
		}
	}()
	return true
}
