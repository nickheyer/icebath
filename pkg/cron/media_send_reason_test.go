package cron

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

type inlineLoop struct{}

func (inlineLoop) IsRunning() bool { return true }
func (inlineLoop) Schedule(fn func()) bool {
	go fn()
	return true
}

type voiceOnlyAdapter struct {
	send func(ctx context.Context) (any, error)
}

func (a voiceOnlyAdapter) SendVoice(ctx context.Context, chatID, audioPath string, metadata map[string]any, isVoice bool) (any, error) {
	return a.send(ctx)
}

// Sends one clip with the adapter media filter keeping it, the clip dir allowlisted wherever the temp root resolves
func sendClipThrough(t *testing.T, send func(ctx context.Context) (any, error)) (string, string) {
	t.Helper()
	s, _ := newTestStore(t)
	dir := t.TempDir()
	t.Setenv("ICEBATH_MEDIA_ALLOW_DIRS", dir)
	media := filepath.Join(dir, "clip.mp3")
	if err := os.WriteFile(media, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	prev := gateway.FilterMediaDeliveryPathsFor
	gateway.FilterMediaDeliveryPathsFor = func(context.Context, []gateway.MediaRef, string, *[]map[string]any) ([]gateway.MediaRef, error) {
		return []gateway.MediaRef{{Path: media}}, nil
	}
	t.Cleanup(func() { gateway.FilterMediaDeliveryPathsFor = prev })
	errs := s.sendMediaViaAdapter(context.Background(), voiceOnlyAdapter{send: send}, "C123", []gateway.MediaRef{{Path: media}}, nil, inlineLoop{}, &Job{ID: "job-x"}, nil)
	if len(errs) != 1 {
		t.Fatalf("got %q", errs)
	}
	return errs[0], media
}

func TestEmptyReasonFallbackTimeoutErrorNamesTheClass(t *testing.T) {
	t.Setenv("ICEBATH_CRON_MEDIA_SEND_TIMEOUT", "1")
	got, media := sendClipThrough(t, func(ctx context.Context) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if strings.TrimRight(got, " \t\r\n") == "failed to send media "+media+":" || !strings.Contains(got, "TimeoutError") {
		t.Fatalf("got %q", got)
	}
	if got != "failed to send media "+media+": TimeoutError" {
		t.Fatalf("got %q", got)
	}
}

func TestEmptyReasonFallbackExceptionWithMessageKeepsIt(t *testing.T) {
	got, media := sendClipThrough(t, func(context.Context) (any, error) { return nil, errors.New("bridge closed") })
	if !strings.Contains(got, "bridge closed") || got != "failed to send media "+media+": bridge closed" {
		t.Fatalf("got %q", got)
	}
}
