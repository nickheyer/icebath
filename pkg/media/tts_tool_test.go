package media

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Replaces a package seam for the test duration
func swap[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// Replaces the tts config loader with a fixed section
func fixedTTSConfig(t *testing.T, cfg map[string]any) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	swap(t, &loadTTSConfig, func(context.Context, *tools.Env) map[string]any { return cfg })
}

// Decoded tool result envelope of one call without a session env
func runTTS(t *testing.T, a ttsCallArgs) map[string]any {
	t.Helper()
	raw, err := textToSpeechTool(context.Background(), nil, a)
	if err != nil {
		t.Fatalf("tool error: %v", err)
	}
	var data map[string]any
	if jerr := json.Unmarshal([]byte(raw), &data); jerr != nil {
		t.Fatalf("result %q: %v", raw, jerr)
	}
	return data
}

// Fake engine writing one byte and recording each chunk
func recordingEngine(captured *[]string) ttsGenerator {
	return func(_ context.Context, _ *tools.Env, text, out string, _ map[string]any) (string, error) {
		*captured = append(*captured, text)
		return out, os.WriteFile(out, []byte{0}, 0o644)
	}
}

// Fake combiner concatenating chunk bytes
func byteConcat(paths []string, outputPath string, _ bool) (string, error) {
	var all []byte
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		all = append(all, b...)
	}
	return outputPath, os.WriteFile(outputPath, all, 0o644)
}

// Fake OpenAI generator recording each chunk
func recordingOpenAI(captured *[]string) func(context.Context, *tools.Env, string, string, map[string]any, openAITTSOptions) (string, error) {
	return func(_ context.Context, _ *tools.Env, text, out string, _ map[string]any, _ openAITTSOptions) (string, error) {
		*captured = append(*captured, text)
		return out, os.WriteFile(out, []byte{0}, 0o644)
	}
}

func TestResolveMaxTextLength(t *testing.T) {
	t.Run("test_unknown_provider_falls_back", func(t *testing.T) {
		if got := resolveMaxTextLength("does-not-exist", map[string]any{}); got != fallbackMaxTextLength {
			t.Fatalf("got %d", got)
		}
	})
	t.Run("test_empty_provider_falls_back", func(t *testing.T) {
		if got := resolveMaxTextLength("", map[string]any{}); got != fallbackMaxTextLength {
			t.Fatalf("got %d", got)
		}
		if got := resolveMaxTextLength("", nil); got != fallbackMaxTextLength {
			t.Fatalf("got %d", got)
		}
	})
}

func TestTextToSpeechToolChunking(t *testing.T) {
	t.Run("test_openai_chunks_at_4096_without_dropping_text", func(t *testing.T) {
		var captured []string
		swap(t, &ttsGenerateOpenAI, recordingOpenAI(&captured))
		swap(t, &ttsConcatAudioFiles, byteConcat)
		fixedTTSConfig(t, map[string]any{"provider": "openai"})
		text := strings.Repeat("A", 5000)
		data := runTTS(t, ttsCallArgs{Text: text, OutputPath: filepath.Join(t.TempDir(), "out.mp3")})
		if data["success"] != true || len(captured) != 2 || len(captured[0]) != 4096 || len(captured[1]) != 904 || strings.Join(captured, "") != text || data["chunk_count"] != float64(2) {
			t.Fatalf("data=%v chunks=%d", data, len(captured))
		}
	})
	t.Run("test_xai_accepts_much_longer_input", func(t *testing.T) {
		var captured []string
		swap(t, &ttsGenerateXAI, recordingEngine(&captured))
		fixedTTSConfig(t, map[string]any{"provider": "xai"})
		data := runTTS(t, ttsCallArgs{Text: strings.Repeat("B", 12000), OutputPath: filepath.Join(t.TempDir(), "out.mp3")})
		if data["success"] != true || len(captured) != 1 || len(captured[0]) != 12000 {
			t.Fatalf("data=%v", data)
		}
	})
	t.Run("test_user_override_is_respected", func(t *testing.T) {
		var captured []string
		swap(t, &ttsGenerateOpenAI, recordingOpenAI(&captured))
		swap(t, &ttsConcatAudioFiles, byteConcat)
		fixedTTSConfig(t, map[string]any{"provider": "openai", "openai": map[string]any{"max_text_length": 100}})
		text := strings.Repeat("C", 500)
		data := runTTS(t, ttsCallArgs{Text: text, OutputPath: filepath.Join(t.TempDir(), "out.mp3")})
		if data["success"] != true || strings.Join(captured, "") != text {
			t.Fatalf("data=%v", data)
		}
		for _, c := range captured {
			if len(c) > 100 {
				t.Fatalf("chunk of %d", len(c))
			}
		}
	})
}

func TestSplitTextForTts(t *testing.T) {
	t.Run("test_short_text_returns_single_chunk", func(t *testing.T) {
		if got := splitTextForTTS("Hello world.", 4096); len(got) != 1 || got[0] != "Hello world." {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("test_empty_text_returns_empty_list", func(t *testing.T) {
		if len(splitTextForTTS("", 4096)) != 0 || len(splitTextForTTS("   ", 4096)) != 0 {
			t.Fatal("expected no chunks")
		}
	})
	t.Run("test_long_text_is_split_without_loss", func(t *testing.T) {
		text := strings.Repeat("A", 5000)
		got := splitTextForTTS(text, 4096)
		if len(got) != 2 || got[0] != strings.Repeat("A", 4096) || got[1] != strings.Repeat("A", 904) || strings.Join(got, "") != text {
			t.Fatalf("got %d chunks", len(got))
		}
	})
	t.Run("test_splits_on_sentence_boundaries", func(t *testing.T) {
		text := "First sentence. Second sentence. Third sentence."
		got := splitTextForTTS(text, 30)
		if len(got) < 2 || strings.ReplaceAll(strings.Join(got, ""), " ", "") != strings.ReplaceAll(text, " ", "") {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("test_handles_very_long_word", func(t *testing.T) {
		text := strings.Repeat("A", 100)
		got := splitTextForTTS(text, 30)
		for _, c := range got {
			if len(c) > 30 {
				t.Fatalf("chunk %q", c)
			}
		}
		if strings.Join(got, "") != text {
			t.Fatalf("got %q", got)
		}
	})
}

func TestSplitOversizedSentence(t *testing.T) {
	t.Run("test_short_sentence_returns_as_is", func(t *testing.T) {
		if got := splitOversizedSentence("Hello world.", 100); len(got) != 1 || got[0] != "Hello world." {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("test_word_boundary_split", func(t *testing.T) {
		for _, c := range splitOversizedSentence(strings.TrimSpace(strings.Repeat("word ", 50)), 30) {
			if len(c) > 30 {
				t.Fatalf("chunk %q", c)
			}
		}
	})
}

// Writes n bytes to a file under dir
func sizedFile(t *testing.T, dir, name string, n int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(strings.Repeat("x", n)), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPackAudioFilesForDelivery(t *testing.T) {
	t.Run("test_single_file_returns_one_group", func(t *testing.T) {
		f := sizedFile(t, t.TempDir(), "a.mp3", 100)
		groups, err := packAudioFilesForDelivery([]string{f}, audioDeliveryProfile{Platform: "default", MaxFileBytes: 10000, SafetyRatio: 0.85})
		if err != nil || len(groups) != 1 || len(groups[0]) != 1 || groups[0][0] != f {
			t.Fatalf("groups=%v err=%v", groups, err)
		}
	})
	t.Run("test_splits_on_size_limit", func(t *testing.T) {
		dir := t.TempDir()
		var files []string
		for _, n := range []string{"chunk00.mp3", "chunk01.mp3", "chunk02.mp3", "chunk03.mp3", "chunk04.mp3"} {
			files = append(files, sizedFile(t, dir, n, 300))
		}
		groups, err := packAudioFilesForDelivery(files, audioDeliveryProfile{Platform: "default", MaxFileBytes: 1000, SafetyRatio: 0.5})
		if err != nil || len(groups) != 5 {
			t.Fatalf("groups=%v err=%v", groups, err)
		}
		for _, g := range groups {
			if len(g) != 1 {
				t.Fatalf("group %v", g)
			}
		}
	})
	t.Run("test_splits_on_suffix_mismatch", func(t *testing.T) {
		dir := t.TempDir()
		groups, err := packAudioFilesForDelivery([]string{sizedFile(t, dir, "a.mp3", 100), sizedFile(t, dir, "b.ogg", 100)},
			audioDeliveryProfile{Platform: "default", MaxFileBytes: 10000, SafetyRatio: 0.85})
		if err != nil || len(groups) != 2 {
			t.Fatalf("groups=%v err=%v", groups, err)
		}
	})
}

func TestBuildAudioDeliveryFiles(t *testing.T) {
	t.Run("test_single_file_passes_through", func(t *testing.T) {
		dir := t.TempDir()
		paths, combined, err := buildAudioDeliveryFiles([]string{sizedFile(t, dir, "chunk.mp3", 100)}, filepath.Join(dir, "output.mp3"),
			audioDeliveryProfile{Platform: "default", MaxFileBytes: 10000, SafetyRatio: 0.85}, false)
		if err != nil || len(paths) != 1 || combined {
			t.Fatalf("paths=%v combined=%v err=%v", paths, combined, err)
		}
	})
	t.Run("test_oversized_chunk_raises", func(t *testing.T) {
		dir := t.TempDir()
		_, _, err := buildAudioDeliveryFiles([]string{sizedFile(t, dir, "chunk.mp3", 100)}, filepath.Join(dir, "output.mp3"),
			audioDeliveryProfile{Platform: "default", MaxFileBytes: 50, SafetyRatio: 0.85}, false)
		var ve *ttsValueError
		if !errors.As(err, &ve) || !strings.Contains(ve.Error(), "exceeds") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestMaxTextLengthForCommandProviders(t *testing.T) {
	t.Run("test_default_for_command_provider", func(t *testing.T) {
		cfg := map[string]any{"providers": map[string]any{"piper-cli": map[string]any{"type": "command", "command": "x"}}}
		if got := resolveMaxTextLength("piper-cli", cfg); got != defaultCommandTTSMaxTextLength {
			t.Fatalf("got %d", got)
		}
	})
	t.Run("test_override_under_legacy_tts_name_block", func(t *testing.T) {
		cfg := map[string]any{"piper-cli": map[string]any{"type": "command", "command": "x", "max_text_length": 7777}}
		if got := resolveMaxTextLength("piper-cli", cfg); got != 7777 {
			t.Fatalf("got %d", got)
		}
	})
	t.Run("test_non_command_unknown_provider_still_falls_back", func(t *testing.T) {
		if got := resolveMaxTextLength("unknown", map[string]any{}); got <= 0 {
			t.Fatalf("got %d", got)
		}
	})
}

func TestTextToSpeechToolWithCommandProvider(t *testing.T) {
	copyCommand := "cp {input_path} {output_path}"
	t.Run("test_command_provider_dispatches_end_to_end", func(t *testing.T) {
		fixedTTSConfig(t, map[string]any{"provider": "py-copy", "providers": map[string]any{
			"py-copy": map[string]any{"type": "command", "command": copyCommand, "output_format": "mp3"}}})
		data := runTTS(t, ttsCallArgs{Text: "hi", OutputPath: filepath.Join(t.TempDir(), "clip.mp3")})
		if data["success"] != true || data["provider"] != "py-copy" || data["voice_compatible"] != false {
			t.Fatalf("data = %v", data)
		}
		if _, err := os.Stat(data["file_path"].(string)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("test_voice_compatible_opt_in_toggles_flag", func(t *testing.T) {
		fixedTTSConfig(t, map[string]any{"provider": "py-copy-ogg", "providers": map[string]any{
			"py-copy-ogg": map[string]any{"type": "command", "command": copyCommand, "output_format": "ogg", "voice_compatible": true}}})
		data := runTTS(t, ttsCallArgs{Text: "hi", OutputPath: filepath.Join(t.TempDir(), "clip.ogg")})
		if data["success"] != true || data["voice_compatible"] != true || !strings.HasPrefix(data["media_tag"].(string), "[[audio_as_voice]]") {
			t.Fatalf("data = %v", data)
		}
	})
	t.Run("test_missing_command_falls_through_to_builtin", func(t *testing.T) {
		fixedTTSConfig(t, map[string]any{"provider": "broken", "providers": map[string]any{"broken": map[string]any{"type": "command", "command": "   "}}})
		swap(t, &ttsModuleImportable, func(context.Context, string) bool { return false })
		swap(t, &ttsEnsureImport, func(context.Context, string) error { return nil })
		data := runTTS(t, ttsCallArgs{Text: "hi", OutputPath: filepath.Join(t.TempDir(), "x.mp3")})
		errText, _ := data["error"].(string)
		if strings.Contains(strings.ToLower(errText), "tts.providers.broken.command is not configured") {
			t.Fatalf("data = %v", data)
		}
	})
}

func TestCheckTtsRequirements(t *testing.T) {
	t.Run("test_configured_command_provider_satisfies_requirement", func(t *testing.T) {
		fixedTTSConfig(t, map[string]any{"provider": "x", "providers": map[string]any{"x": map[string]any{"type": "command", "command": "echo x"}}})
		if !checkTTSRequirements(context.Background(), nil) {
			t.Fatal("expected requirement satisfied")
		}
	})
}

func TestLoadsOnMissAndServesFromCacheOnHit(t *testing.T) {
	cache := newTTSModelCache()
	calls := 0
	load := func() (any, error) { calls++; return "model", nil }
	for range 2 {
		if v, err := ttsCacheGetOrLoad(cache, "a", load); err != nil || v != "model" {
			t.Fatalf("v=%v err=%v", v, err)
		}
	}
	if calls != 1 {
		t.Fatalf("loaded %d times", calls)
	}
}

func TestHitRefreshesRecencySoEvictionIsLruNotFifo(t *testing.T) {
	swap(t, &ttsModelCacheMax, 2)
	cache := newTTSModelCache()
	for _, k := range []string{"a", "b", "a", "c"} {
		key := k
		if _, err := ttsCacheGetOrLoad(cache, key, func() (any, error) { return key, nil }); err != nil {
			t.Fatal(err)
		}
	}
	if keys := cache.Keys(); len(keys) != 2 || keys[0] != "a" || keys[1] != "c" {
		t.Fatalf("keys = %v", keys)
	}
}

// Edge default with no SDK, no NeuTTS fallback and installs recorded as failures
func edgeWithoutSDK(t *testing.T) *[]string {
	t.Helper()
	fixedTTSConfig(t, map[string]any{"provider": "edge"})
	swap(t, &ttsPMExtraAvailable, func(context.Context, string) bool { return false })
	swap(t, &ttsCheckNeuTTS, func(context.Context) bool { return false })
	return recordInstalls(t)
}

// Records any install a requirement check attempts
func recordInstalls(t *testing.T) *[]string {
	t.Helper()
	calls := &[]string{}
	swap(t, &ttsEnsureImport, func(_ context.Context, extra string) error {
		*calls = append(*calls, extra)
		return errors.New("a TTS requirement check installed " + extra)
	})
	return calls
}

func TestInstallableSdkReadsReadyWithoutInstalling(t *testing.T) {
	installs := edgeWithoutSDK(t)
	swap(t, &ttsLazyInstallsAllowed, func(context.Context) bool { return true })
	if !checkTTSRequirements(context.Background(), nil) || len(*installs) != 0 {
		t.Fatalf("installs = %v", *installs)
	}
}

func TestMissingSdkReportsUnavailableWhenInstallCannotHelp(t *testing.T) {
	installs := edgeWithoutSDK(t)
	swap(t, &ttsLazyInstallsAllowed, func(context.Context) bool { return false })
	if checkTTSRequirements(context.Background(), nil) || len(*installs) != 0 {
		t.Fatalf("installs = %v", *installs)
	}
}

func TestSecondProviderNeedsItsOwnCredentialBeforeFirstUseCounts(t *testing.T) {
	installs := recordInstalls(t)
	fixedTTSConfig(t, map[string]any{"provider": "elevenlabs"})
	swap(t, &ttsPMExtraAvailable, func(context.Context, string) bool { return false })
	swap(t, &ttsResolveProviderKey, func(*tools.Env, string, string) string { return "" })
	swap(t, &ttsLazyInstallsAllowed, func(context.Context) bool { return true })
	if checkTTSRequirements(context.Background(), nil) || len(*installs) != 0 {
		t.Fatalf("installs = %v", *installs)
	}
}

func TestDefaultOutputPathsDifferWithinTheSameSecond(t *testing.T) {
	dir := t.TempDir()
	stamps := []time.Time{time.Date(2026, 9, 23, 12, 0, 0, 1000*1000, time.Local), time.Date(2026, 9, 23, 12, 0, 0, 2000*1000, time.Local)}
	swap(t, &ttsNow, func() time.Time { s := stamps[0]; stamps = stamps[1:]; return s })
	swap(t, &ttsDefaultOutputDir, func(context.Context, *tools.Env) string { return dir })
	first, err1, ferr := resolveOutputBase(context.Background(), nil, "", "edge", nil, false)
	second, err2, serr := resolveOutputBase(context.Background(), nil, "", "edge", nil, false)
	if err1 != "" || err2 != "" || ferr != nil || serr != nil {
		t.Fatalf("errors %q %q %v %v", err1, err2, ferr, serr)
	}
	if filepath.Dir(first) != dir || filepath.Dir(second) != dir || first == second {
		t.Fatalf("first=%s second=%s", first, second)
	}
}

func TestDefaultOutputDirFollowsContextvarProfileOverride(t *testing.T) {
	root := t.TempDir()
	defaultHome := filepath.Join(root, "default-home")
	profileHome := filepath.Join(root, "profiles", "ramona")
	for _, d := range []string{defaultHome, profileHome} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", defaultHome)
	scoped := home.WithOverride(context.Background(), profileHome)
	if got := ttsDefaultOutputDir(scoped, nil); got != filepath.Join(profileHome, "cache", "audio") {
		t.Fatalf("scoped = %s", got)
	}
	if got := ttsDefaultOutputDir(context.Background(), nil); got != filepath.Join(defaultHome, "cache", "audio") {
		t.Fatalf("unscoped = %s", got)
	}
}
