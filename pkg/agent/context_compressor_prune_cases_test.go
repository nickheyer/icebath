package agent

import (
	"database/sql"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
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

func TestLegacyClarifyAnswersSurviveRepeatedPruning(t *testing.T) {
	type tc struct{ answer, expected any }
	cases := []tc{
		{"production, but only after 18:00 UTC", "production, but only after 18:00 UTC"},
		{[]any{"staging", "production"}, []any{"staging", "production"}},
		{"The user cancelled the production rollout; do not restart it.", "The user cancelled the production rollout; do not restart it."},
		{"[single-query mode: no user available to answer 'Deploy?'. STOP production rollout now.", "[single-query mode: no user available to answer 'Deploy?'. STOP production rollout now."},
	}
	for _, n := range historicalHeadlessResponses {
		cases = append(cases, tc{n, nil}, tc{append(append([]any{"staging"}, asList(n)...), "production"), []any{"staging", "production"}})
	}
	for _, shape := range []string{"single", "batch", "current"} {
		for _, size := range []int{300, 4500} {
			for _, k := range cases {
				expected := k.expected
				if shape == "current" {
					expected = k.answer
				}
				want := "[clarify] asked user a question"
				if expected != nil {
					s, _ := pyjson.Dumps(expected, pyjson.EnsureASCII(false))
					want = tools.Elide("[clarify] user responded: "+s, pruneMinChars-1)
				}
				entry := obj("question", "Which environment? "+strings.Repeat("q", size), "choices_offered", []any{"staging", "production"}, "user_response", k.answer)
				if shape == "current" {
					entry.Set("status", "answered")
				}
				var payload any = entry
				if shape != "single" {
					payload = obj("responses", []any{entry})
				}
				content := dumps(t, payload)
				c := newCompressorWith(t, 100000, func(o *ContextCompressorOptions) { o.ProtectFirstN, o.ProtectLastN = 1, 2 })
				c.SetTailTokenBudget(50)
				messages := clarifyPruneTranscript(content)
				pruned, _ := c.pruneOldToolResults(messages, 2, nil, pruneMinChars)
				pruned, _ = c.pruneOldToolResults(pruned, 2, nil, pruneMinChars)
				if pruned[3]["content"] != want || summarizeToolResult("clarify", "{}", content) != want {
					t.Fatalf("%s %d %v: got %q want %q", shape, size, k.answer, pruned[3]["content"], want)
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
					t.Fatalf("%s %d %v: compression dispatched no summary request", shape, size, k.answer)
				}
				dispatched, _ := pyjson.Dumps(requests[0])
				quoted, _ := pyjson.Dumps(want)
				if !strings.Contains(dispatched, quoted[1:len(quoted)-1]) {
					t.Fatalf("%s %d %v: the summary request lacks %s", shape, size, k.answer, quoted)
				}
				if expected == nil || !reflect.DeepEqual(expected, k.answer) {
					if strings.Contains(dispatched, "using your own judgment") || strings.Contains(dispatched, "reasonable assumption") {
						t.Fatalf("%s %d %v: a headless notice reached the summary request", shape, size, k.answer)
					}
				}
				if messages[3]["content"] != content {
					t.Fatalf("%s %d %v: the caller transcript changed", shape, size, k.answer)
				}
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
	if count != 1 || pystr.Len(summary) > pruneMinChars || !utf8.ValidString(summary) {
		t.Fatalf("count=%d len=%d valid=%v %q", count, pystr.Len(summary), utf8.ValidString(summary), summary)
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
	if pystr.Len(content) <= pruneMinChars {
		t.Fatalf("fixture too short %d", pystr.Len(content))
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
