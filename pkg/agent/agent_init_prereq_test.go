package agent

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
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
	status := func(kind string, msg string) {}
	o := AIAgentOptions{Model: "m", MaxIterations: 7, SaveTrajectories: true, QuietMode: true, ToolProgressMode: "new",
		Platform: "telegram", PassSessionID: true, LogPrefixChars: 50, ProviderSort: "price", EnabledToolsets: []string{"web"},
		MaxTokens: 99, ServiceTier: "priority", SideAgent: true, UserID: "u", ChatType: "dm", GatewaySessionKey: "agent:main:telegram:dm:1",
		StatusCallback: status, ReactionCallback: func(string) {}}
	a := &AIAgent{}
	copyPassthroughParams(a, o)
	copyGatewayIdentityParams(a, o)
	copyCallbackParams(a, o)
	if a.model != "m" || a.maxIterations != 7 || !a.saveTrajectories || !a.quietMode || a.toolProgressMode != "new" || a.platform != "telegram" ||
		!a.passSessionID || a.logPrefixChars != 50 || a.providerSort != "price" || a.enabledToolsets[0] != "web" || a.maxTokens != 99 ||
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
	out := captureStatusStdout(t)
	a := &AIAgent{credentialPoolEntryID: "stale"}
	initFallbackChain(a, []map[string]any{{"provider": "openrouter", "model": "a"}, {"provider": "x"}, nil, {"provider": "nous", "model": "b"}})
	if len(a.fallbackChain) != 2 || a.fallbackIndex != 0 || a.credentialPoolEntryID != "" {
		t.Fatalf("chain %v entry %q", a.fallbackChain, a.credentialPoolEntryID)
	}
	if got := out.String(); got != "\U0001f504 Fallback chain (2 providers): a (openrouter) → b (nous)\n" {
		t.Fatalf("banner %q", got)
	}
	if a.FallbackModel()["model"] != "a" {
		t.Fatal("legacy fallback model is the first entry")
	}
	out.Reset()
	initFallbackChain(a, []map[string]any{{"provider": "nous", "model": "b"}})
	if out.String() != "\U0001f504 Fallback model: b (nous)\n" {
		t.Fatalf("single banner %q", out.String())
	}
	out.Reset()
	a.quietMode = true
	initFallbackChain(a, nil)
	if out.Len() != 0 || a.FallbackModel() != nil {
		t.Fatal("quiet empty chain prints nothing")
	}
}

func TestOllamaNumCtxOverrideAndCompressorClamp(t *testing.T) {
	a := &AIAgent{baseURL: "https://api.example.com/v1", model: "m", quietMode: true}
	configureOllamaNumCtx(context.Background(), a, map[string]any{"ollama_num_ctx": "16384"}, 8192)
	if a.ollamaNumCtx != 16384 {
		t.Fatalf("explicit num_ctx is never capped, got %d", a.ollamaNumCtx)
	}
	configureOllamaNumCtx(context.Background(), a, map[string]any{"ollama_num_ctx": "lots"}, 0)
	if a.ollamaNumCtx != 0 {
		t.Fatal("an invalid override leaves num_ctx unset on a remote endpoint")
	}
	a.baseURL = "http://127.0.0.1:1/v1"
	configureOllamaNumCtx(context.Background(), a, map[string]any{"ollama_num_ctx": 0}, 0)
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

func TestAutoraiseNoticeMarkerLivesUnderIcebathHome(t *testing.T) {
	h := t.TempDir()
	ctx := home.WithOverride(context.Background(), h)
	if filepath.Dir(codexGPT55AutoraiseNoticeMarker(ctx)) != h {
		t.Fatal("marker is profile scoped")
	}
}

func TestChangedThresholdRenotifiesOnce(t *testing.T) {
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
	ctx := home.WithOverride(context.Background(), t.TempDir())
	cc := NewContextCompressor(ctx, DefaultContextCompressorOptions("cm"))
	cc.Base().ContextLength, cc.Base().ThresholdTokens = 128000, 100000
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

func TestReadReasoningEchoFromConfig(t *testing.T) {
	h := t.TempDir()
	ctx := home.WithOverride(context.Background(), h)
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("model:\n  reasoning_echo: true\n")
	if !readReasoningEchoFromConfig(ctx) {
		t.Fatal("opt in reads true")
	}
	write("model: gpt-5\n")
	if readReasoningEchoFromConfig(ctx) {
		t.Fatal("a string model section reads false")
	}
}

func TestStartupWarningsBecomeStickyNoticesOrWaitForTheFirstTurn(t *testing.T) {
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
	var printed []string
	a := &AIAgent{platform: "cli", quietMode: true, printFn: func(s string) { printed = append(printed, s) }}
	if !a.shouldEmitQuietToolMessages() || !a.shouldStartQuietSpinner() {
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

type hostedEngineSecrets struct{}

func (hostedEngineSecrets) ManagedDotenvKeys() []string   { return nil }
func (hostedEngineSecrets) SourceSuppliedNames() []string { return nil }
func (hostedEngineSecrets) BuildProfileSecretScope(string) map[string]string {
	return map[string]string{}
}
func (hostedEngineSecrets) CurrentSecretScope(context.Context) map[string]string {
	return map[string]string{}
}

const hostedEngineSrc = `
from agent.context_engine import ContextEngine
class Hosted(ContextEngine):
    @property
    def name(self):
        return "hosted_demo"
    def update_from_response(self, usage):
        self.last_prompt_tokens = usage.get("prompt_tokens", 0)
    def should_compress(self, prompt_tokens=None):
        return (prompt_tokens or self.last_prompt_tokens) >= self.threshold_tokens
    def compress(self, messages, current_tokens=None):
        self.compression_count += 1
        return messages[-1:]
    def get_status(self):
        return {"threshold_tokens": self.threshold_tokens}
def register(ctx):
    ctx.register_context_engine(Hosted())
`

func TestHostedContextEngineCallsThroughAndMirrorsItsBase(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is required to run the plugin host child: %v", err)
	}
	h := filepath.Join(t.TempDir(), ".icebath")
	for rel, body := range map[string]string{"config.yaml": "plugins:\n  isolation: host\n  enabled: []\n", "plugins/hosted_demo/__init__.py": hostedEngineSrc} {
		path := filepath.Join(h, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", h)
	bundled, table, adapters, secrets := tools.BundledPlatformSecretEnvs, tools.ConfigEnvTableKeys, tools.RegisteredAdapterSecretEnv, tools.ProfileSecrets
	tools.ProfileSecrets = hostedEngineSecrets{}
	tools.BundledPlatformSecretEnvs = func() []string { return nil }
	tools.ConfigEnvTableKeys = func() []string { return nil }
	tools.RegisteredAdapterSecretEnv = func(*tools.Env) map[string]bool { return map[string]bool{} }
	t.Cleanup(func() {
		tools.BundledPlatformSecretEnvs, tools.ConfigEnvTableKeys, tools.RegisteredAdapterSecretEnv, tools.ProfileSecrets = bundled, table, adapters, secrets
	})
	t.Cleanup(plugins.SetHostInterpreterForTests(python))
	plugins.ResetPluginManagersForTests()
	t.Cleanup(plugins.ResetPluginManagersForTests)
	ctx := home.WithOverride(context.Background(), h)
	obj := plugins.LoadContextEngine(ctx, "hosted_demo")
	if obj == nil {
		t.Fatal("hosted engine did not load")
	}
	e, err := newHostedContextEngine(ctx, obj)
	if err != nil || e.Name() != "hosted_demo" {
		t.Fatalf("adapter %v %v", e, err)
	}
	e.UpdateModel("m", 100000, "", "", "", "")
	if b := e.Base(); b.ContextLength != 100000 || b.ThresholdTokens != 75000 {
		t.Fatalf("update_model mirrored %+v", *b)
	}
	e.UpdateFromResponse(map[string]any{"prompt_tokens": 80000})
	if e.Base().LastPromptTokens != 80000 || !e.ShouldCompress(nil) {
		t.Fatal("hosted usage drives should_compress")
	}
	e.Base().ThresholdTokens = 90000
	if e.ShouldCompress(nil) || e.GetStatus()["threshold_tokens"] != int64(90000) {
		t.Fatal("a host side base edit reaches the hosted engine")
	}
	out, err := e.Compress([]map[string]any{{"role": "user", "content": "a"}, {"role": "user", "content": "b"}}, nil, nil, true, "mem")
	if err != nil || len(out) != 1 || out[0]["content"] != "b" || e.Base().CompressionCount != 1 {
		t.Fatalf("compress %v %v", out, err)
	}
	if e.AcceptsCompressKeyword("force") || !e.AcceptsCompressKeyword("current_tokens") {
		t.Fatal("compress keywords follow the hosted signature")
	}
	if got := e.GetAutomaticCompactionStatusMessage("preflight", "compacting", nil); got == nil || *got != "compacting" {
		t.Fatal("base status hook answers the default")
	}
	clone := e.CloneForAgent()
	clone.UpdateModel("m", 50000, "", "", "", "")
	if clone.Base().ContextLength != 50000 || e.Base().ContextLength != 100000 {
		t.Fatal("a clone is its own engine")
	}
	if v, ok := engineAttr(e, "threshold_percent"); !ok || v != 0.75 {
		t.Fatalf("engine attr %v", v)
	}
}
