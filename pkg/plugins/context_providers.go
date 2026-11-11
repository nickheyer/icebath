package plugins

import (
	"fmt"
	"strings"
)

// Scope keyed process wide provider registry of one category, owned by a higher package
type ScopedRegistry interface {
	Accepts(provider any) bool
	ProviderName(provider any) string
	Identity(provider any) error
	Snapshot(name, scope string) any
	Register(provider any, scope string) (bool, error)
	Restore(name string, current, replacement any, scope string) bool
}

// One generated register category provider method of the plugin context
type scopedProviderRow struct {
	method        string
	kind          string
	base          string
	label         string
	article       string
	normalize     string
	rejectMessage string
	admit         func(h string, m *Manifest) string
	identity      bool
	customAccept  bool
}

var scopedProviderRegistrars = []scopedProviderRow{
	{method: "register_login_backend", kind: "login_backend", base: "LoginBackend", label: "login backend", normalize: "none",
		rejectMessage: "Plugin '%s' login backend rejected: %s", admit: loginBackendProvenanceError, identity: true},
	{method: "register_browser_page_backend", kind: "browser_page_backend", base: "BrowserPageBackend", label: "browser page backend",
		rejectMessage: "Plugin '%s' browser page backend rejected: %s", admit: browserPageBackendProvenanceError},
	{method: "register_image_gen_provider", kind: "image_gen_provider", base: "ImageGenProvider", label: "image_gen provider", article: "an"},
	{method: "register_video_gen_provider", kind: "video_gen_provider", base: "VideoGenProvider", label: "video_gen provider"},
	{method: "register_web_search_provider", kind: "web_search_provider", base: "WebSearchProvider", label: "web provider"},
	{method: "register_browser_provider", kind: "browser_provider", base: "BrowserProvider", label: "browser provider"},
	{method: "register_terminal_environment_provider", kind: "terminal_environment_provider", base: "TerminalEnvironmentProvider",
		label: "terminal environment provider", normalize: "lower", rejectMessage: "Plugin '%s' terminal environment provider rejected: %s"},
	{method: "register_secret_source", kind: "secret_source", base: "SecretSource", label: "secret source", normalize: "none", customAccept: true},
	{method: "register_tts_provider", kind: "tts_provider", base: "TTSProvider", label: "TTS provider", normalize: "lower"},
	{method: "register_transcription_provider", kind: "transcription_provider", base: "TranscriptionProvider", label: "transcription provider", normalize: "lower"},
	{method: "register_delegation_provider", kind: "delegation_provider", base: "DelegationProvider", label: "delegation provider", normalize: "lower"},
}

// Row of a provider kind, false for an unknown kind
func scopedProviderRowFor(kind string) (scopedProviderRow, bool) {
	for _, row := range scopedProviderRegistrars {
		if row.kind == kind {
			return row, true
		}
	}
	return scopedProviderRow{}, false
}

// Registers a provider of one category into its scope keyed registry and leases the slot for unload
func (c *Context) RegisterProvider(kind string, provider any) (*Registration, error) {
	row, ok := scopedProviderRowFor(kind)
	if !ok {
		return nil, fmt.Errorf("unknown plugin provider kind %s", kind)
	}
	if c.ignoredAfterAbandonedLoad(row.method) {
		return nil, nil
	}
	var handle *Registration
	var err error
	serializedReplacement(func() { handle, err = c.registerProviderRow(row, provider) })
	return handle, err
}

// Admission, identity, type check, register and lease body shared by every provider registrar
func (c *Context) registerProviderRow(row scopedProviderRow, provider any) (*Registration, error) {
	if row.admit != nil {
		if refusal := row.admit(c.manager.home, c.manifest); refusal != "" {
			logWarn(fmt.Sprintf(row.rejectMessage, c.manifest.Name, refusal))
			return nil, nil
		}
	}
	registry, err := needService("ProviderRegistries", func(s *Services) ScopedRegistry { return s.ProviderRegistries[row.kind] })
	if err != nil {
		return nil, err
	}
	if row.identity {
		if err := registry.Identity(provider); err != nil {
			logWarn(fmt.Sprintf(row.rejectMessage, c.manifest.Name, err))
			return nil, nil
		}
	}
	article := row.article
	if article == "" {
		article = "a"
	}
	if c.wrongType(registry.Accepts(provider), article, row.label, row.base) {
		return nil, nil
	}
	name := registry.ProviderName(provider)
	switch row.normalize {
	case "", "strip":
		name = pyStrip(name)
	case "lower":
		name = strings.ToLower(pyStrip(name))
	}
	scope := c.manager.home
	previous := registry.Snapshot(name, scope)
	accepted, err := registry.Register(provider, scope)
	if err != nil {
		if row.rejectMessage == "" {
			return nil, err
		}
		logWarn(fmt.Sprintf(row.rejectMessage, c.manifest.Name, err))
		return nil, nil
	}
	if (row.customAccept && !accepted) || !sameRegistration(registry.Snapshot(name, scope), provider) {
		return nil, nil
	}
	handle := c.manager.trackScopedRegistration(c.manifest, row.kind, name, func(name string, current, replacement any, scope string) error {
		registry.Restore(name, current, replacement, scope)
		return nil
	}, provider, previous, nil)
	logInfo(fmt.Sprintf("Plugin '%s' registered %s: %s", c.manifest.Name, row.label, name))
	return handle, nil
}

// Registers a trusted in process browser vault login backend
func (c *Context) RegisterLoginBackend(p any) (*Registration, error) {
	return c.RegisterProvider("login_backend", p)
}

// Registers a browser page backend for browsers without a CDP endpoint
func (c *Context) RegisterBrowserPageBackend(p any) (*Registration, error) {
	return c.RegisterProvider("browser_page_backend", p)
}

// Registers an image generation provider matched by image_gen.provider
func (c *Context) RegisterImageGenProvider(p any) (*Registration, error) {
	return c.RegisterProvider("image_gen_provider", p)
}

// Registers a video generation provider matched by video_gen.provider
func (c *Context) RegisterVideoGenProvider(p any) (*Registration, error) {
	return c.RegisterProvider("video_gen_provider", p)
}

// Registers a web search provider matched by the web backend settings
func (c *Context) RegisterWebSearchProvider(p any) (*Registration, error) {
	return c.RegisterProvider("web_search_provider", p)
}

// Registers a cloud browser provider matched by browser.cloud_provider
func (c *Context) RegisterBrowserProvider(p any) (*Registration, error) {
	return c.RegisterProvider("browser_provider", p)
}

// Registers a terminal environment provider matched by terminal.backend
func (c *Context) RegisterTerminalEnvironmentProvider(p any) (*Registration, error) {
	return c.RegisterProvider("terminal_environment_provider", p)
}

// Registers a secret source run by the dotenv loader when secrets.name is enabled
func (c *Context) RegisterSecretSource(source any) (*Registration, error) {
	return c.RegisterProvider("secret_source", source)
}

// Registers a text to speech provider matched by tts.provider
func (c *Context) RegisterTTSProvider(p any) (*Registration, error) {
	return c.RegisterProvider("tts_provider", p)
}

// Registers a speech to text provider matched by stt.provider
func (c *Context) RegisterTranscriptionProvider(p any) (*Registration, error) {
	return c.RegisterProvider("transcription_provider", p)
}

// Registers a delegation provider matched by delegation.backend
func (c *Context) RegisterDelegationProvider(p any) (*Registration, error) {
	if c.delegationSink != nil {
		c.delegationSink(p)
		return nil, nil
	}
	return c.RegisterProvider("delegation_provider", p)
}
