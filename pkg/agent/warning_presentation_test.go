package agent

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/provider"
)

func warningEmitter(t *testing.T, suppress any) (*AIAgent, *[]string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	text := "{}\n"
	if suppress != nil {
		text = "display: {suppress_warning_notifications: " + map[any]string{true: "true", false: "false", "typo": "typo", "list": "[]", "map": "{}"}[suppress] + "}\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	printed := &[]string{}
	return &AIAgent{platform: "cli", printFn: func(s string) { *printed = append(*printed, s) }}, printed
}

func TestWarningPolicyAtRealPresentationBoundary(t *testing.T) {
	for _, setting := range []any{nil, false, true, "typo", "list", "map"} {
		a, printed := warningEmitter(t, setting)
		var callbacks [][2]any
		a.statusCallback = func(kind string, message any) { callbacks = append(callbacks, [2]any{kind, message}) }
		a.emitWarning("arbitrary diagnostic wording")
		if !reflect.DeepEqual(callbacks, [][2]any{{"warn", "arbitrary diagnostic wording"}}) {
			t.Fatalf("%v callbacks %v", setting, callbacks)
		}
		if want := map[bool]int{true: 0, false: 1}[setting == true]; len(*printed) != want {
			t.Fatalf("%v printed %v", setting, *printed)
		}
		a.emitStatus("⚠ quoted error in ordinary progress")
		if callbacks[len(callbacks)-1] != [2]any{"lifecycle", "⚠ quoted error in ordinary progress"} {
			t.Fatalf("%v status %v", setting, callbacks)
		}
		a.SetPendingFallbackNotice([]string{"unrecognized fallback wording"})
		a.emitPendingFallbackNotice()
		if a.pendingFallbackNotice != nil || len(callbacks) != 3 {
			t.Fatalf("%v fallback notice %v %v", setting, a.pendingFallbackNotice, callbacks)
		}
	}
}

func TestDestinationSnapshotOwnsPolicyAndReaderFailureNeverBreaksEmission(t *testing.T) {
	var emitted [][2]any
	a := &AIAgent{platform: "cli", printFn: func(string) {}}
	a.statusCallback = func(kind string, message any) { emitted = append(emitted, [2]any{kind, message}) }
	a.SetNotificationPresentation("slack", map[string]any{"display": map[string]any{"suppress_warning_notifications": true,
		"platforms": map[string]any{"slack": map[string]any{"suppress_warning_notifications": false}}}})
	a.emitWarning("destination allows this diagnostic")
	if !reflect.DeepEqual(emitted, [][2]any{{"warn", "destination allows this diagnostic"}}) {
		t.Fatalf("emitted %v", emitted)
	}
	saved := warningNotificationsEnabled
	warningNotificationsEnabled = func(context.Context, string, map[string]any) bool { panic("configuration unavailable") }
	t.Cleanup(func() { warningNotificationsEnabled = saved })
	a.emitWarning("still observable when configuration fails")
	if emitted[len(emitted)-1] != [2]any{"warn", "still observable when configuration fails"} {
		t.Fatalf("emitted %v", emitted)
	}
}

func TestDirectPrintDiagnosticsPreserveContentAndMutedTurnHasNoPrints(t *testing.T) {
	for _, suppress := range []bool{false, true} {
		a, printed := warningEmitter(t, suppress)
		if display, _ := NotificationConfigSnapshot(context.Background())["display"].(map[string]any); display["suppress_warning_notifications"] != suppress {
			t.Fatalf("snapshot did not read the owning profile config %v", display)
		}
		a.safePrint("diagnostic", true)
		a.vprint("retry diagnostic", true, true)
		if want := map[bool]int{true: 0, false: 2}[suppress]; len(*printed) != want {
			t.Fatalf("suppress=%v printed %v", suppress, *printed)
		}
		a.safePrint("requested error explanation", false)
		if (*printed)[len(*printed)-1] != "requested error explanation" {
			t.Fatalf("requested output hidden %v", *printed)
		}
		_, restore := a.NotificationTurn(context.Background(), true, "")
		a.safePrint("model echo", false)
		restore()
		if (*printed)[len(*printed)-1] != "requested error explanation" {
			t.Fatalf("muted turn printed %v", *printed)
		}
	}
}

func TestOperatorCallbacksKeepDiagnosticsAndLogs(t *testing.T) {
	for _, suppress := range []any{nil, false, true} {
		a, _ := warningEmitter(t, suppress)
		var logs bytes.Buffer
		operator := slog.New(slog.NewTextHandler(&logs, nil))
		var seen [][]any
		record := func(args ...any) {
			seen = append(seen, args)
			operator.Warn("observed", "args", args)
		}
		a.statusCallback, a.noticeCallback, a.thinkingCallback = record, record, record
		notice := provider.AgentNotice{Level: "error", Text: "notice detail"}
		a.emitWarning("warning detail")
		a.emitDiagnosticStatus("fallback detail")
		a.emitNotice(notice)
		a.emitDiagnosticWait("retry detail")
		want := [][]any{{"warn", "warning detail"}, {"lifecycle", config.DiagnosticText("fallback detail")}, {notice}, {config.DiagnosticText("retry detail")}}
		if !reflect.DeepEqual(seen, want) || strings.Count(logs.String(), "msg=observed") != 4 {
			t.Fatalf("suppress=%v seen %v logs %q", suppress, seen, logs.String())
		}
		_, restore := a.NotificationTurn(context.Background(), true, "")
		a.emitWarning("wake diagnostic")
		a.emitNotice(notice)
		a.emitDiagnosticWait("wake retry")
		restore()
		tail := seen[len(seen)-3:]
		if !reflect.DeepEqual(tail, [][]any{{"warn", "wake diagnostic"}, {notice}, {config.DiagnosticText("wake retry")}}) {
			t.Fatalf("muted wake dropped operator observers %v", tail)
		}
	}
}

func TestEntitlementGuidanceIsClassifiedAtDirectPrint(t *testing.T) {
	for _, suppress := range []any{nil, false, true} {
		a, printed := warningEmitter(t, suppress)
		if !printGuidance(a, "entitlement detail\nnext step") {
			t.Fatal("guidance not printed")
		}
		want := []string{"   \U0001f4a1 entitlement detail", "   \U0001f4a1 next step"}
		if suppress == true {
			want = []string{}
		}
		if !reflect.DeepEqual(*printed, want) {
			t.Fatalf("suppress=%v printed %q", suppress, *printed)
		}
	}
}

func TestNotificationTurnFreezesPresentationAndRestores(t *testing.T) {
	a := &AIAgent{streamDeltaCallback: func(string) {}, reactionCallback: func(string) {}, toolGenCallback: func() {}}
	ctx, restore := a.NotificationTurn(context.Background(), true, "s1")
	if a.streamDeltaCallback != nil || a.reactionCallback != nil || a.toolGenCallback != nil || !a.suppressStatusOutput || !a.muteNotificationReply {
		t.Fatal("muted turn left presentation live")
	}
	if !EventPresentationMuted(ctx, "message.delta", "s1") || EventPresentationMuted(ctx, "approval.request", "s1") || EventPresentationMuted(ctx, "message.delta", "s2") || EventPresentationMuted(context.Background(), "message.delta", "") {
		t.Fatal("muted surface scope")
	}
	restore()
	if a.streamDeltaCallback == nil || a.reactionCallback == nil || a.toolGenCallback == nil || a.suppressStatusOutput || a.muteNotificationReply {
		t.Fatal("presentation not restored")
	}
	if same, done := a.NotificationTurn(ctx, false, "s1"); same != ctx || a.suppressStatusOutput {
		t.Fatal("unmuted turn changed presentation")
	} else {
		done()
	}
	restorePolicy := a.NotificationPolicySnapshot("slack", map[string]any{"display": map[string]any{}})
	if *a.notificationPlatform != "slack" || a.notificationConfig == nil {
		t.Fatal("policy snapshot not bound")
	}
	restorePolicy()
	if a.notificationPlatform != nil || a.notificationConfig != nil {
		t.Fatal("policy snapshot not restored")
	}
	for event, want := range map[string]bool{"task_failure_notice": true, "watch_disabled": true, "watch_overflow_released": true, "completed": false} {
		ev := map[string]any{"type": event}
		if event == "task_failure_notice" {
			ev = map[string]any{"type": "completed", "task_failure_notice": "x"}
		}
		if DiagnosticProcessEvent(ev) != want {
			t.Fatalf("%s diagnostic=%v", event, !want)
		}
	}
}
