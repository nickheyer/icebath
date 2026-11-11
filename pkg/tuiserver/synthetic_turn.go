package tuiserver

import (
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// True when the synthetic heavy turn seam of the isolation certify harness is armed
func synthTurnArmed() bool { return os.Getenv("ICEBATH_ISO_CERTIFY_SYNTH_TURN") == "1" }

// Per turn intensity of a synthetic turn
type synthSpec struct {
	durationS, deltaIntervalS, sleepS float64
	chunk, tokensPerDelta             int64
}

// Agent shaped CPU burn whose turn holds a core with pure computation and streams synthetic deltas
type SyntheticHeavyAgent struct {
	SessionID, Model, Provider, APIMode           string
	BaseURL, APIKey, Platform, CachedSystemPrompt string
	ConfigContextLength                           int
	SessionInputTokens, SessionOutputTokens       int64
	SessionPromptTokens, SessionCompletionTokens  int64
	SessionReasoningTokens, SessionTotalTokens    int64
	SessionAPICalls                               int64
	History                                       []map[string]any

	mu        sync.Mutex
	interrupt pythread.Event
}

// Synthetic agent for a session reporting the given model label
func NewSyntheticHeavyAgent(sessionID, model string) *SyntheticHeavyAgent {
	if model == "" {
		model = "synthetic-heavy"
	}
	return &SyntheticHeavyAgent{SessionID: sessionID, Model: model, Provider: "synthetic", APIMode: "chat_completions",
		ConfigContextLength: 200_000, History: []map[string]any{}}
}

// Clears a pending interrupt before the next turn
func (a *SyntheticHeavyAgent) ClearInterrupt() { a.interrupt.Clear() }

// Stops the running burn
func (a *SyntheticHeavyAgent) Interrupt() { a.interrupt.Set() }

// Teardown that also stops the burn
func (a *SyntheticHeavyAgent) Close() { a.interrupt.Set() }

// Whether the turn has stream consumers, always true for the synthetic loop
func (a *SyntheticHeavyAgent) HasStreamConsumers() bool { return true }

// Python float of a spec value, false when it cannot convert
func synthFloat(v any) (float64, bool) {
	f, err := pyFloat(v)
	return f, err == nil
}

// Python int of a spec value truncating floats, false when it cannot convert
func synthInt(v any) (int64, bool) {
	if f, isFloat := v.(float64); isFloat {
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return 0, false
		}
		return int64(f), true
	}
	n, err := pyint.Int64(v)
	return n, err == nil
}

// Intensity spec of a turn from a JSON object prompt, env and built in defaults filling every absent key
func parseSynthSpec(message any) (synthSpec, error) {
	spec := map[string]any{}
	if text, ok := message.(string); ok && strings.HasPrefix(pystr.Strip(text), "{") {
		if parsed, err := pyjson.Loads(pystr.Strip(text)); err == nil {
			if obj, isDict := parsed.(*pyjson.Object); isDict {
				spec = objectMap(obj)
			}
		}
	}
	floatField := func(key string, def func() float64) (float64, error) {
		raw, present := spec[key]
		if !present {
			return def(), nil
		}
		if f, ok := synthFloat(raw); ok {
			return f, nil
		}
		return 0, fmt.Errorf("could not convert %s to float: %s", key, pyrepr.Repr(raw))
	}
	intField := func(key string, def func() int64) (int64, error) {
		raw, present := spec[key]
		if !present {
			return def(), nil
		}
		if n, ok := synthInt(raw); ok {
			return n, nil
		}
		return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.Repr(raw))
	}
	var out synthSpec
	var err error
	if out.durationS, err = floatField("duration_s", func() float64 { return envFloat("ICEBATH_ISO_CERTIFY_DURATION_S", 8.0) }); err != nil {
		return out, err
	}
	if out.chunk, err = intField("chunk", func() int64 { return int64(envInt("ICEBATH_ISO_CERTIFY_CHUNK", 20_000)) }); err != nil {
		return out, err
	}
	if out.deltaIntervalS, err = floatField("delta_interval_s", func() float64 { return envFloat("ICEBATH_ISO_CERTIFY_DELTA_S", 0.05) }); err != nil {
		return out, err
	}
	if out.tokensPerDelta, err = intField("tokens_per_delta", func() int64 { return int64(envInt("ICEBATH_ISO_CERTIFY_TPD", 512)) }); err != nil {
		return out, err
	}
	if out.sleepS, err = floatField("sleep_s", func() float64 { return 0 }); err != nil {
		return out, err
	}
	return out, nil
}

// Runs one synthetic turn, burning CPU and streaming deltas until the duration elapses or an interrupt lands
func (a *SyntheticHeavyAgent) RunConversation(message any, conversationHistory []map[string]any, streamCallback func(string)) (map[string]any, error) {
	spec, err := parseSynthSpec(message)
	if err != nil {
		return nil, err
	}
	duration, chunk := max(0, spec.durationS), max(1, spec.chunk)
	interval, tokensPerDelta, sleepS := max(0.001, spec.deltaIntervalS), max(0, spec.tokensPerDelta), max(0, spec.sleepS)
	a.mu.Lock()
	base := a.History
	a.mu.Unlock()
	if conversationHistory != nil {
		base = conversationHistory
	}
	base = append([]map[string]any(nil), base...)
	start := pythread.Monotonic()
	lastDelta := start
	var acc uint64
	deltas := 0
	interrupted := false
	for {
		if interrupted = a.interrupt.IsSet(); interrupted {
			break
		}
		now := pythread.Monotonic()
		if now-start >= duration {
			break
		}
		for range chunk {
			acc = acc*1_000_003 + 12_345
		}
		if sleepS > 0 {
			time.Sleep(time.Duration(sleepS * float64(time.Second)))
		}
		if now-lastDelta >= interval {
			deltas++
			a.mu.Lock()
			a.SessionOutputTokens += tokensPerDelta
			a.SessionCompletionTokens += tokensPerDelta
			a.SessionTotalTokens += tokensPerDelta
			a.mu.Unlock()
			if streamCallback != nil {
				streamCallback(fmt.Sprintf("synthtok-%05d ", deltas))
			}
			lastDelta = now
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.SessionAPICalls++
	final := fmt.Sprintf("[synthetic heavy turn] deltas=%d out_tokens=%d interrupted=%s checksum=%04x",
		deltas, a.SessionOutputTokens, pyBoolWord(interrupted), acc&0xFFFF)
	a.History = append(base, map[string]any{"role": "user", "content": pystr.Slice(pyrepr.Str(message), 0, 200)},
		map[string]any{"role": "assistant", "content": final})
	return map[string]any{"final_response": final, "messages": a.History, "interrupted": interrupted, "error": nil, "last_reasoning": nil}, nil
}

// Python bool spelling of a flag
func pyBoolWord(v bool) string {
	if v {
		return "True"
	}
	return "False"
}

// Synthetic agent when the seam is armed, else nil, the override only labelling the reported model
func maybeBuildSyntheticAgent(sessionID string, modelOverride any) *SyntheticHeavyAgent {
	if !synthTurnArmed() {
		return nil
	}
	if dict, ok := modelOverride.(map[string]any); ok {
		if truthy(dict["model"]) {
			modelOverride = pyrepr.Str(dict["model"])
		} else {
			modelOverride = ""
		}
	}
	label, _ := modelOverride.(string)
	return NewSyntheticHeavyAgent(sessionID, label)
}
