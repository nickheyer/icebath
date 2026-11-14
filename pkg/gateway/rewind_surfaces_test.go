package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	_ "modernc.org/sqlite"
)

var rewindSurfaces = []string{"cli", "gateway", "tui"}

type rewindRow struct {
	id      int64
	role    string
	content sql.NullString
	active  int64
}

// Seeds text turns, or image bearing asks answered through a tool call and result when rich
func seedRewind(ctx context.Context, t *testing.T, db *state.SessionDB, sid string, turns int, rich bool) {
	t.Helper()
	if err := db.CreateSession(state.NewSession{ID: sid, Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	add := func(fields map[string]any) {
		if _, err := db.AppendMessage(ctx, sid, fields, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= turns; i++ {
		if !rich {
			add(map[string]any{"role": "user", "content": fmt.Sprintf("q%d", i)})
			add(map[string]any{"role": "assistant", "content": fmt.Sprintf("a%d", i)})
			continue
		}
		image := map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,QUJD"}}
		add(map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("q%d", i)}, image}})
		call := fmt.Sprintf("call-%d", i)
		add(map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": call, "type": "function",
			"function": map[string]any{"name": "terminal", "arguments": "{}"}}}})
		add(map[string]any{"role": "tool", "content": fmt.Sprintf("out%d", i), "tool_call_id": call, "tool_name": "terminal"})
		add(map[string]any{"role": "assistant", "content": fmt.Sprintf("a%d", i)})
	}
}

// Every message row of a session in id order, read straight from the store file
func rewindRows(t *testing.T, sid string) []rewindRow {
	t.Helper()
	conn, err := sql.Open("sqlite", filepath.Join(os.Getenv("ICEBATH_HOME"), "state.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rows, err := conn.Query("SELECT id, role, content, active FROM messages WHERE session_id = ? ORDER BY id", sid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []rewindRow
	for rows.Next() {
		var r rewindRow
		if err := rows.Scan(&r.id, &r.role, &r.content, &r.active); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// Backs up n user turns through one surface, the gateway store or the SessionDB call the CLI and TUI make
func rewindVia(ctx context.Context, t *testing.T, surface string, db *state.SessionDB, sid string, n int) (any, error) {
	t.Helper()
	if surface == "gateway" {
		store := NewSessionStore(ctx, t.TempDir(), DefaultGatewayConfig(ctx), nil)
		store.PinDB(db)
		out, err := store.RewindSession(ctx, sid, n, false)
		if out == nil {
			return nil, err
		}
		return out, err
	}
	warm, _, err := db.GetResumeConversations(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	users := 0
	for _, m := range warm {
		if m["role"] == "user" {
			users++
		}
	}
	o := state.RewindUserTurnOptions{WarmHistory: warm, HasWarmHistory: true, AdoptRowIDs: surface == "tui"}
	ordinal := users - n
	if surface == "tui" && (ordinal < 0 || ordinal >= users) {
		return nil, &state.RewindTargetUnavailableError{Msg: "target user message is no longer in session history"}
	}
	if surface == "cli" {
		ordinal = -n
	}
	out, err := db.RewindUserTurn(ctx, sid, ordinal, o)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func TestEverySurfacePersistsTheSameActiveSet(t *testing.T) {
	for _, n := range []int{1, 2} {
		for _, rich := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d-%v", n, rich), func(t *testing.T) {
				ctx, db := newTestStateDB(t)
				var expected []rewindRow
				for _, surface := range rewindSurfaces {
					sid := fmt.Sprintf("rewind-%s-%d-%v", surface, n, rich)
					seedRewind(ctx, t, db, sid, 3, rich)
					if out, err := rewindVia(ctx, t, surface, db, sid, n); err != nil || out == nil {
						t.Fatalf("%s: %v %v", surface, out, err)
					}
					rows := rewindRows(t, sid)
					if expected == nil {
						expected = rows
					}
					if len(rows) != len(expected) {
						t.Fatalf("%s rows %v", surface, rows)
					}
					for i := range rows {
						if rows[i].role != expected[i].role || rows[i].content != expected[i].content || rows[i].active != expected[i].active {
							t.Fatalf("%s row %d %v != %v", surface, i, rows[i], expected[i])
						}
					}
				}
				var active []rewindRow
				for _, r := range expected {
					if r.active != 0 {
						active = append(active, r)
					}
				}
				if !rich {
					var want []string
					for i := 1; i < 4-n; i++ {
						want = append(want, fmt.Sprintf("q%d", i), fmt.Sprintf("a%d", i))
					}
					if len(active) != len(want) {
						t.Fatalf("active %v", active)
					}
					for i, r := range active {
						if r.content.String != want[i] {
							t.Fatalf("active %v", active)
						}
					}
					return
				}
				if len(active) != 4*(3-n) {
					t.Fatalf("active %v", active)
				}
				var storedUsers []sql.NullString
				for _, r := range expected {
					if r.role == "user" {
						storedUsers = append(storedUsers, r.content)
					}
				}
				userIndex := 0
				for i, r := range active {
					if r.role != []string{"user", "assistant", "tool", "assistant"}[i%4] {
						t.Fatalf("active roles %v", active)
					}
					if r.role == "user" {
						if r.content != storedUsers[userIndex] || !strings.Contains(r.content.String, "QUJD") {
							t.Fatalf("user row %v", r)
						}
						userIndex++
					}
				}
			})
		}
	}
}

func TestCliUndoLeavesTheWarmHistoryShapeAloneWhileTheTuiAdoptsRowIds(t *testing.T) {
	ctx, db := newTestStateDB(t)
	for _, sid := range []string{"shape-cli", "shape-tui"} {
		seedRewind(ctx, t, db, sid, 3, false)
	}
	warm, err := db.GetMessagesAsConversation(ctx, "shape-cli", state.ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range warm {
		if _, has := m["_row_id"]; has {
			t.Fatal("warm history carries row ids")
		}
	}
	out, err := db.RewindUserTurn(ctx, "shape-cli", 2, state.RewindUserTurnOptions{WarmHistory: warm, HasWarmHistory: true})
	if err != nil || len(out.Prefix) != 4 {
		t.Fatalf("%v %v", out, err)
	}
	for _, m := range out.Prefix {
		if _, has := m["_row_id"]; has {
			t.Fatal("the cli history grew row ids")
		}
	}
	installed, err := rewindVia(ctx, t, "tui", db, "shape-tui", 1)
	if err != nil {
		t.Fatal(err)
	}
	var want []int64
	for _, r := range rewindRows(t, "shape-tui") {
		if r.active != 0 {
			want = append(want, r.id)
		}
	}
	prefix := installed.(*state.RewindOutcome).Prefix
	if len(prefix) != len(want) {
		t.Fatalf("prefix %v want %v", prefix, want)
	}
	for i, m := range prefix {
		if m["_row_id"] != want[i] {
			t.Fatalf("prefix %v want %v", prefix, want)
		}
	}
}

func TestOutOfRangeTargetChangesNothingOnEverySurface(t *testing.T) {
	for _, surface := range rewindSurfaces {
		t.Run(surface, func(t *testing.T) {
			ctx, db := newTestStateDB(t)
			sid := "oob-" + surface
			seedRewind(ctx, t, db, sid, 1, false)
			before := rewindRows(t, sid)
			if surface == "tui" {
				_, err := rewindVia(ctx, t, surface, db, sid, 2)
				var unavailable *state.RewindTargetUnavailableError
				if !errors.As(err, &unavailable) {
					t.Fatalf("want a ValueError class refusal, got %v", err)
				}
			} else {
				if err := db.CreateSession(state.NewSession{ID: sid + "-empty", Source: "cli"}); err != nil {
					t.Fatal(err)
				}
				if surface == "gateway" {
					if out, err := rewindVia(ctx, t, surface, db, sid+"-empty", 1); out != nil || err != nil {
						t.Fatalf("%v %v", out, err)
					}
				} else {
					_, err := db.RewindUserTurn(ctx, sid+"-empty", -1, state.RewindUserTurnOptions{WarmHistory: []map[string]any{}, HasWarmHistory: true})
					var unavailable *state.RewindTargetUnavailableError
					if !errors.As(err, &unavailable) {
						t.Fatalf("want RewindTargetUnavailableError, got %v", err)
					}
				}
				if rows := rewindRows(t, sid+"-empty"); len(rows) != 0 {
					t.Fatalf("rows %v", rows)
				}
			}
			after := rewindRows(t, sid)
			if fmt.Sprint(after) != fmt.Sprint(before) {
				t.Fatalf("%v != %v", after, before)
			}
		})
	}
}

func TestUndoAfterAnUnansweredTurnRewindsTheMergedTurnOnEverySurface(t *testing.T) {
	for _, surface := range rewindSurfaces {
		t.Run(surface, func(t *testing.T) {
			ctx, db := newTestStateDB(t)
			sid := "wedged-" + surface
			if err := db.CreateSession(state.NewSession{ID: sid, Source: "cli"}); err != nil {
				t.Fatal(err)
			}
			for _, rc := range [][2]string{{"user", "q1"}, {"assistant", "a1"}, {"user", "q2_failed"}, {"user", "q3"}, {"assistant", "a3"}} {
				if _, err := db.AppendMessage(ctx, sid, map[string]any{"role": rc[0], "content": rc[1]}, state.AppendGuards{}); err != nil {
					t.Fatal(err)
				}
			}
			warm, _, err := db.GetResumeConversations(ctx, sid)
			if err != nil {
				t.Fatal(err)
			}
			users := 0
			for _, m := range warm {
				if m["role"] == "user" {
					users++
				}
			}
			if users != 2 {
				t.Fatalf("users %d", users)
			}
			activeContents := func() []string {
				var out []string
				for _, r := range rewindRows(t, sid) {
					if r.active != 0 {
						out = append(out, r.content.String)
					}
				}
				return out
			}
			if out, err := rewindVia(ctx, t, surface, db, sid, 1); out == nil || err != nil {
				t.Fatalf("%v %v", out, err)
			}
			if got := activeContents(); fmt.Sprint(got) != "[q1 a1]" {
				t.Fatal(got)
			}
			if out, err := rewindVia(ctx, t, surface, db, sid, 1); out == nil || err != nil {
				t.Fatalf("%v %v", out, err)
			}
			if got := activeContents(); len(got) != 0 {
				t.Fatal(got)
			}
		})
	}
}
