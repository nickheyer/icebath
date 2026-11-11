package tuiserver

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Image file holding data under a fresh temp dir and its base64 data URL
func historyImage(t *testing.T, name string, data []byte) (string, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p, "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
}

// Display projection of rows under the launch profile with inline image URLs, failing loudly on a projection error
func projectHistory(rows ...any) []any {
	out, err := tuigateway.HistoryToMessages(context.Background(), rows, "", true)
	if err != nil {
		panic(err)
	}
	return out
}

func TestVerifiedNativeImageHistoryReconcilesWithoutInlinePayload(t *testing.T) {
	path, url := historyImage(t, "sample shot.png", []byte("image bytes"))
	parts := []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}}
	legacy := append(append([]any{map[string]any{"type": "text", "text": "Caption\n\n[Image attached at: " + path + "]"}}, parts...),
		map[string]any{"type": "text", "text": "<memory-context>private recall</memory-context>"})
	current := append([]any{map[string]any{"type": "text", "text": "Caption\n@image:`" + path + "`"}}, parts...)
	legacyRow := map[string]any{"role": "user", "content": legacy}
	currentRow := map[string]any{"role": "user", "content": current}
	got := projectHistory(legacyRow, map[string]any{"role": "assistant", "content": "Done"}, currentRow)
	want := []any{
		map[string]any{"role": "user", "text": "Caption\n\n@image:`" + path + "`"},
		map[string]any{"role": "assistant", "text": "Done"},
		map[string]any{"role": "user", "text": "Caption\n@image:`" + path + "`"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if !reflect.DeepEqual(legacyRow["content"], legacy) || !reflect.DeepEqual(currentRow["content"], current) {
		t.Fatal("stored content changed")
	}
}

func TestFlattenedScreenshotPlaceholderStillProjects(t *testing.T) {
	path, url := historyImage(t, "sample shot.png", []byte("image bytes"))
	part := map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
	legacy := []any{map[string]any{"type": "text", "text": "Caption\n\n[Image attached at: " + path + "]\n[screenshot]"}, part}
	current := []any{map[string]any{"type": "text", "text": "Caption\n@image:`" + path + "`\n[screenshot]"}, part}
	got := projectHistory(map[string]any{"role": "user", "content": legacy}, map[string]any{"role": "user", "content": current})
	want := []any{
		map[string]any{"role": "user", "text": "Caption\n\n@image:`" + path + "`"},
		map[string]any{"role": "user", "text": "Caption\n@image:`" + path + "`"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestScreenshotPlaceholderWithoutImagePartStaysUnprojected(t *testing.T) {
	path, _ := historyImage(t, "shot.png", []byte("image bytes"))
	text := "Caption\n@image:" + path + "\n[screenshot]"
	row := map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}}
	if got := projectHistory(row)[0].(map[string]any)["text"]; got != text {
		t.Fatalf("got %q", got)
	}
}

func TestUnavailableOrMismatchedImageKeepsInlinePayload(t *testing.T) {
	path, _ := historyImage(t, "shot.png", []byte("new bytes"))
	url := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("old bytes"))
	row := map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "Caption\n@image:" + path},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}},
	}}
	if got := projectHistory(row)[0].(map[string]any)["text"].(string); !strings.Contains(got, url) {
		t.Fatalf("got %q", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := projectHistory(row)[0].(map[string]any)["text"].(string); !strings.Contains(got, url) {
		t.Fatalf("got %q", got)
	}
}

var inlineDataURI = "data:image/png;base64," + strings.Repeat("a", 128)

// User image turn and assistant reply the inline image cases read
func imageTurn() []any {
	return []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "what is this?"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": inlineDataURI}},
		}},
		map[string]any{"role": "assistant", "content": "a chart"},
	}
}

func TestCoerceMessageTextSwitchesImageRendering(t *testing.T) {
	content := imageTurn()[0].(map[string]any)["content"]
	if !strings.Contains(tuigateway.CoerceMessageText(content, true), inlineDataURI) {
		t.Fatal("default did not inline the URI")
	}
	off := tuigateway.CoerceMessageText(content, false)
	if strings.Contains(off, inlineDataURI) || !strings.Contains(off, "[image]") {
		t.Fatalf("off %q", off)
	}
	if tuigateway.CoerceMessageText("plain", true) != "plain" || tuigateway.CoerceMessageText("plain", false) != "plain" {
		t.Fatal("plain text changed")
	}
}

func TestHistoryToMessagesHonorsTheSwitch(t *testing.T) {
	ctx := context.Background()
	def, err := tuigateway.HistoryToMessages(ctx, imageTurn(), "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(def[0].(map[string]any)["text"].(string), inlineDataURI) {
		t.Fatalf("default %v", def)
	}
	off, err := tuigateway.HistoryToMessages(ctx, imageTurn(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	text := off[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "[image]") || strings.Contains(text, inlineDataURI) {
		t.Fatalf("off %q", text)
	}
	want := map[string]any{"role": "assistant", "text": "a chart"}
	if !reflect.DeepEqual(off[1], want) || !reflect.DeepEqual(def[1], want) {
		t.Fatalf("assistant rows %v %v", off[1], def[1])
	}
}

// Assistant row with public commentary, a private analysis item and one tool call
func commentaryRow(text string) map[string]any {
	return map[string]any{
		"role": "assistant", "content": "",
		"reasoning":         "Private summary.\n\n" + text,
		"reasoning_content": "Private summary.\n\n" + text,
		"codex_message_items": []any{
			map[string]any{"type": "message", "role": "assistant", "phase": "commentary",
				"content": []any{map[string]any{"type": "output_text", "text": text}}},
			map[string]any{"type": "message", "role": "assistant", "phase": "analysis",
				"content": []any{map[string]any{"type": "output_text", "text": "private analysis"}}},
		},
		"tool_calls": []any{map[string]any{"id": "call-1", "type": "function",
			"function": map[string]any{"name": "terminal", "arguments": "{}"}}},
		"timestamp": int64(1),
	}
}

func TestGatewayHistoryUsesSameDisplayProjection(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	row := commentaryRow("I will inspect the files.")
	projected := projectHistory(row)[0].(map[string]any)
	if !reflect.DeepEqual(projected["display_commentary"], []any{"I will inspect the files."}) {
		t.Fatalf("commentary %v", projected["display_commentary"])
	}
	if projected["display_reasoning"] != "Private summary." {
		t.Fatalf("reasoning %v", projected["display_reasoning"])
	}
	if !reflect.DeepEqual(projected["codex_message_items"], row["codex_message_items"]) {
		t.Fatal("items changed")
	}
	if !strings.HasSuffix(row["reasoning"].(string), "I will inspect the files.") {
		t.Fatal("source reasoning changed")
	}
}
