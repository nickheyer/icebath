package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

var (
	moaUsageFields = []string{"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens"}
	moaAcctFields  = []string{"model", "provider", "temperature"}
	moaCostFields  = []string{"cost_usd", "cost_status", "cost_source"}
)

// Trace directory when moa.save_traces is on, moa.trace_dir overriding the home default
func moaTracesEnabledAndDir(ctx context.Context) (string, bool) {
	if !settings.MoaSaveTraces.Get(ctx) {
		return "", false
	}
	if override := settings.MoaTraceDir.Get(ctx); override != "" {
		return pathutil.ExpandEnv(pathutil.ExpandUser(override)), true
	}
	return filepath.Join(home.Dir(ctx), "moa-traces"), true
}

// Session id safe as a file name, every other character than alphanumerics and dash underscore dot replaced
func moaSanitizeSessionID(sessionID string) string {
	if sessionID == "" {
		return "unknown-session"
	}
	var b strings.Builder
	for _, c := range sessionID {
		if (unicode.IsLetter(c) || unicode.IsNumber(c)) || strings.ContainsRune("-_.", c) {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// Attribute of a reference accounting record or usage, def when absent
func moaAttr(obj any, key string, def any) any {
	if obj == nil {
		return def
	}
	if v := state.ToolCallField(obj, key); v != nil {
		return v
	}
	return def
}

// One reference accounting as a full trace record with its full input messages and output
func moaSlotTrace(acct any, label string) *jsonutil.Object {
	trace := jsonutil.NewObject()
	trace.Set("label", label)
	for _, f := range moaAcctFields {
		trace.Set(f, moaAttr(acct, f, nil))
	}
	trace.Set("input_messages", moaAttr(acct, "messages", nil))
	trace.Set("output", moaAttr(acct, "output", nil))
	usage := jsonutil.NewObject()
	if u := moaAttr(acct, "usage", nil); u != nil {
		for _, f := range moaUsageFields {
			usage.Set(f, moaAttr(u, f, 0))
		}
	}
	trace.Set("usage", usage)
	for _, f := range moaCostFields {
		trace.Set(f, moaAttr(acct, f, nil))
	}
	return trace
}

// Slot trace without its input messages for observability hooks, output overriding the accounting output
func SlotMetrics(acct any, label string, output any) *jsonutil.Object {
	trace := moaSlotTrace(acct, label)
	trace.Delete("input_messages")
	if output != nil {
		trace.Set("output", output)
	}
	return trace
}

// MoAReference is one reference output of a MoA turn, its label, display text and accounting record
type MoAReference struct {
	Label string
	Text  string
	Acct  any
}

// MoATurn is one MoA turn record, empty strings and nil standing for None
type MoATurn struct {
	SessionID               string
	PresetName              string
	ReferenceOutputs        []MoAReference
	AggregatorLabel         string
	AggregatorModel         any
	AggregatorProvider      any
	AggregatorTemperature   any
	AggregatorInputMessages any
	AggregatorOutput        *string
	AggregatorStreamed      bool
}

// Appends one full MoA turn record to the session trace JSONL when tracing is on, failures logged at debug
func SaveMoATurn(ctx context.Context, turn MoATurn) {
	base, ok := moaTracesEnabledAndDir(ctx)
	if !ok {
		return
	}
	var sessionID any
	if turn.SessionID != "" {
		sessionID = turn.SessionID
	}
	if err := writeMoATurn(base, sessionID, turn); err != nil {
		session := turn.SessionID
		if session == "" {
			session = "none"
		}
		pkgLog().Debug(fmt.Sprintf("MoA trace write failed (session=%s): %s", session, err))
	}
}

func writeMoATurn(base string, sessionID any, turn MoATurn) error {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return err
	}
	path := filepath.Join(base, moaSanitizeSessionID(turn.SessionID)+".jsonl")
	location := "assistant_message_in_session_db"
	if !turn.AggregatorStreamed {
		location = "inline"
	} else if turn.AggregatorOutput != nil && *turn.AggregatorOutput != "" {
		location = "inline_from_stream"
	}
	var output any
	if turn.AggregatorOutput != nil {
		output = *turn.AggregatorOutput
	}
	refs := make([]any, len(turn.ReferenceOutputs))
	for i, r := range turn.ReferenceOutputs {
		refs[i] = moaSlotTrace(r.Acct, r.Label)
	}
	aggregator := jsonutil.NewObject()
	for _, kv := range []struct {
		k string
		v any
	}{{"label", turn.AggregatorLabel}, {"model", turn.AggregatorModel}, {"provider", turn.AggregatorProvider},
		{"temperature", turn.AggregatorTemperature}, {"input_messages", turn.AggregatorInputMessages}, {"output", output},
		{"streamed", turn.AggregatorStreamed}, {"output_location", location}} {
		aggregator.Set(kv.k, kv.v)
	}
	record := jsonutil.NewObject()
	record.Set("ts", float64(time.Now().UnixNano())/1e9)
	record.Set("session_id", sessionID)
	record.Set("preset", turn.PresetName)
	record.Set("references", refs)
	record.Set("aggregator", aggregator)
	line, err := jsonutil.Encode(record, jsonutil.Convert(func(v any) (any, error) { return anyutil.String(v), nil }), jsonutil.Spaced())
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
