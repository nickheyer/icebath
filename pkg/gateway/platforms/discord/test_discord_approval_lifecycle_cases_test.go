package discord

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Sent exec approval card for a seeded pending request, the hermes _pending_card fixture
func pendingCard(t *testing.T) (*DiscordAdapter, *fakeDiscordAPI, *execApprovalView, *tools.ApprovalEntry) {
	t.Helper()
	a, f := parityAdapter(t)
	session := "agent:other:discord:555:" + t.Name()
	entry := tools.QueueGatewayApprovalForTests(session, map[string]any{"request_id": "request-1", "command": "ls"})
	rid := "request-1"
	res, err := a.SendExecApproval(context.Background(), "555", "ls", session, nil, nil, true, true, false, &rid)
	if err != nil || !res.Success {
		t.Fatalf("send = %+v %v", res, err)
	}
	return a, f, liveView[*execApprovalView](t, a, res.MessageID), entry
}

// Sets a scripted failure under the fake lock
func (f *fakeDiscordAPI) setFail(fn func(c fakeCall, n int) (int, string)) {
	f.mu.Lock()
	f.fail = fn
	f.mu.Unlock()
}

// Waits for every tracked background task and for the tracker to drain
func awaitBackground(t *testing.T, a *DiscordAdapter) {
	t.Helper()
	for _, task := range a.BackgroundTasks() {
		task.Wait(context.Background())
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(a.BackgroundTasks()) > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
}

// Footer text of the sent card as the view last stamped it
func cardFooter(v *execApprovalView) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	if e := firstEmbed(v.message); e != nil && e.Footer != nil {
		return e.Footer.Text
	}
	return ""
}

// Edits of the card message recorded by the fake
func cardEdits(f *fakeDiscordAPI, v *execApprovalView) []fakeCall {
	return f.matching(http.MethodPatch, "/channels/555/messages/"+v.message.ID)
}

// Whether a fake call is the in place edit answer to a click
func isEditCallback(c fakeCall) bool {
	return c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/callback") && c.Body["type"] == float64(discordgo.InteractionResponseUpdateMessage)
}

func TestClickStopsViewEvenIfInteractionEditFails(t *testing.T) {
	ctx := context.Background()
	for _, editFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "ok", true: "fails"}[editFails], func(t *testing.T) {
			a, f, view, entry := pendingCard(t)
			t.Setenv("DISCORD_ALLOW_ALL_USERS", "true")
			if editFails {
				f.setFail(func(c fakeCall, _ int) (int, string) {
					if isEditCallback(c) {
						return http.StatusInternalServerError, `{"message":"Discord unavailable","code":0}`
					}
					return 0, ""
				})
			}
			err := view.resolve(ctx, clickOn(view.message, "", discordgo.ButtonComponent, "7"), "once", 2, "platform.discord.approval.resolved_once")
			if (err != nil) != editFails {
				t.Fatalf("resolve err = %v", err)
			}
			if entry.Result != "once" || !view.isFinished() {
				t.Fatalf("result %q finished %v", entry.Result, view.isFinished())
			}
			footer := cardFooter(view)
			entry.Observers[0]("resolved")
			awaitBackground(t, a)
			if !view.isFinished() {
				t.Fatal("view revived")
			}
			edits := cardEdits(f, view)
			if !editFails {
				if len(edits) != 0 || cardFooter(view) != footer {
					t.Fatalf("observer edited a clicked card %v", edits)
				}
				return
			}
			if len(edits) != 1 || len(edits[0].Body["components"].([]any)) != 0 {
				t.Fatalf("edits = %v", edits)
			}
			if cardFooter(view) != i18n.T(ctx, "platform.discord.approval.already_resolved", "", nil) {
				t.Fatalf("footer = %q", cardFooter(view))
			}
		})
	}
}

func TestObserverThatFiresBeforeAFailedClickEditStillSeesTheCardRetired(t *testing.T) {
	ctx := context.Background()
	a, f, view, entry := pendingCard(t)
	t.Setenv("DISCORD_ALLOW_ALL_USERS", "true")
	var editedEarly bool
	f.setFail(func(c fakeCall, _ int) (int, string) {
		if !isEditCallback(c) {
			return 0, ""
		}
		entry.Observers[0]("resolved")
		awaitBackground(t, a)
		editedEarly = len(f.matching(http.MethodPatch, "/channels/555/messages/")) != 0
		return http.StatusInternalServerError, `{"message":"Discord unavailable","code":0}`
	})
	if err := view.resolve(ctx, clickOn(view.message, "", discordgo.ButtonComponent, "7"), "once", 2, "platform.discord.approval.resolved_once"); err == nil {
		t.Fatal("failed edit not raised")
	}
	if editedEarly {
		t.Fatal("observer edited a card the click owned")
	}
	edits := cardEdits(f, view)
	if len(edits) != 1 || len(edits[0].Body["components"].([]any)) != 0 {
		t.Fatalf("edits = %v", edits)
	}
	if cardFooter(view) != i18n.T(ctx, "platform.discord.approval.already_resolved", "", nil) {
		t.Fatalf("footer = %q", cardFooter(view))
	}
}

func TestUiTimeoutDoesNotEndWaitOrOverwriteLaterAnswer(t *testing.T) {
	ctx := context.Background()
	for _, uiTimeoutFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "timeout_first", false: "answer_first"}[uiTimeoutFirst], func(t *testing.T) {
			_, _, view, entry := pendingCard(t)
			if uiTimeoutFirst {
				view.onTimeout(ctx)
				select {
				case <-entry.Done():
					t.Fatal("UI timeout ended the wait")
				default:
				}
				if entry.Result != "" {
					t.Fatalf("result = %q", entry.Result)
				}
			}
			if n := tools.ResolveGatewayApproval(view.sessionKey, "once", false, "", view.requestID); n != 1 {
				t.Fatalf("resolved %d", n)
			}
			view.retire(ctx, "resolved", true)
			if !uiTimeoutFirst {
				view.onTimeout(ctx)
			}
			if cardFooter(view) != i18n.T(ctx, "platform.discord.approval.already_resolved", "", nil) || !view.isFinished() {
				t.Fatalf("footer = %q", cardFooter(view))
			}
		})
	}
}

func TestExternalSettlementRetiresCardAndTracksEdit(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct{ reason, key string }{
		{"resolved", "platform.discord.approval.already_resolved"},
		{"timeout", "platform.discord.prompt.expired_footer"},
		{"interrupted", "platform.discord.slash_confirm.resolved_cancel"},
		{"session_closed", "platform.discord.slash_confirm.resolved_cancel"},
		{"notify_failed", "platform.discord.slash_confirm.resolved_cancel"},
	} {
		t.Run(c.reason, func(t *testing.T) {
			a, f, view, entry := pendingCard(t)
			pendingColor := view.message.Embeds[0].Color
			entered, release := make(chan struct{}), make(chan struct{})
			f.setFail(func(fc fakeCall, _ int) (int, string) {
				if fc.Method == http.MethodPatch {
					close(entered)
					<-release
				}
				return 0, ""
			})
			entry.Observers[0](c.reason)
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("retire never edited the card")
			}
			if !view.isFinished() || len(a.BackgroundTasks()) != 1 {
				t.Fatalf("finished %v tasks %d", view.isFinished(), len(a.BackgroundTasks()))
			}
			edit := cardEdits(f, view)[0].Body
			embed := edit["embeds"].([]any)[0].(map[string]any)
			if len(edit["components"].([]any)) != 0 || embed["footer"].(map[string]any)["text"] != i18n.T(ctx, c.key, "", nil) ||
				embed["color"] == float64(pendingColor) {
				t.Fatalf("edit = %v", edit)
			}
			close(release)
			awaitBackground(t, a)
			if n := len(a.BackgroundTasks()); n != 0 {
				t.Fatalf("tasks left %d", n)
			}
		})
	}
}

func TestInflightUiExpiryCannotOverwriteBackendAnswer(t *testing.T) {
	ctx := context.Background()
	_, f, view, _ := pendingCard(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var displayed []string
	first := true
	f.setFail(func(fc fakeCall, _ int) (int, string) {
		if fc.Method != http.MethodPatch {
			return 0, ""
		}
		text := fc.Body["embeds"].([]any)[0].(map[string]any)["footer"].(map[string]any)["text"].(string)
		mu.Lock()
		wait := first
		first = false
		mu.Unlock()
		if wait {
			close(entered)
			<-release
		}
		mu.Lock()
		displayed = append(displayed, text)
		mu.Unlock()
		return 0, ""
	})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); view.onTimeout(ctx) }()
	<-entered
	go func() { defer wg.Done(); view.retire(ctx, "resolved", true) }()
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	want := []string{i18n.T(ctx, "platform.discord.prompt.expired_footer", "", nil), i18n.T(ctx, "platform.discord.approval.already_resolved", "", nil)}
	if len(displayed) != 2 || displayed[0] != want[0] || displayed[1] != want[1] {
		t.Fatalf("displayed = %q", displayed)
	}
}

func TestTimeoutEditsCannotRestoreButtonsOrPendingEmbed(t *testing.T) {
	ctx := context.Background()
	for _, noticeLast := range []bool{false, true} {
		t.Run(map[bool]string{false: "retire_last", true: "notice_last"}[noticeLast], func(t *testing.T) {
			a, f, view, _ := pendingCard(t)
			pendingColor := float64(view.message.Embeds[0].Color)
			var mu sync.Mutex
			state := map[string]any{"content": "command", "components": []any{"live"}, "embeds": []any{map[string]any{"color": pendingColor}}}
			f.setFail(func(fc fakeCall, _ int) (int, string) {
				if fc.Method == http.MethodPatch {
					mu.Lock()
					for k, v := range fc.Body {
						state[k] = v
					}
					mu.Unlock()
				}
				return 0, ""
			})
			notice := func() { a.EditMessage(ctx, "555", view.message.ID, "Timed out", false) }
			retire := func() { view.retire(ctx, "timeout", true) }
			if noticeLast {
				retire()
				notice()
			} else {
				notice()
				retire()
			}
			mu.Lock()
			defer mu.Unlock()
			embed := state["embeds"].([]any)[0].(map[string]any)
			if state["content"] != "Timed out" || len(state["components"].([]any)) != 0 || embed["color"] == pendingColor || !view.isFinished() {
				t.Fatalf("state = %v", state)
			}
		})
	}
}

func TestExternalAnswerThenStaleClickPreservesResolvedObserver(t *testing.T) {
	ctx := context.Background()
	a, f, view, entry := pendingCard(t)
	a.allowedUserIDs = newStrSet("42")
	view.allowedUserIDs = a.allowedUserIDs
	if n := tools.ResolveGatewayApproval(view.sessionKey, "once", false, "", "request-1"); n != 1 || entry.Result != "once" {
		t.Fatalf("resolved %d result %q", n, entry.Result)
	}
	view.retireMu.Lock()
	entry.Observers[0]("resolved")
	if view.isResolved() {
		view.retireMu.Unlock()
		t.Fatal("the resolved observer ran before the stale click")
	}
	click := clickOn(view.message, "", discordgo.ButtonComponent, "42")
	err := view.resolve(ctx, click, "once", 2, "platform.discord.approval.resolved_once")
	view.retireMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	awaitBackground(t, a)
	if cardFooter(view) != i18n.T(ctx, "platform.discord.approval.already_resolved", "", nil) {
		t.Fatalf("footer = %q", cardFooter(view))
	}
	if got := ephemeralReply(t, f, click); got != i18n.T(ctx, "platform.discord.approval.no_longer_pending", "", nil) {
		t.Fatalf("reply = %q", got)
	}
}

func TestFailedClickKeepsAPendingCardLive(t *testing.T) {
	ctx := context.Background()
	a, f, view, entry := pendingCard(t)
	t.Setenv("DISCORD_ALLOW_ALL_USERS", "true")
	real := resolveGatewayApprovalFn
	resolveGatewayApprovalFn = func(string, string, bool, string, string) int { panic("division by zero") }
	defer func() { resolveGatewayApprovalFn = real }()
	click := clickOn(view.message, "", discordgo.ButtonComponent, "7")
	if err := view.resolve(ctx, click, "once", 1, "gateway.exec_approval.action_once"); err != nil {
		t.Fatal(err)
	}
	if got := ephemeralReply(t, f, click); got != i18n.T(ctx, "platform.discord.approval.click_failed", "", nil) {
		t.Fatalf("reply = %q", got)
	}
	if view.isResolved() || view.isFinished() {
		t.Fatal("failed click retired the card")
	}
	resolveGatewayApprovalFn = real
	if n := real(view.sessionKey, "once", false, "", "request-1"); n != 1 {
		t.Fatalf("pending entry lost, resolved %d", n)
	}
	entry.Observers[0]("resolved")
	awaitBackground(t, a)
	if cardFooter(view) != i18n.T(ctx, "platform.discord.approval.already_resolved", "", nil) || !view.isFinished() {
		t.Fatalf("footer = %q", cardFooter(view))
	}
}
