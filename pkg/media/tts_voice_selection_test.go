package media

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// ElevenLabs voice config with a configured voice
func elevenLabsTestConfig() map[string]any {
	return map[string]any{"provider": "elevenlabs", "elevenlabs": map[string]any{"voice_id": "configuredVoice001"}}
}

// Decoded JSON envelope
func decodeEnvelope(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("%q: %v", raw, err)
	}
	return m
}

func TestVoiceSelectionSurfaceFollowsProvider(t *testing.T) {
	cases := []struct {
		name    string
		config  map[string]any
		exposed bool
	}{{"elevenlabs", elevenLabsTestConfig(), true}, {"edge", map[string]any{"provider": "edge"}, false}, {"openai", map[string]any{"provider": "openai"}, false}}
	for _, c := range cases {
		t.Run("test_voice_selection_surface_follows_provider/"+c.name, func(t *testing.T) {
			fixedTTSConfig(t, c.config)
			swap(t, &elevenLabsVoicesGet, func(context.Context, string, string) (int, []byte, error) {
				return 200, []byte(`{"voices": []}`), nil
			})
			swap(t, &ttsResolveProviderKey, func(*tools.Env, string, string) string { return "el-test-key" })
			schema := ttsSchemaOverrides(nil)
			raw, err := textToSpeechTool(context.Background(), nil, ttsCallArgs{FindVoices: map[string]any{"query": "orc"}})
			if err != nil {
				t.Fatal(err)
			}
			searched := decodeEnvelope(t, raw)
			params := schema["parameters"].(map[string]any)
			props := params["properties"].(map[string]any)
			_, hasVoice := props["voice_id"]
			_, hasFind := props["find_voices"]
			required := params["required"].([]any)
			desc, _ := schema["description"].(string)
			if hasVoice != c.exposed || hasFind != c.exposed || (len(required) == 0) != c.exposed || strings.Contains(desc, "find_voices") != c.exposed {
				t.Fatalf("schema = %v", schema)
			}
			if !c.exposed && (searched["success"] != false || !strings.Contains(searched["error"].(string), "elevenlabs")) {
				t.Fatalf("searched = %v", searched)
			}
		})
	}
}

func TestVoiceIDReachesElevenlabsAndIsIgnoredElsewhere(t *testing.T) {
	cfg := elevenLabsTestConfig()
	fixedTTSConfig(t, cfg)
	var seen []string
	swap(t, &ttsImportElevenLabs, func(context.Context) error { return nil })
	swap(t, &ttsResolveProviderKey, func(*tools.Env, string, string) string { return "el-test-key" })
	swap(t, &ttsElevenLabsPost, func(_ context.Context, endpoint, _ string, _ map[string]any) (*http.Response, error) {
		u, _ := url.Parse(endpoint)
		seen = append(seen, strings.TrimPrefix(u.Path, "/v1/text-to-speech/"))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader([]byte("ID3\x03\x00\x00\x00\x00\x00\x00")))}, nil
	})
	dir := t.TempDir()
	run := func(text, out, voice string) map[string]any {
		raw, err := textToSpeechTool(context.Background(), nil, ttsCallArgs{Text: text, OutputPath: filepath.Join(dir, out), VoiceID: voice})
		if err != nil {
			t.Fatal(err)
		}
		return decodeEnvelope(t, raw)
	}
	ok := run("Hail, traveler.", "a.mp3", "pickedVoice0042")
	def := run("Hail, traveler.", "b.mp3", "")
	bad := run("Hail.", "c.mp3", "../v1/user")
	if ok["success"] != true || def["success"] != true || bad["success"] != false {
		t.Fatalf("ok=%v default=%v bad=%v", ok, def, bad)
	}
	if len(seen) != 2 || seen[0] != "pickedVoice0042" || seen[1] != "configuredVoice001" {
		t.Fatalf("seen = %v", seen)
	}
	if cfg["elevenlabs"].(map[string]any)["voice_id"] != "configuredVoice001" {
		t.Fatal("cached config mutated")
	}
	config, errJSON := applyVoiceOverride(map[string]any{"provider": "edge"}, "edge", "pickedVoice0042")
	if _, has := config["elevenlabs"]; errJSON != "" || has {
		t.Fatalf("config=%v err=%q", config, errJSON)
	}
}

func TestFindVoicesPagesBothSources(t *testing.T) {
	cases := []struct {
		name     string
		find     map[string]any
		path     string
		expected map[string]string
		body     string
		nextPage any
	}{
		{"library", map[string]any{"query": "gravelly old", "gender": "Male", "use_case": "characters_animation", "page": "3", "page_size": 500}, "/v1/shared-voices",
			map[string]string{"search": "gravelly old", "gender": "male", "use_cases": "characters_animation", "sort": "usage_character_count_1y", "page": "3", "page_size": "100"},
			`{"voices": [{"voice_id": "lib1", "name": "Gruff", "gender": "male", "age": "old", "accent": "scottish", "descriptive": "gruff", "use_case": "characters_animation", "category": "professional", "rate": 2, "description": "` + strings.Repeat("x", 400) + `"}], "has_more": true, "total_count": 812}`, "4"},
		{"account", map[string]any{"query": "orc", "source": "account", "page": "tok-1"}, "/v2/voices",
			map[string]string{"search": "orc", "next_page_token": "tok-1", "page_size": "20"},
			`{"voices": [{"voice_id": "acct1", "name": "Bram", "category": "premade", "labels": {"gender": "male", "age": "old", "accent": "scottish", "use case": "characters"}, "preview_url": "https://example.test/bram.mp3"}], "has_more": true, "next_page_token": "tok-2"}`, "tok-2"},
		{"defaults", map[string]any{}, "/v1/shared-voices", map[string]string{"sort": "usage_character_count_1y", "page": "0", "page_size": "20"},
			`{"voices": [{"voice_id": "lib2", "name": "Bram", "gender": "male", "accent": "scottish"}], "has_more": false}`, nil},
	}
	for _, c := range cases {
		t.Run("test_find_voices_pages_both_sources/"+c.name, func(t *testing.T) {
			fixedTTSConfig(t, elevenLabsTestConfig())
			swap(t, &ttsResolveProviderKey, func(*tools.Env, string, string) string { return "el-test-key" })
			var calls []string
			var keys []string
			swap(t, &elevenLabsVoicesGet, func(_ context.Context, rawURL, apiKey string) (int, []byte, error) {
				calls = append(calls, rawURL)
				keys = append(keys, apiKey)
				return 200, []byte(c.body), nil
			})
			result := decodeEnvelope(t, handleTextToSpeech(context.Background(), &tools.Call{Args: map[string]any{"find_voices": c.find}}))
			rawBad, err := textToSpeechTool(context.Background(), nil, ttsCallArgs{FindVoices: map[string]any{"page": "tok-2"}})
			if err != nil {
				t.Fatal(err)
			}
			badPage := decodeEnvelope(t, rawBad)
			u, _ := url.Parse(calls[0])
			if u.Scheme+"://"+u.Host != "https://api.elevenlabs.io" || u.Path != c.path || keys[0] != "el-test-key" {
				t.Fatalf("url = %s", calls[0])
			}
			for k, v := range c.expected {
				if got := u.Query().Get(k); got != v {
					t.Fatalf("param %s = %q, want %q", k, got, v)
				}
			}
			row := result["voices"].([]any)[0].(map[string]any)
			if result["success"] != true || result["next_page"] != c.nextPage || row["gender"] != "male" || row["accent"] != "scottish" || result["configured_voice_id"] != "configuredVoice001" {
				t.Fatalf("result = %v", result)
			}
			if c.name == "library" && (result["total"] != float64(812) || len([]rune(row["description"].(string))) != 240 || row["credit_rate"] != float64(2)) {
				t.Fatalf("row = %v", row)
			}
			if badPage["success"] != false || len(calls) != 1 {
				t.Fatalf("bad page = %v calls = %d", badPage, len(calls))
			}
		})
	}
}

func TestProviderSwitchReshapesTheSchemaAtOnce(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ELEVENLABS_API_KEY", "el-test-key")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	configPath := filepath.Join(h, "config.yaml")
	def := tools.Default.Get("text_to_speech")
	if def == nil || def.DynamicSchema == nil {
		t.Fatal("text_to_speech is not registered with dynamic schema overrides")
	}
	props := func(provider string, stamp int64) map[string]any {
		if err := os.WriteFile(configPath, []byte("tts:\n  provider: "+provider+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		at := time.Unix(stamp, 0)
		if err := os.Chtimes(configPath, at, at); err != nil {
			t.Fatal(err)
		}
		return def.DynamicSchema(&tools.Env{Home: h})["parameters"].(map[string]any)["properties"].(map[string]any)
	}
	if _, ok := props("openai", 1_700_000_000)["find_voices"]; ok {
		t.Fatal("openai listing exposed find_voices")
	}
	p := props("elevenlabs", 1_700_000_100)
	if _, ok := p["find_voices"]; !ok {
		t.Fatal("elevenlabs listing lacks find_voices")
	}
	if _, ok := p["voice_id"]; !ok {
		t.Fatal("elevenlabs listing lacks voice_id")
	}
	if _, ok := props("openai", 1_700_000_200)["find_voices"]; ok {
		t.Fatal("switching back kept find_voices")
	}
}
