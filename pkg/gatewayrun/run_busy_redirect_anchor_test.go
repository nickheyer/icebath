package gatewayrun

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

// Loopback provider whose main requests all wait for one release, each arrival signalled
type gatedProvider struct {
	server   *fakellm.Server
	arrivals chan struct{}
	gate     chan struct{}
	once     sync.Once
}

// Provider answering every main request with answer once released, stopped after release at cleanup
func newGatedProvider(t *testing.T, answer string) *gatedProvider {
	t.Helper()
	p := &gatedProvider{arrivals: make(chan struct{}, 64), gate: make(chan struct{})}
	server, err := fakellm.NewResponder(func(*fakellm.Request) any {
		p.arrivals <- struct{}{}
		<-p.gate
		return fakellm.Text{Text: answer}
	}, fakellm.Options{})
	if err != nil {
		t.Fatal(err)
	}
	p.server = server
	t.Cleanup(func() {
		p.release()
		server.Stop()
	})
	return p
}

// Lets every waiting and later main request answer
func (p *gatedProvider) release() { p.once.Do(func() { close(p.gate) }) }

// Waits for the next main request to reach the provider
func (p *gatedProvider) awaitRequest(t *testing.T) {
	t.Helper()
	select {
	case <-p.arrivals:
	case <-time.After(60 * time.Second):
		t.Fatal("no model request reached the provider")
	}
}

// Reply and error of a finished handle message call
type handledTurn struct {
	result any
	err    error
}

// Real runner with a telegram turn for message A whose live agent waits inside its model request
type gatedTurn struct {
	r        *GatewayRunner
	provider *gatedProvider
	key      string
	source   *gateway.SessionSource
	opening  *gateway.MessageEvent
	running  *agent.AIAgent
	done     chan handledTurn
}

// Telegram dm event with the given text, message id and reply expectation
func anchorEvent(text, messageID string, replyExpected *bool) *gateway.MessageEvent {
	event := gateway.NewMessageEvent(text)
	event.Source, event.MessageID, event.ReplyExpected = dispatchSource(), &messageID, replyExpected
	return event
}

// Starts message A on a real runner and returns once its promoted agent is mid request
func startGatedTurn(t *testing.T, openingReplyExpected *bool) *gatedTurn {
	t.Helper()
	t.Setenv("ICEBATH_TELEGRAM_FOLLOWUP_GRACE_SECONDS", "0")
	provider := newGatedProvider(t, "Sunny in Shanghai, and tomorrow is Friday.")
	r, _ := makeDispatchRunner(t, provider.server)
	ctx := context.Background()
	key, err := r.sessionKeyForSource(ctx, dispatchSource())
	if err != nil {
		t.Fatal(err)
	}
	g := &gatedTurn{r: r, provider: provider, key: key, source: dispatchSource(), done: make(chan handledTurn, 1),
		opening: anchorEvent("What is the weather in Shanghai?", "A", openingReplyExpected)}
	go func() {
		result, err := r.handleMessage(ctx, g.opening)
		g.done <- handledTurn{result, err}
	}()
	t.Cleanup(func() {
		provider.release()
		g.finish(t)
		if a := r.cachedAgentFor(key, true); a != nil {
			r.cleanupAgentResources(context.Background(), a)
		}
	})
	provider.awaitRequest(t)
	deadline := time.Now().Add(60 * time.Second)
	for g.running = r.runningSlotAgent(key); g.running == nil || g.running == agentPendingSentinel; g.running = r.runningSlotAgent(key) {
		if time.Now().After(deadline) {
			t.Fatal("the turn never promoted its agent into the running slot")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return g
}

// Waits for message A to finish and returns its outcome, the same outcome on every call
func (g *gatedTurn) finish(t *testing.T) handledTurn {
	t.Helper()
	select {
	case outcome := <-g.done:
		g.done <- outcome
		return outcome
	case <-time.After(120 * time.Second):
		t.Fatal("message A never finished")
		return handledTurn{}
	}
}

// Turn context the running slot of the session publishes, nil before promotion
func (g *gatedTurn) turnContext() *TurnContext {
	var tc *TurnContext
	g.r.withPeekedSessionState(g.key, func(s *SessionState) { tc = s.turn.ctx })
	return tc
}

// Real agent outside any model request on a dead loopback endpoint, its home left to the running case
func offRequestAgent(t *testing.T) *agent.AIAgent {
	t.Helper()
	a, err := agent.NewAIAgent(context.Background(), agent.AIAgentOptions{
		Model: "anthropic/claude-sonnet-4", APIKey: "test", BaseURL: "http://127.0.0.1:9/v1", Provider: "openrouter",
		MaxIterations: 5, QuietMode: true, SkipContextFiles: true, SkipMemory: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Whether an optional reply expectation is set and true
func expectsReply(b *bool) bool {
	return b != nil && *b
}

// test_busy_redirect_anchor.py test_successful_redirect_moves_the_turn_reply_anchor_to_the_redirecting_message
func TestSuccessfulRedirectMovesTheTurnReplyAnchorToTheRedirectingMessage(t *testing.T) {
	for _, route := range []string{"busy_interrupt", "priority"} {
		t.Run(route, func(t *testing.T) {
			ctx := context.Background()
			g := startGatedTurn(t, nil)
			redirecting := anchorEvent("What day is tomorrow?", "B", nil)
			if route == "priority" {
				if err := g.r.hmBusyInterrupt(ctx, redirecting, g.source, g.running, g.key); err != nil {
					t.Fatal(err)
				}
			} else {
				outcome, err := g.r.resolveBusySteerOrRedirect(ctx, redirecting, g.key, "interrupt", g.running)
				if err != nil || !outcome.redirected {
					t.Fatalf("outcome %+v err %v", outcome, err)
				}
			}
			override, ledger, _ := g.opening.ReplyFields()
			if optText(override) != "B" || optText(ledger) != "B" {
				t.Fatalf("opening anchor %q ledger %q", optText(override), optText(ledger))
			}
			anchor, inbound, _ := g.r.turnReplyAnchors(g.turnContext())
			if optText(anchor) != "B" || optText(inbound) != "B" {
				t.Fatalf("turn context anchor %q inbound %q", optText(anchor), optText(inbound))
			}
			g.provider.release()
			outcome := g.finish(t)
			if outcome.err != nil {
				t.Fatal(outcome.err)
			}
			if got := gateway.ReplyAnchorForEvent(g.opening); optText(got) != "B" {
				t.Fatalf("final reply anchor %q", optText(got))
			}
			if optText(g.opening.LedgerMessageID) != "B" || optText(g.opening.MessageID) != "A" {
				t.Fatalf("ledger %q message id %q", optText(g.opening.LedgerMessageID), optText(g.opening.MessageID))
			}
		})
	}
}

// test_busy_redirect_anchor.py test_refused_or_foreign_redirect_leaves_the_anchor_on_the_opening_message
func TestRefusedOrForeignRedirectLeavesTheAnchorOnTheOpeningMessage(t *testing.T) {
	ctx := context.Background()
	install := func(r *GatewayRunner, key string, owner *agent.AIAgent) (*gateway.MessageEvent, *TurnContext, *gateway.MessageEvent) {
		opening := anchorEvent("What is the weather in Shanghai?", "A", nil)
		tc := newTurnContext()
		tc.sessionKey, tc.eventMessageID, tc.inboundMessageID = key, strp("A"), strp("A")
		r.withSessionState(key, func(s *SessionState) { s.turn.agent, s.turn.event, s.turn.ctx = owner, opening, tc })
		return opening, tc, anchorEvent("What day is tomorrow?", "B", nil)
	}
	g := startGatedTurn(t, nil)
	refusing := offRequestAgent(t)
	opening, tc, redirecting := install(g.r, "key", refusing)
	outcome, err := g.r.resolveBusySteerOrRedirect(ctx, redirecting, "key", "interrupt", refusing)
	if err != nil || outcome.redirected {
		t.Fatalf("outcome %+v err %v", outcome, err)
	}
	if got := gateway.ReplyAnchorForEvent(opening); optText(got) != "A" || opening.LedgerMessageID != nil {
		t.Fatalf("anchor %q ledger %v", optText(got), opening.LedgerMessageID)
	}
	if anchor, _, _ := g.r.turnReplyAnchors(tc); optText(anchor) != "A" {
		t.Fatalf("turn context anchor %q", optText(anchor))
	}
	opening2, tc2, redirecting2 := install(g.r, "key2", offRequestAgent(t))
	outcome, err = g.r.resolveBusySteerOrRedirect(ctx, redirecting2, "key2", "interrupt", g.running)
	if err != nil || !outcome.redirected {
		t.Fatalf("displaced outcome %+v err %v", outcome, err)
	}
	if got := gateway.ReplyAnchorForEvent(opening2); optText(got) != "A" {
		t.Fatalf("displaced anchor %q", optText(got))
	}
	if anchor, _, _ := g.r.turnReplyAnchors(tc2); optText(anchor) != "A" {
		t.Fatalf("displaced turn context anchor %q", optText(anchor))
	}
}

// test_busy_redirect_anchor.py test_a_turn_that_takes_in_an_addressed_message_keeps_the_silence_fallback
func TestATurnThatTakesInAnAddressedMessageKeepsTheSilenceFallback(t *testing.T) {
	for _, route := range []string{"busy_interrupt", "priority_interrupt", "busy_steer", "priority_steer", "slash_steer"} {
		t.Run(route, func(t *testing.T) {
			ctx := context.Background()
			notExpected, expected := false, true
			g := startGatedTurn(t, &notExpected)
			incoming := anchorEvent("What day is tomorrow?", "B", &expected)
			switch route {
			case "priority_interrupt":
				if err := g.r.hmBusyInterrupt(ctx, incoming, g.source, g.running, g.key); err != nil {
					t.Fatal(err)
				}
			case "priority_steer":
				if err := g.r.hmBusySteer(ctx, incoming, g.running, g.key); err != nil {
					t.Fatal(err)
				}
			case "slash_steer":
				incoming.Text = "/steer " + incoming.Text
				reply, err := g.r.busySteerCommand(ctx, incoming, g.key, g.source)
				if text, _ := reply.(string); err != nil || !strings.HasPrefix(text, "⏩") {
					t.Fatalf("reply %#v err %v", reply, err)
				}
			default:
				outcome, err := g.r.resolveBusySteerOrRedirect(ctx, incoming, g.key, strings.TrimPrefix(route, "busy_"), g.running)
				if err != nil || !(outcome.redirected || outcome.steered) {
					t.Fatalf("outcome %+v err %v", outcome, err)
				}
			}
			_, _, eventExpected := g.opening.ReplyFields()
			_, _, ctxExpected := g.r.turnReplyAnchors(g.turnContext())
			if !expectsReply(eventExpected) {
				t.Fatalf("opening reply expected %v", eventExpected)
			}
			if !expectsReply(ctxExpected) {
				t.Fatalf("turn context reply expected %v", ctxExpected)
			}
			g.provider.release()
			if outcome := g.finish(t); outcome.err != nil {
				t.Fatal(outcome.err)
			}
		})
	}
}

// A redirect and then a steer reaching one running turn through the inbound path leave its reply on the redirecting message
func TestRedirectThenSteerThroughInboundLeaveTheReplyOnTheRedirectingMessage(t *testing.T) {
	ctx := context.Background()
	notExpected, expected := false, true
	g := startGatedTurn(t, &notExpected)
	redirecting := anchorEvent("What day is tomorrow?", "B", &notExpected)
	if _, err := g.r.handleMessage(ctx, redirecting); err != nil {
		t.Fatal(err)
	}
	g.provider.awaitRequest(t)
	steering := anchorEvent("/steer and add the humidity", "C", &expected)
	reply, err := g.r.handleMessage(ctx, steering)
	if text, _ := reply.(string); err != nil || !strings.HasPrefix(text, "⏩") {
		t.Fatalf("steer reply %#v err %v", reply, err)
	}
	g.provider.release()
	outcome := g.finish(t)
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	if got := gateway.ReplyAnchorForEvent(g.opening); optText(got) != "B" {
		t.Fatalf("final reply anchor %q", optText(got))
	}
	if optText(g.opening.LedgerMessageID) != "B" || optText(g.opening.MessageID) != "A" {
		t.Fatalf("ledger %q message id %q", optText(g.opening.LedgerMessageID), optText(g.opening.MessageID))
	}
	if !expectsReply(g.opening.ReplyExpected) {
		t.Fatalf("opening reply expected %v", g.opening.ReplyExpected)
	}
}

// Busy folds and re-anchors racing the turn's own reads of its reply anchors leave every read whole and race free
func TestBusyFoldsAndReanchorsWhileTheTurnReadsItsReplyAnchors(t *testing.T) {
	r := leaseRunner(0)
	const key = "agent:main:telegram:dm:u1"
	running := &agent.AIAgent{}
	notExpected, expected := false, true
	opening := anchorEvent("What is the weather in Shanghai?", "A", &notExpected)
	opening.GuardReplyFields(&r.sessionState(key).mu)
	tc := newTurnContext()
	tc.sessionKey, tc.eventMessageID, tc.inboundMessageID, tc.replyExpected = key, strp("A"), strp("A"), &notExpected
	r.withSessionState(key, func(s *SessionState) { s.turn.agent, s.turn.event, s.turn.ctx = running, opening, tc })
	incoming := []*gateway.MessageEvent{anchorEvent("What day is tomorrow?", "B", &expected), anchorEvent("And the humidity?", "C", &expected)}
	stop, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			event := incoming[i%2]
			r.foldIntoRunningTurn(running, key, event)
			r.reanchorRunningTurn(running, key, event)
		}
	}()
	for range 20000 {
		override, ledger, replyExpected := opening.ReplyFields()
		anchor := gateway.ReplyAnchorForEvent(opening)
		ctxAnchor, ctxInbound, ctxExpected := r.turnReplyAnchors(tc)
		if (override != nil && *override != "B" && *override != "C") || (ledger != nil && *ledger != *override) || anchor == nil {
			t.Fatalf("event override %q ledger %q anchor %q", optText(override), optText(ledger), optText(anchor))
		}
		if optText(ctxAnchor) != optText(ctxInbound) || replyExpected == nil || ctxExpected == nil {
			t.Fatalf("turn context anchor %q inbound %q expected %v %v", optText(ctxAnchor), optText(ctxInbound), replyExpected, ctxExpected)
		}
	}
	close(stop)
	<-finished
	if got := gateway.ReplyAnchorForEvent(opening); optText(got) != "B" && optText(got) != "C" {
		t.Fatalf("final anchor %q", optText(got))
	}
	if !expectsReply(opening.ReplyExpected) || !expectsReply(tc.replyExpected) {
		t.Fatalf("reply expected %v %v", opening.ReplyExpected, tc.replyExpected)
	}
}
