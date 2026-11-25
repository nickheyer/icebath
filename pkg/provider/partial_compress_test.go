package provider

import (
	"fmt"
	"reflect"
	"testing"
)

func partialMsg(role string, content any) map[string]any {
	return map[string]any{"role": role, "content": content}
}

func partialRoles(msgs []map[string]any) []any {
	out := []any{}
	for _, m := range msgs {
		if m["role"] == "user" || m["role"] == "assistant" {
			out = append(out, m["role"])
		}
	}
	return out
}

func partialNoConsecutiveDupes(msgs []map[string]any) bool {
	r := partialRoles(msgs)
	for i := 0; i+1 < len(r); i++ {
		if r[i] == r[i+1] {
			return false
		}
	}
	return true
}

func partialHistory(nPairs int) []map[string]any {
	h := []map[string]any{}
	for i := range nPairs {
		h = append(h, partialMsg("user", fmt.Sprintf("u%d", i)), partialMsg("assistant", fmt.Sprintf("a%d", i)))
	}
	return h
}

func partialConcat(a, b []map[string]any) []map[string]any {
	return append(append([]map[string]any{}, a...), b...)
}

func TestPartialCompressHereWithCount(t *testing.T) {
	partial, keep, focus := ParsePartialCompressArgs("here 4")
	if !partial || keep != 4 || focus != nil {
		t.Fatalf("got %v %d %v", partial, keep, focus)
	}
}

func TestPartialCompressKeepFlagForms(t *testing.T) {
	for _, arg := range []string{"--keep 5", "-k 5", "--keep=5"} {
		partial, keep, focus := ParsePartialCompressArgs(arg)
		if !partial || keep != 5 || focus != nil {
			t.Fatalf("%s: got %v %d %v", arg, partial, keep, focus)
		}
	}
}

func TestPartialCompressHereCountClampedLowAndHigh(t *testing.T) {
	if _, keepLow, _ := ParsePartialCompressArgs("here 0"); keepLow != 1 {
		t.Fatalf("low clamp %d", keepLow)
	}
	if _, keepHigh, _ := ParsePartialCompressArgs(fmt.Sprintf("here %d", MaxKeepLast+50)); keepHigh != MaxKeepLast {
		t.Fatalf("high clamp %d", keepHigh)
	}
}

func TestPartialCompressSplitTailAlwaysStartsOnUser(t *testing.T) {
	h := []map[string]any{
		partialMsg("user", "u0"), partialMsg("assistant", "a0"), partialMsg("user", "u1"), partialMsg("assistant", "a1"),
		partialMsg("tool", "t1"), partialMsg("assistant", "a1b"), partialMsg("user", "u2"), partialMsg("assistant", "a2"),
	}
	head, tail := SplitHistoryForPartialCompress(h, 1)
	if tail[0]["role"] != "user" || tail[0]["content"] != "u2" {
		t.Fatalf("tail starts on %v", tail[0])
	}
	if !reflect.DeepEqual(partialConcat(head, tail), h) {
		t.Fatalf("head plus tail %v", partialConcat(head, tail))
	}
}

func TestRejoinCompressedValidSeamAssistantThenUser(t *testing.T) {
	head := []map[string]any{partialMsg("user", "[summary]"), partialMsg("assistant", "ack")}
	tail := []map[string]any{partialMsg("user", "next"), partialMsg("assistant", "reply")}
	out := RejoinCompressedHeadAndTail(head, tail)
	if !reflect.DeepEqual(out, partialConcat(head, tail)) || !partialNoConsecutiveDupes(out) {
		t.Fatalf("got %v", out)
	}
}

func TestRejoinCompressedUserUserSeamMerges(t *testing.T) {
	head := []map[string]any{partialMsg("user", "[summary of head]")}
	tail := []map[string]any{partialMsg("user", "latest question"), partialMsg("assistant", "answer")}
	out := RejoinCompressedHeadAndTail(head, tail)
	if !partialNoConsecutiveDupes(out) {
		t.Fatalf("dupes %v", out)
	}
	if out[0]["content"] != "[summary of head]\n\nlatest question" {
		t.Fatalf("merged %v", out[0])
	}
	if !reflect.DeepEqual(out[1], partialMsg("assistant", "answer")) {
		t.Fatalf("second %v", out[1])
	}
}

func TestRejoinCompressedAssistantAssistantSeamMerges(t *testing.T) {
	head := []map[string]any{partialMsg("user", "q"), partialMsg("assistant", "head end")}
	tail := []map[string]any{partialMsg("assistant", "tail start"), partialMsg("user", "u")}
	out := RejoinCompressedHeadAndTail(head, tail)
	if !partialNoConsecutiveDupes(out) || out[len(out)-2]["content"] != "head end\n\ntail start" {
		t.Fatalf("got %v", out)
	}
}

func TestRejoinCompressedToolSeamLeftAlone(t *testing.T) {
	head := []map[string]any{partialMsg("user", "q"), partialMsg("tool", "t1")}
	tail := []map[string]any{partialMsg("user", "u")}
	if out := RejoinCompressedHeadAndTail(head, tail); !reflect.DeepEqual(out, partialConcat(head, tail)) {
		t.Fatalf("got %v", out)
	}
}

func TestCompressFlagsDryRunIsPreview(t *testing.T) {
	for _, form := range []string{"--dry-run", "--dryrun", "--DRY-RUN"} {
		if _, preview, _ := ExtractCompressFlags(form); !preview {
			t.Fatalf("%s not preview", form)
		}
	}
}

func TestCompressFlagsCoexistWithFocusTopic(t *testing.T) {
	rest, preview, _ := ExtractCompressFlags("database schema --dry-run")
	if rest != "database schema" || !preview {
		t.Fatalf("got %q %v", rest, preview)
	}
	partial, _, focus := ParsePartialCompressArgs(rest)
	if partial || focus == nil || *focus != "database schema" {
		t.Fatalf("got %v %v", partial, focus)
	}
}

func TestCompressPreviewFullCompressCounts(t *testing.T) {
	report := SummarizeCompressPreview(partialHistory(5), false, DefaultKeepLast, nil, 1234)
	if report["head_count"] != 10 || report["tail_count"] != 0 || report["total"] != 10 || report["partial"] != false {
		t.Fatalf("got %v", report)
	}
}

func TestCompressPreviewPartialBoundaryCounts(t *testing.T) {
	report := SummarizeCompressPreview(partialHistory(5), true, 2, nil, 999)
	if report["head_count"] != 6 || report["tail_count"] != 4 || report["partial"] != true {
		t.Fatalf("got %v", report)
	}
}

func TestCompressPreviewIsSideEffectFree(t *testing.T) {
	hist := partialHistory(4)
	before := []map[string]any{}
	for _, m := range hist {
		before = append(before, map[string]any{"role": m["role"], "content": m["content"]})
	}
	SummarizeCompressPreview(hist, true, 1, nil, 10)
	if !reflect.DeepEqual(hist, before) {
		t.Fatalf("history changed %v", hist)
	}
}

func TestPartialCompressParseForms(t *testing.T) {
	focus := func(s string) *string { return &s }
	cases := []struct {
		raw     string
		partial bool
		keep    int
		focus   *string
	}{
		{"", false, DefaultKeepLast, nil},
		{"   ", false, DefaultKeepLast, nil},
		{"here", true, DefaultKeepLast, nil},
		{"  HERE 3 ", true, 3, nil},
		{"up to here", true, DefaultKeepLast, nil},
		{"Up To Here 7", true, 7, nil},
		{"up to hereabouts", false, DefaultKeepLast, focus("hereabouts")},
		{"here lots", true, DefaultKeepLast, nil},
		{"--KEEP=9", true, 9, nil},
		{"--keep=", true, DefaultKeepLast, nil},
		{"--keep", false, DefaultKeepLast, focus("--keep")},
		{"-k", false, DefaultKeepLast, focus("-k")},
		{"Database Schema", false, DefaultKeepLast, focus("Database Schema")},
		{"here　٣", true, 3, nil},
	}
	for _, c := range cases {
		partial, keep, got := ParsePartialCompressArgs(c.raw)
		if partial != c.partial || keep != c.keep || (got == nil) != (c.focus == nil) || (got != nil && *got != *c.focus) {
			t.Fatalf("%q: got %v %d %v", c.raw, partial, keep, got)
		}
	}
}

func TestCoerceKeepClampsAndDefaults(t *testing.T) {
	cases := map[string]int{
		"5": 5, "1": 1, "0": 1, "-4": 1, "100": 100, "101": MaxKeepLast, "+7": 7, "1_0": 10,
		"99999999999999999999999": MaxKeepLast, "-99999999999999999999999": 1, "x": DefaultKeepLast,
		"": DefaultKeepLast, "2.5": DefaultKeepLast, "_1": DefaultKeepLast, "٤": 4,
	}
	for in, want := range cases {
		if got := coerceKeep(in); got != want {
			t.Fatalf("%q: got %d want %d", in, got, want)
		}
	}
}

func TestCompressFlagsStripAnywhere(t *testing.T) {
	rest, preview, aggressive := ExtractCompressFlags("  --Preview here\t2 --AGGRESSIVE  ")
	if rest != "here 2" || !preview || !aggressive {
		t.Fatalf("got %q %v %v", rest, preview, aggressive)
	}
	rest, preview, aggressive = ExtractCompressFlags("")
	if rest != "" || preview || aggressive {
		t.Fatalf("empty got %q %v %v", rest, preview, aggressive)
	}
}

func TestCompressPreviewLines(t *testing.T) {
	topic := "db"
	report := SummarizeCompressPreview(partialHistory(5), true, 2, &topic, 1234567)
	want := []string{
		"Preview — no changes made.",
		"Would compress 6 of 10 message(s) (~1,234,567 tokens currently in context).",
		"Boundary: keeping the last 2 exchange(s) (4 message(s)) verbatim.",
		"Focus topic: \"db\"",
		"Run the command again without --preview to apply.",
	}
	if !reflect.DeepEqual(report["lines"], want) {
		t.Fatalf("lines %q", report["lines"])
	}
	empty := ""
	fallback := SummarizeCompressPreview(partialHistory(2), true, 5, &empty, 12)
	want = []string{
		"Preview — no changes made.",
		"Would compress 4 of 4 message(s) (~12 tokens currently in context).",
		"Boundary: 'here' split would keep everything — falling back to full compression.",
		"Run the command again without --preview to apply.",
	}
	if !reflect.DeepEqual(fallback["lines"], want) || fallback["partial"] != false || fallback["tail_count"] != 0 {
		t.Fatalf("fallback %v", fallback)
	}
	full := SummarizeCompressPreview(nil, false, 2, nil, -1500)
	if lines := full["lines"].([]string); len(lines) != 3 || lines[1] != "Would compress 0 of 0 message(s) (~-1,500 tokens currently in context)." {
		t.Fatalf("full %q", lines)
	}
}

func TestPartialCompressSplitDegenerateAndCopies(t *testing.T) {
	head, tail := SplitHistoryForPartialCompress(nil, 2)
	if len(head) != 0 || len(tail) != 0 || head == nil || tail == nil {
		t.Fatalf("empty got %v %v", head, tail)
	}
	noUser := []map[string]any{partialMsg("assistant", "a"), partialMsg("tool", "t")}
	head, tail = SplitHistoryForPartialCompress(noUser, 1)
	if !reflect.DeepEqual(head, noUser) || len(tail) != 0 {
		t.Fatalf("no user got %v %v", head, tail)
	}
	all := partialHistory(2)
	head, tail = SplitHistoryForPartialCompress(all, 5)
	if !reflect.DeepEqual(head, all) || len(tail) != 0 {
		t.Fatalf("all in tail got %v %v", head, tail)
	}
	h := partialHistory(3)
	head, tail = SplitHistoryForPartialCompress(h, 0)
	if len(head) != 4 || len(tail) != 2 {
		t.Fatalf("keep zero got %d %d", len(head), len(tail))
	}
	head = append(head, partialMsg("user", "extra"))
	head[0] = partialMsg("user", "changed")
	if h[4]["content"] != "u2" || h[0]["content"] != "u0" {
		t.Fatalf("split aliases its input %v", h)
	}
}

func TestRejoinCompressedBridgesMultimodalAndEmptySides(t *testing.T) {
	parts := []any{map[string]any{"type": "text", "text": "look"}}
	head := []map[string]any{partialMsg("assistant", "summary")}
	tail := []map[string]any{partialMsg("assistant", parts), partialMsg("user", "u")}
	out := RejoinCompressedHeadAndTail(head, tail)
	want := []map[string]any{partialMsg("assistant", "summary"), partialMsg("user", ""), partialMsg("assistant", parts), partialMsg("user", "u")}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("assistant bridge %v", out)
	}
	out = RejoinCompressedHeadAndTail([]map[string]any{partialMsg("user", parts)}, []map[string]any{partialMsg("user", "q")})
	if !reflect.DeepEqual(out[1], partialMsg("assistant", "")) || len(out) != 3 {
		t.Fatalf("user bridge %v", out)
	}
	if out := RejoinCompressedHeadAndTail(head, nil); !reflect.DeepEqual(out, head) {
		t.Fatalf("empty tail %v", out)
	}
	if out := RejoinCompressedHeadAndTail(nil, tail); !reflect.DeepEqual(out, tail) {
		t.Fatalf("empty head %v", out)
	}
	last := map[string]any{"role": "user", "content": "s", "name": "x"}
	out = RejoinCompressedHeadAndTail([]map[string]any{last}, []map[string]any{partialMsg("user", "q")})
	if last["content"] != "s" || !reflect.DeepEqual(out, []map[string]any{{"role": "user", "content": "s\n\nq", "name": "x"}}) {
		t.Fatalf("merge mutated or lost keys %v %v", last, out)
	}
	odd := []map[string]any{{"role": []any{"user"}, "content": "a"}}
	if out := RejoinCompressedHeadAndTail(odd, odd); len(out) != 2 {
		t.Fatalf("non string roles %v", out)
	}
}
