package media

import (
	"context"
	"fmt"
	"math"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Runs third party plugin code, a panic becoming the returned error
func pluginCall(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = e
			} else {
				err = fmt.Errorf("%v", r)
			}
		}
	}()
	return fn()
}

// Plugin discovery seam shared with the image providers
var ttsEnsurePluginsDiscovered = func(ctx context.Context, force bool) { ensurePluginsDiscoveredHook(ctx, force) }

// Registered TTS provider of a key after optional discovery, rediscovering with force on a miss when asked
func lookupPluginTTSProvider(ctx context.Context, key string, discover, retry bool) (p TTSProvider, err error) {
	err = pluginCall(func() error {
		if discover {
			ttsEnsurePluginsDiscovered(ctx, false)
		}
		p = GetTTSProvider(ctx, key, "")
		if p == nil && retry {
			ttsEnsurePluginsDiscovered(ctx, true)
			p = GetTTSProvider(ctx, key, "")
		}
		return nil
	})
	return p, err
}

// Normalized provider a plugin may service, empty for built ins, nous and same named command providers
func pluginRouteKey(provider string, ttsConfig map[string]any) string {
	key := pystr.Strip(pystr.Lower(provider))
	if key == "" || builtinTTSProviders[key] || key == NousManagedProvider {
		return ""
	}
	if isCommandProviderConfig(getNamedTTSProviderConfig(ttsConfig, key)) {
		return ""
	}
	return key
}

// Voice, model and speed options of a plugin call, nil meaning the provider default
func pluginVoiceKwargs(ttsConfig map[string]any) TTSOptions {
	var opts TTSOptions
	if v, ok := ttsConfig["voice"].(string); ok && v != "" {
		opts.Voice = &v
	}
	if m, ok := ttsConfig["model"].(string); ok && m != "" {
		opts.Model = &m
	}
	if _, isBool := ttsConfig["speed"].(bool); isBool {
		f := 0.0
		if ttsConfig["speed"].(bool) {
			f = 1
		}
		opts.Speed = &f
	} else if f, ok := pyRealNumber(ttsConfig["speed"]); ok {
		opts.Speed = &f
	}
	return opts
}

// Plugin and sample rate when the servicing plugin opted into raw PCM streaming with a valid rate
func pluginPCMStreamingProvider(ctx context.Context, provider string, ttsConfig map[string]any) (TTSProvider, int, bool) {
	key := pluginRouteKey(provider, ttsConfig)
	if key == "" {
		return nil, 0, false
	}
	var plugin TTSProvider
	var rate any
	usable := false
	err := pluginCall(func() error {
		p, err := lookupPluginTTSProvider(ctx, key, true, false)
		if err != nil {
			return err
		}
		if p == nil || !p.StreamsPCM() || !p.IsAvailable() {
			return nil
		}
		plugin, rate, usable = p, p.StreamSampleRate(), true
		return nil
	})
	if err != nil {
		ttsLog().Debug(fmt.Sprintf("plugin TTS streaming lookup failed for '%s'", key), "error", err)
		return nil, 0, false
	}
	if !usable {
		return nil, 0, false
	}
	f, ok := pyRealNumber(rate)
	if _, isBool := rate.(bool); isBool || !ok || math.IsInf(f, 0) || math.IsNaN(f) || f < 1 {
		ttsLog().Warn(fmt.Sprintf("TTS provider '%s' sets streams_pcm without a valid stream_sample_rate (%s); using per-sentence synthesis instead", key, pyrepr.Repr(rate)))
		return nil, 0, false
	}
	return plugin, int(f), true
}

// Synthesis through a registered plugin provider, empty with no error meaning fall through
func dispatchToPluginTTSProvider(ctx context.Context, text, outputPath, provider string, ttsConfig map[string]any) (string, error) {
	key := pluginRouteKey(provider, ttsConfig)
	if key == "" {
		return "", nil
	}
	plugin, err := lookupPluginTTSProvider(ctx, key, true, true)
	if err != nil {
		ttsLog().Debug(fmt.Sprintf("tts plugin dispatch skipped (discovery failed): %v", err))
		return "", nil
	}
	if plugin == nil {
		return "", nil
	}
	format := "mp3"
	if fmtValue := dictGetDefault(ttsConfig, "output_format", defaultCommandTTSOutputFormat); pyTruthy(fmtValue) {
		format = pystr.Lower(pyrepr.Str(fmtValue))
	}
	ttsLog().Info(fmt.Sprintf("Generating speech with plugin TTS provider '%s'...", key))
	opts := pluginVoiceKwargs(ttsConfig)
	opts.Format = format
	var written string
	if err := pluginCall(func() error {
		var serr error
		written, serr = plugin.Synthesize(ctx, text, outputPath, opts)
		return serr
	}); err != nil {
		return "", err
	}
	if written != "" {
		return written, nil
	}
	return outputPath, nil
}

// Whether the registered plugin opts into voice bubble delivery, false on any failure
func pluginProviderIsVoiceCompatible(ctx context.Context, provider string) bool {
	key := pystr.Strip(pystr.Lower(provider))
	if key == "" || builtinTTSProviders[key] {
		return false
	}
	compatible := false
	err := pluginCall(func() error {
		p, err := lookupPluginTTSProvider(ctx, key, false, false)
		if err != nil {
			return err
		}
		compatible = p != nil && p.VoiceCompatible()
		return nil
	})
	if err != nil {
		ttsLog().Debug(fmt.Sprintf("tts plugin voice_compatible check failed for '%s': %v", key, err))
		return false
	}
	return compatible
}

// Whether the discovered plugin provider reports available, false on any failure
func pluginProviderIsAvailable(ctx context.Context, provider string) bool {
	available := false
	_ = pluginCall(func() error {
		p, err := lookupPluginTTSProvider(ctx, provider, true, false)
		if err != nil {
			return err
		}
		available = p != nil && p.IsAvailable()
		return nil
	})
	return available
}
