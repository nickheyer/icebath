package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/media"
)

var toolCallLoggerMu sync.Mutex

// Process wide tool call logger with one size rotated redacting handler on logs tool_calls.log
func toolCallLogger(ctx context.Context) *logging.Logger {
	toolLogger := logging.GetLogger("hermes.tool_calls")
	toolCallLoggerMu.Lock()
	defer toolCallLoggerMu.Unlock()
	if len(toolLogger.Handlers()) == 0 {
		logDir := filepath.Join(home.Dir(ctx), "logs")
		if err := os.MkdirAll(logDir, 0o755); err != nil {
			runLog().Error(fmt.Sprintf("write_tool_log error: %s", err))
			return toolLogger
		}
		handler, err := logging.NewRedactingRotatingFileHandler(ctx, filepath.Join(logDir, "tool_calls.log"), 5*1024*1024, 3, "%(message)s")
		if err != nil {
			runLog().Error(fmt.Sprintf("write_tool_log error: %s", err))
			return toolLogger
		}
		toolLogger.SetLevel(logging.LevelInfo)
		toolLogger.SetPropagate(false)
		toolLogger.AddHandler(handler)
	}
	return toolLogger
}

// Drains the log queue into tool_calls.log until cancelled, then drains what the last iteration queued
func (r *GatewayRunner) runAgentWriteToolLog(ctx context.Context, logQueue *turnQueue) {
	if logQueue == nil {
		return
	}
	toolLogger := toolCallLogger(ctx)
	defer func() {
		for {
			item, ok := logQueue.getNowait()
			if !ok {
				return
			}
			toolLogger.Info(fmt.Sprint(item))
		}
	}()
	for {
		if item, ok := logQueue.getNowait(); ok {
			toolLogger.Info(fmt.Sprint(item))
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// Starts the streaming TTS consumer of a voice input turn on an auto TTS chat, leaving the holder empty when inactive
func (r *GatewayRunner) runAgentStartStreamingTTS(ctx context.Context, tc *TurnContext, messageType gateway.MessageType, statusThreadMetadata map[string]any) {
	adapter, _ := r.deliveryAdapterFor(ctx, tc.source)
	isVoice := strings.ToLower(string(messageType)) == "voice"
	if adapter == nil || !isVoice || !gateway.ShouldAutoTTSForChat(adapter, tc.source.ChatID) {
		return
	}
	consumer, err := gateway.NewStreamingTTSConsumer(ctx, tc.sessionEnv, adapter, tc.source.ChatID, media.LoadTTSConfig(ctx, tc.sessionEnv), statusThreadMetadata, nil)
	if err != nil {
		runLog().Debug(fmt.Sprintf("Could not set up streaming TTS consumer: %s", err))
		return
	}
	if consumer.Active() {
		tc.setStreamingTTSConsumer(consumer)
		consumer.Start()
	}
}

// Waits up to ten seconds for run_sync to create the stream consumer, then runs it
func (r *GatewayRunner) runAgentStreamConsumerTask(ctx context.Context, tc *TurnContext) {
	for range 200 {
		if consumer := tc.streamConsumer(); consumer != nil {
			consumer.Run(ctx)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Gives the stream consumer task five seconds to flush, then cancels it
func awaitStreamTask(ctx context.Context, task *gatewayTask) {
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := task.wait(waitCtx); err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errTaskCancelled) || errors.Is(err, context.Canceled)) {
		task.Cancel()
		_, _ = task.wait(context.Background())
	}
}

// Records the turn's agent as running for its session once created, unless a newer run took the slot
func (r *GatewayRunner) runAgentTrackAgent(ctx context.Context, tc *TurnContext) {
	for tc.currentAgent() == nil {
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	if tc.sessionKey == "" {
		return
	}
	promoted, stale := tc.currentAgent(), false
	r.withSessionState(tc.sessionKey, func(s *SessionState) {
		if tc.runGeneration != nil && s.persistent.runGeneration != *tc.runGeneration {
			stale = true
			return
		}
		s.turn.agent, s.turn.ctx = promoted, tc
	})
	if stale {
		runLog().Info(fmt.Sprintf("Skipping stale agent promotion for %s — generation %d is no longer current", tc.sessionKey, *tc.runGeneration))
		return
	}
	if r.draining.Load() {
		draining := "draining"
		r.updateRuntimeStatus(ctx, &draining, nil)
	}
}

// Stand in text of a media only event, one line per attachment by kind
func mediaOnlyEventText(event *gateway.MessageEvent) string {
	parts := []string{}
	for i, url := range event.MediaURLs {
		switch {
		case eventMediaIsImage(event, i):
			parts = append(parts, "[User sent an image: "+url+"]")
		case eventMediaIsAudio(event, i):
			parts = append(parts, "[User sent audio: "+url+"]")
		case eventMediaIsVideo(event, i):
			parts = append(parts, "[User sent a video: "+url+"]")
		default:
			parts = append(parts, "[User sent a file: "+url+"]")
		}
	}
	return strings.Join(parts, "\n")
}

// Peeks the pending event, transcribes its voice, interrupts the agent and aborts streaming TTS
func (r *GatewayRunner) runAgentFirePendingInterrupt(ctx context.Context, adapter gateway.PlatformAdapter, a *agent.AIAgent, tc *TurnContext,
	interruptDetected *pythread.Event, logContext string, log func()) {
	source := tc.source
	peek, _ := adapter.Base().PeekPendingMessage(tc.sessionKey)
	pendingText := ""
	if peek != nil {
		pendingText = peek.Text
		if len(pendingEventAudioPaths(peek)) > 0 {
			var metadata map[string]any
			if source.ThreadID != nil && *source.ThreadID != "" {
				metadata = map[string]any{"thread_id": *source.ThreadID}
			}
			pendingText, _ = r.transcribeAndEchoPendingVoice(ctx, peek, adapter, source, pendingText, logContext, metadata, true)
		} else if pendingText == "" && len(peek.MediaURLs) > 0 {
			pendingText = mediaOnlyEventText(peek)
		}
	}
	log()
	a.Interrupt(pendingText)
	interruptDetected.Set()
	if consumer := tc.streamingTTSConsumer(); consumer != nil {
		consumer.Abort("barge-in")
	}
}
