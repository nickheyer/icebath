package gateway

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

const parkedSecret = "xoxb-not-a-real-token"

type parkedRunner struct{ descriptor map[string]any }

func (r *parkedRunner) SessionRuntimeDescriptor() map[string]any { return r.descriptor }
func (r *parkedRunner) ThreadMetadataForSource(PluginMessageSource) (map[string]any, error) {
	return nil, nil
}

type parkedSend struct{ chatID, text, replyTo string }

type parkedBot struct {
	*BasePlatformAdapter
	mu     sync.Mutex
	result SendResult
	sent   []parkedSend
}

func (a *parkedBot) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *parkedBot) Disconnect(context.Context) error            { return nil }
func (a *parkedBot) GetChatInfo(context.Context, string) (map[string]any, error) {
	return map[string]any{}, nil
}
func (a *parkedBot) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (SendResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if metadata["_interim_send"] != true {
		return SendResult{}, errors.New("refusal sent without the interim marker")
	}
	a.sent = append(a.sent, parkedSend{chatID, content, replyTo})
	return a.result, nil
}

func (a *parkedBot) chats() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, s := range a.sent {
		out = append(out, s.chatID)
	}
	return out
}

func newParkedBot(result SendResult) *parkedBot {
	a := &parkedBot{result: result}
	cfg := NewPlatformConfig()
	cfg.Enabled = true
	a.BasePlatformAdapter = NewBasePlatformAdapter(cfg, PlatformSlack, a)
	a.GatewayRunner = &parkedRunner{descriptor: map[string]any{"parked_profiles": map[string]any{"beta": "test park"}}}
	return a
}

// Waits until every background task the adapter tracks has finished
func waitBackground(t *testing.T, a *BasePlatformAdapter) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		n := len(a.backgroundTasks)
		a.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("background refusal never finished")
}

func refuseOn(t *testing.T, bot *parkedBot, chatID, messageID string) {
	t.Helper()
	profile := "beta"
	source := &SessionSource{Platform: PlatformSlack, ChatID: chatID, MessageID: &messageID, rejectedRouteProfile: &profile}
	dropRejectedRoute(context.Background(), bot, source)
	waitBackground(t, bot.BasePlatformAdapter)
}

// test_parked_route_refusal_send.py test_an_unsuccessful_refusal_send_is_logged_without_text_or_raw_error
func TestAnUnsuccessfulRefusalSendIsLoggedWithoutTextOrRawError(t *testing.T) {
	logs := captureLogs(t)
	kind := "forbidden"
	bot := newParkedBot(SendResult{Success: false, Error: "invalid_auth for " + parkedSecret, ErrorKind: &kind})
	refuseOn(t, bot, "C", "m1")
	if len(bot.sent) != 1 || bot.sent[0].replyTo != "m1" {
		t.Fatalf("sent %v", bot.sent)
	}
	logged := logs.String()
	var failure string
	for _, line := range strings.Split(logged, "\n") {
		if strings.Contains(line, "not delivered") {
			failure = line
		}
	}
	if !strings.Contains(failure, "error_kind=forbidden") || !strings.Contains(failure, " to C ") || !strings.Contains(failure, "level=WARN") {
		t.Fatalf("failure line %q", failure)
	}
	if strings.Contains(logged, parkedSecret) || strings.Contains(logged, ParkedRouteNotice) || strings.Contains(logged, "was asked to resend") {
		t.Fatalf("unsafe log %q", logged)
	}
}

// test_parked_route_refusal_send.py test_a_successful_refusal_send_logs_no_failure
func TestASuccessfulRefusalSendLogsNoFailure(t *testing.T) {
	logs := captureLogs(t)
	bot := newParkedBot(SendResult{Success: true, MessageID: "n1"})
	refuseOn(t, bot, "C", "m1")
	if len(bot.sent) != 1 || strings.Contains(logs.String(), "not delivered") {
		t.Fatalf("sent %v log %q", bot.sent, logs.String())
	}
}

// test_parked_route_refusal_send.py test_a_backlog_for_a_parked_profile_tells_each_chat_once_per_window
func TestABacklogForAParkedProfileTellsEachChatOncePerWindow(t *testing.T) {
	clock := 1000.0
	old := parkedMonotonic
	parkedMonotonic = func() float64 { return clock }
	t.Cleanup(func() { parkedMonotonic = old })
	bot := newParkedBot(SendResult{Success: true})
	for range 5 {
		refuseOn(t, bot, "C", "m1")
	}
	refuseOn(t, bot, "D", "m2")
	if got := strings.Join(bot.chats(), ","); got != "C,D" {
		t.Fatalf("told %s", got)
	}
	clock += ParkedNoticeWindowS
	refuseOn(t, bot, "C", "m1")
	if got := strings.Join(bot.chats(), ","); got != "C,D,C" {
		t.Fatalf("told %s", got)
	}
}

// An unparked rejected route is only logged, a retryable ingress scope records the refusal without a notice
func TestDropRejectedRouteUnservedAndRetryableIngress(t *testing.T) {
	logs := captureLogs(t)
	bot := newParkedBot(SendResult{Success: true})
	other := "gamma"
	dropRejectedRoute(context.Background(), bot, &SessionSource{Platform: PlatformSlack, ChatID: "C", rejectedRouteProfile: &other})
	if len(bot.sent) != 0 || !strings.Contains(logs.String(), "[Slack] Dropping inbound event for C: explicit profile route targets an unserved profile") {
		t.Fatalf("unserved route %v %q", bot.sent, logs.String())
	}
	ctx, scope, closeScope := RetryableIngress(context.Background())
	beta := "beta"
	dropRejectedRoute(ctx, bot, &SessionSource{Platform: PlatformSlack, ChatID: "C", rejectedRouteProfile: &beta})
	if !scope.Refused() || len(bot.sent) != 0 || !strings.Contains(logs.String(), "routed profile 'beta' on slack is parked") {
		t.Fatalf("retryable ingress %v %v %q", scope.Refused(), bot.sent, logs.String())
	}
	closeScope()
	refuseParkedRoute(ctx, bot, &SessionSource{Platform: PlatformSlack, ChatID: "E"}, "beta", "r9")
	waitBackground(t, bot.BasePlatformAdapter)
	if len(bot.sent) != 1 || bot.sent[0].replyTo != "r9" {
		t.Fatalf("a closed scope must refuse in channel %v", bot.sent)
	}
	var refusal *state.RuntimeStoreError
	parked := NewParkedProfileRefusal("beta")
	if !errors.As(error(parked), &refusal) || refusal.Reason != "runtime_draining" || parked.Profile != "beta" {
		t.Fatalf("parked refusal %v", parked)
	}
	if (&ParkedRouteRefused{}).Error() != "The routed profile is not being served right now; retry this delivery" {
		t.Fatal("refused message")
	}
}
