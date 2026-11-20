package slack

import (
	"fmt"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Rich text blocks of sections paragraphs of size x characters each
func unfurlRichBlocks(sections, size int) []any {
	var elements []any
	for i := 0; i < sections; i++ {
		elements = append(elements, map[string]any{"type": "rich_text_section", "elements": []any{
			map[string]any{"type": "text", "text": strings.Repeat("x", size)}}})
	}
	return []any{map[string]any{"type": "rich_text", "elements": elements}}
}

// Alert attachment carrying its own rich text blocks
func unfurlAttachment(i int) map[string]any {
	return map[string]any{"title": fmt.Sprintf("Alert %d", i), "title_link": "https://example.com/a", "blocks": unfurlRichBlocks(8, 400)}
}

func unfurlAttachments(n int) []any {
	var out []any
	for i := 0; i < n; i++ {
		out = append(out, unfurlAttachment(i))
	}
	return out
}

func TestNestedBlockTextStopsGrowingWithAttachmentCount(t *testing.T) {
	sizes := map[int]int{}
	for _, n := range []int{1, 5, 20} {
		sizes[n] = pystr.Len(appendLinkUnfurls("intro", unfurlAttachments(n)))
	}
	if growth := float64(sizes[20]-sizes[5]) / 15; growth >= 200 {
		t.Fatalf("per attachment growth %v, sizes %v", growth, sizes)
	}
	if sizes[20] >= 3*sizes[1] {
		t.Fatalf("sizes %v", sizes)
	}
	if !tools.HasCompressionMarker(appendLinkUnfurls("intro", unfurlAttachments(20))) {
		t.Fatal("the exhausted budget must leave a compression marker")
	}
}

func TestFirstAttachmentKeepsItsBodyAndHeadersSurviveExhaustion(t *testing.T) {
	out := appendLinkUnfurls("intro", unfurlAttachments(20))
	if !strings.Contains(out, strings.Repeat("x", 400)) {
		t.Fatal("attachment 0 block text must be fully present")
	}
	if !strings.Contains(out, "📎 [Alert 19](https://example.com/a)") {
		t.Fatal("later attachments must still announce themselves")
	}
}
