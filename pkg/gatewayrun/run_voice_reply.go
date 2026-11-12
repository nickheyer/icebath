package gatewayrun

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"maps"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Adapter that plays audio in a connected voice channel of a guild
type voiceChannelPlayer interface {
	PlayInVoiceChannel(ctx context.Context, guildID int64, audioPath string) error
	IsInVoiceChannel(guildID int64) bool
}

// Whether the runner sends a spoken reply, never for errors, agent peers, local turns or turns that spoke
func (r *GatewayRunner) shouldSendVoiceReply(ctx context.Context, event *gateway.MessageEvent, response string, agentMessages []map[string]any, alreadySent bool) bool {
	if response == "" || strings.HasPrefix(response, "Error:") {
		return false
	}
	if event.Source.Platform == "a2a" || event.Source.Platform == gateway.PlatformLocal {
		return false
	}
	chatID := event.Source.ChatID
	voiceMode, hasMode := "", false
	if r.voiceMode != nil {
		voiceMode, hasMode = r.voiceMode.Get(r.voiceKeyForSource(event.Source))
	}
	isVoiceInput := event.MessageType == gateway.MessageTypeVoice
	adapterAutoTTS := false
	func() {
		defer func() { _ = recover() }()
		if adapter, err := r.deliveryAdapterFor(ctx, event.Source); err == nil && adapter != nil {
			adapterAutoTTS = gateway.ShouldAutoTTSForChat(adapter, chatID)
		}
	}()
	if !((hasMode && voiceMode == "all") || (hasMode && voiceMode == "voice_only" && isVoiceInput) || (!hasMode && adapterAutoTTS)) {
		mode := "None"
		if hasMode {
			mode = voiceMode
		}
		runLog().Debug(fmt.Sprintf("Auto voice reply skipped: mode=%s adapter_auto_tts=%s chat=%s platform=%s", mode, pyrepr.Str(adapterAutoTTS), chatID, string(event.Source.Platform)))
		return false
	}
	start := 0
	for i := len(agentMessages) - 1; i >= 0; i-- {
		if agentMessages[i]["role"] == "user" {
			start = i
			break
		}
	}
	for _, msg := range agentMessages[start:] {
		if msg["role"] != "assistant" {
			continue
		}
		calls, _ := msg["tool_calls"].([]any)
		for _, raw := range calls {
			call, _ := pyjson.Dict(raw)
			fn, _ := pyjson.Dict(call["function"])
			if fn["name"] == "text_to_speech" {
				return false
			}
		}
	}
	return !(isVoiceInput && !alreadySent)
}

// Synthesizes the reply and sends it as voice before the text, removing every produced audio file after
func (r *GatewayRunner) sendVoiceReply(ctx context.Context, event *gateway.MessageEvent, text string) {
	var audioPath string
	var actualPaths []string
	defer func() {
		seen := map[string]bool{}
		for _, p := range append([]string{audioPath}, actualPaths...) {
			if p != "" && !seen[p] {
				seen[p] = true
				_ = os.Remove(p)
			}
		}
	}()
	err := func() (err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%s", pyrepr.Str(rec))
			}
		}()
		ttsText := tools.StripMarkdownForTTS(text)
		if ttsText == "" {
			return nil
		}
		audioPath, err = gateway.BuildAutoTTSOutputPath(event.Source.Platform)
		if err != nil {
			return err
		}
		raw, err := media.TextToSpeechTool(ctx, ttsText, audioPath)
		if err != nil {
			return err
		}
		var result map[string]any
		if json.Unmarshal([]byte(raw), &result) != nil || result == nil {
			runes := []rune(raw)
			runLog().Warn("Auto voice reply TTS returned invalid JSON: " + string(runes[:min(len(runes), 200)]))
			return nil
		}
		var candidates []any
		if list, ok := result["file_paths"].([]any); ok && len(list) > 0 {
			candidates = list
		} else if fp, present := result["file_path"]; present {
			candidates = []any{fp}
		} else {
			candidates = []any{audioPath}
		}
		var paths []string
		for _, c := range candidates {
			if !pyrepr.Truthy(c) {
				continue
			}
			if st, err := os.Stat(pyrepr.Str(c)); err == nil && st.Mode().IsRegular() {
				paths = append(paths, pyrepr.Str(c))
			}
		}
		if !pyrepr.Truthy(result["success"]) || len(paths) == 0 {
			runLog().Warn("Auto voice reply TTS failed: " + pyrepr.Str(result["error"]))
			return nil
		}
		actualPaths = paths
		return r.deliverVoiceReply(ctx, event, actualPaths)
	}()
	if err != nil {
		runLog().Warn("Auto voice reply failed: " + pyrepr.ExceptionMessage(err))
	}
}

// Plays the files in the connected voice channel, else sends them as voice messages
func (r *GatewayRunner) deliverVoiceReply(ctx context.Context, event *gateway.MessageEvent, audioPaths []string) error {
	adapter, err := r.deliveryAdapterFor(ctx, event.Source)
	if err != nil {
		return err
	}
	guildID, hasGuild, err := getGuildID(event)
	if err != nil {
		return err
	}
	if player, ok := adapter.(voiceChannelPlayer); ok && hasGuild && guildID != 0 && player.IsInVoiceChannel(guildID) {
		for _, path := range audioPaths {
			if err := player.PlayInVoiceChannel(ctx, guildID, path); err != nil {
				return err
			}
		}
		return nil
	}
	if adapter == nil {
		return nil
	}
	replyAnchor := gateway.ReplyAnchorForEvent(event)
	threadMeta := maps.Clone(r.threadMetadataForSource(event.Source, replyAnchor))
	if threadMeta == nil {
		threadMeta = map[string]any{}
	}
	threadMeta["notify"] = true
	anchor := ""
	if replyAnchor != nil {
		anchor = *replyAnchor
	}
	for _, path := range audioPaths {
		if _, err := adapter.SendVoice(ctx, event.Source.ChatID, path, "", anchor, threadMeta, nil); err != nil {
			return err
		}
	}
	return nil
}
