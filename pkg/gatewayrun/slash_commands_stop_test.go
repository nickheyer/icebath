package gatewayrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
)

// Slack source of a chat type and chat with an optional thread, user and workspace
func slackStopSource(chatType, chatID string, threadID *string, userID, scopeID string) *gateway.SessionSource {
	return &gateway.SessionSource{Platform: gateway.PlatformSlack, ChatType: chatType, ChatID: chatID,
		ThreadID: threadID, UserID: kstr(userID), ScopeID: kstr(scopeID)}
}

// Session key of a source under the default per user modes
func stopKey(t *testing.T, source *gateway.SessionSource, threadPerUser bool, profile string) string {
	t.Helper()
	key, err := gateway.BuildSessionKey(context.Background(), source, true, threadPerUser, profile)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

var stopInvalidatedRe = regexp.MustCompile(`^Invalidated run generation for (.+) → \d+ \((.+)\)$`)

// Drives one stop through the real handler with the running keys, returning the interrupted keys and reasons
func driveStop(t *testing.T, source *gateway.SessionSource, running map[string]*agent.AIAgent, authorized bool) ([][2]string, any) {
	t.Helper()
	return driveStopWith(t, source, running, authorized, nil)
}

// Drives one stop like driveStop with an adapter connected for the source platform
func driveStopWith(t *testing.T, source *gateway.SessionSource, running map[string]*agent.AIAgent, authorized bool, adapter gateway.PlatformAdapter) ([][2]string, any) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	for _, k := range []string{"SLACK_ALLOWED_USERS", "WHATSAPP_ALLOWED_USERS", "TELEGRAM_ALLOWED_USERS", "MATRIX_ALLOWED_USERS",
		"GATEWAY_ALLOWED_USERS", "SLACK_ALLOW_ALL_USERS", "WHATSAPP_ALLOW_ALL_USERS", "TELEGRAM_ALLOW_ALL_USERS", "MATRIX_ALLOW_ALL_USERS"} {
		t.Setenv(k, "")
	}
	if authorized {
		t.Setenv("GATEWAY_ALLOW_ALL_USERS", "true")
	} else {
		t.Setenv("GATEWAY_ALLOW_ALL_USERS", "")
	}
	ctx := context.Background()
	r := allocGatewayRunner()
	r.config = gateway.DefaultGatewayConfig(ctx)
	r.sessionStore = gateway.NewSessionStore(ctx, filepath.Join(h, "sessions"), r.config, nil)
	r.sessions = map[string]*SessionState{}
	r.adapters = pydict.New[gateway.Platform, gateway.PlatformAdapter]()
	if adapter != nil {
		r.adapters.Set(source.Platform, adapter)
	}
	for key, a := range running {
		r.runningAgents().Set(key, a)
	}
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	event := gateway.NewMessageEvent("/stop")
	event.Source = source
	result, err := r.handleStopCommand(ctx, event)
	slog.SetDefault(prev)
	if err != nil {
		t.Fatal(err)
	}
	var interrupted [][2]string
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		var rec struct {
			Msg string `json:"msg"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if m := stopInvalidatedRe.FindStringSubmatch(rec.Msg); m != nil {
			interrupted = append(interrupted, [2]string{m[1], m[2]})
		}
	}
	return interrupted, result
}

// Running map with one real agent under each key
func stopRunning(t *testing.T, keys ...string) map[string]*agent.AIAgent {
	t.Helper()
	out := map[string]*agent.AIAgent{}
	for _, k := range keys {
		out[k] = quietAgent(t)
	}
	return out
}

// Asserts the reply is the plain or ephemeral text of the key
func assertStopReply(t *testing.T, result any, key string) {
	t.Helper()
	want := i18n.T(context.Background(), key, "", nil)
	switch v := result.(type) {
	case *gateway.EphemeralReply:
		if v.Text() != want {
			t.Fatalf("reply %q want %q", v.Text(), want)
		}
	case string:
		if v != want {
			t.Fatalf("reply %q want %q", v, want)
		}
	default:
		t.Fatalf("reply %#v", result)
	}
}

// Asserts the interrupted pairs equal the expected ones in order
func assertInterrupted(t *testing.T, got [][2]string, want ...[2]string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("interrupted %v want %v", got, want)
	}
}

func TestStopFromThreadReachesTopLevelChannelRun(t *testing.T) {
	runningKey := stopKey(t, slackStopSource("channel", "C9", kstr("170.100"), "U-alice", "T1"), false, "")
	stopSource := slackStopSource("thread", "C9", kstr("170.100"), "U-alice", "T1")
	if stopKey(t, stopSource, false, "") == runningKey {
		t.Fatal("keys collide")
	}
	interrupted, result := driveStop(t, stopSource, stopRunning(t, runningKey), true)
	assertInterrupted(t, interrupted, [2]string{runningKey, "stop_command_chat_scope"})
	assertStopReply(t, result, "gateway.stop.stopped")
}

func TestStopWithThreadReachesRollingDmRun(t *testing.T) {
	runningKey := stopKey(t, slackStopSource("dm", "D1", nil, "U-alice", "T1"), false, "")
	stopSource := slackStopSource("dm", "D1", kstr("170.100"), "U-alice", "T1")
	if stopKey(t, stopSource, false, "") == runningKey {
		t.Fatal("keys collide")
	}
	interrupted, result := driveStop(t, stopSource, stopRunning(t, runningKey), true)
	assertInterrupted(t, interrupted, [2]string{runningKey, "stop_command_chat_scope"})
	assertStopReply(t, result, "gateway.stop.stopped")
}

func TestStopReachesPeerRunInPerSenderGroup(t *testing.T) {
	runningKey := stopKey(t, slackStopSource("group", "C9", nil, "U-bob", "T1"), false, "")
	stopSource := slackStopSource("group", "C9", nil, "U-alice", "T1")
	if stopKey(t, stopSource, false, "") == runningKey {
		t.Fatal("keys collide")
	}
	interrupted, result := driveStop(t, stopSource, stopRunning(t, runningKey), true)
	assertInterrupted(t, interrupted, [2]string{runningKey, "stop_command_chat_scope"})
	assertStopReply(t, result, "gateway.stop.stopped")
}

func TestStopDoesNotReachADifferentThreadOfTheSameChannel(t *testing.T) {
	sameThread := stopKey(t, slackStopSource("channel", "C9", kstr("170.100"), "U-alice", "T1"), false, "")
	otherThread := stopKey(t, slackStopSource("thread", "C9", kstr("170.200"), "U-alice", "T1"), false, "")
	stopSource := slackStopSource("thread", "C9", kstr("170.100"), "U-alice", "T1")
	interrupted, result := driveStop(t, stopSource, stopRunning(t, sameThread, otherThread), true)
	assertInterrupted(t, interrupted, [2]string{sameThread, "stop_command_chat_scope"})
	assertStopReply(t, result, "gateway.stop.stopped")
}

func TestStopDoesNotReachAnotherReplyThreadOfAChannelKeyedRun(t *testing.T) {
	sameReply := stopKey(t, slackStopSource("channel", "C9", kstr("170.100"), "U-alice", "T1"), false, "")
	otherReply := stopKey(t, slackStopSource("channel", "C9", kstr("170.200"), "U-alice", "T1"), false, "")
	stopSource := slackStopSource("thread", "C9", kstr("170.100"), "U-alice", "T1")
	interrupted, result := driveStop(t, stopSource, stopRunning(t, sameReply, otherReply), true)
	assertInterrupted(t, interrupted, [2]string{sameReply, "stop_command_chat_scope"})
	assertStopReply(t, result, "gateway.stop.stopped")
}

func TestStopInterruptsEveryRunOfTheCallersThread(t *testing.T) {
	sibling := stopKey(t, slackStopSource("thread", "C9", kstr("170.100"), "U-bob", "T1"), true, "")
	sameThreadChannelRun := stopKey(t, slackStopSource("channel", "C9", kstr("170.100"), "U-alice", "T1"), false, "")
	stopSource := slackStopSource("thread", "C9", kstr("170.100"), "U-alice", "T1")
	interrupted, result := driveStop(t, stopSource, stopRunning(t, sibling, sameThreadChannelRun), true)
	var keys []string
	for _, pair := range interrupted {
		keys = append(keys, pair[0])
	}
	sort.Strings(keys)
	want := []string{sibling, sameThreadChannelRun}
	sort.Strings(want)
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("interrupted %v want %v", keys, want)
	}
	assertStopReply(t, result, "gateway.stop.stopped")
}

func TestALoneThreadSiblingKeepsItsOwnInvalidationReason(t *testing.T) {
	sibling := stopKey(t, slackStopSource("thread", "C9", kstr("170.100"), "U-bob", "T1"), true, "")
	stopSource := slackStopSource("thread", "C9", kstr("170.100"), "U-alice", "T1")
	interrupted, result := driveStop(t, stopSource, stopRunning(t, sibling), true)
	assertInterrupted(t, interrupted, [2]string{sibling, "stop_command_thread_sibling"})
	assertStopReply(t, result, "gateway.stop.stopped")
}

func TestStopMatchesACanonicalisedWhatsappDmChatID(t *testing.T) {
	runningKey := stopKey(t, &gateway.SessionSource{Platform: gateway.PlatformWhatsApp, ChatType: "dm",
		ChatID: "1234567890@s.whatsapp.net", UserID: kstr("1234567890@s.whatsapp.net")}, false, "")
	stopSource := &gateway.SessionSource{Platform: gateway.PlatformWhatsApp, ChatType: "dm",
		ChatID: "1234567890:7@s.whatsapp.net", UserID: kstr("1234567890:7@s.whatsapp.net"), ThreadID: kstr("t1")}
	if stopKey(t, stopSource, false, "") == runningKey {
		t.Fatal("keys collide")
	}
	interrupted, result := driveStop(t, stopSource, stopRunning(t, runningKey), true)
	assertInterrupted(t, interrupted, [2]string{runningKey, "stop_command_chat_scope"})
	assertStopReply(t, result, "gateway.stop.stopped")
}

func TestStopInADmDoesNotReachAGroupRunThatEndsInTheSameUserID(t *testing.T) {
	sameChat := stopKey(t, &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatType: "dm", ChatID: "777",
		ThreadID: kstr("42"), UserID: kstr("777")}, false, "")
	groupRun := stopKey(t, &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatType: "group", ChatID: "-100123",
		UserID: kstr("777")}, false, "")
	dmStop := &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatType: "dm", ChatID: "777", UserID: kstr("777")}
	interrupted, result := driveStop(t, dmStop, stopRunning(t, sameChat, groupRun), true)
	assertInterrupted(t, interrupted, [2]string{sameChat, "stop_command_chat_scope"})
	assertStopReply(t, result, "gateway.stop.stopped")
}

func TestChatScopeFallbackIsAuthorizationGated(t *testing.T) {
	runningKey := stopKey(t, slackStopSource("channel", "C9", kstr("170.100"), "U-alice", "T1"), false, "")
	stopSource := slackStopSource("thread", "C9", kstr("170.100"), "U-alice", "T1")
	interrupted, result := driveStop(t, stopSource, stopRunning(t, runningKey), false)
	assertInterrupted(t, interrupted)
	assertStopReply(t, result, "gateway.stop.no_active")
}

func TestPendingSentinelIsNeverInterrupted(t *testing.T) {
	pending := stopKey(t, slackStopSource("channel", "C9", kstr("170.100"), "U-alice", "T1"), false, "")
	stopSource := slackStopSource("thread", "C9", kstr("170.100"), "U-alice", "T1")
	interrupted, result := driveStop(t, stopSource, map[string]*agent.AIAgent{pending: agentPendingSentinel}, true)
	assertInterrupted(t, interrupted)
	assertStopReply(t, result, "gateway.stop.no_active")
}

func TestStopReachesAChatWhoseIDContainsAColon(t *testing.T) {
	room := &gateway.SessionSource{Platform: gateway.PlatformMatrix, ChatType: "channel", ChatID: "!room:example.org",
		ThreadID: kstr("$t1"), UserID: kstr("@alice:example.org")}
	runningKey := stopKey(t, room, false, "")
	stopSource := &gateway.SessionSource{Platform: gateway.PlatformMatrix, ChatType: "thread", ChatID: "!room:example.org",
		ThreadID: kstr("$t1"), UserID: kstr("@alice:example.org")}
	if stopKey(t, stopSource, false, "") == runningKey {
		t.Fatal("keys collide")
	}
	interrupted, result := driveStop(t, stopSource, stopRunning(t, runningKey), true)
	assertInterrupted(t, interrupted, [2]string{runningKey, "stop_command_chat_scope"})
	assertStopReply(t, result, "gateway.stop.stopped")
}

func TestChatScopeFallbackInterruptsOnlyTheCallersChat(t *testing.T) {
	for _, otherChatID := range []string{"C-other", "C90"} {
		t.Run(otherChatID, func(t *testing.T) {
			sameChat := stopKey(t, slackStopSource("channel", "C9", kstr("170.100"), "U-alice", "T1"), false, "")
			foreign := stopKey(t, slackStopSource("channel", otherChatID, kstr("170.100"), "U-alice", "T1"), false, "")
			stopSource := slackStopSource("thread", "C9", kstr("170.100"), "U-alice", "T1")
			interrupted, result := driveStop(t, stopSource, stopRunning(t, sameChat, foreign), true)
			assertInterrupted(t, interrupted, [2]string{sameChat, "stop_command_chat_scope"})
			assertStopReply(t, result, "gateway.stop.stopped")
		})
	}
}

func TestChatScopeFallbackDoesNotFoldInAPrefixOfTheChatID(t *testing.T) {
	foreign := stopKey(t, slackStopSource("channel", "C90", nil, "U-alice", "T1"), false, "")
	stopSource := slackStopSource("channel", "C9", nil, "U-alice", "T1")
	interrupted, result := driveStop(t, stopSource, stopRunning(t, foreign), true)
	assertInterrupted(t, interrupted)
	assertStopReply(t, result, "gateway.stop.no_active")
}

func TestChatScopeFallbackDoesNotCrossWorkspaceScopeOrProfile(t *testing.T) {
	sameChat := stopKey(t, slackStopSource("channel", "C9", kstr("170.100"), "U-alice", "T1"), false, "")
	otherScope := stopKey(t, slackStopSource("channel", "C9", kstr("170.100"), "U-alice", "T2"), false, "")
	otherProfile := stopKey(t, slackStopSource("channel", "C9", kstr("170.100"), "U-alice", "T1"), false, "work")
	stopSource := slackStopSource("thread", "C9", kstr("170.100"), "U-alice", "T1")
	interrupted, result := driveStop(t, stopSource, stopRunning(t, sameChat, otherScope, otherProfile), true)
	assertInterrupted(t, interrupted, [2]string{sameChat, "stop_command_chat_scope"})
	assertStopReply(t, result, "gateway.stop.stopped")
}

// Discord forum thread source of one participant in a chat
func siblingStopSource(uid string) *gateway.SessionSource {
	return &gateway.SessionSource{Platform: gateway.PlatformDiscord, ChatType: "forum", ChatID: "chan1", ThreadID: kstr("thr1"), UserID: kstr(uid)}
}

func TestStopDoesNotInterruptSiblingWhenUnauthorized(t *testing.T) {
	keyB := stopKey(t, siblingStopSource("userB"), true, "")
	interrupted, result := driveStop(t, siblingStopSource("userA"), stopRunning(t, keyB), false)
	assertInterrupted(t, interrupted)
	reply, _ := result.(string)
	if !strings.Contains(strings.ToLower(reply), "no active") {
		t.Fatalf("reply %#v", result)
	}
}

// Discord adapter whose status clear always fails
type stopBoomAdapter struct {
	*gateway.BasePlatformAdapter
}

func (a *stopBoomAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *stopBoomAdapter) Disconnect(context.Context) error            { return nil }
func (a *stopBoomAdapter) Send(context.Context, string, string, string, map[string]any) (gateway.SendResult, error) {
	return gateway.SendResult{Success: true}, nil
}
func (a *stopBoomAdapter) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"id": chatID}, nil
}
func (a *stopBoomAdapter) StopTyping(context.Context, string, map[string]any) error {
	return errors.New("boom")
}

func TestStopNoActiveAgentSurvivesStatusClearFailure(t *testing.T) {
	adapter := &stopBoomAdapter{}
	adapter.BasePlatformAdapter = gateway.NewBasePlatformAdapter(gateway.NewPlatformConfig(), gateway.PlatformDiscord, adapter)
	_, result := driveStopWith(t, siblingStopSource("userA"), nil, true, adapter)
	reply, _ := result.(string)
	if !strings.Contains(strings.ToLower(reply), "no active") {
		t.Fatalf("reply %#v", result)
	}
}
