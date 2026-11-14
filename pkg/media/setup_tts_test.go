package media

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
)

// One recorded hermes setup_tts run, its scripted answers and the bytes, calls and config it produced
type ttsScreen struct {
	Name              string            `json:"name"`
	Config            map[string]any    `json:"config"`
	Choices           []any             `json:"choices"`
	Prompts           []string          `json:"prompts"`
	YesNo             []bool            `json:"yes_no"`
	Env               map[string]string `json:"env"`
	Supported         map[string]bool   `json:"supported"`
	SupportedSequence map[string][]bool `json:"supported_sequence"`
	Installed         map[string]bool   `json:"installed"`
	Which             map[string]string `json:"which"`
	Sync              string            `json:"sync"`
	Run               string            `json:"run"`
	Platform          string            `json:"platform"`
	Managed           bool              `json:"managed"`
	NousAuth          bool              `json:"nous_auth"`
	XAILoggedIn       bool              `json:"xai_logged_in"`
	XAILogin          bool              `json:"xai_login"`
	Want              struct {
		Stdout string         `json:"stdout"`
		Calls  []any          `json:"calls"`
		Config map[string]any `json:"config"`
		Error  *string        `json:"error"`
	} `json:"want"`
}

// Hermes strings of the recording mapped to their icebath spelling
var ttsScreenRenames = strings.NewReplacer("hermes setup", "icebath setup", "~/.hermes", "~/.icebath",
	"Restart Hermes", "Restart Icebath", "`hermes pm doctor`", "`icebath pm doctor`")

// JSON round trip of a value so recorded and live calls compare as decoded JSON
func jsonPlain(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Text or nil, the None a Python fake records for an omitted keyword
func orNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Temp home and display home for one setup run, the setup console captured
func ttsTestHome(t *testing.T) (context.Context, *bytes.Buffer) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, ".icebath"))
	if err := os.MkdirAll(filepath.Join(tmp, ".icebath"), 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	t.Cleanup(provider.WithSetupOutput(&out))
	return context.Background(), &out
}

// Restores the setup module and hook tables after a test swaps their entries
func keepTTSSeams(t *testing.T) {
	t.Helper()
	savedSetup, savedHooks := ttsSetup, ttsHooks
	t.Cleanup(func() { ttsSetup, ttsHooks = savedSetup, savedHooks })
}

// Installs fakes answering from a recorded scenario and recording every call the hermes fakes recorded
func installTTSScreenFakes(t *testing.T, sc *ttsScreen, cfg config.Tree) *[]any {
	t.Helper()
	keepTTSSeams(t)
	calls := &[]any{}
	record := func(entry ...any) { *calls = append(*calls, entry) }
	answers, prompts, yesNo := append([]any(nil), sc.Choices...), append([]string(nil), sc.Prompts...), append([]bool(nil), sc.YesNo...)
	env := map[string]string{}
	for k, v := range sc.Env {
		env[k] = v
	}
	seq := map[string][]bool{}
	for k, v := range sc.SupportedSequence {
		seq[k] = append([]bool(nil), v...)
	}
	ttsSetup.promptChoice = func(question string, choices []string, def int, description string) int {
		record("prompt_choice", question, append([]string(nil), choices...), def, orNil(description))
		if len(answers) == 0 {
			t.Fatalf("unexpected prompt_choice %q", question)
		}
		ans := answers[0]
		answers = answers[1:]
		if ans == "last" {
			return len(choices) - 1
		}
		return int(ans.(float64))
	}
	ttsSetup.prompt = func(question, def string, password bool) string {
		record("prompt", question, orNil(def), password)
		if len(prompts) == 0 {
			t.Fatalf("unexpected prompt %q", question)
		}
		ans := prompts[0]
		prompts = prompts[1:]
		return ans
	}
	ttsSetup.promptYesNo = func(question string, def bool, _ string) bool {
		record("prompt_yes_no", question, def)
		if len(yesNo) == 0 {
			t.Fatalf("unexpected prompt_yes_no %q", question)
		}
		ans := yesNo[0]
		yesNo = yesNo[1:]
		return ans
	}
	ttsSetup.getEnvValue = func(_ context.Context, key string) (string, error) {
		record("get_env_value", key)
		return env[key], nil
	}
	ttsSetup.saveEnvValue = func(_ context.Context, key, value string) error {
		record("save_env_value", key, value)
		env[key] = value
		return nil
	}
	ttsSetup.saveConfig = func(_ context.Context, c config.Tree) error {
		record("save_config", jsonPlain(t, c))
		return nil
	}
	ttsSetup.moduleInstalled = func(_ context.Context, name string) bool {
		record("module_installed", name)
		return sc.Installed[name]
	}
	ttsHooks.extraSupported = func(_ context.Context, name string) (bool, error) {
		record("extra_supported", name)
		if s, ok := seq[name]; ok {
			seq[name] = s[1:]
			return s[0], nil
		}
		if v, ok := sc.Supported[name]; ok {
			return v, nil
		}
		return true, nil
	}
	ttsHooks.syncVenv = func(_ context.Context, o pm.SyncVenvOptions) error {
		record("sync_venv", append([]string(nil), o.Extras...), o.Explicit)
		if sc.Sync == "fail" {
			return pm.NewInstallError("venv", "resolution refused", "")
		}
		return nil
	}
	ttsHooks.which = func(name string) bool {
		record("which", name)
		return sc.Which[name] != ""
	}
	ttsHooks.runInstall = func(_ context.Context, argv []string) error {
		record("run", append([]string(nil), argv...), true)
		switch sc.Run {
		case "called_process_error":
			return &pysubprocess.CalledProcessError{Cmd: argv, Returncode: 1}
		case "file_not_found":
			return &exec.Error{Name: argv[0], Err: exec.ErrNotFound}
		}
		return nil
	}
	platform := sysPlatform()
	if sc.Platform != "" {
		platform = sc.Platform
	}
	ttsHooks.platform = func() string { return platform }
	ttsHooks.managedNousToolsEnabled = func(context.Context, config.Tree) bool {
		record("managed_nous_tools_enabled")
		return sc.Managed
	}
	ttsHooks.nousAuthPresent = func(context.Context, config.Tree) bool {
		record("nous_subscription_features")
		return sc.NousAuth
	}
	ttsHooks.xaiOAuthLoggedIn = func(context.Context) bool {
		record("xai_oauth_logged_in")
		return sc.XAILoggedIn
	}
	ttsHooks.xaiOAuthLogin = func(context.Context) bool {
		record("xai_oauth_login")
		return sc.XAILogin
	}
	t.Cleanup(func() {
		if len(answers)+len(prompts)+len(yesNo) > 0 {
			t.Errorf("unused answers %v %v %v", answers, prompts, yesNo)
		}
	})
	return calls
}

func TestSetupTTSScreensMatchHermes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "setup_tts_screens.json"))
	if err != nil {
		t.Fatal(err)
	}
	var screens []ttsScreen
	if err := json.Unmarshal(raw, &screens); err != nil {
		t.Fatal(err)
	}
	if len(screens) != 42 {
		t.Fatalf("%d recorded screens", len(screens))
	}
	for i := range screens {
		sc := &screens[i]
		t.Run(sc.Name, func(t *testing.T) {
			ctx, out := ttsTestHome(t)
			cfg := config.Tree(jsonPlain(t, sc.Config).(map[string]any))
			calls := installTTSScreenFakes(t, sc, cfg)
			err := SetupTTS(ctx, cfg)
			if sc.Want.Error != nil {
				if err == nil || err.Error() != *sc.Want.Error {
					t.Fatalf("error %v, want %s", err, *sc.Want.Error)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if want := ttsScreenRenames.Replace(sc.Want.Stdout); out.String() != want {
				t.Fatalf("stdout\n%q\nwant\n%q", out.String(), want)
			}
			if got, want := jsonPlain(t, *calls), jsonPlain(t, sc.Want.Calls); !reflect.DeepEqual(got, want) {
				t.Fatalf("calls\n%v\nwant\n%v", got, want)
			}
			if got, want := jsonPlain(t, cfg), jsonPlain(t, sc.Want.Config); !reflect.DeepEqual(got, want) {
				t.Fatalf("config %v, want %v", got, want)
			}
		})
	}
}

// Picks Edge TTS then answers the voice menu and the optional id prompt, returning the last menu call
func runEdgeVoiceSetup(t *testing.T, cfg config.Tree, pickVoice func(choices []string, def int) int, typed string) (string, []string, int, string) {
	t.Helper()
	ctx, _ := ttsTestHome(t)
	keepTTSSeams(t)
	ttsSetup.saveConfig = func(context.Context, config.Tree) error { return nil }
	ttsSetup.printHeader, ttsSetup.info = func(string) {}, func(...any) {}
	ttsSetup.printInfo, ttsSetup.printSuccess = func(string) {}, func(string) {}
	ttsHooks.managedNousToolsEnabled = func(context.Context, config.Tree) bool { return false }
	type call struct {
		question    string
		choices     []string
		def         int
		description string
	}
	var calls []call
	ttsSetup.promptChoice = func(question string, choices []string, def int, description string) int {
		calls = append(calls, call{question, append([]string(nil), choices...), def, description})
		if strings.Contains(question, "provider") {
			for i, label := range choices {
				if strings.HasPrefix(label, "Edge TTS") {
					return i
				}
			}
		}
		return pickVoice(choices, def)
	}
	ttsSetup.prompt = func(string, string, bool) string { return typed }
	if err := setupTTSProvider(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	last := calls[len(calls)-1]
	return last.question, last.choices, last.def, last.description
}

func TestEdgeSetupPersistsSelectedVoice(t *testing.T) {
	cfg := config.Tree{"tts": map[string]any{"provider": "openai", "edge": map[string]any{"voice": "en-US-AriaNeural"}}}
	_, _, _, description := runEdgeVoiceSetup(t, cfg, func(choices []string, _ int) int {
		for i, label := range choices {
			if strings.HasPrefix(label, "Chinese (Simplified)") {
				return i
			}
		}
		return -1
	}, "")
	tts := cfg["tts"].(map[string]any)
	if !strings.Contains(description, "language") || tts["provider"] != "edge" || tts["edge"].(map[string]any)["voice"] != "zh-CN-XiaoxiaoNeural" {
		t.Fatalf("description %q config %v", description, cfg)
	}
}

func TestUnlistedCurrentVoiceIsOfferedFirstAndKept(t *testing.T) {
	cfg := config.Tree{"tts": map[string]any{"provider": "openai", "edge": map[string]any{"voice": "fr-FR-DeniseNeural"}}}
	_, choices, def, _ := runEdgeVoiceSetup(t, cfg, func(_ []string, def int) int { return def }, "")
	if def != 0 || choices[0] != "Current voice — fr-FR-DeniseNeural" {
		t.Fatalf("default %d choices %v", def, choices)
	}
	if voice := cfg["tts"].(map[string]any)["edge"].(map[string]any)["voice"]; voice != "fr-FR-DeniseNeural" {
		t.Fatalf("voice %v", voice)
	}
}

func TestOtherVoiceAcceptsCustomIdAndBlankKeepsCurrent(t *testing.T) {
	for _, tc := range []struct{ typed, expected string }{{"  de-DE-KatjaNeural ", "de-DE-KatjaNeural"}, {"", "ja-JP-NanamiNeural"}} {
		t.Run(tc.typed, func(t *testing.T) {
			cfg := config.Tree{"tts": map[string]any{"provider": "edge", "edge": map[string]any{"voice": "ja-JP-NanamiNeural"}}}
			_, choices, _, _ := runEdgeVoiceSetup(t, cfg, func(choices []string, _ int) int { return len(choices) - 1 }, tc.typed)
			if choices[len(choices)-1] != "Other voice (enter voice ID)" {
				t.Fatalf("choices %v", choices)
			}
			if voice := cfg["tts"].(map[string]any)["edge"].(map[string]any)["voice"]; voice != tc.expected {
				t.Fatalf("voice %v", voice)
			}
		})
	}
}

func TestTtsXaiStepPrefersExistingApiKeyOverOauth(t *testing.T) {
	ctx, _ := ttsTestHome(t)
	keepTTSSeams(t)
	var messages []string
	ttsHooks.xaiOAuthLoggedIn = func(context.Context) bool { return true }
	ttsSetup.getEnvValue = func(_ context.Context, key string) (string, error) {
		if key == "XAI_API_KEY" {
			return "xai-key", nil
		}
		return "", nil
	}
	ttsSetup.printSuccess = func(msg string) { messages = append(messages, msg) }
	ttsSetup.prompt = func(string, string, bool) string { return "" }
	ttsSetup.promptChoice = func(string, []string, int, string) int {
		t.Fatal("no prompt expected")
		return 0
	}
	got, err := ttsXAIStep(ctx, config.Tree{})
	if err != nil || got != "xai" {
		t.Fatalf("got %q %v", got, err)
	}
	if len(messages) != 1 || !strings.Contains(messages[0], "XAI_API_KEY") {
		t.Fatalf("messages %v", messages)
	}
}

func TestUnsupportedTtsSelectionIsRetainedWithoutInstalling(t *testing.T) {
	for _, extra := range []string{"neutts", "kittentts"} {
		t.Run(extra, func(t *testing.T) {
			ctx, out := ttsTestHome(t)
			keepTTSSeams(t)
			ttsHooks.extraSupported = func(context.Context, string) (bool, error) { return false, nil }
			ttsSetup.promptYesNo = func(string, bool, string) bool {
				t.Fatal("unsupported engine must not install system or Python packages")
				return false
			}
			ttsHooks.syncVenv = func(context.Context, pm.SyncVenvOptions) error {
				t.Fatal("unsupported engine must not install system or Python packages")
				return nil
			}
			got, err := ttsLocalInstallStep(ctx, extra)
			if err != nil || got != extra {
				t.Fatalf("got %q %v", got, err)
			}
			if text := out.String(); !strings.Contains(text, "not supported") || !strings.Contains(text, "selection is saved") {
				t.Fatalf("output %q", text)
			}
		})
	}
}

func TestPythonSetupUsesDeclaredExtrasAndReportsRestart(t *testing.T) {
	installers := map[string]func(context.Context) (bool, error){"neutts": installNeuTTSDeps, "kittentts": installKittenTTSDeps}
	for _, extra := range []string{"neutts", "kittentts"} {
		for _, succeeds := range []bool{true, false} {
			t.Run(extra+map[bool]string{true: "-ok", false: "-refused"}[succeeds], func(t *testing.T) {
				ctx, out := ttsTestHome(t)
				keepTTSSeams(t)
				var calls [][]any
				ttsHooks.syncVenv = func(_ context.Context, o pm.SyncVenvOptions) error {
					calls = append(calls, []any{append([]string(nil), o.Extras...), o.Explicit})
					if !succeeds {
						return pm.NewInstallError("venv", "resolution refused", "")
					}
					return nil
				}
				ttsHooks.extraSupported = func(context.Context, string) (bool, error) { return true, nil }
				ttsHooks.which = func(string) bool { return true }
				ttsSetup.moduleInstalled = func(context.Context, string) bool {
					t.Fatal("installing must never activate the engine in this process")
					return false
				}
				ok, err := installers[extra](ctx)
				if err != nil || ok != succeeds {
					t.Fatalf("installer %v %v", ok, err)
				}
				if !reflect.DeepEqual(calls, [][]any{{[]string{extra}, true}}) {
					t.Fatalf("calls %v", calls)
				}
				text := out.String()
				if succeeds && !strings.Contains(text, "Restart Icebath") {
					t.Fatalf("output %q", text)
				}
				if !succeeds && (!strings.Contains(text, "resolution refused") || !strings.Contains(text, "Retry with: icebath setup") || strings.Contains(text, "installed.")) {
					t.Fatalf("output %q", text)
				}
			})
		}
	}
}

func TestInstallTTSExtraReportsAnOSErrorAsPythonPrintsIt(t *testing.T) {
	ctx, out := ttsTestHome(t)
	keepTTSSeams(t)
	ttsHooks.syncVenv = func(context.Context, pm.SyncVenvOptions) error {
		return &fs.PathError{Op: "open", Path: "/venv/lock", Err: syscall.EACCES}
	}
	ok, err := installTTSExtra(ctx, "kittentts")
	if err != nil || ok {
		t.Fatalf("got %v %v", ok, err)
	}
	want := "Failed to install kittentts: [Errno 13] Permission denied: '/venv/lock'"
	if text := out.String(); !strings.Contains(text, want) || !strings.Contains(text, "Retry with: icebath setup tts") {
		t.Fatalf("output %q lacks %q", text, want)
	}
}
