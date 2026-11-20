package tuiserver

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/setupflow"
)

// Isolated home holding the given config.yaml and .env text
func setupSectionHome(t *testing.T, cfgText, envText string) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeTestFile(t, filepath.Join(h, "config.yaml"), cfgText)
	writeTestFile(t, filepath.Join(h, ".env"), envText)
	config.ResetCache()
	dropCfgCache()
	t.Cleanup(func() {
		config.ResetCache()
		dropCfgCache()
	})
	return h
}

// Response of one request routed through its contract, params and result checked
func contractCall(t *testing.T, method string, params map[string]any) map[string]any {
	t.Helper()
	return handleRequest(context.Background(), map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": params})
}

// Text of a file under the home
func homeFile(t *testing.T, h, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestSetupSectionLoadAnswersNamedValuesWithTheirSubject(t *testing.T) {
	setupSectionHome(t, "model:\n  default: m\n", "")
	var subjects []string
	setupSectionValues["test.echo"] = func(_ context.Context, _ string, _ config.Tree, subject string) (any, error) {
		subjects = append(subjects, subject)
		return map[string]any{"subject": subject}, nil
	}
	t.Cleanup(func() { delete(setupSectionValues, "test.echo") })
	res := resultOf(t, contractCall(t, "setup.section.load", map[string]any{"values": []any{"test.echo:/some/dir", "test.echo"}}))
	want := map[string]any{"test.echo:/some/dir": map[string]any{"subject": "/some/dir"}, "test.echo": map[string]any{"subject": ""}}
	if !reflect.DeepEqual(res["values"], want) || !reflect.DeepEqual(subjects, []string{"/some/dir", ""}) {
		t.Fatalf("values %v subjects %v", res["values"], subjects)
	}
	resp := contractCall(t, "setup.section.load", map[string]any{"values": []any{"test.missing"}})
	if cfgErrCode(resp) != 4001 {
		t.Fatalf("unknown value answered %v", resp)
	}
}

func TestSetupSectionFullSaveWritesAsTheWizardSaves(t *testing.T) {
	h := setupSectionHome(t, "model:\n  default: m\n", "OPENAI_API_KEY=sk\n")
	params := map[string]any{"full_save": true, "writes": []any{
		map[string]any{"target": "config", "key": "terminal.backend", "kind": "text", "value": "local"},
		map[string]any{"target": "config", "key": "proxy.enabled", "kind": "bool", "value": true},
		map[string]any{"target": "env", "key": "TERMINAL_ENV", "kind": "text", "value": "local"},
	}}
	res := resultOf(t, contractCall(t, "setup.section.save", params))
	if res["config"].(map[string]any)["terminal.backend"] != "local" {
		t.Fatalf("result %v", res)
	}
	saved := homeFile(t, h, "config.yaml")
	if strings.Contains(saved, "backend: local") || !strings.Contains(saved, "proxy:\n  enabled: true") || !strings.Contains(saved, "_config_version:") {
		t.Fatalf("config.yaml\n%s", saved)
	}
	if env := homeFile(t, h, ".env"); env != "OPENAI_API_KEY=sk\nTERMINAL_ENV=local\n" {
		t.Fatalf(".env %q", env)
	}
	backups, _ := filepath.Glob(filepath.Join(h, "backups", "config", "config.yaml.pre-setup.*"))
	if len(backups) != 1 {
		t.Fatalf("pre-setup backups %v", backups)
	}
	before := homeFile(t, h, "config.yaml")
	resultOf(t, contractCall(t, "setup.section.save", map[string]any{"full_save": true}))
	if homeFile(t, h, "config.yaml") != before {
		t.Fatal("a save without answers changed the normalized file")
	}
}

func TestSetupSectionHeldWriteSavesTheResolvedValue(t *testing.T) {
	h := setupSectionHome(t, "model:\n  default: m\n", "OPENAI_API_KEY=sk\n")
	t.Setenv("VERCEL_TOKEN", "from-shell")
	params := map[string]any{"writes": []any{
		map[string]any{"target": "env", "key": "VERCEL_TOKEN", "kind": "held", "value": ""},
		map[string]any{"target": "env", "key": "DAYTONA_API_KEY", "kind": "held", "value": ""},
	}}
	res := resultOf(t, contractCall(t, "setup.section.save", params))
	if !reflect.DeepEqual(res["env"], []any{"VERCEL_TOKEN"}) {
		t.Fatalf("result %v", res)
	}
	if env := homeFile(t, h, ".env"); env != "OPENAI_API_KEY=sk\nVERCEL_TOKEN=from-shell\n" {
		t.Fatalf(".env %q", env)
	}
}

func TestSetupActionStartBindsTheParametersOfTheWalk(t *testing.T) {
	setupSectionHome(t, "model:\n  default: m\n", "")
	got := make(chan map[string]string, 1)
	setupParamActions["test.echo"] = func(_ context.Context, _ string, params map[string]string, out io.Writer) bool {
		io.WriteString(out, "echo "+params["host"]+"\n")
		got <- params
		return true
	}
	t.Cleanup(func() { delete(setupParamActions, "test.echo") })
	action, known := setupActionFor("test.echo", pyjson.NewObject())
	if !known || action == nil {
		t.Fatal("param action not found")
	}
	obj := pyjson.NewObject()
	obj.Set("host", "box")
	action, _ = setupActionFor("test.echo", obj)
	job := &setupJob{}
	job.run(context.Background(), "", action)
	if params := <-got; params["host"] != "box" {
		t.Fatalf("params %v", params)
	}
	if lines, _, done, outcome := job.since(0); !done || !outcome || !reflect.DeepEqual(lines, []any{"echo box"}) {
		t.Fatalf("job %v %v %v", lines, done, outcome)
	}
	if _, known := setupActionFor("xai.oauth.login", nil); !known {
		t.Fatal("a plain action lost its lookup")
	}
	if _, known := setupActionFor("no.such.action", nil); known {
		t.Fatal("unknown action found")
	}
	if text := setupParamText(map[string]any{"a": "1", "b": 2}); !reflect.DeepEqual(text, map[string]string{"a": "1", "b": ""}) {
		t.Fatalf("text %v", text)
	}
}

func TestDottedKeyHelpersCreateAndDropNestedKeys(t *testing.T) {
	cfg := map[string]any{"terminal": "flat", "proxy": config.Tree{"enabled": false}, "gone": nil}
	for _, key := range []string{"terminal.backend", "gone.child"} {
		if err := setDottedKey(cfg, key, "ssh"); err == nil || !strings.Contains(err.Error(), "not a mapping") {
			t.Fatalf("%s through a non mapping gave %v", key, err)
		}
	}
	for _, kv := range [][2]any{{"proxy.enabled", true}, {"top", 1}, {"new.level.key", "v"}} {
		if err := setDottedKey(cfg, kv[0].(string), kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]any{"terminal": "flat", "proxy": config.Tree{"enabled": true}, "gone": nil, "top": 1, "new": map[string]any{"level": map[string]any{"key": "v"}}}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("set %v", cfg)
	}
	deleteDottedKey(cfg, "proxy.enabled")
	deleteDottedKey(cfg, "missing.key")
	deleteDottedKey(cfg, "top.child")
	if _, held := cfg["proxy"].(config.Tree)["enabled"]; held || cfg["top"] != 1 {
		t.Fatalf("delete %v", cfg)
	}
}

func TestSetupSectionFullSaveRefusesANonMappingSection(t *testing.T) {
	h := setupSectionHome(t, "model:\n  default: m\nterminal: flat\n", "")
	params := map[string]any{"full_save": true, "writes": []any{
		map[string]any{"target": "config", "key": "terminal.backend", "kind": "text", "value": "docker"},
		map[string]any{"target": "env", "key": "TERMINAL_ENV", "kind": "text", "value": "docker"},
	}}
	if resp := contractCall(t, "setup.section.save", params); resp["error"] == nil {
		t.Fatalf("saved over a non mapping section %v", resp)
	}
	if !strings.Contains(homeFile(t, h, "config.yaml"), "terminal: flat") || homeFile(t, h, ".env") != "" {
		t.Fatal("a refused save changed the files")
	}
}

func TestTerminalSetupValuesProbeTheGatewayHost(t *testing.T) {
	setupSectionHome(t, "model:\n  default: m\n", "")
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, ".vercel", "project.json"), `{"projectId": "prj_9", "orgId": "team_9"}`)
	t.Setenv("USER", "deployer")
	res := resultOf(t, contractCall(t, "setup.section.load", map[string]any{"values": []any{"terminal.vercel_linked:" + dir, "terminal.ssh_defaults",
		"terminal.vercel_default_image", "terminal.docker", "terminal.plugin_backends"}}))
	values := res["values"].(map[string]any)
	if !reflect.DeepEqual(values["terminal.vercel_linked:"+dir], map[string]any{"projectId": "prj_9", "orgId": "team_9"}) {
		t.Fatalf("linked %v", values)
	}
	homeDir, _ := os.UserHomeDir()
	if !reflect.DeepEqual(values["terminal.ssh_defaults"], map[string]any{"user": "deployer", "key": filepath.Join(homeDir, ".ssh", "id_rsa")}) {
		t.Fatalf("ssh defaults %v", values["terminal.ssh_defaults"])
	}
	if values["terminal.vercel_default_image"] != config.DefaultVercelImage {
		t.Fatalf("values %v", values)
	}
	if _, isMap := values["terminal.docker"].(map[string]any); !isMap {
		t.Fatalf("docker %v", values["terminal.docker"])
	}
	rows, _ := values["terminal.plugin_backends"].([]any)
	for _, raw := range rows {
		if row := raw.(map[string]any); row["name"] == "local" || row["name"] == "docker" {
			t.Fatalf("built in listed %v", rows)
		}
	}
}

func TestTerminalSetdefaultKeysAreHeldByTheDefaults(t *testing.T) {
	defaults := map[string]any(config.Defaults())
	for _, key := range [][]string{{"terminal", "cwd"}, {"terminal", "docker_image"}, {"terminal", "daytona_image"}, {"proxy", "enforce_on_docker"}} {
		if _, held := config.CfgGet(defaults, key...); !held {
			t.Fatalf("the wizard setdefault of %v writes on a merged config", key)
		}
	}
}

func TestSpeechEngineInstallReportsTheSyncOutcome(t *testing.T) {
	prevSync := syncSpeechEngine
	t.Cleanup(func() { syncSpeechEngine = prevSync })
	var synced []string
	syncSpeechEngine = func(_ context.Context, extra string, _ io.Writer) error {
		synced = append(synced, extra)
		return nil
	}
	job := &setupJob{}
	action, _ := setupActionFor("tts.install.kittentts", nil)
	job.run(context.Background(), "", action)
	lines, _, done, outcome := job.since(0)
	if !done || !outcome || !reflect.DeepEqual(synced, []string{"kittentts"}) ||
		!reflect.DeepEqual(lines, []any{"Installing kittentts Python package (~25-80MB model downloaded on first use)...", "kittentts installed and ready to speak."}) {
		t.Fatalf("job %v %v %v synced %v", lines, done, outcome, synced)
	}
	syncSpeechEngine = func(context.Context, string, io.Writer) error {
		return &os.PathError{Op: "open", Path: "/venv", Err: os.ErrPermission}
	}
	job = &setupJob{}
	action, _ = setupActionFor("tts.install.neutts", nil)
	job.run(context.Background(), "", action)
	lines, _, _, outcome = job.since(0)
	if outcome || len(lines) != 4 || !strings.HasPrefix(lines[2].(string), "Failed to install neutts: ") || lines[3] != "Retry with /setup tts" {
		t.Fatalf("job %v %v", lines, outcome)
	}
}

// Asker answering from fixed values and recording each question
type fixedAsker struct {
	choice int
	text   string
	asked  []string
	said   []string
	fail   error
}

func (a *fixedAsker) Say(line string) { a.said = append(a.said, line) }
func (a *fixedAsker) Choose(q string, _ []setupflow.Option, _ int) (int, error) {
	a.asked = append(a.asked, q)
	return a.choice, a.fail
}
func (a *fixedAsker) Check(q string, _ []setupflow.Item, selected map[int]bool, _ map[string]int) (map[int]bool, error) {
	a.asked = append(a.asked, q)
	return map[int]bool{1: true}, a.fail
}
func (a *fixedAsker) Text(q, def string, _ bool) (string, error) {
	a.asked = append(a.asked, q)
	return a.text, a.fail
}

func TestMCPFlowUIAsksThroughTheFlow(t *testing.T) {
	ask := &fixedAsker{choice: 1, text: "typed"}
	ui := &mcpFlowUI{ask: ask, out: setupflow.Lines(ask)}
	io.WriteString(ui.Out(), "Discovering tools\n")
	if ui.Prompt("url", "d", false) != "typed" || ui.YesNo("enable?", true) || ui.Color("x", "c") != "x" || !ui.Interactive() {
		t.Fatal("answers lost")
	}
	if idx, picked := ui.SingleSelect("server", []string{"a", "b"}); idx != 1 || !picked {
		t.Fatalf("single %d %v", idx, picked)
	}
	if got := ui.Checklist("tools", []string{"a", "b"}, map[int]bool{0: true}, map[int]bool{0: true}); !reflect.DeepEqual(got, map[int]bool{1: true}) {
		t.Fatalf("checklist %v", got)
	}
	if !reflect.DeepEqual(ask.said, []string{"Discovering tools"}) || ui.err != nil {
		t.Fatalf("said %v err %v", ask.said, ui.err)
	}
	ask.fail = setupflow.ErrCancelled
	if got := ui.Checklist("tools", []string{"a"}, map[int]bool{}, map[int]bool{0: true}); !reflect.DeepEqual(got, map[int]bool{0: true}) || ui.err != setupflow.ErrCancelled {
		t.Fatalf("cancelled checklist %v err %v", got, ui.err)
	}
	if ui.Prompt("x", "keep", false) != "keep" || !ui.YesNo("y", true) {
		t.Fatal("a failed question did not keep the default")
	}
}
