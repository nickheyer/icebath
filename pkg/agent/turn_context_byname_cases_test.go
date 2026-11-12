package agent

import (
	"context"
	"maps"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestComposeUserApiContent(t *testing.T) {
	t.Run("test_none_when_nothing_to_inject", func(t *testing.T) {
		if got, ok := composeUserAPIContent("hello", "", ""); ok || got != "" {
			t.Fatalf("composed %q %v", got, ok)
		}
	})
}

func TestComposeMultimodalContextPart(t *testing.T) {
	t.Run("test_is_the_string_sidecar_injection_tail", func(t *testing.T) {
		if got, ok := composeMultimodalContextPart("", ""); ok || got != "" {
			t.Fatalf("empty part %q %v", got, ok)
		}
		sidecar, sidecarOK := composeUserAPIContent("hello", "likes tea", "CTX")
		part, partOK := composeMultimodalContextPart("likes tea", "CTX")
		if !sidecarOK || !partOK || sidecar != "hello\n\n"+part {
			t.Fatalf("sidecar %q part %q", sidecar, part)
		}
	})
}

func TestMemoryQueryText(t *testing.T) {
	t.Run("test_list_turn_queries_its_text_and_image_only_stays_trivial", func(t *testing.T) {
		image := map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}}
		textPlusImage := []any{map[string]any{"type": "text", "text": "remind me what my dog's name is"}, image}
		if got := memoryQueryText(textPlusImage); got != "remind me what my dog's name is" || memory.IsTrivialPrompt(got) {
			t.Fatalf("text plus image query %q", got)
		}
		if got := memoryQueryText([]any{image}); got != "" || !memory.IsTrivialPrompt(got) {
			t.Fatalf("image only query %q", got)
		}
	})
}

func TestPrologueStamping(t *testing.T) {
	t.Run("test_stamps_api_content_from_plugin_context", func(t *testing.T) {
		a := prologueAgent(t)
		db := blockDB(t, "state.db")
		a.sessionDB = db
		patchPluginHooks(t, func(string, map[string]any) []any { return []any{map[string]any{"context": "PLUGIN-CTX"}} })
		tc := prologueBuild(t, a, nil)
		msg := tc.Messages[tc.CurrentTurnUserIdx]
		composed, _ := composeUserAPIContent("hello", tc.ExtPrefetchCache, tc.PluginUserContext)
		if msg.Content != "hello" || msg.APIContent != composed || msg.APIContent != "hello\n\nPLUGIN-CTX" {
			t.Fatalf("content %v sidecar %q composed %q", msg.Content, msg.APIContent, composed)
		}
		rows, err := db.GetMessagesAsConversation(context.Background(), a.sessionID, state.ConversationOptions{})
		if err != nil || len(rows) == 0 {
			t.Fatalf("stored rows %v %v", rows, err)
		}
		if last := rows[len(rows)-1]; last["content"] != "hello" || last["api_content"] != "hello\n\nPLUGIN-CTX" {
			t.Fatalf("early persist row %v", last)
		}
	})
}

func TestApiContentMatchesWireSubstitutionWithoutMutation(t *testing.T) {
	for id, message := range map[string]map[string]any{
		"user-sidecar":       {"role": "user", "content": "clean", "api_content": "wire user"},
		"assistant-sidecar":  {"role": "assistant", "content": "clean", "api_content": "wire assistant"},
		"system-role":        {"role": "system", "content": "clean", "api_content": "ignored"},
		"tool-role":          {"role": "tool", "content": "clean", "api_content": "ignored"},
		"empty-sidecar":      {"role": "user", "content": "clean", "api_content": ""},
		"non-string-sidecar": {"role": "user", "content": "clean", "api_content": []any{"ignored"}},
	} {
		t.Run(id, func(t *testing.T) {
			caseHome(t)
			original := maps.Clone(message)
			wire := maps.Clone(message)
			substituteAPIContent(wire)
			expected, _ := wire["content"].(string)
			if got := estimateMsgBudgetTokens(context.Background(), message, true); got != provider.EstimateTokens(expected)+10 {
				t.Fatalf("tokens %d for wire content %q", got, expected)
			}
			if !reflect.DeepEqual(message, original) {
				t.Fatalf("message mutated %v", message)
			}
		})
	}
}
