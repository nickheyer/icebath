package telegram

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypil"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Routing fields of a media send applied to the SDK params
type mediaRouting struct {
	ChatID              any
	MessageThreadID     int
	DirectMessagesTopic int
	ReplyParameters     *models.ReplyParameters
	DisableNotification bool
}

// Routing of the media kwargs in SDK field form
func routingOf(kw mediaSendKwargs) mediaRouting {
	r := mediaRouting{ChatID: kw.ChatID, DisableNotification: kw.DisableNotification}
	if kw.Thread.MessageThreadID != nil {
		r.MessageThreadID = *kw.Thread.MessageThreadID
	}
	if kw.Thread.DirectMessagesTopicID != nil {
		r.DirectMessagesTopic = *kw.Thread.DirectMessagesTopicID
	}
	if kw.ReplyToMessageID != nil {
		r.ReplyParameters = &models.ReplyParameters{MessageID: *kw.ReplyToMessageID}
	}
	return r
}

// Context carrying the media read timeout and, for uploads, the media write timeout
func mediaCtx(ctx context.Context, kw mediaSendKwargs, upload bool) context.Context {
	read := kw.ReadTimeout
	return withRequestTimeouts(ctx, &read, upload)
}

// Upload of a file's bytes read fresh on each attempt so a retry resends the whole payload
func uploadOf(path string) (models.InputFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return &models.InputFileUpload{Filename: filepath.Base(path), Data: bytes.NewReader(data)}, nil
}

// File not found error of MEDIA delivery naming the sandbox bind mount fix for container paths
func missingMediaPathError(label, path string) string {
	msg := fmt.Sprintf("%s file not found: %s", label, path)
	for _, prefix := range []string{"/workspace/", "/output/", "/outputs/"} {
		if strings.HasPrefix(path, prefix) {
			msg += " (path may only exist inside the Docker sandbox. " +
				"Bind-mount a host directory and emit the host-visible path in MEDIA: for gateway file delivery.)"
			break
		}
	}
	return msg
}

// Convertible raster format of a file by magic bytes, empty for GIF and unknown formats
func sniffRasterFormat(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 16)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	switch {
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return "png"
	case bytes.HasPrefix(head, []byte("GIF87a")) || bytes.HasPrefix(head, []byte("GIF89a")):
		return ""
	case bytes.HasPrefix(head, []byte("RIFF")) && len(head) >= 12 && string(head[8:12]) == "WEBP":
		return "webp"
	case bytes.HasPrefix(head, []byte("BM")):
		return "bmp"
	case bytes.HasPrefix(head, []byte("II*\x00")) || bytes.HasPrefix(head, []byte("MM\x00*")):
		return "tiff"
	}
	return ""
}

// Pre-compresses a large raster image to a JPEG temp file under the upload timeout, empty when the original is used
func (a *TelegramAdapter) compressImageToJPEG(imagePath string) string {
	info, err := os.Stat(imagePath)
	if err != nil {
		return ""
	}
	ext := strings.ToLower(filepath.Ext(imagePath))
	if ext == ".jpg" || ext == ".jpeg" || info.Size() < imgCompressThresholdBytes || sniffRasterFormat(imagePath) == "" {
		return ""
	}
	tmp, err := a.writeCompressedJPEG(imagePath)
	if err != nil {
		slog.Warn(fmt.Sprintf("[%s] Image compression failed, uploading original: %s", a.Name(), pyrepr.ExceptionMessage(err)))
		return ""
	}
	out, _ := os.Stat(tmp)
	var outSize int64
	if out != nil {
		outSize = out.Size()
	}
	slog.Info(fmt.Sprintf("[%s] Pre-compressed %s (%.1fKB → %s %.1fKB) for Telegram upload", a.Name(), imagePath, float64(info.Size())/1024, tmp,
		float64(outSize)/1024))
	return tmp
}

// Decodes, flattens alpha onto white, resizes past the dimension cap and writes the JPEG temp file
func (a *TelegramAdapter) writeCompressedJPEG(imagePath string) (string, error) {
	f, err := os.Open(imagePath)
	if err != nil {
		return "", err
	}
	src, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		return "", err
	}
	bounds := src.Bounds()
	rgb := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(rgb, rgb.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(rgb, rgb.Bounds(), src, bounds.Min, draw.Over)
	img := rgb
	w, h := bounds.Dx(), bounds.Dy()
	if maxDim := max(w, h); maxDim > imgMaxDimension {
		scale := float64(imgMaxDimension) / float64(maxDim)
		img = pypil.ResizeLanczos(rgb, int(float64(w)*scale), int(float64(h)*scale))
	}
	out, err := os.CreateTemp("", "tg_compress_*.jpg")
	if err != nil {
		return "", err
	}
	if err := jpeg.Encode(out, img, &jpeg.Options{Quality: imgJPEGQuality}); err != nil {
		out.Close()
		os.Remove(out.Name())
		return "", err
	}
	if err := out.Close(); err != nil {
		os.Remove(out.Name())
		return "", err
	}
	return out.Name(), nil
}

// Message id text of a sent message
func sentID(msg *models.Message) string { return strconv.Itoa(msg.ID) }

// sendVoice with the MarkdownV2 caption when it fits and a plain caption when the entities are rejected
func (a *TelegramAdapter) sendVoiceBubble(ctx context.Context, audioPath, chatID string, replyTo *string, metadata map[string]any, caption string,
	duration *int) (*models.Message, error) {
	b := a.currentBot()
	type variant struct {
		text string
		mode models.ParseMode
		set  bool
	}
	var variants []variant
	if caption != "" {
		if formatted := a.FormatMessage(caption); gateway.UTF16Len(formatted) <= 1024 {
			variants = append(variants, variant{formatted, models.ParseModeMarkdown, true})
		}
		variants = append(variants, variant{clipRunes(caption, 1024), "", true})
	} else {
		variants = append(variants, variant{})
	}
	var lastParseErr error
	for _, v := range variants {
		msg, err := sendMedia(ctx, a.pacer, a.Name(), func(c context.Context, kw mediaSendKwargs) (*models.Message, error) {
			file, ferr := uploadOf(audioPath)
			if ferr != nil {
				return nil, ferr
			}
			r := routingOf(kw)
			params := &bot.SendVoiceParams{ChatID: r.ChatID, MessageThreadID: r.MessageThreadID, DirectMessagesTopicID: r.DirectMessagesTopic,
				Voice: file, Caption: v.text, ParseMode: v.mode, ReplyParameters: r.ReplyParameters, DisableNotification: r.DisableNotification}
			if duration != nil {
				params.Duration = *duration
			}
			m, sendErr := b.SendVoice(mediaCtx(c, kw, true), params)
			return m, ptbError(sendErr)
		}, chatID, replyTo, metadata, a.replyToMode, a.notificationsMode, "voice", nil)
		if err == nil {
			return msg, nil
		}
		low := errLower(err)
		if v.mode != "" && (strings.Contains(low, "parse") || strings.Contains(low, "entit")) {
			slog.Warn(fmt.Sprintf("[%s] voice caption MarkdownV2 rejected, retrying plain: %s", a.Name(), redactTelegramErrorText(err)))
			lastParseErr = err
			continue
		}
		return nil, err
	}
	if lastParseErr != nil {
		return nil, lastParseErr
	}
	return nil, &runtimeError{msg: "Telegram send_voice failed for all caption variants"}
}

// Sends audio as a native voice bubble, an audio file or a document by format, transcoding explicit voice requests
func (a *TelegramAdapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	b := a.currentBot()
	if b == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	if _, err := os.Stat(audioPath); err != nil {
		return gateway.SendResult{Success: false, Error: missingMediaPathError("Audio", audioPath)}, nil
	}
	path := audioPath
	transcoded := ""
	defer func() {
		if transcoded != "" {
			os.Remove(transcoded)
		}
	}()
	ext := strings.ToLower(filepath.Ext(path))
	if pyTruthy(extra["is_voice"]) && ext != ".ogg" && ext != ".opus" {
		out, _ := gateway.TranscodeToOggOpus(ctx, path, "32k", 60*time.Second, "")
		if out != "" {
			transcoded, path = out, out
		} else {
			slog.Warn(fmt.Sprintf("[%s] voice transcode unavailable for %s — sending original format (install ffmpeg for voice bubbles)", a.Name(),
				filepath.Base(path)))
		}
	}
	var duration *int
	if d, ok := ProbeVoiceDurationSeconds(ctx, path); ok {
		duration = &d
	}
	var msg *models.Message
	var err error
	switch ext = strings.ToLower(filepath.Ext(path)); ext {
	case ".ogg", ".opus":
		msg, err = a.sendVoiceBubble(ctx, path, chatID, optional(replyTo), metadata, caption, duration)
	case ".mp3", ".m4a":
		msg, err = sendMedia(ctx, a.pacer, a.Name(), func(c context.Context, kw mediaSendKwargs) (*models.Message, error) {
			file, ferr := uploadOf(path)
			if ferr != nil {
				return nil, ferr
			}
			r := routingOf(kw)
			params := &bot.SendAudioParams{ChatID: r.ChatID, MessageThreadID: r.MessageThreadID, DirectMessagesTopicID: r.DirectMessagesTopic, Audio: file,
				ReplyParameters: r.ReplyParameters, DisableNotification: r.DisableNotification}
			if c1024 := caption1024(caption); c1024 != nil {
				params.Caption = *c1024
			}
			if duration != nil {
				params.Duration = *duration
			}
			m, sendErr := b.SendAudio(mediaCtx(c, kw, true), params)
			return m, ptbError(sendErr)
		}, chatID, optional(replyTo), metadata, a.replyToMode, a.notificationsMode, "audio", nil)
	default:
		return a.SendDocument(ctx, chatID, path, caption, "", replyTo, metadata, nil)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return gateway.SendResult{}, err
		}
		slog.Error(fmt.Sprintf("[%s] Failed to send Telegram voice/audio, falling back to base adapter: %s", a.Name(), redactTelegramErrorText(err)))
		return a.BasePlatformAdapter.SendVoice(ctx, chatID, audioPath, caption, replyTo, metadata, nil)
	}
	return gateway.SendResult{Success: true, MessageID: sentID(msg)}, nil
}

// Sends images as albums of ten, animations and failed albums going through the base per image path
func (a *TelegramAdapter) SendMultipleImages(ctx context.Context, chatID string, images []gateway.ImageRef, metadata map[string]any,
	humanDelay float64) (gateway.SendResult, error) {
	b := a.currentBot()
	if b == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	if len(images) == 0 {
		return gateway.SendResult{Success: false, Error: "no images to send"}, nil
	}
	isAnim := func(u string) bool { return !strings.HasPrefix(u, "file://") && gateway.IsAnimationURL(u) }
	var animations, photos []gateway.ImageRef
	for _, img := range images {
		if isAnim(img.URL) {
			animations = append(animations, img)
		} else {
			photos = append(photos, img)
		}
	}
	delivered := false
	if len(animations) > 0 {
		res, err := a.BasePlatformAdapter.SendMultipleImages(ctx, chatID, animations, metadata, humanDelay)
		if err != nil {
			return res, err
		}
		delivered = res.Success
	}
	failure := func() gateway.SendResult {
		if delivered {
			return gateway.SendResult{Success: true}
		}
		return gateway.SendResult{Success: false, Error: "all images failed to send"}
	}
	if len(photos) == 0 {
		return failure(), nil
	}
	const chunkSize = 10
	var chunks [][]gateway.ImageRef
	for i := 0; i < len(photos); i += chunkSize {
		chunks = append(chunks, photos[i:min(i+chunkSize, len(photos))])
	}
	for idx, chunk := range chunks {
		if humanDelay > 0 && idx > 0 && !sleepCtx(ctx, humanDelay) {
			return gateway.SendResult{}, ctx.Err()
		}
		sent, err := a.sendMediaGroupChunk(ctx, chatID, chunk, metadata, idx, len(chunks))
		if err != nil {
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				return gateway.SendResult{}, err
			}
			slog.Warn(fmt.Sprintf("[%s] send_media_group failed (chunk %d/%d), falling back to per-image: %s", a.Name(), idx+1, len(chunks),
				redactTelegramErrorText(err)))
			fallback, ferr := a.BasePlatformAdapter.SendMultipleImages(ctx, chatID, chunk, metadata, humanDelay)
			if ferr != nil {
				return fallback, ferr
			}
			delivered = delivered || fallback.Success
			continue
		}
		delivered = delivered || sent
	}
	return failure(), nil
}

// Sends one album chunk, compressing large local images and removing the temp JPEGs after
func (a *TelegramAdapter) sendMediaGroupChunk(ctx context.Context, chatID string, chunk []gateway.ImageRef, metadata map[string]any, idx, total int) (bool, error) {
	b := a.currentBot()
	type photo struct {
		source  string
		local   string
		caption *string
	}
	var media []photo
	var temps []string
	defer func() {
		for _, t := range temps {
			os.Remove(t)
		}
	}()
	for _, img := range chunk {
		p := photo{source: img.URL, caption: caption1024(img.Alt)}
		if strings.HasPrefix(img.URL, "file://") {
			local, err := url.PathUnescape(img.URL[7:])
			if err != nil {
				local = img.URL[7:]
			}
			if _, statErr := os.Stat(local); statErr != nil {
				slog.Warn(fmt.Sprintf("[%s] Skipping missing image in media group: %s", a.Name(), local))
				continue
			}
			if compressed := a.compressImageToJPEG(local); compressed != "" {
				temps = append(temps, compressed)
				local = compressed
			}
			p.local = local
		}
		media = append(media, p)
	}
	if len(media) == 0 {
		return false, nil
	}
	slog.Info(fmt.Sprintf("[%s] Sending media group of %d photo(s) (chunk %d/%d)", a.Name(), len(media), idx+1, total))
	replyToID, kw, err := buildMediaSendKwargs(chatID, nil, metadata, a.replyToMode, a.notificationsMode)
	if err != nil {
		return false, err
	}
	_, err = sendWithDMTopicReplyAnchorRetry(ctx, a.pacer, a.Name(), func(c context.Context, kw mediaSendKwargs) ([]*models.Message, error) {
		inputs := make([]models.InputMedia, 0, len(media))
		upload := false
		for i, p := range media {
			item := &models.InputMediaPhoto{Media: p.source}
			if p.caption != nil {
				item.Caption = *p.caption
			}
			if p.local != "" {
				data, rerr := os.ReadFile(p.local)
				if rerr != nil {
					return nil, rerr
				}
				name := fmt.Sprintf("file%d", i)
				item.Media, item.MediaAttachment, upload = "attach://"+name, bytes.NewReader(data), true
			}
			inputs = append(inputs, item)
		}
		r := routingOf(kw)
		msgs, sendErr := b.SendMediaGroup(mediaCtx(c, kw, upload), &bot.SendMediaGroupParams{ChatID: r.ChatID, MessageThreadID: r.MessageThreadID,
			DirectMessagesTopicID: r.DirectMessagesTopic, Media: inputs, DisableNotification: r.DisableNotification, ReplyParameters: r.ReplyParameters})
		return msgs, ptbError(sendErr)
	}, kw, metadata, replyToID, "media group", nil)
	if err != nil {
		return false, err
	}
	return true, nil
}

// Sends a local image natively as a photo, falling back to a document then the base adapter
func (a *TelegramAdapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	compressed := a.compressImageToJPEG(imagePath)
	actual := imagePath
	docName := filepath.Base(imagePath)
	if compressed != "" {
		actual = compressed
		docName = strings.TrimSuffix(filepath.Base(imagePath), filepath.Ext(imagePath)) + ".jpg"
		defer os.Remove(compressed)
	}
	onError := func(e error) (gateway.SendResult, error) {
		text := pyrepr.ExceptionMessage(e)
		if strings.Contains(text, "Photo_invalid_dimensions") || strings.Contains(text, "PHOTO_INVALID_DIMENSIONS") {
			slog.Info(fmt.Sprintf("[%s] Image dimensions exceed Telegram photo limits, sending as document: %s", a.Name(), imagePath))
		} else {
			slog.Warn(fmt.Sprintf("[%s] Failed to send Telegram local image as photo, trying document fallback: %s", a.Name(), redactTelegramErrorText(e)))
		}
		return a.SendDocument(ctx, chatID, actual, caption, docName, replyTo, metadata, nil)
	}
	return a.sendLocalFile(ctx, "Image", actual, chatID, replyTo, metadata, "photo", func(c context.Context, b *bot.Bot, r mediaRouting, file models.InputFile) (*models.Message, error) {
		params := &bot.SendPhotoParams{ChatID: r.ChatID, MessageThreadID: r.MessageThreadID, DirectMessagesTopicID: r.DirectMessagesTopic, Photo: file,
			ReplyParameters: r.ReplyParameters, DisableNotification: r.DisableNotification}
		if c1024 := caption1024(caption); c1024 != nil {
			params.Caption = *c1024
		}
		return b.SendPhoto(c, params)
	}, onError)
}

// Native local file send shell, the existence check, upload with routing and the failure handler
func (a *TelegramAdapter) sendLocalFile(ctx context.Context, label, path, chatID, replyTo string, metadata map[string]any, mediaKey string,
	send func(ctx context.Context, b *bot.Bot, r mediaRouting, file models.InputFile) (*models.Message, error),
	onError func(e error) (gateway.SendResult, error)) (gateway.SendResult, error) {
	b := a.currentBot()
	if b == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	if _, err := os.Stat(path); err != nil {
		return gateway.SendResult{Success: false, Error: missingMediaPathError(label, path)}, nil
	}
	msg, err := sendMedia(ctx, a.pacer, a.Name(), func(c context.Context, kw mediaSendKwargs) (*models.Message, error) {
		file, ferr := uploadOf(path)
		if ferr != nil {
			return nil, ferr
		}
		m, sendErr := send(mediaCtx(c, kw, true), b, routingOf(kw), file)
		return m, ptbError(sendErr)
	}, chatID, optional(replyTo), metadata, a.replyToMode, a.notificationsMode, mediaKey, nil)
	if err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return gateway.SendResult{}, err
		}
		return onError(err)
	}
	return gateway.SendResult{Success: true, MessageID: sentID(msg)}, nil
}

// Logs the native media failure then runs the base fallback
func (a *TelegramAdapter) warnThen(mediaKey string, e error, fallback func() (gateway.SendResult, error)) (gateway.SendResult, error) {
	slog.Warn(fmt.Sprintf("[%s] Failed to send %s: %s", a.Name(), mediaKey, redactTelegramErrorText(e)))
	return fallback()
}

// Sends a file natively as a Telegram document attachment
func (a *TelegramAdapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	name := fileName
	if name == "" {
		name = filepath.Base(filePath)
	}
	return a.sendLocalFile(ctx, "File", filePath, chatID, replyTo, metadata, "document", func(c context.Context, b *bot.Bot, r mediaRouting, file models.InputFile) (*models.Message, error) {
		if up, ok := file.(*models.InputFileUpload); ok {
			up.Filename = name
		}
		params := &bot.SendDocumentParams{ChatID: r.ChatID, MessageThreadID: r.MessageThreadID, DirectMessagesTopicID: r.DirectMessagesTopic, Document: file,
			ReplyParameters: r.ReplyParameters, DisableNotification: r.DisableNotification}
		if c1024 := caption1024(caption); c1024 != nil {
			params.Caption = *c1024
		}
		return b.SendDocument(c, params)
	}, func(e error) (gateway.SendResult, error) {
		return a.warnThen("document", e, func() (gateway.SendResult, error) {
			return a.BasePlatformAdapter.SendDocument(ctx, chatID, filePath, caption, fileName, replyTo, metadata, nil)
		})
	})
}

// Sends a video natively with its probed geometry and a JPEG thumbnail
func (a *TelegramAdapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	geometry, hasGeometry := ProbeVideoGeometry(ctx, videoPath)
	thumb := ""
	if hasGeometry {
		thumb = VideoThumbnailJPEG(ctx, videoPath, geometry.Duration)
	}
	if thumb != "" {
		defer os.Remove(thumb)
	}
	return a.sendLocalFile(ctx, "Video", videoPath, chatID, replyTo, metadata, "video", func(c context.Context, b *bot.Bot, r mediaRouting, file models.InputFile) (*models.Message, error) {
		params := &bot.SendVideoParams{ChatID: r.ChatID, MessageThreadID: r.MessageThreadID, DirectMessagesTopicID: r.DirectMessagesTopic, Video: file,
			ReplyParameters: r.ReplyParameters, DisableNotification: r.DisableNotification}
		if c1024 := caption1024(caption); c1024 != nil {
			params.Caption = *c1024
		}
		if hasGeometry {
			params.Width, params.Height = geometry.Width, geometry.Height
			if geometry.Duration != nil {
				params.Duration = *geometry.Duration
			}
		}
		if thumb != "" {
			thumbFile, terr := uploadOf(thumb)
			if terr != nil {
				return nil, terr
			}
			params.Thumbnail = thumbFile
		}
		return b.SendVideo(c, params)
	}, func(e error) (gateway.SendResult, error) {
		return a.warnThen("video", e, func() (gateway.SendResult, error) {
			return a.BasePlatformAdapter.SendVideo(ctx, chatID, videoPath, caption, replyTo, metadata, nil)
		})
	})
}

// Tools env of the active home for URL safety checks
func urlSafetyEnv(ctx context.Context) *tools.Env {
	return &tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}
}

// Sends a URL image as a photo, then as an uploaded download, then through the base text fallback
func (a *TelegramAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	b := a.currentBot()
	if b == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	env := urlSafetyEnv(ctx)
	if !tools.IsSafeURL(env, imageURL) {
		slog.Warn(fmt.Sprintf("[%s] Blocked unsafe image URL (SSRF protection)", a.Name()))
		return a.BasePlatformAdapter.SendImage(ctx, chatID, imageURL, caption, replyTo, metadata)
	}
	sendPhoto := func(photo func() models.InputFile, label string, upload bool) (*models.Message, error) {
		return sendMedia(ctx, a.pacer, a.Name(), func(c context.Context, kw mediaSendKwargs) (*models.Message, error) {
			r := routingOf(kw)
			params := &bot.SendPhotoParams{ChatID: r.ChatID, MessageThreadID: r.MessageThreadID, DirectMessagesTopicID: r.DirectMessagesTopic,
				Photo: photo(), ReplyParameters: r.ReplyParameters, DisableNotification: r.DisableNotification}
			if c1024 := caption1024(caption); c1024 != nil {
				params.Caption = *c1024
			}
			m, sendErr := b.SendPhoto(mediaCtx(c, kw, upload), params)
			return m, ptbError(sendErr)
		}, chatID, optional(replyTo), metadata, a.replyToMode, a.notificationsMode, label, nil)
	}
	msg, err := sendPhoto(func() models.InputFile { return &models.InputFileString{Data: imageURL} }, "URL photo", false)
	if err == nil {
		return gateway.SendResult{Success: true, MessageID: sentID(msg)}, nil
	}
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return gateway.SendResult{}, err
	}
	slog.Warn(fmt.Sprintf("[%s] URL-based send_photo failed, trying file upload: %s", a.Name(), redactTelegramErrorText(err)))
	data, err := fetchSafeImage(ctx, env, imageURL)
	if err == nil {
		msg, err = sendPhoto(func() models.InputFile {
			return &models.InputFileUpload{Filename: "photo", Data: bytes.NewReader(data)}
		}, "uploaded photo", true)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return gateway.SendResult{}, err
		}
		slog.Error(fmt.Sprintf("[%s] File upload send_photo also failed: %s", a.Name(), pyrepr.ExceptionMessage(err)))
		return a.BasePlatformAdapter.SendImage(ctx, chatID, imageURL, caption, replyTo, metadata)
	}
	return gateway.SendResult{Success: true, MessageID: sentID(msg)}, nil
}

// Downloads an image through the SSRF safe client with every redirect hop rechecked
func fetchSafeImage(ctx context.Context, env *tools.Env, imageURL string) ([]byte, error) {
	client := tools.NewSSRFSafeClient(env, 30*time.Second, true)
	inner := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := gateway.SSRFRedirectGuard(env, req.URL.String()); err != nil {
			return err
		}
		if inner == nil {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		}
		return inner(req, via)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("Client error '%d %s' for url '%s'", resp.StatusCode, strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)+" "), imageURL)
	}
	return io.ReadAll(resp.Body)
}

// Sends an animated GIF natively, falling back to a photo
func (a *TelegramAdapter) SendAnimation(ctx context.Context, chatID, animationURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	b := a.currentBot()
	if b == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	msg, err := sendMedia(ctx, a.pacer, a.Name(), func(c context.Context, kw mediaSendKwargs) (*models.Message, error) {
		r := routingOf(kw)
		params := &bot.SendAnimationParams{ChatID: r.ChatID, MessageThreadID: r.MessageThreadID, DirectMessagesTopicID: r.DirectMessagesTopic,
			Animation: &models.InputFileString{Data: animationURL}, ReplyParameters: r.ReplyParameters, DisableNotification: r.DisableNotification}
		if c1024 := caption1024(caption); c1024 != nil {
			params.Caption = *c1024
		}
		m, sendErr := b.SendAnimation(mediaCtx(c, kw, false), params)
		return m, ptbError(sendErr)
	}, chatID, optional(replyTo), metadata, a.replyToMode, a.notificationsMode, "animation", nil)
	if err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return gateway.SendResult{}, err
		}
		slog.Error(fmt.Sprintf("[%s] Failed to send Telegram animation, falling back to photo: %s", a.Name(), redactTelegramErrorText(err)))
		return a.SendImage(ctx, chatID, animationURL, caption, replyTo, metadata)
	}
	return gateway.SendResult{Success: true, MessageID: sentID(msg)}, nil
}
