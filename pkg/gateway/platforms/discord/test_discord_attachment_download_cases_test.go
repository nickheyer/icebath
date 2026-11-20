package discord

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/tools"
)

var oggBytes = []byte("OggS\x00\x02\x00\x00\x00\x00\x00\x00\x00\x00voice")

func cdnAttachment(name string) *discordgo.MessageAttachment {
	return &discordgo.MessageAttachment{URL: "https://cdn.discordapp.com/attachments/fake/" + name, Filename: name}
}

// Fake CDN answering attachment reads with the given body
func serveAttachment(api *fakeDiscordAPI, body []byte) {
	api.mu.Lock()
	api.fail = func(c fakeCall, n int) (int, string) {
		if c.Method == http.MethodGet && strings.HasPrefix(c.Path, "/attachments/") {
			return 200, string(body)
		}
		return 0, ""
	}
	api.mu.Unlock()
}

func TestReadAttachmentBytesReturnsNoneWhenReadRaises(t *testing.T) {
	a, api := sendAdapter(t)
	api.mu.Lock()
	api.fail = func(c fakeCall, n int) (int, string) { return 403, "Forbidden" }
	api.mu.Unlock()
	raw, err := a.readAttachmentBytes(context.Background(), cdnAttachment("file.png"), "media")
	if raw != nil || err != nil {
		t.Fatalf("raw %v err %v", raw, err)
	}
}

func TestCacheDiscordImageFallsBackToURLWhenBytesValidatorRejects(t *testing.T) {
	a, api := sendAdapter(t)
	serveAttachment(api, []byte("<html>forbidden</html>"))
	prevBytes, prevURL := cacheImageFromBytesAsync, cacheImageFromURL
	t.Cleanup(func() { cacheImageFromBytesAsync, cacheImageFromURL = prevBytes, prevURL })
	cacheImageFromBytesAsync = func(context.Context, []byte, string) (string, error) { return "", errors.New("not a valid image") }
	urlCalls := 0
	cacheImageFromURL = func(context.Context, string, string, int) (string, error) {
		urlCalls++
		return "/tmp/fallback.png", nil
	}
	got, err := a.cacheDiscordImage(context.Background(), cdnAttachment("file.png"), ".png")
	if err != nil || got != "/tmp/fallback.png" || urlCalls != 1 {
		t.Fatalf("got %q err %v url calls %d", got, err, urlCalls)
	}
}

func TestCacheDiscordAudioPrefersAttReadOverURL(t *testing.T) {
	a, api := sendAdapter(t)
	serveAttachment(api, oggBytes)
	prevBytes, prevURL := cacheAudioFromBytesAsync, cacheAudioFromURL
	t.Cleanup(func() { cacheAudioFromBytesAsync, cacheAudioFromURL = prevBytes, prevURL })
	var gotBytes []byte
	var gotExt string
	cacheAudioFromBytesAsync = func(_ context.Context, data []byte, ext string) (string, error) {
		gotBytes, gotExt = data, ext
		return "/tmp/voice.ogg", nil
	}
	cacheAudioFromURL = func(context.Context, string, string, int) (string, error) {
		t.Fatal("URL fallback called")
		return "", nil
	}
	got, err := a.cacheDiscordAudio(context.Background(), cdnAttachment("voice.ogg"), ".ogg")
	if err != nil || got != "/tmp/voice.ogg" || !slices.Equal(gotBytes, oggBytes) || gotExt != ".ogg" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestCacheDiscordDocumentFallbackBlockedBySsrfGuard(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := newTestAdapter(t, nil)
	att := cdnAttachment("doc.pdf")
	prev := isSafeURL
	t.Cleanup(func() { isSafeURL = prev })
	var checked []string
	isSafeURL = func(_ *tools.Env, raw string) bool {
		checked = append(checked, raw)
		return false
	}
	_, err := a.cacheDiscordDocument(context.Background(), att)
	if err == nil || !strings.Contains(err.Error(), "SSRF") || !slices.Equal(checked, []string{att.URL}) {
		t.Fatalf("err %v checked %v", err, checked)
	}
}
