package manage

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/vault"
)

// Gives a staged clone vault its own keychain entry, returning the pointer it made or nil
func vaultOwnCopiedKey(vaultDir, publishedDir string) (map[string]any, error) {
	ref, err := vault.NewVaultStore(context.Background(), vaultDir).OwnCopiedKey(publishedDir)
	if err != nil || ref == nil {
		return nil, err
	}
	m, _ := pyjson.Plain(ref).(map[string]any)
	return m, nil
}

func vaultDeleteKey(ref map[string]any) { vault.VaultDeleteKeyRef(ref) }

func releaseProfileLogHandlers(dir string) (int, error) {
	return logging.ReleaseProfileLogHandlers(dir)
}

// Auxiliary call_llm content and the Python class name of a raised error
func auxCallLLM(ctx context.Context, task string, messages []map[string]any, temperature float64, maxTokens int, timeout time.Duration) (string, string, error) {
	msgs := make([]any, len(messages))
	for i, m := range messages {
		msgs[i] = m
	}
	resp, err := provider.CallLLM(ctx, map[string]any{"task": task, "messages": msgs, "temperature": temperature,
		"max_tokens": maxTokens, "timeout": timeout.Seconds()})
	if err != nil {
		return "", provider.AuxExcTypeName(err), err
	}
	return firstChoiceContent(resp), "", nil
}

// choices[0].message.content of a wire response, empty when any step is missing or falsy
func firstChoiceContent(resp any) string {
	m, _ := resp.(map[string]any)
	choices, _ := m["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	content, _ := msg["content"].(string)
	return content
}
