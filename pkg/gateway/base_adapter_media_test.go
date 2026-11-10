package gateway

import (
	"context"
	"errors"
	"testing"
	"time"
)

type mediaAdapter struct {
	*BasePlatformAdapter
	sent      []string
	images    []string
	files     []string
	anims     []string
	failImage bool
	deleted   chan [2]string
}

func (a *mediaAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *mediaAdapter) Disconnect(context.Context) error            { return nil }
func (a *mediaAdapter) Send(_ context.Context, chatID, content, replyTo string, _ map[string]any) (SendResult, error) {
	a.sent = append(a.sent, chatID+"|"+content+"|"+replyTo)
	return SendResult{Success: true}, nil
}
func (a *mediaAdapter) GetChatInfo(context.Context, string) (map[string]any, error) {
	return map[string]any{}, nil
}
func (a *mediaAdapter) SendImage(_ context.Context, _ string, url, caption, _ string, _ map[string]any) (SendResult, error) {
	a.images = append(a.images, url+"|"+caption)
	if a.failImage {
		return SendResult{Success: false, Error: "rejected"}, nil
	}
	return SendResult{Success: true}, nil
}
func (a *mediaAdapter) SendImageFile(_ context.Context, _ string, path, _ string, _ string, _, _ map[string]any) (SendResult, error) {
	a.files = append(a.files, path)
	return SendResult{}, errors.New("upload failed")
}
func (a *mediaAdapter) SendAnimation(_ context.Context, _ string, url, _ string, _ string, _ map[string]any) (SendResult, error) {
	a.anims = append(a.anims, url)
	return SendResult{Success: true}, nil
}
func (a *mediaAdapter) DeleteMessage(_ context.Context, chatID, messageID string) (bool, error) {
	a.deleted <- [2]string{chatID, messageID}
	return true, nil
}

func newMediaAdapter() *mediaAdapter {
	a := &mediaAdapter{deleted: make(chan [2]string, 1)}
	a.BasePlatformAdapter = NewBasePlatformAdapter(NewPlatformConfig(), PlatformTelegram, a)
	return a
}

func TestSendMultipleImagesRoutesEachKindAndAggregates(t *testing.T) {
	a := newMediaAdapter()
	images := []ImageRef{{URL: "file:///tmp/a%20b.png", Alt: "local"}, {URL: "https://x.test/cat.gif", Alt: "anim"},
		{URL: "https://x.test/cat.png", Alt: "still"}}
	res, err := a.SendMultipleImages(context.Background(), "c", images, nil, 0)
	if err != nil || !res.Success || res.Error != nil {
		t.Fatalf("res %+v err %v", res, err)
	}
	if len(a.files) != 1 || a.files[0] != "/tmp/a b.png" || len(a.anims) != 1 || len(a.images) != 1 || a.images[0] != "https://x.test/cat.png|still" {
		t.Fatalf("files %v anims %v images %v", a.files, a.anims, a.images)
	}
}

func TestSendMultipleImagesAllFailedAndEmpty(t *testing.T) {
	a := newMediaAdapter()
	a.failImage = true
	res, _ := a.SendMultipleImages(context.Background(), "c", []ImageRef{{URL: "https://x.test/a.png"}}, nil, 0)
	if res.Success || res.Error != "all images failed to send" {
		t.Fatalf("%+v", res)
	}
	res, _ = a.SendMultipleImages(context.Background(), "c", nil, nil, 0)
	if res.Success || res.Error != "no images to send" {
		t.Fatalf("%+v", res)
	}
}

func TestBaseSendImageFallsBackToCaptionedText(t *testing.T) {
	a := newBaseFake(PlatformTelegram)
	rec := &mediaAdapter{}
	rec.BasePlatformAdapter = NewBasePlatformAdapter(NewPlatformConfig(), PlatformTelegram, rec)
	if _, err := rec.BasePlatformAdapter.SendImage(context.Background(), "c", "https://x.test/p.png", "look", "r1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.BasePlatformAdapter.SendPrivateNotice(context.Background(), "c", strPtr("u"), "psst", "", nil); err != nil {
		t.Fatal(err)
	}
	if len(rec.sent) != 2 || rec.sent[0] != "c|look\nhttps://x.test/p.png|r1" || rec.sent[1] != "c|psst|" {
		t.Fatal(rec.sent)
	}
	res, err := a.EditMessage(context.Background(), "c", "m", "x", true)
	if err != nil || res.Success || res.Error != "Not supported" {
		t.Fatalf("%+v", res)
	}
	if _, ok := any(a).(MessageDeleter); ok {
		t.Fatal("base adapter reports a message delete")
	}
	if _, ok, _ := a.CreateHandoffThread(context.Background(), "c", "n"); ok {
		t.Fatal("base handoff thread created")
	}
	if _, err := a.SendDraft(context.Background(), "c", 1, "x", nil); err == nil || err.Error() != "NotImplementedError: baseFakeAdapter does not implement send_draft" {
		t.Fatal(err)
	}
}

func TestScheduleEphemeralDeleteDeletesAfterTTL(t *testing.T) {
	a := newMediaAdapter()
	start := time.Now()
	a.ScheduleEphemeralDelete(context.Background(), "c", "m1", 0)
	select {
	case got := <-a.deleted:
		if got != [2]string{"c", "m1"} || time.Since(start) < time.Second {
			t.Fatalf("deleted %v after %v", got, time.Since(start))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ephemeral message never deleted")
	}
}

func TestBaseTraitsAndLengthDefaults(t *testing.T) {
	a := newBaseFake(PlatformTelegram)
	if a.Name() != "Telegram" || a.IsConnected() || !a.Traits.SupportsAsyncDelivery || a.Traits.TypedCommandPrefix != "/" ||
		!a.Traits.InteractiveResume {
		t.Fatal("class defaults differ")
	}
	if n, _ := a.MaxMessageLengthForChat("c"); n != 4096 {
		t.Fatal(n)
	}
	a.Traits.MaxMessageLength = 2000
	if n, _ := a.MaxMessageLengthForChat("c"); n != 2000 {
		t.Fatal(n)
	}
	if fn, _ := a.MessageLenFnForChat("c"); fn("héllo") != 5 {
		t.Fatal("code point length expected")
	}
	quietRuntimeStatus(t)
	a.MarkConnected(context.Background(), "")
	if !a.IsConnected() || a.HasFatalError() {
		t.Fatal("connect not marked")
	}
	a.SetFatalError(context.Background(), "x", "boom", false)
	if a.IsConnected() || !a.HasFatalError() || a.FatalErrorRetryable() {
		t.Fatal("fatal not recorded")
	}
	a.MarkConnected(context.Background(), "http://127.0.0.1:1")
	if a.HasFatalError() || !a.FatalErrorRetryable() {
		t.Fatal("connect did not clear the fatal error")
	}
}

type sinkRecorder struct{ calls []string }

func (s *sinkRecorder) OnDelta(text string)      { s.calls = append(s.calls, "delta:"+text) }
func (s *sinkRecorder) OnSegmentBreak()          { s.calls = append(s.calls, "break") }
func (s *sinkRecorder) OnCommentary(text string) { s.calls = append(s.calls, "commentary:"+text) }

func TestRenderMessageEventMapsOntoSink(t *testing.T) {
	a := newBaseFake(PlatformTelegram)
	sink := &sinkRecorder{}
	for _, ev := range []StreamEvent{MessageChunk{Text: "hi"}, MessageChunk{}, MessageStop{}, MessageStop{Final: true},
		Commentary{Text: "c"}, Commentary{}, NewToolCallFinished("t")} {
		a.RenderMessageEvent(ev, sink)
	}
	if len(sink.calls) != 3 || sink.calls[0] != "delta:hi" || sink.calls[1] != "break" || sink.calls[2] != "commentary:c" {
		t.Fatal(sink.calls)
	}
	if _, ok := a.FormatToolEvent(context.Background(), MessageChunk{Text: "x"}, "all", 40); ok {
		t.Fatal("non tool event rendered")
	}
}
