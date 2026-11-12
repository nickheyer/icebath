package slack

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

const (
	unfurlChannel    = "C0BF1EYUA9H"
	unfurlTeam       = "T025KND0E"
	unfurlOriginalTS = "1787365409.908499"
	unfurlEventTS    = "1787365411.012100"
	unfurlUser       = "U0374GH838U"
)

// Harness adapter of the unfurl cases with the bot id the events mention
func unfurlAdapter(t *testing.T) (*SlackAdapter, *capturedEvents) {
	t.Helper()
	a, _, delivered := newHarnessAdapter(t, nil)
	bot := "U0BCLP7DB7B"
	a.botUserID = &bot
	return a, delivered
}

// Replaces the display name lookup for the case
func patchResolveUserName(t *testing.T, fn func(*SlackAdapter, context.Context, string, string, string) string) {
	t.Helper()
	prev := resolveUserNameFn
	resolveUserNameFn = fn
	t.Cleanup(func() { resolveUserNameFn = prev })
}

func unfurlOriginalEvent() map[string]any {
	return map[string]any{"type": "message", "user": unfurlUser, "text": "<@U0BCLP7DB7B> see https://example.com/a and https://example.com/b",
		"channel": unfurlChannel, "channel_type": "channel", "ts": unfurlOriginalTS, "team": unfurlTeam, "client_msg_id": "cmid-1"}
}

func unfurlEditEvent() map[string]any {
	return map[string]any{"type": "message", "subtype": "message_changed", "channel": unfurlChannel, "channel_type": "channel",
		"team": unfurlTeam, "ts": unfurlEventTS, "event_ts": unfurlEventTS,
		"message": map[string]any{"type": "message", "user": unfurlUser, "text": "<@U0BCLP7DB7B> see https://example.com/a and https://example.com/b",
			"ts": unfurlOriginalTS, "client_msg_id": "cmid-1", "attachments": []any{map[string]any{"title": "unfurled"}}}}
}

func unfurlBody() map[string]any {
	return map[string]any{"team_id": unfurlTeam, "event_id": "Ev0BRUTU4GP7"}
}

// User edit of the original message
func unfurlUserEdit() map[string]any {
	edit := unfurlEditEvent()
	msg := edit["message"].(map[string]any)
	msg["text"] = "<@U0BCLP7DB7B> edited text"
	msg["edited"] = map[string]any{"user": unfurlUser, "ts": "1787365412.000000"}
	return edit
}

// Runs the handler and turns a raised panic into its value, the way pytest.raises catches the exception
func handleCatching(a *SlackAdapter, event map[string]any) (raised any, err error) {
	defer func() { raised = recover() }()
	return nil, a.handleSlackMessage(context.Background(), event, unfurlBody())
}

func richard(*SlackAdapter, context.Context, string, string, string) string { return "richard" }

// test_slack_unfurl_duplicate_turn.py TestUnfurlDuringInflightMessage.test_unfurl_arriving_mid_flight_does_not_create_second_turn
func TestUnfurlDuringInflightMessageUnfurlArrivingMidFlightDoesNotCreateSecondTurn(t *testing.T) {
	a, delivered := unfurlAdapter(t)
	release, entered := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	patchResolveUserName(t, func(*SlackAdapter, context.Context, string, string, string) string {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return "richard"
	})
	first := make(chan error, 1)
	go func() { first <- a.handleSlackMessage(context.Background(), unfurlOriginalEvent(), unfurlBody()) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the first copy never reached enrichment")
	}
	second := make(chan error, 1)
	go func() { second <- a.handleSlackMessage(context.Background(), unfurlEditEvent(), unfurlBody()) }()
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the unfurl blocked behind the first copy")
	}
	close(release)
	select {
	case err := <-first:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first copy never finished")
	}
	if n := len(delivered.all()); n != 1 {
		t.Fatalf("unfurl became a duplicate user turn (%d deliveries); this is the spurious-interrupt bug", n)
	}
}

// test_slack_unfurl_duplicate_turn.py TestUnfurlDuringInflightMessage.test_unfurl_after_completion_still_suppressed
func TestUnfurlDuringInflightMessageUnfurlAfterCompletionStillSuppressed(t *testing.T) {
	a, delivered := unfurlAdapter(t)
	patchResolveUserName(t, richard)
	for _, event := range []map[string]any{unfurlOriginalEvent(), unfurlEditEvent()} {
		if err := a.handleSlackMessage(context.Background(), event, unfurlBody()); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(delivered.all()); n != 1 {
		t.Fatalf("deliveries = %d", n)
	}
}

// test_slack_unfurl_duplicate_turn.py TestClaimDoesNotSwallowRealMessages.test_a_different_message_is_unaffected
func TestClaimDoesNotSwallowRealMessagesADifferentMessageIsUnaffected(t *testing.T) {
	a, delivered := unfurlAdapter(t)
	patchResolveUserName(t, richard)
	other := unfurlOriginalEvent()
	other["ts"], other["client_msg_id"] = "1787365500.000100", "cmid-2"
	for _, event := range []map[string]any{unfurlOriginalEvent(), other} {
		if err := a.handleSlackMessage(context.Background(), event, unfurlBody()); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(delivered.all()); n != 2 {
		t.Fatalf("deliveries = %d", n)
	}
}

// test_slack_unfurl_duplicate_turn.py TestClaimDoesNotSwallowRealMessages.test_a_genuine_user_edit_is_still_suppressed
func TestClaimDoesNotSwallowRealMessagesAGenuineUserEditIsStillSuppressed(t *testing.T) {
	a, delivered := unfurlAdapter(t)
	patchResolveUserName(t, richard)
	for _, event := range []map[string]any{unfurlOriginalEvent(), unfurlUserEdit()} {
		if err := a.handleSlackMessage(context.Background(), event, unfurlBody()); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(delivered.all()); n != 1 {
		t.Fatalf("deliveries = %d", n)
	}
}

// test_slack_unfurl_duplicate_turn.py TestClaimHelperBounded.test_claim_map_is_evicted_oldest_first
func TestClaimHelperBoundedClaimMapIsEvictedOldestFirst(t *testing.T) {
	a, _ := unfurlAdapter(t)
	a.processedTSCap = 10
	for i := range 25 {
		a.rememberProcessedMessageTS(fmt.Sprintf("%d.0", i))
		time.Sleep(time.Millisecond)
	}
	if len(a.processedMessageTS) > 10 {
		t.Fatalf("claims = %d", len(a.processedMessageTS))
	}
	if _, ok := a.processedMessageTS["24.0"]; !ok {
		t.Fatal("newest claim evicted")
	}
	if _, ok := a.processedMessageTS["0.0"]; ok {
		t.Fatal("oldest claim kept")
	}
}

// test_slack_unfurl_duplicate_turn.py TestClaimHelperBounded.test_empty_ts_is_ignored
func TestClaimHelperBoundedEmptyTsIsIgnored(t *testing.T) {
	a, _ := unfurlAdapter(t)
	a.rememberProcessedMessageTS("")
	if len(a.processedMessageTS) != 0 {
		t.Fatalf("claims = %v", a.processedMessageTS)
	}
}

// test_slack_unfurl_duplicate_turn.py TestClaimReleasedOnFailure.test_failed_first_copy_releases_claim_so_edit_can_redrive
func TestClaimReleasedOnFailureFailedFirstCopyReleasesClaimSoEditCanRedrive(t *testing.T) {
	a, delivered := unfurlAdapter(t)
	calls := 0
	patchResolveUserName(t, func(*SlackAdapter, context.Context, string, string, string) string {
		calls++
		if calls == 1 {
			panic("slack api died mid-enrichment")
		}
		return "richard"
	})
	if raised, _ := handleCatching(a, unfurlOriginalEvent()); raised == nil {
		t.Fatal("the failing first copy did not raise")
	}
	if _, ok := a.processedMessageTS[unfurlOriginalTS]; ok {
		t.Fatal("the failed copy kept its claim")
	}
	if err := a.handleSlackMessage(context.Background(), unfurlUserEdit(), unfurlBody()); err != nil {
		t.Fatal(err)
	}
	if n := len(delivered.all()); n != 1 {
		t.Fatalf("deliveries = %d", n)
	}
}

// test_slack_unfurl_duplicate_turn.py TestClaimReleasedOnFailure.test_failure_does_not_release_a_preexisting_claim
func TestClaimReleasedOnFailureFailureDoesNotReleaseAPreexistingClaim(t *testing.T) {
	a, delivered := unfurlAdapter(t)
	patchResolveUserName(t, richard)
	if err := a.handleSlackMessage(context.Background(), unfurlOriginalEvent(), unfurlBody()); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.processedMessageTS[unfurlOriginalTS]; !ok {
		t.Fatal("the delivered copy holds no claim")
	}
	resolveUserNameFn = func(*SlackAdapter, context.Context, string, string, string) string { panic("boom") }
	second := unfurlOriginalEvent()
	second["client_msg_id"] = "cmid-replay"
	_, _ = handleCatching(a, second)
	if _, ok := a.processedMessageTS[unfurlOriginalTS]; !ok {
		t.Fatal("a failing duplicate released the earlier claim")
	}
	if n := len(delivered.all()); n != 1 {
		t.Fatalf("deliveries = %d", n)
	}
}
