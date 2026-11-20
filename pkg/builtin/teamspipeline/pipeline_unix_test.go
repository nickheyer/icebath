//go:build !windows

package teamspipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareAudioPathExtractsWavThroughFFmpeg(t *testing.T) {
	bin, work := t.TempDir(), t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$FFMPEG_ARGS\"\nif [ -n \"$FFMPEG_FAIL\" ]; then printf 'boom \\377 \\n' >&2; exit 3; fi\nprintf audio > \"$4\"\n"
	if err := os.WriteFile(filepath.Join(bin, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FFMPEG_ARGS", filepath.Join(work, "args.txt"))
	recording := filepath.Join(work, "meeting.recording.mp4")
	pipeline, _ := testPipeline(t, nil)

	audio, err := pipeline.prepareAudioPath(context.Background(), recording)
	if err != nil {
		t.Fatal(err)
	}
	if audio != filepath.Join(work, "meeting.recording.wav") {
		t.Fatalf("audio path %q", audio)
	}
	if args, _ := os.ReadFile(filepath.Join(work, "args.txt")); string(args) != "-y\n-i\n"+recording+"\n"+audio+"\n" {
		t.Fatalf("args %q", args)
	}
	if body, _ := os.ReadFile(audio); string(body) != "audio" {
		t.Fatalf("audio body %q", body)
	}

	t.Setenv("FFMPEG_FAIL", "1")
	var retryable *TeamsPipelineRetryableError
	if _, err := pipeline.prepareAudioPath(context.Background(), recording); !errors.As(err, &retryable) || err.Error() != "ffmpeg audio extraction failed: boom �" {
		t.Fatalf("failure %q", err)
	}
}
