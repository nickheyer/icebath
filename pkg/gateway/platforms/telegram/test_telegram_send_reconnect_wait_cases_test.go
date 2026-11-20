package telegram

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
)

// Runner double exposing a mutable live adapter table
type liveRunner struct {
	mu       sync.Mutex
	adapters *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]
}

func (r *liveRunner) Adapters() *pydict.Dict[gateway.Platform, gateway.PlatformAdapter] {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.adapters
}

// Disconnected adapter with rich sends off and a short reconnect wait
func newReconnectAdapter(t *testing.T) *TelegramAdapter {
	a := newTestAdapter(t, nil)
	a.richSendDisabled.Store(true)
	a.reconnectWaitSeconds, a.reconnectPollInterval = 0.6, 0.05
	return a
}

func TestSendWaitsAndSucceedsWhenBotReturns(t *testing.T) {
	a := newReconnectAdapter(t)
	fake := &scriptedBot{}
	time.AfterFunc(120*time.Millisecond, func() { attachScriptedBot(t, a, fake) })
	result, err := a.Send(context.Background(), "123", "hello", "", nil)
	if err != nil || !result.Success || result.MessageID != "42" {
		t.Fatalf("result %+v err %v", result, err)
	}
	if len(fake.callsOf("sendMessage")) == 0 {
		t.Fatal("sendMessage never reached the bot")
	}
}

func TestSendDelegatesToReplacementAdapterInstalledMidWait(t *testing.T) {
	old := newReconnectAdapter(t)
	live, fake := newScriptedAdapter(t, nil)
	live.richSendDisabled.Store(true)
	live.reconnectWaitSeconds = 0.01
	runner := &liveRunner{adapters: pydict.New[gateway.Platform, gateway.PlatformAdapter]()}
	old.GatewayRunner = runner
	time.AfterFunc(120*time.Millisecond, func() {
		runner.mu.Lock()
		runner.adapters.Set(old.Platform, live)
		runner.mu.Unlock()
	})
	result, err := old.Send(context.Background(), "123", "hello", "", nil)
	if err != nil || !result.Success || result.MessageID != "42" {
		t.Fatalf("result %+v err %v", result, err)
	}
	if len(fake.callsOf("sendMessage")) == 0 {
		t.Fatal("the replacement adapter did not send")
	}
}

func TestSendDelegatesImmediatelyWhenReplacementAlreadyLive(t *testing.T) {
	old := newReconnectAdapter(t)
	old.reconnectWaitSeconds = 30
	live, fake := newScriptedAdapter(t, nil)
	live.richSendDisabled.Store(true)
	runner := &liveRunner{adapters: pydict.New[gateway.Platform, gateway.PlatformAdapter]()}
	runner.adapters.Set(old.Platform, live)
	old.GatewayRunner = runner
	started := time.Now()
	result, err := old.Send(context.Background(), "123", "hello", "", nil)
	if err != nil || !result.Success {
		t.Fatalf("result %+v err %v", result, err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("must not wait when the replacement is already live")
	}
	if len(fake.callsOf("sendMessage")) == 0 {
		t.Fatal("the replacement adapter did not send")
	}
}

func TestSendTimeoutIsRetryableNotConnected(t *testing.T) {
	a := newReconnectAdapter(t)
	a.reconnectWaitSeconds, a.reconnectPollInterval = 0.15, 0.05
	result, err := a.Send(context.Background(), "123", "hello", "", nil)
	if err != nil || result.Success || result.Error != "Not connected" || !result.Retryable {
		t.Fatalf("result %+v err %v", result, err)
	}
}

func TestSendPermanentFatalFailsImmediatelyWithoutWait(t *testing.T) {
	a := newReconnectAdapter(t)
	a.reconnectWaitSeconds = 30
	a.SetFatalError(context.Background(), "telegram_auth_error", "invalid token", false)
	started := time.Now()
	result, err := a.Send(context.Background(), "123", "hello", "", nil)
	if err != nil || result.Success || result.Error != "Not connected" || result.Retryable {
		t.Fatalf("result %+v err %v", result, err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("must not wait on a permanent fatal error")
	}
}
