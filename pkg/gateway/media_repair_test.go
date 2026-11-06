package gateway

import (
	"testing"
)

func TestRepairsExplicitComputerUseMediaPathFromJSONResult(t *testing.T) {
	captureName := "computer_use_0123456789abcdef0123456789abcdef.png"
	canonical := `C:\Users\Alice\AppData\Local\hermes\cache\images\` + captureName
	response := "Here is the screenshot.\nMEDIA:/Users/Alice/AppData/Local/hermes/cache/images/" + captureName
	messages := []any{
		map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "capture", "function": map[string]any{"name": "computer_use"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "capture", "content": pyDumps(t, map[string]any{"screenshot_path": canonical})},
	}
	if got, want := RepairExplicitComputerUseMediaPaths(response, messages), "Here is the screenshot.\nMEDIA:"+canonical; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRepairsExplicitPathFromMultimodalTextSummary(t *testing.T) {
	captureName := "computer_use_fedcba9876543210fedcba9876543210.jpg"
	canonical := `D:\Hermes Data\cache\images\` + captureName
	response := `MEDIA:"/Users/Alice/Hermes Data/cache/images/` + captureName + `"`
	messages := []any{map[string]any{
		"role": "tool", "name": "computer_use", "tool_call_id": "capture",
		"content": []any{
			map[string]any{"type": "text", "text": "capture mode=screen 1920x1080\n  (shareable screenshot saved to " + canonical + ")"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/jpeg;base64,AAAA"}},
		},
	}}
	if got, want := RepairExplicitComputerUseMediaPaths(response, messages), `MEDIA:"`+canonical+`"`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestDoesNotAutoAttachComputerUseCapture(t *testing.T) {
	canonical := `C:\Users\Alice\AppData\Local\hermes\cache\images\computer_use_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.png`
	messages := []any{map[string]any{"role": "tool", "name": "computer_use", "content": pyDumps(t, map[string]any{"screenshot_path": canonical})}}
	if got := RepairExplicitComputerUseMediaPaths("Done.", messages); got != "Done." {
		t.Fatalf("got %q", got)
	}
}

func TestDoesNotRewriteUnmatchedOrPreviousTurnCapture(t *testing.T) {
	oldName := "computer_use_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.png"
	currentName := "computer_use_cccccccccccccccccccccccccccccccc.png"
	history := []any{map[string]any{"role": "tool", "name": "computer_use", "content": pyDumps(t, map[string]any{"screenshot_path": `C:\cache\images\` + oldName})}}
	currentTurn := []any{
		map[string]any{"role": "user", "content": "Send the current screenshot."},
		map[string]any{"role": "tool", "name": "computer_use", "content": pyDumps(t, map[string]any{"screenshot_path": `C:\cache\images\` + currentName})},
	}
	response := "MEDIA:/Users/Alice/cache/images/" + oldName
	got := RepairExplicitComputerUseMediaPathsAt(t.Context(), response, append(append([]any{}, history...), currentTurn...), len(history))
	if got != response {
		t.Fatalf("got %q, want %q", got, response)
	}
}

func TestMalformedJSONResultFailsClosed(t *testing.T) {
	captureName := "computer_use_dddddddddddddddddddddddddddddddd.png"
	canonical := `C:\Users\Alice\AppData\Local\hermes\cache\images\` + captureName
	payload := pyDumps(t, map[string]any{
		"summary":         "capture\n  (shareable screenshot saved to " + canonical + ")",
		"screenshot_path": canonical,
	})
	truncated := payload[:len(payload)-5]
	response := "MEDIA:/Users/Alice/AppData/Local/hermes/cache/images/" + captureName
	messages := []any{map[string]any{"role": "tool", "name": "computer_use", "content": truncated}}
	if got := RepairExplicitComputerUseMediaPaths(response, messages); got != response {
		t.Fatalf("got %q, want %q", got, response)
	}
}

func TestCompressionFallbackSlicesFromLastUserMessage(t *testing.T) {
	captureName := "computer_use_eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee.png"
	canonical := `C:\cache\images\` + captureName
	response := "MEDIA:/cache/images/" + captureName
	currentTurn := []any{
		map[string]any{"role": "user", "content": "Send the screenshot."},
		map[string]any{"role": "tool", "name": "computer_use", "content": pyDumps(t, map[string]any{"screenshot_path": canonical})},
	}
	if got, want := RepairExplicitComputerUseMediaPathsAt(t.Context(), response, currentTurn, 10), "MEDIA:"+canonical; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := RepairExplicitComputerUseMediaPathsAt(t.Context(), response, currentTurn[1:], 10); got != response {
		t.Fatalf("got %q, want %q", got, response)
	}
}
