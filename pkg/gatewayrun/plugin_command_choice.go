package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Hops a coroutine onto the gateway loop and returns its concurrent future, nil when the loop refuses it
func (r *GatewayRunner) runCoroutineThreadsafe(ctx context.Context, fn func(ctx context.Context) (any, error), logMessage string) *concurrentFuture {
	fut := newConcurrentFuture()
	taskCtx := context.WithoutCancel(ctx)
	start := func() {
		fut.setRunningOrNotifyCancel()
		task := spawnGatewayTask(taskCtx, "plugin-choice", fn)
		task.addDoneCallback(func(done *gatewayFuture) {
			result, taskErr := done.wait(context.Background())
			fut.set(result, taskErr)
		})
	}
	if r.gatewayLoop == nil {
		start()
		return fut
	}
	if err := r.gatewayLoop.CallSoonThreadsafe(start); err != nil {
		runLog().Debug(fmt.Sprintf("%s: %s", logMessage, err))
		return nil
	}
	return fut
}

// Binds the messaging asker of a plugin command unless one is bound already or the source is a local session
func (r *GatewayRunner) chatChoices(ctx context.Context, source *gateway.SessionSource, sessionKey string) context.Context {
	if plugins.AskerBound(ctx) || source.Platform == gateway.PlatformLocal {
		return ctx
	}
	return plugins.BindAsker(ctx, func(question string, labels []string) (string, bool) {
		return r.askChat(ctx, source, sessionKey, question, labels)
	})
}

// Asks a plugin choice in the chat with the clarify prompt and waits for the answer, false when none came
func (r *GatewayRunner) askChat(ctx context.Context, source *gateway.SessionSource, sessionKey, question string, labels []string) (string, bool) {
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil || adapter == nil {
		return "", false
	}
	clarifyID := newClarifyID()
	send := clarifySendArgs{chatID: source.ChatID, question: question, choices: labels, clarifyID: clarifyID,
		sessionKey: sessionKey, metadata: r.threadMetadataForSource(source, nil)}
	tools.RegisterGatewayClarify(clarifyID, sessionKey, question, labels, false)
	fallback := func() *concurrentFuture {
		coro := textFallbackCoro(adapter, send)
		if coro == nil {
			return nil
		}
		return r.runCoroutineThreadsafe(ctx, coro, "Plugin choice text fallback failed to schedule")
	}
	card := r.runCoroutineThreadsafe(ctx, func(ctx context.Context) (any, error) {
		if sender, ok := adapter.(clarifySender); ok {
			return sender.SendClarify(ctx, send.chatID, send.question, send.choices, send.clarifyID, send.sessionKey, send.metadata)
		}
		return adapter.Base().SendClarify(ctx, send.chatID, send.question, send.choices, send.clarifyID, send.sessionKey, send.metadata)
	}, "Plugin choice send failed to schedule")
	env := tools.ToolEnvFrom(ctx)
	response, answered := clarifySendThenWait(ctx, env, card, clarifyID, sessionKey, fallback)
	if answered {
		return response, true
	}
	if retirer, ok := adapter.(clarifyCardRetirer); ok {
		notice := clarifyExpiredNotice(ctx)
		r.runCoroutineThreadsafe(ctx, func(ctx context.Context) (any, error) {
			return nil, retirer.RetireClarifyCard(ctx, clarifyID, notice)
		}, "Plugin choice card retire failed to schedule")
	}
	return "", false
}
