package langfuse

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	sdkVersion     = "4.15.2"
	tracerName     = "langfuse-sdk"
	defaultBaseURL = "https://cloud.langfuse.com"
	serializeDepth = 20

	attrTraceName        = "langfuse.trace.name"
	attrTraceUserID      = "user.id"
	attrTraceSessionID   = "session.id"
	attrTraceTags        = "langfuse.trace.tags"
	attrTraceMetadata    = "langfuse.trace.metadata"
	attrTraceInput       = "langfuse.trace.input"
	attrTraceOutput      = "langfuse.trace.output"
	attrObsType          = "langfuse.observation.type"
	attrObsMetadata      = "langfuse.observation.metadata"
	attrObsLevel         = "langfuse.observation.level"
	attrObsStatusMessage = "langfuse.observation.status_message"
	attrObsInput         = "langfuse.observation.input"
	attrObsOutput        = "langfuse.observation.output"
	attrObsModel         = "langfuse.observation.model.name"
	attrObsModelParams   = "langfuse.observation.model.parameters"
	attrObsUsage         = "langfuse.observation.usage_details"
	attrObsCost          = "langfuse.observation.cost_details"
	attrEnvironment      = "langfuse.environment"
	attrRelease          = "langfuse.release"
	attrVersion          = "langfuse.version"
	attrAsRoot           = "langfuse.internal.as_root"
	attrIsAppRoot        = "langfuse.internal.is_app_root"
)

var (
	generationLikeTypes = map[string]bool{"generation": true, "embedding": true}
	spanLikeTypes       = map[string]bool{"span": true, "agent": true, "tool": true, "chain": true, "retriever": true, "evaluator": true, "guardrail": true}
	commonReleaseEnvs   = []string{"RENDER_GIT_COMMIT", "CI_COMMIT_SHA", "CIRCLE_SHA1", "SOURCE_VERSION", "TRAVIS_COMMIT",
		"GIT_COMMIT", "GITHUB_SHA", "BITBUCKET_COMMIT", "BUILD_SOURCEVERSION", "DRONE_COMMIT_SHA"}
	errExportURL = errors.New("langfuse: the OTLP traces endpoint is not an http or https URL")
)

// SDK debug line, visible at info when LANGFUSE_DEBUG is true
func sdkDebug(message string) {
	if strings.ToLower(os.Getenv("LANGFUSE_DEBUG")) == "true" {
		pkgLog().Info(message)
		return
	}
	pkgLog().Debug(message)
}

// Python json.dumps through the Langfuse event serializer, a string passed through untouched and nil absent
func serializeAttr(v any) (any, bool) {
	if v == nil {
		return nil, false
	}
	if s, ok := v.(string); ok {
		return s, true
	}
	return eventJSON(v), true
}

// JSON text of a value as the Langfuse event serializer writes it
func eventJSON(v any) string {
	out, err := pyjson.Dumps(eventDefault(v, 1))
	if err != nil {
		return fmt.Sprintf(`"<not serializable object of type: %s>"`, pyrepr.TypeName(v))
	}
	return out
}

// Serializer conversion of one value, media to its reference, unsafe floats and ints to strings, deep containers to type names
func eventDefault(v any, depth int) any {
	switch t := v.(type) {
	case nil, bool, string:
		return v
	case *lfMedia:
		return t.serialized()
	case time.Time:
		return isoformatUTC(t)
	case float64:
		return safeFloat(t)
	case float32:
		return safeFloat(float64(t))
	case []byte:
		if utf8.Valid(t) {
			return string(t)
		}
		return "<not serializable bytes>"
	case json.Number:
		if n, ok := new(big.Int).SetString(t.String(), 10); ok {
			return jsSafeInt(n, t)
		}
		if f, err := t.Float64(); err == nil {
			return safeFloat(f)
		}
		return t.String()
	case *big.Int:
		return jsSafeInt(t, t)
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		n, _ := new(big.Int).SetString(fmt.Sprint(t), 10)
		return jsSafeInt(n, t)
	}
	if depth >= serializeDepth {
		return "<" + pyrepr.TypeName(v) + ">"
	}
	if items, ok := dictItems(v); ok {
		out := pyjson.NewObject()
		for _, it := range items {
			out.Set(it.key, eventDefault(it.val, depth+1))
		}
		return out
	}
	if elems, ok := listItems(v); ok {
		out := make([]any, len(elems))
		for i, e := range elems {
			out[i] = eventDefault(e, depth+1)
		}
		return out
	}
	if vars, ok := structVars(v); ok {
		items, _ := dictItems(vars)
		out := pyjson.NewObject()
		for _, it := range items {
			out.Set(it.key, eventDefault(it.val, depth+1))
		}
		return out
	}
	return "<" + pyrepr.TypeName(v) + ">"
}

// Float or its NaN and Infinity spelling
func safeFloat(f float64) any {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	return f
}

// Integer kept when JavaScript can hold it exactly, else its decimal text
func jsSafeInt(n *big.Int, original any) any {
	limit := big.NewInt(1<<53 - 1)
	if n.CmpAbs(limit) <= 0 {
		return original
	}
	return n.String()
}

// Python isoformat of an instant with a Z suffix for UTC
func isoformatUTC(t time.Time) string {
	layout := "2006-01-02T15:04:05"
	if t.Nanosecond()/1000 != 0 {
		layout += ".000000"
	}
	s := t.Format(layout + "-07:00")
	return strings.Replace(s, "+00:00", "Z", 1)
}

// One span attribute candidate, a nil value meaning absent
type attrEntry struct {
	key string
	val any
}

// Observation metadata flattened one level into prefixed attributes, strings and ints raw, the rest serialized
func flattenMetadata(metadata any, prefix string) []attrEntry {
	items, ok := dictItems(metadata)
	if !ok {
		v, _ := serializeAttr(metadata)
		return []attrEntry{{prefix, v}}
	}
	out := make([]attrEntry, 0, len(items))
	for _, it := range items {
		if _, isStr := it.val.(string); isStr || isStrictInt(it.val) {
			out = append(out, attrEntry{prefix + "." + it.key, it.val})
			continue
		}
		v, _ := serializeAttr(it.val)
		out = append(out, attrEntry{prefix + "." + it.key, v})
	}
	return out
}

// Reports a Python int, bool included, without widening floats
func isStrictInt(v any) bool {
	switch t := v.(type) {
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, *big.Int:
		return true
	case json.Number:
		_, ok := new(big.Int).SetString(t.String(), 10)
		return ok
	}
	return false
}

// Field values one create or update call carries, nil and empty strings meaning not given
type obsFields struct {
	input, output, metadata any
	level, statusMessage    string
	model, modelParameters  any
	usageDetails            any
	costDetails             any
}

// Nil for an empty optional string
func optStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Serialized value or nil
func serialized(v any) any {
	out, _ := serializeAttr(v)
	return out
}

// Span attributes create_span_attributes builds
func spanAttributes(f obsFields, obsType any) []attrEntry {
	out := []attrEntry{{attrObsType, obsType}, {attrObsLevel, optStr(f.level)}, {attrObsStatusMessage, optStr(f.statusMessage)},
		{attrVersion, nil}, {attrObsInput, serialized(f.input)}, {attrObsOutput, serialized(f.output)}}
	return dropNil(append(out, flattenMetadata(f.metadata, attrObsMetadata)...))
}

// Span attributes create_generation_attributes builds
func generationAttributes(f obsFields, obsType string) []attrEntry {
	out := []attrEntry{{attrObsType, obsType}, {attrObsLevel, optStr(f.level)}, {attrObsStatusMessage, optStr(f.statusMessage)},
		{attrVersion, nil}, {attrObsInput, serialized(f.input)}, {attrObsOutput, serialized(f.output)},
		{attrObsModel, f.model}, {attrObsUsage, serialized(f.usageDetails)}, {attrObsCost, serialized(f.costDetails)},
		{attrObsModelParams, serialized(f.modelParameters)}}
	return dropNil(append(out, flattenMetadata(f.metadata, attrObsMetadata)...))
}

// Entries with a value
func dropNil(entries []attrEntry) []attrEntry {
	out := entries[:0]
	for _, e := range entries {
		if e.val != nil {
			out = append(out, e)
		}
	}
	return out
}

// OpenTelemetry attribute of a value, false with a warning for a type an attribute cannot hold
func toAttr(key string, v any) (attribute.KeyValue, bool) {
	switch t := v.(type) {
	case string:
		return attribute.String(key, t), true
	case bool:
		return attribute.Bool(key, t), true
	case float64:
		return attribute.Float64(key, t), true
	case float32:
		return attribute.Float64(key, float64(t)), true
	case json.Number:
		if !isStrictInt(t) {
			if f, err := t.Float64(); err == nil {
				return attribute.Float64(key, f), true
			}
		}
	case []string:
		return attribute.StringSlice(key, t), true
	case []byte:
		return attribute.String(key, string(t)), true
	}
	if isStrictInt(v) {
		if n, ok := pyIntValue(v); ok {
			switch t := n.(type) {
			case bool:
				return attribute.Bool(key, t), true
			case *big.Int:
				if t.IsInt64() {
					return attribute.Int64(key, t.Int64()), true
				}
			default:
				if b, ok := new(big.Int).SetString(fmt.Sprint(t), 10); ok && b.IsInt64() {
					return attribute.Int64(key, b.Int64()), true
				}
			}
		}
	}
	if elems, ok := listItems(v); ok {
		strs := make([]string, 0, len(elems))
		for _, e := range elems {
			s, isStr := e.(string)
			if !isStr {
				strs = nil
				break
			}
			strs = append(strs, s)
		}
		if strs != nil || len(elems) == 0 {
			return attribute.StringSlice(key, strs), true
		}
	}
	pkgLog().Warn(fmt.Sprintf("Invalid type %s for attribute '%s' value. Expected one of ['bool', 'str', 'bytes', 'int', 'float'] or a sequence of those types",
		pyrepr.TypeName(v), key))
	return attribute.KeyValue{}, false
}

// Sets attribute entries on a span in order, skipping values an attribute cannot hold
func setAttrs(span trace.Span, entries []attrEntry) {
	kvs := make([]attribute.KeyValue, 0, len(entries))
	for _, e := range entries {
		if e.val == nil {
			continue
		}
		if kv, ok := toAttr(e.key, e.val); ok {
			kvs = append(kvs, kv)
		}
	}
	if len(kvs) > 0 {
		span.SetAttributes(kvs...)
	}
}

// Python len of a string in code points
func strLen(s string) int { return utf8.RuneCountInString(s) }

// Reports a propagated string within the 200 character limit, warning when it is not
func validPropagatedString(value any, key string) bool {
	s, ok := value.(string)
	if !ok {
		pkgLog().Warn(fmt.Sprintf("Propagated attribute '%s' value is not a string. Dropping value.", key))
		return false
	}
	if n := strLen(s); n > 200 {
		pkgLog().Warn(fmt.Sprintf("Propagated attribute '%s' value is over 200 characters (%d chars). Dropping value.", key, n))
		return false
	}
	return true
}

// Validated propagated value, list entries filtered, nil when nothing survives
func validatePropagated(value any, key string) any {
	if elems, ok := listItems(value); ok {
		kept := []string{}
		for _, e := range elems {
			if validPropagatedString(e, key) {
				kept = append(kept, e.(string))
			}
		}
		if len(kept) == 0 {
			return nil
		}
		return kept
	}
	if !validPropagatedString(value, key) {
		return nil
	}
	return value
}

// Span attributes propagate_attributes stamps on observations opened under the propagated trace attributes
func propagatedAttrs(p *pyjson.Object) []attrEntry {
	if p == nil {
		return nil
	}
	validated := map[string]any{}
	for _, key := range []string{"user_id", "session_id", "version", "tags", "trace_name"} {
		if v, ok := p.Get(key); ok && v != nil {
			if vv := validatePropagated(v, key); vv != nil {
				validated[key] = vv
			}
		}
	}
	var meta []attrEntry
	if md, ok := p.Get("metadata"); ok && md != nil {
		items, _ := dictItems(md)
		for _, it := range items {
			coerced, isStr := it.val.(string)
			if !isStr {
				coerced = pyStr(it.val)
			}
			if validPropagatedString(coerced, "metadata."+it.key) {
				meta = append(meta, attrEntry{attrTraceMetadata + "." + it.key, coerced})
			}
		}
	}
	out := []attrEntry{{attrTraceUserID, validated["user_id"]}, {attrTraceSessionID, validated["session_id"]}}
	out = append(out, meta...)
	out = append(out, attrEntry{attrVersion, validated["version"]}, attrEntry{attrTraceTags, validated["tags"]},
		attrEntry{attrTraceName, validated["trace_name"]})
	return dropNil(out)
}

// Settings one Langfuse client is constructed with
type clientOptions struct {
	publicKey, secretKey, baseURL string
	environment, release          string
	sampleRate                    *float64
	defaultService                bool
}

// One Langfuse client exporting OpenTelemetry spans to its project and uploading media
type lfClient struct {
	publicKey   string
	secretKey   string
	baseURL     string
	environment *string
	release     *string
	timeout     time.Duration
	provider    *sdktrace.TracerProvider
	tracer      trace.Tracer
	media       *mediaManager
	shutMu      sync.Mutex
	shut        bool
}

// Langfuse client built from options and the SDK environment, an error where the SDK constructor raises
func newClient(o clientOptions) (*lfClient, error) {
	var environment, release *string
	if o.environment != "" {
		environment = &o.environment
	} else if v, ok := os.LookupEnv("LANGFUSE_TRACING_ENVIRONMENT"); ok {
		environment = &v
	}
	if o.release != "" {
		release = &o.release
	} else if v := os.Getenv("LANGFUSE_RELEASE"); v != "" {
		release = &v
	} else {
		for _, name := range commonReleaseEnvs {
			if v, ok := os.LookupEnv(name); ok {
				release = &v
				break
			}
		}
	}
	sampleRate := 1.0
	if o.sampleRate != nil {
		sampleRate = *o.sampleRate
	} else if text, ok := os.LookupEnv("LANGFUSE_SAMPLE_RATE"); ok {
		f, err := pyint.ParseFloat(text)
		if err != nil {
			return nil, err
		}
		sampleRate = f
	}
	if !(sampleRate >= 0 && sampleRate <= 1) {
		return nil, fmt.Errorf("Sample rate must be between 0.0 and 1.0, got %s", pyrepr.Float(sampleRate))
	}
	timeoutSecs := 5
	if text, ok := os.LookupEnv("LANGFUSE_TIMEOUT"); ok {
		n, ok := pyint.Parse(text)
		if !ok {
			return nil, &pyint.Error{Kind: "ValueError", Msg: "invalid literal for int() with base 10: " + pyrepr.StrRepr(text)}
		}
		timeoutSecs = clampInt(n)
	}
	tracingEnabled := strings.ToLower(envOr("LANGFUSE_TRACING_ENABLED", "true")) != "false"
	if !tracingEnabled {
		pkgLog().Info("Configuration: Langfuse tracing is explicitly disabled. No data will be sent to the Langfuse API.")
	}
	sdkDisabled := strings.ToLower(envOr("OTEL_SDK_DISABLED", "false")) == "true"
	if sdkDisabled {
		pkgLog().Warn("OTEL_SDK_DISABLED is set. Langfuse tracing will be disabled and no traces will appear in the UI.")
	}
	c := &lfClient{publicKey: o.publicKey, secretKey: o.secretKey, baseURL: o.baseURL, environment: environment,
		release: release, timeout: time.Duration(timeoutSecs) * time.Second}
	media, err := newMediaManager(c)
	if err != nil {
		return nil, err
	}
	c.media = media
	c.tracer = noop.NewTracerProvider().Tracer(tracerName)
	if tracingEnabled {
		provider, err := c.buildProvider(sampleRate, o.defaultService)
		if err != nil {
			media.stop()
			return nil, err
		}
		c.provider = provider
		if !sdkDisabled {
			c.tracer = provider.Tracer(tracerName, trace.WithInstrumentationVersion(sdkVersion),
				trace.WithInstrumentationAttributes(attribute.String("public_key", o.publicKey)))
		}
	}
	media.start()
	return c, nil
}

// Process env value or the default when unset
func envOr(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

// Tracer provider with the Langfuse resource, sampler and batch processor feeding the OTLP HTTP exporter
func (c *lfClient) buildProvider(sampleRate float64, defaultService bool) (*sdktrace.TracerProvider, error) {
	var flushAt, flushIntervalMs *int
	if text, ok := os.LookupEnv("LANGFUSE_FLUSH_AT"); ok {
		n, ok := pyint.Parse(text)
		if !ok {
			return nil, &pyint.Error{Kind: "ValueError", Msg: "invalid literal for int() with base 10: " + pyrepr.StrRepr(text)}
		}
		v := clampInt(n)
		flushAt = &v
	}
	if text, ok := os.LookupEnv("LANGFUSE_FLUSH_INTERVAL"); ok {
		f, err := pyint.ParseFloat(text)
		if err != nil {
			return nil, err
		}
		v := int(f * 1000)
		flushIntervalMs = &v
	}
	exporter, err := c.buildExporter()
	if err != nil {
		return nil, err
	}
	var bspOpts []sdktrace.BatchSpanProcessorOption
	if c.timeout != 0 {
		bspOpts = append(bspOpts, sdktrace.WithExportTimeout(c.timeout))
	}
	if flushAt != nil {
		bspOpts = append(bspOpts, sdktrace.WithMaxExportBatchSize(*flushAt))
	}
	if flushIntervalMs != nil {
		bspOpts = append(bspOpts, sdktrace.WithBatchTimeout(time.Duration(*flushIntervalMs)*time.Millisecond))
	}
	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(c.buildResource(defaultService)),
		sdktrace.WithSpanProcessor(sdktrace.NewBatchSpanProcessor(&mediaExporter{next: exporter, media: c.media}, bspOpts...)),
	}
	if sampleRate < 1 {
		opts = append(opts, sdktrace.WithSampler(sdktrace.TraceIDRatioBased(sampleRate)))
	}
	return sdktrace.NewTracerProvider(opts...), nil
}

// OTLP HTTP protobuf exporter posting to the Langfuse traces endpoint with the SDK headers
func (c *lfClient) buildExporter() (sdktrace.SpanExporter, error) {
	endpoint := c.baseURL + "/api/public/otel/v1/traces"
	if path := os.Getenv("LANGFUSE_OTEL_TRACES_EXPORT_PATH"); path != "" {
		endpoint = c.baseURL + "/" + path
	}
	headers := map[string]string{
		"Authorization":          "Basic " + base64.StdEncoding.EncodeToString([]byte(c.publicKey+":"+c.secretKey)),
		"x-langfuse-sdk-name":    "python",
		"x-langfuse-sdk-version": sdkVersion,
		"x-langfuse-public-key":  c.publicKey,
	}
	opts := []otlptracehttp.Option{otlptracehttp.WithHeaders(headers)}
	if c.timeout != 0 {
		opts = append(opts, otlptracehttp.WithTimeout(c.timeout),
			otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: true, InitialInterval: time.Second, MaxInterval: 32 * time.Second, MaxElapsedTime: c.timeout}))
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		cause := errExportURL
		if err != nil {
			cause = err
		}
		opts = append(opts, otlptracehttp.WithHTTPClient(&http.Client{Transport: failingTransport{cause}}))
	} else {
		opts = append(opts, otlptracehttp.WithEndpointURL(endpoint))
	}
	return otlptracehttp.New(context.Background(), opts...)
}

// Transport that fails every request with the cause the endpoint could not be used
type failingTransport struct{ cause error }

func (t failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, t.cause }

// Resource with the SDK telemetry, the env attributes, the default service name and the Langfuse environment and release
func (c *lfClient) buildResource(defaultService bool) *resource.Resource {
	opts := []resource.Option{resource.WithTelemetrySDK(), resource.WithFromEnv()}
	if defaultService {
		opts = append(opts, resource.WithAttributes(attribute.String("service.name", "icebath-agent")))
	}
	var lf []attribute.KeyValue
	if c.environment != nil {
		lf = append(lf, attribute.String(attrEnvironment, *c.environment))
	}
	if c.release != nil {
		lf = append(lf, attribute.String(attrRelease, *c.release))
	}
	opts = append(opts, resource.WithAttributes(lf...))
	res, err := resource.New(context.Background(), opts...)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("Invalid key value resource attribute pair in OTEL_RESOURCE_ATTRIBUTES: %v", err))
	}
	if res == nil {
		res = resource.Empty()
	}
	if _, ok := res.Set().Value("service.name"); !ok {
		merged, mergeErr := resource.Merge(res, resource.NewSchemaless(attribute.String("service.name", "unknown_service")))
		if mergeErr == nil {
			res = merged
		}
	}
	return res
}

// Deterministic 32 hex trace id from a seed, random without one
func createTraceID(seed string) string {
	if seed == "" {
		var b [16]byte
		_, _ = rand.Read(b[:])
		return hex.EncodeToString(b[:])
	}
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:16])
}

// Exports every ended span and waits for queued media uploads
func (c *lfClient) flush() {
	if c.provider != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := c.provider.ForceFlush(ctx); err != nil {
			sdkDebug(fmt.Sprintf("OTEL tracer provider flush failed: %v", err))
		}
		cancel()
		sdkDebug("Successfully flushed OTEL tracer provider")
	}
	c.media.join()
	sdkDebug("Successfully flushed media upload queue")
}

// Flushes once and stops the media upload workers, the tracer provider kept exporting until process exit
func (c *lfClient) shutdown() {
	c.shutMu.Lock()
	if c.shut {
		c.shutMu.Unlock()
		return
	}
	c.shut = true
	c.shutMu.Unlock()
	c.flush()
	c.media.stop()
}

// Shuts the tracer provider down at process exit, draining the batch processor
func (c *lfClient) shutdownProvider() {
	if c.provider == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.provider.Shutdown(ctx); err != nil {
		sdkDebug(fmt.Sprintf("OTEL tracer provider shutdown failed: %v", err))
	}
}

// One Langfuse observation wrapping its OpenTelemetry span
type observation struct {
	client  *lfClient
	span    trace.Span
	asType  string
	traceID string
	id      string
}

// Root observation of a trace id under a remote parent, stamped with the propagated trace attributes
func (c *lfClient) startRoot(traceID, name, asType string, input, metadata any, propagated *pyjson.Object) *observation {
	if !isHex(traceID, 32) {
		pkgLog().Warn(fmt.Sprintf("Passed trace ID '%s' is not a valid 32 lowercase hex char Langfuse trace id. Ignoring trace ID.", traceID))
	}
	tid, err := trace.TraceIDFromHex(traceID)
	if err != nil {
		panic(fmt.Sprintf("Langfuse trace id %s is not hexadecimal", traceID))
	}
	var sid trace.SpanID
	_, _ = rand.Read(sid[:])
	parent := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})
	_, span := c.tracer.Start(trace.ContextWithSpanContext(context.Background(), parent), name)
	if span.IsRecording() {
		setAttrs(span, propagatedAttrs(propagated))
		setAttrs(span, []attrEntry{{attrIsAppRoot, true}, {attrAsRoot, true}})
	}
	return c.wrap(span, asType, obsFields{input: input, metadata: metadata})
}

// Child observation under parent, stamped with the propagated trace attributes
func (p *observation) startChild(name, asType string, f obsFields, propagated *pyjson.Object) *observation {
	if !generationLikeTypes[asType] && !spanLikeTypes[asType] {
		pkgLog().Warn(fmt.Sprintf("Unknown observation type: %s, falling back to LangfuseSpan", asType))
		asType = "span"
	}
	_, span := p.client.tracer.Start(trace.ContextWithSpan(context.Background(), p.span), name)
	if span.IsRecording() {
		setAttrs(span, propagatedAttrs(propagated))
	}
	if !generationLikeTypes[asType] {
		f.model, f.modelParameters, f.usageDetails, f.costDetails = nil, nil, nil, nil
	}
	return p.client.wrap(span, asType, f)
}

// Observation over a started span with its type, the client environment and release and initial fields applied
func (c *lfClient) wrap(span trace.Span, asType string, f obsFields) *observation {
	sc := span.SpanContext()
	o := &observation{client: c, span: span, asType: asType, traceID: sc.TraceID().String(), id: sc.SpanID().String()}
	span.SetAttributes(attribute.String(attrObsType, asType))
	if c.environment != nil {
		span.SetAttributes(attribute.String(attrEnvironment, *c.environment))
	}
	if c.release != nil {
		span.SetAttributes(attribute.String(attrRelease, *c.release))
	}
	if !span.IsRecording() {
		return o
	}
	f.input = o.processMedia(f.input, "input")
	f.output = o.processMedia(f.output, "output")
	f.metadata = o.processMedia(f.metadata, "metadata")
	entries := o.attributes(f)
	setAttrs(span, entries[1:])
	o.statusIfError(f.level, f.statusMessage)
	return o
}

// Generation or span attributes of fields for this observation type, the type entry first
func (o *observation) attributes(f obsFields) []attrEntry {
	if generationLikeTypes[o.asType] {
		return generationAttributes(f, o.asType)
	}
	var obsType any
	if spanLikeTypes[o.asType] || o.asType == "event" {
		obsType = o.asType
	}
	entries := spanAttributes(f, obsType)
	if obsType == nil {
		return append([]attrEntry{{attrObsType, nil}}, entries...)
	}
	return entries
}

// Media in a field value replaced by Langfuse media references with their uploads queued
func (o *observation) processMedia(v any, field string) any {
	if v == nil {
		return nil
	}
	out, _ := o.client.media.findAndProcess(v, o.traceID, o.id, field)
	return out
}

// Marks the span status as an error for an ERROR level
func (o *observation) statusIfError(level, message string) {
	if level == "ERROR" && o.span.IsRecording() {
		o.span.SetStatus(codes.Error, message)
	}
}

// Applies new fields to a recording observation
func (o *observation) update(f obsFields) {
	if !o.span.IsRecording() {
		return
	}
	f.input = o.processMedia(f.input, "input")
	f.output = o.processMedia(f.output, "output")
	f.metadata = o.processMedia(f.metadata, "metadata")
	setAttrs(o.span, o.attributes(f))
	o.statusIfError(f.level, f.statusMessage)
}

// Sets trace level input and output on a recording observation
func (o *observation) setTraceIO(input, output any) {
	if !o.span.IsRecording() {
		return
	}
	input = o.processMedia(input, "input")
	output = o.processMedia(output, "output")
	setAttrs(o.span, dropNil([]attrEntry{{attrTraceInput, serialized(input)}, {attrTraceOutput, serialized(output)}}))
}

// Ends the span
func (o *observation) end() { o.span.End() }

// Reports a string of n lowercase hex characters
func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
