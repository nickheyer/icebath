package messagetools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Reaction store whose writes fail, counting releases like the MagicMock close assertion
type failingReactionStore struct{ released int }

func (f *failingReactionStore) LatestMessageRowID(ctx context.Context, sessionID, role string, offset int, requireText bool) (int64, bool, error) {
	return 42, true, nil
}

func (f *failingReactionStore) GetMessageRole(ctx context.Context, sessionID string, rowID int64) (string, bool, error) {
	return "user", true, nil
}

func (f *failingReactionStore) SetMessageReaction(ctx context.Context, sessionID string, rowID int64, emoji *string, author string) ([]any, bool, error) {
	return nil, false, errors.New("write failed")
}

func TestReactionDatabaseClosesWhenWriteFails(t *testing.T) {
	store := &failingReactionStore{}
	prev := openReactionSessionDB
	openReactionSessionDB = func(ctx context.Context, env *tools.Env) (reactionStore, func()) {
		return store, func() { store.released++ }
	}
	t.Cleanup(func() { openReactionSessionDB = prev })
	t.Setenv("ICEBATH_SESSION_ID", "session-1")
	result := reactToMessageTool(context.Background(), &tools.Env{}, "👍", nil, nil)
	if !strings.Contains(result, "write failed") || store.released != 1 {
		t.Fatalf("result %s released %d", result, store.released)
	}
}

// Temp profile whose state database holds the given sessions and their user messages
func reactionTestHome(t *testing.T, sessions map[string][]string, order []string) (*tools.Env, map[string][]int64) {
	t.Helper()
	dir := t.TempDir()
	db, err := state.Acquire(context.Background(), home.StatePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer state.ReleaseOrClose(db)
	rows := map[string][]int64{}
	for _, id := range order {
		if err := db.CreateSession(state.NewSession{ID: id, Source: "local"}); err != nil {
			t.Fatal(err)
		}
		for _, text := range sessions[id] {
			if _, err := db.AppendMessage(context.Background(), id, map[string]any{"role": "user", "content": text}, state.AppendGuards{}); err != nil {
				t.Fatal(err)
			}
			row, ok, err := db.LatestMessageRowID(context.Background(), id, "user", 0, true)
			if err != nil || !ok {
				t.Fatalf("latest row %v %v", ok, err)
			}
			rows[id] = append(rows[id], row)
		}
	}
	return &tools.Env{Home: dir, Values: &sync.Map{}}, rows
}

type emittedReaction struct {
	event   string
	payload map[string]any
}

func captureReactions(t *testing.T) *[]emittedReaction {
	t.Helper()
	var got []emittedReaction
	tools.SetDesktopEmitter(func(sid, event string, payload map[string]any) error {
		got = append(got, emittedReaction{event, payload})
		return nil
	})
	t.Cleanup(func() { tools.SetDesktopEmitter(nil) })
	return &got
}

func dispatchReact(t *testing.T, env *tools.Env, args map[string]any) map[string]any {
	t.Helper()
	raw := reactToMessageDef.Handler(context.Background(), &tools.Call{Name: "react_to_message", Args: args, Env: env})
	v, err := pyjson.Loads(raw)
	if err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return v.(*pyjson.Object).Vals
}

func reactionEmojis(t *testing.T, env *tools.Env, sessionID string, row int64) string {
	t.Helper()
	db, err := state.Acquire(context.Background(), home.StatePath(env.Home))
	if err != nil {
		t.Fatal(err)
	}
	defer state.ReleaseOrClose(db)
	got, err := db.GetMessageReactions(context.Background(), sessionID, row)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := state.DumpsUnicode(got)
	return text
}

func TestReactionUsesDurableSessionWithRoutingKey(t *testing.T) {
	for _, routingKey := range []string{"agent:main:local:dm:chat-a", "chat-b"} {
		t.Run(routingKey, func(t *testing.T) {
			env, rows := reactionTestHome(t, map[string][]string{
				"chat-a": {"Earlier message", "Latest message"},
				"chat-b": {"Other conversation"},
			}, []string{"chat-a", "chat-b"})
			earlier, latest, foreign := rows["chat-a"][0], rows["chat-a"][1], rows["chat-b"][0]
			emitted := captureReactions(t)
			tools.SetSessionVars(env, tools.SessionVars{SessionKey: routingKey, SessionID: "chat-a"})
			result := dispatchReact(t, env, map[string]any{"emoji": "👍"})
			if result["success"] != true || result["row_id"] != latest {
				t.Fatalf("result %v", result)
			}
			if got := dispatchReact(t, env, map[string]any{"emoji": "❤️", "messages_back": int64(1)}); got["row_id"] != earlier {
				t.Fatalf("messages_back %v", got)
			}
			if got := dispatchReact(t, env, map[string]any{"emoji": "😂", "message_row_id": earlier}); got["success"] != true {
				t.Fatalf("explicit row %v", got)
			}
			if got := dispatchReact(t, env, map[string]any{"emoji": "👍", "message_row_id": foreign}); got["error"] == nil {
				t.Fatalf("foreign row %v", got)
			}
			if got := dispatchReact(t, env, map[string]any{"emoji": "👍", "messages_back": int64(99)}); got["error"] == nil {
				t.Fatalf("too far back %v", got)
			}
			if got := dispatchReact(t, env, map[string]any{"emoji": "", "message_row_id": earlier}); got["success"] != true {
				t.Fatalf("retract %v", got)
			}
			tools.ClearSessionVars(env)
			if got := reactionEmojis(t, env, "chat-a", latest); !strings.Contains(got, `"emoji": "👍"`) {
				t.Fatalf("latest reactions %s", got)
			}
			if got := reactionEmojis(t, env, "chat-a", earlier); got != "[]" {
				t.Fatalf("earlier reactions %s", got)
			}
			if got := reactionEmojis(t, env, "chat-b", foreign); got != "[]" {
				t.Fatalf("foreign reactions %s", got)
			}
			want := []int64{latest, earlier, earlier, earlier}
			if len(*emitted) != len(want) {
				t.Fatalf("emitted %v", *emitted)
			}
			for i, e := range *emitted {
				if e.event != "message.reaction" || e.payload["row_id"] != want[i] {
					t.Fatalf("emitted[%d] %v", i, e)
				}
			}
			first, _ := state.DumpsUnicode((*emitted)[0].payload["reactions"])
			last, _ := state.DumpsUnicode((*emitted)[3].payload["reactions"])
			if !strings.Contains(first, `"emoji": "👍"`) || last != "[]" {
				t.Fatalf("first %s last %s", first, last)
			}
		})
	}
}

func TestReactionKeepsLegacyKeyFallbackAndEmptySession(t *testing.T) {
	env, rows := reactionTestHome(t, map[string][]string{"legacy": {"Legacy message"}}, []string{"legacy"})
	emitted := captureReactions(t)
	tools.SetSessionVars(env, tools.SessionVars{SessionKey: "legacy", SessionID: "missing"})
	result := dispatchReact(t, env, map[string]any{"emoji": "👍"})
	if msg, _ := result["error"].(string); !strings.Contains(msg, "No user message") || len(*emitted) != 0 {
		t.Fatalf("result %v emitted %v", result, *emitted)
	}
	tools.ClearSessionVars(env)
	if got := reactionEmojis(t, env, "legacy", rows["legacy"][0]); got != "[]" {
		t.Fatalf("legacy reactions %s", got)
	}
	tools.SetSessionVars(env, tools.SessionVars{SessionKey: "legacy", SessionID: ""})
	if got := dispatchReact(t, env, map[string]any{"emoji": "👍"}); got["success"] != true {
		t.Fatalf("legacy key %v", got)
	}
	tools.ClearSessionVars(env)
	t.Setenv("ICEBATH_SESSION_ID", "")
	t.Setenv("ICEBATH_SESSION_KEY", "")
	if msg, _ := dispatchReact(t, env, map[string]any{"emoji": "👍"})["error"].(string); !strings.Contains(msg, "No active session") {
		t.Fatalf("no session %q", msg)
	}
}

func TestSwitchingMessageReactionsOffWithdrawsTheTool(t *testing.T) {
	dir := t.TempDir()
	env := &tools.Env{Home: dir}
	if checkReactRequirements(env) {
		t.Fatal("opt in switch must default off")
	}
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("display:\n"+body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("  message_reactions: true\n")
	if !checkReactRequirements(env) {
		t.Fatal("explicit true ignored")
	}
	write("  message_reactions: false\n")
	if checkReactRequirements(env) {
		t.Fatal("explicit false ignored")
	}
}
