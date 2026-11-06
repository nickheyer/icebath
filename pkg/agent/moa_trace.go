package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

var (
	moaUsageFields = []string{"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens"}
	moaAcctFields  = []string{"model", "provider", "temperature"}
	moaCostFields  = []string{"cost_usd", "cost_status", "cost_source"}
)

// Reads the moa config section per call, swapped by tests
var loadMoAConfig = func(ctx context.Context) (map[string]any, error) {
	cfg, err := config.Load(home.Dir(ctx), os.LookupEnv)
	if err != nil {
		return nil, err
	}
	switch section := cfg["moa"].(type) {
	case map[string]any:
		return section, nil
	case config.Tree:
		return map[string]any(section), nil
	}
	return map[string]any{}, nil
}

// Trace directory when moa.save_traces is on, moa.trace_dir overriding the home default
func moaTracesEnabledAndDir(ctx context.Context) (string, bool) {
	moaCfg, err := loadMoAConfig(ctx)
	if err != nil || !pyrepr.Truthy(moaCfg["save_traces"]) {
		return "", false
	}
	if override := moaCfg["trace_dir"]; pyrepr.Truthy(override) {
		return pypath.Expandvars(pypath.Expanduser(pyrepr.Str(override))), true
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
		if pystr.IsAlnum(c) || strings.ContainsRune("-_.", c) {
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
func moaSlotTrace(acct any, label string) *pyjson.Object {
	trace := pyjson.NewObject()
	trace.Set("label", label)
	for _, f := range moaAcctFields {
		trace.Set(f, moaAttr(acct, f, nil))
	}
	trace.Set("input_messages", moaAttr(acct, "messages", nil))
	trace.Set("output", moaAttr(acct, "output", nil))
	usage := pyjson.NewObject()
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
func SlotMetrics(acct any, label string, output any) *pyjson.Object {
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
		slog.Debug(fmt.Sprintf("MoA trace write failed (session=%s): %s", pyrepr.Str(sessionID), err))
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
	aggregator := pyjson.NewObject()
	for _, kv := range []struct {
		k string
		v any
	}{{"label", turn.AggregatorLabel}, {"model", turn.AggregatorModel}, {"provider", turn.AggregatorProvider},
		{"temperature", turn.AggregatorTemperature}, {"input_messages", turn.AggregatorInputMessages}, {"output", output},
		{"streamed", turn.AggregatorStreamed}, {"output_location", location}} {
		aggregator.Set(kv.k, kv.v)
	}
	record := pyjson.NewObject()
	record.Set("ts", float64(time.Now().UnixNano())/1e9)
	record.Set("session_id", sessionID)
	record.Set("preset", turn.PresetName)
	record.Set("references", refs)
	record.Set("aggregator", aggregator)
	line, err := pyjson.Dumps(record, pyjson.EnsureASCII(false), pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil }))
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
