package media

import (
	"context"
	"fmt"
	"iter"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
)

const DefaultTTSOutputFormat = "mp3"

var ValidTTSOutputFormats = map[string]bool{"mp3": true, "wav": true, "ogg": true, "opus": true, "flac": true}

// Synthesis keyword arguments, a nil pointer meaning the provider default
type TTSOptions struct {
	Voice  *string
	Model  *string
	Speed  *float64
	Format string
	Extra  map[string]any
}

// Raised by a provider that has no streaming synthesis
type TTSNotImplementedError struct{ Message string }

func (e *TTSNotImplementedError) Error() string { return e.Message }

// Text to speech backend serviced when tts.provider names neither a built in nor a command provider
type TTSProvider interface {
	provider.CatalogProviderBase
	ListVoices() []map[string]any
	DefaultVoice() string
	Synthesize(ctx context.Context, text, outputPath string, opts TTSOptions) (string, error)
	Stream(ctx context.Context, text string, opts TTSOptions) (iter.Seq2[[]byte, error], error)
	Warm(ctx context.Context) error
	Release(ctx context.Context) error
	VoiceCompatible() bool
	StreamsPCM() bool
	StreamSampleRate() any
}

// Embeddable TTSProvider defaults dispatching through Self, which also binds the catalog defaults
type TTSProviderDefaults struct {
	provider.CatalogProviderDefaults
}

// Defaults bound to the outer provider
func NewTTSProviderDefaults(self TTSProvider) TTSProviderDefaults {
	return TTSProviderDefaults{provider.CatalogProviderDefaults{Self: self}}
}

func (d TTSProviderDefaults) tts() TTSProvider {
	p, ok := d.Self.(TTSProvider)
	if !ok {
		panic("media.TTSProviderDefaults.Self is not bound to its TTS provider")
	}
	return p
}

// Voice catalog entries, empty unless overridden
func (TTSProviderDefaults) ListVoices() []map[string]any { return []map[string]any{} }

// Id of the first voice entry, empty when the catalog is empty or the entry has no string id
func (d TTSProviderDefaults) DefaultVoice() string {
	voices := d.tts().ListVoices()
	if len(voices) == 0 {
		return ""
	}
	id, _ := voices[0]["id"].(string)
	return id
}

// Refuses streaming so the dispatcher falls back to synthesize and a whole file read
func (d TTSProviderDefaults) Stream(context.Context, string, TTSOptions) (iter.Seq2[[]byte, error], error) {
	return nil, &TTSNotImplementedError{fmt.Sprintf("TTS provider %s does not implement streaming "+
		"synthesis. Use synthesize() instead, or implement stream() if your backend supports it.", pyrepr.StrRepr(d.tts().Name()))}
}

// Pre loads resident resources when speech output turns on, nothing unless overridden
func (TTSProviderDefaults) Warm(context.Context) error { return nil }

// Frees resident resources when the last speech lease ends, nothing unless overridden
func (TTSProviderDefaults) Release(context.Context) error { return nil }

// Output is not voice bubble material unless the provider opts in
func (TTSProviderDefaults) VoiceCompatible() bool { return false }

// Raw PCM streaming is off unless the provider opts in
func (TTSProviderDefaults) StreamsPCM() bool { return false }

// No PCM sample rate unless the provider declares one
func (TTSProviderDefaults) StreamSampleRate() any { return nil }

// Output format clamped to the valid set, anything else the mp3 default
func ResolveTTSOutputFormat(value any) string {
	s, ok := value.(string)
	if !ok {
		return DefaultTTSOutputFormat
	}
	v := pystr.Lower(pystr.Strip(s))
	if ValidTTSOutputFormats[v] {
		return v
	}
	return DefaultTTSOutputFormat
}

// Python plugin TTS provider object proxied from the plugin host
type hostedTTSProvider struct{ obj *plugins.HostedObject }

// Plugin host object that subclasses TTSProvider as a provider, any other value unchanged
func adaptHostedTTSProvider(p any) any {
	obj, ok := p.(*plugins.HostedObject)
	if !ok || obj == nil || !obj.HasMethod("synthesize") {
		return p
	}
	return hostedTTSProvider{obj: obj}
}

func (h hostedTTSProvider) attr(name string) (any, error) {
	return h.obj.Get(context.Background(), name)
}

func (h hostedTTSProvider) mustCall(method string) any {
	v, err := h.obj.Call(context.Background(), method, nil, nil)
	if err != nil {
		panic(err)
	}
	return v
}

func (h hostedTTSProvider) Name() string {
	v, _ := h.attr("name")
	s, _ := v.(string)
	return s
}

func (h hostedTTSProvider) DisplayName() string {
	if v, err := h.attr("display_name"); err == nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return pystr.Title(h.Name())
}

func (h hostedTTSProvider) GetSetupSchema() map[string]any {
	if m, ok := h.mustCall("get_setup_schema").(map[string]any); ok {
		return m
	}
	return map[string]any{"name": h.DisplayName(), "badge": "", "tag": "", "env_vars": []any{}}
}

func (h hostedTTSProvider) IsAvailable() bool { return pyTruthy(h.mustCall("is_available")) }

func hostedRows(v any) []map[string]any {
	rows, _ := v.([]any)
	out := []map[string]any{}
	for _, r := range rows {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func (h hostedTTSProvider) ListModels() []map[string]any {
	return hostedRows(h.mustCall("list_models"))
}

func (h hostedTTSProvider) DefaultModel() string {
	s, _ := h.mustCall("default_model").(string)
	return s
}

func (h hostedTTSProvider) ListVoices() []map[string]any {
	return hostedRows(h.mustCall("list_voices"))
}

func (h hostedTTSProvider) DefaultVoice() string {
	s, _ := h.mustCall("default_voice").(string)
	return s
}

// Keyword arguments of a synthesize or stream call, None for every unset default
func (o TTSOptions) kwargs() map[string]any {
	kw := map[string]any{}
	for k, v := range o.Extra {
		kw[k] = v
	}
	kw["voice"], kw["model"], kw["speed"] = nil, nil, nil
	if o.Voice != nil {
		kw["voice"] = *o.Voice
	}
	if o.Model != nil {
		kw["model"] = *o.Model
	}
	if o.Speed != nil {
		kw["speed"] = *o.Speed
	}
	kw["format"] = o.Format
	return kw
}

func (h hostedTTSProvider) Synthesize(ctx context.Context, text, outputPath string, opts TTSOptions) (string, error) {
	if opts.Format == "" {
		opts.Format = DefaultTTSOutputFormat
	}
	v, err := h.obj.Call(ctx, "synthesize", []any{text, outputPath}, opts.kwargs())
	if err != nil {
		return "", err
	}
	s, _ := v.(string)
	return s, nil
}

func (h hostedTTSProvider) Stream(ctx context.Context, text string, opts TTSOptions) (iter.Seq2[[]byte, error], error) {
	if opts.Format == "" {
		opts.Format = "opus"
	}
	v, err := h.obj.Call(ctx, "stream", []any{text}, opts.kwargs())
	if err != nil {
		return nil, err
	}
	next, err := hostedIterator(ctx, v)
	if err != nil {
		return nil, err
	}
	return func(yield func([]byte, error) bool) {
		for {
			item, more, err := next()
			if err != nil {
				yield(nil, err)
				return
			}
			if !more {
				return
			}
			var chunk []byte
			switch b := item.(type) {
			case []byte:
				chunk = b
			case string:
				chunk = []byte(b)
			default:
				yield(nil, fmt.Errorf("TTS provider %s stream() yielded %T instead of bytes", pyrepr.StrRepr(h.Name()), item))
				return
			}
			if !yield(chunk, nil) {
				return
			}
		}
	}, nil
}

func (h hostedTTSProvider) Warm(ctx context.Context) error {
	_, err := h.obj.Call(ctx, "warm", nil, nil)
	return err
}

func (h hostedTTSProvider) Release(ctx context.Context) error {
	_, err := h.obj.Call(ctx, "release", nil, nil)
	return err
}

func (h hostedTTSProvider) VoiceCompatible() bool {
	v, err := h.attr("voice_compatible")
	if err != nil {
		panic(err)
	}
	return pyTruthy(v)
}

func (h hostedTTSProvider) StreamsPCM() bool {
	v, err := h.attr("streams_pcm")
	if err != nil {
		panic(err)
	}
	return pyTruthy(v)
}

func (h hostedTTSProvider) StreamSampleRate() any {
	v, err := h.attr("stream_sample_rate")
	if err != nil {
		panic(err)
	}
	return v
}

// Step function over a stream() result, a hosted iterator or a plain sequence, a TypeError for anything else
func hostedIterator(ctx context.Context, v any) (func() (any, bool, error), error) {
	switch it := v.(type) {
	case *plugins.HostedObject:
		return func() (any, bool, error) { return it.Next(ctx) }, nil
	case []any:
		i := 0
		return func() (any, bool, error) {
			if i >= len(it) {
				return nil, false, nil
			}
			i++
			return it[i-1], true, nil
		}, nil
	}
	return nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(v))
}
