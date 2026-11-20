package gatewayrun

import (
	"context"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Keyword arguments of one agent run
type runAgentParams struct {
	message                    string
	contextPrompt              string
	history                    []map[string]any
	source                     *gateway.SessionSource
	sessionID                  string
	sessionKey                 string
	runGeneration              *int
	interruptDepth             int
	eventMessageID             *string
	inboundMessageID           *string
	channelPrompt              *string
	moaConfig                  map[string]any
	persistUserMessage         any
	persistUserTimestamp       *float64
	persistUserDisplayKind     *string
	messageType                gateway.MessageType
	persistUserDisplayMetadata map[string]any
	replyExpected              *bool
	scheduledHeartbeat         bool
	titleUserMessage           *string
	sessionEnv                 *tools.Env
}

// Agent run of one turn under the profile scope of its source, the full run conversation result
func (r *GatewayRunner) runAgent(ctx context.Context, p runAgentParams) (map[string]any, error) {
	scoped, err := r.profileScopeForSource(ctx, p.source)
	if err != nil {
		return nil, err
	}
	return r.runAgentInner(scoped, p)
}

// Runs the agent for one turn with its progress, stream, tracking, interrupt and heartbeat tasks, then drains follow ups
func (r *GatewayRunner) runAgentInner(ctx context.Context, p runAgentParams) (map[string]any, error) {
	if r.getProxyURL(ctx) != "" {
		result, err := r.runAgentViaProxy(ctx, proxyRunParams{message: p.message, contextPrompt: p.contextPrompt, history: p.history,
			source: p.source, sessionID: p.sessionID, sessionKey: p.sessionKey, runGeneration: p.runGeneration,
			eventMessageID: p.eventMessageID, scheduledHeartbeat: p.scheduledHeartbeat})
		if err != nil {
			return nil, err
		}
		if captured := gateway.ExecutionResultOf(ctx); captured != nil {
			if _, present := captured.Get("result"); !present {
				captured.Set("result", pyjson.Ordered(result))
			}
		}
		return result, nil
	}
	disp, err := r.runAgentDisplaySettings(ctx, p.source)
	if err != nil {
		return nil, err
	}
	if p.scheduledHeartbeat {
		quiet := *disp
		quiet.toolProgressEnabled, quiet.thinkingEnabled, quiet.nativeSlackTaskCards, quiet.needsProgressQueue = false, false, false, false
		disp = &quiet
	}
	tc, turnRunner, cleanupAdapter, err := r.runAgentBuildTurnContext(ctx, disp, p.message, p.source, p.sessionKey, p.runGeneration, turnContextParams{
		contextPrompt: p.contextPrompt, history: p.history, sessionID: p.sessionID, interruptDepth: p.interruptDepth,
		eventMessageID: p.eventMessageID, inboundMessageID: p.inboundMessageID, channelPrompt: p.channelPrompt, moaConfig: p.moaConfig,
		titleUserMessage: p.titleUserMessage, persistUserMessage: p.persistUserMessage, persistUserTimestamp: p.persistUserTimestamp,
		persistUserDisplayKind: p.persistUserDisplayKind, replyExpected: p.replyExpected,
		persistUserDisplayMetadata: p.persistUserDisplayMetadata, scheduledHeartbeat: p.scheduledHeartbeat, sessionEnv: p.sessionEnv,
		voiceTurn: pystr.Lower(string(p.messageType)) == "voice",
	})
	if err != nil {
		return nil, err
	}
	statusThreadMetadata, err := r.runAgentBindTurnWiring(ctx, tc, turnRunner, p.source, p.eventMessageID, disp.nativeSlackTaskCards)
	if err != nil {
		return nil, err
	}
	quietTurn := p.scheduledHeartbeat || tc.muteNotificationReply
	if !quietTurn {
		r.runAgentStartStreamingTTS(ctx, tc, p.messageType, statusThreadMetadata)
	}
	taskCtx := context.WithoutCancel(ctx)
	var progressTask, logTask, notifyTask *gatewayTask
	if disp.needsProgressQueue {
		progressTask = spawnGatewayTask(taskCtx, "progress-sender", func(c context.Context) (any, error) {
			return nil, turnRunner.sendProgressMessages(c)
		})
	}
	if disp.logModeEnabled {
		logTask = spawnGatewayTask(taskCtx, "tool-log-writer", func(c context.Context) (any, error) {
			r.runAgentWriteToolLog(c, disp.logQueue)
			return nil, nil
		})
	}
	streamTask := spawnGatewayTask(taskCtx, "stream-consumer", func(c context.Context) (any, error) {
		r.runAgentStreamConsumerTask(c, tc)
		return nil, nil
	})
	trackingTask := spawnGatewayTask(taskCtx, "track-agent", func(c context.Context) (any, error) {
		r.runAgentTrackAgent(c, tc)
		return nil, nil
	})
	interruptDetected := &pythread.Event{}
	interruptMonitor := spawnGatewayTask(taskCtx, "interrupt-monitor", func(c context.Context) (any, error) {
		r.runAgentMonitorForInterrupt(c, tc, interruptDetected)
		return nil, nil
	})
	var executorTask atomic.Pointer[gatewayTask]
	if !quietTurn {
		notifyTask = spawnGatewayTask(taskCtx, "long-running-notify", func(c context.Context) (any, error) {
			return nil, r.runAgentNotifyLongRunning(c, disp, tc, &executorTask)
		})
	}
	response, followup, queued, err := func() (response, followup map[string]any, queued bool, err error) {
		defer r.runAgentCleanupTurnTasks(ctx, tc, progressTask, logTask, interruptMonitor, notifyTask, trackingTask, streamTask)
		worker, err := r.runAgentStartTurnWorker(ctx, tc, turnRunner.runSync)
		if err != nil {
			return nil, nil, false, err
		}
		executorTask.Store(worker.executorTask)
		response, err = r.runAgentAwaitTurnWorker(ctx, worker, tc, interruptDetected, interruptMonitor)
		if err != nil {
			return nil, nil, false, err
		}
		if response != nil {
			response["_notification_reply_muted"] = tc.muteNotificationReply
			if tc.capturedResult != nil {
				tc.capturedResult.Set("_notification_reply_muted", tc.muteNotificationReply)
			}
		}
		if err := r.runAgentEvictOnFallback(ctx, tc); err != nil {
			return nil, nil, false, err
		}
		result := tc.result()
		adapter, err := r.deliveryAdapterFor(ctx, p.source)
		if err != nil {
			return nil, nil, false, err
		}
		r.runAgentFinalizeStreamingTTS(tc, adapter)
		pendingEvent, pending := r.runAgentDrainPending(ctx, result, adapter, p.source, p.sessionKey)
		if pendingEvent != nil || pending != "" {
			followup, err = r.runAgentQueuedFollowup(ctx, tc, adapter, pending, pendingEvent, response, result, streamTask)
			return nil, followup, true, err
		}
		return response, nil, false, nil
	}()
	if err != nil {
		return nil, err
	}
	if queued {
		return followup, nil
	}
	r.runAgentMarkStreamedDelivery(ctx, response, tc)
	r.runAgentScheduleBubbleCleanup(response, cleanupAdapter, tc)
	return response, nil
}
