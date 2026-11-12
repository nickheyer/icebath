package gatewayrun

import (
	"context"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Stream delta and interim commentary callbacks the turn hands its agent
type turnStreamCallbacks struct {
	consumer        *gateway.GatewayStreamConsumer
	streamDelta     func(text any)
	interim         func(text string, alreadyStreamed bool)
	wantInterimMsgs bool
}

// Builds the stream consumer and the delta and interim callbacks the platform and streaming config allow
func (t *TurnRunner) setupStreamConsumer(ctx context.Context, platformKey string) turnStreamCallbacks {
	tc := t.ctx
	if tc.muteNotificationReply {
		return turnStreamCallbacks{}
	}
	var streamConsumer *gateway.GatewayStreamConsumer
	stts := tc.streamingTTSConsumer()
	scfg := gateway.DefaultStreamingConfig()
	if t.runner.config != nil {
		scfg = t.runner.config.Streaming
	}
	var platStreaming any
	if tc.resolveDisplaySetting != nil {
		platStreaming = tc.resolveDisplaySetting(ctx, tc.userConfig, platformKey, "streaming", nil)
	}
	held := plugins.StreamHeld(ctx)
	wantStreamDeltas := !held && !tc.scheduledHeartbeat && scfg.EnabledFor(platStreaming)
	wantInterimMessages := tc.interimAssistantMessagesEnabled && !tc.scheduledHeartbeat && !held && !finiteViewerTurn(ctx)
	if wantStreamDeltas || wantInterimMessages {
		if err := t.buildStreamConsumer(ctx, scfg, wantStreamDeltas, wantInterimMessages, &streamConsumer); err != nil {
			slog.Debug("Could not set up stream consumer: " + err.Error())
		}
	}
	var deltaSinks []func(text *string)
	if streamConsumer != nil && streamConsumer.StreamDeltasEnabled {
		sc := streamConsumer
		deltaSinks = append(deltaSinks, func(text *string) {
			if text == nil {
				sc.OnDelta(nil)
				return
			}
			sc.OnDelta(*text)
		})
	}
	if stts != nil {
		deltaSinks = append(deltaSinks, stts.OnDelta)
	}
	localViewerRoute := tc.source != nil && tc.source.Platform == gateway.PlatformLocal
	var streamDelta func(text any)
	if len(deltaSinks) > 0 || t.approvalOwner != nil {
		streamDelta = func(raw any) {
			if !tc.runStillCurrent() {
				return
			}
			var text *string
			if s, ok := raw.(string); ok {
				text = &s
			}
			delivered := len(deltaSinks) > 0
			if text != nil {
				delivered = t.publishExecution("message.delta", map[string]any{"text": *text}) || delivered
			} else if localViewerRoute {
				t.publishExecution("message.delta", map[string]any{"text": nil})
			}
			for _, sink := range deltaSinks {
				sink(text)
			}
			if text != nil && (!delivered || finiteViewerTurn(ctx)) {
				panic(noStreamConsumer{})
			}
		}
	}
	interim := func(text string, alreadyStreamed bool) {
		if !tc.runStillCurrent() {
			return
		}
		unseen := false
		if localViewerRoute && !alreadyStreamed && strings.TrimSpace(text) != "" {
			unseen = !t.publishExecution("message.interim", map[string]any{"text": text, "already_streamed": false})
		}
		if stts != nil {
			stts.OnDelta(nil)
			if !alreadyStreamed {
				stts.OnDelta(&text)
				stts.OnDelta(nil)
			}
		}
		if unseen {
			return
		}
		switch {
		case streamConsumer != nil:
			if alreadyStreamed {
				streamConsumer.OnSegmentBreak()
			} else {
				streamConsumer.OnCommentary(text)
			}
		case !alreadyStreamed && tc.statusAdapter != nil && strings.TrimSpace(text) != "":
			t.sendStatusText(text, tc.statusThreadMetadata, "interim_assistant_callback scheduling error")
		}
	}
	return turnStreamCallbacks{consumer: streamConsumer, streamDelta: streamDelta, interim: interim, wantInterimMsgs: wantInterimMessages}
}

// Creates the consumer on the delivery adapter of the source and publishes it to the stream holder
func (t *TurnRunner) buildStreamConsumer(ctx context.Context, scfg gateway.StreamingConfig, wantStreamDeltas, wantInterimMessages bool, out **gateway.GatewayStreamConsumer) error {
	tc := t.ctx
	adapter, err := t.runner.deliveryAdapterFor(ctx, tc.source)
	if err != nil || adapter == nil {
		return err
	}
	supportsIncremental := adapter.Base() == nil || adapter.Base().Traits.SupportsMessageEditing
	if native, ok := adapter.(gateway.NativeStreamingAdapter); ok && native.SupportsNativeStreaming() {
		supportsIncremental = true
	}
	consumerStreamDeltas := wantStreamDeltas && supportsIncremental
	onMissingCursor := "raise"
	if wantInterimMessages {
		onMissingCursor = "fallback"
	}
	consumerCfg, pauseTyping, err := t.runner.buildStreamConsumerConfig(tc.source, scfg, adapter, onMissingCursor)
	if err != nil {
		return err
	}
	var onNewMessage func()
	if tc.progressQueue != nil {
		queue := tc.progressQueue
		onNewMessage = func() { queue.put(progressReset{}) }
	}
	replyTo := ""
	if anchor, _, _ := t.runner.turnReplyAnchors(tc); anchor != nil {
		replyTo = *anchor
	}
	consumer := gateway.NewGatewayStreamConsumer(adapter, tc.source.ChatID, gateway.StreamConsumerOptions{
		Config: consumerCfg, Metadata: tc.statusThreadMetadata, OnNewMessage: onNewMessage,
		OnBeforeFinalize: pauseTyping, InitialReplyToID: replyTo, RunStillCurrent: tc.runStillCurrent,
	})
	tc.setStreamConsumer(consumer)
	consumer.StreamDeltasEnabled = consumerStreamDeltas
	*out = consumer
	return nil
}
