package provider

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const turnAuthorFamily = "\U0001F468‍\U0001F469‍\U0001F467"

func TestParseTurnAuthorRejectsJunkAndAuthorsWithoutIDOrName(t *testing.T) {
	for _, raw := range []any{nil, 42, []any{}, []any{"bot:alpha"}, "not json", `"a string"`, "[1, 2]", []byte{0xff},
		map[string]any{}, map[string]any{"is_bot": true}, map[string]any{"id": "", "name": "   "}} {
		if got := ParseTurnAuthor(raw); got != nil {
			t.Errorf("%#v parsed to %v", raw, got)
		}
	}
}

func TestTurnAuthorEnvRoundTripAndTake(t *testing.T) {
	author := pyjson.NewObject()
	author.Set("id", "bot:alpha")
	author.Set("name", "Alpha")
	author.Set("is_bot", true)
	env, err := TurnAuthorEnv(author)
	if err != nil || len(env) != 1 || env[TurnAuthorEnvName] != `{"id":"bot:alpha","name":"Alpha","is_bot":true}` {
		t.Fatalf("env %v %v", env, err)
	}
	if got := TurnAuthorFromEnv(env); !pyrepr.Equal(got, author) {
		t.Fatalf("round trip %v", got)
	}
	env["OTHER"] = "kept"
	if got := TakeTurnAuthorFromEnv(env); !pyrepr.Equal(got, author) || len(env) != 1 || env["OTHER"] != "kept" {
		t.Fatalf("take %v %v", got, env)
	}
	if TakeTurnAuthorFromEnv(env) != nil {
		t.Fatal("second take")
	}
	t.Setenv(TurnAuthorEnvName, `{"id": "u1"}`)
	if got := TakeTurnAuthorFromEnv(nil); got == nil || TurnAuthorFromEnv(nil) != nil {
		t.Fatal("process environment take removes the variable")
	}
}

func TestTurnAuthorCleansFieldsAndQualifiesOrigins(t *testing.T) {
	got := ParseTurnAuthor(map[string]any{"id": "bot:alpha", "name": " \x07" + turnAuthorFamily + " Fam ", "is_bot": "YES ", "origin": "desk-1"})
	if id, _ := got.Get("id"); id != "bot:desk-1/alpha" {
		t.Fatalf("origin id %v", id)
	}
	if name, _ := got.Get("name"); name != turnAuthorFamily+" Fam" {
		t.Fatalf("cleaned name %q", name)
	}
	if A2AKey(got) != "a2a:bot:desk-1/alpha" || A2AKey(ParseTurnAuthor(`{"id": "u1", "is_bot": 0}`)) != "" || A2AKey(nil) != "" {
		t.Fatal("a2a key")
	}
	long := ParseTurnAuthor(map[string]any{"name": string(make([]rune, 0)) + repeatRune('n', 250)})
	if name, _ := long.Get("name"); len([]rune(name.(string))) != 200 {
		t.Fatal("field cap")
	}
	if id, _ := ParseTurnAuthor(`{"id": "bot:a/b", "origin": "x"}`).Get("id"); id != "bot:a/b" {
		t.Fatal("qualified id keeps its origin")
	}
	if BotAuthorID("p", "  ") != "bot:p" || LocalOrigin() == "" {
		t.Fatal("bot id and local origin")
	}
}

func repeatRune(r rune, n int) string {
	out := make([]rune, n)
	for i := range out {
		out[i] = r
	}
	return string(out)
}
