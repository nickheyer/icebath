package agent

import (
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestBoostedOutputCapLadders(t *testing.T) {
	ladder := func(a *AIAgent, requested int) []int {
		var out []int
		for n := 1; n <= 4; n++ {
			out = append(out, a.boostedOutputCap(requested, n, 0))
		}
		return out
	}
	if got := ladder(&AIAgent{maxTokens: 4096}, 0); !reflect.DeepEqual(got, []int{8192, 16384, 32768, 32768}) {
		t.Fatalf("small explicit max_tokens ladder %v", got)
	}
	for _, requested := range []int{32768, 65536} {
		got := ladder(&AIAgent{}, requested)
		if got[0] <= requested {
			t.Fatalf("first budget %d not above requested %d", got[0], requested)
		}
		for _, b := range got {
			if b > requested*2 {
				t.Fatalf("budget %d above twice the requested %d", b, requested)
			}
		}
	}
	if got := (&AIAgent{}).boostedOutputCap(0, 2, 3000); got != 12000 {
		t.Fatalf("explicit base boost %d", got)
	}
}

func TestPartialResultShape(t *testing.T) {
	msgs := []*state.Message{{Role: "user", Content: "hi"}}
	got := PartialResult(msgs, 3, "cut", nil, false, false)
	if got["partial"] != true || got["error"] != "cut" || got["completed"] != false || got["api_calls"] != 3 {
		t.Fatalf("partial result %v", got)
	}
	if _, ok := got["failed"]; ok {
		t.Fatal("partial result carries failed")
	}
	detail := "detail"
	got = PartialResult(msgs, 1, "cut", &detail, true, true)
	if got["failed"] != true || got["error"] != "detail" || got["compression_exhausted"] != true {
		t.Fatalf("failed result %v", got)
	}
	if _, ok := got["partial"]; ok {
		t.Fatal("failed result carries partial")
	}
}

func TestRepetitionCopyAndWindowNotice(t *testing.T) {
	if repetitionStreamCut[0] != "🔁 Response dominated by repeated text — stopping the stream mid-loop." {
		t.Fatalf("stream cut line %q", repetitionStreamCut[0])
	}
	if repetitionDominated[2] != "Model output entered a repetition loop and was truncated mid-loop; refusing to continue a degenerate response." {
		t.Fatalf("dominated error %q", repetitionDominated[2])
	}
	notice := windowFilledNotice(130500, 131072)
	if !strings.Contains(notice, "The prompt used 130,500 of this model's 131,072-token context window") {
		t.Fatalf("window notice %q", notice)
	}
}

func TestThinkTagPattern(t *testing.T) {
	for _, s := range []string{"<think>", "<THINKING foo>", "<reasoning>", "<reasoning_scratchpad>"} {
		if !thinkTagRE.MatchString(s) {
			t.Errorf("%q not matched", s)
		}
	}
	if thinkTagRE.MatchString("<thought>") {
		t.Error("thought tag matched")
	}
}

type recordingSpinner struct{ stops []string }

func (s *recordingSpinner) Stop(text string) { s.stops = append(s.stops, text) }

func TestStopThinkingSpinnerClearsCallback(t *testing.T) {
	var cleared []string
	a := &AIAgent{thinkingCallback: func(s string) { cleared = append(cleared, s) }}
	sp := &recordingSpinner{}
	if got := stopThinkingSpinner(a, sp); got != nil {
		t.Fatalf("spinner not dropped: %v", got)
	}
	if !reflect.DeepEqual(sp.stops, []string{""}) || !reflect.DeepEqual(cleared, []string{""}) {
		t.Fatalf("stops %v cleared %v", sp.stops, cleared)
	}
	if stopThinkingSpinner(&AIAgent{}, nil) != nil {
		t.Fatal("nil spinner returned non nil")
	}
}

func TestShouldStreamEarlyRefusals(t *testing.T) {
	off := &AIAgent{}
	off.DisableStreaming()
	if shouldStream(t.Context(), off) {
		t.Fatal("disabled streaming streamed")
	}
	for _, base := range []string{"acp://local", "ACP+TCP://host:1"} {
		if shouldStream(t.Context(), &AIAgent{baseURL: base}) {
			t.Fatalf("%s streamed", base)
		}
	}
}

func TestAppendLiveMessageStampsAndConverts(t *testing.T) {
	var msgs []*state.Message
	row := appendLiveMessage(&msgs, map[string]any{"role": "assistant", "content": "x", lengthFragmentKey: true})
	if len(msgs) != 1 || msgs[0].Content != "x" || msgs[0].Extra[lengthFragmentKey] != true {
		t.Fatalf("appended %#v", msgs)
	}
	if _, ok := row["timestamp"]; !ok {
		t.Fatal("row not stamped")
	}
}

func TestCloseLiveToolSequence(t *testing.T) {
	msgs := []*state.Message{{Role: "user", Content: "q"}, {Role: "tool", Content: "r", ToolCallID: "c1"}}
	closeLiveToolSequence(&msgs, "  done  ")
	if len(msgs) != 3 || msgs[2].Role != "assistant" || msgs[2].Content != "done" {
		t.Fatalf("closing row %#v", msgs[len(msgs)-1])
	}
	closeLiveToolSequence(&msgs, "again")
	if len(msgs) != 3 {
		t.Fatal("closed a transcript that ends on an assistant row")
	}
}
