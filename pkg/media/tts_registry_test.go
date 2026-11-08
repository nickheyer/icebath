package media

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sort"
	"testing"
)

type fakeTTSProvider struct {
	TTSProviderDefaults
	name        string
	voiceCompat bool
	models      []map[string]any
	voices      []map[string]any
	synth       func(text, outputPath string, opts TTSOptions) (string, error)
}

func newFakeTTSProvider(name string) *fakeTTSProvider {
	p := &fakeTTSProvider{name: name}
	p.TTSProviderDefaults = NewTTSProviderDefaults(p)
	return p
}

func (p *fakeTTSProvider) Name() string          { return p.name }
func (p *fakeTTSProvider) VoiceCompatible() bool { return p.voiceCompat }
func (p *fakeTTSProvider) ListModels() []map[string]any {
	if p.models != nil {
		return p.models
	}
	return p.TTSProviderDefaults.ListModels()
}
func (p *fakeTTSProvider) ListVoices() []map[string]any {
	if p.voices != nil {
		return p.voices
	}
	return p.TTSProviderDefaults.ListVoices()
}
func (p *fakeTTSProvider) Synthesize(_ context.Context, text, outputPath string, opts TTSOptions) (string, error) {
	if p.synth != nil {
		return p.synth(text, outputPath, opts)
	}
	return outputPath, nil
}

func resetTTSRegistry(t *testing.T) {
	t.Helper()
	ttsRegistry.ResetForTests()
	t.Cleanup(ttsRegistry.ResetForTests)
}

func TestTTSRegistryRegistration(t *testing.T) {
	t.Run("test_happy_path", func(t *testing.T) {
		resetTTSRegistry(t)
		p := newFakeTTSProvider("cartesia")
		if err := RegisterTTSProvider(p, ""); err != nil {
			t.Fatal(err)
		}
		if got := GetTTSProvider(context.Background(), "cartesia", ""); got != TTSProvider(p) {
			t.Fatalf("get_provider = %v", got)
		}
		list := ListTTSProviders(context.Background(), "")
		if len(list) != 1 || list[0].Name() != "cartesia" {
			t.Fatalf("list_providers = %v", list)
		}
	})
	names := make([]string, 0, len(ttsRegistryBuiltinNames))
	for n := range ttsRegistryBuiltinNames {
		names = append(names, n)
	}
	sort.Strings(names)
	t.Run("test_rejects_builtin_shadow_with_warning", func(t *testing.T) {
		for _, builtin := range names {
			t.Run(builtin, func(t *testing.T) {
				resetTTSRegistry(t)
				var buf bytes.Buffer
				saved := ttsRegistryLog
				ttsRegistryLog = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
				t.Cleanup(func() { ttsRegistryLog = saved })
				if err := RegisterTTSProvider(newFakeTTSProvider(builtin), ""); err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(buf.Bytes(), []byte(builtin)) || !bytes.Contains(buf.Bytes(), []byte("level=WARN")) {
					t.Fatalf("warning log = %q", buf.String())
				}
				if GetTTSProvider(context.Background(), builtin, "") != nil {
					t.Fatal("builtin name registered")
				}
				if len(ListTTSProviders(context.Background(), "")) != 0 {
					t.Fatal("list_providers not empty")
				}
			})
		}
	})
}

func TestTTSRegistryLookup(t *testing.T) {
	t.Run("test_get_provider_non_string_returns_none", func(t *testing.T) {
		resetTTSRegistry(t)
		if GetTTSProvider(context.Background(), nil, "") != nil || GetTTSProvider(context.Background(), 123, "") != nil {
			t.Fatal("non string name resolved a provider")
		}
	})
	t.Run("test_list_providers_sorted", func(t *testing.T) {
		resetTTSRegistry(t)
		for _, n := range []string{"zylo", "alpha", "middle"} {
			if err := RegisterTTSProvider(newFakeTTSProvider(n), ""); err != nil {
				t.Fatal(err)
			}
		}
		var got []string
		for _, p := range ListTTSProviders(context.Background(), "") {
			got = append(got, p.Name())
		}
		if len(got) != 3 || got[0] != "alpha" || got[1] != "middle" || got[2] != "zylo" {
			t.Fatalf("names = %v", got)
		}
	})
}

func TestTTSProviderContract(t *testing.T) {
	t.Run("test_default_model_first_listed", func(t *testing.T) {
		p := newFakeTTSProvider("cartesia")
		p.models = []map[string]any{{"id": "sonic-2"}, {"id": "sonic-1"}}
		if got := p.DefaultModel(); got != "sonic-2" {
			t.Fatalf("default_model = %q", got)
		}
	})
	t.Run("test_default_voice_first_listed", func(t *testing.T) {
		p := newFakeTTSProvider("cartesia")
		p.voices = []map[string]any{{"id": "voice-aria"}, {"id": "voice-jasper"}}
		if got := p.DefaultVoice(); got != "voice-aria" {
			t.Fatalf("default_voice = %q", got)
		}
	})
	t.Run("test_get_setup_schema_default_minimal", func(t *testing.T) {
		schema := newFakeTTSProvider("cartesia").GetSetupSchema()
		if schema["name"] != "Cartesia" {
			t.Fatalf("name = %v", schema["name"])
		}
		if envs, ok := schema["env_vars"].([]any); !ok || len(envs) != 0 {
			t.Fatalf("env_vars = %v", schema["env_vars"])
		}
	})
	t.Run("test_stream_raises_not_implemented_by_default", func(t *testing.T) {
		_, err := newFakeTTSProvider("cartesia").Stream(context.Background(), "hello", TTSOptions{})
		var nie *TTSNotImplementedError
		if !errors.As(err, &nie) {
			t.Fatalf("stream error = %v", err)
		}
	})
}

func TestTTSResolveOutputFormat(t *testing.T) {
	formats := make([]string, 0, len(ValidTTSOutputFormats))
	for f := range ValidTTSOutputFormats {
		formats = append(formats, f)
	}
	sort.Strings(formats)
	t.Run("test_valid_passes_through", func(t *testing.T) {
		for _, valid := range formats {
			t.Run(valid, func(t *testing.T) {
				if got := ResolveTTSOutputFormat(valid); got != valid {
					t.Fatalf("resolve_output_format(%q) = %q", valid, got)
				}
			})
		}
	})
	t.Run("test_non_string_returns_default", func(t *testing.T) {
		if ResolveTTSOutputFormat(123) != DefaultTTSOutputFormat || ResolveTTSOutputFormat([]any{}) != DefaultTTSOutputFormat {
			t.Fatal("non string format not defaulted")
		}
	})
}

func TestTTSBuiltinSync(t *testing.T) {
	t.Run("test_registry_builtins_match_dispatcher_builtins", func(t *testing.T) {
		for n := range ttsRegistryBuiltinNames {
			if !builtinTTSProviders[n] {
				t.Errorf("registry only: %s", n)
			}
		}
		for n := range builtinTTSProviders {
			if !ttsRegistryBuiltinNames[n] {
				t.Errorf("dispatcher only: %s", n)
			}
		}
	})
}
