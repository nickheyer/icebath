package tools

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
)

// STT catalog doubles holding the transcription_common local model table
func installSTTCatalogDoubles(t *testing.T) {
	t.Helper()
	catalog := map[string][]string{
		"local":      {"base", "tiny", "small", "medium", "large-v3", "turbo"},
		"groq":       {"whisper-large-v3-turbo", "whisper-large-v3"},
		"openai":     {"whisper-1", "gpt-4o-mini-transcribe", "gpt-4o-transcribe", "gpt-transcribe"},
		"mistral":    {"voxtral-mini-latest", "voxtral-mini-2602"},
		"xai":        {"grok-voice-transcribe-2.0", "grok-voice-transcribe-1.0"},
		"elevenlabs": {"scribe_v2", "scribe_v1"},
	}
	swapHook(t, &ToolsConfigHooks.STTModelCatalog, func(p string) ([]string, bool) {
		m, ok := catalog[p]
		return slices.Clone(m), ok
	})
	swapHook(t, &ToolsConfigHooks.STTModelConfigKey, func(p string) (string, bool) {
		if p == "elevenlabs" {
			return "model_id", true
		}
		return "", false
	})
	swapHook(t, &ToolsConfigHooks.STTDefaultLocalModel, func() string { return "base" })
}

func TestLocalBrowserRowReadyOnlyWhenSetupWouldBeANoop(t *testing.T) {
	for _, c := range []struct {
		harness  bool
		chromium string
		ready    bool
	}{{true, "/pm/chromium/chrome", true}, {true, "", false}, {false, "/pm/chromium/chrome", false}} {
		s, _ := newTestToolsSetup(t, "")
		swapHook(t, &ToolsConfigHooks.FindBrowserUseCLI, func(*Env) []string {
			if c.harness {
				return []string{"harness"}
			}
			return nil
		})
		swapHook(t, &ToolsConfigHooks.ChromiumExecutable, func(*Env) string { return c.chromium })
		if got, err := postSetupReady["chromium"](s); err != nil || got != c.ready {
			t.Fatalf("chromium ready %v %v for %+v", got, err, c)
		}
		if got, err := postSetupReady["browserbase"](s); err != nil || got != c.harness {
			t.Fatalf("browserbase ready %v %v for %+v", got, err, c)
		}
	}
}

func TestImportableSDKDoesNotBypassPMConstraints(t *testing.T) {
	for _, c := range []struct{ key, extra string }{{"ddgs", "ddgs"}, {"faster_whisper", "stt-whisper"}, {"kittentts", "kittentts"}, {"piper", "piper"}} {
		for _, refused := range []bool{false, true} {
			t.Run(c.key, func(t *testing.T) {
				s, out := newTestToolsSetup(t, "")
				installSTTCatalogDoubles(t)
				swapHook(t, &postSetupModuleImportable, func(_ context.Context, name string) bool { return name == c.key })
				var calls []pm.SyncVenvOptions
				swapHook(t, &postSetupSyncVenv, func(_ context.Context, opts pm.SyncVenvOptions) error {
					calls = append(calls, opts)
					if refused {
						return pm.NewInstallError("venv", "outside frozen feature set", "")
					}
					return nil
				})
				if err := s.runPostSetup(c.key); err != nil {
					t.Fatal(err)
				}
				if len(calls) != 1 || !slices.Equal(calls[0].Extras, []string{c.extra}) || !calls[0].Explicit ||
					calls[0].Repair || calls[0].ProjectRoot != "" || calls[0].Plugins != nil || calls[0].EvictIncompatiblePlugins ||
					calls[0].Automatic != nil || calls[0].Carried != nil || calls[0].Stdout != nil {
					t.Fatalf("sync_venv calls %+v", calls)
				}
				output := out.String()
				if refused {
					if !strings.Contains(output, "outside frozen feature set") || !strings.Contains(output, "Retry with: icebath tools") ||
						strings.Contains(output, "Restart Icebath") {
						t.Fatalf("refused output %q", output)
					}
				} else if !strings.Contains(output, "Restart Icebath") {
					t.Fatalf("output %q", output)
				}
			})
		}
	}
}
