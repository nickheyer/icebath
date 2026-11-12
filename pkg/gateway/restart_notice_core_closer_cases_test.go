package gateway_test

import (
	"os"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestRestartNoticeReplacesTheCoreFailedTurnBoundaryOfTheSameTurn(t *testing.T) {
	for _, principal := range []string{"person", "cron-owner"} {
		t.Run(principal, func(t *testing.T) {
			ctx, db, authority, live, row := gateway.ClaimedOnRealStoreForTests(t, principal)
			env := &tools.Env{Home: os.Getenv("ICEBATH_HOME")}
			a, err := agent.NewAIAgent(tools.WithToolEnv(ctx, env), agent.AIAgentOptions{SessionDB: db, SessionID: "s", Model: "test-model",
				Provider: "openai-compat", APIKey: "test", BaseURL: "http://127.0.0.1:1/v1", QuietMode: true, SkipContextFiles: true, SkipMemory: true, Env: env})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { a.Close() })
			rows := []map[string]any{{"role": "user", "content": "please work"}}
			if _, err := a.FlushMessagesToSessionDB(rows); err != nil {
				t.Fatal(err)
			}
			result := map[string]any{"final_response": "", "interrupted": true, "completed": false, "interrupt_message": "Gateway restarting",
				"messages": []*state.Message{state.MessageFromDict(rows[0])}}
			agent.CloseDurableFailedTurn(ctx, a, result)
			if got := gateway.FailedTurnNoticesForTests(t, ctx, db); !slices.Equal(got, []string{agent.FailedTurnNotice}) {
				t.Fatalf("core closer notices %q", got)
			}
			captured := pyjson.ObjectOf("result", pyjson.ObjectOf("final_response", "", "interrupted", true, "completed", false,
				"interrupt_message", "Gateway restarting", "messages", []any{map[string]any{"role": "user", "content": "please work"}}))
			if err := gateway.CommitAndPublishForTests(ctx, authority, live, row, "", "interrupted", captured); err != nil {
				t.Fatal(err)
			}
			expected := gateway.RestartInterruptNoticeForTests
			if principal != "person" {
				expected = agent.AutomationInterruptedNotice
			}
			if got := gateway.FailedTurnNoticesForTests(t, ctx, db); !slices.Equal(got, []string{expected}) {
				t.Fatalf("settled notices %q", got)
			}
			messages, err := db.GetMessages(ctx, "s", state.GetMessagesOptions{})
			if err != nil {
				t.Fatal(err)
			}
			roles := []string{}
			for _, m := range messages {
				role, _ := m.Get("role")
				roles = append(roles, role.(string))
			}
			if !slices.Equal(roles, []string{"user", "assistant"}) {
				t.Fatalf("roles %q", roles)
			}
		})
	}
}
