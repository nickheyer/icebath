package discord

import (
	"context"
	"net/http"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Adapter with a live fake client and the component gate env cleared
func parityAdapter(t *testing.T) (*DiscordAdapter, *fakeDiscordAPI) {
	a, f := sendAdapter(t)
	for _, v := range []string{"DISCORD_ALLOW_ALL_USERS", "GATEWAY_ALLOW_ALL_USERS", "GATEWAY_ALLOWED_USERS"} {
		t.Setenv(v, "")
	}
	return a, f
}

// Every view kind with empty allowlists, the way the hermes parity fixture builds them
func parityViews(t *testing.T, a *DiscordAdapter) map[string]*hermesView {
	t.Helper()
	ctx := context.Background()
	noopModel := func(context.Context, string, string, string) (string, error) { return "", nil }
	noopChoice := func(context.Context, string, string) (string, error) { return "", nil }
	ev, err1 := a.newExecApprovalView(ctx, "s", strSet{}, nil, false, nil, true, true, false, "")
	sc, err2 := a.newSlashConfirmView(ctx, "s", "c", strSet{}, nil)
	up, err3 := a.newUpdatePromptView(ctx, "s", strSet{}, nil)
	cl, err4 := a.newClarifyChoiceView(ctx, []string{"a"}, "c", strSet{}, nil)
	mp, err5 := a.newModelPickerView(ctx, nil, "m", "p", "s", noopModel, strSet{}, nil)
	cp, err6 := a.newChoicePickerView(ctx, []map[string]any{{"value": "v"}}, noopChoice, strSet{}, nil)
	for _, err := range []error{err1, err2, err3, err4, err5, err6} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return map[string]*hermesView{"exec": &ev.hermesView, "slash": &sc.hermesView, "update": &up.hermesView, "clarify": &cl.hermesView,
		"model": &mp.hermesView, "choice": &cp.hermesView}
}

// Click by user 1 on message m1 carrying the select value x
func parityClick() *discordgo.Interaction {
	return clickOn(&discordgo.Message{ID: "m1", ChannelID: "5"}, "", discordgo.ButtonComponent, "1", "x")
}

// Parity calls of the hermes case table keyed by view name
var parityCalls = []struct {
	name string
	call func(v viewHooks, ctx context.Context, i *discordgo.Interaction) error
}{
	{"exec", func(v viewHooks, ctx context.Context, i *discordgo.Interaction) error {
		return v.(*execApprovalView).resolve(ctx, i, "once", 0, "x")
	}},
	{"slash", func(v viewHooks, ctx context.Context, i *discordgo.Interaction) error {
		return v.(*slashConfirmView).resolve(ctx, i, "once", 0, "x")
	}},
	{"update", func(v viewHooks, ctx context.Context, i *discordgo.Interaction) error {
		return v.(*updatePromptView).respond(ctx, i, "y", 0, "x")
	}},
	{"clarify", func(v viewHooks, ctx context.Context, i *discordgo.Interaction) error {
		return v.(*clarifyChoiceView).resolveChoice(ctx, i, 0, "a")
	}},
	{"clarify", func(v viewHooks, ctx context.Context, i *discordgo.Interaction) error {
		return v.(*clarifyChoiceView).onOther(ctx, i)
	}},
	{"model", func(v viewHooks, ctx context.Context, i *discordgo.Interaction) error {
		return v.(*modelPickerView).onProviderSelected(ctx, i)
	}},
	{"model", func(v viewHooks, ctx context.Context, i *discordgo.Interaction) error {
		return v.(*modelPickerView).onBack(ctx, i)
	}},
	{"choice", func(v viewHooks, ctx context.Context, i *discordgo.Interaction) error {
		return v.(*choicePickerView).onSelect(ctx, i)
	}},
}

func TestUnauthorizedClickUsesTheSharedNotice(t *testing.T) {
	for _, c := range parityCalls {
		t.Run(c.name, func(t *testing.T) {
			a, f := parityAdapter(t)
			view := parityViews(t, a)[c.name]
			i := parityClick()
			if err := c.call(view.hooks, context.Background(), i); err != nil {
				t.Fatal(err)
			}
			if got := ephemeralReply(t, f, i); got != gateway.UnauthorizedActionNoticeFor(context.Background(), gateway.PlatformDiscord) {
				t.Fatalf("reply = %q", got)
			}
			if view.isResolved() {
				t.Fatal("a refused click resolved the view")
			}
		})
	}
}

// Number of in place edit responses posted for one interaction
func editResponses(f *fakeDiscordAPI, i *discordgo.Interaction) int {
	n := 0
	for _, cb := range f.callbacks(i) {
		if cb["type"] == float64(discordgo.InteractionResponseUpdateMessage) {
			n++
		}
	}
	return n
}

func TestAnsweredViewIsReleased(t *testing.T) {
	answers := []struct {
		name string
		call func(v viewHooks, ctx context.Context, i *discordgo.Interaction) error
	}{
		parityCalls[1], parityCalls[2], parityCalls[3], parityCalls[4],
		{"model", func(v viewHooks, ctx context.Context, i *discordgo.Interaction) error {
			return v.(*modelPickerView).switchSelectedModel(ctx, i, "m2")
		}},
		{"model", func(v viewHooks, ctx context.Context, i *discordgo.Interaction) error {
			return v.(*modelPickerView).onCancel(ctx, i)
		}},
	}
	prev := gateway.SlashConfirmResolve
	gateway.SlashConfirmResolve = func(context.Context, string, string, string) string { return "" }
	t.Cleanup(func() { gateway.SlashConfirmResolve = prev })
	for _, c := range answers {
		t.Run(c.name, func(t *testing.T) {
			a, f := parityAdapter(t)
			t.Setenv("DISCORD_ALLOW_ALL_USERS", "true")
			view := parityViews(t, a)[c.name]
			i := parityClick()
			if err := c.call(view.hooks, context.Background(), i); err != nil {
				t.Fatal(err)
			}
			if n := editResponses(f, i); n != 1 {
				t.Fatalf("edit responses = %d", n)
			}
			if !view.isResolved() || !view.isFinished() {
				t.Fatal("answered view still live")
			}
		})
	}
}

// Prompt message with one titled embed bound to the view
func attachCard(view *hermesView) *discordgo.Message {
	msg := &discordgo.Message{ID: "77", ChannelID: "555", Embeds: []*discordgo.MessageEmbed{{Title: "card"}}}
	view.mu.Lock()
	view.message = msg
	view.mu.Unlock()
	return msg
}

func TestOnTimeoutDisablesAndGreysEmbed(t *testing.T) {
	for _, name := range []string{"exec", "slash", "update", "clarify"} {
		t.Run(name, func(t *testing.T) {
			a, f := parityAdapter(t)
			view := parityViews(t, a)[name]
			attachCard(view)
			view.hooks.onTimeout(context.Background())
			if !view.isResolved() {
				t.Fatal("timeout left the view unresolved")
			}
			for _, child := range view.children {
				if !child.disabled {
					t.Fatalf("enabled child %s", child.repr())
				}
			}
			edits := f.matching(http.MethodPatch, "/channels/555/messages/77")
			if len(edits) != 1 {
				t.Fatalf("edits = %v", edits)
			}
			embed := edits[0].Body["embeds"].([]any)[0].(map[string]any)
			footer, _ := embed["footer"].(map[string]any)
			if footer["text"] == "" || footer["text"] == nil || embed["color"] == nil {
				t.Fatalf("embed = %v", embed)
			}
			comps := edits[0].Body["components"].([]any)
			if name == "exec" {
				if len(comps) != 0 || !view.isFinished() {
					t.Fatalf("exec card kept controls %v", comps)
				}
				return
			}
			if len(comps) == 0 {
				t.Fatal("controls removed")
			}
			for _, row := range comps {
				for _, c := range row.(map[string]any)["components"].([]any) {
					if c.(map[string]any)["disabled"] != true {
						t.Fatalf("component %v enabled", c)
					}
				}
			}
		})
	}
}

func TestOnTimeoutWithoutMessageIsSafe(t *testing.T) {
	a, f := parityAdapter(t)
	for _, view := range parityViews(t, a) {
		view.hooks.onTimeout(context.Background())
	}
	if edits := f.matching(http.MethodPatch, ""); len(edits) != 0 {
		t.Fatalf("edits = %v", edits)
	}
}

func TestTimeoutAfterResolutionKeepsTheResolvedEmbed(t *testing.T) {
	for _, name := range []string{"slash", "update", "clarify", "model"} {
		t.Run(name, func(t *testing.T) {
			a, f := parityAdapter(t)
			view := parityViews(t, a)[name]
			attachCard(view)
			view.mu.Lock()
			view.resolved = true
			view.mu.Unlock()
			view.hooks.onTimeout(context.Background())
			if edits := f.matching(http.MethodPatch, ""); len(edits) != 0 {
				t.Fatalf("edits = %v", edits)
			}
		})
	}
}
