package agent

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	testOutboundImageBudgetBytes = 24_000_000
	testManyImageCapPx           = 2000
	testMB                       = 1_000_000
)

var (
	unreadableImageURL = "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("not an image header at all"))
	remoteImageURL     = "https://example.com/shot.png"
)

// Data url whose PNG IHDR declares the width and height, the 24 header bytes a header only reader inspects
func pngDataURL(width, height int) string {
	ihdr := append([]byte("IHDR"), binary.BigEndian.AppendUint32(nil, uint32(width))...)
	ihdr = append(ihdr, binary.BigEndian.AppendUint32(nil, uint32(height))...)
	ihdr = append(ihdr, 0x08, 0x06, 0x00, 0x00, 0x00)
	raw := append([]byte("\x89PNG\r\n\x1a\n"), binary.BigEndian.AppendUint32(nil, uint32(len(ihdr)))...)
	raw = append(raw, ihdr...)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
}

// OpenAI shaped tool results, one image block each
func screenshotToolMessages(n int, url string) []map[string]any {
	var messages []map[string]any
	for i := range n {
		messages = append(messages, map[string]any{"role": "tool", "tool_call_id": fmt.Sprintf("call_%d", i),
			"content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("shot %d", i)}, imagePart(url)}})
	}
	return messages
}

// Anthropic wire user turns, each carrying one image tool_result
func wireToolResults(n int, url string) []map[string]any {
	data := url[strings.Index(url, ",")+1:]
	result := []map[string]any{{"role": "user", "content": []any{map[string]any{"type": "text", "text": "start"}}}}
	for i := range n {
		result = append(result, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("shot %d", i)}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": fmt.Sprintf("t%d", i),
				"content": []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": data}}}}}})
	}
	return result
}

func wireImageBlocks(result []map[string]any) int {
	n := 0
	for _, msg := range result {
		blocks, _ := msg["content"].([]any)
		for _, block := range blocks {
			bm, _ := block.(map[string]any)
			inner := []any{block}
			if bm["type"] == "tool_result" {
				inner, _ = bm["content"].([]any)
			}
			for _, x := range inner {
				if xm, ok := x.(map[string]any); ok && xm["type"] == "image" {
					n++
				}
			}
		}
	}
	return n
}

func repeatInts(v, n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func retireCount(blocks []int, reserved int, sizes []int, reservedBytes int) int {
	return provider.OutboundImageRetireCount(blocks, reserved, sizes, reservedBytes, false)
}

func TestRetireCount(t *testing.T) {
	cases := []struct {
		name          string
		blocks        []int
		reserved      int
		sizes         []int
		reservedBytes int
		expected      int
	}{
		{"at the limit", repeatInts(1, testOutboundImageLimit), 0, nil, 0, 0},
		{"one over", repeatInts(1, testOutboundImageLimit+1), 0, nil, 0, testImageEvictionBatch},
		{"first window after a batch", repeatInts(1, testOutboundImageLimit+testImageEvictionBatch), 0, nil, 0, testImageEvictionBatch},
		{"second window", repeatInts(1, testOutboundImageLimit+testImageEvictionBatch+1), 0, nil, 0, 2 * testImageEvictionBatch},
		{"one carrier breaching alone", []int{testOutboundImageLimit + 5}, 0, nil, 0, 1},
		{"four ten block carriers", []int{10, 10, 10, 10}, 0, nil, 0, 2},
		{"uploads alone breach", repeatInts(1, 5), testOutboundImageLimit + 1, nil, 0, 0},
		{"uploads alone breach with a batch of carriers", repeatInts(1, testOutboundImageFloor+testImageEvictionBatch), testOutboundImageLimit + 1, nil, 0, testImageEvictionBatch},
		{"uploads breach with fewer carriers than the floor", []int{1, 1}, testOutboundImageLimit + 5, nil, 0, 0},
		{"byte pressure from uploads", []int{1, 1, 1}, 5, repeatInts(3*testMB, 3), 25 * testMB, 3},
		{"bytes bind before blocks", repeatInts(1, 13), 0, repeatInts(2*testMB, 13), 0, testImageEvictionBatch},
		{"empty", []int{}, 0, nil, 0, 0},
	}
	for _, tc := range cases {
		if got := retireCount(tc.blocks, tc.reserved, tc.sizes, tc.reservedBytes); got != tc.expected {
			t.Errorf("%s: retire %d, want %d", tc.name, got, tc.expected)
		}
	}
}

func TestPreShrunkScreenshotsDoNotTripTheManyImageTrigger(t *testing.T) {
	shrunk := pngDataURL(1568, 882)
	for _, n := range []int{testOutboundImageLimit + 1, 30} {
		if got := EvictStaleOutboundToolImages(screenshotToolMessages(n, shrunk), nil); got != 0 {
			t.Fatalf("n=%d retired %d pre shrunk screenshots", n, got)
		}
		wire := wireToolResults(n, shrunk)
		provider.EvictOldScreenshots(wire)
		if got := wireImageBlocks(wire); got != n {
			t.Fatalf("n=%d wire pass kept %d blocks", n, got)
		}
	}
}

func TestOneOversizedImageKeepsTheStrictTrigger(t *testing.T) {
	shrunk := pngDataURL(1568, 882)
	odds := map[string]string{"oversized": pngDataURL(testManyImageCapPx+1, 8), "unreadable": unreadableImageURL, "remote": remoteImageURL}
	for _, odd := range []string{"oversized", "unreadable", "remote"} {
		url := odds[odd]
		upload := map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "look at this"}, imagePart(url)}}
		outbound := append([]map[string]any{upload}, screenshotToolMessages(testOutboundImageLimit, shrunk)...)
		if got := EvictStaleOutboundToolImages(outbound, nil); got != testImageEvictionBatch {
			t.Fatalf("%s: retired %d, want one batch", odd, got)
		}
		if kept := upload["content"].([]any)[1].(map[string]any)["image_url"].(map[string]any)["url"]; kept != url {
			t.Fatalf("%s: upload rewritten to %v", odd, kept)
		}
		wire := wireToolResults(testOutboundImageLimit, shrunk)
		var source map[string]any
		if odd == "remote" {
			source = map[string]any{"type": "url", "url": url}
		} else {
			source = map[string]any{"type": "base64", "media_type": "image/png", "data": url[strings.Index(url, ",")+1:]}
		}
		wire[0]["content"] = append(wire[0]["content"].([]any), map[string]any{"type": "image", "source": source})
		provider.EvictOldScreenshots(wire)
		if got := wireImageBlocks(wire); got != testOutboundImageLimit-testImageEvictionBatch+1 {
			t.Fatalf("%s: wire pass kept %d blocks", odd, got)
		}
	}
}

func TestWirePassEnforcesTheByteBudgetBelowTheBlockCeiling(t *testing.T) {
	big := strings.Repeat("Q", 3*testMB)
	imageBlock := func() map[string]any {
		return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/jpeg", "data": big}}
	}
	result := []map[string]any{{"role": "user", "content": []any{imageBlock()}}}
	for i := range 9 {
		result = append(result, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "s"}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": fmt.Sprintf("t%d", i), "content": []any{imageBlock()}}}})
	}
	before := wireImageBlocks(result)
	if !(10*len(big) > testOutboundImageBudgetBytes && before < testOutboundImageLimit) {
		t.Fatalf("fixture outside the byte pressure window: %d blocks", before)
	}
	provider.EvictOldScreenshots(result)
	if got := wireImageBlocks(result); got >= before {
		t.Fatalf("the wire pass ignored byte pressure while under the block ceiling: %d -> %d", before, got)
	}
}

func pairwiseMoves(counts []int) int {
	moves := 0
	for i := 1; i < len(counts); i++ {
		if counts[i] != counts[i-1] {
			moves++
		}
	}
	return moves
}

func ceilDiv(a, b int) int { return (a + b - 1) / b }

func TestQuantumShrinksToTheFitWindowForHeavyCarriers(t *testing.T) {
	window := testOutboundImageLimit / 3
	var counts []int
	for n := window + 1; n < window+10; n++ {
		c := retireCount(repeatInts(3, n), 0, nil, 0)
		if c <= 0 || 3*(n-c) > testOutboundImageLimit {
			t.Fatalf("n=%d retire %d leaves the request over the ceiling", n, c)
		}
		counts = append(counts, c)
	}
	if moves := pairwiseMoves(counts); moves > len(counts)/(window-testOutboundImageFloor) {
		t.Fatalf("retire count moved %d times over %v", moves, counts)
	}
}

func TestUnfixableUploadBreachStillRetiresInBatches(t *testing.T) {
	uploads := 51
	var counts []int
	for n := 1; n <= 30; n++ {
		c := retireCount(repeatInts(1, n), uploads, nil, 0)
		kept := n - c
		if kept < min(n, testOutboundImageFloor) || kept >= testOutboundImageFloor+testImageEvictionBatch {
			t.Fatalf("n=%d kept %d", n, kept)
		}
		counts = append(counts, c)
	}
	if moves := pairwiseMoves(counts); moves > ceilDiv(30-testOutboundImageFloor, testImageEvictionBatch) {
		t.Fatalf("retire count moved %d times over %v", moves, counts)
	}
}

func TestUnfixableUploadBreachUnderBytePressureFitsAndStillSteps(t *testing.T) {
	var counts []int
	for n := 1; n <= 30; n++ {
		c := retireCount(repeatInts(1, n), testOutboundImageLimit+1, repeatInts(3*testMB, n), 6*testMB)
		kept := n - c
		if 6*testMB+3*testMB*kept > 24*testMB || kept < min(n, testOutboundImageFloor) {
			t.Fatalf("n=%d kept %d", n, kept)
		}
		counts = append(counts, c)
	}
	if moves := pairwiseMoves(counts); moves > ceilDiv(30-testOutboundImageFloor, 4) {
		t.Fatalf("retire count moved %d times over %v", moves, counts)
	}
}

// Uploads with unreadable headers plus one screenshot tool round per size
func mixedToolHistory(sizes []int, uploads int) []map[string]any {
	content := []any{map[string]any{"type": "text", "text": "look"}}
	for range uploads {
		content = append(content, imagePart("data:image/png;base64,"+strings.Repeat("U", 285_000)))
	}
	rows := []map[string]any{{"role": "user", "content": content}}
	for i, size := range sizes {
		rows = append(rows, map[string]any{"role": "assistant", "content": "shot", "tool_calls": []any{map[string]any{"id": fmt.Sprintf("c%d", i), "type": "function",
			"function": map[string]any{"name": "vision_analyze", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "tool_call_id": fmt.Sprintf("c%d", i), "content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("shot %d", i)},
				imagePart("data:image/png;base64," + strings.Repeat("Q", size))}})
	}
	return rows
}

func hasImagePart(parts any) bool {
	list, _ := parts.([]any)
	for _, p := range list {
		if pm, ok := p.(map[string]any); ok && pm["type"] == "image_url" {
			return true
		}
	}
	return false
}

func supersetOf(a, b map[string]bool) bool {
	for k := range b {
		if !a[k] {
			return false
		}
	}
	return true
}

func TestASmallNewScreenshotNeverRestoresRetiredOnes(t *testing.T) {
	retired := func(sizes []int) (map[string]bool, map[string]bool) {
		rows := mixedToolHistory(sizes, testOutboundImageLimit+1)
		outbound := make([]map[string]any, len(rows))
		for i, m := range rows {
			cp := make(map[string]any, len(m))
			for k, v := range m {
				cp[k] = v
			}
			outbound[i] = cp
		}
		EvictStaleOutboundToolImages(outbound, nil)
		kept := map[string]bool{}
		for _, m := range outbound {
			if m["role"] == "tool" && hasImagePart(m["content"]) {
				kept[m["tool_call_id"].(string)] = true
			}
		}
		kwargs, err := provider.GetTransport("anthropic_messages").BuildKwargs("claude-sonnet-4-5", rows, nil, map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		wireKept := map[string]bool{}
		var wireRows []map[string]any
		switch rows := kwargs["messages"].(type) {
		case []map[string]any:
			wireRows = rows
		case []any:
			for _, r := range rows {
				wireRows = append(wireRows, r.(map[string]any))
			}
		}
		for _, m := range wireRows {
			blocks, _ := m["content"].([]any)
			for _, b := range blocks {
				bm, _ := b.(map[string]any)
				inner, isList := bm["content"].([]any)
				if bm["type"] != "tool_result" || !isList {
					continue
				}
				for _, x := range inner {
					if xm, ok := x.(map[string]any); ok && xm["type"] == "image" {
						wireKept[bm["tool_use_id"].(string)] = true
					}
				}
			}
		}
		gone, wireGone := map[string]bool{}, map[string]bool{}
		for i := range sizes {
			id := fmt.Sprintf("c%d", i)
			if !kept[id] {
				gone[id] = true
			}
			if !wireKept[id] {
				wireGone[id] = true
			}
		}
		return gone, wireGone
	}
	prevGone, prevWire := map[string]bool{}, map[string]bool{}
	for extra := range 4 {
		sizes := append(repeatInts(2_900_000, 11), repeatInts(100_000, extra)...)
		gone, wire := retired(sizes)
		if !supersetOf(gone, prevGone) || !supersetOf(wire, prevWire) {
			t.Fatalf("extra=%d restored retired screenshots: %v %v after %v %v", extra, gone, wire, prevGone, prevWire)
		}
		prevGone, prevWire = gone, wire
	}
	if len(prevGone) < testImageEvictionBatch {
		t.Fatalf("only %d retired", len(prevGone))
	}
}

func TestRetirementIsLatchedWhenAnUploadLeavesTheRequest(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	env := &tools.Env{Home: h}
	ctx := tools.WithToolEnv(context.Background(), env)
	a, err := NewAIAgent(ctx, AIAgentOptions{APIKey: "k", BaseURL: "https://api.anthropic.com", Provider: "anthropic", Model: "claude-sonnet-4-5",
		QuietMode: true, SkipContextFiles: true, SkipMemory: true, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	a.cachedSystemPrompt = "SYS"
	shrunk, oversized := pngDataURL(1568, 882), pngDataURL(testManyImageCapPx+400, 8)
	var toolRows []map[string]any
	for i := range testOutboundImageLimit {
		toolRows = append(toolRows, map[string]any{"role": "assistant", "content": "shot", "tool_calls": []any{map[string]any{"id": fmt.Sprintf("c%d", i), "type": "function",
			"function": map[string]any{"name": "vision_analyze", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "tool_call_id": fmt.Sprintf("c%d", i), "content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("shot %d", i)}, imagePart(shrunk)}})
	}
	kept := func(uploadURL string) []string {
		upload := []any{map[string]any{"type": "text", "text": "photo"}}
		if uploadURL != "" {
			upload = append(upload, imagePart(uploadURL))
		}
		history := append([]map[string]any{{"role": "user", "content": upload}}, toolRows...)
		history = append(history, map[string]any{"role": "user", "content": "next"})
		var ids []string
		for _, m := range provider.IterationSummaryAPIMessages(a, history) {
			if m["role"] == "tool" && hasImagePart(m["content"]) {
				ids = append(ids, m["tool_call_id"].(string))
			}
		}
		return ids
	}
	before := kept(oversized)
	if len(before) != testOutboundImageLimit-testImageEvictionBatch {
		t.Fatalf("kept %d screenshots with the oversized upload", len(before))
	}
	after := kept("")
	if strings.Join(after, ",") != strings.Join(before, ",") {
		t.Fatalf("retirement unlatched: %v -> %v", before, after)
	}
}
