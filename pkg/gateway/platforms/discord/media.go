package discord

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	discordDefaultUploadLimitBytes = 20 * 1024 * 1024
	opusGranuleRate                = 48000
	discordImageBatchSize          = 10
	discordMediaDownloadTimeout    = 30 * time.Second
)

// Playback seconds of an Ogg Opus stream from its last granule minus the pre skip, false when not Ogg Opus
func oggOpusDuration(data []byte) (float64, bool) {
	if len(data) < 27 || string(data[:4]) != "OggS" {
		return 0, false
	}
	head := 27 + int(data[26])
	if len(data) < head+12 || string(data[head:head+8]) != "OpusHead" {
		return 0, false
	}
	preSkip := int64(binary.LittleEndian.Uint16(data[head+10 : head+12]))
	var granule int64
	found, pos := false, 0
	for pos+27 <= len(data) && string(data[pos:pos+4]) == "OggS" {
		bodyStart := pos + 27 + int(data[pos+26])
		if bodyStart > len(data) {
			break
		}
		if pageGranule := int64(binary.LittleEndian.Uint64(data[pos+6 : pos+14])); pageGranule != -1 {
			granule, found = pageGranule, true
		}
		sum := 0
		for _, b := range data[pos+27 : bodyStart] {
			sum += int(b)
		}
		pos = bodyStart + sum
	}
	if !found {
		return 0, false
	}
	secs := float64(granule-preSkip) / opusGranuleRate
	return secs, secs > 0
}

// Byte size of a file on disk, os.path.getsize
var getsize = func(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// File size cap discord.py reports for a guild boost tier
func guildFilesizeLimit(tier discordgo.PremiumTier) int {
	switch tier {
	case discordgo.PremiumTier2:
		return 52428800
	case discordgo.PremiumTier3:
		return 104857600
	}
	return 10485760
}

// Attachment cap of a channel, the guild boost limit floored at the platform default
func (a *DiscordAdapter) discordUploadLimitBytes(ch *discordgo.Channel) int {
	client := a.liveClient()
	if ch != nil && ch.GuildID != "" && client != nil && client.State != nil {
		if g, err := client.State.Guild(ch.GuildID); err == nil && g != nil {
			return max(guildFilesizeLimit(g.PremiumTier), discordDefaultUploadLimitBytes)
		}
	}
	return discordDefaultUploadLimitBytes
}

// Sends plain content to a channel with the client mention policy
func (a *DiscordAdapter) sendPlain(ctx context.Context, ch *discordgo.Channel, content string) error {
	client := a.liveClient()
	if client == nil {
		return errors.New("Not connected")
	}
	if _, err := client.ChannelMessageSendComplex(ch.ID, &discordgo.MessageSend{Content: content, AllowedMentions: a.allowedMentions}, discordgo.WithContext(ctx)); err != nil {
		return dpyError(err)
	}
	return nil
}

// Preflights a file against the channel cap, posting the size notice or the caption, nil when it may upload
func (a *DiscordAdapter) rejectOversizedUpload(ctx context.Context, ch *discordgo.Channel, path, filename, caption string) *gateway.SendResult {
	size, err := getsize(path)
	if err != nil {
		return &gateway.SendResult{Success: false, Error: fmt.Sprintf("Cannot stat file %s: %s", filename, pypath.OSErrorText(err))}
	}
	limit := a.discordUploadLimitBytes(ch)
	if size <= int64(limit) {
		return nil
	}
	sizeMB, limitMB := float64(size)/(1024*1024), float64(limit)/(1024*1024)
	msg := fmt.Sprintf("File too large for Discord upload: %s is %.1f MB (limit %.0f MB)", filename, sizeMB, limitMB)
	slog.Warn(fmt.Sprintf("[%s] %s", a.Name(), msg))
	notice := i18n.T(ctx, "platform.discord.media.upload_too_large", "", map[string]any{"filename": filename,
		"size_mb": fmt.Sprintf("%.1f", sizeMB), "limit_mb": fmt.Sprintf("%.0f", limitMB)})
	var sendErr error
	if a.WarningNotificationsEnabled(ctx, "", "", nil) {
		if !isForumParent(ch) {
			sendErr = a.sendPlain(ctx, ch, notice)
		}
	} else if caption != "" {
		if isForumParent(ch) {
			a.sendToForum(ctx, ch, caption)
		} else {
			sendErr = a.sendPlain(ctx, ch, caption)
		}
	}
	if sendErr != nil {
		slog.Debug(fmt.Sprintf("[%s] Failed to send oversized-file notice for %s", a.Name(), filename), "error", sendErr)
	}
	return &gateway.SendResult{Success: false, Error: msg}
}

// Uploads a local file as an attachment, a new thread post in a forum, failing loud when nothing attached
func (a *DiscordAdapter) sendFileAttachment(ctx context.Context, chatID, path, caption, fileName string, metadata map[string]any) (gateway.SendResult, error) {
	client := a.liveClient()
	if client == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return gateway.SendResult{Success: false, Error: "File not found: " + path}, nil
	}
	ch, err := a.resolveChannel(ctx, promptTargetID(chatID, metadata))
	if err != nil {
		return gateway.SendResult{}, err
	}
	filename := fileName
	if filename == "" {
		filename = pypath.Basename(path)
	}
	if rejected := a.rejectOversizedUpload(ctx, ch, path, filename, caption); rejected != nil {
		return *rejected, nil
	}
	_, ext := pypath.Splitext(filename)
	ext = pystr.Lower(ext)
	if ext == "" {
		ext = "no-ext"
	}
	slog.Info(fmt.Sprintf("[%s] Sending file attachment %s (%s) to %s", a.Name(), filename, ext, chatID))
	f, err := os.Open(path)
	if err != nil {
		return gateway.SendResult{}, err
	}
	defer f.Close()
	files := []*discordgo.File{{Name: filename, Reader: f}}
	if isForumParent(ch) {
		return a.forumPostFile(ctx, ch, "", pystr.Strip(caption), files), nil
	}
	msg, err := client.ChannelMessageSendComplex(ch.ID, &discordgo.MessageSend{Content: caption, Files: files, AllowedMentions: a.allowedMentions}, discordgo.WithContext(ctx))
	if err != nil {
		return gateway.SendResult{}, dpyError(err)
	}
	if len(msg.Attachments) == 0 {
		shown := msg.ID
		if shown == "" {
			shown = "?"
		}
		slog.Warn(fmt.Sprintf("[%s] Discord returned message %s with no attachments for %s", a.Name(), shown, filename))
		var id any
		if msg.ID != "" {
			id = msg.ID
		}
		return gateway.SendResult{Success: false, Error: fmt.Sprintf("Discord accepted the message but attached no files (%s)", filename), MessageID: id}, nil
	}
	return gateway.SendResult{Success: true, MessageID: msg.ID}, nil
}

// File attachment send whose failure is a failed delivery, a vanished file naming the kind's not found text
func (a *DiscordAdapter) sendLocalFile(ctx context.Context, chatID, path, caption, fileName, notFound, kind string, metadata map[string]any) gateway.SendResult {
	res, err := a.sendFileAttachment(ctx, chatID, path, caption, fileName, metadata)
	if err == nil {
		return res
	}
	if errors.Is(err, fs.ErrNotExist) {
		return gateway.SendResult{Success: false, Error: notFound + ": " + path}
	}
	slog.Error(fmt.Sprintf("[%s] Failed to send %s: %s", a.Name(), kind, err))
	return gateway.SendResult{Success: false, Error: err.Error()}
}

// Local image file as a native attachment
func (a *DiscordAdapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendLocalFile(ctx, chatID, imagePath, caption, "", "Image file not found", "local image", metadata), nil
}

// Local video file as a native attachment
func (a *DiscordAdapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendLocalFile(ctx, chatID, videoPath, caption, "", "Video file not found", "local video", metadata), nil
}

// Arbitrary file as a native attachment under the caller's file name
func (a *DiscordAdapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendLocalFile(ctx, chatID, filePath, caption, fileName, "File not found", "document", metadata), nil
}

// HTTP client for media downloads through the Discord proxy
func discordMediaClient(ctx context.Context) (*http.Client, error) {
	proxyURL, err := gateway.ResolveProxyURL(ctx, "DISCORD_PROXY", nil, "")
	if err != nil {
		return nil, err
	}
	session, _, err := gateway.ProxyKwargsForAiohttp(proxyURL)
	if err != nil {
		return nil, err
	}
	return standaloneClient(session, discordMediaDownloadTimeout), nil
}

// Images as one message per ten attachments, downloads inlined, a failed chunk retried per image
func (a *DiscordAdapter) SendMultipleImages(ctx context.Context, chatID string, images []gateway.ImageRef, metadata map[string]any, humanDelay float64) (gateway.SendResult, error) {
	if a.liveClient() == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	if len(images) == 0 {
		return gateway.SendResult{Success: false, Error: "no images to send"}, nil
	}
	ch, err := a.resolveChannel(ctx, promptTargetID(chatID, metadata))
	if err != nil {
		slog.Warn(fmt.Sprintf("[%s] Failed to resolve channel for multi-image send: %s", a.Name(), err))
		return a.BasePlatformAdapter.SendMultipleImages(ctx, chatID, images, metadata, humanDelay)
	}
	var chunks [][]gateway.ImageRef
	for i := 0; i < len(images); i += discordImageBatchSize {
		chunks = append(chunks, images[i:min(i+discordImageBatchSize, len(images))])
	}
	delivered := false
	for idx, chunk := range chunks {
		if humanDelay > 0 && idx > 0 {
			select {
			case <-ctx.Done():
				return gateway.SendResult{}, ctx.Err()
			case <-time.After(time.Duration(humanDelay * float64(time.Second))):
			}
		}
		sent, err := a.sendImageChunk(ctx, ch, chunk, idx, len(chunks))
		if err != nil {
			slog.Warn(fmt.Sprintf("[%s] Multi-image Discord send failed (chunk %d/%d), falling back to per-image: %s", a.Name(), idx+1, len(chunks), err))
			fallback, ferr := a.BasePlatformAdapter.SendMultipleImages(ctx, chatID, chunk, metadata, humanDelay)
			if ferr != nil {
				return gateway.SendResult{}, ferr
			}
			delivered = delivered || fallback.Success
			continue
		}
		delivered = delivered || sent
	}
	if delivered {
		return gateway.SendResult{Success: true}, nil
	}
	return gateway.SendResult{Success: false, Error: "all images failed to send"}, nil
}

// One message of up to ten images, skipped entries noted, true when the message went out
func (a *DiscordAdapter) sendImageChunk(ctx context.Context, ch *discordgo.Channel, chunk []gateway.ImageRef, idx, total int) (bool, error) {
	var files []*discordgo.File
	var captions, skipNotices []string
	var httpClient *http.Client
	env := &tools.Env{Home: home.Dir(ctx)}
	for _, img := range chunk {
		if img.Alt != "" {
			captions = append(captions, img.Alt)
		}
		if strings.HasPrefix(img.URL, "file://") {
			localPath := pyurl.Unquote(img.URL[7:])
			if _, err := os.Stat(localPath); err != nil {
				slog.Warn(fmt.Sprintf("[%s] Skipping missing image: %s", a.Name(), localPath))
				continue
			}
			size, err := getsize(localPath)
			if err != nil {
				slog.Warn(fmt.Sprintf("[%s] Skipping unreadable image %s: %s", a.Name(), localPath, pypath.OSErrorText(err)))
				continue
			}
			if limit := a.discordUploadLimitBytes(ch); size > int64(limit) {
				sizeMB, limitMB := float64(size)/(1024*1024), float64(limit)/(1024*1024)
				slog.Warn(fmt.Sprintf("[%s] Skipping oversized image in batch: %s is %.1f MB (limit %.0f MB)", a.Name(), pypath.Basename(localPath), sizeMB, limitMB))
				skipNotices = append(skipNotices, i18n.T(ctx, "platform.discord.media.image_skipped_too_large", "", map[string]any{
					"filename": pypath.Basename(localPath), "size_mb": fmt.Sprintf("%.1f", sizeMB), "limit_mb": fmt.Sprintf("%.0f", limitMB)}))
				continue
			}
			data, err := os.ReadFile(localPath)
			if err != nil {
				return false, err
			}
			files = append(files, &discordgo.File{Name: pypath.Basename(localPath), Reader: bytes.NewReader(data)})
			continue
		}
		if !isSafeURL(env, img.URL) {
			slog.Warn(fmt.Sprintf("[%s] Blocked unsafe image URL in batch", a.Name()))
			continue
		}
		data, ext, err := a.downloadBatchImage(ctx, env, &httpClient, img.URL)
		if err != nil {
			slog.Warn(fmt.Sprintf("[%s] Download failed for %s: %s", a.Name(), cut(img.URL, 80), err))
			continue
		}
		if data == nil {
			continue
		}
		files = append(files, &discordgo.File{Name: fmt.Sprintf("image_%d.%s", len(files), ext), Reader: bytes.NewReader(data)})
	}
	if len(files) == 0 {
		shown := a.WarningNotificationsEnabled(ctx, "", "", nil)
		if !shown && len(captions) > 0 {
			if isForumParent(ch) {
				a.sendToForum(ctx, ch, captions[0])
			} else if err := a.sendPlain(ctx, ch, captions[0]); err != nil {
				return false, err
			}
		}
		if len(skipNotices) > 0 && shown && !isForumParent(ch) {
			if err := a.sendPlain(ctx, ch, strings.Join(skipNotices, "\n")); err != nil {
				slog.Debug(fmt.Sprintf("[%s] Failed to send oversized-image notices", a.Name()), "error", err)
			}
		}
		return false, nil
	}
	content := ""
	if len(captions) > 0 {
		content = captions[0]
	}
	if len(skipNotices) > 0 {
		var lines []string
		var hidden *string
		if content != "" {
			lines, hidden = []string{content}, &content
		}
		content = ""
		if w := a.WarningText(ctx, strings.Join(append(lines, skipNotices...), "\n"), hidden, "", "", nil); w != nil {
			content = *w
		}
	}
	slog.Info(fmt.Sprintf("[%s] Sending %d image(s) as single Discord message (chunk %d/%d)", a.Name(), len(files), idx+1, total))
	if isForumParent(ch) {
		a.forumPostFile(ctx, ch, "", pystr.Strip(content), files)
		return true, nil
	}
	client := a.liveClient()
	if client == nil {
		return false, errors.New("Not connected")
	}
	if _, err := client.ChannelMessageSendComplex(ch.ID, &discordgo.MessageSend{Content: content, Files: files, AllowedMentions: a.allowedMentions}, discordgo.WithContext(ctx)); err != nil {
		return false, dpyError(err)
	}
	return true, nil
}

// Downloads one batch image through the redirect guard, nil data after a logged non 200 status
func (a *DiscordAdapter) downloadBatchImage(ctx context.Context, env *tools.Env, httpClient **http.Client, rawURL string) ([]byte, string, error) {
	if *httpClient == nil {
		c, err := discordMediaClient(ctx)
		if err != nil {
			return nil, "", err
		}
		*httpClient = c
	}
	status, data, headers, err := readURLImageWithRedirectGuard(ctx, env, *httpClient, rawURL, discordMediaDownloadTimeout, nil)
	if err != nil {
		return nil, "", err
	}
	if status != 200 {
		slog.Warn(fmt.Sprintf("[%s] Failed to download image (HTTP %d) in batch: %s", a.Name(), status, cut(rawURL, 80)))
		return nil, "", nil
	}
	contentType, ok := headers["content-type"]
	if !ok {
		contentType = "image/png"
	}
	return data, imageExtFromContentType(contentType), nil
}

// Audio as a native Ogg Opus voice message, a plain attachment when the flag says it is not a voice note
func (a *DiscordAdapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	if isVoice, ok := extra["is_voice"].(bool); ok && !isVoice {
		return a.sendLocalFile(ctx, chatID, audioPath, caption, "", "Audio file not found", "audio", metadata), nil
	}
	res, err := a.sendVoiceMessage(ctx, chatID, audioPath, caption, replyTo, metadata)
	if err != nil {
		slog.Error(fmt.Sprintf("[%s] Failed to send audio: %s", a.Name(), err))
		return gateway.SendResult{Success: false, Error: err.Error()}, nil
	}
	return res, nil
}

// Voice message body, a forum thread post or a raw voice flagged upload falling back to an audio file
func (a *DiscordAdapter) sendVoiceMessage(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	ch, err := a.resolveChannel(ctx, promptTargetID(chatID, metadata))
	if err != nil {
		return gateway.SendResult{}, err
	}
	if _, err := os.Stat(audioPath); err != nil {
		return gateway.SendResult{Success: false, Error: "Audio file not found: " + audioPath}, nil
	}
	filename := pypath.Basename(audioPath)
	if rejected := a.rejectOversizedUpload(ctx, ch, audioPath, filename, caption); rejected != nil {
		return *rejected, nil
	}
	reference := a.replyReferenceForSend(replyTo, ch)
	if isForumParent(ch) {
		data, err := os.ReadFile(audioPath)
		if err != nil {
			return gateway.SendResult{}, err
		}
		return a.forumPostFile(ctx, ch, "", pystr.Strip(caption), []*discordgo.File{{Name: filename, Reader: bytes.NewReader(data)}}), nil
	}
	voice, duration, ok, err := oggOpusVoicePayload(ctx, audioPath)
	if err != nil {
		return gateway.SendResult{}, err
	}
	if ok {
		res, verr := a.postVoiceMessage(ctx, ch, voice, duration, replyTo, reference)
		if verr == nil {
			return res, nil
		}
		slog.Debug(fmt.Sprintf("Voice message flag failed, falling back to file: %s", verr))
	} else {
		slog.Warn(fmt.Sprintf("[%s] %s is not Ogg/Opus and could not be transcoded (install ffmpeg for voice messages); sending it as an audio attachment", a.Name(), filename))
	}
	data, err := os.ReadFile(audioPath)
	if err != nil {
		return gateway.SendResult{}, err
	}
	client := a.liveClient()
	if client == nil {
		return gateway.SendResult{}, errors.New("Not connected")
	}
	send := func(ref *discordgo.MessageReference) (*discordgo.Message, error) {
		return client.ChannelMessageSendComplex(ch.ID, &discordgo.MessageSend{Files: []*discordgo.File{{Name: filename, Reader: bytes.NewReader(data)}},
			Reference: ref, AllowedMentions: a.allowedMentions}, discordgo.WithContext(ctx))
	}
	msg, err := send(reference)
	if err != nil {
		err = dpyError(err)
		if reference == nil || !isReplyReferenceRejected(err) {
			return gateway.SendResult{}, err
		}
		if msg, err = send(nil); err != nil {
			return gateway.SendResult{}, dpyError(err)
		}
	}
	return gateway.SendResult{Success: true, MessageID: msg.ID}, nil
}

// Transcoder of non Opus audio for voice messages
var transcodeToOggOpus = gateway.TranscodeToOggOpus

// Ogg Opus bytes and duration of a voice message, the file itself or an ffmpeg transcode, false when neither is Ogg Opus
func oggOpusVoicePayload(ctx context.Context, audioPath string) ([]byte, float64, bool, error) {
	data, err := os.ReadFile(audioPath)
	if err != nil {
		return nil, 0, false, err
	}
	if d, ok := oggOpusDuration(data); ok {
		return data, d, true, nil
	}
	converted, err := transcodeToOggOpus(ctx, audioPath, "48k", 30*time.Second, "")
	if err != nil || converted == "" {
		return nil, 0, false, nil
	}
	defer os.Remove(converted)
	data, err = os.ReadFile(converted)
	if err != nil {
		return nil, 0, false, err
	}
	d, ok := oggOpusDuration(data)
	return data, d, ok, nil
}

// Posts a native voice message, flags 8192 with the Ogg Opus upload and a flat waveform, through the raw API
func (a *DiscordAdapter) postVoiceMessage(ctx context.Context, ch *discordgo.Channel, data []byte, duration float64, replyTo string, reference *discordgo.MessageReference) (gateway.SendResult, error) {
	client := a.liveClient()
	if client == nil {
		return gateway.SendResult{}, errors.New("Not connected")
	}
	rounded, _ := strconv.ParseFloat(strconv.FormatFloat(duration, 'f', 2, 64), 64)
	payload := map[string]any{"flags": 8192, "attachments": []map[string]any{{"id": "0", "filename": "voice-message.ogg", "duration_secs": rounded,
		"waveform": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{128}, 256))}}}
	if reference != nil {
		payload["message_reference"] = map[string]any{"message_id": replyTo, "fail_if_not_exists": false}
	}
	contentType, body, err := discordgo.MultipartBodyWithJSON(payload, []*discordgo.File{{Name: "voice-message.ogg", ContentType: "audio/ogg", Reader: bytes.NewReader(data)}})
	if err != nil {
		return gateway.SendResult{}, err
	}
	endpoint := discordgo.EndpointChannelMessages(ch.ID)
	raw, err := client.RequestRaw(http.MethodPost, endpoint, contentType, body, endpoint, 0, discordgo.WithContext(ctx))
	if err != nil {
		return gateway.SendResult{}, dpyError(err)
	}
	var msg struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return gateway.SendResult{}, err
	}
	id := strings.Trim(string(msg.ID), `"`)
	return gateway.SendResult{Success: true, MessageID: id}, nil
}

// Downloads a media URL and posts it as an inline attachment, the base URL send on any failure
func (a *DiscordAdapter) sendURLMedia(ctx context.Context, chatID, rawURL, caption, kind string, filenameFor func(headers map[string]string) string,
	fallback func(md map[string]any) (gateway.SendResult, error), metadata, errorMetadata map[string]any) (gateway.SendResult, error) {
	if a.liveClient() == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	if !isSafeURL(&tools.Env{Home: home.Dir(ctx)}, rawURL) {
		slog.Warn(fmt.Sprintf("[%s] Blocked unsafe %s URL during Discord send_%s", a.Name(), kind, kind))
		return fallback(metadata)
	}
	res, err := a.uploadURLMedia(ctx, chatID, rawURL, caption, kind, filenameFor, metadata)
	if err != nil {
		slog.Error(fmt.Sprintf("[%s] Failed to send %s attachment, falling back to URL: %s", a.Name(), kind, err))
		return fallback(errorMetadata)
	}
	return res, nil
}

// Body of the URL media send whose errors the caller turns into the URL fallback
func (a *DiscordAdapter) uploadURLMedia(ctx context.Context, chatID, rawURL, caption, kind string, filenameFor func(headers map[string]string) string, metadata map[string]any) (gateway.SendResult, error) {
	ch, err := a.resolveChannel(ctx, promptTargetID(chatID, metadata))
	if err != nil {
		return gateway.SendResult{}, err
	}
	httpClient, err := discordMediaClient(ctx)
	if err != nil {
		return gateway.SendResult{}, err
	}
	status, data, headers, err := readURLImageWithRedirectGuard(ctx, &tools.Env{Home: home.Dir(ctx)}, httpClient, rawURL, discordMediaDownloadTimeout, nil)
	if err != nil {
		return gateway.SendResult{}, err
	}
	if status != 200 {
		return gateway.SendResult{}, fmt.Errorf("Failed to download %s: HTTP %d", kind, status)
	}
	files := []*discordgo.File{{Name: filenameFor(headers), Reader: bytes.NewReader(data)}}
	if isForumParent(ch) {
		return a.forumPostFile(ctx, ch, "", pystr.Strip(caption), files), nil
	}
	client := a.liveClient()
	if client == nil {
		return gateway.SendResult{}, errors.New("Not connected")
	}
	msg, err := client.ChannelMessageSendComplex(ch.ID, &discordgo.MessageSend{Content: caption, Files: files, AllowedMentions: a.allowedMentions}, discordgo.WithContext(ctx))
	if err != nil {
		return gateway.SendResult{}, dpyError(err)
	}
	return gateway.SendResult{Success: true, MessageID: msg.ID}, nil
}

// Image URL downloaded and posted as an inline attachment
func (a *DiscordAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	return a.sendURLMedia(ctx, chatID, imageURL, caption, "image", func(h map[string]string) string {
		contentType, ok := h["content-type"]
		if !ok {
			contentType = "image/png"
		}
		return "image." + imageExtFromContentType(contentType)
	}, func(md map[string]any) (gateway.SendResult, error) {
		return a.BasePlatformAdapter.SendImage(ctx, chatID, imageURL, caption, replyTo, md)
	}, metadata, metadata)
}

// Animated GIF URL downloaded and posted as an inline attachment
func (a *DiscordAdapter) SendAnimation(ctx context.Context, chatID, animationURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	return a.sendURLMedia(ctx, chatID, animationURL, caption, "animation", func(map[string]string) string { return "animation.gif" },
		func(md map[string]any) (gateway.SendResult, error) {
			return a.BasePlatformAdapter.SendAnimation(ctx, chatID, animationURL, caption, replyTo, md)
		}, metadata, metadata)
}
