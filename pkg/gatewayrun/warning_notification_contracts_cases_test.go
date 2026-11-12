package gatewayrun

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Status sends the adapter took within the wait, a short settle when none are expected
func warningSends(adapter *timeoutNoticeAdapter, want bool) []string {
	deadline := time.Now().Add(300 * time.Millisecond)
	if want {
		deadline = time.Now().Add(5 * time.Second)
	}
	for time.Now().Before(deadline) {
		adapter.mu.Lock()
		n := len(adapter.sends)
		adapter.mu.Unlock()
		if want && n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return append([]string{}, adapter.sends...)
}

func TestRealRetryProducersKeepFinalFailureAndPersistence(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		h := t.TempDir()
		t.Setenv("ICEBATH_HOME", h)
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(fmt.Sprintf("display: {suppress_warning_notifications: %v}", !enabled)), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		adapter := newTimeoutNoticeAdapter(utf8.RuneCountInString)
		source := &gateway.SessionSource{Platform: gateway.PlatformSlack, ChatID: "D1", UserID: new("U1")}
		loop := gateway.NewCallbackLoop()
		go func() { _ = loop.Run(ctx) }()
		tc := newTurnContext()
		tc.source, tc.userConfig, tc.runStillCurrent = source, gateway.LoadGatewayConfigMap(ctx, ""), func() bool { return true }
		tc.statusAdapter, tc.statusChatID, tc.statusThreadMetadata, tc.loopForStep = adapter, "D1", map[string]any{}, loop
		runner := &GatewayRunner{}
		turnRunner := &TurnRunner{runner: runner, ctx: tc, turnCtx: ctx}
		a, err := agent.NewAIAgent(ctx, agent.AIAgentOptions{Provider: "nous", Model: "model", APIKey: "k", BaseURL: "https://example.invalid/v1",
			QuietMode: true, SkipContextFiles: true, SkipMemory: true, EnabledToolsets: []string{}, Cwd: h})
		if err != nil {
			t.Fatal(err)
		}
		var logs bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
		a.SetSuppressStatusOutput(true)
		a.SetStatusCallback(turnRunner.statusCallbackSync)
		a.SetEmptyStreakCostUSD(big.NewRat(125, 100))
		final, messages := a.TerminalEmptyTurn([]map[string]any{}, "stop")
		if sent := warningSends(adapter, enabled); (len(sent) > 0) != enabled {
			t.Fatalf("enabled %v: sent %q", enabled, sent)
		}
		if messages[len(messages)-1]["_empty_terminal_sentinel"] != true {
			t.Fatalf("enabled %v: rows %v", enabled, messages)
		}
		if !strings.Contains(logs.String(), "Empty response") {
			t.Fatalf("enabled %v: log %q", enabled, logs.String())
		}
		result := map[string]any{"final_response": final, "messages": messages, "failed": true}
		response, silent, _, err := runner.hmwaShapeAgentResponse(ctx, result, source, []map[string]any{}, &gateway.SessionEntry{SessionID: "session"},
			"", "", 0, "session", "slack", time.Now(), nil, nil)
		if err != nil || response == "" || response == "(empty)" || silent {
			t.Fatalf("enabled %v: shaped %q silent %v err %v", enabled, response, silent, err)
		}
		adapter.mu.Lock()
		adapter.sends = nil
		adapter.mu.Unlock()
		guard := &provider.NousRateGuard{Home: h}
		if err := guard.Record(http.Header{}, time.Now().Add(60*time.Second), 60*time.Second); err != nil {
			t.Fatal(err)
		}
		action, guardResult := a.NousRateLimitGuardTurn(ctx)
		if sent := warningSends(adapter, enabled); (len(sent) > 0) != enabled {
			t.Fatalf("enabled %v: guard sent %q", enabled, sent)
		}
		text, _ := guardResult["final_response"].(string)
		if action != "return" || guardResult["failed"] != true || a.SessionMessages() == nil || !strings.Contains(text, "/retry") {
			t.Fatalf("enabled %v: guard %s %v persisted %v", enabled, action, guardResult, a.SessionMessages() != nil)
		}
		if a.RetryStatusBufferLen() != 0 {
			t.Fatalf("enabled %v: %d status lines left buffered", enabled, a.RetryStatusBufferLen())
		}
		slog.SetDefault(prev)
		loop.Close()
		a.Close()
	}
}
