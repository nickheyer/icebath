package discord

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	mentionTokenRE  = regexp.MustCompile(`<@[!&]?\d+>`)
	channelTokenRE  = regexp.MustCompile(`<#\d+>`)
	whitespaceRunRE = regexp.MustCompile(`\s+`)
)

// Media cache and SSRF helpers the attachment paths call, the base adapter functions
var (
	cacheImageFromBytesAsync = gateway.CacheImageFromBytesAsync
	cacheImageFromURL        = gateway.CacheImageFromURL
	cacheAudioFromBytesAsync = gateway.CacheAudioFromBytesAsync
	cacheAudioFromURL        = gateway.CacheAudioFromURL
	isSafeURL                = tools.IsSafeURL
)

// Shutdown flush deadline for pending text batches, kept below the per adapter disconnect budget
func textBatchFlushDeadlineSeconds() float64 {
	budget := 5.0
	if raw := pystr.Strip(os.Getenv("ICEBATH_GATEWAY_ADAPTER_DISCONNECT_TIMEOUT")); raw != "" {
		if parsed, err := pyint.ParseFloat(raw); err == nil && parsed > 0 {
			budget = parsed
		}
	}
	headroom := math.Max(0.5, budget*0.2)
	deadline := math.Max(1.0, budget-headroom)
	return math.Min(deadline, budget*0.9)
}

// Provisional auto thread name with mention tokens stripped, cut to 80 characters
func deriveAutoThreadName(ctx context.Context, content string) string {
	content = pystr.Strip(content)
	content = mentionTokenRE.ReplaceAllString(content, "")
	content = channelTokenRE.ReplaceAllString(content, "")
	content = pystr.Strip(whitespaceRunRE.ReplaceAllString(content, " "))
	if content == "" {
		return i18n.T(ctx, "platform.discord.thread.default_name", "", nil)
	}
	name := cut(content, 80)
	if utf8.RuneCountInString(content) > 80 {
		name = cut(name, 77) + "..."
	}
	return name
}

// Attachment bytes through the bot's HTTP session, nil when unreadable, an error when oversized
func (a *DiscordAdapter) readAttachmentBytes(ctx context.Context, att *discordgo.MessageAttachment, mediaType string) ([]byte, error) {
	h := home.Dir(ctx)
	if att.Size != 0 {
		if err := mediacache.ValidateInboundMediaSize(ctx, h, int64(att.Size), mediaType, nil); err != nil {
			return nil, err
		}
	}
	client := a.liveClient()
	if client == nil {
		return nil, nil
	}
	raw, err := func() ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, att.URL, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
		}
		return io.ReadAll(resp.Body)
	}()
	if err != nil {
		name := att.Filename
		if name == "" {
			name = att.URL
		}
		slog.Warn(fmt.Sprintf("[Discord] Authenticated attachment read failed for %s: %s", name, err))
		return nil, nil
	}
	if err := mediacache.ValidateInboundMediaSize(ctx, h, int64(len(raw)), mediaType, nil); err != nil {
		return nil, err
	}
	return raw, nil
}

// Image attachment cached locally, the authenticated read first and the guarded URL download after
func (a *DiscordAdapter) cacheDiscordImage(ctx context.Context, att *discordgo.MessageAttachment, ext string) (string, error) {
	raw, err := a.readAttachmentBytes(ctx, att, "image")
	if err != nil {
		return "", err
	}
	if raw != nil {
		path, err := cacheImageFromBytesAsync(ctx, raw, ext)
		if err == nil {
			return path, nil
		}
		slog.Debug(fmt.Sprintf("[Discord] cache_image_from_bytes rejected att.read() data; falling back to URL: %s", err))
	}
	return cacheImageFromURL(ctx, att.URL, ext, 2)
}

// Audio attachment cached locally, the authenticated read first and the guarded URL download after
func (a *DiscordAdapter) cacheDiscordAudio(ctx context.Context, att *discordgo.MessageAttachment, ext string) (string, error) {
	raw, err := a.readAttachmentBytes(ctx, att, "audio")
	if err != nil {
		return "", err
	}
	if raw != nil {
		path, err := cacheAudioFromBytesAsync(ctx, raw, ext)
		if err == nil {
			return path, nil
		}
		slog.Debug(fmt.Sprintf("[Discord] cache_audio_from_bytes failed; falling back to URL: %s", err))
	}
	return cacheAudioFromURL(ctx, att.URL, ext, 2)
}

// Document attachment bytes, the authenticated read first and an SSRF guarded download after
func (a *DiscordAdapter) cacheDiscordDocument(ctx context.Context, att *discordgo.MessageAttachment) ([]byte, error) {
	raw, err := a.readAttachmentBytes(ctx, att, "document")
	if err != nil || raw != nil {
		return raw, err
	}
	if !isSafeURL(&tools.Env{Home: home.Dir(ctx)}, att.URL) {
		return nil, fmt.Errorf("Blocked unsafe attachment URL (SSRF protection): %s", att.URL)
	}
	proxyURL, err := gateway.ResolveProxyURL(ctx, "DISCORD_PROXY", nil, "")
	if err != nil {
		return nil, err
	}
	session, _, err := gateway.ProxyKwargsForAiohttp(proxyURL)
	if err != nil {
		return nil, err
	}
	resp, err := standaloneDo(ctx, standaloneClient(session, 30*time.Second), http.MethodGet, att.URL, http.Header{}, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// Image or audio attachment cached locally, the CDN URL when caching fails
func (a *DiscordAdapter) cacheSimpleMedia(ctx context.Context, att *discordgo.MessageAttachment, contentType, kind string, exts map[string]bool, defaultExt string) string {
	parts := strings.Split(contentType, "/")
	ext := "." + strings.Split(parts[len(parts)-1], ";")[0]
	if !exts[ext] {
		ext = defaultExt
	}
	cacher := a.cacheDiscordAudio
	if kind == "image" {
		cacher = a.cacheDiscordImage
	}
	cached, err := cacher(ctx, att, ext)
	if err != nil {
		fmt.Printf("[Discord] Failed to cache %s attachment: %s\n", kind, err)
		return att.URL
	}
	fmt.Printf("[Discord] Cached user %s: %s\n", kind, cached)
	return cached
}
