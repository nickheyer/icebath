package nemorelay

import (
	"encoding/hex"
	"math/big"
	"strings"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"

	"github.com/nickheyer/icebath/pkg/nemorelay/logging"
)

const (
	w3cMaxVersion   = 254
	traceFlagSample = 0x01
)

// Derives the deterministic Relay trace id from all sixteen uuid bytes
func RelayTraceID(id uuid.UUID) trace.TraceID {
	return trace.TraceID(id)
}

// Derives the deterministic Relay span id from the last eight uuid bytes
func RelaySpanID(id uuid.UUID) trace.SpanID {
	var span trace.SpanID
	copy(span[:], id[8:])
	return span
}

// Formats a sampled W3C traceparent from Relay trace and span ids
func FormatTraceparent(traceUUID, spanUUID uuid.UUID) string {
	return "00-" + hex.EncodeToString(traceUUID[:]) + "-" + hex.EncodeToString(spanUUID[8:]) + "-01"
}

// Reports whether the uuid has a nonzero span half usable as a Relay identifier
func isUsableRelayIdentifier(id uuid.UUID) bool {
	for _, b := range id[8:] {
		if b != 0 {
			return true
		}
	}
	return false
}

// W3C span context carried between Relay spans with its canonical tracestate header
type spanContext struct {
	traceID    trace.TraceID
	spanID     trace.SpanID
	flags      byte
	remote     bool
	traceState string
}

func (c spanContext) valid() bool {
	return c.traceID.IsValid() && c.spanID.IsValid()
}

// Injects the context as canonical traceparent and nonempty tracestate headers
func (c spanContext) headers() (*string, *string) {
	if !c.valid() {
		return nil, nil
	}
	flags := "00"
	if c.flags&traceFlagSample != 0 {
		flags = "01"
	}
	traceparent := "00-" + hex.EncodeToString(c.traceID[:]) + "-" + hex.EncodeToString(c.spanID[:]) + "-" + flags
	if c.traceState == "" {
		return &traceparent, nil
	}
	state := c.traceState
	return &traceparent, &state
}

// Converts to an OpenTelemetry span context for exporters
func (c spanContext) otel() trace.SpanContext {
	state, _ := trace.ParseTraceState(c.traceState)
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    c.traceID,
		SpanID:     c.spanID,
		TraceFlags: trace.TraceFlags(c.flags),
		TraceState: state,
		Remote:     c.remote,
	})
}

// Normalized W3C trace context that always holds a valid traceparent
type W3CTraceContext struct {
	traceparent string
	tracestate  *string
}

// Validates and canonicalizes a traceparent and optional tracestate
func NewW3CTraceContext(traceparent string, tracestate *string) (*W3CTraceContext, error) {
	normalized, state, _ := normalizeW3CHeaders(&traceparent, tracestate)
	if normalized == nil {
		return nil, invalidArgumentError("invalid W3C trace context")
	}
	return &W3CTraceContext{traceparent: *normalized, tracestate: state}, nil
}

func (c *W3CTraceContext) Traceparent() string {
	return c.traceparent
}

func (c *W3CTraceContext) Tracestate() *string {
	return cloneStringPtr(c.tracestate)
}

func (c *W3CTraceContext) spanContext() (spanContext, bool) {
	return w3cSpanContext(&c.traceparent, c.tracestate)
}

// Returns the OpenTelemetry span context of valid W3C headers
func W3CSpanContext(traceparent, tracestate *string) (trace.SpanContext, bool) {
	context, ok := w3cSpanContext(traceparent, tracestate)
	if !ok {
		return trace.SpanContext{}, false
	}
	return context.otel(), true
}

func w3cSpanContext(traceparent, tracestate *string) (spanContext, bool) {
	_, _, context := normalizeW3CHeaders(traceparent, tracestate)
	if context == nil {
		return spanContext{}, false
	}
	return *context, true
}

// Canonicalizes W3C headers and drops invalid ones with a warning
func normalizeW3CHeaders(traceparent, tracestate *string) (*string, *string, *spanContext) {
	if traceparent == nil {
		if tracestate != nil {
			logging.Warn("nemo_relay.runtime", "Ignoring tracestate without traceparent", map[string]any{"event": "invalid_w3c_trace_context"})
		}
		return nil, nil, nil
	}
	carrierState := ""
	hasState := false
	if tracestate != nil {
		if header, ok := parseW3CTracestate(*tracestate); ok {
			carrierState = header
			hasState = true
		} else {
			logging.Warn("nemo_relay.runtime", "Ignoring invalid W3C tracestate", map[string]any{"event": "invalid_w3c_trace_context"})
		}
	}
	context, ok := extractTraceparent(*traceparent, carrierState, hasState)
	if !ok {
		logging.Warn("nemo_relay.runtime", "Ignoring invalid W3C trace context", map[string]any{"event": "invalid_w3c_trace_context"})
		return nil, nil, nil
	}
	normalized, state := context.headers()
	return normalized, state, &context
}

// Validates a tracestate header and returns its canonical member list
func parseW3CTracestate(header string) (string, bool) {
	members := make([]string, 0)
	keys := map[string]struct{}{}
	for index, member := range strings.Split(header, ",") {
		if index >= 32 {
			return "", false
		}
		member = strings.Trim(member, " \t")
		if member == "" {
			continue
		}
		key, value, found := strings.Cut(member, "=")
		if !found {
			return "", false
		}
		if !validRelayTracestateKey(key) {
			return "", false
		}
		if _, duplicate := keys[key]; duplicate {
			return "", false
		}
		keys[key] = struct{}{}
		if len(value) < 1 || len(value) > 256 {
			return "", false
		}
		for i := 0; i < len(value); i++ {
			b := value[i]
			if b < 0x20 || b > 0x7e || b == ',' || b == '=' {
				return "", false
			}
		}
		if !otelTracestateKeyValid(key) || !otelTracestateValueValid(value) {
			return "", false
		}
		members = append(members, key+"="+value)
	}
	return strings.Join(members, ","), true
}

func tracestateNameChars(name string) bool {
	for i := 0; i < len(name); i++ {
		b := name[i]
		if !(b >= 'a' && b <= 'z') && !(b >= '0' && b <= '9') && b != '_' && b != '-' && b != '*' && b != '/' {
			return false
		}
	}
	return true
}

func validRelayTracestateKey(key string) bool {
	if tenant, system, found := strings.Cut(key, "@"); found {
		return len(tenant) >= 1 && len(tenant) <= 241 &&
			isASCIIAlphanumeric(tenant[0]) && tracestateNameChars(tenant) &&
			len(system) >= 1 && len(system) <= 14 &&
			system[0] >= 'a' && system[0] <= 'z' && tracestateNameChars(system)
	}
	return len(key) >= 1 && len(key) <= 256 && key[0] >= 'a' && key[0] <= 'z' && tracestateNameChars(key)
}

// Applies the OpenTelemetry trace state key rules the Relay tracestate must also satisfy
func otelTracestateKeyValid(key string) bool {
	if len(key) > 256 {
		return false
	}
	vendorStart := -1
	for i := 0; i < len(key); i++ {
		b := key[i]
		lowerOrDigit := (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
		if !lowerOrDigit && b != '_' && b != '-' && b != '*' && b != '/' && b != '@' {
			return false
		}
		switch {
		case i == 0 && !lowerOrDigit:
			return false
		case b == '@':
			if vendorStart >= 0 || i+14 < len(key) {
				return false
			}
			vendorStart = i
		case vendorStart >= 0 && i == vendorStart+1 && !lowerOrDigit:
			return false
		}
	}
	return true
}

func otelTracestateValueValid(value string) bool {
	return len(value) <= 256 && !strings.ContainsAny(value, ",=")
}

func isASCIIAlphanumeric(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// Splits like Rust split_terminator, dropping one trailing empty piece
func splitTerminator(value string, separator string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, separator)
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// Parses an unsigned hexadecimal integer the way Rust from_str_radix does
func parseRustHex(text string, bits int) (*big.Int, bool) {
	if strings.HasPrefix(text, "+") {
		text = text[1:]
	}
	if text == "" {
		return nil, false
	}
	for i := 0; i < len(text); i++ {
		if _, ok := hexDigit(text[i]); !ok {
			return nil, false
		}
	}
	value, ok := new(big.Int).SetString(text, 16)
	if !ok || value.BitLen() > bits {
		return nil, false
	}
	return value, true
}

func hexDigit(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	}
	return 0, false
}

// Extracts a remote span context with the OpenTelemetry trace context propagator rules
func extractTraceparent(header string, tracestate string, hasState bool) (spanContext, bool) {
	parts := splitTerminator(strings.TrimSpace(header), "-")
	if len(parts) < 4 {
		return spanContext{}, false
	}
	version, ok := parseRustHex(parts[0], 8)
	if !ok {
		return spanContext{}, false
	}
	if version.Int64() > w3cMaxVersion || (version.Int64() == 0 && len(parts) != 4) {
		return spanContext{}, false
	}
	if strings.ContainsFunc(parts[1], isASCIIUpper) {
		return spanContext{}, false
	}
	traceValue, ok := parseRustHex(parts[1], 128)
	if !ok {
		return spanContext{}, false
	}
	if strings.ContainsFunc(parts[2], isASCIIUpper) {
		return spanContext{}, false
	}
	spanValue, ok := parseRustHex(parts[2], 64)
	if !ok {
		return spanContext{}, false
	}
	options, ok := parseRustHex(parts[3], 8)
	if !ok {
		return spanContext{}, false
	}
	if version.Int64() == 0 && options.Int64() > 2 {
		return spanContext{}, false
	}
	context := spanContext{flags: byte(options.Int64()) & traceFlagSample, remote: true}
	traceValue.FillBytes(context.traceID[:])
	spanValue.FillBytes(context.spanID[:])
	if hasState {
		if state, ok := otelTraceStateFromString(tracestate); ok {
			context.traceState = state
		}
	}
	if !context.valid() {
		return spanContext{}, false
	}
	return context, true
}

func isASCIIUpper(r rune) bool {
	return r >= 'A' && r <= 'Z'
}

// Parses a tracestate header the way the OpenTelemetry trace state parser does
func otelTraceStateFromString(header string) (string, bool) {
	members := make([]string, 0)
	for _, member := range splitTerminator(header, ",") {
		separator := strings.IndexByte(member, '=')
		if separator < 0 {
			return "", false
		}
		key := member[:separator]
		value := strings.TrimLeft(member[separator:], "=")
		if !otelTracestateKeyValid(key) || !otelTracestateValueValid(value) {
			return "", false
		}
		members = append(members, key+"="+value)
	}
	return strings.Join(members, ","), true
}

// Builds the child headers for a Relay span under a parent span context
func w3cHeadersForParent(parent spanContext, spanUUID uuid.UUID) (*string, *string) {
	child := spanContext{
		traceID:    parent.traceID,
		spanID:     RelaySpanID(spanUUID),
		flags:      parent.flags,
		remote:     true,
		traceState: parent.traceState,
	}
	return child.headers()
}

func cloneStringPtr(value *string) *string {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}
