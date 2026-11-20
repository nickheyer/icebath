package gatewayrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
)

var noiseChatPlatforms = []gateway.Platform{"telegram", "slack", "feishu"}

var noisyStatusMessages = []string{
	"🗜️ Preflight compression check before sending...",
	"📦 Pre-API compression: ~123,456 tokens near the context/output limit. Compacting before the next model call.",
	"🗜️ Compacting context — summarizing earlier conversation so I can continue...",
	"💤 Resumed after 3600s idle — compacting ~120,000 tokens before continuing.",
	"⚠️  Session compressed 12 times — accuracy may degrade. Consider /new to start fresh.",
	"⚠ Compression summary failed: upstream error. Inserted a fallback context marker.",
	"⏱️ Rate limited. Waiting 30.0s (attempt 2/3)...",
	"⏳ Retrying in 4.2s (attempt 1/3)...",
	"🗜️ Context too large (~250,000 tokens) — compressing (1/3)...",
	"🗜️ Compressed 30 → 12 messages, retrying...",
	"🗜️ Compressed ~250,000 → ~120,000 tokens, retrying...",
	"🗜️ Context reduced to 120,000 tokens (was 250,000), retrying...",
	"⚠ Compression model small (openrouter) context is 32,000 tokens, but the main model big (anthropic)'s compression threshold was 100,000 " +
		"tokens. Auto-lowered this session's threshold to 30,000 tokens so compression can run.",
	"⚠ Configured auxiliary compression provider 'openai' is unavailable — context compression will drop middle turns without a summary. Check " +
		"auxiliary.compression in config.yaml and reauthenticate that provider.",
	"⚠ Skipping concurrent compression — another path is already compressing this session. Will retry after it finishes.",
}

var visibleCompressionMessages = []string{
	"Compressed: 30 → 12 messages",
	"Compression aborted: 30 messages preserved",
	"Compressed with fallback: 30 → 12 messages",
	"No changes from compression: 30 messages",
	"⚠ Compression aborted: auth failure. No messages were dropped — conversation continues unchanged. Run /compress to retry, or /new " +
		"to start a fresh session.",
	"⚠ Compression returned an empty transcript. No session split was performed; conversation continues unchanged.",
	"⏳ Compression already in progress for this session (holder: pid=12345:tid=7:agent=1:nonce=ab). Please wait for it to finish.",
	"⏳ Compression skipped: could not acquire this session's compression lock. Another compression may still be running, or " +
		"the lock check failed — try again shortly.",
	agent.ContextOverflowBlockedWarning(85_000, 72_000, "cooldown:30"),
	agent.ContextOverflowBlockedWarning(85_000, 72_000, "ineffective"),
}

func statusCtx(t *testing.T) context.Context {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	return context.Background()
}

func TestTelegramStatusSuppressesAuxiliaryAndRetryNoise(t *testing.T) {
	ctx := statusCtx(t)
	for _, message := range []string{
		"⚠ Auxiliary title generation failed: HTTP 400: Operation contains cybersecurity risk",
		"⚠ Compression summary failed: upstream error. Inserted a fallback context marker.",
		"🗜️ Compacting context — summarizing earlier conversation so I can continue...",
		"ℹ Configured compression model 'small-model' failed (timeout). Recovered using main model — check auxiliary.compression.model in config.yaml.",
		"⏳ Retrying in 4.2s (attempt 1/3)...",
		"⏱️ Rate limited. Waiting 30.0s (attempt 2/3)...",
		"⚠️ Max retries (3) exhausted — trying fallback...",
	} {
		if got := prepareGatewayStatusMessage(ctx, gateway.PlatformTelegram, "warn", message); got != nil {
			t.Errorf("%q not suppressed: %q", message, *got)
		}
	}
}

func TestProgrammaticSurfacesKeepRawStatus(t *testing.T) {
	ctx := statusCtx(t)
	message := "⏳ Retrying in 4.2s (attempt 1/3)..."
	for _, platform := range []gateway.Platform{"local", "api_server", "webhook", "msgraph_webhook"} {
		if got := prepareGatewayStatusMessage(ctx, platform, "lifecycle", message); got == nil || *got != message {
			t.Errorf("%s: got %v", platform, got)
		}
	}
}

func TestTelegramStatusKeepsLegitimateHeartbeatMessages(t *testing.T) {
	ctx := statusCtx(t)
	for _, message := range []string{"still on it", "⏳ Working — 3 min"} {
		if got := prepareGatewayStatusMessage(ctx, gateway.PlatformTelegram, "lifecycle", message); got == nil || *got != message {
			t.Errorf("%q: got %v", message, got)
		}
	}
}

func TestAllChatGatewaysSuppressNoise(t *testing.T) {
	ctx := statusCtx(t)
	for _, platform := range noiseChatPlatforms {
		for _, message := range noisyStatusMessages {
			if got := prepareGatewayStatusMessage(ctx, platform, "warn", message); got != nil {
				t.Errorf("%s %q not suppressed", platform, message)
			}
		}
	}
}

func TestAllRoutineCompressionStatusesSuppressedFromSourceConstants(t *testing.T) {
	ctx := statusCtx(t)
	for _, platform := range noiseChatPlatforms {
		for _, message := range agent.RoutineCompressionStatusSamples() {
			if got := prepareGatewayStatusMessage(ctx, platform, "lifecycle", message); got != nil {
				t.Errorf("%s %q not suppressed", platform, message)
			}
		}
	}
}

func TestManualCompressFeedbackAndFailureNoticesStayVisible(t *testing.T) {
	ctx := statusCtx(t)
	for _, platform := range noiseChatPlatforms {
		for _, message := range visibleCompressionMessages {
			if got := prepareGatewayStatusMessage(ctx, platform, "warn", message); got == nil || *got != message {
				t.Errorf("%s %q: got %v", platform, message, got)
			}
		}
	}
}

func TestPluginPlatformStringSuppressesNoise(t *testing.T) {
	ctx := statusCtx(t)
	if got := prepareGatewayStatusMessage(ctx, "irc", "warn", "⏳ Retrying in 4.2s (attempt 1/3)..."); got != nil {
		t.Fatalf("got %q", *got)
	}
}

// Context whose gateway config sets compression.progress_notices, nil leaving the key out
func progressNoticesCtx(t *testing.T, enabled *bool) context.Context {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	body := "model: test\n"
	if enabled != nil {
		body += fmt.Sprintf("compression:\n  progress_notices: %t\n", *enabled)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return context.Background()
}

func TestHeartbeatIsCompactionProgressForTuiRetagging(t *testing.T) {
	if !agent.IsCompactionProgressStatus(agent.CompactionHeartbeatStatus) {
		t.Fatal("heartbeat not classified as compaction progress")
	}
}

func TestHeartbeatSuppressedOnChatPlatformsByDefault(t *testing.T) {
	off := false
	ctx := progressNoticesCtx(t, &off)
	for _, message := range []string{agent.CompactionStatus, agent.CompactionHeartbeatStatus} {
		if got := prepareGatewayStatusMessage(ctx, gateway.PlatformTelegram, "lifecycle", message); got != nil {
			t.Errorf("%q not suppressed", message)
		}
	}
}

func TestHeartbeatPassesWhenProgressNoticesEnabled(t *testing.T) {
	on := true
	ctx := progressNoticesCtx(t, &on)
	if got := prepareGatewayStatusMessage(ctx, gateway.PlatformTelegram, "lifecycle", agent.CompactionHeartbeatStatus); got == nil || *got != agent.CompactionHeartbeatStatus {
		t.Fatalf("got %v", got)
	}
}

var progressChatPlatforms = []gateway.Platform{"telegram", "discord", "slack", "whatsapp"}

func TestEnabledStillSuppressesNonCompressionNoise(t *testing.T) {
	on := true
	ctx := progressNoticesCtx(t, &on)
	for _, platform := range progressChatPlatforms {
		for _, message := range []string{
			"⚠ Auxiliary title generation failed: HTTP 400: Operation contains cybersecurity risk",
			"⏳ Retrying in 4.2s (attempt 1/3)...",
			"⏱️ Rate limited. Waiting 30.0s (attempt 2/3)...",
			"⚠️ Max retries (3) exhausted — trying fallback...",
			"⚠ Compression summary failed: upstream error. Inserted a fallback context marker.",
			"⚠ Configured auxiliary compression provider 'openai' is unavailable — context compression will drop middle turns without a summary. Check " +
				"auxiliary.compression in config.yaml and reauthenticate that provider.",
			"⚠ Skipping concurrent compression — another path is already compressing this session. Will retry after it finishes.",
		} {
			if got := prepareGatewayStatusMessage(ctx, platform, "warn", message); got != nil {
				t.Errorf("%s %q not suppressed", platform, message)
			}
		}
	}
}

func TestCompactionCompletionNoticeRespectsProgressNoticesGate(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		ctx := progressNoticesCtx(t, &enabled)
		for _, platform := range progressChatPlatforms {
			got := prepareGatewayStatusMessage(ctx, platform, "compacted", agent.CompactionDoneStatus)
			if enabled && (got == nil || *got != agent.CompactionDoneStatus) {
				t.Errorf("enabled %s: got %v", platform, got)
			}
			if !enabled && got != nil {
				t.Errorf("default %s: got %q", platform, *got)
			}
		}
	}
}

func TestEnabledGateDoesNotLeakToRawPlatforms(t *testing.T) {
	on := true
	ctx := progressNoticesCtx(t, &on)
	message := agent.RoutineCompressionStatusSamples()[0]
	for _, platform := range []gateway.Platform{"local", "api_server", "webhook", "msgraph_webhook"} {
		if got := prepareGatewayStatusMessage(ctx, platform, "lifecycle", message); got == nil || *got != message {
			t.Errorf("%s: got %v", platform, got)
		}
	}
}

func TestProgressRegexCoversEveryRoutineSample(t *testing.T) {
	for _, message := range agent.RoutineCompressionStatusSamples() {
		if ok, _ := compressionProgressStatusRe.MatchString(message); !ok {
			t.Errorf("routine compression sample not covered by the opt-in gate: %q", message)
		}
	}
}
