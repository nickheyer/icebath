package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
)

type turnRun func(ctx context.Context, a *GatewayAgent, message any, o agent.RunConversationOptions) (map[string]any, error)

// Replaces the credential, route, agent build and conversation steps of a turn the way hermes tests patch them
func patchTurn(t *testing.T, run turnRun) {
	t.Helper()
	prevEnsure, prevResolve, prevInit, prevRun, prevDelay := ensureTurnCredentials, resolveTurnRoute, initTurnAgent, runTurnConversation, settlePaintDelay
	ensureTurnCredentials = func(*CLI) bool { return true }
	resolveTurnRoute = func(c *CLI, _ any) turnRoute { return turnRoute{signature: c.activeAgentRouteSignature} }
	initTurnAgent = func(*CLI, string, *cliRuntime, map[string]any) bool { return true }
	runTurnConversation = run
	settlePaintDelay = 0
	t.Cleanup(func() {
		ensureTurnCredentials, resolveTurnRoute, initTurnAgent, runTurnConversation, settlePaintDelay = prevEnsure, prevResolve, prevInit, prevRun, prevDelay
	})
}

func doneResult() map[string]any {
	return map[string]any{"final_response": "done", "messages": []map[string]any{{"role": "assistant", "content": "done"}},
		"api_calls": 1, "completed": true, "partial": true, "response_previewed": true}
}

func drain(q *inputQueue) []any {
	var out []any
	for {
		item, ok := q.GetNoWait()
		if !ok {
			return out
		}
		out = append(out, item)
	}
}

func interruptedTurn(payload any) *chatTurn {
	return &chatTurn{result: map[string]any{"interrupted": true, "interrupt_message": payload, "final_response": ""}}
}

func closedDone() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}

func TestTuplePayloadIsRequeuedWithItsImages(t *testing.T) {
	recordPrints(t)
	c := bareCLI()
	c.interruptQueue, c.pendingInput = &inputQueue{}, &inputQueue{}
	payload := queuedInput{Text: "look at this", Images: []string{"/tmp/clip.png"}}
	c.chatRenderTurn(interruptedTurn(payload), closedDone(), nil)
	if got := drain(c.pendingInput); len(got) != 1 || !reflect.DeepEqual(got[0], payload) {
		t.Fatalf("requeued %#v", got)
	}
}

func TestQueuedPartsJoinTextAndMergeImages(t *testing.T) {
	recordPrints(t)
	c := bareCLI()
	c.interruptQueue, c.pendingInput = &inputQueue{}, &inputQueue{}
	c.interruptQueue.Put("second")
	c.interruptQueue.Put(queuedInput{Text: "third", Images: []string{"/tmp/b.png"}})
	c.chatRenderTurn(interruptedTurn(queuedInput{Text: "first", Images: []string{"/tmp/a.png"}}), closedDone(), nil)
	want := queuedInput{Text: "first\nsecond\nthird", Images: []string{"/tmp/a.png", "/tmp/b.png"}}
	if got := drain(c.pendingInput); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("requeued %#v", got)
	}
}

func chatCLI(t *testing.T) *CLI {
	t.Helper()
	recordPrints(t)
	c := configuredCLI(t, map[string]any{"tool_progress": "all"}, CLIOptions{})
	c.agent = offlineAgent(t, c)
	c.interruptQueue, c.pendingInput = &inputQueue{}, &inputQueue{}
	return c
}

func TestUnacknowledgedInterruptMessageIsRequeuedNotDropped(t *testing.T) {
	c := chatCLI(t)
	c.interruptQueue.Put("urgent new message")
	patchTurn(t, func(context.Context, *GatewayAgent, any, agent.RunConversationOptions) (map[string]any, error) {
		time.Sleep(500 * time.Millisecond)
		return map[string]any{"final_response": "turn finished normally", "messages": []map[string]any{{"role": "user", "content": "original"},
			{"role": "assistant", "content": "turn finished normally"}}, "api_calls": 1, "completed": true, "partial": true, "response_previewed": true}, nil
	})
	var interrupts []string
	prev := interruptTurnAgent
	interruptTurnAgent = func(a *GatewayAgent, message string) {
		interrupts = append(interrupts, message)
		prev(a, message)
	}
	t.Cleanup(func() { interruptTurnAgent = prev })
	c.chat("original", nil, false)
	if !slices.Equal(interrupts, []string{"urgent new message"}) {
		t.Fatalf("interrupt calls %q", interrupts)
	}
	queued := drain(c.pendingInput)
	if !slices.ContainsFunc(queued, func(q any) bool { return strings.Contains(interruptText(q), "urgent new message") }) {
		t.Fatalf("interrupt message was dropped; pending_input=%#v", queued)
	}
	if c.agent.InterruptRequested() {
		t.Fatal("the stale interrupt flag must be cleared so the next turn does not abort itself")
	}
}

func TestChatPersistsCleanInputWhenAQueuedNoteChangesApiMessage(t *testing.T) {
	c := chatCLI(t)
	var gotMessage any
	var gotOptions agent.RunConversationOptions
	patchTurn(t, func(_ context.Context, _ *GatewayAgent, message any, o agent.RunConversationOptions) (map[string]any, error) {
		gotMessage, gotOptions = message, o
		return doneResult(), nil
	})
	c.pendingModelSwitchNote = "[MODEL SWITCH NOTE]"
	c.chat("clean prompt", nil, false)
	if gotMessage != "[MODEL SWITCH NOTE]\n\nclean prompt" || gotOptions.PersistUserMessage != "clean prompt" {
		t.Fatalf("api message %q persist %q", gotMessage, gotOptions.PersistUserMessage)
	}
}

func TestChatPreservesCleanMultimodalInputWhenNoteChangesApiMessage(t *testing.T) {
	c := chatCLI(t)
	var gotMessage any
	var gotOptions agent.RunConversationOptions
	patchTurn(t, func(_ context.Context, _ *GatewayAgent, message any, o agent.RunConversationOptions) (map[string]any, error) {
		gotMessage, gotOptions = message, o
		return doneResult(), nil
	})
	cleanParts := []any{
		map[string]any{"type": "text", "text": "Describe this screenshot"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
	}
	c.pendingModelSwitchNote = "[MODEL SWITCH NOTE]"
	c.chat(cleanParts, nil, false)
	if !reflect.DeepEqual(gotOptions.PersistUserMessage, cleanParts) {
		t.Fatalf("persisted %#v", gotOptions.PersistUserMessage)
	}
	apiParts, ok := gotMessage.([]any)
	if !ok || len(apiParts) != 2 {
		t.Fatalf("api message %#v", gotMessage)
	}
	if first, _ := apiParts[0].(map[string]any); first["text"] != "[MODEL SWITCH NOTE]\n\nDescribe this screenshot" {
		t.Fatalf("first api part %#v", apiParts[0])
	}
	if cleanParts[0].(map[string]any)["text"] != "Describe this screenshot" {
		t.Fatal("the clean parts must not carry the note")
	}
	if !reflect.DeepEqual(apiParts[1], cleanParts[1]) {
		t.Fatalf("image part changed: %#v", apiParts[1])
	}
}

// Chat completions server answering done and keeping every request body
func recordingChatServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "test/model", "object": "model"}}})
			return
		}
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		chunk := func(delta map[string]any, finish any) string {
			line, _ := json.Marshal(map[string]any{"id": "c1", "object": "chat.completion.chunk", "created": 1, "model": "test/model",
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
			return "data: " + string(line) + "\n\n"
		}
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if body["stream"] != true {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "c1", "object": "chat.completion", "model": "test/model",
				"usage":   map[string]any{"prompt_tokens": 12, "completion_tokens": 3, "total_tokens": 15},
				"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "done", "tool_calls": nil}, "finish_reason": "stop"}}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, chunk(map[string]any{"role": "assistant", "content": "done"}, nil))
		_, _ = io.WriteString(w, chunk(map[string]any{}, "stop"))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string{}, bodies...)
	}
}

// A CLI whose turns run the real agent against the given chat completions server
func liveChatCLI(t *testing.T, baseURL string) *CLI {
	t.Helper()
	recordPrints(t)
	resetProcessState(t)
	prevDelay := settlePaintDelay
	settlePaintDelay = 0
	t.Cleanup(func() { settlePaintDelay = prevDelay })
	c := configuredCLI(t, map[string]any{"tool_progress": "all"}, CLIOptions{Model: "test/model", Provider: "openai", APIKey: "test-key-1234567890", BaseURL: baseURL})
	c.interruptQueue, c.pendingInput = &inputQueue{}, &inputQueue{}
	return c
}

func TestChatMultimodalNotePersistsCleanInputOnce(t *testing.T) {
	srv, bodies := recordingChatServer(t)
	c := liveChatCLI(t, srv.URL+"/v1")
	cleanParts := []any{
		map[string]any{"type": "text", "text": "Describe this screenshot"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
	}
	c.pendingModelSwitchNote = "[MODEL SWITCH NOTE]"
	if _, ok := c.chat(cleanParts, nil, false); !ok {
		t.Fatal("the turn produced no response")
	}
	sent := false
	for _, body := range bodies() {
		sent = sent || strings.Contains(body, `[MODEL SWITCH NOTE]\n\nDescribe this screenshot`)
	}
	if !sent {
		t.Fatalf("the provider never saw the note: %q", bodies())
	}
	rows, err := c.sessionDB.GetMessagesAsConversation(c.ctx, c.sessionID, state.ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var users []any
	for _, row := range rows {
		if strings.Contains(cliStr(row["content"], ""), "MODEL SWITCH NOTE") {
			t.Fatalf("the note reached the durable transcript: %v", row)
		}
		if row["role"] == "user" {
			users = append(users, row["content"])
		}
	}
	if len(users) != 1 || users[0] != "Describe this screenshot\n[screenshot]" {
		t.Fatalf("stored user rows %#v of %v", users, rows)
	}
}

func TestChatClearsPreviousTurnPersistenceOverrideBeforeStaging(t *testing.T) {
	srv, _ := recordingChatServer(t)
	c := liveChatCLI(t, srv.URL+"/v1")
	c.pendingModelSwitchNote = "[MODEL SWITCH NOTE]"
	if _, ok := c.chat("previous clean prompt", nil, false); !ok {
		t.Fatal("the first turn produced no response")
	}
	var stagedMessage map[string]any
	prev := runTurnConversation
	runTurnConversation = func(_ context.Context, a *GatewayAgent, _ any, _ agent.RunConversationOptions) (map[string]any, error) {
		a.mu.Lock()
		stagedMessage = a.pendingCLIUserMessage
		a.mu.Unlock()
		return doneResult(), nil
	}
	t.Cleanup(func() { runTurnConversation = prev })
	c.chat("new prompt", nil, false)
	if stagedMessage == nil || stagedMessage["role"] != "user" || stagedMessage["content"] != "new prompt" {
		t.Fatalf("staged row %v", stagedMessage)
	}
	if _, isFloat := stagedMessage["timestamp"].(float64); !isFloat {
		t.Fatalf("staged timestamp %#v", stagedMessage["timestamp"])
	}
}

func TestChatSubmitsAtReferencesUnexpanded(t *testing.T) {
	c := chatCLI(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "example.py"), []byte("CLIENT-SIDE-CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	var gotMessage any
	var staged map[string]any
	patchTurn(t, func(_ context.Context, a *GatewayAgent, message any, _ agent.RunConversationOptions) (map[string]any, error) {
		a.mu.Lock()
		gotMessage, staged = message, a.pendingCLIUserMessage
		a.mu.Unlock()
		return doneResult(), nil
	})
	c.chat("inspect @file:example.py", nil, false)
	if gotMessage != "inspect @file:example.py" || staged["content"] != "inspect @file:example.py" {
		t.Fatalf("submitted %q staged %v", gotMessage, staged)
	}
}

func TestChatTurnAdoptsChainAddedAfterTheCliOpenedAndKeepsItOnTornConfig(t *testing.T) {
	recordPrints(t)
	one := 1
	compact := true
	c := configuredCLI(t, map[string]any{"tool_progress": "all"}, CLIOptions{Compact: &compact, MaxTurns: &one})
	c.agent = offlineAgent(t, c)
	c.interruptQueue, c.pendingInput = &inputQueue{}, &inputQueue{}
	patchTurn(t, func(context.Context, *GatewayAgent, any, agent.RunConversationOptions) (map[string]any, error) {
		return doneResult(), nil
	})
	fallback := []map[string]any{{"provider": "xai-oauth", "model": "grok-4.6"}}
	turn := func(configText string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), []byte(configText), 0o600); err != nil {
			t.Fatal(err)
		}
		c.chat("hello", nil, false)
	}
	turn("fallback_providers:\n  - provider: xai-oauth\n    model: grok-4.6\n")
	if !reflect.DeepEqual(c.fallbackModel, fallback) {
		t.Fatalf("cli chain %v", c.fallbackModel)
	}
	turn("fallback_providers: [\n  - provider: {{{\n")
	if !reflect.DeepEqual(c.fallbackModel, fallback) {
		t.Fatalf("a torn config wiped the chain: cli %v", c.fallbackModel)
	}
}

// Arms a voice TTS turn against a recording speaker and reports the turn and whether the speaker started
func armTurn(t *testing.T, requirements, voiceTTS bool) (*chatTurn, bool) {
	t.Helper()
	recordPrints(t)
	prevCheck, prevStream := checkTTSRequirements, streamTTSToSpeaker
	checkTTSRequirements = func(context.Context) bool { return requirements }
	started := make(chan struct{}, 1)
	streamTTSToSpeaker = func(_ context.Context, _ media.TextQueue, _, done media.Event, _ func(string), _ string) {
		done.Set()
		started <- struct{}{}
	}
	t.Cleanup(func() { checkTTSRequirements, streamTTSToSpeaker = prevCheck, prevStream })
	c := bareCLI()
	c.voiceTTS = voiceTTS
	c.voiceTTSDone = newSyncEvent()
	turn := &chatTurn{}
	c.chatSetupTurnAudio(turn, "hello", false)
	spoke := false
	if turn.useStreamingTTS {
		if turn.textQueue == nil || turn.stopEvent == nil {
			t.Fatal("an armed turn needs its text queue and stop event")
		}
		turn.textQueue.Put(nil)
		select {
		case <-started:
			spoke = true
		case <-time.After(2 * time.Second):
		}
	}
	return turn, spoke
}

func TestStreamingTtsArmsWithoutImportingSounddevice(t *testing.T) {
	turn, spoke := armTurn(t, true, true)
	if !turn.useStreamingTTS || !spoke {
		t.Fatalf("armed %v speaker started %v", turn.useStreamingTTS, spoke)
	}
}

func TestStreamingTtsArmsWithAPlainWorkingImportToo(t *testing.T) {
	if turn, _ := armTurn(t, true, true); !turn.useStreamingTTS {
		t.Fatal("arming must come from the requirements check alone")
	}
}

func TestStreamingTtsStaysOffWhenProviderUnavailable(t *testing.T) {
	if turn, _ := armTurn(t, false, true); turn.useStreamingTTS || turn.textQueue != nil {
		t.Fatalf("armed %v queue %v with the provider unavailable", turn.useStreamingTTS, turn.textQueue)
	}
}

func TestStreamingTtsNotArmedWithoutVoiceTts(t *testing.T) {
	if turn, _ := armTurn(t, true, false); turn.useStreamingTTS || turn.textQueue != nil {
		t.Fatalf("armed %v queue %v without voice TTS", turn.useStreamingTTS, turn.textQueue)
	}
}

func TestInterruptedReplyPanelAfterToolCallBoundary(t *testing.T) {
	for _, tc := range []struct {
		response    string
		expectPanel bool
	}{
		{"Partial answer.", false},
		{"Operation interrupted: waiting for model response (3.0s elapsed).", true},
	} {
		c, rec := streamCLI(t, "76")
		c.streamDelta("Partial answer.\n")
		c.streamDelta(nil)
		c.lastTurnInterrupted = true
		rec.reset()
		c.chatPrintResponsePanel(&chatTurn{result: map[string]any{"interrupted": true, "final_response": tc.response}}, tc.response)
		if printed := len(rec.raw()) > 0; printed != tc.expectPanel {
			t.Fatalf("response %q printed %q", tc.response, rec.raw())
		}
	}
}

func TestUnstreamedFinalReplyAfterStreamedSegmentStillPrintsPanel(t *testing.T) {
	c, rec := streamCLI(t, "76")
	c.streamDelta("Looking that up.\n")
	c.streamDelta(nil)
	c.lastTurnInterrupted = false
	rec.reset()
	c.chatPrintResponsePanel(&chatTurn{result: map[string]any{"completed": true, "final_response": "Final B"}}, "Final B")
	if !slices.ContainsFunc(rec.plain(), func(line string) bool { return strings.Contains(line, "Final B") }) {
		t.Fatalf("the unstreamed final reply was not printed: %q", rec.plain())
	}
}

func TestRefusedNoticePrintsTheUsersTextAndDiscardLineAsWritten(t *testing.T) {
	text := "fix the *bug* in __init__.py and `run` it"
	notice := "Your message was not sent.\n" + strings.Join([]string{
		"⏸ This chat is paused. Icebath couldn't confirm how the last turn ended (usually because it restarted mid-turn):",
		`    "` + text + `"`,
		"  Icebath won't re-run it by itself, since it may already have done part of the work.",
		"  To drop it and continue, type:",
		"/discard a1",
		"  1 message is waiting and will run once that turn is dropped:",
		`    "next"`,
	}, "\n")
	for _, mode := range []string{"strip", "render"} {
		rec := recordPrints(t)
		t.Setenv("COLUMNS", "200")
		c := bareCLI()
		c.finalResponseMarkdown = mode
		c.chatPrintResponsePanel(&chatTurn{result: map[string]any{"failed": true, "failure_reason": "gateway_connection", "error": notice, "final_response": ""}}, notice)
		var lines []string
		for _, entry := range rec.plain() {
			for _, line := range strings.Split(entry, "\n") {
				lines = append(lines, strings.TrimSpace(line))
			}
		}
		if !slices.Contains(lines, `"`+text+`"`) || !slices.Contains(lines, "/discard a1") {
			t.Fatalf("mode %s rewrote the notice: %q", mode, lines)
		}
	}
}

func TestCliChatRegistersSecretCaptureCallback(t *testing.T) {
	recordPrints(t)
	c := configuredCLI(t, map[string]any{"tool_progress": "all"}, CLIOptions{})
	skills.SetSecretCaptureCallback(nil)
	t.Cleanup(func() { skills.SetSecretCaptureCallback(nil) })
	prev := ensureTurnCredentials
	ensureTurnCredentials = func(*CLI) bool { return false }
	t.Cleanup(func() { ensureTurnCredentials = prev })
	c.chat("hello", nil, false)
	registered := skills.SecretCaptureCallback()
	if registered == nil || reflect.ValueOf(registered).Pointer() != reflect.ValueOf(skills.SecretCaptureFunc(c.secretCaptureCallback)).Pointer() {
		t.Fatal("chat must register the CLI secret capture callback")
	}
}
