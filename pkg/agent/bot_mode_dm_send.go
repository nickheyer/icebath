package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/botmode"
	"github.com/nickheyer/icebath/pkg/botmode/dmdelivery"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	shellutil "github.com/nickheyer/icebath/pkg/utils/shell"
)

// Output tail a delivery runner's completion carries, a whole reply plus its attribution header
const replyCompletionChars = messageMaxChars + 2000

var (
	botPeerTargetRE  = regexp.MustCompile(`^([a-z0-9][a-z0-9_-]{0,63})/([a-zA-Z0-9][a-zA-Z0-9_-]{0,63})$`)
	botLocalTargetRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
)

// Terminal tool the delivery runner is launched through, the seam a test swaps
var botDMTerminalTool = tools.TerminalTool

func init() {
	tools.RegisterInlineToolExecutor(messageAgentToolName, agentInlineExecutor(messageAgentToolName,
		func(a *AIAgent, ctx context.Context, args map[string]any, ictx tools.InlineToolContext) (string, error) {
			return messageAgentTool(ctx, a, botDMArgText(args["target"]), botDMArgText(args["message"]), ictx.TaskID)
		}))
}

// Text of a tool argument, empty for a missing or falsy one
func botDMArgText(value any) string {
	if !anyutil.IsTrue(value) {
		return ""
	}
	return anyutil.String(value)
}

// Local profile folder id a target names, by handle, exact folder id, then one unambiguous friendly name
func resolveLocalName(target string, roster []string, root *string) (string, bool) {
	want := strings.ToLower(strings.TrimSpace(target))
	if want == "" {
		return "", false
	}
	want = botmode.ProfileForHandle(want, roster)
	for _, name := range roster {
		if strings.ToLower(name) == want {
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
	payload := jsonutil.ObjectOf("error", message, "reason", tools.ClassifyAgentError(message))
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
func botDMDumps(payload *jsonutil.Object) string {
	text, err := jsonutil.Encode(payload, jsonutil.Spaced(), jsonutil.ASCII())
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

	body := strings.TrimSpace(message)
	if body == "" {
		return botDMErr("message is required — compose what you want to say to that agent.", nil, nil), nil
	}
	if n := utf8.RuneCountInString(body); n > messageMaxChars {
		return botDMErr(fmt.Sprintf("message too long (%d chars > %d). "+
			"Send the essentials; share large content as a file path instead.", n, messageMaxChars), nil, nil), nil
	}
	rawTarget := strings.TrimLeft(strings.TrimSpace(target), "@")
	if rawTarget == "" {
		return rosterErr("target is required."), nil
	}
	meHome, ok := rosterHomes[me]
	if !ok {
		meHome = h
	}
	content := fmt.Sprintf("Message from 🤖 %s (@%s): ", botmode.DisplayName(me, meHome, roster), botmode.Handle(me, roster)) + body
	author := jsonutil.ObjectOf("id", "bot:"+me, "name", botmode.Handle(me, roster), "is_bot", true)

	peerMatch := botPeerTargetRE.FindStringSubmatch(rawTarget)
	if peerMatch != nil || slices.Contains(peers, strings.ToLower(rawTarget)) {
		peerName, peerProfile := strings.ToLower(rawTarget), ""
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
		peerAuthor := jsonutil.ObjectOf("id", provider.BotAuthorID(me, provider.LocalOrigin()), "name", botmode.Handle(me, roster), "is_bot", true)
		return startDelivery(ctx, []string{botmode.IcebathCLI(), "-p", botmode.ProfileName(root), "peer", "dm", dmTarget}, content,
			fmt.Sprintf("@%s on peer '%s'", who, peerName), true, taskID, a, "", peerAuthor)
	}

	resolved, found := resolveLocalName(rawTarget, roster, &root)
	if !found && !botLocalTargetRE.MatchString(rawTarget) {
		return rosterErr(fmt.Sprintf("Invalid target: %s.", errchain.Show(rawTarget))), nil
	}
	if !found || resolved == me {
		if found {
			return botDMErr("You can't message yourself. Pick a teammate from the roster.", nil, nil), nil
		}
		return rosterErr(fmt.Sprintf("No teammate named '%s' on this install or on a registered peer. "+
			"Pick a name from the roster (roles are listed in your system prompt).", rawTarget)), nil
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

// Shell command of the cleanup owning background runner for one DM file, every argv word quoted
func deliveryCommand(argv []string, dmFile string, stdinFile bool, profileHome string, author *jsonutil.Object) (string, error) {
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
		resolved, err := pathutil.Resolve(profileHome)
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
	if author != nil && author.Len() > 0 {
		text, err := jsonutil.Encode(author, jsonutil.ASCII())
		if err != nil {
			return "", err
		}
		runnerArgv = slices.Insert(runnerArgv, 3, "--author", text)
	}
	return shellutil.Join(runnerArgv), nil
}

// Writes the DM file, admits a local delivery through the profile authority and hands the file to the runner
func startDelivery(ctx context.Context, argv []string, content, label string, stdinFile bool, taskID string, a *AIAgent, profileHome string, author *jsonutil.Object) (string, error) {
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
			return botDMDumps(jsonutil.ObjectOf("status", "ambiguous", "delivery_id", deliveryID,
				"error", fmt.Sprintf("Live delivery admission could not be confirmed: %s. Do not resend.", dmdelivery.ExcText(err)),
				"evidence_file", dmFile)), nil
		}
		if record != nil {
			command, err := deliveryCommand(argv, dmFile, false, profileHome, author)
			if err != nil {
				return "", err
			}
			notification, err := jsonutil.DecodeObject(spawnDelivery(ctx, command, label, "", "", taskID, a))
			if err != nil {
				return "", err
			}
			result := jsonutil.ObjectOf("status", record["status"], "delivery_id", record["delivery_id"], "to", label,
				"detail", "Durably queued for the live Bot Chat owner. Do NOT wait or resend; finish your turn.")
			if notifyErr := notification.ValueOr("error", nil); anyutil.IsTrue(notifyErr) {
				result.Set("notification_error", notifyErr)
			} else if procID := notification.ValueOr("process_id", nil); anyutil.IsTrue(procID) {
				result.Set("process_id", procID)
				replyDelivery := notification.ValueOr("reply_delivery", "notification")
				result.Set("reply_delivery", replyDelivery)
				if replyDelivery == "poll" {
					result.Set("detail", "Durably queued for the live Bot Chat owner. Do NOT resend. "+anyutil.String(notification.ValueOr("detail", "")))
				}
			}
			return jsonutil.Encode(result, jsonutil.Spaced(), jsonutil.ASCII())
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
			pkgLog().Error(fmt.Sprintf("message_agent delivery spawn failed: %v", rec))
			answer = botDMErr(fmt.Sprintf("Delivery to %s could not be started: %v", label, rec), nil, nil)
		}
		if dmFile != "" && !transferred {
			dmdelivery.UnlinkDMFile(dmFile)
		}
	}()
	raw := botDMTerminalTool(ctx, a.env, tools.TerminalRequest{Command: command, Background: true, NotifyOnComplete: true,
		TaskID: taskID, Workdir: pm.RepoRoot(), HostLocal: true, CompletionOutputChars: replyCompletionChars})
	parsed := jsonutil.NewObject()
	if decoded, err := jsonutil.Decode(raw); err == nil {
		obj, isObject := decoded.(*jsonutil.Object)
		if !isObject {
			panic(fmt.Sprintf("the terminal tool replied with %s where a mapping was expected", errchain.Kind(decoded)))
		}
		parsed = obj
	}
	procID := ""
	if id := parsed.ValueOr("session_id", nil); anyutil.IsTrue(id) {
		procID = anyutil.String(id)
	}
	if startErr := parsed.ValueOr("error", nil); anyutil.IsTrue(startErr) {
		return botDMErr(fmt.Sprintf("Delivery to %s failed to start: %s", label, anyutil.String(startErr)), nil, nil)
	}
	if parsed.ValueOr("status", nil) == "pending_approval" {
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
	pollOnly := parsed.ValueOr("notify_on_complete", nil) == false
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
	return botDMDumps(jsonutil.ObjectOf("status", "queued", "delivery_id", deliveryID, "to", label, "reply_delivery", replyDelivery,
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
			pkgLog().Warn(fmt.Sprintf("message_agent: could not persist the reply of %s into session %s: %s",
				procID, sessionID, dmdelivery.ExcText(err)))
		}
	}()
	return true
}
