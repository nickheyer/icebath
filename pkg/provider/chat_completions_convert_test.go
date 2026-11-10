package provider

import "testing"

func TestModelForcesMaxCompletionTokensExportMatchesTheModelFamilies(t *testing.T) {
	if !ModelForcesMaxCompletionTokens("openai/gpt-5.4") || !ModelForcesMaxCompletionTokens("o3-mini") || ModelForcesMaxCompletionTokens("claude-sonnet-4") {
		t.Fatal("max completion tokens")
	}
}

func TestModelConsumesThoughtSignatureIsTheRouteSignatureRule(t *testing.T) {
	cases := []struct {
		model, baseURL, provider string
	}{
		{"gemini-2.5-pro", "", ""}, {"gpt-5", "https://generativelanguage.googleapis.com/v1beta", ""},
		{"gpt-5", "", "vertex"}, {"gpt-5", "https://api.openai.com/v1", "openai"}, {"mistral-large", "https://api.mistral.ai/v1", "mistral"},
	}
	for _, c := range cases {
		if ModelConsumesThoughtSignature(c.model, c.baseURL, c.provider) != modelConsumesThoughtSignature(c.model, c.baseURL, c.provider) {
			t.Fatalf("export differs for %+v", c)
		}
	}
	if !ModelConsumesThoughtSignature("gemini-2.5-pro", "", "") || ModelConsumesThoughtSignature("mistral-large", "https://api.mistral.ai/v1", "mistral") {
		t.Fatal("gemini model or mistral route misclassified")
	}
}

func TestEffortLadderIndexMatchesTheLadder(t *testing.T) {
	for _, level := range []string{"none", "high", "ultra", "bogus", ""} {
		if EffortLadderIndex(level) != ladderIndex(level) {
			t.Fatalf("export differs for %q", level)
		}
	}
	if EffortLadderIndex("xhigh") <= EffortLadderIndex("high") || EffortLadderIndex("bogus") != -1 {
		t.Fatal("ladder order")
	}
}

func TestStripStrayAndUnterminatedToolCallWrappers(t *testing.T) {
	for in, want := range map[string]string{
		"a</tool_call> b</function>\tc":     "abc",
		"x</ns:Function_Calls>\n\n y":       "xy",
		"keep </tool_result>\u2003z":        "keep z",
		"no </functions> change":            "no </functions> change",
		"a</antml.x:tool_calls>b":           "ab",
		"plain text with no tag whatever":   "plain text with no tag whatever",
		"tail</function_call>":              "tail",
		"</tool_calls>\r\n\x0b\x0cafter it": "after it",
	} {
		if got := StripStrayToolCallClosers(in); got != want {
			t.Fatalf("closers of %q: got %q want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"keep\n<tool_call>{\"name\"":             "keep",
		"keep\n  <ns:function_calls id=1>\nrest": "keep",
		"inline <tool_call> stays":               "inline <tool_call> stays",
		"a\n<arg_key>k\nb":                       "a\nb",
		"a\nvalue</arg_value>  \nb":              "a\nb",
		"<TOOL_RESULT>x":                         "",
		"a\n<function name=\"f\">cut tail stays": "a\n<function name=\"f\">cut tail stays",
	} {
		if got := StripUnterminatedToolCalls(in); got != want {
			t.Fatalf("unterminated of %q: got %q want %q", in, got, want)
		}
	}
}
