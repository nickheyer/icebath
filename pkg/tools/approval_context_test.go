package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/settings"
)

func TestNormalizeApprovalMode(t *testing.T) {
	cases := map[any]string{false: "off", "off": "off", "  SMART  ": "smart", true: "manual", "": "manual", "auto": "manual"}
	for in, want := range cases {
		if got := normalizeApprovalMode(in); got != want {
			t.Errorf("normalize(%v) = %q want %q", in, got, want)
		}
	}
	if getApprovalMode(approvalYAMLEnv(t, "approvals:\n  mode: false\n")) != "off" {
		t.Fatal("config false maps to off")
	}
	if getApprovalMode(approvalYAMLEnv(t, "")) != settings.ApprovalMode.Default {
		t.Fatal("default mode is the registry default")
	}
}

// Env of a fresh home whose config.yaml holds text
func approvalYAMLEnv(t *testing.T, text string) *Env {
	t.Helper()
	h := t.TempDir()
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Env{Home: h}
}

func TestApprovalTimeoutAndWindow(t *testing.T) {
	if getApprovalTimeout(approvalYAMLEnv(t, "")) != 300 {
		t.Fatal("default timeout")
	}
	if getApprovalTimeout(approvalYAMLEnv(t, "approvals:\n  timeout: abc\n")) != 300 {
		t.Fatal("bad timeout falls back")
	}
	if getApprovalTimeout(approvalYAMLEnv(t, "approvals:\n  timeout: 90\n")) != 90 {
		t.Fatal("configured timeout")
	}
	if got := getApprovalTimeout(approvalYAMLEnv(t, "approvals:\n  timeout: 1073741824\n")); got != int(MaxSafeTimeoutSeconds) {
		t.Fatalf("clamped timeout %d", got)
	}
	for seconds, want := range map[int]string{300: "5 minutes", 90: "90 seconds", 7200: "2 hours", 60: "1 minute", 3600: "1 hour", 1: "1 second", 0: "0 seconds"} {
		if got := formatApprovalWindow(seconds); got != want {
			t.Errorf("window(%d) = %q want %q", seconds, got, want)
		}
	}
	kw := approvalTimeoutNoticeKwargs(approvalYAMLEnv(t, "approvals:\n  timeout: 90\n"))
	if kw["waited"] != "90 seconds" || kw["suggested"] != 270 {
		t.Fatalf("notice kwargs %v", kw)
	}
}

func TestBinaryModesAndTransport(t *testing.T) {
	for raw, want := range map[string]string{`"approve"`: "approve", `" APPROVE "`: "approve", `"deny"`: "deny", `"off"`: "deny", `"allow"`: "deny", "yes": "deny", "null": "deny", "true": "deny", `"nonsense"`: "deny"} {
		if got := getCronApprovalMode(approvalYAMLEnv(t, "approvals:\n  cron_mode: "+raw+"\n")); got != want {
			t.Errorf("cron_mode %v = %q want %q", raw, got, want)
		}
	}
	if getSingleQueryApprovalMode(approvalYAMLEnv(t, "")) != "deny" || getUnattendedApprovalMode(approvalYAMLEnv(t, "")) != "deny" {
		t.Fatal("default binary modes are deny")
	}
	sel, fb := getApprovalTransportConfig(approvalYAMLEnv(t, ""))
	if sel != "builtin" || fb != "" {
		t.Fatalf("transport default %q %q", sel, fb)
	}
	sel, fb = getApprovalTransportConfig(approvalYAMLEnv(t, "security:\n  approval:\n    transport: ' Slack '\n    transport_fallback: BUILTIN\n"))
	if sel != "slack" || fb != "builtin" {
		t.Fatalf("transport %q %q", sel, fb)
	}
}

func TestSessionContextPredicates(t *testing.T) {
	for _, name := range []string{"ICEBATH_SESSION_PLATFORM", "ICEBATH_CRON_SESSION", "ICEBATH_GATEWAY_SESSION", "ICEBATH_INTERACTIVE", "ICEBATH_SINGLE_QUERY_SESSION", "ICEBATH_SESSION_KEY"} {
		t.Setenv(name, "")
	}
	env := &Env{Config: config.Tree{}, Values: &sync.Map{}}
	if IsGatewayApprovalContext(env) {
		t.Fatal("no platform is not a gateway context")
	}
	env.Values.Store("session.platform", "webhook")
	if IsGatewayApprovalContext(env) || !isUnattendedPlatformApprovalContext(env) || !NoUserCanAnswer(env) {
		t.Fatal("webhook is unattended")
	}
	for _, p := range []string{"msgraph_webhook", "api_server"} {
		env.Values.Store("session.platform", p)
		if IsGatewayApprovalContext(env) {
			t.Fatalf("%s must not be a gateway context", p)
		}
	}
	env.Values.Store("session.platform", "telegram")
	if !IsGatewayApprovalContext(env) {
		t.Fatal("telegram is a gateway context")
	}
	env.Values.Store("session.cron", true)
	if IsGatewayApprovalContext(env) || !isCronApprovalContext(env) {
		t.Fatal("cron is never a gateway context")
	}
	env.Values.Delete("session.cron")
	RestoreSessionVar(env, "ICEBATH_CRON_SESSION", "1", true)
	if IsGatewayApprovalContext(env) || !isCronApprovalContext(env) {
		t.Fatal("the session variable a cron run binds marks the cron context")
	}
	RestoreSessionVar(env, "ICEBATH_CRON_SESSION", "", false)
	t.Setenv("ICEBATH_SESSION_KEY", "proc-key")
	if getCurrentSessionKey(env, "default") != "proc-key" {
		t.Fatal("process env session key")
	}
	SetCurrentSessionKey(env, "ctx-key")
	if getCurrentSessionKey(env, "default") != "ctx-key" {
		t.Fatal("context session key overrides process env")
	}
	ResetCurrentSessionKey(env)
	if getCurrentSessionKey(&Env{Values: &sync.Map{}}, "default") != "proc-key" {
		t.Fatal("reset restores the env fallback")
	}
	env.Values.Store("session.interactive", true)
	if !isInteractiveCLI(env) {
		t.Fatal("bound interactive flag")
	}
	env.Values.Delete("session.interactive")
	if isInteractiveCLI(env) {
		t.Fatal("reset interactive flag")
	}
	prev := FiniteTurnRequired
	FiniteTurnRequired = func(*Env) (bool, bool) { return true, true }
	defer func() { FiniteTurnRequired = prev }()
	if !isSingleQueryApprovalContext(env) {
		t.Fatal("finite turn is single query")
	}
	if !shouldFallThroughToCLIApproval(true, true, false) || shouldFallThroughToCLIApproval(true, true, true) || shouldFallThroughToCLIApproval(false, true, false) {
		t.Fatal("cli fall through")
	}
	env.Values.Store("session.source", "tui")
	if !isLocalInteractiveSession(env) {
		t.Fatal("tui source is local interactive")
	}
	if isViewerTurnWithApprover(context.Background()) {
		t.Fatal("no gateway hook means no approver")
	}
}

func TestFireApprovalHookForwardsCorrelationIDs(t *testing.T) {
	var got []map[string]any
	ApprovalHookInvoker = func(_ context.Context, name string, payload map[string]any) error {
		payload["hook"] = name
		got = append(got, payload)
		return errors.New("observer failed")
	}
	defer func() { ApprovalHookInvoker = nil }()
	ctx := context.Background()
	env := &Env{Values: &sync.Map{}}
	fireApprovalHook(ctx, WithObservabilityContext(env, "turn-1", "call-1", "sess-1"), "pre_approval_request", map[string]any{"command": "ls"})
	if len(got) != 1 || got[0]["turn_id"] != "turn-1" || got[0]["tool_call_id"] != "call-1" || got[0]["session_id"] != "sess-1" || got[0]["hook"] != "pre_approval_request" {
		t.Fatalf("hook payload %v", got)
	}
	fireApprovalHook(ctx, env, "post_approval_response", map[string]any{"turn_id": "explicit"})
	if got[1]["turn_id"] != "explicit" || got[1]["tool_call_id"] != "" {
		t.Fatalf("hook payload %v", got[1])
	}
	if _, present := got[1]["session_id"]; present {
		t.Fatal("empty session id is not forwarded")
	}
}

func TestSessionYoloContract(t *testing.T) {
	stored := map[string]bool{}
	persist := func(key string) func(bool) error {
		return func(on bool) error { stored[key] = on; return nil }
	}
	defer ClearSession(context.Background(), "s1")
	defer ClearSession(context.Background(), "s2")
	if !ToggleSessionYolo(context.Background(), "s1", nil, false, persist("s1")) || !stored["s1"] {
		t.Fatal("toggle on persists before the flip")
	}
	cfg := WithSessionYolo(map[string]any{"max_iterations": 5}, "s1")
	if cfg["yolo_mode"] != true || cfg["max_iterations"] != 5 {
		t.Fatalf("row config %v", cfg)
	}
	ClearSession(context.Background(), "s1")
	if !RestoreSessionYolo(context.Background(), "s1", stored["s1"]) || !IsSessionYoloEnabled("s1") {
		t.Fatal("restore re arms the bypass")
	}
	TransferSessionYolo(context.Background(), "s1", "s2")
	if !IsSessionYoloEnabled("s2") || IsSessionYoloEnabled("s1") {
		t.Fatal("transfer moves the flag")
	}
	ClearSession(context.Background(), "s2")
	if ToggleSessionYolo(context.Background(), "s2", nil, true, persist("s2")) || stored["s2"] {
		t.Fatal("toggle after restart switches the stored on flag off")
	}
	if RestoreSessionYolo(context.Background(), "s2", false) || IsSessionYoloEnabled("s2") {
		t.Fatal("stored off never re arms")
	}
	broken := func(bool) error { return errors.New("disk full") }
	if !ToggleSessionYolo(context.Background(), "s2", nil, false, broken) || !IsSessionYoloEnabled("s2") {
		t.Fatal("failed write never blocks the live flip")
	}
	ClearSession(context.Background(), "s2")
	if WithSessionYolo(nil, "s2") != nil {
		t.Fatal("no bypass leaves the config untouched")
	}
	on := true
	RestoreGatewayYolo(context.Background(), "s2", &on, false)
	if !IsSessionYoloEnabled("s2") {
		t.Fatal("gateway restore arms the persisted value")
	}
	off := false
	RestoreGatewayYolo(context.Background(), "s2", &off, false)
	if !IsSessionYoloEnabled("s2") {
		t.Fatal("after restoration the live toggle stays authoritative")
	}
	ClearSession(context.Background(), "s2")
	RestoreGatewayYolo(context.Background(), "s2", nil, true)
	if !IsSessionYoloEnabled("s2") {
		t.Fatal("launch policy seeds once per boundary")
	}
	DisableSessionYolo(context.Background(), "s2")
	ApplyLaunchYolo(context.Background(), "s2")
	if IsSessionYoloEnabled("s2") {
		t.Fatal("a later off sticks")
	}
}

func TestApprovalTimeoutExportsMatchUnexported(t *testing.T) {
	env := &Env{}
	if GetApprovalTimeout(env) != getApprovalTimeout(env) {
		t.Fatalf("GetApprovalTimeout differs")
	}
	for _, s := range []int{0, 1, 60, 300, 3600, 7200, 90} {
		if FormatApprovalWindow(s) != formatApprovalWindow(s) {
			t.Fatalf("FormatApprovalWindow(%d) differs", s)
		}
	}
}

func TestGetApprovalModeExportedMatchesThePrivateReader(t *testing.T) {
	env := approvalYAMLEnv(t, "approvals:\n  mode: smart\n")
	if GetApprovalMode(env) != getApprovalMode(env) || GetApprovalMode(env) != "smart" {
		t.Fatalf("GetApprovalMode %q", GetApprovalMode(env))
	}
}
