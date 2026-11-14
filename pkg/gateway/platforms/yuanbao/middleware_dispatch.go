package yuanbao

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// DispatchMiddleware builds the message event and hands it to the gateway, group turns serialised per session
type DispatchMiddleware struct{ middlewareBase }

func newDispatchMiddleware() InboundMiddleware {
	return &DispatchMiddleware{middlewareBase{"DispatchMiddleware", "dispatch"}}
}

const (
	groupQueueIdleS = 2.0
	groupQueuePollS = 0.1
)

// Gateway dispatch of a built inbound event, a package var the way hermes tests replace handle_message
var handleMessageFn = func(ctx context.Context, adapter *YuanbaoAdapter, event *gateway.MessageEvent) error {
	return adapter.HandleMessage(ctx, event)
}

// Pending dispatches of one group session in arrival order, guarded by the adapter lock
type groupQueue struct {
	items []func(ctx context.Context) error
	wake  chan struct{}
}

// Queues a dispatch for the session, returning the queue size and whether the queue was just created
func (a *YuanbaoAdapter) enqueueGroupDispatch(sessionKey string, dispatch func(ctx context.Context) error) (int, bool) {
	a.mu.Lock()
	queue, known := a.groupQueues[sessionKey]
	if !known {
		queue = &groupQueue{wake: make(chan struct{}, 1)}
		a.groupQueues[sessionKey] = queue
	}
	queue.items = append(queue.items, dispatch)
	size := len(queue.items)
	a.mu.Unlock()
	select {
	case queue.wake <- struct{}{}:
	default:
	}
	return size, !known
}

// Queue of a group session, nil when none is open
func (a *YuanbaoAdapter) groupQueueOf(sessionKey string) *groupQueue {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.groupQueues[sessionKey]
}

// Forgets the queue of a session unless a newer queue took its place
func (a *YuanbaoAdapter) dropGroupQueue(sessionKey string, queue *groupQueue) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.groupQueues[sessionKey] == queue {
		delete(a.groupQueues, sessionKey)
	}
}

// Next dispatch of the queue with the count left behind, nil once the idle queue was dropped
func (a *YuanbaoAdapter) nextGroupDispatch(ctx context.Context, sessionKey string, queue *groupQueue) (func(ctx context.Context) error, int, error) {
	idle := time.NewTimer(durationOf(groupQueueIdleS))
	defer idle.Stop()
	expired := false
	for {
		a.mu.Lock()
		if len(queue.items) > 0 {
			dispatch := queue.items[0]
			queue.items = queue.items[1:]
			remaining := len(queue.items)
			a.mu.Unlock()
			return dispatch, remaining, nil
		}
		if expired {
			if a.groupQueues[sessionKey] == queue {
				delete(a.groupQueues, sessionKey)
			}
			a.mu.Unlock()
			return nil, 0, nil
		}
		a.mu.Unlock()
		select {
		case <-queue.wake:
		case <-idle.C:
			expired = true
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
	}
}

// Tracks an inbound task until it finishes so a disconnect can cancel it
func (a *YuanbaoAdapter) trackInbound(task *gateway.AdapterTask) {
	a.mu.Lock()
	a.inboundTasks[task] = struct{}{}
	a.mu.Unlock()
	task.AddDoneCallback(func() {
		a.mu.Lock()
		delete(a.inboundTasks, task)
		a.mu.Unlock()
	})
}

// Remembers the message a session is processing and its text so a recall can find and redact it
func (a *YuanbaoAdapter) recordProcessing(sessionKey, msgID, rawText string) {
	if msgID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if sessionKey != "" {
		a.processingMsgIDs.Set(sessionKey, msgID)
		a.processingMsgTexts[sessionKey] = rawText
	}
	if rawText == "" {
		return
	}
	a.msgContentCache.Set(msgID, rawText)
	for _, oldest := range a.msgContentCache.Keys()[:max(0, a.msgContentCache.Len()-msgContentCacheN)] {
		a.msgContentCache.Delete(oldest)
	}
}

// Builds the message event of the context, records it for recall matching and hands it to the gateway
func dispatchInboundEvent(ctx context.Context, adapter *YuanbaoAdapter, ic *InboundContext, sessionKey string) error {
	msgType := gateway.MessageTypeText
	if classified, isGatewayType := ic.MsgType.(gateway.MessageType); isGatewayType {
		msgType = classified
	}
	for _, mediaType := range ic.MediaTypes {
		if strings.HasPrefix(mediaType, "application/") || strings.HasPrefix(mediaType, "text/") {
			msgType = gateway.MessageTypeDocument
			break
		}
	}
	event := gateway.NewMessageEvent(ic.RawText)
	event.MessageType, event.Source = msgType, ic.Source
	if ic.Push != nil {
		event.RawMessage = ic.Push
	}
	msgID := strOrEmpty(ic.MsgID)
	if msgID != "" {
		event.MessageID = &msgID
	}
	event.MediaURLs = append([]string{}, ic.MediaURLs...)
	event.MediaTypes = append([]string{}, ic.MediaTypes...)
	event.ReplyToMessageID, event.ReplyToText, event.ChannelPrompt = ic.ReplyToMessageID, ic.ReplyToText, ic.ChannelPrompt
	adapter.recordProcessing(sessionKey, msgID, ic.RawText)
	return handleMessageFn(ctx, adapter, event)
}

// Drains the group queue one dispatch at a time, each waiting for its session to go idle
func consumeGroupQueue(ctx context.Context, adapter *YuanbaoAdapter, sessionKey string) error {
	queue := adapter.groupQueueOf(sessionKey)
	if queue == nil {
		return nil
	}
	defer adapter.dropGroupQueue(sessionKey, queue)
	for {
		dispatch, remaining, err := adapter.nextGroupDispatch(ctx, sessionKey, queue)
		if err != nil || dispatch == nil {
			return err
		}
		slog.Debug(fmt.Sprintf("[%s] Group queue: dispatching for %s (remaining=%d)", adapter.Name(), pystr.Slice(sessionKey, 0, 50), remaining))
		err = dispatch(ctx)
		for err == nil && adapter.HasActiveSession(sessionKey) {
			err = sleep(ctx, groupQueuePollS)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			slog.Error(fmt.Sprintf("[%s] Group queue consumer error", adapter.Name()), "error", err)
		}
	}
}

// Dispatches a direct message at once and queues a group message behind the turns of its session
func (m *DispatchMiddleware) Handle(ctx context.Context, ic *InboundContext, next func() error) error {
	adapter := ic.Adapter
	sessionKey, err := adapter.SourceSessionKey(ctx, ic.Source)
	if err != nil {
		return err
	}
	dispatch := func(tctx context.Context) error { return dispatchInboundEvent(tctx, adapter, ic, sessionKey) }
	if ic.ChatType == "group" {
		size, created := adapter.enqueueGroupDispatch(sessionKey, dispatch)
		slog.Info(fmt.Sprintf("[%s] Group message enqueued (qsize=%d) for %s", adapter.Name(), size, pystr.Slice(sessionKey, 0, 50)))
		if created {
			adapter.trackInbound(gateway.SpawnAdapterTask(ctx, func(tctx context.Context) error {
				return consumeGroupQueue(tctx, adapter, sessionKey)
			}))
		}
	} else {
		adapter.trackInbound(gateway.SpawnAdapterTask(ctx, dispatch))
	}
	return next()
}
