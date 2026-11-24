package agent

import (
	"bytes"
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
)

func captureStatusStdout(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := statusStdout
	statusStdout = &buf
	t.Cleanup(func() { statusStdout = previous })
	return &buf
}

func TestCopyInitParamsLandEveryConstructorKeyword(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	status := func(kind string, msg string) {}
	o := AIAgentOptions{Model: "m", MaxIterations: new(7), SaveTrajectories: true, QuietMode: true, ToolProgressMode: new("new"),
		Platform: "telegram", PassSessionID: true, LogPrefixChars: new(50), ProviderSort: "price", EnabledToolsets: []string{"web"},
		MaxTokens: iptr(99), ServiceTier: "priority", SideAgent: true, UserID: "u", ChatType: "dm", GatewaySessionKey: "agent:main:telegram:dm:1",
		StatusCallback: status, ReactionCallback: func(string) {}}
	a := &AIAgent{}
	copyPassthroughParams(a, o)
	copyGatewayIdentityParams(a, o)
	copyCallbackParams(a, o)
	if a.model != "m" || a.maxIterations != 7 || !a.saveTrajectories || !a.quietMode || a.toolProgressMode != "new" || a.platform != "telegram" ||
		!a.passSessionID || a.logPrefixChars != 50 || a.providerSort != "price" || a.enabledToolsets[0] != "web" || *a.maxTokens != 99 ||
		a.serviceTier != "priority" || !a.sideAgent {
		t.Fatalf("passthrough params not copied: %+v", a)
	}
	if a.userID != "u" || a.chatType != "dm" || a.gatewaySessionKey != "agent:main:telegram:dm:1" {
		t.Fatal("gateway identity params not copied")
	}
	if a.statusCallback == nil || a.reactionCallback == nil || a.noticeCallback != nil {
		t.Fatal("callback params not copied verbatim")
	}
}

func TestFallbackChainKeepsProviderAndModelEntriesAndPrintsTheChain(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out := captureStatusStdout(t)
	a := &AIAgent{credentialPoolEntryID: "stale"}
	initFallbackChain(a, []map[string]any{{"provider": "openrouter", "model": "a"}, {"provider": "x"}, nil, {"provider": "deepseek", "model": "b"}})
	if len(a.fallbackChain) != 2 || a.fallbackIndex != 0 || a.credentialPoolEntryID != "" {
		t.Fatalf("chain %v entry %q", a.fallbackChain, a.credentialPoolEntryID)
	}
	if got := out.String(); got != "\U0001f504 Fallback chain (2 providers): a (openrouter) → b (deepseek)\n" {
		t.Fatalf("banner %q", got)
	}
	if a.fallbackChain[0]["model"] != "a" || a.fallbackChain[1]["model"] != "b" {
		t.Fatalf("chain order %v", a.fallbackChain)
	}
	out.Reset()
	initFallbackChain(a, []map[string]any{{"provider": "deepseek", "model": "b"}})
	if out.String() != "\U0001f504 Fallback model: b (deepseek)\n" {
		t.Fatalf("single banner %q", out.String())
	}
	out.Reset()
	a.quietMode = true
	initFallbackChain(a, nil)
	if out.Len() != 0 || len(a.fallbackChain) != 0 {
		t.Fatal("quiet empty chain prints nothing")
	}
}

func TestOllamaNumCtxOverrideAndCompressorClamp(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ollamaCtx := func(numCtx any) context.Context {
		return settings.WithFrozen(context.Background(), map[string]any{"model": map[string]any{"ollama_num_ctx": numCtx}})
	}
	a := &AIAgent{baseURL: "https://api.example.com/v1", model: "m", quietMode: true}
	configureOllamaNumCtx(ollamaCtx("16384"), a, 8192)
	if a.ollamaNumCtx != 16384 {
		t.Fatalf("explicit num_ctx is never capped, got %d", a.ollamaNumCtx)
	}
	configureOllamaNumCtx(ollamaCtx("lots"), a, 0)
	if a.ollamaNumCtx != 0 {
		t.Fatal("an invalid override leaves num_ctx unset on a remote endpoint")
	}
	a.baseURL = "http://127.0.0.1:1/v1"
	configureOllamaNumCtx(ollamaCtx(0), a, 0)
	if a.ollamaNumCtx != 0 {
		t.Fatal("a zero override skips detection and stays unset")
	}
	engine := newStubEngine(131072, 0.5)
	a.contextCompressor = engine
	a.ollamaNumCtx = 32768
	clampCompressorToOllamaNumCtx(a)
	if engine.ContextLength != 32768 {
		t.Fatalf("compressor window %d", engine.ContextLength)
	}
	a.ollamaNumCtx = 65536
	clampCompressorToOllamaNumCtx(a)
	if engine.ContextLength != 32768 {
		t.Fatal("a num_ctx above the window never widens it")
	}
}

var testAutoraise = map[string]any{"model": "gpt-5.5", "from": 0.50, "to": 0.85}

func TestMarkerLivesUnderIcebathHome(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	h := t.TempDir()
	ctx := home.WithOverride(context.Background(), h)
	if filepath.Dir(codexGPT55AutoraiseNoticeMarker(ctx)) != h {
		t.Fatal("marker is profile scoped")
	}
}

func TestChangedThresholdRenotifiesOnce(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := home.WithOverride(context.Background(), t.TempDir())
	recordCodexGPT55AutoraiseNotice(ctx, testAutoraise)
	if !codexGPT55AutoraiseNoticeSeen(ctx, testAutoraise) {
		t.Fatal("recorded notice reads seen")
	}
	changed := map[string]any{"model": "gpt-5.5", "from": 0.60, "to": 0.85}
	if codexGPT55AutoraiseNoticeSeen(ctx, changed) {
		t.Fatal("a changed threshold renotifies")
	}
	recordCodexGPT55AutoraiseNotice(ctx, changed)
	if !codexGPT55AutoraiseNoticeSeen(ctx, changed) || codexGPT55AutoraiseNoticeSeen(ctx, testAutoraise) {
		t.Fatal("marker moves forward")
	}
	if codexGPT55AutoraiseNoticeSeen(ctx, map[string]any{"model": "gpt-5.5"}) {
		t.Fatal("a malformed autoraise reads unseen")
	}
}

func TestCompressionSummaryShowsTheAutoraiseNoticeOncePerProfile(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out := captureStatusStdout(t)
	ctx := home.WithOverride(context.Background(), t.TempDir())
	engine := newStubEngine(272000, 0.85)
	engine.ThresholdPercent = 0.85
	var printed []string
	a := &AIAgent{contextCompressor: engine, compressionThresholdAutoraised: testAutoraise, printFn: func(s string) { printed = append(printed, s) }}
	a.compressionFeasibilityChecked = true
	cs := CompressionSettings{Threshold: 0.5, Enabled: true, AutoraiseNoticeEnabled: true}
	emitCompressionSummary(ctx, a, cs)
	if !strings.Contains(out.String(), "\U0001f4ca Context limit: 272,000 tokens (compress at 85% = 231,200)") {
		t.Fatalf("banner %q", out.String())
	}
	want := "ℹ Codex gpt-5.5 caps context at 272K, so auto-compaction was raised to 85% (from 50%) to use more of the window before summarizing.\n" +
		"  Opt back out: icebath config set compression.codex_gpt55_autoraise false"
	if a.compressionWarning != want || a.compressionFeasibilityChecked {
		t.Fatalf("warning %v", a.compressionWarning)
	}
	a.quietMode = true
	emitCompressionSummary(ctx, a, cs)
	if a.compressionWarning != nil {
		t.Fatal("second init stays silent once the marker is recorded")
	}
	cs.AutoraiseNoticeEnabled = false
	a.compressionThresholdAutoraised = map[string]any{"model": "gpt-5.4", "from": 0.5, "to": 0.85}
	emitCompressionSummary(ctx, a, cs)
	if a.compressionWarning != nil {
		t.Fatal("the display gate suppresses the banner")
	}
	if got := buildCodexGPT5AutoraiseNotice(map[string]any{"model": "openai/GPT-5.3-codex-spark", "from": 0.5, "to": 0.85}, 0); !strings.Contains(got, "caps context at 128K") {
		t.Fatalf("spark cap %q", got)
	}
	_ = printed
}

func TestSnapshotPrimaryRuntimeRecordsRouteAndCompressorState(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := home.WithOverride(context.Background(), t.TempDir())
	cc := NewContextCompressor(ctx, DefaultContextCompressorOptions("cm"))
	cc.SetContextLength(128000)
	cc.SetThresholdTokens(100000)
	a := &AIAgent{model: "m", provider: "anthropic", requestedProvider: "anthropic", apiMode: "anthropic_messages", apiKey: "k",
		clientKwargs: map[string]any{"api_key": "k"}, requestOverrides: map[string]any{"x": 1}, contextCompressor: cc,
		anthropicAPIKey: "ak", anthropicBaseURL: "https://api.anthropic.com", reasoningEchoFlag: true}
	a.setBaseURL("https://api.anthropic.com")
	snapshotPrimaryRuntime(ctx, a)
	rt := a.PrimaryRuntime()
	if rt["compressor_model"] != "cm" || rt["compressor_context_length"] != 128000 || rt["compressor_threshold_tokens"] != 100000 ||
		rt["anthropic_api_key"] != "ak" || rt["is_anthropic_oauth"] != false || rt[provider.SnapshotOwnerKey] != nil || rt["reasoning_echo_flag"] != true {
		t.Fatalf("snapshot %v", rt)
	}
	a.requestOverrides["x"] = 2
	if rt["request_overrides"].(map[string]any)["x"] != 1 {
		t.Fatal("overrides are copied")
	}
	switched := buildPrimaryRuntimeSnapshot(ctx, a, "chat_completions")
	if _, ok := switched["anthropic_api_key"]; ok || switched["reasoning_config"] != nil || switched["compressor_api_mode"] != "" {
		t.Fatalf("switch snapshot %v", switched)
	}
}

func TestStartupWarningsBecomeStickyNoticesOrWaitForTheFirstTurn(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var notices []provider.AgentNotice
	a := &AIAgent{platform: "telegram"}
	a.emitStartupWarning("  ✓ provider recovered  ")
	if !a.hasPendingStartupWarnings() || len(a.pendingStartupNotices) != 1 {
		t.Fatal("a notice raised before the callback is wired waits")
	}
	n := a.pendingStartupNotices[0]
	if n.Level != "success" || n.Kind != "sticky" || n.Key != n.ID || !strings.HasPrefix(n.Key, "startup-warning.") || len(n.Key) != len("startup-warning.")+8 {
		t.Fatalf("notice %+v", n)
	}
	a.noticeCallback = func(n provider.AgentNotice) { notices = append(notices, n) }
	a.replayStartupWarnings()
	if len(notices) != 1 || notices[0].Text != "✓ provider recovered" || a.hasPendingStartupWarnings() {
		t.Fatalf("replayed %+v", notices)
	}
	a.emitStartupWarning("memory provider unavailable")
	if len(notices) != 2 || notices[1].Level != "warn" {
		t.Fatal("a wired callback gets the notice at once")
	}
	cli := &AIAgent{platform: "cli", gatewaySessionKey: "agent:main:cli"}
	cli.emitStartupWarning("held")
	if len(cli.pendingStartupPrints) != 1 || cli.pendingStartupPrints[0] != "held" {
		t.Fatal("a gateway hosted CLI holds the print for its first turn")
	}
}

func TestCallbackRailConvertsArgumentsAndSwallowsFailures(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	type label string
	var got []string
	a := &AIAgent{}
	a.callCallback("status_callback", func(kind string, msg string) { got = append(got, kind+":"+msg) }, "_emit_status", "lifecycle", label("x"))
	a.callCallback("status_callback", func(kind string) { panic("boom") }, "_emit_status", "lifecycle")
	a.callCallback("status_callback", func(a, b string) {}, "_emit_status", "only one")
	a.callCallback("status_callback", nil, "_emit_status", "x")
	if !reflect.DeepEqual(got, []string{"lifecycle:x"}) {
		t.Fatalf("got %v", got)
	}
	var thinking []string
	a.thinkingCallback = func(s string) { thinking = append(thinking, s) }
	a.EmitWaitNotice("waiting on provider")
	if len(thinking) != 1 || a.lastActivityDesc != "waiting on provider" {
		t.Fatal("wait notice rewrites the status line and touches activity")
	}
}

func TestQuietGatesAndRetryBuffer(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var printed []string
	a := &AIAgent{platform: "cli", quietMode: true, printFn: func(s string) { printed = append(printed, s) }}
	if !a.shouldEmitQuietToolMessages() || !a.shouldStartQuietSpinner(context.Background()) {
		t.Fatal("quiet CLI with a printer emits tool messages and spinners")
	}
	a.suppressStatusOutput = true
	if a.shouldEmitQuietToolMessages() {
		t.Fatal("suppressed status output wins")
	}
	a.vprint("hidden", true, false)
	a.muteNotificationReply = true
	a.safePrint("muted", false)
	a.muteNotificationReply = false
	a.safePrint("shown", false)
	if !reflect.DeepEqual(printed, []string{"shown"}) {
		t.Fatalf("printed %v", printed)
	}
	a.bufferStatus("retrying")
	a.bufferVprint("vp")
	a.BufferDiagnosticStatus("diag")
	if len(a.retryStatusBuffer) != 3 || a.retryStatusBuffer[2].message != any(config.DiagnosticText("diag")) {
		t.Fatalf("buffer %v", a.retryStatusBuffer)
	}
	a.clearStatusBuffer()
	if len(a.retryStatusBuffer) != 0 {
		t.Fatal("recovery drops the retry chatter")
	}
	if !isUnattendedTurn(&AIAgent{platform: "cron"}) || !isUnattendedTurn(&AIAgent{delegateDepth: 1}) || isUnattendedTurn(&AIAgent{platform: "cli"}) {
		t.Fatal("unattended turn rule")
	}
}
