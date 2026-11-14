package slack

import (
	"context"
	"strings"
	"testing"
	"time"
)

const (
	blockKitRichMD    = "# Title\n\n- a\n  - nested\n\n---\n\nbody text"
	blockKitRichTable = "| Item | Status | Note |\n|---|---:|---|\n| Hermes | ok | table |"
)

// Harness adapter of the block kit cases with its config extras and the update answering the message ts
func blockKitAdapter(t *testing.T, extra map[string]any) (*SlackAdapter, *fakeSlackAPI) {
	t.Helper()
	a, f, _ := newHarnessAdapter(t, extra)
	f.respond("chat.postMessage", map[string]any{"ok": true, "ts": "111.222"})
	f.respond("chat.update", map[string]any{"ok": true, "ts": "111.222"})
	return a, f
}

// Arguments of the last call to the method
func lastArgs(t *testing.T, f *fakeSlackAPI, method string) map[string]any {
	t.Helper()
	calls := f.callsTo(method)
	if len(calls) == 0 {
		t.Fatalf("%s never called", method)
	}
	return calls[len(calls)-1].Args
}

// test_slack_block_kit_adapter.py TestSendMessageBlocks.test_disabled_by_default_no_blocks
func TestSendMessageBlocksDisabledByDefaultNoBlocks(t *testing.T) {
	a, f := blockKitAdapter(t, nil)
	if _, err := a.Send(context.Background(), "C1", blockKitRichMD, "", nil); err != nil {
		t.Fatal(err)
	}
	kwargs := lastArgs(t, f, "chat.postMessage")
	if _, has := kwargs["blocks"]; has || kwargs["text"] == "" || kwargs["text"] == nil {
		t.Fatalf("kwargs = %v", kwargs)
	}
}

// test_slack_block_kit_adapter.py TestSendMessageBlocks.test_unfurl_config_suppresses_previews_without_changing_link_text
func TestSendMessageBlocksUnfurlConfigSuppressesPreviewsWithoutChangingLinkText(t *testing.T) {
	a, f := blockKitAdapter(t, map[string]any{"unfurl_links": false, "unfurl_media": false})
	if _, err := a.Send(context.Background(), "C1", "[Hermes](https://example.com/hermes)", "", nil); err != nil {
		t.Fatal(err)
	}
	kwargs := lastArgs(t, f, "chat.postMessage")
	if kwargs["text"] != "<https://example.com/hermes|Hermes>" || kwargs["unfurl_links"] != false || kwargs["unfurl_media"] != false {
		t.Fatalf("kwargs = %v", kwargs)
	}
}

// test_slack_block_kit_adapter.py TestSendMessageBlocks.test_enabled_but_unrenderable_falls_back_to_text
func TestSendMessageBlocksEnabledButUnrenderableFallsBackToText(t *testing.T) {
	a, f := blockKitAdapter(t, map[string]any{"rich_blocks": true})
	parts := make([]string, 60)
	for i := range parts {
		parts[i] = "---"
	}
	if _, err := a.Send(context.Background(), "C1", strings.Join(parts, "\n\n"), "", nil); err != nil {
		t.Fatal(err)
	}
	kwargs := lastArgs(t, f, "chat.postMessage")
	if _, has := kwargs["blocks"]; has || kwargs["text"] == "" || kwargs["text"] == nil {
		t.Fatalf("kwargs = %v", kwargs)
	}
}

// test_slack_block_kit_adapter.py TestSendMessageBlocks.test_feedback_buttons_opt_in_appended_to_blocks
func TestSendMessageBlocksFeedbackButtonsOptInAppendedToBlocks(t *testing.T) {
	a, f := blockKitAdapter(t, map[string]any{"rich_blocks": true, "feedback_buttons": true})
	if _, err := a.Send(context.Background(), "C1", "final answer", "", nil); err != nil {
		t.Fatal(err)
	}
	blocks, _ := lastArgs(t, f, "chat.postMessage")["blocks"].([]any)
	if len(blocks) == 0 {
		t.Fatal("no blocks")
	}
	feedback, _ := blocks[len(blocks)-1].(map[string]any)
	elements, _ := feedback["elements"].([]any)
	first, _ := elements[0].(map[string]any)
	if feedback["type"] != "context_actions" || first["type"] != "feedback_buttons" || first["action_id"] != "hermes_feedback" {
		t.Fatalf("feedback block = %v", feedback)
	}
}

// test_slack_block_kit_adapter.py TestEditMessageBlocks.test_intermediate_edit_no_blocks
func TestEditMessageBlocksIntermediateEditNoBlocks(t *testing.T) {
	a, f := blockKitAdapter(t, map[string]any{"rich_blocks": true})
	a.editMessage(context.Background(), "C1", "111.222", blockKitRichMD, false, nil)
	kwargs := lastArgs(t, f, "chat.update")
	if _, has := kwargs["blocks"]; has || kwargs["text"] == "" || kwargs["text"] == nil {
		t.Fatalf("kwargs = %v", kwargs)
	}
}

// test_slack_block_kit_adapter.py TestEditMessageBlocks.test_finalize_edit_gets_blocks
func TestEditMessageBlocksFinalizeEditGetsBlocks(t *testing.T) {
	a, f := blockKitAdapter(t, map[string]any{"rich_blocks": true})
	a.editMessage(context.Background(), "C1", "111.222", blockKitRichMD, true, nil)
	kwargs := lastArgs(t, f, "chat.update")
	if blocks, _ := kwargs["blocks"].([]any); len(blocks) == 0 || kwargs["text"] == "" || kwargs["text"] == nil {
		t.Fatalf("kwargs = %v", kwargs)
	}
}

// test_slack_block_kit_adapter.py TestEditMessageBlocks.test_block_rejection_retries_edit_without_blocks_using_workspace_client
func TestEditMessageBlocksBlockRejectionRetriesEditWithoutBlocksUsingWorkspaceClient(t *testing.T) {
	a, f, _ := newHarnessAdapter(t, map[string]any{"rich_blocks": true})
	a.teamClients.Set("T_SECONDARY", newWebClient("xoxb-secondary", ""))
	f.Respond("chat.update", map[string]any{"ok": false, "error": "invalid_blocks"})
	f.Respond("chat.update", map[string]any{"ok": true, "ts": "111.222"})
	result := a.editMessage(context.Background(), "C1", "111.222", blockKitRichTable, true, map[string]any{"team_id": "T_SECONDARY"})
	if !result.Success {
		t.Fatalf("result = %+v", result)
	}
	calls := f.callsTo("chat.update")
	if len(calls) != 2 {
		t.Fatalf("chat.update calls = %d", len(calls))
	}
	first, second := calls[0], calls[1]
	for _, c := range []fakeCall{first, second} {
		if c.Header.Get("Authorization") != "Bearer xoxb-secondary" {
			t.Fatalf("edit went through %q, not the workspace client", c.Header.Get("Authorization"))
		}
	}
	if blocks, _ := first.Args["blocks"].([]any); len(blocks) == 0 {
		t.Fatalf("first = %v", first.Args)
	}
	if blocks, ok := second.Args["blocks"].([]any); !ok || len(blocks) != 0 || second.Args["text"] == "" {
		t.Fatalf("second = %v", second.Args)
	}
}

// test_slack_block_kit_adapter.py TestEditMessageBlocks.test_timeout_error_on_edit_is_retryable_transient
func TestEditMessageBlocksTimeoutErrorOnEditIsRetryableTransient(t *testing.T) {
	prev := slackWebTimeout
	slackWebTimeout = 100 * time.Millisecond
	t.Cleanup(func() { slackWebTimeout = prev })
	a, f, _ := newHarnessAdapter(t, nil)
	f.RespondAfter("chat.update", 2*time.Second, map[string]any{"ok": true})
	result := a.editMessage(context.Background(), "C1", "111.222", blockKitRichMD, true, nil)
	if result.Success || !result.Retryable || result.ErrorKind == nil || *result.ErrorKind != "transient" {
		t.Fatalf("result = %+v", result)
	}
}

// test_slack_block_kit_adapter.py TestMarkdownBlockMode.test_enabled_sends_markdown_block_with_raw_content
func TestMarkdownBlockModeEnabledSendsMarkdownBlockWithRawContent(t *testing.T) {
	a, f := blockKitAdapter(t, map[string]any{"markdown_blocks": true})
	if _, err := a.Send(context.Background(), "C1", blockKitRichTable, "", nil); err != nil {
		t.Fatal(err)
	}
	kwargs := lastArgs(t, f, "chat.postMessage")
	blocks, _ := kwargs["blocks"].([]any)
	first, _ := blocks[0].(map[string]any)
	if first["type"] != "markdown" || first["text"] != blockKitRichTable || kwargs["text"] == "" {
		t.Fatalf("kwargs = %v", kwargs)
	}
}

// test_slack_block_kit_adapter.py TestMarkdownBlockMode.test_edit_finalize_uses_markdown_block
func TestMarkdownBlockModeEditFinalizeUsesMarkdownBlock(t *testing.T) {
	a, f := blockKitAdapter(t, map[string]any{"markdown_blocks": true})
	a.editMessage(context.Background(), "C1", "111.222", blockKitRichTable, true, nil)
	blocks, _ := lastArgs(t, f, "chat.update")["blocks"].([]any)
	first, _ := blocks[0].(map[string]any)
	if first["type"] != "markdown" || first["text"] != blockKitRichTable {
		t.Fatalf("blocks = %v", blocks)
	}
}
