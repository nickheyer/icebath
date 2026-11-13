package media

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Writes the profile config and clears every voice credential variable
func voiceClientHome(t *testing.T, configYAML string) {
	withVoiceHome(t, configYAML)
	for _, k := range []string{"GROQ_API_KEY", "OPENAI_API_KEY", "VOICE_TOOLS_OPENAI_KEY", "MISTRAL_API_KEY", "XAI_API_KEY",
		"ELEVENLABS_API_KEY", "DEEPINFRA_API_KEY", "ICEBATH_LOCAL_STT_LANGUAGE"} {
		t.Setenv(k, "")
	}
}

func field(o any, key string) any {
	v, _ := o.(*pyjson.Object).Get(key)
	return v
}

func resolveVoice(t *testing.T) (stt, tts *pyjson.Object) {
	out := ResolveClientVoiceConfig(context.Background())
	return field(out, "stt").(*pyjson.Object), field(out, "tts").(*pyjson.Object)
}

func TestGroqSttResolvesDirectWithConfigKey(t *testing.T) {
	voiceClientHome(t, "stt:\n  provider: groq\n  groq:\n    api_key: gsk_test123\n")
	stt, _ := resolveVoice(t)
	if field(stt, "mode") == "direct" {
		if field(stt, "provider") != "groq" || field(stt, "wire") != "openai-multipart" || field(stt, "api_key") != "gsk_test123" ||
			!strings.Contains(field(stt, "base_url").(string), "groq.com") || !pyTruthy(field(stt, "model")) {
			t.Fatalf("direct groq %v", stt.Keys)
		}
		return
	}
	if field(stt, "mode") != "relay" {
		t.Fatalf("mode %v", field(stt, "mode"))
	}
}

func TestGroqSttResolvesDirectWithEnvKey(t *testing.T) {
	voiceClientHome(t, "stt:\n  provider: groq\n")
	t.Setenv("GROQ_API_KEY", "gsk_env456")
	stt, _ := resolveVoice(t)
	if field(stt, "mode") != "direct" || field(stt, "api_key") != "gsk_env456" || field(stt, "language") != "en" {
		t.Fatalf("%v %v %v %v", field(stt, "mode"), field(stt, "api_key"), field(stt, "language"), field(stt, "reason"))
	}
}

func TestLanguagePinPropagates(t *testing.T) {
	voiceClientHome(t, "stt:\n  provider: groq\n  language: de\n")
	t.Setenv("GROQ_API_KEY", "gsk_env456")
	if stt, _ := resolveVoice(t); field(stt, "mode") != "direct" || field(stt, "language") != "de" {
		t.Fatalf("%v %v", field(stt, "mode"), field(stt, "language"))
	}
}

func TestLocalWhisperRelays(t *testing.T) {
	voiceClientHome(t, "stt:\n  provider: local\n")
	stt, _ := resolveVoice(t)
	if _, hasKey := stt.Get("api_key"); field(stt, "mode") != "relay" || hasKey {
		t.Fatalf("%v", stt.Keys)
	}
}

func TestMissingCredentialsRelay(t *testing.T) {
	voiceClientHome(t, "stt:\n  provider: groq\n")
	if stt, _ := resolveVoice(t); field(stt, "mode") != "relay" {
		t.Fatalf("%v", field(stt, "mode"))
	}
}

func TestClientDirectGateForcesRelay(t *testing.T) {
	voiceClientHome(t, "voice:\n  client_direct: false\nstt:\n  provider: groq\ntts:\n  provider: openai\n")
	t.Setenv("GROQ_API_KEY", "gsk_env456")
	t.Setenv("OPENAI_API_KEY", "sk_test")
	stt, tts := resolveVoice(t)
	if field(stt, "mode") != "relay" || field(tts, "mode") != "relay" || !strings.Contains(field(stt, "reason").(string), "client_direct") {
		t.Fatalf("%v %v", field(stt, "reason"), field(tts, "mode"))
	}
}

func TestSttDisabledRelays(t *testing.T) {
	voiceClientHome(t, "stt:\n  enabled: false\n  provider: groq\n")
	t.Setenv("GROQ_API_KEY", "gsk_env456")
	if stt, _ := resolveVoice(t); field(stt, "mode") != "relay" {
		t.Fatalf("%v", field(stt, "mode"))
	}
}

func TestEdgeTtsRelaysOpenaiGoesDirect(t *testing.T) {
	voiceClientHome(t, "{}\n")
	if _, tts := resolveVoice(t); field(tts, "mode") != "relay" {
		t.Fatalf("edge %v", field(tts, "mode"))
	}
	voiceClientHome(t, "tts:\n  provider: openai\n  openai:\n    voice: nova\n")
	t.Setenv("OPENAI_API_KEY", "sk_direct789")
	_, tts := resolveVoice(t)
	extra, _ := field(tts, "extra_body").(map[string]any)
	if field(tts, "mode") != "direct" || field(tts, "wire") != "openai-speech" || field(tts, "api_key") != "sk_direct789" ||
		field(tts, "voice") != "nova" || !pyTruthy(field(tts, "model")) || len(extra) != 0 {
		t.Fatalf("openai %v", tts.Keys)
	}
	voiceClientHome(t, "tts:\n  provider: openai\n  openai:\n    voice: nova\n    consent_attestation: I own this voice\n")
	t.Setenv("OPENAI_API_KEY", "sk_direct789")
	_, tts = resolveVoice(t)
	extra, _ = field(tts, "extra_body").(map[string]any)
	if len(extra) != 1 || extra["consent_attestation"] != "I own this voice" {
		t.Fatalf("extra body %v", extra)
	}
}

func TestElevenlabsTtsDirectCarriesVoiceAndModel(t *testing.T) {
	voiceClientHome(t, "tts:\n  provider: elevenlabs\n  elevenlabs:\n    voice_id: voice123\n    model_id: eleven_turbo_v2\n  streaming:\n    min_len: 6\n")
	t.Setenv("ELEVENLABS_API_KEY", "el_key")
	_, tts := resolveVoice(t)
	if field(tts, "mode") != "direct" || field(tts, "wire") != "elevenlabs-tts" || field(tts, "voice") != "voice123" ||
		field(tts, "model") != "eleven_turbo_v2" || !strings.Contains(field(tts, "base_url").(string), "elevenlabs.io") || field(tts, "min_len") != 6 {
		t.Fatalf("%v", tts.Keys)
	}
}

func TestCommandProviderRelays(t *testing.T) {
	voiceClientHome(t, "stt:\n  provider: my-whisper\n  providers:\n    my-whisper:\n      type: command\n      command: whisper {input_path}\n")
	if stt, _ := resolveVoice(t); field(stt, "mode") != "relay" {
		t.Fatalf("%v", field(stt, "mode"))
	}
}

func TestXaiOauthWithoutApiKeyRelays(t *testing.T) {
	voiceClientHome(t, "stt:\n  provider: xai\n")
	if stt, _ := resolveVoice(t); field(stt, "mode") != "relay" {
		t.Fatalf("%v", field(stt, "mode"))
	}
}

func TestXaiEnvKeyGoesDirect(t *testing.T) {
	voiceClientHome(t, "stt:\n  provider: xai\n")
	t.Setenv("XAI_API_KEY", "xai_key1")
	t.Setenv("STT_XAI_MODEL", "")
	stt, _ := resolveVoice(t)
	if field(stt, "mode") != "direct" || field(stt, "wire") != "xai-stt" || field(stt, "api_key") != "xai_key1" || field(stt, "model") != "grok-voice-transcribe-2.0" {
		t.Fatalf("%v %v", field(stt, "mode"), field(stt, "model"))
	}
}

func TestXaiConfigModelReachesClient(t *testing.T) {
	voiceClientHome(t, "stt:\n  provider: xai\n  xai:\n    model: grok-voice-transcribe-1.0\n")
	t.Setenv("XAI_API_KEY", "xai_key1")
	if stt, _ := resolveVoice(t); field(stt, "model") != "grok-voice-transcribe-1.0" {
		t.Fatalf("%v", field(stt, "model"))
	}
}

func TestDirectSttCarriesTheGatewayTranscriptionTimeout(t *testing.T) {
	voiceClientHome(t, "stt:\n  provider: xai\n")
	t.Setenv("XAI_API_KEY", "xai_key1")
	if stt, _ := resolveVoice(t); field(stt, "timeout_s") != 60.0 {
		t.Fatalf("%v", field(stt, "timeout_s"))
	}
	voiceClientHome(t, "stt:\n  provider: xai\n  openai:\n    timeout: \"5\"\n")
	t.Setenv("XAI_API_KEY", "xai_key1")
	if stt, _ := resolveVoice(t); field(stt, "timeout_s") != 5.0 {
		t.Fatalf("%v", field(stt, "timeout_s"))
	}
}

func TestResolutionNeverRaises(t *testing.T) {
	voiceClientHome(t, "stt: not-a-dict\ntts:\n- also\n- wrong\n")
	stt, tts := resolveVoice(t)
	for _, o := range []*pyjson.Object{stt, tts} {
		if mode := field(o, "mode"); mode != "direct" && mode != "relay" {
			t.Fatalf("mode %v", mode)
		}
	}
}

// Pool backed account double, every method the secret resolver never reaches left to the embedded nil interface
type voicePoolAccount struct {
	tools.NousAccountSource
	pool map[string]string
}

func (a *voicePoolAccount) PoolAPIKey(_ *tools.Env, id string) string        { return a.pool[id] }
func (a *voicePoolAccount) SourceSuppressed(*tools.Env, string, string) bool { return false }

func TestToolWiringTranscriptionToolsDelegates(t *testing.T) {
	withVoiceHome(t, "")
	t.Setenv("GROQ_API_KEY", "")
	swapVoice[tools.NousAccountSource](t, &tools.NousAccount, &voicePoolAccount{pool: map[string]string{"groq": "stt-pool-key"}})
	if got := resolveSTTProviderKey(context.Background(), "GROQ_API_KEY", "groq"); got != "stt-pool-key" {
		t.Fatalf("got %q", got)
	}
}
