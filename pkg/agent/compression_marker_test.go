package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var imitableMarkerRE = regexp.MustCompile(`(?:\.\.\.|\x{2026})\s?\[[^\]\n]*truncat`)

func TestMintedMarkerIsCaughtByTheDispatchBoundaryGuard(t *testing.T) {
	for _, out := range []string{tools.Elide(strings.Repeat("y", 4000), 199), tools.ElideMiddle(strings.Repeat("y", 4000), 100, 100)} {
		if !tools.HasCompressionMarker(out) {
			t.Fatalf("no marker in %q", out)
		}
	}
}

func TestRealRenderersEmitAGuardVisibleMarkerNeverTheImitableIdiom(t *testing.T) {
	quoted := strings.Repeat("a'b\"c ", 400)
	clarify := `{"responses": [{"status": "answered", "user_response": "` + strings.Repeat("A", 5000) + `"}]}`
	outputs := map[string]string{
		"clarify":       summarizeToolResult("clarify", "{}", clarify),
		"fallback_turn": compactFallbackTurn(strings.Repeat("z ", 5000)),
		"verbatim_user": buildVerbatimUserSection([]map[string]any{{"role": "user", "content": strings.Repeat("q", 30000)}}),
		"record":        boundOversizedRecord(strings.Repeat("r", 50000), 4000),
		"active_task":   latestUserTaskSnapshot([]map[string]any{{"role": "user", "content": quoted}}),
	}
	for name, out := range outputs {
		if out == "" || !tools.HasCompressionMarker(out) || imitableMarkerRE.MatchString(out) {
			t.Fatalf("%s %q", name, out[max(0, len(out)-300):])
		}
	}
	turns := []map[string]any{{"role": "user", "content": strings.Repeat("v", 500)}}
	for i := 0; i < 6; i++ {
		turns = append(turns, map[string]any{"role": "user", "content": strings.Repeat("u", 3_998)})
	}
	if strings.Contains(buildVerbatimUserSection(turns), "chars omitted") {
		t.Fatal("straddler quoted as a marker only")
	}
}

func TestSteerRowCountsAsAUserOriginatedTurn(t *testing.T) {
	row := SteerUserRow("focus on the error handling")
	if !isRealUserMessage(row) || !IsUserOriginatedTurn(row) || !state.IsActionableUserTurn(row) {
		t.Fatal("steer row is not human input")
	}
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateSession(state.NewSession{ID: "s1", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	for _, fields := range []map[string]any{{"role": "user", "content": "first question"}, {"role": "assistant", "content": "first answer"},
		{"role": "user", "content": row["content"], "display_kind": row["display_kind"]}} {
		if _, err := db.AppendMessage(ctx, "s1", fields, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	recents, err := db.ListRecentUserMessages(ctx, "s1", 5, false)
	if err != nil {
		t.Fatal(err)
	}
	var previews []string
	for _, r := range recents {
		p, _ := r.Get("preview")
		previews = append(previews, string([]rune(p.(string))[:5]))
	}
	if len(previews) != 2 || previews[0] != string([]rune(row["content"].(string))[:5]) || previews[1] != "first" {
		t.Fatalf("previews %v", previews)
	}
}

// Standalone handoff with a live ask appended after the end marker
func compositeHandoff(ask string) map[string]any {
	h := standaloneHandoff()
	h["content"] = h["content"].(string) + "\n\n" + ask
	return h
}

func TestUserOriginatedTurnPredicate(t *testing.T) {
	handoff := standaloneHandoff()
	if !state.IsCompactionSummaryMessage(handoff) || IsUserOriginatedTurn(handoff) {
		t.Fatal("standalone handoff not user originated")
	}
	if !IsUserOriginatedTurn(map[string]any{"role": "user", "content": "hello"}) {
		t.Fatal("plain user")
	}
	carrier := compositeHandoff("REAL ASK")
	split, live := state.SplitUserOriginatedTurn(carrier)
	if !IsUserOriginatedTurn(carrier) || UserOriginatedTurnView(carrier)["content"] != "REAL ASK" || live["content"] != "REAL ASK" ||
		split["display_kind"] != "hidden" || strings.Contains(split["content"].(string), "REAL ASK") {
		t.Fatalf("force user leading carrier %v %v", split, live)
	}
	hidden, typed := compositeHandoff("REAL ASK"), compositeHandoff("REAL ASK")
	hidden["display_kind"], typed["display_kind"] = "hidden", "auto_continue"
	if UserOriginatedTurnView(hidden)["content"] != "REAL ASK" || UserOriginatedTurnView(typed) != nil {
		t.Fatal("hidden legacy carrier projects, typed carrier does not")
	}
	messages := []map[string]any{{"role": "assistant", "content": "prefix"}, compositeHandoff("REAL ASK"), {"role": "assistant", "content": "failed"}}
	prefix, rewound, err := HistoryBeforeUserOriginatedTurn(messages, 1)
	if err != nil || rewound["content"] != "REAL ASK" || !sameRows(prefix[:1], messages[:1]) || prefix[1]["display_kind"] != "hidden" ||
		strings.Contains(prefix[1]["content"].(string), "REAL ASK") || !strings.HasSuffix(messages[1]["content"].(string), "REAL ASK") {
		t.Fatalf("rewind prefix %v %v %v", prefix, rewound, err)
	}
	carrier = compositeHandoff("REAL ASK")
	carrier["display_metadata"] = map[string]any{"reactions": []any{map[string]any{"emoji": "\U0001F44D", "author": "user"}}, "synthetic_only": "do not project"}
	if got := fmt.Sprint(UserOriginatedTurnView(carrier)["display_metadata"]); got != fmt.Sprint(map[string]any{"reactions": []any{map[string]any{"emoji": "\U0001F44D", "author": "user"}}}) {
		t.Fatalf("display metadata %s", got)
	}
	if IsUserOriginatedTurn(map[string]any{"role": "user", "content": "opaque", "display_kind": "hidden"}) {
		t.Fatal("hidden row originated")
	}
}

func TestReanchorSkipsHandoffFallback(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []map[string]any
		ask  string
		want int
	}{
		{"test_fallback_skips_standalone_handoff", []map[string]any{{"role": "system", "content": "sys"}, standaloneHandoff()}, "missing ask", -1},
		{"test_exact_match_still_wins", []map[string]any{standaloneHandoff(), {"role": "user", "content": "live ask"}}, "live ask", 1},
		{"test_fallback_prefers_real_user_over_handoff", []map[string]any{{"role": "user", "content": "original ask"}, standaloneHandoff()}, "rewritten ask", 0},
		{"test_exact_live_projection_reanchors_to_composite_carrier", []map[string]any{{"role": "user", "content": "older ask"}, compositeHandoff("rewritten ask")}, "rewritten ask", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reanchorCurrentTurnUserIdx(rowsToMessages(tc.rows), tc.ask); got != tc.want {
				t.Fatalf("%q got %d want %d", tc.ask, got, tc.want)
			}
		})
	}
}
