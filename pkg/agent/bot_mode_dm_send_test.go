package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/botmode"
	"github.com/nickheyer/icebath/pkg/botmode/dmdelivery"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Runs message_agent through the registered inline executor and decodes its JSON answer
func sendBotDM(t *testing.T, a *AIAgent, target, message any) map[string]any {
	t.Helper()
	run := tools.InlineToolExecutorOf(messageAgentToolName)
	if run == nil {
		t.Fatal("message_agent has no inline executor")
	}
	args := map[string]any{}
	if target != nil {
		args["target"] = target
	}
	if message != nil {
		args["message"] = message
	}
	raw, err := run(context.Background(), agentExecutorHost{a}, args, tools.InlineToolContext{TaskID: "task-7"})
	if err != nil {
		t.Fatalf("message_agent failed: %v", err)
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(raw), &answer); err != nil {
		t.Fatalf("answer %q: %v", raw, err)
	}
	return answer
}

// Swaps the terminal tool the runner is launched through for one that records its requests
func recordBotDMTerminal(t *testing.T, answer func(req tools.TerminalRequest) string) *[]tools.TerminalRequest {
	t.Helper()
	var seen []tools.TerminalRequest
	saved := botDMTerminalTool
	botDMTerminalTool = func(_ context.Context, _ *tools.Env, req tools.TerminalRequest) string {
		seen = append(seen, req)
		return answer(req)
	}
	t.Cleanup(func() { botDMTerminalTool = saved })
	return &seen
}

// Message files left in the per user DM directory
func botDMFiles(t *testing.T) []string {
	t.Helper()
	dir, err := botmode.DMDir()
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "dm-*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// Bot Chat agent of a managed install whose temp root is private to the test
func botChatSender(t *testing.T, teammates ...string) (*AIAgent, string) {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	h := botModeManagedHome(t, teammates...)
	t.Setenv("ICEBATH_HOME", h)
	return botModeAgent(t, h, "Bot Chat"), h
}

func TestMessageAgentRefusesWhatItCannotDeliver(t *testing.T) {
	outside := botModeManagedHome(t, "scribe")
	t.Setenv("ICEBATH_HOME", outside)
	if got := sendBotDM(t, botModeAgent(t, outside, "Planning"), "scribe", "hi"); !strings.Contains(got["error"].(string), "only available in a Bot Mode 'Bot Chat' session") {
		t.Fatalf("outside Bot Chat %v", got)
	}
	unmanaged := botModeManagedHome(t)
	if got := sendBotDM(t, botModeAgent(t, unmanaged, "Bot Chat"), "scribe", "hi"); !strings.Contains(got["error"].(string), "not Bot-Mode-managed") {
		t.Fatalf("unmanaged install %v", got)
	}
	a, _ := botChatSender(t, "scribe")
	spawned := recordBotDMTerminal(t, func(tools.TerminalRequest) string { return `{"session_id": "proc_x"}` })
	for _, c := range []struct {
		target, message any
		want            string
		roster          bool
	}{
		{"scribe", "  ", "message is required", false},
		{"scribe", strings.Repeat("é", messageMaxChars+1), "message too long (16001 chars > 16000)", false},
		{" @ ", "hi", "target is required.", true},
		{"bad target!", "hi", "Invalid target: 'bad target!'.", true},
		{"ghost", "hi", "No teammate named 'ghost' on this install, on a connected machine, or on a registered peer.", true},
		{"nowhere/scribe", "hi", "No registered peer named 'nowhere'.", true},
		{"@icebath", "hi", "You can't message yourself. Pick a teammate from the roster.", false},
	} {
		got := sendBotDM(t, a, c.target, c.message)
		text, _ := got["error"].(string)
		if !strings.Contains(text, c.want) || got["reason"] == nil {
			t.Fatalf("%v -> %v", c.target, got)
		}
		if _, listed := got["teammates"]; listed != c.roster {
			t.Fatalf("%v roster listing %v", c.target, got)
		}
		if c.roster && (len(got["teammates"].([]any)) != 1 || got["teammates"].([]any)[0] != "scribe" || len(got["peers"].([]any)) != 0) {
			t.Fatalf("%v roster %v", c.target, got)
		}
	}
	if len(*spawned) != 0 || len(botDMFiles(t)) != 0 {
		t.Fatalf("a refusal started %v and left %v", *spawned, botDMFiles(t))
	}
}

func TestMessageAgentHandsAPeerMessageToTheBackgroundRunner(t *testing.T) {
	a, h := botChatSender(t, "scribe")
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("bot_peers:\n  mini:\n    url: http://127.0.0.1:9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	answer := `{"session_id": "proc_42", "notify_on_complete": true}`
	spawned := recordBotDMTerminal(t, func(tools.TerminalRequest) string { return answer })
	got := sendBotDM(t, a, "mini/Scribe", " hello there ")
	files := botDMFiles(t)
	if len(files) != 1 || len(*spawned) != 1 {
		t.Fatalf("files %v requests %v answer %v", files, *spawned, got)
	}
	wantID, err := dmdelivery.DMDeliveryID(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if got["status"] != "queued" || got["to"] != "@Scribe on peer 'mini'" || got["reply_delivery"] != "notification" ||
		got["process_id"] != "proc_42" || got["delivery_id"] != wantID || got["queued_at"].(float64) <= 0 ||
		!strings.Contains(got["detail"].(string), "do NOT wait or poll") {
		t.Fatalf("ack %v", got)
	}
	req := (*spawned)[0]
	if !req.Background || !req.NotifyOnComplete || !req.HostLocal || req.CompletionOutputChars != 18000 || req.TaskID != "task-7" {
		t.Fatalf("request %+v", req)
	}
	argv, err := pyshlex.SplitPOSIX(req.Command.(string), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) != 13 || argv[1] != botmode.DMRunnerEntry || argv[2] != "--run-delivery" || argv[3] != "--author" || argv[5] != "stdin" ||
		argv[6] != files[0] || filepath.Base(argv[7]) != "icebath" || strings.Join(argv[8:], " ") != "-p default peer dm mini/Scribe" {
		t.Fatalf("runner argv %q", argv)
	}
	var author map[string]any
	if err := json.Unmarshal([]byte(argv[4]), &author); err != nil || author["name"] != "icebath" || author["is_bot"] != true ||
		!strings.HasPrefix(author["id"].(string), "bot:") || !strings.HasSuffix(author["id"].(string), "default") {
		t.Fatalf("author %q %v", argv[4], err)
	}
	if body, err := os.ReadFile(files[0]); err != nil || string(body) != "Message from 🤖 icebath (@icebath): hello there" {
		t.Fatalf("payload %q %v", body, err)
	}
	if err := os.Remove(files[0]); err != nil {
		t.Fatal(err)
	}
	for reply, want := range map[string]string{
		`{"error": "no shell"}`:                            "Delivery to @mini on peer 'mini' failed to start: no shell",
		`{"status": "pending_approval", "error": ""}`:      "needs terminal approval that nobody in this turn can grant, so nothing was sent.",
		`{"status": "ok"}`:                                 "failed to start: no process id returned",
		`["proc"]`:                                         "could not be started: 'list' object has no attribute 'get'",
		`{"session_id": "p", "notify_on_complete": false}`: "",
	} {
		answer = reply
		got := sendBotDM(t, a, "mini", "again")
		if want == "" {
			if got["reply_delivery"] != "poll" || !strings.Contains(got["detail"].(string), "process(action='wait', session_id='p')") ||
				strings.Contains(got["detail"].(string), "saved into this session's transcript") || len(botDMFiles(t)) != 1 {
				t.Fatalf("poll ack %v files %v", got, botDMFiles(t))
			}
			if err := os.Remove(botDMFiles(t)[0]); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if text, _ := got["error"].(string); !strings.Contains(text, want) || len(botDMFiles(t)) != 0 {
			t.Fatalf("%s -> %v files %v", reply, got, botDMFiles(t))
		}
	}
}

func TestMessageAgentReportsALocalTeammateWithNoLiveOwnerAsNotSent(t *testing.T) {
	a, _ := botChatSender(t, "scribe")
	spawned := recordBotDMTerminal(t, func(tools.TerminalRequest) string { return `{"session_id": "proc_x"}` })
	got := sendBotDM(t, a, "Scribe", "status?")
	text, _ := got["error"].(string)
	if got["reason"] != "runtime_unavailable" || !strings.Contains(text, "the message was NOT sent; it is safe to resend") || got["delivery_id"] == "" {
		t.Fatalf("answer %v", got)
	}
	if len(*spawned) != 0 || len(botDMFiles(t)) != 0 {
		t.Fatalf("no send started %v and left %v", *spawned, botDMFiles(t))
	}
}

func TestMessageAgentQueuesARelayedMessageAndStartsItsReplyWaiter(t *testing.T) {
	a, h := botChatSender(t, "scribe")
	roster, err := json.Marshal(map[string]any{"updated_at": time.Now().Unix(), "agents": []map[string]any{
		{"profile": "default", "handle": "icebath", "connection_id": "mini", "connection_label": "Mac Mini", "title": "Moxie"},
		{"profile": "default", "handle": "icebath", "connection_id": "cloud", "connection_label": "Cloud", "title": "Moxie"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(h, botmode.RelayDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, botmode.RelayDirName, botmode.RosterFile), roster, 0o600); err != nil {
		t.Fatal(err)
	}
	answer := `{"session_id": "proc_9", "notify_on_complete": true}`
	spawned := recordBotDMTerminal(t, func(tools.TerminalRequest) string { return answer })
	if got := sendBotDM(t, a, "moxie", "hi"); !strings.Contains(got["error"].(string), "'moxie' exists on several connected machines — disambiguate with one of: icebath@mini, icebath@cloud.") {
		t.Fatalf("ambiguous %v", got)
	}
	got := sendBotDM(t, a, "icebath@mini", "ship it")
	if got["status"] != "queued" || got["to"] != "@icebath on Mac Mini" || got["process_id"] != "proc_9" || len(*spawned) != 1 {
		t.Fatalf("relay ack %v requests %v", got, *spawned)
	}
	argv, err := pyshlex.SplitPOSIX((*spawned)[0].Command.(string), false)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := got["delivery_id"].(string)
	if id == "" || len(argv) != 6 || argv[2] != "--wait-reply" || filepath.Base(argv[3]) != id+".json" || argv[4] != "@icebath on mini" {
		t.Fatalf("waiter argv %q for %v", argv, got)
	}
	answer = `{"error": "no shell"}`
	got = sendBotDM(t, a, "icebath@cloud", "ship it")
	if got["status"] != "queued" || got["to"] != "@icebath on Cloud" || !strings.Contains(got["notification_error"].(string), "failed to start: no shell") ||
		!strings.Contains(got["detail"].(string), "Do NOT resend.") || len(botDMFiles(t)) != 0 {
		t.Fatalf("waiter failure %v", got)
	}
}

func TestMessageAgentSavesAPollOnlySendersReplyIntoItsTranscript(t *testing.T) {
	a, h := botChatSender(t, "scribe")
	a.env = &tools.Env{Home: h}
	ctx := tools.WithToolEnv(context.Background(), a.env)
	raw := tools.TerminalTool(ctx, a.env, tools.TerminalRequest{Command: "echo reply-from-scribe", Background: true, TaskID: "task-7",
		Workdir: t.TempDir(), HostLocal: true, CompletionOutputChars: replyCompletionChars})
	var started map[string]any
	if err := json.Unmarshal([]byte(raw), &started); err != nil {
		t.Fatalf("terminal %q: %v", raw, err)
	}
	procID, _ := started["session_id"].(string)
	if procID == "" {
		t.Fatalf("terminal %q", raw)
	}
	if persistReplyWhenDone(ctx, "proc_unknown", a) {
		t.Fatal("an untracked process armed a transcript row")
	}
	if !persistReplyWhenDone(ctx, procID, a) {
		t.Fatal("a tracked process armed nothing")
	}
	db := a.sessionDB.(*state.SessionDB)
	deadline := time.Now().Add(20 * time.Second)
	for {
		rows, err := db.GetMessagesAsConversation(context.Background(), "sess-1", state.ConversationOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 {
			content, _ := rows[0]["content"].(string)
			if rows[0]["role"] != "user" || !strings.Contains(content, "reply-from-scribe") {
				t.Fatalf("delivery row %v", rows[0])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no delivery row, transcript %v", rows)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
