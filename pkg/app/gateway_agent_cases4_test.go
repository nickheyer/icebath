package app

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
)

// Gateway double of the session cases: resumes carry the pending rows and a submit completes unless a lost turn is ahead
type sessionCaseGateway struct {
	client  *scriptedClient
	conn    *gatewaycli.GatewayConnection
	mu      sync.Mutex
	pending []any
}

func newSessionCaseGateway(t *testing.T) *sessionCaseGateway {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	g := &sessionCaseGateway{client: &scriptedClient{}}
	g.client.override = func(method string, params map[string]any) (any, bool, error) {
		g.client.record(method, params)
		g.mu.Lock()
		defer g.mu.Unlock()
		switch method {
		case "session.resume":
			return map[string]any{"session_id": params["session_id"], "subscription_id": "sub-1", "revision": 1,
				"execution_generation": 7, "messages": []any{}, "pending": append([]any{}, g.pending...)}, true, nil
		case "prompt.resolve_unknown":
			kept := []any{}
			for _, row := range g.pending {
				if row.(map[string]any)["admission_id"] != params["admission_id"] {
					kept = append(kept, row)
				}
			}
			g.pending = kept
		case "prompt.submit":
			if len(g.pending) == 0 {
				g.client.events.PutNowait(wireValue(g.client.t, map[string]any{"jsonrpc": "2.0", "method": "event", "params": map[string]any{
					"type": "message.complete", "session_id": scriptSID, "admission_id": "adm-1", "payload": map[string]any{"text": "ok", "outcome": "completed"}}}))
			}
			return map[string]any{"admission_id": "adm-1", "status": "queued", "execution_generation": 7}, true, nil
		}
		return map[string]any{}, true, nil
	}
	g.conn = scriptedConnection(t, g.client)
	if _, err := g.conn.Reattach(context.Background(), scriptSID, nil); err != nil {
		t.Fatal(err)
	}
	return g
}

func (g *sessionCaseGateway) methods() []string {
	g.client.mu.Lock()
	defer g.client.mu.Unlock()
	out := make([]string, len(g.client.calls))
	for i, call := range g.client.calls {
		out[i] = call.method
	}
	return out
}

// Session id and value of every show_reasoning write
func (g *sessionCaseGateway) reasoningSets() [][2]any {
	var out [][2]any
	for _, params := range g.client.configSets("show_reasoning") {
		out = append(out, [2]any{params["session_id"], params["value"]})
	}
	return out
}

// Hangs the gateway up and waits until the connection noticed
func (g *sessionCaseGateway) restart(t *testing.T) {
	t.Helper()
	g.client.events.PutNowait(&gateway.GatewayClientError{Msg: "Gateway disconnected"})
	waitUntil(t, g.conn.Closed)
}

// One turn bounded to ten seconds, a turn still waiting answered as such
func runBounded(a *GatewayAgent, text string) map[string]any {
	done := make(chan map[string]any, 1)
	go func() {
		result, _ := a.RunConversation(context.Background(), text, agent.RunConversationOptions{})
		done <- result
	}()
	select {
	case result := <-done:
		return result
	case <-time.After(10 * time.Second):
		a.mu.Lock()
		a.turn = nil
		a.mu.Unlock()
		return map[string]any{"failed": false, "error": "still waiting"}
	}
}

func TestANewSessionGetsTheShowReasoningFlag(t *testing.T) {
	g := newSessionCaseGateway(t)
	a := scriptedAgentOn(t, g.conn, gatewayAgentOptions{ReasoningCallback: func(string) {}})
	a.SetSessionID("local-new")
	if got := g.reasoningSets(); !reflect.DeepEqual(got, [][2]any{{scriptSID, "1"}, {"local-new", "1"}}) {
		t.Fatalf("reasoning sets %v", got)
	}
}

func TestAHiddenViewerResumingASessionTurnsItsReasoningFlagOff(t *testing.T) {
	g := newSessionCaseGateway(t)
	a := scriptedAgentOn(t, g.conn, gatewayAgentOptions{})
	a.SetSessionID("local-shown")
	found := false
	for _, set := range g.reasoningSets() {
		found = found || set == [2]any{"local-shown", "0"}
	}
	if !found {
		t.Fatalf("reasoning sets %v", g.reasoningSets())
	}
}

func TestARestartedGatewayGetsTheFlagAgainBeforeTheNextTurn(t *testing.T) {
	g := newSessionCaseGateway(t)
	a := scriptedAgentOn(t, g.conn, gatewayAgentOptions{ReasoningCallback: func(string) {}})
	g.restart(t)
	if got := runBounded(a, "after the restart"); got["final_response"] != "ok" {
		t.Fatalf("result %v", got)
	}
	if got := g.reasoningSets(); !reflect.DeepEqual(got, [][2]any{{scriptSID, "1"}, {scriptSID, "1"}}) {
		t.Fatalf("reasoning sets %v", got)
	}
	methods := g.methods()
	resumes, lastSet, submit := 0, -1, -1
	for i, method := range methods {
		switch method {
		case "session.resume":
			resumes++
		case "config.set":
			lastSet = i
		case "prompt.submit":
			if submit < 0 {
				submit = i
			}
		}
	}
	if resumes != 2 || lastSet < 0 || submit < 0 || lastSet > submit {
		t.Fatalf("methods %v", methods)
	}
}

func TestAPromptBehindALostTurnShowsTheDiscardBannerAndDiscardLetsItRun(t *testing.T) {
	recordPrints(t)
	g := newSessionCaseGateway(t)
	a := scriptedAgentOn(t, g.conn, gatewayAgentOptions{})
	g.mu.Lock()
	g.pending = []any{map[string]any{"admission_id": "adm-lost", "status": "unknown", "execution_generation": 4, "text": "first ask"}}
	g.mu.Unlock()
	g.restart(t)
	refused := runBounded(a, "second ask")
	if refused["failed"] != true || !strings.Contains(cliStr(refused["error"], ""), "/discard adm-lost") {
		t.Fatalf("refused %v", refused)
	}
	for _, method := range g.methods() {
		if method == "prompt.submit" {
			t.Fatal("the prompt was queued behind the lost turn")
		}
	}
	host := bareCLI()
	host.sessionID, host.gatewaySessionOpen = scriptSID, true
	host.sessionDB = gatewaycli.NewGatewaySessionStore(g.conn)
	t.Cleanup(func() { _ = host.sessionDB.CloseReader() })
	host.handleDiscardCommand("/discard adm-lost")
	resolved := false
	for _, params := range g.client.callsOf("prompt.resolve_unknown") {
		resolved = resolved || sameJSON(params, map[string]any{"session_id": scriptSID, "admission_id": "adm-lost", "execution_generation": 4})
	}
	if !resolved {
		t.Fatalf("resolve calls %v", g.client.callsOf("prompt.resolve_unknown"))
	}
	if got := runBounded(a, "second ask"); got["final_response"] != "ok" {
		t.Fatalf("result after the discard %v", got)
	}
}

func TestABareDiscardPrintsUsageAndCallsNothing(t *testing.T) {
	rec := recordPrints(t)
	g := newSessionCaseGateway(t)
	host := bareCLI()
	host.sessionID, host.gatewaySessionOpen = scriptSID, true
	host.sessionDB = gatewaycli.NewGatewaySessionStore(g.conn)
	t.Cleanup(func() { _ = host.sessionDB.CloseReader() })
	calls := len(g.methods())
	host.handleDiscardCommand("/discard")
	if got := rec.plain(); !reflect.DeepEqual(got, []string{"  Usage: /discard <admission_id>"}) {
		t.Fatalf("printed %q", got)
	}
	if len(g.methods()) != calls {
		t.Fatalf("methods %v", g.methods()[calls:])
	}
}

func sixRows() []map[string]any {
	rows := make([]map[string]any, 6)
	for i := range rows {
		role := "assistant"
		if i%2 == 0 {
			role = "user"
		}
		rows[i] = map[string]any{"role": role, "content": "m" + string(rune('0'+i))}
	}
	return rows
}

// Agent whose session.resume answers the compacted transcript once a mutation ran
func compressingAgent(t *testing.T, reply map[string]any) (*GatewayAgent, *scriptedClient) {
	t.Helper()
	client := &scriptedClient{}
	if reply != nil {
		client.mutateReplies = []any{reply}
	}
	compacted := []any{map[string]any{"role": "user", "content": "summary", "row_id": 1}, map[string]any{"role": "assistant", "content": "ok", "row_id": 2}}
	client.override = func(method string, params map[string]any) (any, bool, error) {
		if method != "session.resume" {
			return nil, false, nil
		}
		done := len(client.callsOf("session.mutate")) > 0
		client.record(method, params)
		messages := []any{}
		if done {
			messages = compacted
		}
		return map[string]any{"revision": 9, "execution_generation": 7, "messages": messages}, true, nil
	}
	a, _ := makeScriptedAgent(t, client, gatewayAgentOptions{})
	client.clearCalls()
	return a, client
}

func TestManualCompressIsTheGatewayCompressMutation(t *testing.T) {
	a, client := compressingAgent(t, nil)
	a.SetPrintFn(func(string) {})
	result, err := compressNowOnGateway(context.Background(), a, sixRows(), agent.CompressRequest{})
	if err != nil {
		t.Fatal(err)
	}
	mutate := client.lastCall("session.mutate")
	if mutate["operation"] != "compress" || !sameJSON(mutate["expected_revision"], 9) || !sameJSON(mutate["payload"], map[string]any{}) {
		t.Fatalf("mutate %v", mutate)
	}
	if !sameJSON(mutate["expected_generation"], 7) || result.Status != "compressed" {
		t.Fatalf("generation %v status %s", mutate["expected_generation"], result.Status)
	}
	if !sameJSON(result.AfterMessages, []any{map[string]any{"role": "user", "content": "summary"}, map[string]any{"role": "assistant", "content": "ok"}}) {
		t.Fatalf("after %v", result.AfterMessages)
	}
	if result.Summary == nil || result.Summary.Headline != "Compressed: 6 → 2 messages" {
		t.Fatalf("summary %+v", result.Summary)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.compressor.compressionCount != 1 {
		t.Fatalf("compression count %d", a.compressor.compressionCount)
	}
}

// Owner report of a manual compression as the gateway sends it
func ownerReport(feedback agent.ManualCompressionFeedback) map[string]any {
	var note any
	if feedback.Note != nil {
		note = *feedback.Note
	}
	return map[string]any{"noop": feedback.Noop, "aborted": feedback.Aborted, "refused_would_grow": feedback.RefusedWouldGrow,
		"fallback_used": feedback.FallbackUsed, "headline": feedback.Headline, "token_line": feedback.TokenLine, "note": note}
}

func TestCliCompressPrintsTheGatewaysNoopReportAndChangesNothing(t *testing.T) {
	failed := &agent.ManualCompressionState{LastCompressAborted: true, LastSummaryError: "provider exploded"}
	for _, tc := range []struct {
		name, command string
		flags         *agent.ManualCompressionState
		shown         string
	}{
		{"aborted", "/compress", failed, "⚠️ Compression aborted: 6 messages preserved"},
		{"aborted here", "/compress here 1", failed, "⚠️ Compression aborted: 6 messages preserved"},
		{"noop", "/compress", nil, "🗜️ No changes from compression: 6 messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			history := sixRows()
			report := agent.SummarizeManualCompression(history, history, 90, 90, tc.flags)
			a, client := compressingAgent(t, map[string]any{"session_id": scriptSID, "operation": "compress", "status": "noop", "summary": ownerReport(report)})
			rec := recordPrints(t)
			a.mu.Lock()
			a.compressor.lastPromptTokens = 229
			a.mu.Unlock()
			host := bareCLI()
			host.conversationHistory, host.agent, host.sessionID = sixRows(), a, scriptSID
			host.manualCompress(tc.command)
			out := printedText(rec)
			if !strings.Contains(out, tc.shown) || strings.Contains(out, "✅") {
				t.Fatalf("output %q", out)
			}
			if strings.Contains(out, "Reason: provider exploded") != (tc.flags != nil) {
				t.Fatalf("reason line in %q", out)
			}
			if !sameJSON(host.conversationHistory, history) {
				t.Fatalf("history %v", host.conversationHistory)
			}
			a.mu.Lock()
			count, prompt := a.compressor.compressionCount, a.compressor.lastPromptTokens
			a.mu.Unlock()
			if count != 0 || prompt != 229 {
				t.Fatalf("compression count %d last prompt tokens %d", count, prompt)
			}
			if got := len(client.callsOf("session.resume")); got != 1 {
				t.Fatalf("session.resume calls %d", got)
			}
		})
	}
}

func TestManualCompressReportsTheGatewaysCommittedSummary(t *testing.T) {
	note := "Summary generation failed; Icebath used limited fallback context and removed 4 message(s)."
	fallback := agent.ManualCompressionFeedback{FallbackUsed: true, Headline: "Compressed with fallback: 6 → 2 messages",
		TokenLine: "Approx request size: ~90 → ~30 tokens", Note: &note}
	a, _ := compressingAgent(t, map[string]any{"session_id": scriptSID, "operation": "compress", "revision": 10, "summary": ownerReport(fallback)})
	a.SetPrintFn(func(string) {})
	result, err := compressNowOnGateway(context.Background(), a, sixRows(), agent.CompressRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary == nil || !reflect.DeepEqual(ownerReport(*result.Summary), ownerReport(fallback)) {
		t.Fatalf("summary %+v", result.Summary)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(result.AfterMessages) != 2 || a.compressor.compressionCount != 1 {
		t.Fatalf("after %v count %d", result.AfterMessages, a.compressor.compressionCount)
	}
}
