package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Notice for one user, a plain send unless the platform has a private channel
func (b *BasePlatformAdapter) SendPrivateNotice(ctx context.Context, chatID string, userID *string, content, replyTo string, metadata map[string]any) (SendResult, error) {
	return b.Self().Send(ctx, chatID, content, replyTo, metadata)
}

// Typing indicator, nothing where the platform has none
func (b *BasePlatformAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	return nil
}

// Clears the typing indicator, nothing where the platform has none
func (b *BasePlatformAdapter) StopTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	return nil
}

// Stops typing with the thread metadata so a per thread status never clears a sibling thread
func (b *BasePlatformAdapter) stopTypingWithMetadata(ctx context.Context, chatID string, metadata map[string]any) error {
	if len(metadata) > 0 {
		return b.Self().StopTyping(ctx, chatID, metadata)
	}
	return b.Self().StopTyping(ctx, chatID, nil)
}

// Sends each image through its native sender, success when at least one was delivered
func (b *BasePlatformAdapter) SendMultipleImages(ctx context.Context, chatID string, images []ImageRef, metadata map[string]any, humanDelay float64) (SendResult, error) {
	self := b.Self()
	name := self.Name()
	delivered := false
	for _, image := range images {
		if humanDelay > 0 {
			timer := time.NewTimer(secondsDuration(humanDelay))
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return SendResult{}, ctx.Err()
			}
		}
		alt := []rune(image.Alt)
		if len(alt) > 30 {
			alt = alt[:30]
		}
		slog.Info(fmt.Sprintf("[%s] Sending image: %s (alt=%s)", name, SafeURLForLog(image.URL, 80), string(alt)))
		var result SendResult
		var err error
		switch {
		case strings.HasPrefix(image.URL, "file://"):
			var path string
			if path, err = pyurl.URL2Pathname(image.URL[7:], false, false); err == nil {
				result, err = self.SendImageFile(ctx, chatID, path, image.Alt, "", metadata, nil)
			}
		case isAnimationURL(image.URL):
			result, err = self.SendAnimation(ctx, chatID, image.URL, image.Alt, "", metadata)
		default:
			result, err = self.SendImage(ctx, chatID, image.URL, image.Alt, "", metadata)
		}
		if err != nil {
			slog.Error(fmt.Sprintf("[%s] Error sending image: %v", name, err))
			continue
		}
		if !result.Success {
			slog.Error(fmt.Sprintf("[%s] Failed to send image: %v", name, result.Error))
		} else {
			delivered = true
		}
	}
	if len(images) == 0 {
		return SendResult{Success: false, Error: "no images to send"}, nil
	}
	if delivered {
		return SendResult{Success: true}, nil
	}
	return SendResult{Success: false, Error: "all images failed to send"}, nil
}

// Image by URL, the caption and URL as text where the platform has no native image send
func (b *BasePlatformAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (SendResult, error) {
	text := imageURL
	if caption != "" {
		text = caption + "\n" + imageURL
	}
	return b.Self().Send(ctx, chatID, text, replyTo, metadata)
}

// Animated image by URL, the image send where the platform has no native animation
func (b *BasePlatformAdapter) SendAnimation(ctx context.Context, chatID, animationURL, caption, replyTo string, metadata map[string]any) (SendResult, error) {
	return b.Self().SendImage(ctx, chatID, animationURL, caption, replyTo, metadata)
}

// Local image file, a friendly failure notice where the platform has no native file send
func (b *BasePlatformAdapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (SendResult, error) {
	return b.sendMediaFallbackNotice(ctx, "send_image_file", "image", imagePath, chatID, caption, replyTo, metadata, "")
}

// Native voice message, a friendly failure notice by default
func (b *BasePlatformAdapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (SendResult, error) {
	return b.sendMediaFallbackNotice(ctx, "send_voice", "audio", audioPath, chatID, caption, replyTo, metadata, "")
}

// Native inline video, a friendly failure notice by default
func (b *BasePlatformAdapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (SendResult, error) {
	return b.sendMediaFallbackNotice(ctx, "send_video", "video", videoPath, chatID, caption, replyTo, metadata, "")
}

// Native document send, a friendly failure notice naming only the caller's file name by default
func (b *BasePlatformAdapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (SendResult, error) {
	return b.sendMediaFallbackNotice(ctx, "send_document", "file", filePath, chatID, caption, replyTo, metadata, fileName)
}
