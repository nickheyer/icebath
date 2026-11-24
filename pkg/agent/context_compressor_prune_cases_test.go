package agent

import (
	"database/sql"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Assistant row issuing one function call with the given id, name and arguments
func toolCallRow(id, name, args string) map[string]any {
	return map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": id, "type": "function",
		"function": map[string]any{"name": name, "arguments": args}}}}
}

// Copies each row map so a pass cannot alias the caller's rows
func dictCopies(rows []map[string]any) []map[string]any {
	out := make([]map[string]any, len(rows))
	for i, r := range rows {
		m := make(map[string]any, len(r))
		for k, v := range r {
			m[k] = v
		}
		out[i] = m
	}
	return out
}

// Stores the text in an in memory sqlite table and fails unless it reads back unchanged
func sqliteRoundTrip(t *testing.T, text string) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var back string
	if _, err := db.Exec("CREATE TABLE messages (content TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO messages VALUES (?)", text); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT content FROM messages").Scan(&back); err != nil || back != text {
		t.Fatalf("sqlite round trip %q %v", back, err)
	}
}

// Clarify fixture transcript with the answered call at index 3 and twelve finished steps after it
func clarifyPruneTranscript(content string) []map[string]any {
	messages := []map[string]any{
		{"role": "system", "content": "Test fixture"},
		{"role": "user", "content": "Implement the task"},
		toolCallRow("clarify-1", "clarify", "{}"),
		{"role": "tool", "tool_call_id": "clarify-1", "content": content},
	}
	for i := 0; i < 12; i++ {
		messages = append(messages, map[string]any{"role": "assistant", "content": "Finished step " + strconv.Itoa(i)},
			map[string]any{"role": "user", "content": "Continue step " + strconv.Itoa(i)})
	}
	return append(messages, map[string]any{"role": "assistant", "content": "Current step finished"})
}

func TestClarifyAnswersSurviveRepeatedPruning(t *testing.T) {
	for _, size := range []int{300, 4500} {
		for _, answer := range clarifyAnswers {
			want := clarifyAnswerSummary(answer)
			content := clarifyResultContent(t, size, "answered", answer)
			c := newCompressorWith(t, 100000, func(o *ContextCompressorOptions) { o.ProtectFirstN, o.ProtectLastN = 1, 2 })
			c.SetTailTokenBudget(50)
			messages := clarifyPruneTranscript(content)
			pruned, _ := c.pruneOldToolResults(messages, 2, nil, pruneMinChars)
			pruned, _ = c.pruneOldToolResults(pruned, 2, nil, pruneMinChars)
			if pruned[3]["content"] != want || summarizeToolResult("clarify", "{}", content) != want {
				t.Fatalf("%d %v: got %q want %q", size, answer, pruned[3]["content"], want)
			}
			var requests []map[string]any
			patchCallLLM(t, func(kwargs map[string]any) (any, error) {
				requests = append(requests, kwargs)
				return map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "## Progress\nWork is in progress."}, "finish_reason": "stop"}}}, nil
			})
			if _, err := c.Compress(messages, nil, nil, true, ""); err != nil {
				t.Fatal(err)
			}
			if len(requests) == 0 {
				t.Fatalf("%d %v: compression dispatched no summary request", size, answer)
			}
			dispatched, _ := jsonutil.Encode(requests[0], jsonutil.Spaced(), jsonutil.ASCII())
			quoted, _ := jsonutil.Encode(want, jsonutil.Spaced(), jsonutil.ASCII())
			if !strings.Contains(dispatched, quoted[1:len(quoted)-1]) {
				t.Fatalf("%d %v: the summary request lacks %s", size, answer, quoted)
			}
			if messages[3]["content"] != content {
				t.Fatalf("%d %v: the caller transcript changed", size, answer)
			}
		}
	}
}

func TestUnpairedSurrogatesAreSafeThroughPruningAndSqlite(t *testing.T) {
	c := fixtureCompressor(t)
	content := `{"responses": [{"status": "answered", "user_response": "Привет 😀` +
		strings.Repeat(`\ud83d`, 1000) + `"}], "outcome": "submitted"}`
	messages := []map[string]any{
		toolCallRow("clarify-1", "clarify", "{}"),
		{"role": "tool", "tool_call_id": "clarify-1", "content": content},
		{"role": "user", "content": "recent request"},
		{"role": "assistant", "content": "recent response"},
	}
	pruned, count := c.pruneOldToolResults(messages, 2, nil, pruneMinChars)
	summary, _ := pruned[1]["content"].(string)
	if count != 1 || utf8.RuneCountInString(summary) > pruneMinChars || !utf8.ValidString(summary) {
		t.Fatalf("count=%d len=%d valid=%v %q", count, utf8.RuneCountInString(summary), utf8.ValidString(summary), summary)
	}
	if !strings.Contains(summary, "Привет 😀") || !strings.Contains(summary, `\ud83d`) {
		t.Fatalf("summary %q", summary)
	}
	sqliteRoundTrip(t, summary)
	again, _ := c.pruneOldToolResults(pruned, 2, nil, pruneMinChars)
	if again[1]["content"] != summary {
		t.Fatalf("second pass %q", again[1]["content"])
	}
}

func TestPruneKeepsDenialAcrossPasses(t *testing.T) {
	c := fixtureCompressor(t)
	cliDenied, _, _ := tools.CommandGateRefusalTexts()
	args := dumps(t, obj("command", "rm -rf build"))
	content := tools.TerminalErrorJSON(cliDenied, "blocked")
	if utf8.RuneCountInString(content) <= pruneMinChars {
		t.Fatalf("fixture too short %d", utf8.RuneCountInString(content))
	}
	messages := []map[string]any{
		toolCallRow("t1", "terminal", args),
		{"role": "tool", "tool_call_id": "t1", "content": content},
		{"role": "user", "content": "recent request"},
		{"role": "assistant", "content": "recent response"},
	}
	pruned, count := c.pruneOldToolResults(messages, 2, nil, pruneMinChars)
	summary, _ := pruned[1]["content"].(string)
	if count != 1 || !strings.Contains(summary, "BLOCKED, not run") || !strings.Contains(summary, "did NOT consent") {
		t.Fatalf("count=%d %q", count, summary)
	}
	again, _ := c.pruneOldToolResults(pruned, 2, nil, pruneMinChars)
	if again[1]["content"] != summary {
		t.Fatalf("second pass %q", again[1]["content"])
	}
}

func TestSideEffectFinalResultReturnedEndToEnd(t *testing.T) {
	c := fixtureCompressor(t)
	history := []map[string]any{
		{"role": "user", "content": "Do the work and use the tools."},
		{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "call_1", "type": "function",
			"function": map[string]any{"name": "run_side_effect", "arguments": "{}"}}}},
		{"role": "tool", "tool_call_id": "call_1", "content": "side effect completed"},
		{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "call_2", "type": "function",
			"function": map[string]any{"name": "gather_final_result", "arguments": "{}"}}}},
	}
	compressed := c.sanitizeToolPairs(dictCopies(history))
	last := compressed[len(compressed)-1]
	if last["role"] != "assistant" || !slices.Equal(callIDs(last), []string{"call_2"}) || last["content"] == "(tool call removed)" {
		t.Fatalf("tail %v", last)
	}
	messages := append(dictCopies(compressed), map[string]any{"role": "tool", "tool_call_id": "call_2", "content": "final computed result: 42"})
	if _, err := state.RepairMessageSequence(&messages); err != nil {
		t.Fatal(err)
	}
	found := false
	var results []map[string]any
	for _, m := range messages {
		if m["role"] == "assistant" {
			for _, id := range callIDs(m) {
				found = found || id == "call_2"
			}
		}
		if m["role"] == "tool" && m["tool_call_id"] == "call_2" {
			results = append(results, m)
		}
	}
	if !found || len(results) != 1 || !strings.Contains(results[0]["content"].(string), "42") {
		t.Fatalf("repair dropped the result %v", messages)
	}
}
