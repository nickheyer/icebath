package app

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// CLI agent whose staging lands on an in process agent the way the hermes CLI shares one with its turn
type inProcessStageAgent struct {
	*GatewayAgent
	real *agent.AIAgent
}

func (d *inProcessStageAgent) StagePendingCLIUserMessage(staged map[string]any, appendToTranscript func()) {
	d.real.StagePendingCLIUserMessage(staged, appendToTranscript)
}

// One async delegation event of the display session whose second goal is the finished task
func delegationDisplayEvent(status string, truncated bool) map[string]any {
	return map[string]any{"type": "async_delegation", "session_key": "display-session", "delegation_id": "deleg-test",
		"goals":   []any{"Do not display this sibling", "Review [bold]changes[/bold]"},
		"results": []any{map[string]any{"task_index": 1, "status": status, "truncated": truncated, "summary": "Full result evidence"}}}
}

// Status, truncation and the label the display line names for each delegation outcome
var delegationDisplayRows = []struct {
	status    string
	truncated bool
	label     string
}{{"completed", false, "Completed"}, {"failed", false, "Failed"}, {"cancelled", false, "Cancelled"}, {"completed", true, "Incomplete"},
	{"stalled", false, "Stalled"}, {"unknown", false, "Unknown"}, {"rejected", false, "Failed"}, {"other_terminal_state", false, "Incomplete"}}

func TestCompletionDisplayKeepsPayloadSeparateAcrossSurfaces(t *testing.T) {
	caseHome(t)
	ctx := context.Background()
	var c *CLI
	for _, row := range delegationDisplayRows {
		event := delegationDisplayEvent(row.status, row.truncated)
		original := pycopy.DeepCopy(event)
		payload := tools.FormatProcessNotification(event)
		c, _ = notificationCLI(t, "", event)
		rec := recordPrints(t)
		c.drainProcessNotifications("cli-idle")
		queued, ok := c.pendingInput.GetNoWait()
		if !ok {
			t.Fatalf("%s: nothing queued", row.status)
		}
		var chatted []any
		savedChat := tuiChat
		tuiChat = func(_ *CLI, message any, _ []string, _ bool) (string, bool) {
			chatted = append(chatted, message)
			return "", true
		}
		c.tuiProcessOneInput(queued)
		tuiChat = savedChat
		visible := printedText(rec)
		expected := fmt.Sprintf("Subagent Task %s: %s", row.label, "Review [bold]changes[/bold]")
		if !strings.Contains(visible, expected) {
			t.Fatalf("%s: %q not in %q", row.status, expected, visible)
		}
		if strings.Contains(visible, "ASYNC DELEGATION") || strings.Contains(visible, "Full result evidence") {
			t.Fatalf("%s: payload leaked into %q", row.status, visible)
		}
		queuedMessage, isNotification := queued.(tools.TimelineNotification)
		if chattedMessage, _ := chatted[0].(tools.TimelineNotification); !isNotification || queuedMessage.Text != payload || len(chatted) != 1 || chattedMessage.Text != payload {
			t.Fatalf("%s: queued %#v chatted %v", row.status, queued, chatted)
		}
		real, err := agent.NewAIAgent(ctx, agent.AIAgentOptions{Model: "test-model", APIKey: "k", BaseURL: "http://127.0.0.1:9/v1", Provider: "openai",
			Platform: "cli", SessionID: c.sessionID, QuietMode: true, SkipContextFiles: true, SkipMemory: true})
		if err != nil {
			t.Fatal(err)
		}
		c.conversationHistory, c.turnCtx = []map[string]any{}, ctx
		c.setAgent(&inProcessStageAgent{GatewayAgent: offlineAgent(t, c), real: real})
		var runMessage any
		var runOptions agent.RunConversationOptions
		patchTurn(t, func(_ context.Context, _ cliAgent, message any, o agent.RunConversationOptions) (map[string]any, error) {
			runMessage, runOptions = message, o
			return map[string]any{}, nil
		})
		c.chatStageUserMessage(c.agentRef(), queuedMessage)
		c.chatRunAgent(&chatTurn{}, queuedMessage.Text)
		staged := c.conversationHistory[len(c.conversationHistory)-1]
		if content, isText := staged["content"].(string); !isText || content != payload || staged["display_kind"] != "async_delegation_complete" {
			t.Fatalf("%s: staged %v", row.status, staged)
		}
		core, _ := real.StageTurnUserMessage(runMessage, runOptions.PersistUserMessage)
		if reflect.ValueOf(core).UnsafePointer() != reflect.ValueOf(staged).UnsafePointer() {
			t.Fatalf("%s: the turn built a fresh row %v instead of adopting the staged one", row.status, core)
		}
		if core["display_metadata"].(map[string]any)["display_text"] != expected {
			t.Fatalf("%s: display metadata %v", row.status, core["display_metadata"])
		}
		if !reflect.DeepEqual(pycopy.DeepCopy(staged), any(staged)) {
			t.Fatalf("%s: staged row does not survive a deep copy", row.status)
		}
		planned, _ := provider.BuildPromptCachePlan([]map[string]any{core}, nil, provider.PromptCachePlanOptions{TTL: "5m", ToolPartMarkers: true})
		if got := provider.StripAnthropicCacheControl(planned)[0]["content"]; got != payload {
			t.Fatalf("%s: cache plan content %v", row.status, got)
		}
		db, err := state.NewSessionDB(ctx, filepath.Join(t.TempDir(), fmt.Sprintf("%s-%v.db", row.status, row.truncated)), false)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.CreateSession(state.NewSession{ID: c.sessionID, Source: "cli"}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.AppendMessagesBatch(ctx, c.sessionID, c.conversationHistory, state.AppendGuards{}, nil); err != nil {
			t.Fatal(err)
		}
		objects, err := db.GetMessages(ctx, c.sessionID, state.GetMessagesOptions{})
		db.Close()
		if err != nil {
			t.Fatal(err)
		}
		restored := make([]map[string]any, 0, len(objects))
		for _, object := range objects {
			restored = append(restored, pyjson.Plain(pyjson.Ordered(object)).(map[string]any))
		}
		if restored[0]["content"] != payload {
			t.Fatalf("%s: restored %v", row.status, restored[0])
		}
		entries, _, _ := collectResumeEntries(c, restored, map[string]any{}, func(text string) string { return text })
		if !reflect.DeepEqual(entries, []resumeEntry{{Role: "event", Text: expected}}) {
			t.Fatalf("%s: resume entries %v", row.status, entries)
		}
		if !reflect.DeepEqual(event, original) {
			t.Fatalf("%s: event mutated to %v", row.status, event)
		}
	}
	grouped := map[string]any{"group": "Review", "goals": []any{"First", "Second"},
		"results": []any{map[string]any{"task_index": 0, "status": "completed"}, map[string]any{"task_index": 1, "status": "failed"}}}
	if got := tools.AsyncDelegationDisplayText(grouped); got != "Subagent Tasks Finished with Issues: Review (2 tasks)" {
		t.Fatalf("grouped %q", got)
	}
	early := map[string]any{"group": "Review", "goals": []any{"First", "Second"}, "task_failure_notice": true, "results": []any{grouped["results"].([]any)[1]}}
	if got := tools.AsyncDelegationDisplayText(early); got != "Subagent Task Failed: Second" {
		t.Fatalf("early %q", got)
	}
	grouped["results"].([]any)[1].(map[string]any)["status"] = "completed"
	if got := tools.AsyncDelegationDisplayText(grouped); got != "Subagent Tasks Completed: Review (2 tasks)" {
		t.Fatalf("grouped completed %q", got)
	}
	if got := tools.AsyncDelegationDisplayText(map[string]any{"goal": "Legacy task", "status": "timeout"}); got != "Subagent Task Timed Out: Legacy task" {
		t.Fatalf("legacy %q", got)
	}
	if got := tools.AsyncDelegationDisplayText(map[string]any{"results": []any{}, "error": "Worker crashed"}); !strings.Contains(got, "Failed") {
		t.Fatalf("crashed %q", got)
	}
	rec := recordPrints(t)
	c.printUserMessagePreview("[ASYNC DELEGATION BATCH COMPLETE — user-authored]")
	if !strings.Contains(printedText(rec), "[ASYNC DELEGATION BATCH COMPLETE — user-authored]") {
		t.Fatalf("user authored preview %q", printedText(rec))
	}
}
