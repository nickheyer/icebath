package agent

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var rebuildPNG, _ = hex.DecodeString("89504e470d0a1a0a0000000d49484452000000010000000108020000009077" +
	"53de0000000c4944415408d763f8cfc0f00f00050201ff2ecf3ca10000000049454e44ae426082")

// Private home and context for one image replay case
func replayHome(t *testing.T) (context.Context, *tools.Env, string) {
	t.Helper()
	h := caseHome(t)
	env := &tools.Env{Home: h}
	return tools.WithToolEnv(context.Background(), env), env, t.TempDir()
}

// Writes the home config with the image input mode and the vision section
func replayConfig(t *testing.T, env *tools.Env, mode string, vision map[string]any) {
	t.Helper()
	if vision == nil {
		vision = map[string]any{}
	}
	raw, _ := json.Marshal(map[string]any{"agent": map[string]any{"image_input_mode": mode}, "vision": vision})
	if err := os.WriteFile(filepath.Join(env.Home, "config.yaml"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Native user turn the way the ingress builds it, bytes retained under native inputs first
func replayLiveTurn(t *testing.T, ctx context.Context, env *tools.Env, tmp, name, text string, data []byte) (map[string]any, string) {
	t.Helper()
	staged := filepath.Join(tmp, name)
	if err := os.WriteFile(staged, data, 0o600); err != nil {
		t.Fatal(err)
	}
	refs, err := gateway.CaptureNativeMedia(ctx, []string{staged})
	if err != nil || len(refs) == 0 {
		t.Fatalf("capture %v %v", refs, err)
	}
	path, _ := refs[0].(*pyjson.Object).Get("path")
	parts, skipped, err := tools.BuildNativeContentParts(env, text, []string{path.(string)}, nil)
	if err != nil || len(skipped) > 0 {
		t.Fatalf("parts skipped %v %v", skipped, err)
	}
	return map[string]any{"role": "user", "content": parts}, path.(string)
}

// Stand in agent of a custom vision route at a fixed turn timestamp
func replayAgent(env *tools.Env) *AIAgent {
	return &AIAgent{apiMode: "chat_completions", provider: "custom", model: "fake-vision", currentTurnTimestamp: 10_000.0, env: env}
}

// Session store with session s of the api server source
func replayDB(t *testing.T, ctx context.Context, tmp string) *state.SessionDB {
	t.Helper()
	db, err := state.NewSessionDB(ctx, filepath.Join(tmp, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateSession(state.NewSession{ID: "s", Source: "api_server"}); err != nil {
		t.Fatal(err)
	}
	return db
}

func replayAppend(t *testing.T, ctx context.Context, db *state.SessionDB, role string, content any, ts float64) {
	t.Helper()
	if _, err := db.AppendMessage(ctx, "s", map[string]any{"role": role, "content": content, "timestamp": ts}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
}

// User wire contents of one request built over the reloaded transcript plus a follow up ask
func replayUserRows(t *testing.T, ctx context.Context, a *AIAgent, db *state.SessionDB, followUp string) []any {
	t.Helper()
	history, err := db.GetMessagesAsConversation(ctx, "s", state.ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	history = append(history, map[string]any{"role": "user", "content": followUp})
	request, _ := buildAPIMessages(ctx, a, rowsToMessages(history), len(history)-1, "", "", nil, "")
	var users []any
	for _, m := range request {
		if m["role"] == "user" {
			users = append(users, m["content"])
		}
	}
	return users
}

// Persists the live turns through the durable projection and rebuilds the follow up request
func replayRebuilt(t *testing.T, ctx context.Context, env *tools.Env, tmp string, live []map[string]any) []any {
	t.Helper()
	db := replayDB(t, ctx, tmp)
	for i, turn := range live {
		replayAppend(t, ctx, db, "user", state.DurableContent(turn["content"]), 1000.0+2*float64(i))
		replayAppend(t, ctx, db, "assistant", fmt.Sprintf("reply %d", i), 1001.0+2*float64(i))
	}
	return replayUserRows(t, ctx, replayAgent(env), db, "and the expiry date?")
}

func replaySame(t *testing.T, got, want any) bool {
	t.Helper()
	return reflect.DeepEqual(jsonParts(t, got), jsonParts(t, want))
}

func screenshotText(v any) bool {
	s, ok := v.(string)
	return ok && strings.HasSuffix(s, "\n[screenshot]")
}

func TestImageReplay(t *testing.T) {
	caseHome(t)
	t.Run("test_replay_window_is_resolved_once_per_turn", func(t *testing.T) {
		ctx, env, tmp := replayHome(t)
		replayConfig(t, env, "native", nil)
		live, _ := replayLiveTurn(t, ctx, env, tmp, "label.png", "What is on this label?", rebuildPNG)
		db := replayDB(t, ctx, tmp)
		replayAppend(t, ctx, db, "user", state.DurableContent(live["content"]), 1000.0)
		replayAppend(t, ctx, db, "assistant", "reply", 1001.0)
		a := replayAgent(env)
		first := func() any { return replayUserRows(t, ctx, a, db, "and the expiry date?")[0] }
		if !replaySame(t, first(), live["content"]) {
			t.Fatal("first request did not replay the image")
		}
		replayConfig(t, env, "text", nil)
		if !replaySame(t, first(), live["content"]) {
			t.Fatal("same turn changed its wire bytes")
		}
		a.currentTurnTimestamp += 60
		if _, ok := first().(string); !ok {
			t.Fatal("next turn kept replaying under text mode")
		}
	})
	t.Run("test_rebuilt_turn_resends_the_image_byte_identical_only_for_native_input", func(t *testing.T) {
		for _, tc := range []struct {
			mode     string
			replayed bool
		}{{"native", true}, {"text", false}} {
			t.Run(tc.mode, func(t *testing.T) {
				ctx, env, tmp := replayHome(t)
				replayConfig(t, env, tc.mode, nil)
				live, _ := replayLiveTurn(t, ctx, env, tmp, "label.png", "What is on this label?", rebuildPNG)
				users := replayRebuilt(t, ctx, env, tmp, []map[string]any{live})
				if users[len(users)-1] != "and the expiry date?" {
					t.Fatalf("follow up %v", users[len(users)-1])
				}
				if tc.replayed && !replaySame(t, users[0], live["content"]) || !tc.replayed && !screenshotText(users[0]) {
					t.Fatalf("first row %#v", users[0])
				}
			})
		}
	})
	t.Run("test_replay_window_skips_untrusted_bytes_and_typed_markers", func(t *testing.T) {
		ctx, env, tmp := replayHome(t)
		replayConfig(t, env, "native", map[string]any{"replay_recent_images": 2})
		oldest, _ := replayLiveTurn(t, ctx, env, tmp, "oldest.png", "first photo", rebuildPNG)
		older, _ := replayLiveTurn(t, ctx, env, tmp, "older.png", "second photo", rebuildPNG)
		typed := map[string]any{"role": "user", "content": "I literally typed\n[screenshot]"}
		changed, changedPath := replayLiveTurn(t, ctx, env, tmp, "changed.png", "third photo", append(append([]byte{}, rebuildPNG...), 0))
		newest, _ := replayLiveTurn(t, ctx, env, tmp, "new.png", "fourth photo", rebuildPNG)
		_ = os.Chmod(changedPath, 0o600)
		if err := os.WriteFile(changedPath, []byte("tampered"), 0o600); err != nil {
			t.Fatal(err)
		}
		users := replayRebuilt(t, ctx, env, tmp, []map[string]any{oldest, older, typed, changed, newest})
		if !replaySame(t, users[4], newest["content"]) || !screenshotText(users[3]) || users[2] != "I literally typed\n[screenshot]" ||
			!screenshotText(users[1]) || !screenshotText(users[0]) {
			t.Fatalf("users %#v", users)
		}
	})
	t.Run("test_replay_keeps_part_order_and_appended_turn_context", func(t *testing.T) {
		ctx, env, tmp := replayHome(t)
		replayConfig(t, env, "native", nil)
		live, _ := replayLiveTurn(t, ctx, env, tmp, "label.png", "What is on this label?", rebuildPNG)
		msg := state.MessageFromDict(live)
		appendMultimodalContext(ctx, replayAgent(env), msg, "recalled memory", "plugin hook context", false)
		live["content"] = msg.Content
		parts := jsonParts(t, msg.Content).([]any)
		if parts[len(parts)-1].(map[string]any)["type"] != "text" {
			t.Fatalf("last part %v", parts[len(parts)-1])
		}
		rebuilt := jsonParts(t, replayRebuilt(t, ctx, env, tmp, []map[string]any{live})[0]).([]any)
		if !reflect.DeepEqual(rebuilt[:2], parts[:2]) {
			t.Fatalf("rebuilt head %v", rebuilt[:2])
		}
		third := rebuilt[2].(map[string]any)
		if text, _ := third["text"].(string); third["type"] != "text" || !strings.HasSuffix(text, "plugin hook context") {
			t.Fatalf("third part %v", third)
		}
	})
	t.Run("test_api_server_image_replays_with_its_position_and_detail", func(t *testing.T) {
		for id, parts := range map[string][]any{
			"image-first": {map[string]any{"type": "image_url", "image_url": map[string]any{"url": "IMG"}}, map[string]any{"type": "text", "text": "what is this?"}},
			"detail-low":  {map[string]any{"type": "text", "text": "what is this?"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "IMG", "detail": "low"}}},
		} {
			t.Run(id, func(t *testing.T) {
				ctx, env, tmp := replayHome(t)
				replayConfig(t, env, "native", nil)
				url := "data:image/png;base64," + base64.StdEncoding.EncodeToString(rebuildPNG)
				wire := make([]any, len(parts))
				for i, p := range parts {
					part := p.(map[string]any)
					if part["type"] != "image_url" {
						wire[i] = pyjson.ObjectOf("type", part["type"], "text", part["text"])
						continue
					}
					image := pyjson.ObjectOf("url", url)
					if detail, ok := part["image_url"].(map[string]any)["detail"]; ok {
						image.Set("detail", detail)
					}
					wire[i] = pyjson.ObjectOf("type", "image_url", "image_url", image)
				}
				parts = wire
				media, err := gateway.CommitAPIImages(ctx, parts)
				if err != nil {
					t.Fatal(err)
				}
				content, err := gateway.RestoreAPIImages(ctx, parts, media)
				if err != nil {
					t.Fatal(err)
				}
				live := map[string]any{"role": "user", "content": jsonParts(t, content)}
				if got := replayRebuilt(t, ctx, env, tmp, []map[string]any{live})[0]; !replaySame(t, got, content) {
					t.Fatalf("rebuilt %#v", got)
				}
			})
		}
	})
	t.Run("test_growing_session_drops_replayed_images_in_sticky_batches", func(t *testing.T) {
		ctx, env, tmp := replayHome(t)
		replayConfig(t, env, "native", map[string]any{"replay_recent_images": 3})
		db := replayDB(t, ctx, tmp)
		var previous []any
		dropped, rewrites := map[int]bool{}, 0
		for n := range 10 {
			live, _ := replayLiveTurn(t, ctx, env, tmp, fmt.Sprintf("p%d.png", n), fmt.Sprintf("photo %d", n), append(append([]byte{}, rebuildPNG...), byte(n)))
			replayAppend(t, ctx, db, "user", state.DurableContent(live["content"]), 1000.0+2*float64(n))
			replayAppend(t, ctx, db, "assistant", fmt.Sprintf("reply %d", n), 1001.0+2*float64(n))
			users := replayUserRows(t, ctx, replayAgent(env), db, "and now?")
			rows := users[:len(users)-1]
			for i := range min(len(previous), len(rows)) {
				if !replaySame(t, previous[i], rows[i]) {
					rewrites++
					break
				}
			}
			replayed := map[int]bool{}
			for i, row := range rows {
				if _, ok := row.([]any); ok {
					replayed[i] = true
				} else if _, ok := row.([]map[string]any); ok {
					replayed[i] = true
				}
			}
			for i := range replayed {
				if dropped[i] {
					t.Fatalf("turn %d: dropped image %d returned", n, i)
				}
			}
			for i := range rows {
				if !replayed[i] {
					dropped[i] = true
				}
			}
			if len(replayed) < min(3, len(rows)) {
				t.Fatalf("turn %d: replayed %v of %d", n, replayed, len(rows))
			}
			previous = rows
		}
		if rewrites > 4 {
			t.Fatalf("rewrites %d", rewrites)
		}
	})
}
