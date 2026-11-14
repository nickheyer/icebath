package tuiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

const (
	onbE2ESID    = "ui-onboarding"
	onbE2EStored = "onboarding-chat"
)

// Onboarding chat whose real agent sends setup cards to a scripted desktop that answers each frame
type onbE2EChat struct {
	ctx   context.Context
	db    *state.DB
	agent *agent.AIAgent
	home  string
	mu    sync.Mutex
	cards []map[string]any
	reply []map[string]any
	show  func(card map[string]any)
}

// Live onboarding chat on a fresh launch root with every card frame answered by the scripted desktop
func onboardingE2EChat(t *testing.T) *onbE2EChat {
	t.Helper()
	root := onboardingSessionRoot(t)
	ctx := context.Background()
	db := getDB(ctx)
	if err := db.CreateSession(state.NewSession{ID: onbE2EStored, Source: "desktop"}); err != nil {
		t.Fatal(err)
	}
	facts := tools.OnboardingFacts{OS: "windows", Arch: "amd64", MachineKind: "PC", NvidiaPlatform: "discrete",
		FactsSource: "local", Plugins: []string{"nvidia-app", "nvidia-broadcast", "blender"}, Tours: true}
	if _, err := tools.StartOnboarding(ctx, db, onbE2EStored, facts); err != nil {
		t.Fatal(err)
	}
	onbLiveSession(t, onbE2ESID, map[string]any{"session_key": onbE2EStored, "source": "desktop"})
	chat := &onbE2EChat{ctx: ctx, db: db, home: root}
	write, emit, answerable, clients := tuigateway.CurrentSinks()
	tuigateway.BindSinks(func(frame *pyjson.Object) {
		card, _ := pyjson.Plain(frame.GetOr("params", nil)).(map[string]any)
		chat.mu.Lock()
		chat.cards = append(chat.cards, card)
		show := chat.show
		var result map[string]any
		if show == nil && len(chat.reply) > 0 {
			result, chat.reply = chat.reply[0], chat.reply[1:]
		}
		chat.mu.Unlock()
		if show != nil {
			show(card)
			return
		}
		if !tuigateway.ResolveResponse(objectOf(map[string]any{"jsonrpc": "2.0", "id": frame.GetOr("id", nil), "result": result}), nil) {
			t.Error("the desktop answer was not taken")
		}
	}, func(string, string, map[string]any) {}, func(string) bool { return true }, clients)
	t.Cleanup(func() { tuigateway.BindSinks(write, emit, answerable, clients) })
	opts := agent.AIAgentOptions{SessionDB: db, SessionID: onbE2EStored, Platform: "desktop", EnabledToolsets: []string{"onboarding"},
		Env: &tools.Env{Home: root, Values: &sync.Map{}}}
	agentCbs(ctx, onbE2ESID).applyTo(&opts)
	chat.agent = testAgent(t, opts)
	return chat
}

// Runs one setup_beat call through the agent's inline executor, the desktop answering its card with reply
func (c *onbE2EChat) beat(t *testing.T, reply map[string]any, args map[string]any) map[string]any {
	t.Helper()
	c.mu.Lock()
	if reply != nil {
		c.reply = append(c.reply, reply)
	}
	callID := fmt.Sprintf("call_%d", len(c.cards))
	c.mu.Unlock()
	out, err := tools.InlineToolExecutorOf("setup_beat")(c.ctx, c.agent.InlineExecutorHost(), args, tools.InlineToolContext{TaskID: "task", ToolCallID: callID})
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]any{}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("setup_beat answered %q", out)
	}
	return result
}

// Settles a general tool result of the chat through the onboarding hook
func (c *onbE2EChat) observe(t *testing.T, name string, args map[string]any, result string) {
	t.Helper()
	tools.ObserveOnboardingToolResult(c.ctx, c.db, onbE2EStored, true, name, args, result)
}

func TestEveryBeatFromTheNameCardToDone(t *testing.T) {
	chat := onboardingE2EChat(t)
	named := chat.beat(t, map[string]any{"answer": map[string]any{"name": "Sid"}}, map[string]any{"beat": "name"})
	if reminder, _ := named["reminder"].(string); named["next"] != "theme" || !strings.Contains(reminder, "Sid") {
		t.Fatalf("named %v", named)
	}
	if got := chat.beat(t, map[string]any{"answer": map[string]any{"accent": "Ultraviolet"}}, map[string]any{"beat": "next"}); got["next"] != "layout" {
		t.Fatalf("theme %v", got)
	}
	if typed := chat.beat(t, map[string]any{"wrote_instead": true}, map[string]any{"beat": "next"}); typed["wrote_instead"] != true {
		t.Fatalf("typed %v", typed)
	}
	chat.observe(t, "set_theme", map[string]any{"layout": "terminal-deck"}, `{"success": true, "applied": {"layout": "terminal-deck"}}`)
	connectors := chat.beat(t, map[string]any{"answer": map[string]any{"connected": []any{"slack"}, "newly_connected": []any{"slack"}}},
		map[string]any{"beat": "connectors", "suggest": []any{"slack", "outlook"}})
	if connectors["next"] != "plugins" {
		t.Fatalf("connectors %v", connectors)
	}
	if plugins := chat.beat(t, map[string]any{"answer": map[string]any{"installed": []any{"nvidia-app"}}}, map[string]any{"beat": "next"}); plugins["next"] != "tour" {
		t.Fatalf("plugins %v", plugins)
	}
	tour := chat.beat(t, map[string]any{"answer": map[string]any{"tour": "quick"}}, map[string]any{"beat": "next"})
	if reminder, _ := tour["reminder"].(string); !strings.Contains(reminder, `"preset":"quick"`) {
		t.Fatalf("tour %v", tour)
	}
	chat.observe(t, "gui_tour", map[string]any{"action": "start", "preset": "quick"}, `{"success": true}`)
	build := chat.beat(t, map[string]any{"answer": map[string]any{"build": "games"}},
		map[string]any{"beat": "next", "options": []any{map[string]any{"id": "games", "label": "Tune my games"}}})
	if reminder, _ := build["reminder"].(string); !strings.Contains(reminder, "--accept-package-agreements") || !strings.Contains(reminder, "How does this look?") {
		t.Fatalf("build %v", build)
	}
	if done := chat.beat(t, nil, map[string]any{"beat": "done"}); done["completed"] != true {
		t.Fatalf("done %v", done)
	}
	beats := []any{}
	for _, card := range chat.cards {
		beats = append(beats, card["beat"])
	}
	if !reflect.DeepEqual(beats, []any{"name", "theme", "layout", "connectors", "plugins", "tour", "build"}) {
		t.Fatalf("beats %v", beats)
	}
	if card := chat.cards[3]["card"]; !reflect.DeepEqual(card, map[string]any{"suggested": []any{"slack", "outlook"}}) {
		t.Fatalf("connectors card %v", card)
	}
	options, _ := chat.cards[6]["card"].(map[string]any)["options"].([]any)
	if len(options) == 0 || options[0].(map[string]any)["id"] != "games" {
		t.Fatalf("build options %v", options)
	}
	record := tools.LoadOnboardingRecord(chat.db, onbE2EStored)
	picks, _ := record["picks"].(map[string]any)
	if record["phase"] != "complete" || picks["layout"] != "terminal-deck" {
		t.Fatalf("record %v", record)
	}
	section := onboardingSection(t, chat.home)
	if section["run"] != false || onboardingSeen(section)["profile_build_offered"] != true {
		t.Fatalf("onboarding section %v", section)
	}
	if _, refused := chat.beat(t, nil, map[string]any{"beat": "next"})["error"]; !refused {
		t.Fatal("a beat after done was not refused")
	}
}

func TestAMessageWrittenWhileACardWaitsEndsTheTurnAndTheNextTurnBringsTheCardBack(t *testing.T) {
	chat := onboardingE2EChat(t)
	chat.beat(t, map[string]any{"answer": map[string]any{"name": "Sid"}}, map[string]any{"beat": "name"})
	chat.beat(t, map[string]any{"answer": map[string]any{"accent": "Ultraviolet"}}, map[string]any{"beat": "next"})
	closed := chat.beat(t, map[string]any{"wrote_instead": true}, map[string]any{"beat": "next"})
	if _, said := closed["said"]; closed["beat"] != "layout" || closed["wrote_instead"] != true || said {
		t.Fatalf("closed %v", closed)
	}
	if !chat.agent.InterruptRequested() || chat.agent.ToolInterruptReason() != "user sent a new message" {
		t.Fatalf("interrupt %v reason %q", chat.agent.InterruptRequested(), chat.agent.ToolInterruptReason())
	}
	stageOnboardingTurnNote(chat.ctx, onbE2ESID, chat.agent, false)
	note := chat.agent.GatewayTurnContextNotes()
	if !strings.Contains(note, "the layout card is closed") || !strings.Contains(note, `setup_beat(beat="layout")`) || !strings.Contains(note, `"layout":"sidebar-left" or "terminal-deck"`) {
		t.Fatalf("note %q", note)
	}
}

func TestSkipSetupOnTheCardEndsOnboarding(t *testing.T) {
	chat := onboardingE2EChat(t)
	chat.show = func(map[string]any) {
		reply := welcomeClaimCall(chat.ctx, "onboarding.skip", map[string]any{"session_id": onbE2ESID})
		if !reflect.DeepEqual(reply["result"], map[string]any{"skipped": true}) {
			t.Errorf("skip %v", reply)
		}
	}
	skipped := chat.beat(t, nil, map[string]any{"beat": "next"})
	if reminder, _ := skipped["reminder"].(string); skipped["skipped_all"] != true || !strings.Contains(reminder, "stop guiding") {
		t.Fatalf("skipped %v", skipped)
	}
	if phase := tools.LoadOnboardingRecord(chat.db, onbE2EStored)["phase"]; phase != "skipped" {
		t.Fatalf("phase %v", phase)
	}
}
