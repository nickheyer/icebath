package media

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Records the spawn of a command provider run and starts the real child
func captureCommandSpawn(t *testing.T) *struct {
	cmd     *exec.Cmd
	purpose string
} {
	t.Helper()
	got := &struct {
		cmd     *exec.Cmd
		purpose string
	}{}
	saved := spawnCommandProvider
	spawnCommandProvider = func(cmd *exec.Cmd, purpose string) error {
		got.cmd, got.purpose = cmd, purpose
		return cmd.Start()
	}
	t.Cleanup(func() { spawnCommandProvider = saved })
	return got
}

func envList(cmd *exec.Cmd) map[string]string {
	out := map[string]string{}
	for _, kv := range cmd.Env {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}

func TestTTSResolveCommandProviderConfig(t *testing.T) {
	t.Run("test_builtin_names_are_never_command_providers", func(t *testing.T) {
		cfg := map[string]any{"providers": map[string]any{
			"openai": map[string]any{"type": "command", "command": "echo hi"},
			"edge":   map[string]any{"type": "command", "command": "echo hi"},
		}}
		for name := range builtinTTSProviders {
			if resolveCommandTTSProviderConfig(name, cfg) != nil {
				t.Fatalf("%s resolved as a command provider", name)
			}
		}
	})
	t.Run("test_missing_provider_returns_none", func(t *testing.T) {
		if resolveCommandTTSProviderConfig("nope", map[string]any{"providers": map[string]any{}}) != nil {
			t.Fatal("missing provider resolved")
		}
	})
	t.Run("test_case_insensitive_lookup", func(t *testing.T) {
		cfg := map[string]any{"providers": map[string]any{"piper-cli": map[string]any{"type": "command", "command": "x"}}}
		if resolveCommandTTSProviderConfig("PIPER-CLI", cfg) == nil {
			t.Fatal("case insensitive lookup failed")
		}
	})
	t.Run("test_native_piper_cannot_be_shadowed_by_command_entry", func(t *testing.T) {
		cfg := map[string]any{"providers": map[string]any{"piper": map[string]any{"type": "command", "command": "some-script"}}}
		if resolveCommandTTSProviderConfig("piper", cfg) != nil {
			t.Fatal("native piper shadowed")
		}
	})
}

func TestTTSCommandEnv(t *testing.T) {
	t.Run("test_command_provider_uses_sanitized_child_env", func(t *testing.T) {
		t.Setenv("AUXILIARY_VISION_API_KEY", "sk-vision")
		t.Setenv("GATEWAY_RELAY_SECRET", "relay-secret")
		t.Setenv("OPENAI_API_KEY", "sk-openai")
		t.Setenv("MY_SAFE_TTS_VAR", "keep")
		got := captureCommandSpawn(t)
		res, err := runCommandProvider(nil, "echo hi", 5, nil)
		if err != nil || res.Returncode != 0 {
			t.Fatalf("run = %v, %v", res, err)
		}
		env := envList(got.cmd)
		for _, k := range []string{"AUXILIARY_VISION_API_KEY", "GATEWAY_RELAY_SECRET", "OPENAI_API_KEY"} {
			if _, ok := env[k]; ok {
				t.Fatalf("%s leaked into the child env", k)
			}
		}
		if env["MY_SAFE_TTS_VAR"] != "keep" {
			t.Fatalf("MY_SAFE_TTS_VAR = %q", env["MY_SAFE_TTS_VAR"])
		}
	})
	t.Run("test_env_passthrough_forwards_the_served_profiles_value", func(t *testing.T) {
		t.Setenv("MY_TTS_TOKEN", "tok-launch")
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("MY_TTS_TOKEN=tok-work\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		env := &tools.Env{Home: dir, Config: config.Tree{}, Secrets: config.NewSecrets(dir)}
		tools.SetMultiplexActive(true)
		t.Cleanup(func() { tools.SetMultiplexActive(false) })
		res, err := runCommandProvider(env, `printf '%s\n' "$MY_TTS_TOKEN"`, 30, []string{"MY_TTS_TOKEN"})
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(res.Stdout) != "tok-work" {
			t.Fatalf("stdout = %q", res.Stdout)
		}
	})
}

func TestTTSGetNamedProviderConfig(t *testing.T) {
	t.Run("test_providers_block_wins", func(t *testing.T) {
		cfg := map[string]any{"providers": map[string]any{"voxcpm": map[string]any{"command": "new"}}, "voxcpm": map[string]any{"command": "legacy"}}
		if got := getNamedTTSProviderConfig(cfg, "voxcpm"); !reflect.DeepEqual(got, map[string]any{"command": "new"}) {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("test_legacy_tts_name_block_still_resolves", func(t *testing.T) {
		cfg := map[string]any{"voxcpm": map[string]any{"type": "command", "command": "legacy"}}
		if got := getNamedTTSProviderConfig(cfg, "voxcpm"); !reflect.DeepEqual(got, map[string]any{"type": "command", "command": "legacy"}) {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("test_builtin_names_do_not_leak_through_legacy_path", func(t *testing.T) {
		cfg := map[string]any{"openai": map[string]any{"command": "oops", "type": "command"}}
		if got := getNamedTTSProviderConfig(cfg, "openai"); len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})
}

func TestTTSIsCommandProviderConfig(t *testing.T) {
	t.Run("test_type_mismatch_is_false", func(t *testing.T) {
		if isCommandProviderConfig(map[string]any{"type": "native", "command": "x"}) {
			t.Fatal("native type accepted")
		}
	})
}

func TestTTSIterCommandProviders(t *testing.T) {
	t.Run("test_iterates_only_user_command_providers", func(t *testing.T) {
		cfg := map[string]any{"providers": map[string]any{
			"openai":    map[string]any{"type": "command", "command": "shouldnt show up"},
			"piper-cli": map[string]any{"type": "command", "command": "piper-cli"},
			"voxcpm":    map[string]any{"type": "command", "command": "voxcpm"},
			"broken":    map[string]any{"type": "command", "command": ""},
		}}
		var names []string
		for _, p := range iterCommandTTSProviders(cfg) {
			names = append(names, p[0].(string))
		}
		if !reflect.DeepEqual(names, []string{"piper-cli", "voxcpm"}) {
			t.Fatalf("names = %v", names)
		}
	})
}

func TestTTSCommandConfigGetters(t *testing.T) {
	t.Run("test_timeout_defaults", func(t *testing.T) {
		if getCommandTTSTimeout(map[string]any{}) != float64(defaultCommandTTSTimeoutSeconds) {
			t.Fatal("timeout default")
		}
	})
	t.Run("test_output_format_defaults", func(t *testing.T) {
		if getCommandTTSOutputFormat(map[string]any{}, "") != defaultCommandTTSOutputFormat {
			t.Fatal("format default")
		}
	})
}

func TestTTSShellQuoteContext(t *testing.T) {
	check := func(t *testing.T, tpl, want string) {
		if got := shellQuoteContext(tpl, strings.Index(tpl, "{output_path}")); got != want {
			t.Fatalf("quote context = %q", got)
		}
	}
	t.Run("test_bare_context", func(t *testing.T) { check(t, "tts {output_path}", "") })
	t.Run("test_inside_double_quotes", func(t *testing.T) { check(t, `tts "{output_path}"`, `"`) })
	t.Run("test_escaped_double_quote_inside_double", func(t *testing.T) { check(t, `tts "foo \" {output_path}"`, `"`) })
}

func commandTemplateValues(over map[string]string) map[string]string {
	p := map[string]string{"input_path": "/tmp/in.txt", "text_path": "/tmp/in.txt", "output_path": "/tmp/out.mp3",
		"format": "mp3", "voice": "", "model": "", "speed": "1.0"}
	for k, v := range over {
		p[k] = v
	}
	return p
}

func TestTTSRenderCommandTemplate(t *testing.T) {
	t.Run("test_substitutes_all_placeholders", func(t *testing.T) {
		got := renderCommandTemplate("tts --voice {voice} --in {input_path} --out {output_path}",
			commandTemplateValues(map[string]string{"voice": "af_sky", "model": "tiny"}))
		if !strings.Contains(got, "af_sky") || !strings.Contains(got, "/tmp/out.mp3") {
			t.Fatalf("rendered %q", got)
		}
	})
	t.Run("test_literal_braces_survive", func(t *testing.T) {
		got := renderCommandTemplate("echo '{{not a placeholder}}' && tts --in {input_path}", commandTemplateValues(nil))
		if !strings.Contains(got, "{not a placeholder}") {
			t.Fatalf("rendered %q", got)
		}
	})
	t.Run("test_injection_is_neutralized", func(t *testing.T) {
		got := renderCommandTemplate("tts --voice {voice} --out {output_path}",
			commandTemplateValues(map[string]string{"output_path": "/tmp/out; rm -rf /", "voice": "$(whoami)"}))
		if hostOSName() != "nt" {
			if !strings.Contains(got, "'$(whoami)'") && !strings.Contains(got, `'\''`) {
				t.Fatalf("rendered %q", got)
			}
			if strings.Contains(strings.ReplaceAll(got, "'/tmp/out; rm -rf /'", ""), "; rm -rf /") {
				t.Fatalf("injection survived in %q", got)
			}
		}
	})
	t.Run("test_preserves_shell_quoting_style", func(t *testing.T) {
		got := renderCommandTemplate(`tts --voice "{voice}"`, commandTemplateValues(map[string]string{"voice": "bob's voice"}))
		if !strings.Contains(got, `"bob's voice"`) {
			t.Fatalf("rendered %q", got)
		}
	})
}

func TestTTSRunCommand(t *testing.T) {
	t.Run("test_silent_after_progress_still_times_out_with_stderr", func(t *testing.T) {
		_, err := runCommandProvider(nil, "echo 'starting tier 1' >&2; sleep 1", 0.2, nil)
		var te *commandTimeoutError
		if !errors.As(err, &te) {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(te.Stderr, "starting tier 1") {
			t.Fatalf("stderr = %q", te.Stderr)
		}
		var cause *commandTimeoutError
		if !errors.As(errors.Unwrap(te), &cause) {
			t.Fatal("timeout not chained from a timeout")
		}
	})
}

func TestTTSGenerateCommand(t *testing.T) {
	t.Run("test_writes_output_file", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "clip.mp3")
		got, err := generateCommandTTS(nil, "hello world", out, "py-copy", map[string]any{"command": "cp {input_path} {output_path}"}, map[string]any{})
		if err != nil || got != out {
			t.Fatalf("generate = %q, %v", got, err)
		}
		data, err := os.ReadFile(out)
		if err != nil || strings.TrimPrefix(string(data), "\ufeff") != "hello world" {
			t.Fatalf("output = %q, %v", data, err)
		}
	})
	t.Run("test_timeout_raises_runtime", func(t *testing.T) {
		if hostOSName() == "nt" {
			return
		}
		_, err := generateCommandTTS(nil, "hello", filepath.Join(t.TempDir(), "x.mp3"), "slow", map[string]any{"command": "sleep 10", "timeout": 1}, map[string]any{})
		var re *ttsRuntimeError
		if !errors.As(err, &re) || !strings.Contains(re.Error(), "timed out") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestTTSCommandEnvPassthrough(t *testing.T) {
	t.Run("test_env_passthrough_restores_named_keys", func(t *testing.T) {
		t.Setenv("MY_TTS_API_KEY", "sk-provider")
		t.Setenv("OPENAI_API_KEY", "sk-openai")
		got := captureCommandSpawn(t)
		res, err := runCommandProvider(nil, "echo hi", 5, []string{"MY_TTS_API_KEY"})
		if err != nil || res.Returncode != 0 {
			t.Fatalf("run = %v, %v", res, err)
		}
		env := envList(got.cmd)
		if env["MY_TTS_API_KEY"] != "sk-provider" {
			t.Fatalf("MY_TTS_API_KEY = %q", env["MY_TTS_API_KEY"])
		}
		if _, ok := env["OPENAI_API_KEY"]; ok {
			t.Fatal("OPENAI_API_KEY leaked")
		}
	})
	t.Run("test_allowlist_parsed_from_provider_config", func(t *testing.T) {
		if got := commandEnvPassthrough(map[string]any{"env_passthrough": []any{"A_KEY", " B_KEY ", ""}}); !reflect.DeepEqual(got, []string{"A_KEY", "B_KEY"}) {
			t.Fatalf("got %v", got)
		}
		if got := commandEnvPassthrough(map[string]any{}); len(got) != 0 {
			t.Fatalf("got %v", got)
		}
		if got := commandEnvPassthrough(map[string]any{"env_passthrough": "A_KEY"}); len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})
}

func TestTTSCommandProviderSpawnGroupKwargs(t *testing.T) {
	t.Run("test_nt_branch_hides_the_console_window", func(t *testing.T) {
		kw := providerPopenGroupKwargs("nt")
		if kw["creationflags"] != uint32(0x08000000|0x00000200) {
			t.Fatalf("creationflags = %v", kw["creationflags"])
		}
		if _, ok := kw["start_new_session"]; ok {
			t.Fatal("start_new_session present")
		}
	})
	t.Run("test_nt_branch_matches_the_shared_detach_bundle", func(t *testing.T) {
		if providerPopenGroupKwargs("nt")["creationflags"] != uint32(windowsCreateNewProcessGroup|windowsCreateNoWindow) {
			t.Fatal("creationflags differ from the detach bundle")
		}
	})
	t.Run("test_posix_branch_leaves_the_group_to_spawn_owned", func(t *testing.T) {
		if len(providerPopenGroupKwargs("posix")) != 0 {
			t.Fatal("posix kwargs not empty")
		}
	})
	t.Run("test_posix_spawn_is_owned_and_has_no_console_flags", func(t *testing.T) {
		if hostOSName() == "nt" {
			return
		}
		got := captureCommandSpawn(t)
		if _, err := runCommandProvider(nil, "echo hi", 5, nil); err != nil {
			t.Fatal(err)
		}
		if got.purpose != "tts-command" {
			t.Fatalf("purpose = %q", got.purpose)
		}
		if got.cmd.SysProcAttr != nil {
			t.Fatalf("spawn attributes set before spawn_owned: %+v", got.cmd.SysProcAttr)
		}
	})
}

func TestTTSCommandRenderQuotesBareValues(t *testing.T) {
	if hostOSName() == "nt" {
		return
	}
	got := renderCommandTemplate("tts {voice}", commandTemplateValues(map[string]string{"voice": "a b"}))
	if got != "tts "+pyshlex.Quote("a b") {
		t.Fatalf("rendered %q", got)
	}
}
