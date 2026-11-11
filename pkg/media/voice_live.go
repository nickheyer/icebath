package media

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	GPTLiveMode        = "gpt-live"
	ChainedMode        = "chained"
	DefaultLiveModel   = "gpt-live-1"
	DefaultLiveVoice   = "marin"
	DefaultLiveBaseURL = "https://api.openai.com/v1"
)

var GPTLiveVoices = []string{"marin", "cedar", "quartz", "ripple", "vesper", "willow", "stone", "gleam", "meridian",
	"bossa", "tempo", "beacon", "delta", "cinder"}

const LivePersona = "You are Icebath, a calm and friendly voice assistant. Speak naturally at an unhurried pace. " +
	"Be clear and direct, not overly cheerful. If the user is frustrated, acknowledge it briefly " +
	"and focus on the next helpful step.\n\n" +
	"Backchannel policy: Use moderate backchannels. Acknowledge naturally without competing with " +
	"the main response.\n\n" +
	"Interruption policy: Stop speaking when the user interrupts. Listen to what they say.\n\n" +
	"Delegation policy:\n" +
	"Backend tools:\n" +
	"- Icebath agent: a full AI agent with tools — it can run commands, read and edit files, " +
	"browse the web, search, remember things across sessions, schedule tasks, and reason " +
	"carefully about anything. It is the one who actually does work and knows facts.\n\n" +
	"Delegate to the backend when:\n" +
	"- The user asks a question that needs facts, current information, or careful reasoning.\n" +
	"- The user asks you to do, check, find, make, fix, run or remember anything.\n" +
	"- A correction changes work already requested.\n\n" +
	"Do not delegate to the backend when:\n" +
	"- The user greets you, makes small talk, or asks you to repeat a result already provided.\n" +
	"- You need a brief clarification to understand the request.\n\n" +
	"Delegate before giving an answer that depends on backend work. Do not guess the result " +
	"while waiting; say briefly that you are checking, then wait for the result."

func voiceLiveLog() *slog.Logger { return slog.Default().With("logger", "tools.voice_live") }

// ValueError of a GPT-Live session that cannot start, a missing key
type VoiceLiveValueError struct{ Msg string }

func (e *VoiceLiveValueError) Error() string { return e.Msg }

// RuntimeError of a GPT-Live session the vendor rejected, with its status and detail
type VoiceLiveRuntimeError struct{ Msg string }

func (e *VoiceLiveRuntimeError) Error() string { return e.Msg }

// The gpt_live block of a voice section, empty when absent or not a dict
func liveSection(voice map[string]any) map[string]any {
	section, ok := voice["gpt_live"].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return section
}

// Chained by default, gpt-live for that mode under any of its spellings
func VoiceChatMode(voice map[string]any) string {
	raw := voice["voice_chat_mode"]
	if !pyrepr.Truthy(raw) {
		raw = ChainedMode
	}
	mode := strings.ReplaceAll(pystr.Lower(pystr.Strip(pyrepr.Str(raw))), "_", "-")
	if mode == GPTLiveMode || mode == "gptlive" || mode == "live" {
		return GPTLiveMode
	}
	return ChainedMode
}

// Text of a section value or the fallback for a falsy one
func liveText(live map[string]any, key, fallback string) string {
	v := live[key]
	if !pyrepr.Truthy(v) {
		return fallback
	}
	return pyrepr.Str(v)
}

// API key and base URL, the gpt_live key first else the OpenAI audio key chain of the STT and TTS providers
func resolveLiveCredentials(ctx context.Context, live map[string]any) (string, string) {
	apiKey := pystr.Strip(liveText(live, "api_key", ""))
	if apiKey == "" {
		apiKey = tools.ResolveOpenAIAudioAPIKey(&tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)})
	}
	baseURL := strings.TrimRight(pystr.Strip(liveText(live, "base_url", DefaultLiveBaseURL)), "/")
	return apiKey, baseURL
}

// Persona of the live voice followed by the configured extra instructions
func LiveInstructions(live map[string]any) string {
	if extra := pystr.Strip(liveText(live, "instructions", "")); extra != "" {
		return LivePersona + "\n\n" + extra
	}
	return LivePersona
}

// Selected voice chat mode and whether GPT-Live can start, never carrying the key
func ResolveGPTLiveStatus(ctx context.Context) *pyjson.Object {
	voice := voiceConfig(ctx)
	live := liveSection(voice)
	apiKey, _ := resolveLiveCredentials(ctx, live)
	var reason any
	if apiKey == "" {
		reason = "no OpenAI API key (set OPENAI_API_KEY or voice.gpt_live.api_key)"
	}
	return pyjson.ObjectOf("mode", VoiceChatMode(voice), "available", apiKey != "", "reason", reason,
		"model", liveText(live, "model", DefaultLiveModel), "voice", liveText(live, "voice", DefaultLiveVoice))
}

// Session object of a client delegation WebRTC session with the prior conversation as its input
func BuildLiveSessionConfig(live map[string]any, history []any) *pyjson.Object {
	cfg := pyjson.ObjectOf("model", liveText(live, "model", DefaultLiveModel), "instructions", LiveInstructions(live),
		"audio", pyjson.ObjectOf("output", pyjson.ObjectOf("voice", liveText(live, "voice", DefaultLiveVoice))),
		"delegation", pyjson.ObjectOf("type", "client"))
	if len(history) > 0 {
		cfg.Set("input", history)
	}
	return cfg
}

// Exchanges the renderer's SDP offer for the live session answer with the key kept on this host
func CreateWebRTCSession(ctx context.Context, sdpOffer string, history []any) (any, error) {
	live := liveSection(voiceConfig(ctx))
	apiKey, baseURL := resolveLiveCredentials(ctx, live)
	if apiKey == "" {
		return nil, &VoiceLiveValueError{"GPT-Live needs an OpenAI API key (OPENAI_API_KEY or voice.gpt_live.api_key)"}
	}
	body, err := pyjson.Dumps(pyjson.ObjectOf("session", BuildLiveSessionConfig(live, history),
		"transport", pyjson.ObjectOf("type", "webrtc", "sdp", sdpOffer)))
	if err != nil {
		return nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, baseURL+"/live/sessions", bytes.NewReader([]byte(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		detail := pystr.Slice(pystr.DecodeUTF8Replace(raw), 0, 600)
		voiceLiveLog().Warn(fmt.Sprintf("GPT-Live session creation failed: %d %s", resp.StatusCode, detail))
		return nil, &VoiceLiveRuntimeError{fmt.Sprintf("GPT-Live session creation failed (%d): %s", resp.StatusCode, detail)}
	}
	text, err := pystr.DecodeUTF8(raw)
	if err != nil {
		return nil, err
	}
	return pyjson.Loads(text)
}
