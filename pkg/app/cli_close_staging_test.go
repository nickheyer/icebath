package app

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/state"
)

// AIAgent the CLI holds whose staging append and run block until the case releases them
type closeRaceAgent struct {
	*GatewayAgent
	real           *agent.AIAgent
	blockAppend    atomic.Bool
	stagingEntered chan struct{}
	releaseStaging chan struct{}
	runEntered     chan struct{}
	releaseRun     chan struct{}
	result         map[string]any
}

func (d *closeRaceAgent) StagePendingCLIUserMessage(staged map[string]any, appendToTranscript func()) {
	d.real.StagePendingCLIUserMessage(staged, func() {
		if d.blockAppend.CompareAndSwap(true, false) {
			close(d.stagingEntered)
			select {
			case <-d.releaseStaging:
			case <-time.After(5 * time.Second):
			}
		}
		appendToTranscript()
	})
}

func (d *closeRaceAgent) PersistActiveSessionBeforeClose(ctx context.Context, readCLIHistory func() []map[string]any) bool {
	return d.real.PersistActiveSessionBeforeClose(ctx, readCLIHistory)
}

func (d *closeRaceAgent) SessionID() string { return d.real.SessionID() }

func (d *closeRaceAgent) RunConversation(context.Context, any, agent.RunConversationOptions) (map[string]any, error) {
	close(d.runEntered)
	select {
	case <-d.releaseRun:
	case <-time.After(5 * time.Second):
	}
	return d.result, nil
}

// Waits for a signal up to the given time, true when it arrived
func signalled(ch chan struct{}, wait time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(wait):
		return false
	}
}

func TestCloseWaitsForAtomicCliStagingBeforeSnapshot(t *testing.T) {
	tmp := t.TempDir()
	c := chatCLI(t)
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, ".icebath"))
	ctx := context.Background()
	sessionID := c.sessionID
	db, err := state.NewSessionDB(ctx, filepath.Join(tmp, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.CreateSession(state.NewSession{ID: sessionID, Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	prefix := []map[string]any{{"role": "user", "content": "old prompt"}, {"role": "assistant", "content": "old answer"}}
	for _, message := range prefix {
		if _, err := db.AppendMessage(ctx, sessionID, map[string]any{"role": message["role"], "content": message["content"]}, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	real, err := agent.NewAIAgent(ctx, agent.AIAgentOptions{Model: "test-model", APIKey: "k", BaseURL: "http://127.0.0.1:9/v1", Provider: "openai",
		Platform: "cli", SessionID: sessionID, SessionDB: db, QuietMode: true, SkipContextFiles: true, SkipMemory: true})
	if err != nil {
		t.Fatal(err)
	}
	real.SetSessionDBCreated(true)
	real.SetUnflushedSessionMessagesForTests(prefix)
	real.SetCachedSystemPrompt("test system prompt")
	double := &closeRaceAgent{GatewayAgent: offlineAgent(t, c), real: real, stagingEntered: make(chan struct{}), releaseStaging: make(chan struct{}),
		runEntered: make(chan struct{}), releaseRun: make(chan struct{}), result: map[string]any{"final_response": "done",
			"messages":  append(append([]map[string]any{}, prefix...), map[string]any{"role": "assistant", "content": "done"}),
			"api_calls": 1, "completed": true, "partial": true, "response_previewed": true}}
	double.blockAppend.Store(true)
	c.agent = double
	c.conversationHistory = append([]map[string]any{}, prefix...)
	patchTurn(t, func(ctx context.Context, a cliAgent, message any, o agent.RunConversationOptions) (map[string]any, error) {
		return a.RunConversation(ctx, message, o)
	})
	chatDone := make(chan struct{})
	go func() {
		defer close(chatDone)
		c.chat("new prompt", nil, false)
	}()
	if !signalled(double.stagingEntered, 5*time.Second) {
		t.Fatal("staging never reached the transcript append")
	}
	closeStarted, closeFinished := make(chan struct{}), make(chan struct{})
	go func() {
		close(closeStarted)
		c.persistActiveSessionBeforeClose()
		close(closeFinished)
	}()
	if !signalled(closeStarted, 5*time.Second) {
		t.Fatal("close never started")
	}
	if signalled(closeFinished, 100*time.Millisecond) {
		t.Fatal("close took its snapshot inside the staging handoff")
	}
	close(double.releaseStaging)
	if !signalled(double.runEntered, 5*time.Second) || !signalled(closeFinished, 5*time.Second) {
		t.Fatal("the run or the close never ran")
	}
	close(double.releaseRun)
	if !signalled(chatDone, 10*time.Second) {
		t.Fatal("chat never returned")
	}
	rows, err := db.GetMessagesAsConversation(ctx, sessionID, state.ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var contents []any
	for _, row := range rows {
		contents = append(contents, row["content"])
	}
	if len(contents) != 3 || contents[0] != "old prompt" || contents[1] != "old answer" || contents[2] != "new prompt" {
		t.Fatalf("stored %v", contents)
	}
}
