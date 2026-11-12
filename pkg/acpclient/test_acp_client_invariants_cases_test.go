package acpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

func acmeVendor() Vendor {
	v := BaseVendor()
	v.DefaultCommand, v.DefaultArgs = "acme-agent", []string{"--acp", "--stdio"}
	v.CommandEnvVars, v.ArgsEnvVar = []string{"ACME_ACP_COMMAND"}, "ACME_ACP_ARGS"
	v.MarkerBaseURL, v.PlaceholderModel, v.Label = "acp://acme", "acme-acp", "Acme ACP"
	return v
}

func acmeClient(t *testing.T, o Options) *Client {
	t.Helper()
	acpProbe.Lock()
	acpProbe.verdicts = map[string]bool{}
	acpProbe.Unlock()
	c, err := New(context.Background(), acmeVendor(), o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func hermeticHome(t *testing.T) string {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(dir, "icebath"))
	return dir
}

func dispatch(t *testing.T, c *Client, msg map[string]any, cwd string) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	handled, err := c.handleServerMessage(msg, &buf, cwd, &[]string{}, &[]string{}, true)
	if err != nil || !handled {
		t.Fatalf("handled %v err %v", handled, err)
	}
	payload := strings.TrimSpace(buf.String())
	if payload == "" {
		t.Fatal("the client must answer every agent request")
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func acpRequest(method string, params map[string]any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": params}
}

func resultContent(response map[string]any) string {
	result, _ := response["result"].(map[string]any)
	s, _ := result["content"].(string)
	return s
}

func TestAgentPermissionRequestIsAnsweredCancelledNeverGranted(t *testing.T) {
	tmp := hermeticHome(t)
	c := acmeClient(t, Options{ACPCwd: tmp})
	response := dispatch(t, c, acpRequest("session/request_permission", map[string]any{
		"sessionId": "s1", "toolCall": map[string]any{"toolCallId": "tc1", "title": "rm -rf /", "kind": "execute"},
		"options": []any{map[string]any{"optionId": "allow-always", "name": "Always", "kind": "allow_always"},
			map[string]any{"optionId": "allow-once", "name": "Allow", "kind": "allow_once"}}}), tmp)
	if response["id"] != float64(7) || !reflect.DeepEqual(response["result"].(map[string]any)["outcome"], map[string]any{"outcome": "cancelled"}) {
		t.Fatalf("%v", response)
	}
}

func TestReadTextFileRedactsSensitiveContent(t *testing.T) {
	for _, toggle := range []string{"true", "false"} {
		t.Run("redaction-"+map[string]string{"true": "on", "false": "off"}[toggle], func(t *testing.T) {
			tmp := hermeticHome(t)
			t.Setenv("ICEBATH_REDACT_SECRETS", toggle)
			c := acmeClient(t, Options{ACPCwd: tmp})
			secret := filepath.Join(tmp, "config.env")
			os.WriteFile(secret, []byte("OPENAI_API_KEY=sk-proj-abc123def456ghi789jkl012"), 0o600)
			content := resultContent(dispatch(t, c, acpRequest("fs/read_text_file", map[string]any{"path": secret}), tmp))
			if strings.Contains(content, "abc123def456") || !strings.Contains(content, "OPENAI_API_KEY=") {
				t.Fatalf("content %q", content)
			}
		})
	}
}

func TestReadTextFileOutsideTheSessionCwdIsRefused(t *testing.T) {
	tmp := hermeticHome(t)
	c := acmeClient(t, Options{ACPCwd: tmp})
	workspace := filepath.Join(tmp, "workspace")
	os.Mkdir(workspace, 0o755)
	outside := filepath.Join(tmp, "outside.txt")
	os.WriteFile(outside, []byte("OUTSIDE-SECRET-5512"), 0o600)
	for _, path := range []string{outside, workspace + "/../outside.txt"} {
		response := dispatch(t, c, acpRequest("fs/read_text_file", map[string]any{"path": path}), workspace)
		if b, _ := json.Marshal(response); response["error"] == nil || strings.Contains(string(b), "OUTSIDE-SECRET") {
			t.Fatalf("%s", b)
		}
	}
}

func TestReadTextFileThroughASymlinkOutOfTheCwdIsRefused(t *testing.T) {
	tmp := hermeticHome(t)
	c := acmeClient(t, Options{ACPCwd: tmp})
	workspace := filepath.Join(tmp, "workspace")
	os.Mkdir(workspace, 0o755)
	os.WriteFile(filepath.Join(tmp, "outside.txt"), []byte("OUTSIDE-SECRET-5512"), 0o600)
	link := filepath.Join(workspace, "inside-looking.txt")
	if err := os.Symlink(filepath.Join(tmp, "outside.txt"), link); err != nil {
		t.Skip("symlinks unavailable on this host")
	}
	response := dispatch(t, c, acpRequest("fs/read_text_file", map[string]any{"path": link}), workspace)
	if b, _ := json.Marshal(response); response["error"] == nil || strings.Contains(string(b), "OUTSIDE-SECRET") {
		t.Fatalf("%s", b)
	}
}

func TestRelativeFsPathsAreRefused(t *testing.T) {
	tmp := hermeticHome(t)
	c := acmeClient(t, Options{ACPCwd: tmp})
	os.WriteFile(filepath.Join(tmp, "note.txt"), []byte("hello"), 0o600)
	if response := dispatch(t, c, acpRequest("fs/read_text_file", map[string]any{"path": "note.txt"}), tmp); response["error"] == nil {
		t.Fatalf("%v", response)
	}
}

func TestFsReadTextFileDecodesAsUtf8UnderNonUtf8Locale(t *testing.T) {
	tmp := hermeticHome(t)
	t.Setenv("LC_ALL", "zh_CN.GBK")
	c := acmeClient(t, Options{ACPCwd: tmp})
	target := filepath.Join(tmp, "note.md")
	os.WriteFile(target, []byte("# 中文标题\nem dash — here\n"), 0o600)
	response := dispatch(t, c, acpRequest("fs/read_text_file", map[string]any{"path": target}), tmp)
	content := resultContent(response)
	if response["error"] != nil || !strings.Contains(content, "中文标题") || !strings.Contains(content, "em dash —") {
		t.Fatalf("%v", response)
	}
}

func TestWriteTextFileOutsideTheSessionCwdIsRefused(t *testing.T) {
	tmp := hermeticHome(t)
	c := acmeClient(t, Options{ACPCwd: tmp})
	workspace := filepath.Join(tmp, "workspace")
	os.Mkdir(workspace, 0o755)
	outside := filepath.Join(tmp, "outside.txt")
	response := dispatch(t, c, acpRequest("fs/write_text_file", map[string]any{"path": outside, "content": "pwned"}), workspace)
	if _, err := os.Stat(outside); response["error"] == nil || err == nil {
		t.Fatalf("%v", response)
	}
}

func TestWriteTextFileRespectsSafeRoot(t *testing.T) {
	tmp := hermeticHome(t)
	c := acmeClient(t, Options{ACPCwd: tmp})
	safeRoot := filepath.Join(tmp, "workspace")
	os.Mkdir(safeRoot, 0o755)
	outside := filepath.Join(tmp, "outside.txt")
	t.Setenv("ICEBATH_WRITE_SAFE_ROOT", safeRoot)
	response := dispatch(t, c, acpRequest("fs/write_text_file", map[string]any{"path": outside, "content": "should-not-write"}), tmp)
	if _, err := os.Stat(outside); response["error"] == nil || !strings.Contains(fmt.Sprint(response["error"]), "ICEBATH_WRITE_SAFE_ROOT") || err == nil {
		t.Fatalf("%v", response)
	}
}

func TestWriteTextFileInsideTheCwdLands(t *testing.T) {
	tmp := hermeticHome(t)
	c := acmeClient(t, Options{ACPCwd: tmp})
	target := filepath.Join(tmp, "sub", "out.txt")
	response := dispatch(t, c, acpRequest("fs/write_text_file", map[string]any{"path": target, "content": "written"}), tmp)
	if got, _ := os.ReadFile(target); response["error"] != nil || string(got) != "written" {
		t.Fatalf("%v %q", response, got)
	}
}

func TestUnknownAgentRequestGetsMethodNotFound(t *testing.T) {
	tmp := hermeticHome(t)
	c := acmeClient(t, Options{ACPCwd: tmp})
	response := dispatch(t, c, acpRequest("terminal/create", map[string]any{"command": "id"}), tmp)
	if response["error"].(map[string]any)["code"] != float64(-32601) {
		t.Fatalf("%v", response)
	}
}

// Client launching the test binary as the fake agent, every spawned process recorded in order
func recordingClient(t *testing.T, mode string, spawned *[]*process) *Client {
	t.Helper()
	tmp := hermeticHome(t)
	t.Setenv("ACPCLIENT_FAKE", mode)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	prev := startProcess
	startProcess = func(argv []string, cwd string, env map[string]string) (*process, error) {
		p, err := prev(argv, cwd, env)
		if err == nil {
			*spawned = append(*spawned, p)
		}
		return p, err
	}
	t.Cleanup(func() {
		startProcess = prev
		for _, p := range *spawned {
			if _, ended := p.poll(); !ended {
				p.cmd.Process.Kill()
			}
		}
	})
	return acmeClient(t, Options{Command: exe, Args: []string{"fake"}, ACPCwd: tmp})
}

func TestOverlappingSessionsReapTheirOwnProcess(t *testing.T) {
	var spawned []*process
	c := recordingClient(t, "server", &spawned)
	ctx := context.Background()
	a, err := c.openSession(ctx, 30, true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.openSession(ctx, 30, true)
	if err != nil {
		t.Fatal(err)
	}
	a.close()
	_, aEnded := spawned[0].poll()
	_, bEnded := spawned[1].poll()
	if !aEnded || bEnded {
		t.Fatalf("session A teardown: own child leaked=%v, sibling process killed=%v", !aEnded, bEnded)
	}
	if c.IsClosed() {
		t.Fatal("a shared client is not closed while a sibling session is live")
	}
	b.close()
	if _, ended := spawned[1].poll(); !ended || !c.IsClosed() {
		t.Fatal("the last session to drain still flips is_closed for single-session callers")
	}
}

func TestCloseTerminatesEveryLiveSessionProcess(t *testing.T) {
	var spawned []*process
	c := recordingClient(t, "server", &spawned)
	ctx := context.Background()
	for range 2 {
		if _, err := c.openSession(ctx, 30, true); err != nil {
			t.Fatal(err)
		}
	}
	c.Close()
	for _, p := range spawned {
		if _, ended := p.poll(); !ended {
			t.Fatal("a live session process survived close")
		}
	}
	if !c.IsClosed() {
		t.Fatal("close must mark the client closed")
	}
}

func TestCliDeathIsReportedAsACrashNotATimeout(t *testing.T) {
	var spawned []*process
	c := recordingClient(t, "crash", &spawned)
	err := c.withSession(context.Background(), 30, true, func(*session) error { return nil })
	var pe *Error
	if !errors.As(err, &pe) || pe.Kind != "RuntimeError" || !strings.Contains(pe.Msg, "exited early: fatal: agent segfaulted") {
		t.Fatalf("%v", err)
	}
}

// Wraps the start seam so a session begins only after the fixture CLI closed its stdin
func waitForStdinClose(t *testing.T, marker string) {
	t.Setenv("ACPCLIENT_FAKE_MARK", marker)
	prev := startProcess
	startProcess = func(argv []string, cwd string, env map[string]string) (*process, error) {
		p, err := prev(argv, cwd, env)
		for deadline := time.Now().Add(30 * time.Second); err == nil && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if _, serr := os.Stat(marker); serr == nil {
				return p, nil
			}
		}
		return p, err
	}
	t.Cleanup(func() { startProcess = prev })
}

func TestCliThatStopsReadingIsReportedWithItsExitText(t *testing.T) {
	var spawned []*process
	c := recordingClient(t, "stdin-close", &spawned)
	waitForStdinClose(t, filepath.Join(t.TempDir(), "stdin-closed"))
	err := c.withSession(context.Background(), 30, true, func(*session) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "exited early: fatal: agent cannot start") {
		t.Fatalf("%v", err)
	}
}

func TestCliThatStopsReadingButKeepsRunningFailsFast(t *testing.T) {
	var spawned []*process
	c := recordingClient(t, "deaf", &spawned)
	waitForStdinClose(t, filepath.Join(t.TempDir(), "stdin-closed"))
	started := time.Now()
	err := c.withSession(context.Background(), 900, true, func(*session) error { return nil })
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) || !errors.Is(err, syscall.EPIPE) {
		t.Fatalf("%v", err)
	}
	if time.Since(started) >= 60*time.Second {
		t.Fatal("a deaf CLI must fail fast")
	}
}

func TestModelDiscoveryDoesNotAllowFileRequests(t *testing.T) {
	var spawned []*process
	c := recordingClient(t, "fs-request", &spawned)
	target := filepath.Join(t.TempDir(), "should-not-be-read.txt")
	os.WriteFile(target, []byte("private"), 0o600)
	t.Setenv("ACPCLIENT_FAKE_TARGET", target)
	models, err := c.ListModels(context.Background(), 30)
	if err != nil || !reflect.DeepEqual(models, []string{"model-sol"}) {
		t.Fatalf("%v %v", models, err)
	}
}

// Probe and start doubles that report a missing binary and capture the env the spawn passed
func captureFailedStart(t *testing.T, captured *map[string]string) {
	prevProbe, prevStart := probeRun, startProcess
	probeRun = func(context.Context, []string) (*pysubprocess.Completed, error) {
		return nil, &fs.PathError{Op: "fork/exec", Path: "acme-agent", Err: fs.ErrNotExist}
	}
	startProcess = func(argv []string, cwd string, env map[string]string) (*process, error) {
		*captured = env
		return nil, &fs.PathError{Op: "fork/exec", Path: argv[0], Err: fs.ErrNotExist}
	}
	t.Cleanup(func() { probeRun, startProcess = prevProbe, prevStart })
}

func launchClient(t *testing.T, tmp string) *Client {
	return acmeClient(t, Options{APIKey: "acme-acp", BaseURL: "acp://acme", ACPCommand: "acme-agent", ACPArgs: []string{"--acp", "--stdio"}, ACPCwd: tmp})
}

func TestRunPromptPreservesRealHomeWhenProfileHomeAvailable(t *testing.T) {
	tmp := t.TempDir()
	icebathHome := filepath.Join(tmp, "icebath")
	os.MkdirAll(filepath.Join(icebathHome, "home"), 0o755)
	realHome := filepath.Join(tmp, "real-home")
	os.Mkdir(realHome, 0o755)
	t.Setenv("HOME", realHome)
	t.Setenv("ICEBATH_HOME", icebathHome)
	t.Setenv("ICEBATH_REAL_HOME", "")
	os.Unsetenv("ICEBATH_REAL_HOME")
	t.Setenv("TERMINAL_HOME_MODE", "")
	os.Unsetenv("TERMINAL_HOME_MODE")
	var captured map[string]string
	captureFailedStart(t, &captured)
	_, _, err := launchClient(t, tmp).runPrompt(context.Background(), "hello", 1, "")
	if err == nil || !strings.Contains(err.Error(), "Could not start Acme ACP command") {
		t.Fatalf("%v", err)
	}
	if captured["HOME"] != realHome || captured["ICEBATH_REAL_HOME"] != realHome {
		t.Fatalf("HOME %q ICEBATH_REAL_HOME %q", captured["HOME"], captured["ICEBATH_REAL_HOME"])
	}
}

func TestRunPromptPassesHomeWhenParentEnvIsClean(t *testing.T) {
	tmp := hermeticHome(t)
	t.Setenv("HOME", "")
	os.Unsetenv("HOME")
	var captured map[string]string
	captureFailedStart(t, &captured)
	_, _, err := launchClient(t, tmp).runPrompt(context.Background(), "hello", 1, "")
	if err == nil || !strings.Contains(err.Error(), "Could not start Acme ACP command") || captured["HOME"] == "" {
		t.Fatalf("%v %q", err, captured["HOME"])
	}
}

// Probe double answering one completed help run
func probeAnswers(t *testing.T, returncode int, stdout string, calls *int) {
	prev := probeRun
	probeRun = func(context.Context, []string) (*pysubprocess.Completed, error) {
		*calls++
		return &pysubprocess.Completed{Returncode: returncode, Stdout: []byte(stdout)}, nil
	}
	t.Cleanup(func() { probeRun = prev })
}

func TestProbeTrueWhenHelpAdvertisesAcp(t *testing.T) {
	hermeticHome(t)
	acmeClient(t, Options{ACPCwd: t.TempDir()})
	var calls int
	probeAnswers(t, 0, "Usage: acme-agent [--acp] [--stdio]", &calls)
	if v := acpSupported(context.Background(), "acme-agent", []string{"--acp", "--stdio"}); v == nil || !*v {
		t.Fatal("help advertising --acp must probe true")
	}
}

func TestProbeFalseWhenHelpLacksAcpAndRunPromptFastFails(t *testing.T) {
	tmp := hermeticHome(t)
	var calls int
	probeAnswers(t, 0, "Usage: other [--print] [--model]", &calls)
	_, _, err := launchClient(t, tmp).runPrompt(context.Background(), "hello", 1, "")
	if err == nil || !strings.Contains(err.Error(), "ACP transport not supported") {
		t.Fatalf("%v", err)
	}
}

func TestProbeInconclusiveFallsThroughToSpawnError(t *testing.T) {
	tmp := hermeticHome(t)
	var captured map[string]string
	captureFailedStart(t, &captured)
	_, _, err := launchClient(t, tmp).runPrompt(context.Background(), "hello", 1, "")
	if err == nil || !strings.Contains(err.Error(), "Could not start Acme ACP command") {
		t.Fatalf("%v", err)
	}
}

func TestProbeSkippedForCustomArgsWithoutAcp(t *testing.T) {
	var calls int
	probeAnswers(t, 0, "", &calls)
	if v := acpSupported(context.Background(), "mycli", []string{"--custom-transport"}); v == nil || !*v || calls != 0 {
		t.Fatalf("verdict %v calls %d", v, calls)
	}
}

func sessionWithConfigOptions() map[string]any {
	return map[string]any{"sessionId": "s1", "configOptions": []any{map[string]any{
		"id": "model", "category": "model", "type": "select", "currentValue": "auto",
		"options": []any{map[string]any{"value": "auto", "name": "Auto"}, map[string]any{"value": "model-terra", "name": "Terra"},
			map[string]any{"value": "model-fable", "name": "Fable", "_meta": map[string]any{"acmePolicy": "off"}}}}}}
}

func acmeDisabled(entry any) bool {
	meta, _ := dictGet(entry, "_meta").(map[string]any)
	return meta["acmePolicy"] == "off"
}

func selection(t *testing.T, session map[string]any, model, placeholder string, disabled func(any) bool) (string, map[string]any, bool) {
	t.Helper()
	method, params, ok, err := modelSelectionRequest(session, model, placeholder, disabled)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return "", nil, false
	}
	out := map[string]any{}
	for _, k := range params.Keys {
		out[k] = params.Vals[k]
	}
	return method, out, true
}

func TestModelSelectionPrefersStableConfigOption(t *testing.T) {
	method, params, ok := selection(t, sessionWithConfigOptions(), "model-terra", "", neverDisabled)
	if !ok || method != "session/set_config_option" || !reflect.DeepEqual(params, map[string]any{"sessionId": "s1", "configId": "model", "value": "model-terra"}) {
		t.Fatalf("%s %v", method, params)
	}
}

func TestModelSelectionRejectsDisabledConfigOption(t *testing.T) {
	if _, _, ok := selection(t, sessionWithConfigOptions(), "model-fable", "", acmeDisabled); ok {
		t.Fatal("a disabled model must not be selected")
	}
}

func TestModelSelectionRejectsUnknownConfigOption(t *testing.T) {
	if _, _, ok := selection(t, sessionWithConfigOptions(), "not-served-here", "", neverDisabled); ok {
		t.Fatal("an unknown model must not be selected")
	}
}

func TestModelSelectionFallsBackToLegacyExtension(t *testing.T) {
	legacy := map[string]any{"sessionId": "s1", "models": map[string]any{"availableModels": []any{map[string]any{"modelId": "auto"}, map[string]any{"modelId": "model-terra"}}}}
	method, params, ok := selection(t, legacy, "model-terra", "", neverDisabled)
	if !ok || method != "session/set_model" || !reflect.DeepEqual(params, map[string]any{"sessionId": "s1", "modelId": "model-terra"}) {
		t.Fatalf("%s %v", method, params)
	}
}

func TestModelSelectionSkipsProviderPlaceholderSlug(t *testing.T) {
	if _, _, ok := selection(t, sessionWithConfigOptions(), "acme-acp", "acme-acp", neverDisabled); ok {
		t.Fatal("the placeholder slug means the session default")
	}
}
