package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

func billingResultFor(t *testing.T, classified *provider.Classified, summary string, calls int) map[string]any {
	t.Helper()
	return billingFailureResult(context.Background(), classified, summary, nil, calls, "anthropic", "https://api.anthropic.com", "claude-opus-5", nil, UsageWindowRefusal{})
}

func TestTerminalResponse(t *testing.T) {
	caseHome(t)
	unverified := func() *provider.Classified {
		return classifiedBilling(t, "You're out of extra usage. Add more at claude.ai/settings/usage and keep going.")
	}
	t.Run("test_unverified_terminal_response_does_not_assert_billing", func(t *testing.T) {
		final := billingResultFor(t, unverified(), "HTTP 400: out of extra usage", 3)["final_response"].(string)
		if strings.HasPrefix(final, "Billing or credits exhausted") || !strings.Contains(final, "unverified") ||
			!(strings.Contains(final, "content-filter") || strings.Contains(final, "content filter")) {
			t.Fatalf("final %q", final)
		}
	})
	t.Run("test_unverified_terminal_response_structured_fields", func(t *testing.T) {
		result := billingResultFor(t, unverified(), "HTTP 400: out of extra usage", 3)
		block, _ := result["billing_block"].(map[string]any)
		if result["failed"] != true || result["failure_reason"] != "billing" || result["billing_unverified"] != true || block == nil || block["unverified"] != true {
			t.Fatalf("result %v", result)
		}
	})
	t.Run("test_confirmed_terminal_response_stays_assertive", func(t *testing.T) {
		result := billingResultFor(t, classifiedBilling(t, "Your credit balance is too low to access the Anthropic API."), "HTTP 400: credit balance too low", 1)
		block, _ := result["billing_block"].(map[string]any)
		_, flagged := block["unverified"]
		if !strings.HasPrefix(result["final_response"].(string), "Billing or credits exhausted") || result["billing_unverified"] != false || flagged {
			t.Fatalf("result %v", result)
		}
	})
}

type raisingMetricsClient struct{}

func (raisingMetricsClient) LastReferenceMetrics() any { panic(errors.New("boom")) }

func TestConversationLoopHelper(t *testing.T) {
	caseHome(t)
	t.Run("test_returns_none_for_a_non_moa_client", func(t *testing.T) {
		if moaReferenceMetricsForHook(&AIAgent{client: struct{}{}}) != nil {
			t.Fatal("a client without the accessor reports no metrics")
		}
	})
	t.Run("test_returns_none_when_there_is_no_client", func(t *testing.T) {
		if moaReferenceMetricsForHook(&AIAgent{}) != nil {
			t.Fatal("no client reports no metrics")
		}
	})
	t.Run("test_a_raising_accessor_is_swallowed", func(t *testing.T) {
		if moaReferenceMetricsForHook(&AIAgent{client: raisingMetricsClient{}}) != nil {
			t.Fatal("a raising accessor must be swallowed")
		}
	})
}

func TestLengthContinuationAssembly(t *testing.T) {
	caseHome(t)
	t.Run("test_distinct_continuation_is_preserved", func(t *testing.T) {
		got := joinTruncatedParts([]TruncatedPart{{Text: "The first half ends here", CutStream: true}, {Text: "and the second half adds new information."}})
		if got != "The first half ends here\nand the second half adds new information." {
			t.Fatalf("joined %q", got)
		}
	})
	t.Run("test_repeated_tail_is_trimmed_without_losing_new_text", func(t *testing.T) {
		repeated := "A sufficiently long repeated transition sentence ends here."
		got := joinTruncatedParts([]TruncatedPart{{Text: "Existing answer. " + repeated, CutStream: true}, {Text: repeated + " New inbound details remain visible."}})
		if got != "Existing answer. "+repeated+" New inbound details remain visible." {
			t.Fatalf("joined %q", got)
		}
	})
}

func redirectedMessages(streamed string, rows []map[string]any, text string) (*AIAgent, []*state.Message) {
	a := bareSteerAgent()
	a.setCurrentStreamedAssistantText(streamed)
	messages := rowsToMessages(rows)
	applyActiveTurnRedirect(a, &messages, text)
	return a, messages
}

func rowText(m *state.Message) string {
	content, _ := m.Content.(string)
	return content
}

func TestActiveTurnRedirectCheckpoint(t *testing.T) {
	caseHome(t)
	start := func() []map[string]any { return []map[string]any{{"role": "user", "content": "start"}} }
	t.Run("test_repetition_dominated_partial_is_not_replayed", func(t *testing.T) {
		repeated := strings.Repeat("The same degenerate partial response keeps repeating without progress.\n", 10)
		_, messages := redirectedMessages(repeated, start(), "Change course.")
		spacer, correction := messages[len(messages)-2], messages[len(messages)-1]
		replayed := correction.APIContent
		if strings.Contains(replayed, repeated) || strings.Contains(replayed, "Visible response before the interruption:") || !strings.Contains(replayed, provider.RepetitionLoopInterrupted) {
			t.Fatalf("replayed %q", replayed)
		}
		if spacer.Role != "assistant" || spacer.DisplayKind != "hidden" || strings.Contains(rowText(spacer), repeated) || strings.Contains(spacer.APIContent, repeated) {
			t.Fatalf("alternation row %+v", spacer)
		}
	})
	t.Run("test_ordinary_partial_remains_replayable", func(t *testing.T) {
		visible := "I found the migration entry point and was about to inspect it."
		_, messages := redirectedMessages(visible, start(), "Change course.")
		if !strings.Contains(messages[len(messages)-1].APIContent, visible) {
			t.Fatalf("replayed %q", messages[len(messages)-1].APIContent)
		}
	})
	t.Run("test_assistant_tail_puts_correction_last", func(t *testing.T) {
		_, messages := redirectedMessages("Visible draft.", append(start(), map[string]any{"role": "assistant", "content": "committed assistant item"}), "Use Postgres instead.")
		last := messages[len(messages)-1]
		if len(messages) != 3 || messages[0].Role != "user" || messages[1].Role != "assistant" || last.Role != "user" || last.Content != "Use Postgres instead." {
			t.Fatalf("messages %v", messagesToRows(messages))
		}
		replayed := last.APIContent
		if !strings.Contains(replayed, "Visible draft.") || !strings.Contains(replayed, "Context from the interrupted assistant response") || !strings.HasSuffix(replayed, "Use Postgres instead.") {
			t.Fatalf("replayed %q", replayed)
		}
	})
	t.Run("test_scaffolding_never_lands_in_transcript_content", func(t *testing.T) {
		scaffolding := []string{"[This response was interrupted by a user correction.]", "Visible response before the interruption:", "[Context from the interrupted assistant response]"}
		for _, tailRole := range []string{"tool", "assistant"} {
			for _, streamed := range []string{"Partial reply on screen.", ""} {
				rows := start()
				if tailRole == "assistant" {
					rows = append(rows, map[string]any{"role": "assistant", "content": "committed"})
				} else {
					rows = append(rows, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "a"}}},
						map[string]any{"role": "tool", "content": "out", "tool_call_id": "a"})
				}
				_, messages := redirectedMessages(streamed, rows, "New direction.")
				for _, msg := range messages {
					for _, marker := range scaffolding {
						if msg.Role == "assistant" && strings.Contains(rowText(msg)+msg.APIContent, marker) {
							t.Fatalf("scaffolding leaked into an assistant row (tail=%s streamed=%v): %+v", tailRole, streamed != "", msg)
						}
						if msg.DisplayKind != "hidden" && strings.Contains(rowText(msg), marker) {
							t.Fatalf("scaffolding leaked into visible content (tail=%s streamed=%v): %q", tailRole, streamed != "", rowText(msg))
						}
					}
				}
				last := messages[len(messages)-1]
				if last.Content != "New direction." || !strings.Contains(last.APIContent, "[This response was interrupted by a user correction.]") ||
					(streamed != "" && !strings.Contains(last.APIContent, streamed)) {
					t.Fatalf("correction %+v", last)
				}
			}
		}
	})
	t.Run("test_checkpoint_never_replays_chain_of_thought", func(t *testing.T) {
		for _, tailRole := range []string{"user", "assistant"} {
			a := bareSteerAgent()
			a.setCurrentStreamedAssistantText("Visible draft.")
			rows := start()
			if tailRole == "assistant" {
				rows = append(rows, map[string]any{"role": "assistant", "content": "committed"})
			}
			messages := rowsToMessages(rows)
			applyActiveTurnRedirect(a, &messages, "Change course.")
			var serialized strings.Builder
			for _, m := range messages {
				serialized.WriteString(fmt.Sprint(m.Content) + m.APIContent)
			}
			text := serialized.String()
			if strings.Contains(text, "SECRET chain of thought.") || strings.Contains(text, "Reasoning shown before the interruption") || !strings.Contains(text, "Visible draft.") {
				t.Fatalf("tail %s serialized %q", tailRole, text)
			}
		}
	})
}

func TestEmptyHiddenAssistantRehealRegression(t *testing.T) {
	caseHome(t)
	t.Run("test_active_turn_redirect_hidden_placeholder_has_provider_replay_payload", func(t *testing.T) {
		_, messages := redirectedMessages("", []map[string]any{{"role": "user", "content": "start"}}, "Use Postgres instead.")
		spacer, correction := messages[len(messages)-2], messages[len(messages)-1]
		if spacer.Role != "assistant" || spacer.Content != "" || spacer.DisplayKind != "hidden" || spacer.APIContent != InterruptedPlaceholder {
			t.Fatalf("alternation row %+v", spacer)
		}
		if correction.Role != "user" || correction.Content != "Use Postgres instead." {
			t.Fatalf("correction %+v", correction)
		}
	})
}
