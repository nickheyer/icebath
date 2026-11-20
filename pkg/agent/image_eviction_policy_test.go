package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const (
	testOutboundImageLimit = 20
	testImageEvictionBatch = 8
	testOutboundImageFloor = 3
)

// Image part carrying the base64 payload as a data url
func imagePart(data string) map[string]any {
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": data}}
}

// Assistant vision call plus its tool result carrying the given image parts after a text part
func imageToolPair(i int, text string, images ...map[string]any) []map[string]any {
	content := []any{map[string]any{"type": "text", "text": text}}
	for _, p := range images {
		content = append(content, p)
	}
	return []map[string]any{
		{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": fmt.Sprintf("call_%d", i), "type": "function",
			"function": map[string]any{"name": "vision_analyze", "arguments": fmt.Sprintf(`{"image_url":"shot%d.png"}`, i)}}}},
		{"role": "tool", "tool_call_id": fmt.Sprintf("call_%d", i), "content": content},
	}
}

// One screenshot pair with the default eighty byte blob
func imageTool(i int) []map[string]any {
	return imageToolPair(i, fmt.Sprintf("Image attached natively shot %d", i), imagePart(fmt.Sprintf("data:image/png;base64,%s%d", strings.Repeat("A", 80), i)))
}

// Pair whose tool result carries k images named by the prefix
func multiImageTool(i, k int, prefix string) []map[string]any {
	parts := make([]map[string]any, k)
	for j := range parts {
		parts[j] = imagePart(fmt.Sprintf("data:image/png;base64,%s%d%d", prefix, i, j))
	}
	return imageToolPair(i, fmt.Sprintf("shot %d", i), parts...)
}

func historyWithScreenshots(n int) []map[string]any {
	msgs := []map[string]any{{"role": "user", "content": "look at these"}}
	for i := 0; i < n; i++ {
		msgs = append(msgs, imageTool(i)...)
	}
	return append(msgs, map[string]any{"role": "user", "content": "compare them"})
}

// User row carrying n uploads named U0 upward after a text part
func uploadsRow(n int) map[string]any {
	content := []any{map[string]any{"type": "text", "text": "look"}}
	for k := 0; k < n; k++ {
		content = append(content, imagePart(fmt.Sprintf("data:image/png;base64,U%d", k)))
	}
	return map[string]any{"role": "user", "content": content}
}

func imageBearingToolIDs(messages []map[string]any) []string {
	out := []string{}
	for _, m := range messages {
		if m["role"] == "tool" && toolContentHasImages(m["content"]) {
			out = append(out, m["tool_call_id"].(string))
		}
	}
	return out
}

// Total image blocks in the request the way the provider counts them
func outboundImageBlocks(messages []map[string]any) int {
	total := 0
	for _, m := range messages {
		inner := m["content"]
		if d, ok := inner.(map[string]any); ok && pyTruthy(d["_multimodal"]) {
			inner = d["content"]
		}
		parts, _ := inner.([]any)
		for _, p := range parts {
			if d, ok := p.(map[string]any); ok && (d["type"] == "image_url" || d["type"] == "input_image" || d["type"] == "image") {
				total++
			}
		}
	}
	return total
}

func callRange(lo, hi int) []string {
	out := []string{}
	for i := lo; i < hi; i++ {
		out = append(out, fmt.Sprintf("call_%d", i))
	}
	return out
}

func sanitizeForSend(history []map[string]any) []map[string]any {
	return sanitizeAPIMessages(context.Background(), "", history)
}

func clonedForSend(history []map[string]any) []map[string]any {
	out := make([]map[string]any, len(history))
	for i, m := range history {
		out[i] = cloneMessageForSend(m).(map[string]any)
	}
	return out
}

func userImageCount(outbound []map[string]any) int {
	for _, m := range outbound {
		if m["role"] == "user" {
			n := 0
			parts, _ := m["content"].([]any)
			for _, p := range parts {
				if d, ok := p.(map[string]any); ok && d["type"] == "image_url" {
					n++
				}
			}
			return n
		}
	}
	return -1
}

func frontierMoves(frontier []string) int {
	moves := 0
	for i := 1; i < len(frontier); i++ {
		if frontier[i] != frontier[i-1] {
			moves++
		}
	}
	return moves
}

func TestOutboundRetiredImageIDsLatchPerSession(t *testing.T) {
	a := &AIAgent{sessionID: "s1"}
	ids := outboundRetiredImageIDs(a)
	ids["call_1"] = true
	if !outboundRetiredImageIDs(a)["call_1"] {
		t.Fatal("the set is not latched within a session")
	}
	a.sessionID = "s2"
	if len(outboundRetiredImageIDs(a)) != 0 {
		t.Fatal("a new session inherited retired ids")
	}
}

func TestOutboundStaleVisionEviction(t *testing.T) {
	t.Run("nothing is evicted below the provider limit", func(t *testing.T) {
		outbound := sanitizeForSend(historyWithScreenshots(testOutboundImageLimit))
		if n := EvictStaleOutboundToolImages(outbound, nil); n != 0 || !slices.Equal(imageBearingToolIDs(outbound), callRange(0, testOutboundImageLimit)) {
			t.Fatalf("evicted %d", n)
		}
	})
	t.Run("eviction retires a batch once over the limit", func(t *testing.T) {
		n := testOutboundImageLimit + 1
		outbound := sanitizeForSend(historyWithScreenshots(n))
		if pruned := EvictStaleOutboundToolImages(outbound, nil); pruned != testImageEvictionBatch {
			t.Fatalf("pruned %d", pruned)
		}
		if !slices.Equal(imageBearingToolIDs(outbound), callRange(testImageEvictionBatch, n)) {
			t.Fatalf("kept %v", imageBearingToolIDs(outbound))
		}
		var oldest map[string]any
		for _, m := range outbound {
			if m["tool_call_id"] == "call_0" {
				oldest = m
				break
			}
		}
		parts, ok := oldest["content"].([]any)
		if !ok || toolContentHasImages(parts) {
			t.Fatalf("oldest %v", oldest)
		}
		marked := false
		for _, p := range parts {
			if d, ok := p.(map[string]any); ok && d["type"] == "text" && strings.Contains(fmt.Sprint(d["text"]), "screenshot removed") {
				marked = true
			}
		}
		if !marked {
			t.Fatalf("no removal text %v", parts)
		}
	})
	t.Run("frontier holds between batch advances", func(t *testing.T) {
		var frontier []string
		for n := testOutboundImageFloor + 1; n < testOutboundImageLimit+3*testImageEvictionBatch; n++ {
			msgs := clonedForSend(historyWithScreenshots(n))
			EvictStaleOutboundToolImages(msgs, nil)
			kept := imageBearingToolIDs(msgs)
			if len(kept) > testOutboundImageLimit {
				t.Fatalf("n=%d kept %d", n, len(kept))
			}
			frontier = append(frontier, kept[0])
		}
		if moves := frontierMoves(frontier); moves != 3 {
			t.Fatalf("frontier moved %d times %v", moves, frontier)
		}
	})
	t.Run("frontier holds when tool results carry several images", func(t *testing.T) {
		surviving := func(n int) []string {
			history := []map[string]any{{"role": "user", "content": "start"}}
			for i := 0; i < n; i++ {
				history = append(history, multiImageTool(i, 3, "A")...)
			}
			msgs := clonedForSend(history)
			EvictStaleOutboundToolImages(msgs, nil)
			if b := outboundImageBlocks(msgs); b > testOutboundImageLimit {
				t.Fatalf("n=%d blocks %d", n, b)
			}
			return imageBearingToolIDs(msgs)
		}
		window := testOutboundImageLimit / 3
		lo, hi := window+1, window+1+4*(window-testOutboundImageFloor)
		var frontier []string
		for n := lo; n < hi; n++ {
			kept := surviving(n)
			if len(kept) < testOutboundImageFloor {
				t.Fatalf("n=%d kept %v", n, kept)
			}
			frontier = append(frontier, kept[0])
		}
		if moves := frontierMoves(frontier); moves > (hi-lo)/(window-testOutboundImageFloor) {
			t.Fatalf("frontier moved %d times %v", moves, frontier)
		}
	})
	t.Run("multi image tool results count as blocks", func(t *testing.T) {
		history := []map[string]any{{"role": "user", "content": "start"}}
		for i := 0; i < 8; i++ {
			history = append(history, multiImageTool(i, 3, "AAA")...)
		}
		outbound := sanitizeForSend(history)
		if EvictStaleOutboundToolImages(outbound, nil) <= 0 || outboundImageBlocks(outbound) > testOutboundImageLimit {
			t.Fatalf("blocks %d", outboundImageBlocks(outbound))
		}
	})
	t.Run("user uploads count against the ceiling", func(t *testing.T) {
		screenshots := []map[string]any{}
		for i := 0; i < 16; i++ {
			screenshots = append(screenshots, imageTool(i)...)
		}
		if n := EvictStaleOutboundToolImages(sanitizeForSend(append([]map[string]any{{"role": "user", "content": "start"}}, screenshots...)), nil); n != 0 {
			t.Fatalf("16 screenshots alone evicted %d", n)
		}
		for _, uploads := range []int{8, 12} {
			outbound := sanitizeForSend(append([]map[string]any{uploadsRow(uploads)}, screenshots...))
			EvictStaleOutboundToolImages(outbound, nil)
			if outboundImageBlocks(outbound) > testOutboundImageLimit || userImageCount(outbound) != uploads {
				t.Fatalf("%d uploads blocks=%d intact=%d", uploads, outboundImageBlocks(outbound), userImageCount(outbound))
			}
		}
	})
	t.Run("keep newest is a floor when uploads fill the ceiling", func(t *testing.T) {
		history := []map[string]any{uploadsRow(testOutboundImageLimit + 1)}
		for i := 0; i < 5; i++ {
			history = append(history, imageTool(i)...)
		}
		outbound := sanitizeForSend(history)
		EvictStaleOutboundToolImages(outbound, nil)
		if kept := len(imageBearingToolIDs(outbound)); kept < testOutboundImageFloor || kept >= testOutboundImageFloor+testImageEvictionBatch {
			t.Fatalf("kept %d", kept)
		}
	})
	t.Run("a batch that would blind the model stops at the floor", func(t *testing.T) {
		history := []map[string]any{uploadsRow(testOutboundImageLimit - 5)}
		for i := 0; i < 6; i++ {
			history = append(history, imageTool(i)...)
		}
		outbound := sanitizeForSend(history)
		EvictStaleOutboundToolImages(outbound, nil)
		kept := imageBearingToolIDs(outbound)
		if outboundImageBlocks(outbound) > testOutboundImageLimit || len(kept) < testOutboundImageFloor || !slices.Equal(kept[len(kept)-testOutboundImageFloor:], callRange(6-testOutboundImageFloor, 6)) {
			t.Fatalf("kept %v blocks %d", kept, outboundImageBlocks(outbound))
		}
	})
	t.Run("byte pressure overrides the keep newest floor", func(t *testing.T) {
		mb := 1024 * 1024
		hardLimit := 32 * mb
		blob := func(sizeMB float64) map[string]any {
			return imagePart("data:image/jpeg;base64," + strings.Repeat("Q", int(sizeMB*float64(mb))))
		}
		content := []any{map[string]any{"type": "text", "text": "look"}}
		for k := 0; k < 5; k++ {
			content = append(content, blob(5.0))
		}
		history := []map[string]any{{"role": "user", "content": content}}
		for i := 0; i < 3; i++ {
			history = append(history, imageToolPair(i, fmt.Sprintf("shot %d", i), blob(3.0))...)
		}
		outbound := sanitizeForSend(history)
		size := func() int {
			s, err := pyjson.Dumps(outbound, pyjson.EnsureASCII(false))
			if err != nil {
				t.Fatal(err)
			}
			return len([]rune(s))
		}
		if size() <= hardLimit {
			t.Fatal("fixture starts under the hard limit")
		}
		EvictStaleOutboundToolImages(outbound, nil)
		if size() >= hardLimit || len(imageBearingToolIDs(outbound)) != 0 || userImageCount(outbound) != 5 {
			t.Fatalf("size %d kept %v uploads %d", size(), imageBearingToolIDs(outbound), userImageCount(outbound))
		}
	})
	t.Run("floor yields when eviction can fix the breach", func(t *testing.T) {
		history := append([]map[string]any{{"role": "user", "content": "start"}}, multiImageTool(0, testOutboundImageLimit+5, "A")...)
		outbound := sanitizeForSend(history)
		if outboundImageBlocks(outbound) <= testOutboundImageLimit {
			t.Fatal("fixture under the ceiling")
		}
		EvictStaleOutboundToolImages(outbound, nil)
		if b := outboundImageBlocks(outbound); b > testOutboundImageLimit {
			t.Fatalf("blocks %d", b)
		}
	})
	t.Run("does not rewrite persisted history", func(t *testing.T) {
		n := testOutboundImageLimit + 1
		history := historyWithScreenshots(n)
		outbound := clonedForSend(history)
		EvictStaleOutboundToolImages(outbound, nil)
		if !slices.Equal(imageBearingToolIDs(history), callRange(0, n)) || !slices.Equal(imageBearingToolIDs(outbound), callRange(testImageEvictionBatch, n)) {
			t.Fatalf("history %v outbound %v", imageBearingToolIDs(history), imageBearingToolIDs(outbound))
		}
	})
	t.Run("user uploads are not evicted", func(t *testing.T) {
		history := []map[string]any{{"role": "user", "content": []any{map[string]any{"type": "text", "text": "look"}, imagePart("data:image/png;base64,USERUPLOAD")}}}
		for i := 0; i < testOutboundImageLimit; i++ {
			history = append(history, imageTool(i)...)
		}
		outbound := sanitizeForSend(history)
		if EvictStaleOutboundToolImages(outbound, nil) <= 0 {
			t.Fatal("nothing evicted")
		}
		for _, m := range outbound {
			if m["role"] == "user" {
				url := m["content"].([]any)[1].(map[string]any)["image_url"].(map[string]any)["url"].(string)
				if !strings.HasSuffix(url, "USERUPLOAD") {
					t.Fatalf("upload rewritten %q", url)
				}
				break
			}
		}
	})
}
