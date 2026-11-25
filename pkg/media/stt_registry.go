package media

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Model, language and extra keyword arguments of one plugin transcribe call, nil meaning unset
type TranscribeOptions struct {
	Model    any
	Language any
	Extra    map[string]any
}

// Pluggable speech to text backend registered by plugins under a non built in name
type TranscriptionProvider interface {
	provider.CatalogProviderBase
	Transcribe(ctx context.Context, filePath string, opts TranscribeOptions) (any, error)
	StreamingCapable() bool
	OpenStreamSession(ctx context.Context, language, prompt any) (TranscriptionStreamSession, error)
}

// Live 16 kHz mono s16le PCM to transcript session for one utterance
type TranscriptionStreamSession interface {
	PushAudio(chunk []byte) error
	Finalize() (map[string]any, error)
	PartialTranscript() (string, error)
}

// Embeddable TranscriptionProvider defaults dispatching through Self
type TranscriptionProviderDefaults struct {
	provider.CatalogProviderDefaults
}

// Defaults bound to the outer provider
func NewTranscriptionProviderDefaults(self TranscriptionProvider) TranscriptionProviderDefaults {
	return TranscriptionProviderDefaults{provider.CatalogProviderDefaults{Self: self}}
}

// Live streaming is off unless the provider opts in
func (TranscriptionProviderDefaults) StreamingCapable() bool { return false }

// Refuses live sessions for providers without streaming support
func (d TranscriptionProviderDefaults) OpenStreamSession(context.Context, any, any) (TranscriptionStreamSession, error) {
	return nil, &sttNotImplementedError{msg: fmt.Sprintf("%s does not support live streaming transcription", d.Self.Name())}
}

// Python NotImplementedError of an unsupported provider capability
type sttNotImplementedError struct{ msg string }

func (e *sttNotImplementedError) Error() string { return e.msg }

// Embeddable stream session default with no partial transcript
type TranscriptionStreamSessionDefaults struct{}

var transcriptionRegistryBuiltinNames = sttSet("local", "local_command", "groq", "openai", "mistral", "xai", "elevenlabs", "deepinfra")

// Warns that a plugin tried to take a built in STT name and was ignored
func warnTranscriptionBuiltinCollision(key string) error {
	names := make([]string, 0, len(transcriptionRegistryBuiltinNames))
	for n := range transcriptionRegistryBuiltinNames {
		names = append(names, n)
	}
	sort.Strings(names)
	pkgLog().Warn(fmt.Sprintf("Transcription provider '%s' shadows a built-in name; registration ignored. "+
		"Built-in STT providers (%s) always win — pick a different name.", key, strings.Join(names, ", ")))
	return nil
}

var transcriptionRegistry = provider.NewProviderRegistry[TranscriptionProvider](provider.ProviderRegistryOptions{
	Label: "Transcription", Logger: pkgLog(), Normalize: provider.LowerKey,
	BuiltinNames: transcriptionRegistryBuiltinNames, OnBuiltinCollision: warnTranscriptionBuiltinCollision,
})

// Registers a plugin transcription provider, a built in name ignored with a warning
func RegisterTranscriptionProvider(p TranscriptionProvider, scope string) error {
	return transcriptionRegistry.Register(p, scope)
}

// Provider registered under a name, scoped registration first, nil when absent
func GetTranscriptionProvider(ctx context.Context, name, scope string) TranscriptionProvider {
	return transcriptionRegistry.GetProvider(ctx, name, scope)
}

// Plugin facing registry that register_transcription_provider writes into, a plugin host object stored as the provider adapted from it
func TranscriptionScopedRegistry() plugins.ScopedRegistry {
	return plugins.HostedScopedRegistry{Inner: transcriptionRegistry.Scoped(), Adapt: adaptHostedTranscriptionProvider,
		Object: func(held any) (*plugins.HostedObject, bool) {
			h, ok := held.(hostedTranscriptionProvider)
			return h.obj, ok
		}}
}

// Python plugin transcription provider object proxied from the plugin host
type hostedTranscriptionProvider struct{ obj *plugins.HostedObject }

// Plugin host object that subclasses TranscriptionProvider as a provider, any other value unchanged
func adaptHostedTranscriptionProvider(p any) any {
	obj, ok := p.(*plugins.HostedObject)
	if !ok || obj == nil || !obj.HasMethod("transcribe") {
		return p
	}
	return hostedTranscriptionProvider{obj: obj}
}

func (h hostedTranscriptionProvider) mustCall(method string) any {
	v, err := h.obj.Call(context.Background(), method, nil, nil)
	if err != nil {
		panic(err)
	}
	return v
}

func (h hostedTranscriptionProvider) Name() string {
	v, _ := h.obj.Get(context.Background(), "name")
	s, _ := v.(string)
	return s
}

func (h hostedTranscriptionProvider) DisplayName() string {
	if v, err := h.obj.Get(context.Background(), "display_name"); err == nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return textutil.Title(h.Name())
}

func (h hostedTranscriptionProvider) GetSetupSchema() map[string]any {
	if m, ok := h.mustCall("get_setup_schema").(map[string]any); ok {
		return m
	}
	return map[string]any{"name": h.DisplayName(), "badge": "", "tag": "", "env_vars": []any{}}
}

func (h hostedTranscriptionProvider) IsAvailable() bool {
	return anyutil.IsTrue(h.mustCall("is_available"))
}

func (h hostedTranscriptionProvider) ListModels() []map[string]any {
	return hostedRows(h.mustCall("list_models"))
}

func (h hostedTranscriptionProvider) DefaultModel() string {
	s, _ := h.mustCall("default_model").(string)
	return s
}

func (h hostedTranscriptionProvider) Transcribe(ctx context.Context, filePath string, opts TranscribeOptions) (any, error) {
	kwargs := map[string]any{}
	for k, v := range opts.Extra {
		kwargs[k] = v
	}
	kwargs["model"], kwargs["language"] = opts.Model, opts.Language
	return h.obj.Call(ctx, "transcribe", []any{filePath}, kwargs)
}

func (h hostedTranscriptionProvider) StreamingCapable() bool {
	v, err := h.obj.Get(context.Background(), "streaming_capable")
	return err == nil && anyutil.IsTrue(v)
}

func (h hostedTranscriptionProvider) OpenStreamSession(ctx context.Context, language, prompt any) (TranscriptionStreamSession, error) {
	v, err := h.obj.Call(ctx, "open_stream_session", nil, map[string]any{"language": language, "prompt": prompt})
	if err != nil {
		return nil, err
	}
	obj, ok := v.(*plugins.HostedObject)
	if !ok || obj == nil {
		return nil, fmt.Errorf("transcription provider %s open_stream_session() returned %T instead of a session", h.Name(), v)
	}
	return hostedTranscriptionStreamSession{obj: obj, ctx: ctx}, nil
}

// Python live transcription session proxied from the plugin host
type hostedTranscriptionStreamSession struct {
	obj *plugins.HostedObject
	ctx context.Context
}

func (s hostedTranscriptionStreamSession) PushAudio(chunk []byte) error {
	_, err := s.obj.Call(s.ctx, "push_audio", []any{chunk}, nil)
	return err
}

func (s hostedTranscriptionStreamSession) Finalize() (map[string]any, error) {
	v, err := s.obj.Call(s.ctx, "finalize", nil, nil)
	if err != nil {
		return nil, err
	}
	m, _ := v.(map[string]any)
	return m, nil
}

func (s hostedTranscriptionStreamSession) PartialTranscript() (string, error) {
	if !s.obj.HasMethod("partial_transcript") {
		return "", nil
	}
	v, err := s.obj.Call(s.ctx, "partial_transcript", nil, nil)
	if err != nil {
		return "", err
	}
	str, _ := v.(string)
	return str, nil
}
