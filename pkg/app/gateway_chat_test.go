package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Ensure that reports a configured provider
func ensureConfigured() (bool, error) { return true, nil }

// Connector that fails the case when the launch connects
func forbiddenConnector(t *testing.T) func(ctx context.Context) (gatewayChatConn, error) {
	return func(ctx context.Context) (gatewayChatConn, error) {
		t.Fatal("connected")
		return nil, nil
	}
}

// Owner fake whose session settles every submitted prompt with a completed turn
func sessionPeer(t *testing.T, describe map[string]any, snapshot map[string]any, onCreate func(params map[string]any)) *fakeGatewayPeer {
	return newFakeGatewayPeer(t, func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
		switch method {
		case "runtime.describe":
			return describe, nil
		case "session.create":
			if onCreate != nil {
				onCreate(params)
			}
			return snapshot, nil
		case "session.resume":
			return snapshot, nil
		case "prompt.submit":
			p.emit(map[string]any{"type": "message.complete", "session_id": snapshot["stored_session_id"], "admission_id": "mine",
				"payload": map[string]any{"text": "ok", "outcome": "completed"}})
			return map[string]any{"admission_id": "mine"}, nil
		case "prompt.receipt":
			return map[string]any{"status": "terminal", "result": map[string]any{"completed": true}}, nil
		}
		return snapshot, nil
	})
}

// Changes into dir for the case
func chdirFor(t *testing.T, dir string) {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
}

func TestUnsupportedLaunchOptionsFailBeforeConnection(t *testing.T) {
	tempHome(t)
	for _, option := range []string{"image", "worktree", "run_budget"} {
		_, errOut := captureStreams(t)
		if code := launchFromArgs(context.Background(), map[string]any{option: true}, ensureConfigured, forbiddenConnector(t)); code != 2 {
			t.Fatalf("%s exit %d", option, code)
		}
		if !strings.Contains(errOut.String(), strings.ReplaceAll(option, "_", "-")) {
			t.Fatalf("%s stderr %q", option, errOut.String())
		}
	}
	_, errOut := captureStreams(t)
	if code := launchFromArgs(context.Background(), map[string]any{"create_if_missing": true}, ensureConfigured, forbiddenConnector(t)); code != 2 {
		t.Fatalf("create-if-missing exit %d", code)
	}
	if !strings.Contains(errOut.String(), "create-if-missing") {
		t.Fatalf("stderr %q", errOut.String())
	}
	frozen := map[string]any{"info": map[string]any{"launch_request": map[string]any{"source": "cli", "model": "m",
		"toolsets": []any{"file"}, "reasoning": "high"}, "cwd": "/w"}}
	ctx := context.Background()
	if err := gatewaycli.CheckResumePolicy(ctx, map[string]any{"resume": "stored", "model": "m", "toolsets": "file", "reasoning": "high", "query": "x"}, frozen); err != nil {
		t.Fatalf("repeated launch flags refused: %v", err)
	}
	for _, override := range []map[string]any{{"model": "other"}, {"source": "tui"}, {"toolsets": "browser"}, {"in_dir": "/elsewhere"}} {
		args := map[string]any{"resume": "stored", "query": "x"}
		for k, v := range override {
			args[k] = v
		}
		var refused *gateway.GatewayClientError
		if err := gatewaycli.CheckResumePolicy(ctx, args, frozen); !errors.As(err, &refused) {
			t.Fatalf("%v accepted: %v", override, err)
		}
	}
	_, errOut = captureStreams(t)
	if code := launchFromArgs(ctx, map[string]any{"safe_mode": true, "query": "x"}, ensureConfigured, forbiddenConnector(t)); code != 1 {
		t.Fatalf("safe mode exit %d", code)
	}
	if !strings.Contains(errOut.String(), "--model") {
		t.Fatalf("stderr %q", errOut.String())
	}
}

func TestResumeRepeatingSourceDesktopMatchesTheStoredGUISession(t *testing.T) {
	frozen := map[string]any{"info": map[string]any{"launch_request": map[string]any{"source": "gui"}}}
	if err := gatewaycli.CheckResumePolicy(context.Background(), map[string]any{"resume": "stored", "source": "desktop", "query": "x"}, frozen); err != nil {
		t.Fatal(err)
	}
}

// Parses an icebath command line through the full CLI, the namespace of the dispatched command
func parseIcebathCommand(t *testing.T, command string) map[string]any {
	t.Helper()
	argv, err := pyshlex.SplitPOSIX(command, false)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, arg := range argv[1:] {
		if !strings.HasPrefix(arg, "<") {
			kept = append(kept, arg)
		}
	}
	c := commands.NewCLI()
	var ns map[string]any
	handler := func(cmd *cobra.Command, args []string) error {
		ns = c.Namespace(cmd, args)
		return nil
	}
	var bind func(cmd *cobra.Command)
	bind = func(cmd *cobra.Command) {
		c.Bind(strings.TrimSpace(strings.TrimPrefix(cmd.CommandPath(), c.Root.Name())), handler)
		for _, sub := range cmd.Commands() {
			bind(sub)
		}
	}
	bind(c.Root)
	if err := c.Execute(kept); err != nil {
		t.Fatalf("%q does not parse: %v", command, err)
	}
	return ns
}

func TestRefusalsNameTheReplacementAndARunnableSafeModeExample(t *testing.T) {
	tempHome(t)
	_, errOut := captureStreams(t)
	if code := launchFromArgs(context.Background(), map[string]any{"worktree": true, "run_budget": 30.0}, ensureConfigured, forbiddenConnector(t)); code != 2 {
		t.Fatalf("exit %d", code)
	}
	text := errOut.String()
	if !strings.Contains(text, "--worktree: use") || !strings.Contains(text, "icebath --tui -w") ||
		!strings.Contains(text, "--run-budget: use") || !strings.Contains(text, "run_budget_seconds") {
		t.Fatalf("stderr %q", text)
	}
	for _, name := range gatewayChatUnsupported {
		if _, ok := gatewayChatRelocated[name]; !ok {
			t.Fatalf("%s refused without saying where it went", name)
		}
	}
	_, errOut = captureStreams(t)
	if code := launchFromArgs(context.Background(), map[string]any{"safe_mode": true, "query": "x"}, ensureConfigured, forbiddenConnector(t)); code != 1 {
		t.Fatalf("exit %d", code)
	}
	example := ""
	for _, line := range strings.Split(errOut.String(), "\n") {
		if _, after, ok := strings.Cut(line, "Example: "); ok {
			example = after
			break
		}
	}
	ns := parseIcebathCommand(t, example)
	if ns["safe_mode"] != true || ns["model"] == nil || ns["model"] == "" || ns["provider"] == nil || ns["provider"] == "" || ns["query"] == nil || ns["query"] == "" {
		t.Fatalf("example %q parsed to %v", example, ns)
	}
}

func TestCreationPreservesAdvertisedCwdModelAndToolsets(t *testing.T) {
	tempHome(t)
	captureStreams(t)
	dir := t.TempDir()
	chdirFor(t, dir)
	t.Setenv("ICEBATH_ACCEPT_HOOKS", "1")
	var create map[string]any
	describe := map[string]any{"session_create": map[string]any{"sources": []any{"cli"}, "parameters": []any{
		"cwd", "model", "toolsets", "request_id", "source", "skills", "checkpoints", "accept_hooks", "pass_session_id"}}}
	peer := sessionPeer(t, describe, map[string]any{"stored_session_id": "stored"}, func(params map[string]any) { create = params })
	var submitted []string
	inner := peer.answer
	peer.answer = func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
		if method == "prompt.submit" {
			submitted = append(submitted, params["text"].(string))
		}
		return inner(p, method, params)
	}
	args := map[string]any{"query": "literal", "model": "explicit-model", "toolsets": "terminal, file", "quiet": true,
		"skills": []any{"a,b", "a"}, "checkpoints": true, "pass_session_id": true}
	code, err := runGatewayChat(context.Background(), args, nil, peer.connector())
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	resolved, _ := os.Getwd()
	if create["cwd"] != resolved || create["model"] != "explicit-model" ||
		!slices.Equal(anyStrings(create["toolsets"]), []string{"terminal", "file"}) ||
		!slices.Equal(anyStrings(create["skills"]), []string{"a", "b"}) ||
		create["checkpoints"] != true || create["pass_session_id"] != true || create["accept_hooks"] != true ||
		create["source"] != "cli" || create["request_id"] == nil || create["request_id"] == "" {
		t.Fatalf("create %v", create)
	}
	if len(submitted) != 1 || submitted[0] != "literal" {
		t.Fatalf("submitted %q", submitted)
	}
}

// Strings of a decoded JSON list
func anyStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, len(list))
	for i, item := range list {
		out[i], _ = item.(string)
	}
	return out
}

func TestFiniteRunsCreateOneshotSessionsAndExplicitSourceWins(t *testing.T) {
	tempHome(t)
	captureStreams(t)
	chdirFor(t, t.TempDir())
	sources := []any{"cli", "tool", "oneshot"}
	var created []any
	describe := func() map[string]any {
		return map[string]any{"session_create": map[string]any{"sources": sources, "parameters": []any{"cwd", "request_id", "source"}}}
	}
	run := func(args map[string]any) {
		t.Helper()
		peer := sessionPeer(t, describe(), map[string]any{"stored_session_id": "stored"}, func(params map[string]any) { created = append(created, params["source"]) })
		if code, err := runGatewayChat(context.Background(), args, nil, peer.connector()); err != nil || code != 0 {
			t.Fatalf("%v: code %d err %v", args, code, err)
		}
	}
	run(map[string]any{"query": "q", "quiet": true})
	run(map[string]any{"oneshot": "z"})
	run(map[string]any{"query": "q", "quiet": true, "source": "tool"})
	sources = []any{"cli", "tool"}
	run(map[string]any{"query": "q", "quiet": true})
	if !slices.Equal(created, []any{"oneshot", "oneshot", "tool", "cli"}) {
		t.Fatalf("created %v", created)
	}
}

func TestRPCPreservesNotificationsAndErrors(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			_, raw, err := ws.ReadMessage()
			if err != nil {
				return
			}
			decoded, _ := pyjson.Loads(string(raw))
			request, _ := decoded.(*pyjson.Object)
			event, _ := pyjson.Dumps(pyjson.ObjectOf("method", "message.complete", "params", pyjson.ObjectOf("text", "reply")))
			answer, _ := pyjson.Dumps(pyjson.ObjectOf("id", request.Vals["id"], "error", pyjson.ObjectOf("message", "stale_generation")))
			_ = ws.WriteMessage(websocket.TextMessage, []byte(event))
			_ = ws.WriteMessage(websocket.TextMessage, []byte(answer))
		}
	}))
	defer server.Close()
	t.Setenv("ICEBATH_TUI_GATEWAY_URL", "ws"+strings.TrimPrefix(server.URL, "http"))
	client, err := gateway.ConnectGateway(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, err = client.RPC(context.Background(), "session.interrupt", map[string]any{"session_id": "stored", "execution_generation": 4})
	var refused *gateway.GatewayClientError
	if !errors.As(err, &refused) || !strings.Contains(refused.Msg, "stale_generation") {
		t.Fatalf("rpc error %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	event, err := client.EventQueue().Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	frame, _ := pyjson.Plain(event).(map[string]any)
	if frame["params"].(map[string]any)["text"] != "reply" {
		t.Fatalf("event %v", frame)
	}
}

func TestYoloSlashTogglesTheSessionBypassOnTheOwner(t *testing.T) {
	out, _ := captureStreams(t)
	peer := newFakeGatewayPeer(t, func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
		value, ok := params["value"]
		if !ok {
			value = "0"
		}
		return map[string]any{"key": "yolo", "value": value, "scope": "session"}, nil
	})
	view := newGatewayChatView(peer, map[string]any{"stored_session_id": "sid"}, false, nil, "")
	for _, line := range []string{"/yolo off", "/yolo"} {
		keep, err := viewCommand(view, line)
		if err != nil || !keep {
			t.Fatalf("%s: keep %v err %v", line, keep, err)
		}
	}
	calls := peer.recorded()
	if len(calls) != 2 || calls[0].method != "config.set" || len(calls[0].params) != 3 || calls[0].params["value"] != "0" ||
		calls[0].params["session_id"] != "sid" || calls[0].params["key"] != "yolo" ||
		calls[1].method != "config.set" || len(calls[1].params) != 2 || calls[1].params["key"] != "yolo" {
		t.Fatalf("calls %v", calls)
	}
	if !strings.Contains(out.String(), "YOLO off for this session") {
		t.Fatalf("stdout %q", out.String())
	}
}

func TestEveryRelocationHintNamesACommandThatParses(t *testing.T) {
	pattern := regexp.MustCompile("`(icebath [^`]*)`")
	for _, hint := range gatewayChatRelocated {
		for _, m := range pattern.FindAllStringSubmatch(hint, -1) {
			parseIcebathCommand(t, m[1])
		}
	}
}

func TestOneLetterAliasRefusalNamesAFlagTheUserCanType(t *testing.T) {
	tempHome(t)
	_, errOut := captureStreams(t)
	if code := launchFromArgs(context.Background(), map[string]any{"w": true}, ensureConfigured, forbiddenConnector(t)); code != 2 {
		t.Fatalf("exit %d", code)
	}
	text := errOut.String()
	if !strings.Contains(text, "options: -w.") || !strings.Contains(text, "\n  -w: use `icebath --tui -w`") || strings.Contains(text, "--w") {
		t.Fatalf("stderr %q", text)
	}
}

func TestDocumentedEnvOptInsRideTheClassicCreateAndBypassNeedsAModel(t *testing.T) {
	tempHome(t)
	_, errOut := captureStreams(t)
	chdirFor(t, t.TempDir())
	flags := []any{"yolo", "ignore_rules", "safe_mode", "ignore_user_config", "accept_hooks"}
	var created []map[string]any
	describe := map[string]any{"session_create": map[string]any{"sources": []any{"cli"},
		"parameters": append([]any{"cwd", "model", "request_id", "source"}, flags...)}}
	for _, name := range []string{"ICEBATH_YOLO_MODE", "ICEBATH_IGNORE_RULES", "ICEBATH_SAFE_MODE", "ICEBATH_ACCEPT_HOOKS"} {
		t.Setenv(name, "1")
	}
	t.Setenv("ICEBATH_IGNORE_USER_CONFIG", "true")
	peer := sessionPeer(t, describe, map[string]any{"stored_session_id": "stored"}, func(params map[string]any) { created = append(created, params) })
	if code, err := runGatewayChat(context.Background(), map[string]any{"query": "q", "quiet": true, "model": "m"}, nil, peer.connector()); err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	for _, flag := range flags {
		if created[0][flag.(string)] != true {
			t.Fatalf("%s missing from %v", flag, created[0])
		}
	}
	for _, name := range []string{"ICEBATH_YOLO_MODE", "ICEBATH_IGNORE_RULES", "ICEBATH_SAFE_MODE", "ICEBATH_ACCEPT_HOOKS"} {
		os.Unsetenv(name)
	}
	errOut.Reset()
	if code := launchFromArgs(context.Background(), map[string]any{"query": "q"}, ensureConfigured, forbiddenConnector(t)); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errOut.String(), "--model") {
		t.Fatalf("stderr %q", errOut.String())
	}
}

func TestLaunchKeyFlagsAreNeverSilentlyDropped(t *testing.T) {
	tempHome(t)
	captureStreams(t)
	dir := t.TempDir()
	answer := func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
		switch method {
		case "runtime.describe":
			return map[string]any{"session_create": map[string]any{"sources": []any{"cli"}, "parameters": []any{"cwd", "model", "api_key", "source"}}}, nil
		case "session.info":
			return map[string]any{"launch_request": map[string]any{"source": "cli", "model": "m"}, "cwd": dir}, nil
		case "prompt.submit":
			p.emit(map[string]any{"type": "message.complete", "session_id": "stored", "admission_id": "mine",
				"payload": map[string]any{"text": "ok", "outcome": "completed"}})
			return map[string]any{"admission_id": "mine"}, nil
		case "prompt.receipt":
			return map[string]any{"status": "terminal", "result": map[string]any{"completed": true}}, nil
		}
		return map[string]any{"stored_session_id": "stored", "execution_generation": 0,
			"info": map[string]any{"launch_request": map[string]any{"source": "cli", "model": "m"}}}, nil
	}
	peer := newFakeGatewayPeer(t, answer)
	args := map[string]any{"resume": "stored", "api_key": "launch-key", "query": "q", "quiet": true}
	if code, err := runGatewayChat(context.Background(), args, nil, peer.connector()); err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	sent := map[string]map[string]any{}
	for _, c := range peer.recorded() {
		sent[c.method] = c.params
	}
	if resume := sent["session.resume"]; len(resume) != 2 || resume["session_id"] != "stored" || resume["api_key"] != "launch-key" {
		t.Fatalf("resume %v", resume)
	}
	newPeer := newFakeGatewayPeer(t, answer)
	view := newGatewayChatView(newPeer, map[string]any{"stored_session_id": "stored", "execution_generation": 0}, true, nil, "")
	view.launchAPIKey = "launch-key"
	if err := onViewLoop(view, func() error { return gatewayNew(context.Background(), view, "") }); err != nil {
		t.Fatal(err)
	}
	for _, c := range newPeer.recorded() {
		if c.method == "session.create" && c.params["api_key"] != "launch-key" {
			t.Fatalf("create %v", c.params)
		}
	}
}

func TestNewAndBranchMoveTheTerminalContinuePointer(t *testing.T) {
	for _, command := range []string{"/new", "/branch"} {
		h := t.TempDir()
		t.Setenv("ICEBATH_HOME", h)
		t.Setenv("TMUX_PANE", "%9")
		captureStreams(t)
		peer := newFakeGatewayPeer(t, func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
			switch method {
			case "runtime.describe":
				return map[string]any{"session_create": map[string]any{"sources": []any{"cli"}, "parameters": []any{"cwd", "source"}}}, nil
			case "session.info":
				return map[string]any{"launch_request": map[string]any{"source": "cli"}, "cwd": h}, nil
			case "session.mutate":
				return map[string]any{"status": "applied", "branched_session_id": "moved"}, nil
			}
			return map[string]any{"stored_session_id": "moved", "execution_generation": 0, "revision": 1, "info": map[string]any{}}, nil
		})
		view := newGatewayChatView(peer, map[string]any{"stored_session_id": "old", "execution_generation": 0, "info": map[string]any{}}, true, nil, "")
		WriteBreadcrumb(context.Background(), "old", "")
		var err error
		if command == "/new" {
			err = onViewLoop(view, func() error { return gatewayNew(context.Background(), view, "") })
		} else {
			err = onViewLoop(view, func() error {
				keep, err := view.command(context.Background(), command)
				if err == nil && !keep {
					t.Fatal("branch detached")
				}
				return err
			})
		}
		if err != nil {
			t.Fatalf("%s: %v", command, err)
		}
		if got := ReadBreadcrumb(context.Background())["session_id"]; got != "moved" {
			t.Fatalf("%s: breadcrumb %v", command, got)
		}
	}
}

func TestOneshotZWritesNoSessionLinesToStderrLikeMain(t *testing.T) {
	tempHome(t)
	chdirFor(t, t.TempDir())
	describe := map[string]any{"session_create": map[string]any{"sources": []any{"cli", "oneshot"}, "parameters": []any{"cwd", "request_id", "source"}}}
	_, errOut := captureStreams(t)
	peer := sessionPeer(t, describe, map[string]any{"stored_session_id": "stored-z"}, nil)
	if code, err := runGatewayChat(context.Background(), map[string]any{"oneshot": "z"}, nil, peer.connector()); err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	if strings.Contains(errOut.String(), "stored-z") {
		t.Fatalf("stderr %q", errOut.String())
	}
	_, errOut = captureStreams(t)
	peer = sessionPeer(t, describe, map[string]any{"stored_session_id": "stored-z"}, nil)
	if code, err := runGatewayChat(context.Background(), map[string]any{"query": "q", "quiet": true}, nil, peer.connector()); err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	if text := errOut.String(); !strings.Contains(text, "Session: stored-z") || !strings.Contains(text, "session_id: stored-z") {
		t.Fatalf("stderr %q", text)
	}
}
