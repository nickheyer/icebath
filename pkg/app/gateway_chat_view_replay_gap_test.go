package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Runs a one shot chat view against a real owner whose viewer pauses for the mode given
func replayGapOneshot(t *testing.T, mode string, quiet bool, emitter *StreamJSONEmitter) int {
	t.Helper()
	installSessionsStoreWiring(t)
	replayGapFrameCap(t)
	ctx := context.Background()
	owner := newReplayGapOwner(t, filepath.Join(t.TempDir(), "state.db"), mode)
	code := func() int {
		defer owner.finish()
		raw, err := owner.RPCTimeout(ctx, "session.resume", map[string]any{"session_id": "s"}, 0)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, _ := pyjson.Plain(raw).(map[string]any)
		view := newGatewayChatView(owner, snapshot, quiet, emitter, "")
		type outcome struct {
			code int
			err  error
		}
		done := make(chan outcome, 1)
		go func() {
			code, err := view.Run(ctx, "write a long answer", true)
			done <- outcome{code, err}
		}()
		select {
		case got := <-done:
			if got.err != nil {
				t.Fatal(got.err)
			}
			return got.code
		case <-time.After(30 * time.Second):
			t.Fatal("the one shot view did not finish within 30 s")
		}
		return -1
	}()
	if owner.gapCount() == 0 {
		t.Fatal("probe did not overflow the fanout (vacuous)")
	}
	return code
}

func TestStreamJsonResultIsAuthoritativeAndNoTextFollowsTheGap(t *testing.T) {
	for _, mode := range []string{"ended", "live"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			out, _ := captureStreams(t)
			if code := replayGapOneshot(t, mode, false, NewStreamJSONEmitter("m", "s")); code != 0 {
				t.Fatalf("exit %d", code)
			}
			records := streamEvents(t, out)
			var text strings.Builder
			for _, record := range records {
				if record["type"] == "text" {
					piece, _ := record["text"].(string)
					text.WriteString(piece)
				}
			}
			if !strings.HasPrefix(replayGapFinal, text.String()) || text.Len() >= len(replayGapFinal) {
				t.Fatalf("text events after the gap (a hole or a splice): %d of %d bytes", text.Len(), len(replayGapFinal))
			}
			last := records[len(records)-1]
			if last["type"] != "result" || last["text"] != replayGapFinal || last["replay_gap"] != true {
				t.Fatalf("last record %v %v", last["type"], last["replay_gap"])
			}
		})
	}
}

func TestQuietOneshotPrintsTheAnswerOnce(t *testing.T) {
	for _, mode := range []string{"ended", "live"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			out, _ := captureStreams(t)
			if code := replayGapOneshot(t, mode, true, nil); code != 0 {
				t.Fatalf("exit %d", code)
			}
			if count := strings.Count(out.String(), replayGapFinal); count != 1 {
				t.Fatalf("answer printed %d times", count)
			}
		})
	}
}
