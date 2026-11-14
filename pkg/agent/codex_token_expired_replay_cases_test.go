package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

const codexTokenExpiredText = "Provided authentication token is expired. Please try signing in again."

// Codex backend 401 whose body carries the token_expired code
func codex401TokenExpired(t *testing.T) *provider.APIError {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"error": map[string]any{"message": codexTokenExpiredText, "type": "invalid_request_error", "code": "token_expired"}})
	if err != nil {
		t.Fatal(err)
	}
	return &provider.APIError{Provider: "openai-codex", Model: "gpt-5.3-codex", StatusCode: 401, Body: string(raw), Message: codexTokenExpiredText}
}

// Assistant rows that each replay a cached encrypted reasoning blob
func codexCachedHistory(n int) []*state.Message {
	var out []*state.Message
	for i := range n {
		out = append(out, state.MessageFromDict(map[string]any{"role": "assistant", "content": fmt.Sprintf("answer %d", i),
			"codex_reasoning_items": []any{map[string]any{"type": "reasoning", "id": fmt.Sprintf("rs_%d", i), "encrypted_content": strings.Repeat("gAAA", 8)}}}))
	}
	return out
}

func TestPooledTokenExpiredStripsCachedReasoningBeforeBenchingEntries(t *testing.T) {
	h := caseHome(t)
	var entries []provider.PooledCredential
	for i, token := range []string{"same-bearer", "other-bearer"} {
		entries = append(entries, provider.PooledCredential{Provider: "openai-codex", ID: fmt.Sprintf("acct-%d", i), Label: fmt.Sprintf("acct-%d", i),
			AuthType: "oauth", Priority: i, Source: fmt.Sprintf("acct-%d", i), AccessToken: token})
	}
	pool := provider.NewPool(agentPoolEnv(home.WithOverride(context.Background(), h)), "openai-codex", entries)
	a := &AIAgent{provider: "openai-codex", apiMode: "codex_responses", baseURL: "https://chatgpt.com/backend-api/codex", model: "gpt-5.3-codex",
		apiKey: "same-bearer", codexReasoningReplayEnabled: true, credentialPool: pool, quietMode: true}
	messages := codexCachedHistory(2)
	apiMessages := make([]any, len(messages))
	for i, m := range messages {
		apiMessages[i] = state.MessageToDict(m)
	}
	apiErr := codex401TokenExpired(t)
	classified := provider.ClassifyError(provider.ClassifyInput{Err: apiErr, Status: 401, Body: apiErr.Body, Provider: a.provider, Model: a.model})
	if classified.Reason != provider.ReasonAuth {
		t.Fatalf("reclassified as %s", classified.Reason)
	}
	status := 401
	s := &LoopState{Retry: &TurnRetryState{}, Messages: messages, APIMessages: apiMessages}
	e := &apiErrorTurn{apiError: apiErr, classified: classified, statusCode: &status, errorContext: provider.ExtractAPIErrorContext(apiErr),
		errorMsg: apiErr.Error(), providerName: a.provider, baseURL: a.baseURL, model: a.model}
	retried, recoveredWithPool, err := recoverAfterClassification(context.Background(), a, s, e)
	if err != nil {
		t.Fatal(err)
	}
	if !retried || recoveredWithPool {
		t.Fatalf("retried %v recovered with pool %v", retried, recoveredWithPool)
	}
	for _, m := range s.Messages {
		if _, has := state.MessageToDict(m)["codex_reasoning_items"]; has {
			t.Fatalf("cached reasoning left on %v", state.MessageToDict(m))
		}
	}
	for _, entry := range pool.Entries() {
		if entry.LastStatus != "" {
			t.Fatalf("entry %s benched with %q", entry.ID, entry.LastStatus)
		}
	}
}
