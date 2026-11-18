package tui

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Env lookup over key value pairs
func envOf(kv ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return envMap(m)
}

func TestCarriesFlagOnTheLaunchingSessionPolicy(t *testing.T) {
	for _, flag := range []string{"ICEBATH_SAFE_MODE", "ICEBATH_IGNORE_USER_CONFIG"} {
		t.Run("carries "+flag+" on the launching session policy", func(t *testing.T) {
			field := "ignore_user_config"
			if flag == "ICEBATH_SAFE_MODE" {
				field = "safe_mode"
			}
			typed, _ := localCreationOptions(envOf(flag, "1"))
			options := wireParams(typed)
			if options[field] != true {
				t.Fatalf("options %v", options)
			}
			if falsy, _ := localCreationOptions(envOf(flag, "0")); wireParams(falsy)[field] != nil {
				t.Fatal("falsy flag carried")
			}
			contract := &tuigateway.SessionCreateDescriptor{Sources: []string{"tui"}, Parameters: []string{"source", "request_id", field}}
			_, params, err := canonicalRequest("session.create", withKeys(options, "request_id", "owned"), contract)
			if err != nil || params[field] != true {
				t.Fatalf("params %v err %v", params, err)
			}
		})
	}
}

func TestRetainsPreparedIdentityAndRejectsUnsupportedTUILaunchPolicy(t *testing.T) {
	contract := &tuigateway.SessionCreateDescriptor{Sources: []string{"tui"}, Parameters: []string{"request_id", "source", "model", "cwd", "toolsets"}}
	method, params, err := canonicalRequest("session.create", map[string]any{"request_id": "fresh", "model": "local-model", "cwd": "/tmp/project"}, contract)
	want := map[string]any{"request_id": "fresh", "model": "local-model", "cwd": "/tmp/project", "source": "tui"}
	if err != nil || method != "session.create" || !reflect.DeepEqual(params, want) {
		t.Fatalf("%s %v %v", method, params, err)
	}
	if _, _, err := canonicalRequest("session.create", map[string]any{}, &tuigateway.SessionCreateDescriptor{Sources: []string{"cli"}}); err == nil || !strings.Contains(err.Error(), "tui") {
		t.Fatalf("cli only err %v", err)
	}
	if _, _, err := canonicalRequest("session.create", map[string]any{"yolo": true}, contract); err == nil || !strings.Contains(err.Error(), "yolo") {
		t.Fatalf("yolo err %v", err)
	}
	submit := wireParams(tuigateway.CanonicalSubmitParams{SessionID: "sid", InputID: ptr("prepared-id"), Text: "hello", Queued: true})
	if !reflect.DeepEqual(submit, map[string]any{"session_id": "sid", "input_id": "prepared-id", "text": "hello", "queued": true}) {
		t.Fatalf("submit %v", submit)
	}
	if got := wireParams(tuigateway.CanonicalApprovalParams{SessionID: "sid", ExecutionGeneration: 9, PromptID: "approval-9", Choice: "once"}); !reflect.DeepEqual(got, map[string]any{"session_id": "sid", "execution_generation": int64(9), "prompt_id": "approval-9", "choice": "once"}) {
		t.Fatalf("shared control %v", got)
	}
	answer, err := tuigateway.CanonicalPromptSubmit.Result(map[string]any{"admission_id": "server-admission", "ref": map[string]any{"profile_id": "/tmp/profile", "session_id": "sid"}, "status": "queued"})
	if receipt := answer.AdmissionReceipt; err != nil || receipt == nil || receipt.AdmissionID != "server-admission" || receipt.Ref.ProfileID != "/tmp/profile" || receipt.Ref.SessionID != "sid" {
		t.Fatalf("receipt %#v %v", answer, err)
	}
}

func TestCarriesSkillsCheckpointsPassSessionIDAcceptHooksOntoSessionCreate(t *testing.T) {
	typed, _ := localCreationOptions(envOf("ICEBATH_TUI_SKILLS", "a, b,a", "ICEBATH_TUI_CHECKPOINTS", "1", "ICEBATH_TUI_PASS_SESSION_ID", "1", "ICEBATH_ACCEPT_HOOKS", "1"))
	options := wireParams(typed)
	want := map[string]any{"skills": []any{"a", "b"}, "checkpoints": true, "pass_session_id": true, "accept_hooks": true}
	if !reflect.DeepEqual(options, want) {
		t.Fatalf("options %v", options)
	}
	if got, _ := localCreationOptions(envOf("ICEBATH_ACCEPT_HOOKS", "0", "ICEBATH_TUI_SKILLS", " , ")); len(wireParams(got)) != 0 {
		t.Fatalf("got %v", got)
	}
	params := []string{"request_id", "source"}
	for k := range options {
		params = append(params, k)
	}
	_, sent, err := canonicalRequest("session.create", withKeys(options, "request_id", "owned"), &tuigateway.SessionCreateDescriptor{Sources: []string{"tui"}, Parameters: params})
	for k, v := range options {
		if err != nil || !reflect.DeepEqual(sent[k], v) {
			t.Fatalf("%s sent %v err %v", k, sent[k], err)
		}
	}
}

func TestRebuildsMaxTurnsFromTheLauncherEnvironment(t *testing.T) {
	if got, _ := localCreationOptions(envOf("ICEBATH_TUI_MAX_TURNS", "5", "ICEBATH_MODEL", "local-model")); wireParams(got)["max_turns"] != int64(5) {
		t.Fatalf("max turns %v", got)
	}
	if got, _ := localCreationOptions(envOf()); got.MaxTurns != nil {
		t.Fatal("max turns without env")
	}
	var wire []string
	for _, value := range []string{"0", "-1", "none", "unlimited"} {
		got, _ := localCreationOptions(envOf("ICEBATH_TUI_MAX_TURNS", value))
		raw, _ := json.Marshal(wireParams(got)["max_turns"])
		wire = append(wire, string(raw))
	}
	if strings.Join(wire, " ") != `0 -1 0 0` {
		t.Fatalf("wire %v", wire)
	}
	if _, err := localCreationOptions(envOf("ICEBATH_TUI_MAX_TURNS", "many")); err == nil || !strings.Contains(err.Error(), "many") {
		t.Fatalf("unreadable cap err %v", err)
	}
}

func TestCarriesYoloAndIgnoreRulesOntoSessionCreate(t *testing.T) {
	for _, f := range [][2]string{{"ICEBATH_YOLO_MODE", "yolo"}, {"ICEBATH_IGNORE_RULES", "ignore_rules"}} {
		t.Run("carries "+f[0]+" onto session.create as the frozen launch flag", func(t *testing.T) {
			if set, _ := localCreationOptions(envOf(f[0], "1")); wireParams(set)[f[1]] != true {
				t.Fatal("not carried")
			}
			if falsy, _ := localCreationOptions(envOf(f[0], "0")); wireParams(falsy)[f[1]] != nil {
				t.Fatal("falsy carried")
			}
			if unset, _ := localCreationOptions(envOf()); wireParams(unset)[f[1]] != nil {
				t.Fatal("unset carried")
			}
		})
	}
}

func TestSendsTheLaunchersClientEnvironmentOnlyToAGatewayThatTakesIt(t *testing.T) {
	typed, _ := localCreationOptions(envOf("ICEBATH_TUI_CLIENT_ENV", `{"PATH": "/client/bin"}`))
	options := wireParams(typed)
	if !reflect.DeepEqual(options["client_env"], map[string]any{"PATH": "/client/bin"}) {
		t.Fatalf("client env %v", options["client_env"])
	}
	_, takes, _ := canonicalRequest("session.create", withKeys(options, "request_id", "r"), &tuigateway.SessionCreateDescriptor{Sources: []string{"tui"}, Parameters: []string{"request_id", "source", "client_env"}})
	if !reflect.DeepEqual(takes["client_env"], map[string]any{"PATH": "/client/bin"}) {
		t.Fatalf("takes %v", takes)
	}
	_, older, err := canonicalRequest("session.create", withKeys(options, "request_id", "r"), &tuigateway.SessionCreateDescriptor{Sources: []string{"tui"}, Parameters: []string{"request_id", "source"}})
	if _, has := older["client_env"]; has || err != nil {
		t.Fatalf("older %v %v", older, err)
	}
	for _, raw := range []string{"not json", "[]", `{"PATH": 1}`} {
		if got, _ := localCreationOptions(envOf("ICEBATH_TUI_CLIENT_ENV", raw)); got.ClientEnv != nil {
			t.Fatalf("%q carried", raw)
		}
	}
}
