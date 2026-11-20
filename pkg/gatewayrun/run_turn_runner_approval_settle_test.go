package gatewayrun

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Adapter recording edits and sends with a configurable message limit and length measure
type timeoutNoticeAdapter struct {
	*gateway.BasePlatformAdapter
	mu       sync.Mutex
	calls    []string
	edits    []string
	sends    []string
	measure  func(string) int
	limitErr error
}

func (a *timeoutNoticeAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *timeoutNoticeAdapter) Disconnect(context.Context) error            { return nil }
func (a *timeoutNoticeAdapter) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"id": chatID}, nil
}
func (a *timeoutNoticeAdapter) Send(_ context.Context, _, content, _ string, _ map[string]any) (gateway.SendResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, "send")
	a.sends = append(a.sends, content)
	return gateway.SendResult{Success: true}, nil
}
func (a *timeoutNoticeAdapter) EditMessage(_ context.Context, _, messageID, content string, _ bool) (gateway.SendResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, "edit:"+messageID)
	a.edits = append(a.edits, content)
	return gateway.SendResult{Success: true}, nil
}
func (a *timeoutNoticeAdapter) MaxMessageLengthForChat(string) (int, error) {
	if a.limitErr != nil {
		return 0, a.limitErr
	}
	return 4096, nil
}
func (a *timeoutNoticeAdapter) MessageLenFnForChat(string) (func(string) int, error) {
	return a.measure, nil
}

func newTimeoutNoticeAdapter(measure func(string) int) *timeoutNoticeAdapter {
	a := &timeoutNoticeAdapter{measure: measure}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(gateway.NewPlatformConfig(), gateway.PlatformTelegram, a)
	return a
}

func timeoutNoticeCtx(t *testing.T, adapter gateway.PlatformAdapter) (context.Context, *TurnContext) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tc := newTurnContext()
	tc.statusAdapter, tc.statusChatID = adapter, "c"
	return context.Background(), tc
}

func TestTextPromptIsNeverEditedInPlace(t *testing.T) {
	adapter := newTimeoutNoticeAdapter(utf8.RuneCountInString)
	ctx, tc := timeoutNoticeCtx(t, adapter)
	postTimeoutNotice(ctx, tc, "ls", nil, 300, "")
	if strings.Join(adapter.calls, ",") != "send" {
		t.Fatalf("calls %v", adapter.calls)
	}
	card := "card-9"
	postTimeoutNotice(ctx, tc, "ls", &card, 300, "")
	if adapter.calls[len(adapter.calls)-1] != "edit:card-9" {
		t.Fatalf("calls %v", adapter.calls)
	}
}

func TestCardBodyFillsTheMessageInThePlatformsOwnUnits(t *testing.T) {
	for _, command := range []string{"echo " + strings.Repeat("x", 5000), "echo " + strings.Repeat("\U0001F600", 3000)} {
		adapter := newTimeoutNoticeAdapter(gateway.UTF16Len)
		ctx, tc := timeoutNoticeCtx(t, adapter)
		card := "card-9"
		postTimeoutNotice(ctx, tc, command, &card, 300, "")
		if len(adapter.edits) != 1 {
			t.Fatalf("edits %d", len(adapter.edits))
		}
		content := adapter.edits[0]
		n := gateway.UTF16Len(content)
		if strings.Contains(command, "\U0001F600") {
			if n != 4095 && n != 4096 {
				t.Errorf("astral body %d units", n)
			}
		} else if n != 4096 {
			t.Errorf("ascii body %d units", n)
		}
		if !strings.HasSuffix(content, "...\n```") {
			t.Errorf("body ends %q", content[len(content)-12:])
		}
	}
}

func TestCardBodyFailureStillSendsTheNotice(t *testing.T) {
	adapter := newTimeoutNoticeAdapter(utf8.RuneCountInString)
	adapter.limitErr = errors.New("limit lookup broke")
	ctx, tc := timeoutNoticeCtx(t, adapter)
	card := "card-9"
	postTimeoutNotice(ctx, tc, "ls", &card, 300, "")
	if strings.Join(adapter.calls, ",") != "send" || adapter.sends[0] != gateway.FormatApprovalTimedOutNotice(ctx, 300) {
		t.Fatalf("calls %v sends %v", adapter.calls, adapter.sends)
	}
}
