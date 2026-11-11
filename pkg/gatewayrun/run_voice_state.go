package gatewayrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Voice mode key of a chat, namespaced by the speaking profile under multiplexing except for default
func voiceKey(platform gateway.Platform, chatID string, profile *string) string {
	base := fmt.Sprintf("%s:%s", platform, chatID)
	name := ""
	if profile != nil {
		name = pystr.Strip(*profile)
	}
	if name == "" || name == "default" {
		return base
	}
	return name + ":" + base
}

// Voice mode key of the bot and chat pair a source arrived on, namespaced by the receiving adapter's owner
func (r *GatewayRunner) voiceKeyForSource(source *gateway.SessionSource) string {
	return voiceKey(source.Platform, source.ChatID, r.adapterProfileForSource(source))
}

// Persists the per chat voice modes, a failed write only logged
func (r *GatewayRunner) saveVoiceModes() {
	path := r.voiceModePath()
	err := os.MkdirAll(filepath.Dir(path), 0o755)
	if err == nil {
		object := pyjson.NewObject()
		for key, mode := range r.voiceMode.All() {
			object.Set(key, mode)
		}
		var payload string
		if payload, err = pyjson.Dumps(object, pyjson.Indent(2)); err == nil {
			err = os.WriteFile(path, []byte(payload), 0o644)
		}
	}
	if err != nil {
		runLog().Warn(fmt.Sprintf("Failed to save voice modes: %s", pyrepr.ExceptionMessage(err)))
	}
}

// Records a voice mode, a new key going last as a dict assignment does
func (r *GatewayRunner) setVoiceMode(key, mode string) {
	if r.voiceMode == nil {
		r.voiceMode = pydict.New[string, string]()
	}
	r.voiceMode.Set(key, mode)
}

// Adds or discards a chat in the adapter's enabled or disabled auto TTS set, adding clears the other set
func toggleAdapterAutoTTSSet(adapter gateway.PlatformAdapter, chatID string, on, enable bool) {
	if adapter == nil || adapter.Base() == nil {
		return
	}
	target, other := adapter.Base().AutoTTSChats(enable), adapter.Base().AutoTTSChats(!enable)
	if target == nil {
		return
	}
	if !on {
		delete(target, chatID)
		return
	}
	target[chatID] = struct{}{}
	if other != nil {
		delete(other, chatID)
	}
}

// Puts a chat in or out of the adapter's auto TTS disabled set
func (r *GatewayRunner) setAdapterAutoTTSDisabled(adapter gateway.PlatformAdapter, chatID string, disabled bool) {
	toggleAdapterAutoTTSSet(adapter, chatID, disabled, false)
}

// Puts a chat in or out of the adapter's auto TTS enabled set
func (r *GatewayRunner) setAdapterAutoTTSEnabled(adapter gateway.PlatformAdapter, chatID string, enabled bool) {
	toggleAdapterAutoTTSSet(adapter, chatID, enabled, true)
}

// Records and persists a chat's voice mode and mirrors it into the adapter's auto TTS sets
func (r *GatewayRunner) applyVoiceMode(adapter gateway.PlatformAdapter, key, chatID, mode string) {
	r.setVoiceMode(key, mode)
	r.saveVoiceModes()
	toggleAdapterAutoTTSSet(adapter, chatID, true, mode != "off")
}

// Restores persisted voice state into a live adapter, the speak default and its per chat override sets
func (r *GatewayRunner) syncVoiceModeStateToAdapter(ctx context.Context, adapter gateway.PlatformAdapter) {
	if adapter == nil || adapter.Base() == nil || adapter.Base().Platform == "" {
		return
	}
	base := adapter.Base()
	platform := base.Platform
	sets := []struct {
		chats map[string]struct{}
		modes map[string]bool
	}{{base.AutoTTSChats(false), map[string]bool{"off": true}}, {base.AutoTTSChats(true), map[string]bool{"voice_only": true, "all": true}}}
	autoTTSDefault := false
	if tree, err := config.Load(home.Dir(ctx), nil); err == nil {
		if voice, ok := tree["voice"].(map[string]any); ok {
			autoTTSDefault = pyrepr.Truthy(voice["auto_tts"])
		}
	}
	base.SetAutoTTSDefault(platform != "a2a" && autoTTSDefault)
	owner := base.OwnerProfile()
	prefix := voiceKey(platform, "", &owner)
	for _, set := range sets {
		clear(set.chats)
		for key, mode := range r.voiceMode.All() {
			if set.modes[mode] && strings.HasPrefix(key, prefix) {
				set.chats[key[len(prefix):]] = struct{}{}
			}
		}
	}
}
