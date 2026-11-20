package agent

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

const degenerateEnglishPrompt = "Build the workbook and report back."

func TestLooksLikeDegenerateFinalFragmentsInAnEnglishConversation(t *testing.T) {
	for _, text := range []string{"пар", "?warming up", ",and", "ошибка", "!warming"} {
		if !looksLikeDegenerateFinal(text, degenerateEnglishPrompt) {
			t.Errorf("%q should read as a fragment", text)
		}
	}
}

func TestLooksLikeDegenerateFinalTerseAnswersAreNotFragments(t *testing.T) {
	for _, text := range []string{
		"42", "SQLite", "report.csv", "3.14159", "€12.50", "你好。", "Done.", "True", "a51143fbbe",
		"2026-09-19", "/tmp/out.log", "$5", "#123", "-1", "(a)", ".env", "~/x", "+1", "✅",
		":8080", "::1", ":)", ";;", ":=", "}", "N/A", "**Done**", "`ok`", "{}", "null",
		"Reading is a skill", "The answer is 43.", strings.Repeat("x", 25), "",
	} {
		if looksLikeDegenerateFinal(text, degenerateEnglishPrompt) {
			t.Errorf("%q should stay an answer", text)
		}
	}
}

func TestLooksLikeDegenerateFinalTerseAnswersInTheUsersOwnScriptAreNotFragments(t *testing.T) {
	cases := []struct {
		prompt any
		text   string
	}{
		{"把工作簿建好然后告诉我", "是"}, {"把工作簿建好然后告诉我", "已完成"},
		{"Собери таблицу и отчитайся", "Готово"}, {"Собери таблицу и отчитайся", "да"},
		{"ワークブックを作って報告して", "はい"}, {"أنشئ المصنف ثم أخبرني", "تم"},
		{[]any{map[string]any{"type": "text", "text": "Собери таблицу"}}, "нет"},
	}
	for _, c := range cases {
		if looksLikeDegenerateFinal(c.text, c.prompt) {
			t.Errorf("%q to %v should stay an answer", c.text, c.prompt)
		}
	}
}

func TestDetectsTrailingLetMeNow(t *testing.T) {
	if !trailingContinueIntent("Found the config file. Let me now update it.") {
		t.Fatal("let me now tail missed")
	}
}

func TestDetectsTrailingIWillNow(t *testing.T) {
	if !trailingContinueIntent("The tests pass. I will now push the branch.") || !trailingContinueIntent("Good. I'll now run the linter") {
		t.Fatal("i will now tail missed")
	}
}

func TestDetectsTrailingNextI(t *testing.T) {
	if !trailingContinueIntent("Done with step one. Next, I check the logs") || !trailingContinueIntent("Step one complete. Next: I run the tests") {
		t.Fatal("next i tail missed")
	}
}

func TestIgnoresIntentFollowedByMoreContent(t *testing.T) {
	if trailingContinueIntent("I will now explain the tradeoffs. First, caching: the design keeps the prefix stable. Second, alternation: roles must strictly alternate.") {
		t.Fatal("mid message intent tripped the guard")
	}
}

func TestIgnoresLongSubstantiveReplies(t *testing.T) {
	if trailingContinueIntent(strings.Repeat("Here is the full analysis. ", 30) + "Let me now summarize.") {
		t.Fatal("long reply tripped the guard")
	}
}

func TestIgnoresPlainFinalAnswers(t *testing.T) {
	for _, text := range []string{"The answer is 42.", "All tests pass and the branch is pushed.", ""} {
		if trailingContinueIntent(text) {
			t.Errorf("%q tripped the guard", text)
		}
	}
}

func TestIgnoresConversationalFutureOffers(t *testing.T) {
	for _, text := range []string{"I can help with that tomorrow if you'd like.", "If you want, I will happily review the PR once CI is green. Just say so!"} {
		if trailingContinueIntent(text) {
			t.Errorf("%q tripped the guard", text)
		}
	}
}

func TestPromotedReasoningDetectorCatchesFirstPersonPlanTails(t *testing.T) {
	for _, tail := range []string{
		"Let me batch the terminal calls and run them in parallel.",
		"...Let me load the doctrine skill first, then run checks.",
		"Initial hypothesis: the config is stale. I need to check the log.",
		"I'm going to run the tests now",
		"嗯，长度合适。Let me check the file first.",
	} {
		if trailingContinueIntent(tail) || !promotedReasoningAnnouncesAction(tail) {
			t.Errorf("%q misclassified", tail)
		}
	}
	if !promotedReasoningAnnouncesAction(strings.Repeat("Thinking about the task. ", 60) + "Let me read the file.") {
		t.Fatal("long monologue tail missed")
	}
}

func TestPromotedReasoningDetectorIgnoresStatedAnswers(t *testing.T) {
	for _, text := range []string{
		"The answer is 42.",
		"Let me check the arithmetic. 6 times 7 is 42, so the answer is 42.",
		"If you want, I will happily review the PR once CI is green. Just say so!",
		"structured reasoning answer",
		"",
	} {
		if promotedReasoningAnnouncesAction(text) {
			t.Errorf("%q read as a plan", text)
		}
	}
}

func TestPromotedReasoningDetectorCatchesThaiPlanTails(t *testing.T) {
	for _, tail := range []string{
		"พร้อมแล้ว — จะให้ผมส่ง JANUS...",
		"ผมจะตรวจโค้ดให้เดี๋ยวนี้เลยครับ",
		"เข้าใจแล้ว. ต่อไปจะลองรันเทสต์ดูครับ",
		"คิดอยู่… ขอเริ่มจากไฟล์แรกก่อนนะครับ",
		"บั๊กอยู่ตรงนี้\nจะแก้ให้เลยครับ",
	} {
		if !promotedReasoningAnnouncesAction(tail) {
			t.Errorf("%q missed", tail)
		}
	}
}

func TestPromotedReasoningDetectorIgnoresThaiStatedAnswers(t *testing.T) {
	for _, text := range []string{"คำตอบคือ 42 ครับ", "ตรวจสอบแล้ว. คำตอบคือ 42 ครับ", "พรุ่งนี้จะฝนตกทั่วประเทศ"} {
		if promotedReasoningAnnouncesAction(text) {
			t.Errorf("%q read as a plan", text)
		}
	}
}

func TestToolResultsThisTurnStopsAtTheLatestUserRow(t *testing.T) {
	msgs := []*state.Message{{Role: "user"}, {Role: "tool"}, {Role: "user"}, {Role: "assistant"}, {Role: "tool"}, {Role: "tool"}}
	if got := toolResultsThisTurn(msgs); got != 2 {
		t.Fatalf("toolResultsThisTurn = %d", got)
	}
}

func TestIntentAckContinuationModeMirrorsToolUseEnforcement(t *testing.T) {
	cases := []struct {
		mode    any
		apiMode string
		want    string
	}{
		{true, "chat_completions", "all"}, {"Always", "chat_completions", "all"}, {false, "codex_responses", "off"},
		{"never", "codex_responses", "off"}, {[]any{"GPT-5"}, "chat_completions", "all"}, {[]any{"claude"}, "chat_completions", "off"},
		{"auto", "codex_responses", "codex_only"}, {nil, "chat_completions", "off"}, {"weird", "codex_responses", "codex_only"},
	}
	for _, c := range cases {
		a := &AIAgent{model: "gpt-5.4", apiMode: c.apiMode, intentAckContinuation: c.mode}
		if got := intentAckContinuationMode(a); got != c.want {
			t.Errorf("mode %v on %s = %s, want %s", c.mode, c.apiMode, got, c.want)
		}
	}
}
