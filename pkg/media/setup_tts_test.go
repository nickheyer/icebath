package media

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
)

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
