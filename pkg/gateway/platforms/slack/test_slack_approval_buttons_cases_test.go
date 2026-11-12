package slack

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Adapter of the approval cases, workspace T1 owning channel C1 with bot U_BOT
func approvalAdapter(t *testing.T) (*SlackAdapter, *fakeSlackAPI) {
	a, f, _ := newHarnessAdapter(t, nil)
	a.teamClients.Set("T1", newWebClient("xoxb-test-token", ""))
	a.teamBotUserIDs = map[string]string{"T1": "U_BOT"}
	a.channelTeam.Set("C1", "T1")
	return a, f
}

// Runner double whose authorization check records every source it sees
type authRunner struct {
	authFn      func(source *gateway.SessionSource) bool
	seenSources []*gateway.SessionSource
}

func (r *authRunner) IsUserAuthorized(_ context.Context, source *gateway.SessionSource, _ bool) (bool, error) {
	r.seenSources = append(r.seenSources, source)
	return r.authFn(source), nil
}

// Binds the runner as the message handler owner the way set_message_handler binds runner.handle
func attachAuthRunner(a *SlackAdapter, authFn func(*gateway.SessionSource) bool) *authRunner {
	if authFn == nil {
		authFn = func(*gateway.SessionSource) bool { return true }
	}
	r := &authRunner{authFn: authFn}
	a.SetBoundMessageHandler(r, func(context.Context, *gateway.MessageEvent) (any, error) { return nil, nil })
	return r
}

// Ack double counting its calls
type ackRecorder struct{ calls int }

func (r *ackRecorder) ack(map[string]any) error { r.calls++; return nil }

func blocksOf(t *testing.T, args map[string]any) []any {
	t.Helper()
	blocks, ok := args["blocks"].([]any)
	if !ok {
		t.Fatalf("no blocks in %v", args)
	}
	return blocks
}

func TestSlackExecApprovalSendsBlocksWithButtons(t *testing.T) {
	a, f := approvalAdapter(t)
	f.respond("chat.postMessage", map[string]any{"ok": true, "ts": "1234.5678"})
	desc := "dangerous deletion"
	result, err := a.SendExecApproval(context.Background(), "C1", "rm -rf /important", "agent:main:slack:group:C1:1111", &desc, nil, true, true, false, nil)
	if err != nil || !result.Success || result.MessageID != "1234.5678" {
		t.Fatalf("result %+v err %v", result, err)
	}
	calls := f.callsTo("chat.postMessage")
	if len(calls) != 1 {
		t.Fatalf("chat.postMessage calls %d", len(calls))
	}
	blocks := blocksOf(t, calls[0].Args)
	if len(blocks) != 2 {
		t.Fatalf("blocks %v", blocks)
	}
	section := blocks[0].(map[string]any)
	text := section["text"].(map[string]any)["text"].(string)
	if section["type"] != "section" || !strings.Contains(text, "rm -rf /important") || !strings.Contains(text, "dangerous deletion") {
		t.Fatalf("section %v", section)
	}
	actions := blocks[1].(map[string]any)
	elements := actions["elements"].([]any)
	if actions["type"] != "actions" || len(elements) != 4 {
		t.Fatalf("actions %v", actions)
	}
	ids := map[string]bool{}
	for _, e := range elements {
		el := e.(map[string]any)
		ids[el["action_id"].(string)] = true
		if el["value"] != "agent:main:slack:group:C1:1111" {
			t.Fatalf("button value %v", el["value"])
		}
	}
	for _, id := range []string{"hermes_approve_once", "hermes_approve_session", "hermes_approve_always", "hermes_deny"} {
		if !ids[id] {
			t.Fatalf("missing %s in %v", id, ids)
		}
	}
}

func TestSlackExecApprovalSmartDenyOwnerOverrideHidesPersistentButtons(t *testing.T) {
	a, f := approvalAdapter(t)
	f.respond("chat.postMessage", map[string]any{"ok": true, "ts": "1234.5678"})
	if _, err := a.SendExecApproval(context.Background(), "C1", "rm -rf /", "s", nil, nil, false, true, true, nil); err != nil {
		t.Fatal(err)
	}
	blocks := blocksOf(t, f.callsTo("chat.postMessage")[0].Args)
	var ids []string
	for _, e := range blocks[1].(map[string]any)["elements"].([]any) {
		ids = append(ids, e.(map[string]any)["action_id"].(string))
	}
	if strings.Join(ids, ",") != "hermes_approve_once,hermes_deny" {
		t.Fatalf("buttons %v", ids)
	}
	if text := blocks[0].(map[string]any)["text"].(map[string]any)["text"].(string); !strings.Contains(strings.ToLower(text), "one operation") {
		t.Fatalf("section %q", text)
	}
}

// Section text of the first chat.update block
func updatedSectionText(t *testing.T, f *fakeSlackAPI) string {
	t.Helper()
	calls := f.callsTo("chat.update")
	if len(calls) == 0 {
		t.Fatal("no chat.update call")
	}
	return blocksOf(t, calls[len(calls)-1].Args)[0].(map[string]any)["text"].(map[string]any)["text"].(string)
}

func TestSlackApprovalActionTruncatesInflatedOriginalText(t *testing.T) {
	a, f := approvalAdapter(t)
	attachAuthRunner(a, nil)
	a.approvalResolved.Set("1.2", false)
	prev := resolveGatewayApprovalFn
	resolveGatewayApprovalFn = func(string, string, bool, string, string) int { return 1 }
	t.Cleanup(func() { resolveGatewayApprovalFn = prev })
	inflated := strings.Repeat("a", 2990) + strings.Repeat("&amp;", 10)
	body := map[string]any{"message": map[string]any{"ts": "1.2", "blocks": []any{
		map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": inflated}}}},
		"channel": map[string]any{"id": "C1"}, "user": map[string]any{"name": "alice", "id": "U_ALICE"}}
	ack := &ackRecorder{}
	a.handleApprovalAction(context.Background(), ack.ack, body, map[string]any{"action_id": "hermes_approve_once", "value": "session-key"})
	if n := pystr.Len(updatedSectionText(t, f)); n > 3000 {
		t.Fatalf("section text %d chars", n)
	}
}

func TestSlackApprovalActionGlobalAllowlistBlocksUnauthorizedClick(t *testing.T) {
	a, _ := approvalAdapter(t)
	a.approvalResolved.Set("1234.5678", false)
	t.Setenv("SLACK_ALLOWED_USERS", "")
	t.Setenv("SLACK_ALLOW_ALL_USERS", "")
	t.Setenv("GATEWAY_ALLOW_ALL_USERS", "")
	t.Setenv("GATEWAY_ALLOWED_USERS", "U_OWNER")
	resolved := 0
	prev := resolveGatewayApprovalFn
	resolveGatewayApprovalFn = func(string, string, bool, string, string) int { resolved++; return 1 }
	t.Cleanup(func() { resolveGatewayApprovalFn = prev })
	body := map[string]any{"message": map[string]any{"ts": "1234.5678", "blocks": []any{}},
		"channel": map[string]any{"id": "C1"}, "user": map[string]any{"name": "mallory", "id": "U_ATTACKER"}}
	ack := &ackRecorder{}
	a.handleApprovalAction(context.Background(), ack.ack, body, map[string]any{"action_id": "hermes_approve_once", "value": "agent:main:slack:group:C1:1111"})
	if ack.calls != 1 || resolved != 0 {
		t.Fatalf("ack calls %d, resolve calls %d", ack.calls, resolved)
	}
}

func TestSlackInteractiveAuthDelegatesToGatewayRunnerAuth(t *testing.T) {
	a, _ := approvalAdapter(t)
	runner := attachAuthRunner(a, func(s *gateway.SessionSource) bool { return deref(s.UserID) == "U_OK" })
	operator, intruder := "operator", "intruder"
	if !a.isInteractiveUserAuthorized(context.Background(), "U_OK", "C1", &operator, "") {
		t.Fatal("runner approved user rejected")
	}
	if a.isInteractiveUserAuthorized(context.Background(), "U_BAD", "C1", &intruder, "") {
		t.Fatal("runner rejected user approved")
	}
	if len(runner.seenSources) != 2 {
		t.Fatalf("sources %d", len(runner.seenSources))
	}
	s := runner.seenSources[0]
	if s.Platform != gateway.PlatformSlack || s.ChatID != "C1" || s.ChatType != "group" {
		t.Fatalf("source %+v", s)
	}
}

func TestSlackSlashConfirmActionTruncatesInflatedOriginalText(t *testing.T) {
	a, f := approvalAdapter(t)
	attachAuthRunner(a, nil)
	a.approvalResolved.Set("2222.3333", false)
	prev := gateway.SlashConfirmResolve
	gateway.SlashConfirmResolve = func(context.Context, string, string, string) string { return "ok" }
	t.Cleanup(func() { gateway.SlashConfirmResolve = prev })
	inflated := strings.Repeat("b", 2990) + strings.Repeat("&lt;", 10)
	body := map[string]any{"message": map[string]any{"ts": "2222.3333", "blocks": []any{
		map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": inflated}}}},
		"channel": map[string]any{"id": "C1"}, "user": map[string]any{"name": "owner", "id": "U_OWNER"}}
	ack := &ackRecorder{}
	a.handleSlashConfirmAction(context.Background(), ack.ack, body, map[string]any{"action_id": "hermes_confirm_once", "value": "agent:main:slack:group:C1:1111|confirm-1"})
	if n := pystr.Len(updatedSectionText(t, f)); n > 3000 {
		t.Fatalf("section text %d chars", n)
	}
}

func TestSlackThreadContextIncludesSelfBotRepliesAsAssistantOnColdStart(t *testing.T) {
	a, f := approvalAdapter(t)
	f.respond("conversations.replies", map[string]any{"ok": true, "messages": []any{
		map[string]any{"ts": "1000.0", "user": "U1", "text": "Parent"},
		map[string]any{"ts": "1000.1", "bot_id": "B_SELF", "user": "U_BOT", "text": "Previous bot self-reply"},
		map[string]any{"ts": "1000.15", "bot_id": "B_OTHER", "user": "U_OTHER_BOT", "text": "Deploy succeeded"},
		map[string]any{"ts": "1000.2", "user": "U1", "text": "Current"}}})
	a.userNameCache.Set(teamKey{"T1", "U1"}, "Alice")
	a.userNameCache.Set(teamKey{"T1", "U_OTHER_BOT"}, "DeployBot")
	got := a.fetchThreadContext(context.Background(), "C1", "1000.0", "1000.2", "T1", 30, "", false)
	for _, want := range []string{"Alice: Parent", "[assistant] Previous bot self-reply", "Deploy succeeded"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "[assistant] Alice") {
		t.Fatalf("human labelled assistant in %q", got)
	}
}

func TestSlackThreadContextFetchThreadContextExtractsBlockKitParent(t *testing.T) {
	a, f := approvalAdapter(t)
	f.respond("conversations.replies", map[string]any{"ok": true, "messages": []any{
		map[string]any{"ts": "1000.0", "bot_id": "B_ALERT", "subtype": "bot_message", "username": "alertbot", "text": "low_alerts (checkout)",
			"blocks": []any{
				map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "*Trigger fired:* low_alerts"}},
				map[string]any{"type": "actions", "elements": []any{map[string]any{"type": "button",
					"text": map[string]any{"type": "plain_text", "text": "View graph"}, "url": "https://example.example/view/abc123"}}}}},
		map[string]any{"ts": "1000.1", "user": "U1", "text": "what's going on?"}}})
	got := a.fetchThreadContext(context.Background(), "C1", "1000.0", "1000.1", "T1", 30, "", false)
	for _, want := range []string{"low_alerts (checkout)", "https://example.example/view/abc123", "[thread parent]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}

func TestSlackThreadContextFetchThreadContextIncludesSelfBotRepliesWithAssistantLabel(t *testing.T) {
	a, f := approvalAdapter(t)
	f.respond("conversations.replies", map[string]any{"ok": true, "messages": []any{
		map[string]any{"ts": "1000.0", "bot_id": "B_CRON", "text": "Cron summary"},
		map[string]any{"ts": "1000.1", "bot_id": "B_SELF", "user": "U_BOT", "text": "Previous self reply"},
		map[string]any{"ts": "1000.2", "user": "U1", "text": "Follow-up question"},
		map[string]any{"ts": "1000.3", "user": "U1", "text": "Current"}}})
	a.userNameCache.Set(teamKey{"T1", "U1"}, "Alice")
	got := a.fetchThreadContext(context.Background(), "C1", "1000.0", "1000.3", "T1", 30, "", false)
	for _, want := range []string{"Cron summary", "[thread parent]", "[assistant] Previous self reply", "Follow-up question"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "Current") {
		t.Fatalf("current message leaked into %q", got)
	}
}

func TestSlackThreadContextFetchThreadContextMultiWorkspace(t *testing.T) {
	a, f := approvalAdapter(t)
	a.teamClients.Set("T2", newWebClient("xoxb-t2", ""))
	a.teamBotUserIDs = map[string]string{"T1": "U_BOT_T1", "T2": "U_BOT_T2"}
	bot := "U_BOT_T1"
	a.botUserID = &bot
	a.channelTeam.Set("C2", "T2")
	f.respond("conversations.replies", map[string]any{"ok": true, "messages": []any{
		map[string]any{"ts": "2000.0", "user": "U2", "text": "Parent T2"},
		map[string]any{"ts": "2000.1", "bot_id": "B_FOREIGN", "user": "U_BOT_T1", "team": "T2", "text": "Cross-workspace bot reply"},
		map[string]any{"ts": "2000.2", "bot_id": "B_SELF_T2", "user": "U_BOT_T2", "team": "T2", "text": "Own T2 bot reply"},
		map[string]any{"ts": "2000.3", "user": "U2", "text": "Current"}}})
	a.userNameCache.Set(teamKey{"T2", "U2"}, "Bob")
	got := a.fetchThreadContext(context.Background(), "C2", "2000.0", "2000.3", "T2", 30, "", false)
	for _, want := range []string{"Parent T2", "Cross-workspace bot reply", "[assistant] Own T2 bot reply"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}

func TestThreadSuspensionRequiresAFreshContext(t *testing.T) {
	a, _ := approvalAdapter(t)
	ctx := context.Background()
	store := gateway.NewSessionStore(ctx, t.TempDir()+"/sessions", &gateway.GatewayConfig{}, nil)
	a.SetSessionStore(store)
	entry, err := store.GetOrCreateSession(ctx, a.threadSessionSource(ctx, "C1", "1000.0", "U123", "", "group"), false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !a.hasActiveSessionForThread(ctx, "C1", "1000.0", "U123", "", "group") {
		t.Fatal("fresh thread session must be active")
	}
	if _, err := store.SuspendSession(ctx, entry.SessionKey); err != nil {
		t.Fatal(err)
	}
	if a.hasActiveSessionForThread(ctx, "C1", "1000.0", "U123", "", "group") {
		t.Fatal("a suspended thread session must not count as active")
	}
}

func TestSessionKeyChatTypeDMThreadNotFoundWithGroupType(t *testing.T) {
	a, _ := approvalAdapter(t)
	ctx := context.Background()
	store := gateway.NewSessionStore(ctx, t.TempDir()+"/sessions", &gateway.GatewayConfig{GroupSessionsPerUser: true}, nil)
	a.SetSessionStore(store)
	if _, err := store.GetOrCreateSession(ctx, a.threadSessionSource(ctx, "D0DMCHANNEL", "2000.0", "U_USER", "", "dm"), false, true); err != nil {
		t.Fatal(err)
	}
	if !a.hasActiveSessionForThread(ctx, "D0DMCHANNEL", "2000.0", "U_USER", "", "dm") {
		t.Fatal("the dm thread session must be found with the dm chat type")
	}
	if a.hasActiveSessionForThread(ctx, "D0DMCHANNEL", "2000.0", "U_USER", "", "group") {
		t.Fatal("the group chat type must not find the dm session")
	}
}

func TestThreadEngagementMentionedThreadsCapEvictsOldestTimestamps(t *testing.T) {
	a, _ := approvalAdapter(t)
	a.mentionedThreadsCap = 4
	for _, ts := range []string{"1000.000002", "999.999999", "1000.000004", "1000.000001", "1000.000003"} {
		a.mentionedThreads[ts] = true
	}
	a.trimMentionedThreads()
	if len(a.mentionedThreads) != 3 || !a.mentionedThreads["1000.000002"] || !a.mentionedThreads["1000.000003"] || !a.mentionedThreads["1000.000004"] {
		t.Fatalf("remaining %v", a.mentionedThreads)
	}
}

// Swaps the inbound message entry for a recorder of the events it receives
func captureSlackMessages(t *testing.T) *[]map[string]any {
	var forwarded []map[string]any
	prev := handleSlackMessageFn
	handleSlackMessageFn = func(_ *SlackAdapter, _ context.Context, event, _ map[string]any) error {
		forwarded = append(forwarded, event)
		return nil
	}
	t.Cleanup(func() { handleSlackMessageFn = prev })
	return &forwarded
}

func reactionEvent(user, reaction, ts, itemUser, eventTS string) map[string]any {
	return map[string]any{"type": "reaction_added", "user": user, "reaction": reaction,
		"item": map[string]any{"type": "message", "channel": "C1", "ts": ts}, "item_user": itemUser, "event_ts": eventTS}
}

func TestSlackReactionForwardingDefaultOffReactionAckedAndDropped(t *testing.T) {
	a, _ := approvalAdapter(t)
	forwarded := captureSlackMessages(t)
	if err := a.handleSlackReaction(context.Background(), reactionEvent("U1", "thumbsup", "2000.0", "U_BOT", "3000.0"), false); err != nil {
		t.Fatal(err)
	}
	if len(*forwarded) != 0 {
		t.Fatalf("forwarded %v", *forwarded)
	}
}

func TestSlackReactionForwardingReactionSynthesizesMessageInThread(t *testing.T) {
	a, f := approvalAdapter(t)
	a.Config.Extra["reaction_triggers"] = true
	f.respond("conversations.replies", map[string]any{"ok": true, "messages": []any{
		map[string]any{"ts": "2000.0", "thread_ts": "1000.0", "user": "U_BOT", "text": "Proposal"}}})
	forwarded := captureSlackMessages(t)
	if err := a.handleSlackReaction(context.Background(), reactionEvent("U1", "thumbsup", "2000.0", "U_BOT", "3000.0"), false); err != nil {
		t.Fatal(err)
	}
	if len(*forwarded) != 1 {
		t.Fatalf("forwarded %v", *forwarded)
	}
	synth := (*forwarded)[0]
	reaction := synth["_hermes_reaction"].(map[string]any)
	if synth["type"] != "message" || synth["user"] != "U1" || synth["text"] != "reaction:added:👍" || synth["channel"] != "C1" ||
		synth["thread_ts"] != "1000.0" || synth["ts"] != "3000.0" || reaction["name"] != "thumbsup" || reaction["action"] != "added" ||
		reaction["reacted_to_ts"] != "2000.0" || synth["_hermes_force_process"] != true {
		t.Fatalf("synthetic event %v", synth)
	}
}

func TestSlackReactionForwardingTargetChannelHandoffRoutesTopLevel(t *testing.T) {
	a, f := approvalAdapter(t)
	a.Config.Extra["reaction_triggers"] = []any{"task"}
	a.Config.Extra["reaction_trigger_target"] = "C_TRAIN"
	f.respond("conversations.replies", map[string]any{"ok": true, "messages": []any{map[string]any{"ts": "1000.0", "user": "U_OTHER", "text": "Handoff me"}}})
	forwarded := captureSlackMessages(t)
	if err := a.handleSlackReaction(context.Background(), reactionEvent("U1", "task", "1000.0", "U_OTHER", "3000.0"), false); err != nil {
		t.Fatal(err)
	}
	if len(*forwarded) != 1 {
		t.Fatalf("forwarded %v", *forwarded)
	}
	synth := (*forwarded)[0]
	if _, has := synth["thread_ts"]; has || synth["channel"] != "C_TRAIN" || synth["_hermes_no_thread_response"] != true || synth["_hermes_reaction_source_channel"] != "C1" {
		t.Fatalf("synthetic event %v", synth)
	}
}

func TestSlackReactionForwardingTargetChannelWithThreadRoutesToThread(t *testing.T) {
	a, f := approvalAdapter(t)
	a.Config.Extra["reaction_triggers"] = []any{"task"}
	a.Config.Extra["reaction_trigger_target"] = "C_TRAIN:1719.0001"
	f.respond("conversations.replies", map[string]any{"ok": true, "messages": []any{map[string]any{"ts": "1000.0", "user": "U_OTHER", "text": "Handoff me"}}})
	forwarded := captureSlackMessages(t)
	if err := a.handleSlackReaction(context.Background(), reactionEvent("U1", "task", "1000.0", "U_OTHER", "3000.0"), false); err != nil {
		t.Fatal(err)
	}
	if len(*forwarded) != 1 {
		t.Fatalf("forwarded %v", *forwarded)
	}
	synth := (*forwarded)[0]
	if _, has := synth["_hermes_no_thread_response"]; has || synth["channel"] != "C_TRAIN" || synth["thread_ts"] != "1719.0001" {
		t.Fatalf("synthetic event %v", synth)
	}
}

func TestSlackReactionForwardingHookFiresEvenWhenRoutingDisabled(t *testing.T) {
	a, _ := approvalAdapter(t)
	var hookEvents []map[string]any
	a.SetReactionHandler(func(_ context.Context, event map[string]any) error {
		hookEvents = append(hookEvents, event)
		return nil
	})
	forwarded := captureSlackMessages(t)
	ctx := context.Background()
	if err := a.handleSlackReaction(ctx, reactionEvent("U1", "thumbsup", "1000.0", "U_BOT", "3000.0"), false); err != nil {
		t.Fatal(err)
	}
	removed := reactionEvent("U1", "thumbsup", "1000.0", "U_BOT", "3001.0")
	removed["type"] = "reaction_removed"
	if err := a.handleSlackReaction(ctx, removed, true); err != nil {
		t.Fatal(err)
	}
	if len(*forwarded) != 0 || len(hookEvents) != 2 || hookEvents[0]["event_name"] != "reaction:added" || hookEvents[1]["event_name"] != "reaction:removed" {
		t.Fatalf("forwarded %v hooks %v", *forwarded, hookEvents)
	}
	first := hookEvents[0]
	if first["platform"] != "slack" || first["reaction"] != "thumbsup" || first["user_id"] != "U1" || first["channel_id"] != "C1" || first["message_ts"] != "1000.0" {
		t.Fatalf("hook event %v", first)
	}
}

func TestSlackReactionForwardingTriggerConfigParsing(t *testing.T) {
	a, _ := approvalAdapter(t)
	ctx := context.Background()
	setOf := func(m map[string]bool) string {
		var keys []string
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return strings.Join(keys, ",")
	}
	if a.slackReactionTriggers(ctx) != nil {
		t.Fatal("default must be off")
	}
	cases := []struct {
		value any
		want  string
		off   bool
	}{{false, "", true}, {true, "", false}, {[]any{"thumbsup", ":task:"}, "task,thumbsup", false},
		{"thumbsup, task", "task,thumbsup", false}, {"all", "", false}, {"false", "", true}}
	for _, c := range cases {
		a.Config.Extra["reaction_triggers"] = c.value
		got := a.slackReactionTriggers(ctx)
		if (got == nil) != c.off || setOf(got) != c.want {
			t.Fatalf("%v gave %v", c.value, got)
		}
	}
}

// Runner double rejecting everyone while recording the users it checked and the turns it handled
type rejectingRunner struct {
	authChecked []string
	handled     []*gateway.MessageEvent
}

func (r *rejectingRunner) IsUserAuthorized(_ context.Context, source *gateway.SessionSource, _ bool) (bool, error) {
	r.authChecked = append(r.authChecked, deref(source.UserID))
	return false, nil
}

func TestSlackReactionAuthorizationGateUnauthorizedReactorRejectedInPipeline(t *testing.T) {
	a, f, captured := newHarnessAdapter(t, map[string]any{"reaction_triggers": true})
	a.teamClients.Set("T1", newWebClient("xoxb-test-token", ""))
	a.teamBotUserIDs = map[string]string{"T1": "U_BOT"}
	a.channelTeam.Set("C1", "T1")
	f.respond("conversations.replies", map[string]any{"ok": true, "messages": []any{map[string]any{"ts": "1000.0", "user": "U_BOT", "text": "Proposal"}}})
	runner := &rejectingRunner{}
	a.SetBoundMessageHandler(runner, func(_ context.Context, ev *gateway.MessageEvent) (any, error) {
		runner.handled = append(runner.handled, ev)
		return nil, nil
	})
	if err := a.handleSlackReaction(context.Background(), reactionEvent("U_RANDO", "thumbsup", "1000.0", "U_BOT", "3000.0"), false); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, u := range runner.authChecked {
		found = found || u == "U_RANDO"
	}
	if !found || len(runner.handled) != 0 || len(captured.all()) != 0 {
		t.Fatalf("auth checked %v handled %d dispatched %d", runner.authChecked, len(runner.handled), len(captured.all()))
	}
}
