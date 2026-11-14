package tuiserver

import (
	"context"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Agent the onboarding session helpers read, its tools, toolsets, chat and store
type onbPreludeAgent struct {
	valid    map[string]bool
	toolsets []string
	sid      string
	db       *state.DB
}

func (a *onbPreludeAgent) ValidToolNames() map[string]bool { return a.valid }
func (a *onbPreludeAgent) EnabledToolsets() []string       { return a.toolsets }
func (a *onbPreludeAgent) SessionID() string               { return a.sid }
func (a *onbPreludeAgent) OnboardingStore() tools.OnboardingStore {
	if a.db == nil {
		return nil
	}
	return a.db
}

// Agent of the onboarding chat built with setup_beat and terminal
func onbChatAgent(db *state.DB, sid string) *onbPreludeAgent {
	return &onbPreludeAgent{valid: map[string]bool{"setup_beat": true, "terminal": true}, sid: sid, db: db}
}

// Store of the case with an onboarding chat and a plain chat, the run started in the first
func onbChatStore(t *testing.T) *state.DB {
	t.Helper()
	onboardingSessionRoot(t)
	db := getDB(context.Background())
	onbStartRecord(t, db, "onb")
	if err := db.CreateSession(state.NewSession{ID: "plain", Source: "desktop"}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestOnlyAChatWithARunInProgressGetsTheOnboardingToolset(t *testing.T) {
	db := onbChatStore(t)
	ctx := context.Background()
	if got := tools.OnboardingToolsets(ctx, []string{"web", "file"}, db, "onb"); !reflect.DeepEqual(got, []string{"web", "file", "onboarding"}) {
		t.Fatalf("onb %v", got)
	}
	if got := tools.OnboardingToolsets(ctx, []string{"web", "file"}, db, "plain"); !reflect.DeepEqual(got, []string{"web", "file"}) {
		t.Fatalf("plain %v", got)
	}
	if got := tools.OnboardingToolsets(ctx, []string{}, db, "onb"); got == nil || len(got) != 0 {
		t.Fatalf("zero tool chat %v", got)
	}
	built := &onbPreludeAgent{toolsets: []string{"web", "onboarding"}}
	if got := tools.CarryOnboardingToolset(ctx, []string{"web"}, built); !reflect.DeepEqual(got, []string{"web", "onboarding"}) {
		t.Fatalf("carried %v", got)
	}
	if got := tools.CarryOnboardingToolset(ctx, []string{"web"}, &onbPreludeAgent{toolsets: []string{"web"}}); !reflect.DeepEqual(got, []string{"web"}) {
		t.Fatalf("carried into another chat %v", got)
	}
}

func TestAnUnrestrictedChatWithARunKeepsEveryToolAndGetsSetupBeat(t *testing.T) {
	db := onbChatStore(t)
	ctx := context.Background()
	env := &tools.Env{Home: t.TempDir()}
	enabled := tools.OnboardingToolsets(ctx, nil, db, "onb")
	selected, _ := tools.Default.SelectToolNames(ctx, env, enabled, nil, tools.SelectionContext{})
	unrestricted, _ := tools.Default.SelectToolNames(ctx, env, nil, nil, tools.SelectionContext{})
	want := maps.Clone(unrestricted)
	want["setup_beat"] = true
	if !reflect.DeepEqual(selected, want) {
		t.Fatalf("selected %v want %v", selected, want)
	}
	found := false
	for _, name := range enabled {
		found = found || name == "context_engine"
	}
	if !found {
		t.Fatalf("enabled %v lacks context_engine", enabled)
	}
	if got := tools.OnboardingToolsets(ctx, nil, db, "plain"); got != nil {
		t.Fatalf("plain %v", got)
	}
	if got := tools.CarryOnboardingToolset(ctx, nil, &onbPreludeAgent{toolsets: enabled}); !reflect.DeepEqual(got, enabled) {
		t.Fatalf("carried %v", got)
	}
	if got := tools.CarryOnboardingToolset(ctx, nil, &onbPreludeAgent{}); got != nil {
		t.Fatalf("carried into an unrestricted agent %v", got)
	}
}

func TestTheOpeningPlaysTheGreetingAndNameCardUntilTheNameCardWasShown(t *testing.T) {
	db := onbChatStore(t)
	agent := onbChatAgent(db, "onb")
	name := map[string]any{"beat": "name"}
	if got := tools.OnboardingPrelude(agent, nil); !reflect.DeepEqual(got, []tools.OnboardingPreludeStep{{Text: tools.OnboardingGreeting, Tool: "setup_beat", Args: name}}) {
		t.Fatalf("opening %v", got)
	}
	relaunch := []map[string]any{{"role": "assistant", "content": tools.OnboardingGreeting}}
	if got := tools.OnboardingPrelude(agent, relaunch); !reflect.DeepEqual(got, []tools.OnboardingPreludeStep{{Text: "", Tool: "setup_beat", Args: name}}) {
		t.Fatalf("relaunch %v", got)
	}
	if _, err := tools.AdvanceOnboarding(context.Background(), db, "onb", tools.OnbShown{Beat: "name"}); err != nil {
		t.Fatal(err)
	}
	if got := tools.OnboardingPrelude(agent, nil); got != nil {
		t.Fatalf("after the name card %v", got)
	}
	if got := tools.OnboardingPrelude(&onbPreludeAgent{valid: map[string]bool{"terminal": true}}, nil); got != nil {
		t.Fatalf("an agent without setup_beat %v", got)
	}
}

func TestTheOpeningGreetsAndAsksTheNameInTheRunSLocale(t *testing.T) {
	for _, row := range []struct {
		locale          *string
		opensWith, asks string
	}{
		{strp("de"), "Hallo, ich bin Icebath.", "Wie darf ich Sie nennen?"},
		{strp("zh_TW"), "你好，我是 Icebath。我們", "我該怎麼稱呼你？"},
		{strp("ja-JP"), "こんにちは、Icebath です。", "なんとお呼びすればいいですか？"},
		{strp("tlh"), tools.OnboardingGreeting, "What should I call you?"},
		{nil, tools.OnboardingGreeting, "What should I call you?"},
	} {
		label := "None"
		if row.locale != nil {
			label = *row.locale
		}
		t.Run(label, func(t *testing.T) {
			onboardingSessionRoot(t)
			ctx := context.Background()
			db := getDB(ctx)
			if err := db.CreateSession(state.NewSession{ID: "onb", Source: "desktop"}); err != nil {
				t.Fatal(err)
			}
			facts := tools.OnboardingFacts{OS: "macos", FactsSource: "local", Tours: true}
			if row.locale != nil {
				facts.Locale = *row.locale
			}
			if _, err := tools.StartOnboarding(ctx, db, "onb", facts); err != nil {
				t.Fatal(err)
			}
			agent := onbChatAgent(db, "onb")
			steps := tools.OnboardingPrelude(agent, nil)
			if len(steps) != 1 {
				t.Fatalf("opening %v", steps)
			}
			hello := steps[0]
			if !strings.HasPrefix(hello.Text, row.opensWith) || hello.Tool != "setup_beat" || !reflect.DeepEqual(hello.Args, map[string]any{"beat": "name"}) {
				t.Fatalf("opening %+v", hello)
			}
			again := tools.OnboardingPrelude(agent, []map[string]any{{"role": "assistant", "content": hello.Text}})
			if len(again) == 0 || again[0].Text != "" {
				t.Fatalf("relaunch %v", again)
			}
			var shown []map[string]any
			tools.SetupBeatTool(ctx, &tools.Env{Home: t.TempDir()}, db, "onb", "", hello.Args, func(payload map[string]any) (map[string]any, error) {
				shown = append(shown, payload)
				return nil, nil
			}, nil)
			if len(shown) == 0 || shown[0]["question"] != row.asks {
				t.Fatalf("shown %v", shown)
			}
		})
	}
}

func TestACompressionRotationCarriesTheRunRecord(t *testing.T) {
	onboardingSessionRoot(t)
	ctx := context.Background()
	db := getDB(ctx)
	onbStartRecord(t, db, "parent")
	if err := db.PatchSessionModelConfig(ctx, "parent", map[string]any{"codex_thread_id": "segment-only"}); err != nil {
		t.Fatal(err)
	}
	if err := db.PublishCompressionChild(ctx, state.CompressionChildPublish{ParentSessionID: "parent", ChildSessionID: "child",
		Source: "desktop", Messages: []map[string]any{{"role": "user", "content": "summary"}}}); err != nil {
		t.Fatal(err)
	}
	if child, parent := tools.LoadOnboardingRecord(db, "child"), tools.LoadOnboardingRecord(db, "parent"); child == nil || !reflect.DeepEqual(child, parent) {
		t.Fatalf("child %v parent %v", child, parent)
	}
	if value, err := db.GetSessionModelConfigValue(ctx, "child", "codex_thread_id", nil); err != nil || value != nil {
		t.Fatalf("codex_thread_id %v %v", value, err)
	}
	if active := tools.ActiveOnboardingSessionID(db); active != "child" {
		t.Fatalf("active %q", active)
	}
	deleted, err := tools.DeleteOnboardingChat(ctx, db, "parent", func() bool {
		gone, err := db.DeleteSession(ctx, "parent", state.DeleteSessionOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return gone
	})
	if err != nil || !deleted {
		t.Fatalf("delete %v %v", deleted, err)
	}
	if active := tools.ActiveOnboardingSessionID(db); active != "child" {
		t.Fatalf("active after the delete %q", active)
	}
	if _, err := tools.AdvanceOnboarding(ctx, db, "child", tools.OnbEnd{}); err != nil {
		t.Fatal(err)
	}
	if active := tools.ActiveOnboardingSessionID(db); active != "" {
		t.Fatalf("active after the end %q", active)
	}
}
