package acp

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

var launchEnv = []string{"ICEBATH_YOLO_MODE", "ICEBATH_SAFE_MODE", "ICEBATH_IGNORE_USER_CONFIG", "ICEBATH_IGNORE_RULES",
	"ICEBATH_ACCEPT_HOOKS", "ICEBATH_MODEL", "ICEBATH_INFERENCE_MODEL", "ICEBATH_INFERENCE_PROVIDER"}

var launchFlags = []string{"yolo", "safe_mode", "ignore_user_config", "ignore_rules", "accept_hooks"}

func clearLaunchEnv(t *testing.T) {
	t.Helper()
	for _, name := range launchEnv {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

func launchAgent(t *testing.T) (*GatewayACPAgent, *[]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	created := &[]map[string]any{}
	a, link := bareGatewayAgent(t, nil)
	link.set(func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
		switch method {
		case "runtime.describe":
			return jsonObj(t, `{"session_create": {"sources": ["acp"]}, "capabilities": ["acp-editor-policy-v1"]}`), nil
		case "config.set":
			return pyjson.ObjectOf("key", params["key"], "value", params["value"], "scope", "session"), nil
		}
		if method != "session.create" {
			t.Errorf("unexpected %s", method)
		}
		mu.Lock()
		*created = append(*created, params)
		mu.Unlock()
		return pyjson.ObjectOf("session_id", "s", "info", pyjson.ObjectOf("cwd", params["cwd"])), nil
	})
	return a, created
}

func TestLaunchEnvRidesSessionCreate(t *testing.T) {
	clearLaunchEnv(t)
	for _, name := range launchEnv[:5] {
		t.Setenv(name, "1")
	}
	t.Setenv("ICEBATH_MODEL", "explicit-model")
	t.Setenv("ICEBATH_INFERENCE_PROVIDER", "openrouter")
	a, created := launchAgent(t)
	if _, err := a.NewSession(context.Background(), &pyacp.NewSessionRequest{Cwd: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	for _, flag := range launchFlags {
		if (*created)[0][flag] != true {
			t.Fatalf("%s = %v", flag, (*created)[0][flag])
		}
	}
}

func quietEntryMain(t *testing.T) {
	t.Helper()
	swap(t, &entryMain, func(context.Context, []string, io.Writer, io.Writer) int { return 0 })
}

func TestSafeModeAcpSessionFreezesTheLaunchModel(t *testing.T) {
	clearLaunchEnv(t)
	t.Setenv("ICEBATH_SAFE_MODE", "1")
	t.Setenv("ICEBATH_IGNORE_USER_CONFIG", "1")
	quietEntryMain(t)
	ctx := context.Background()
	if _, err := CmdACP(ctx, map[string]any{"model": "anthropic/claude-sonnet-4", "provider": "openrouter"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	a, created := launchAgent(t)
	if _, err := a.NewSession(ctx, &pyacp.NewSessionRequest{Cwd: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	params := (*created)[0]
	if params["model"] != "anthropic/claude-sonnet-4" || params["provider"] != "openrouter" || params["safe_mode"] != true {
		t.Fatalf("params = %v", params)
	}
	frozenParams := map[string]any{}
	for k, v := range params {
		if k != "request_id" {
			frozenParams[k] = pyjson.Plain(v)
		}
	}
	frozen, err := gateway.BypassLaunchPolicy(ctx, frozenParams)
	if err != nil {
		t.Fatal(err)
	}
	if frozen.Model == nil || *frozen.Model != "anthropic/claude-sonnet-4" || !frozen.SafeMode {
		t.Fatalf("frozen policy = %+v", frozen)
	}
}

func TestBypassWithoutAModelNamesTheMissingFlag(t *testing.T) {
	for _, flag := range []string{"ICEBATH_SAFE_MODE", "ICEBATH_IGNORE_USER_CONFIG"} {
		t.Run(flag, func(t *testing.T) {
			clearLaunchEnv(t)
			t.Setenv(flag, "1")
			a, created := launchAgent(t)
			_, err := a.NewSession(context.Background(), &pyacp.NewSessionRequest{Cwd: t.TempDir()})
			var refused *gateway.GatewayClientError
			if err == nil || !strings.Contains(err.Error(), "icebath -m <model>") || !asClientError(err, &refused) {
				t.Fatalf("err = %v", err)
			}
			if len(*created) != 0 {
				t.Fatalf("created = %v", *created)
			}
		})
	}
}

func asClientError(err error, target **gateway.GatewayClientError) bool {
	refused, ok := err.(*gateway.GatewayClientError)
	*target = refused
	return ok
}

func TestBypassWithAModelButNoProviderIsRefusedAtSessionNew(t *testing.T) {
	for _, flag := range []string{"ICEBATH_SAFE_MODE", "ICEBATH_IGNORE_USER_CONFIG"} {
		t.Run(flag, func(t *testing.T) {
			clearLaunchEnv(t)
			t.Setenv(flag, "1")
			t.Setenv("ICEBATH_MODEL", "gpt-4o-mini")
			a, created := launchAgent(t)
			_, err := a.NewSession(context.Background(), &pyacp.NewSessionRequest{Cwd: t.TempDir()})
			var refused *gateway.GatewayClientError
			if err == nil || !strings.Contains(err.Error(), "launch with --provider too") || !asClientError(err, &refused) {
				t.Fatalf("err = %v", err)
			}
			if len(*created) != 0 {
				t.Fatalf("created = %v", *created)
			}
		})
	}
}

func TestCliOnlyAcpFlagsReachTheSession(t *testing.T) {
	clearLaunchEnv(t)
	quietEntryMain(t)
	ctx := context.Background()
	if _, err := CmdACP(ctx, map[string]any{"accept_hooks": true, "ignore_rules": true}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	a, created := launchAgent(t)
	if _, err := a.NewSession(ctx, &pyacp.NewSessionRequest{Cwd: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	params := (*created)[0]
	if params["accept_hooks"] != true || params["ignore_rules"] != true {
		t.Fatalf("params = %v", params)
	}
	if _, ok := params["yolo"]; ok {
		t.Fatal("yolo rode the session")
	}
	if _, ok := params["safe_mode"]; ok {
		t.Fatal("safe_mode rode the session")
	}
}

func TestAModeChangeQueuedBehindANewWatermarkStillRepaintsTheEditor(t *testing.T) {
	const owner = `{"edit_approval_policy": "session"}`
	ctx := context.Background()
	command := func(text string) func(a *GatewayACPAgent) error {
		name, arg, _ := strings.Cut(text, " ")
		return func(a *GatewayACPAgent) error {
			_, _, err := runGatewayCommand(ctx, a, "s", name, arg)
			return err
		}
	}
	installs := []struct {
		name string
		run  func(a *GatewayACPAgent) error
	}{
		{"set_session_model", func(a *GatewayACPAgent) error {
			_, err := a.setSessionModel(ctx, "p:m", "s")
			return err
		}},
		{"deferred_confirm", func(a *GatewayACPAgent) error {
			a.confirmer = func(string) MutationConfirm {
				return func(context.Context, map[string]any) (bool, error) { return true, nil }
			}
			return a.confirmDeferredPick(ctx, "s", "s", jsonObj(t, `{"payload": {"model": "m"}, "confirm": "tok"}`))
		}},
		{"new", command("new")}, {"compress", command("compress")}, {"model", command("model p:m")},
		{"undo", command("undo")}, {"retry", command("retry")},
	}
	for _, install := range installs {
		t.Run(install.name, func(t *testing.T) {
			swap(t, &parseModelInput, func(context.Context, string, string, map[string]bool) (string, string) { return "p", "m" })
			a, link := bareGatewayAgent(t, map[string]string{"s": `{"pending": [], "replay_epoch": "e", "last_sequence": 10, "execution_generation": 1, "revision": 1}`})
			editor := &editorFake{}
			onLoop(a, func() bool { a.conn = editor; a.modes["s"] = "default"; return true })
			link.set(func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
				switch method {
				case "session.resume":
					return jsonObj(t, `{"session_id": "s", "pending": [], "prompts": [], "replay_epoch": "e", "last_sequence": 12, "execution_generation": 1, "revision": 3, "info": `+owner+`, "messages": [{"row_id": 1, "role": "user", "content": "hello"}]}`), nil
				case "session.mutate":
					return pyjson.ObjectOf("session_id", "s", "operation", params["operation"], "revision", int64(3), "model", "m", "provider", "p",
						"rewound_count", int64(1), "target_message", pyjson.ObjectOf("content", "hello")), nil
				case "session.stats":
					return jsonObj(t, `{"context_length": 1000, "context_tokens": 0}`), nil
				}
				t.Errorf("unexpected %s", method)
				return pyjson.NewObject(), nil
			})
			if err := onLoop(a, func() error { return install.run(a) }); err != nil {
				t.Fatal(err)
			}
			link.put(pyjson.ObjectOf("params", pyjson.ObjectOf("session_id", "s", "type", "session.info", "replay_epoch", "e", "seq", int64(11), "payload", jsonObj(t, owner))))
			link.put(io.ErrUnexpectedEOF)
			done := make(chan error, 1)
			go func() { done <- onLoop(a, func() error { return a.events(context.Background()) }) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the pump kept running")
			}
			if failure := onLoop(a, func() error { return a.failure }); failure != io.ErrUnexpectedEOF {
				t.Fatalf("failure = %v", failure)
			}
			var modes []string
			for _, u := range updatesOf(editor) {
				if update, ok := u.(*pyacp.CurrentModeUpdate); ok {
					modes = append(modes, update.CurrentModeID)
				}
			}
			sequence := onLoop(a, func() any {
				snapshot, _ := a.snapshots.Get("s")
				return dget(snapshot, "last_sequence")
			})
			if sequence != int64(12) || onLoop(a, func() string { return a.modes["s"] }) != "dont_ask" || !reflect.DeepEqual(modes, []string{"dont_ask"}) {
				t.Fatalf("sequence %v modes %v", sequence, modes)
			}
		})
	}
}

func TestAcpPromptSettlesAVoiceStoppedReplyAsAnEndedTurn(t *testing.T) {
	a, link := bareGatewayAgent(t, map[string]string{"s": `{}`})
	editor := &editorFake{}
	onLoop(a, func() bool { a.conn = editor; return true })
	link.set(func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
		if method == "prompt.submit" {
			return jsonObj(t, `{"voice_stopped": true}`), nil
		}
		return pyjson.NewObject(), nil
	})
	resp, err := a.Prompt(context.Background(), textPrompt("s", "stop"))
	if err != nil || resp.StopReason != "end_turn" {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	if methods := link.methods(); !reflect.DeepEqual(methods, []string{"prompt.submit"}) {
		t.Fatalf("methods = %v", methods)
	}
	held, inflight := onLoop(a, func() bool { _, ok := a.admissions["s"]; return ok }), onLoop(a, func() int { return a.inflight.Len() })
	if held || inflight != 0 {
		t.Fatalf("admission held %v inflight %d", held, inflight)
	}
	notice := i18n.T(context.Background(), "cli.voice.stop_phrase_typed", "", nil)
	found := false
	for _, text := range emittedText(editor) {
		found = found || strings.Contains(text, notice)
	}
	if !found {
		t.Fatalf("texts = %v", emittedText(editor))
	}
}

func TestSuccessorUnknownDoesNotOverwriteCompletedEditorPrompt(t *testing.T) {
	for _, beforeAck := range []bool{false, true} {
		t.Run(map[bool]string{false: "after_ack", true: "before_ack"}[beforeAck], func(t *testing.T) {
			a, link := bareGatewayAgent(t, map[string]string{"s": `{}`})
			publish := func() {
				projectJSON(t, a, `{"session_id": "s", "type": "message.complete", "admission_id": "ours", "payload": {"outcome": "completed", "text": "done"}}`)
				projectJSON(t, a, `{"session_id": "s", "type": "session.info", "payload": {"pending": [{"admission_id": "next", "status": "unknown"}]}}`)
			}
			link.set(func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
				if method != "prompt.submit" {
					t.Errorf("unexpected %s", method)
				}
				if beforeAck {
					publish()
				}
				return jsonObj(t, `{"admission_id": "ours"}`), nil
			})
			submitted := promptOn(a, context.Background(), "s", "our work")
			if !beforeAck {
				until(t, "the admission", func() bool { return onLoop(a, func() string { return a.admissions["s"] }) == "ours" })
				publish()
			}
			resp, err := submitted.wait(t, 5*time.Second)
			if err != nil || resp.StopReason != "end_turn" {
				t.Fatalf("resp = %+v, %v", resp, err)
			}
			if calls := link.log(); len(calls) != 1 {
				t.Fatalf("calls = %v", calls)
			}
		})
	}
}

func TestMislabeledOrUnadmittableImageNeverRejectsThePrompt(t *testing.T) {
	bmp := append([]byte("BM"), []byte(strings.Repeat("0", 64))...)
	jpeg := append([]byte("\xff\xd8\xff"), []byte(strings.Repeat("0", 32))...)
	cases := []struct{ name, mime string }{{"jpg-named-png", "image/png"}, {"jpeg-no-mime", "image/jpeg"}, {"bmp", ""}, {"oversize", ""}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			profileHome := filepath.Join(dir, "home")
			if err := os.MkdirAll(profileHome, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ICEBATH_HOME", profileHome)
			config.ResetCache()
			if tc.name == "oversize" {
				if err := os.WriteFile(filepath.Join(profileHome, "config.yaml"), []byte("gateway:\n  max_inbound_media_bytes: 8\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var blocks []pyacp.ContentBlock
			switch tc.name {
			case "jpg-named-png":
				path := filepath.Join(dir, "shot.jpg")
				os.WriteFile(path, pngFixture, 0o600)
				blocks = []pyacp.ContentBlock{&pyacp.ResourceContentBlock{URI: fileURI(path), Name: "shot.jpg"}}
			case "bmp":
				path := filepath.Join(dir, "pic.bmp")
				os.WriteFile(path, bmp, 0o600)
				blocks = []pyacp.ContentBlock{&pyacp.ResourceContentBlock{URI: fileURI(path), Name: "pic.bmp"}}
			case "oversize":
				blocks = []pyacp.ContentBlock{&pyacp.ImageContentBlock{Data: base64.StdEncoding.EncodeToString(pngFixture), MimeType: "image/png"}}
			default:
				blocks = []pyacp.ContentBlock{&pyacp.ImageContentBlock{Data: base64.StdEncoding.EncodeToString(jpeg), MimeType: ""}}
			}
			a := NewGatewayACPAgent()
			link := &scriptedGateway{}
			link.fakeGateway = newFakeGateway(func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
				onLoop(a, func() bool { a.terminals.Set("adm-1", pyjson.NewObject()); return true })
				return jsonObj(t, `{"admission_id": "adm-1"}`), nil
			})
			onLoop(a, func() bool { a.gateway = link; a.snapshots.Set("s", pyjson.NewObject()); return true })
			prompt := append([]pyacp.ContentBlock{pyacp.TextBlock("what is this?")}, blocks...)
			resp, err := a.Prompt(context.Background(), &pyacp.PromptRequest{Prompt: prompt, SessionID: "s"})
			if err != nil || resp.StopReason != "end_turn" {
				t.Fatalf("resp = %+v, %v", resp, err)
			}
			calls := link.log()
			if len(calls) != 1 || calls[0].method != "prompt.submit" || !strings.HasPrefix(pyStr(calls[0].params["text"]), "what is this?") {
				t.Fatalf("calls = %v", calls)
			}
			attachments, has := calls[0].params["attachments"].([]any)
			if tc.mime != "" {
				if !has || len(attachments) != 1 || attachments[0].(*pyjson.Object).GetOr("mime", nil) != tc.mime {
					t.Fatalf("attachments = %v", calls[0].params["attachments"])
				}
			} else if has || !strings.Contains(pyStr(calls[0].params["text"]), "[Image omitted:") {
				t.Fatalf("params = %v", calls[0].params)
			}
		})
	}
}
