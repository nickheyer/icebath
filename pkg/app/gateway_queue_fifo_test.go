package app

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
)

const fifoText = "N5 queued while the turn runs"

// Remembers one admission this viewer queued on the scripted session
func rememberQueued(conn *gatewaycli.GatewayConnection, admission, text string) *gatewaycli.SessionState {
	state := conn.State(scriptSID)
	state.Lock()
	state.QueuedInputs.Set(admission, text)
	state.Unlock()
	return state
}

// Number of admissions the viewer still remembers as queued
func rememberedQueued(state *gatewaycli.SessionState) int {
	state.Lock()
	defer state.Unlock()
	return state.QueuedInputs.Len()
}

// Seeded item the idle tick queued for an admission the owner started
func adoptedItem(t *testing.T, c *CLI) *seededQueryMessage {
	t.Helper()
	item, ok := c.pendingInput.GetNoWait()
	if !ok {
		t.Fatal("nothing was queued")
	}
	seeded, isSeeded := item.(*seededQueryMessage)
	if !isSeeded {
		t.Fatalf("queued %T %v", item, item)
	}
	return seeded
}

// Turn of a fresh agent on the connection with the text staged as the typed user row
func runStagedTurn(t *testing.T, conn *gatewaycli.GatewayConnection, text string) map[string]any {
	t.Helper()
	a := scriptedAgentOn(t, conn, gatewayAgentOptions{})
	a.SetPendingCLIUserMessage(map[string]any{"role": "user", "content": text})
	return runTurn(t, a, text)
}

// Pending list event of the owner with the given rows
func publishPending(state *gatewaycli.SessionState, rows ...any) {
	state.ApplyEvent(context.Background(), "session.info", map[string]any{"pending": append([]any{}, rows...)})
}

// Busy CLI that owns one queued admission the owner lists, every cancel and replace refused with the reason
func ownedQueueCLI(t *testing.T, refusal string) (*CLI, *gatewaycli.GatewayConnection, *printRecorder) {
	t.Helper()
	c, client, conn, rec := idleNotifyCLI(t)
	c.uiMu.Lock()
	c.agentRunning = true
	c.uiMu.Unlock()
	rememberQueued(conn, "adm-q", fifoText)
	client.mu.Lock()
	client.override = func(method string, params map[string]any) (any, bool, error) {
		switch method {
		case "prompt.receipt":
			return map[string]any{"admission_id": "adm-q", "status": "queued", "sequence": 1}, true, nil
		case "prompt.cancel", "prompt.replace_queued":
			return nil, true, &gateway.GatewayRPCError{Msg: refusal}
		}
		return nil, false, nil
	}
	client.mu.Unlock()
	return c, conn, rec
}

// The two verbs that change one queued row, each with its argument text
var queueRowVerbs = []struct {
	name string
	run  func(c *CLI)
}{
	{"_queue_remove", func(c *CLI) { c.queueRemove("1") }},
	{"_queue_edit", func(c *CLI) { c.queueEdit("1 new text") }},
}

func TestARefusedQueueRmOrEditSaysWhatWasRefused(t *testing.T) {
	for _, verb := range queueRowVerbs {
		t.Run(verb.name, func(t *testing.T) {
			c, _, rec := ownedQueueCLI(t, "unsupported_control")
			verb.run(c)
			out := printedText(rec)
			if strings.Contains(out, "Nothing was queued") || strings.Contains(out, "send it again") {
				t.Fatalf("submit refusal text in %q", out)
			}
			if !strings.Contains(strings.ToLower(out), "queue item 1") || !strings.Contains(out, "unsupported_control") {
				t.Fatalf("output %q", out)
			}
		})
	}
}

func TestAnRmOrEditOfARowTheDrainAlreadyStartedSaysItStarted(t *testing.T) {
	for _, verb := range queueRowVerbs {
		t.Run(verb.name, func(t *testing.T) {
			c, conn, rec := ownedQueueCLI(t, "stale_generation")
			verb.run(c)
			if out := printedText(rec); !strings.Contains(out, "already started") {
				t.Fatalf("output %q", out)
			}
			state := conn.State(scriptSID)
			state.Lock()
			_, owed := state.QueuedInputs.Get("adm-q")
			state.Unlock()
			if !owed {
				t.Fatal("the started admission is no longer owed a paint")
			}
		})
	}
}

func TestAnRmRacingTheDrainClaimTellsTheUserTheGatewayStoppedIt(t *testing.T) {
	c, client, conn, rec := idleNotifyCLI(t)
	c.uiMu.Lock()
	c.agentRunning = true
	c.uiMu.Unlock()
	state := rememberQueued(conn, "adm-q", fifoText)
	client.mu.Lock()
	client.override = func(method string, params map[string]any) (any, bool, error) {
		switch method {
		case "prompt.receipt":
			return map[string]any{"admission_id": "adm-q", "status": "queued", "sequence": 1}, true, nil
		case "prompt.cancel":
			return map[string]any{"admission_id": "adm-q", "status": "started"}, true, nil
		}
		return nil, false, nil
	}
	client.mu.Unlock()
	c.queueRemove("1")
	if out := printedText(rec); !strings.Contains(out, "already started") || !strings.Contains(out, "stopped") {
		t.Fatalf("output %q", out)
	}
	state.Lock()
	_, owed := state.QueuedInputs.Get("adm-q")
	state.Unlock()
	if !owed {
		t.Fatal("the interrupted turn is no longer owed a paint")
	}
}

// Busy CLI whose prompt.submit answer is decided by the given function, every sent submission id recorded
func busySubmitCLI(t *testing.T, answer func(sent int) (any, error)) (*CLI, *scriptedClient, *gatewaycli.GatewayConnection, *printRecorder, *[]any) {
	t.Helper()
	c, client, conn, rec := idleNotifyCLI(t)
	c.uiMu.Lock()
	c.agentRunning = true
	c.uiMu.Unlock()
	seen := &[]any{}
	client.mu.Lock()
	client.override = func(method string, params map[string]any) (any, bool, error) {
		if method != "prompt.submit" {
			return nil, false, nil
		}
		client.record(method, params)
		*seen = append(*seen, params["submission_id"])
		reply, err := answer(len(*seen))
		return reply, true, err
	}
	client.mu.Unlock()
	return c, client, conn, rec, seen
}

// Submit double that loses its answer the first failures times and then admits adm-q
func flakySubmit(failures int) func(sent int) (any, error) {
	return func(sent int) (any, error) {
		if sent <= failures {
			return nil, &gateway.GatewayClientError{Msg: "Gateway did not answer prompt.submit"}
		}
		return map[string]any{"admission_id": "adm-q", "status": "queued"}, nil
	}
}

func TestAQueueSubmitWhoseAnswerWasLostIsRetriedUnderTheSameIdentity(t *testing.T) {
	c, _, conn, rec, seen := busySubmitCLI(t, flakySubmit(1))
	c.queueEnqueue(fifoText)
	if len(*seen) != 2 || (*seen)[0] != (*seen)[1] {
		t.Fatalf("submission ids %v", *seen)
	}
	if out := printedText(rec); strings.Contains(out, "Nothing was queued") {
		t.Fatalf("output %q", out)
	}
	state := conn.State(scriptSID)
	state.Lock()
	defer state.Unlock()
	if text, _ := state.QueuedInputs.Get("adm-q"); state.QueuedInputs.Len() != 1 || text != fifoText {
		t.Fatalf("queued inputs %v", state.QueuedInputs.Keys)
	}
}

func TestAQueueSubmitWithNoAnswerAtAllNeverSaysNothingWasQueued(t *testing.T) {
	c, _, conn, rec, _ := busySubmitCLI(t, flakySubmit(2))
	c.queueEnqueue(fifoText)
	out := printedText(rec)
	if strings.Contains(out, "Nothing was queued") || strings.Contains(out, "send it again") || !strings.Contains(out, "/queue list") {
		t.Fatalf("output %q", out)
	}
	if rememberedQueued(conn.State(scriptSID)) != 0 {
		t.Fatal("an unanswered submit was remembered as queued")
	}
}

func TestAQueueSubmitTheOwnerRefusedStillSaysNothingWasQueued(t *testing.T) {
	c, client, _, rec, _ := busySubmitCLI(t, func(int) (any, error) { return nil, &gateway.GatewayRPCError{Msg: "queue_full"} })
	c.queueEnqueue(fifoText)
	if out := printedText(rec); !strings.Contains(out, "Nothing was queued") || !strings.Contains(out, "queue_full") {
		t.Fatalf("output %q", out)
	}
	if n := len(client.callsOf("prompt.submit")); n != 1 {
		t.Fatalf("a verdict was retried: %d submits", n)
	}
}
