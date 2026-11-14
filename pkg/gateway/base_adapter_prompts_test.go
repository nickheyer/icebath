package gateway

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type eaPlain struct{ *BasePlatformAdapter }

func (a *eaPlain) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *eaPlain) Disconnect(context.Context) error            { return nil }
func (a *eaPlain) Send(context.Context, string, string, string, map[string]any) (SendResult, error) {
	return SendResult{Success: true}, nil
}
func (a *eaPlain) GetChatInfo(context.Context, string) (map[string]any, error) {
	return map[string]any{}, nil
}

type eaButtons struct {
	eaPlain
	prompts []*ExecApprovalPrompt
}

func (a *eaButtons) SendExecApprovalPrompt(_ context.Context, p *ExecApprovalPrompt) (SendResult, error) {
	a.prompts = append(a.prompts, p)
	return SendResult{Success: true, MessageID: "m1"}, nil
}

func newEAButtons(t *testing.T) *eaButtons {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := &eaButtons{}
	a.BasePlatformAdapter = NewBasePlatformAdapter(NewPlatformConfig(), PlatformTelegram, a)
	return a
}

func TestChoiceSetFollowsTheSharedRule(t *testing.T) {
	cases := []struct {
		perm, sess, smart bool
		want              []string
	}{
		{true, true, false, []string{"once", "session", "always", "deny"}},
		{false, true, false, []string{"once", "session", "deny"}},
		{true, false, false, []string{"once", "deny"}},
		{true, true, true, []string{"once", "deny"}},
		{false, false, true, []string{"once", "deny"}},
	}
	for _, c := range cases {
		a := newEAButtons(t)
		desc := "cleanup"
		res, err := a.SendExecApproval(context.Background(), "chat", "rm -rf /tmp/x", "sess", &desc, nil, c.perm, c.sess, c.smart, nil)
		if err != nil || !res.Success || len(a.prompts) != 1 {
			t.Fatalf("send: %v %+v %d", err, res, len(a.prompts))
		}
		p := a.prompts[0]
		if !reflect.DeepEqual(p.Choices(), c.want) {
			t.Fatalf("choices %v want %v", p.Choices(), c.want)
		}
		if !strings.Contains(p.Text, "rm -rf /tmp/x") || !strings.Contains(p.Text, "cleanup") {
			t.Fatalf("text %q", p.Text)
		}
	}
}

func TestCommandIsTruncatedToThePlatformBudget(t *testing.T) {
	a := newEAButtons(t)
	a.Traits.EACmdBudget = 20
	if _, err := a.SendExecApproval(context.Background(), "chat", strings.Repeat("x", 100), "sess", nil, nil, true, true, false, nil); err != nil {
		t.Fatal(err)
	}
	p := a.prompts[0]
	if !strings.Contains(p.Text, strings.Repeat("x", 20)+"...") || strings.Contains(p.Text, strings.Repeat("x", 21)) {
		t.Fatalf("text %q", p.Text)
	}
	if p.Command != strings.Repeat("x", 100) {
		t.Fatalf("command %q", p.Command)
	}
}

type duckApprovalSender struct{}

func (duckApprovalSender) SendExecApproval(context.Context, string, string, string, *string, map[string]any, bool, bool, bool, *string) (SendResult, error) {
	return SendResult{Success: true}, nil
}

func TestRunnerOnlyOffersButtonsToAdaptersThatRenderThem(t *testing.T) {
	plain := &eaPlain{}
	plain.BasePlatformAdapter = NewBasePlatformAdapter(NewPlatformConfig(), PlatformTelegram, plain)
	if !rendersExecApprovalButtons(newEAButtons(t)) || rendersExecApprovalButtons(plain) {
		t.Fatal("button detection follows the prompt hook override")
	}
	if !rendersExecApprovalButtons(duckApprovalSender{}) || rendersExecApprovalButtons(struct{}{}) {
		t.Fatal("duck typed detection follows send_exec_approval")
	}
	res, _ := plain.SendExecApproval(context.Background(), "chat", "ls", "sess", nil, nil, true, true, false, nil)
	if res.Success || res.Error != "Not supported" {
		t.Fatalf("base default %+v", res)
	}
}

type historyStubStore struct{ transcript []any }

func (s *historyStubStore) LoadTranscript(context.Context, string) ([]any, error) {
	return append([]any(nil), s.transcript...), nil
}

func ttsToolRow(path string) map[string]any {
	return map[string]any{"role": "tool", "content": `{"success": true, "file_path": "` + path + `", "media_tag": "MEDIA:` + path + `"}`}
}

func historyAdapter(transcript []any) *eaPlain {
	a := &eaPlain{}
	a.BasePlatformAdapter = NewBasePlatformAdapter(NewPlatformConfig(), PlatformAPIServer, a)
	a.SetSessionStore(&historyStubStore{transcript})
	return a
}

func TestCurrentTurnTTSMediaNotTreatedAsHistory(t *testing.T) {
	current := "/opt/data/cache/audio/tts_now.mp3"
	a := historyAdapter([]any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "hello"},
		map[string]any{"role": "user", "content": "say it out loud"},
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{}}},
		ttsToolRow(current),
		map[string]any{"role": "assistant", "content": "MEDIA:" + current},
	})
	paths, err := a.historyMediaPathsForSession(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := paths[current]; ok {
		t.Fatalf("current turn media deduped: %v", paths)
	}
}

func TestPriorTurnMediaStillDeduped(t *testing.T) {
	old := "/opt/data/cache/audio/tts_old.mp3"
	a := historyAdapter([]any{
		map[string]any{"role": "user", "content": "speak"},
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{}}},
		ttsToolRow(old),
		map[string]any{"role": "assistant", "content": "MEDIA:" + old},
		map[string]any{"role": "user", "content": "thanks"},
		map[string]any{"role": "assistant", "content": "you're welcome"},
	})
	paths, err := a.historyMediaPathsForSession(context.Background(), "k")
	if _, ok := paths[old]; err != nil || !ok {
		t.Fatalf("prior media missing: %v %v", paths, err)
	}
	bounded, err := a.boundedHistoryMediaPathsForSession(context.Background(), "k")
	if _, ok := bounded[old]; err != nil || !ok {
		t.Fatalf("bounded lookup: %v %v", bounded, err)
	}
}

func TestCollectHistoryMediaPathsIncludesImageGenerateJSON(t *testing.T) {
	paths, err := collectHistoryMediaPaths(context.Background(), []any{
		map[string]any{"role": "user", "content": "make a cat"},
		map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c", "function": map[string]any{"name": "image_generate"}}}},
		map[string]any{"role": "tool", "tool_call_id": "c", "content": `{"success": true, "image": "/tmp/gen/cat.png"}`},
		map[string]any{"role": "tool", "tool_call_id": "d", "content": "Saved MEDIA:/tmp/voice/note.ogg done"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/tmp/gen/cat.png", "/tmp/voice/note.ogg"} {
		if _, ok := paths[p]; !ok {
			t.Fatalf("missing %s in %v", p, paths)
		}
	}
}

const sensitiveHostPath = "/home/jayne/.icebath/cache/media/sensitive_host_path_abc123.bin"

func TestSendDocumentFallbackIncludesExplicitFilenameOnly(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := newMediaAdapter()
	res, err := a.SendDocument(context.Background(), "123", sensitiveHostPath, "", "report.pdf", "", nil, nil)
	if err != nil || !res.Success || len(a.sent) != 1 {
		t.Fatalf("%v %+v %v", err, res, a.sent)
	}
	if strings.Contains(a.sent[0], sensitiveHostPath) || strings.Contains(a.sent[0], "/home/") || !strings.Contains(a.sent[0], "report.pdf") {
		t.Fatalf("sent %q", a.sent[0])
	}
}

func TestCaptionIsPreservedInFallback(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := newMediaAdapter()
	if _, err := a.SendVideo(context.Background(), "123", sensitiveHostPath, "Here's the daily summary.", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.sent[0], "Here's the daily summary.") || strings.Contains(a.sent[0], sensitiveHostPath) {
		t.Fatalf("sent %q", a.sent[0])
	}
}

func TestRejectedPathAppearsInLog(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(dir, "home"))
	outside := filepath.Join(dir, "outside.ogg")
	if err := os.WriteFile(outside, []byte("OggS"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_MEDIA_DELIVERY_STRICT", "1")
	t.Setenv("ICEBATH_MEDIA_TRUST_RECENT_FILES", "0")
	saved := mediaDeliverySafeRoots
	mediaDeliverySafeRoots = func() []string { return nil }
	t.Cleanup(func() { mediaDeliverySafeRoots = saved })
	logs := captureLogs(t)
	out, err := FilterMediaDeliveryPathsFor(context.Background(), []MediaRef{{Path: outside}}, "", nil)
	if err != nil || len(out) != 0 {
		t.Fatalf("%v %v", out, err)
	}
	if !strings.Contains(logs.String(), outside) || !strings.Contains(logs.String(), "denied by the delivery policy") {
		t.Fatalf("log %q", logs.String())
	}
}

func TestMissingFileIsLoggedAsNotFoundNotUnsafe(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(dir, "home"))
	logs := captureLogs(t)
	out, err := FilterMediaDeliveryPathsFor(context.Background(), []MediaRef{{Path: filepath.Join(dir, "never-written.pdf")}}, "", nil)
	if err != nil || len(out) != 0 {
		t.Fatalf("%v %v", out, err)
	}
	if !strings.Contains(logs.String(), "not found on this host") || strings.Contains(logs.String(), "unsafe") {
		t.Fatalf("log %q", logs.String())
	}
}

func TestCraftedNullPathDoesNotAbortBatch(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(dir, "home"))
	t.Setenv("ICEBATH_MEDIA_DELIVERY_STRICT", "0")
	good := filepath.Join(dir, "good.png")
	if err := os.WriteFile(good, []byte("\x89PNG"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := FilterMediaDeliveryPathsFor(context.Background(), []MediaRef{{Path: "~\x00evil.png"}, {Path: good}}, "", nil)
	resolved, _ := filepath.EvalSymlinks(good)
	if err != nil || !reflect.DeepEqual(out, []MediaRef{{Path: resolved}}) {
		t.Fatalf("%v %v", out, err)
	}
}

func extractLocal(t *testing.T, content string, existing map[string]bool) ([]string, string) {
	savedExp, savedIs := localFileExpanduser, localFileIsFile
	t.Cleanup(func() { localFileExpanduser, localFileIsFile = savedExp, savedIs })
	localFileIsFile = func(p string) bool { return existing == nil || existing[p] }
	localFileExpanduser = func(p string) string {
		if strings.HasPrefix(p, "~/") {
			return "/home/user" + p[1:]
		}
		return p
	}
	return ExtractLocalFiles(context.Background(), content)
}

func TestExtractLocalFilesDetection(t *testing.T) {
	paths, cleaned := extractLocal(t, "Here is the screenshot /root/screenshots/game.png enjoy", nil)
	if !reflect.DeepEqual(paths, []string{"/root/screenshots/game.png"}) || strings.Contains(cleaned, "/root/screenshots/game.png") || !strings.Contains(cleaned, "Here is the screenshot") {
		t.Fatalf("absolute %v %q", paths, cleaned)
	}
	paths, cleaned = extractLocal(t, "Check out ~/photos/cat.jpg for the cat", nil)
	if !reflect.DeepEqual(paths, []string{"/home/user/photos/cat.jpg"}) || strings.Contains(cleaned, "~/photos/cat.jpg") {
		t.Fatalf("tilde %v %q", paths, cleaned)
	}
	for _, c := range []struct{ prefix, exts, suffix string }{
		{"Video at /tmp/clip", ".mp4 .mov .avi .mkv .webm", " here"},
		{"Image at /tmp/pic", ".png .jpg .jpeg .gif .webp", " here"},
		{"Report at /tmp/report", ".pdf .docx .doc .odt .rtf .txt .md", " attached"},
	} {
		for _, ext := range strings.Fields(c.exts) {
			paths, _ := extractLocal(t, c.prefix+ext+c.suffix, nil)
			want := strings.Fields(c.prefix)[2] + ext
			if !reflect.DeepEqual(paths, []string{want}) {
				t.Fatalf("ext %s: %v", ext, paths)
			}
		}
	}
	if paths, _ := extractLocal(t, "/var/data/image.png", nil); !reflect.DeepEqual(paths, []string{"/var/data/image.png"}) {
		t.Fatalf("line start %v", paths)
	}
}

func TestExtractLocalFilesGuardsAndExclusions(t *testing.T) {
	if paths, cleaned := extractLocal(t, "See /tmp/nope.png here", map[string]bool{}); len(paths) != 0 || !strings.Contains(cleaned, "/tmp/nope.png") {
		t.Fatalf("nonexistent %v %q", paths, cleaned)
	}
	if paths, cleaned := extractLocal(t, "Visit https://example.com/images/photo.png for details", nil); len(paths) != 0 || !strings.Contains(cleaned, "https://example.com/images/photo.png") {
		t.Fatalf("url %v %q", paths, cleaned)
	}
	if paths, cleaned := extractLocal(t, "Here's how:\n```python\nimg = open('/tmp/image.png')\n```\nDone.", nil); len(paths) != 0 || !strings.Contains(cleaned, "/tmp/image.png") {
		t.Fatalf("fenced %v %q", paths, cleaned)
	}
	if paths, cleaned := extractLocal(t, "Use the path `/tmp/image.png` in your config", nil); len(paths) != 0 || !strings.Contains(cleaned, "`/tmp/image.png`") {
		t.Fatalf("inline %v %q", paths, cleaned)
	}
	if paths, _ := extractLocal(t, "See /tmp/img.png and also /tmp/img.png again", nil); !reflect.DeepEqual(paths, []string{"/tmp/img.png"}) {
		t.Fatalf("dedup %v", paths)
	}
}

func TestExtractLocalFilesCleanupAndEdges(t *testing.T) {
	if _, cleaned := extractLocal(t, "Before /tmp/x.png after", nil); !strings.Contains(cleaned, "Before") || !strings.Contains(cleaned, "after") || strings.Contains(cleaned, "/tmp/x.png") {
		t.Fatalf("removed %q", cleaned)
	}
	if _, cleaned := extractLocal(t, "Before\n\n\n/tmp/x.png\n\n\nAfter", nil); strings.Contains(cleaned, "\n\n\n") {
		t.Fatalf("collapsed %q", cleaned)
	}
	if paths, cleaned := extractLocal(t, "", nil); len(paths) != 0 || cleaned != "" {
		t.Fatalf("empty %v %q", paths, cleaned)
	}
	for content, want := range map[string]string{
		"See C:\\Users\\test\\image.png here": "C:\\Users\\test\\image.png",
		"See C:/Users/test/image.png here":    "C:/Users/test/image.png",
		"Video at D:/data/clip.mp4 ready":     "D:/data/clip.mp4",
		"Path e:/audio/track.mp3 done":        "e:/audio/track.mp3",
	} {
		paths, cleaned := extractLocal(t, content, nil)
		if !reflect.DeepEqual(paths, []string{want}) || strings.Contains(cleaned, want) {
			t.Fatalf("drive %q: %v %q", content, paths, cleaned)
		}
	}
	if paths, _ := extractLocal(t, "File at foo\\bar.png here", nil); len(paths) != 0 {
		t.Fatalf("relative windows %v", paths)
	}
	for _, suffix := range []string{",", ".", ")", ":", ";"} {
		if paths, _ := extractLocal(t, "See /tmp/img.png"+suffix+" details", nil); len(paths) != 1 {
			t.Fatalf("suffix %q: %v", suffix, paths)
		}
	}
}

func TestStreamingTTSTurnKeyAndSuppression(t *testing.T) {
	mid := "m7"
	var upd int64 = 0
	if k, ok := StreamingTTSTurnKey("", 3, nil); ok || k != "" {
		t.Fatal("blank session key has no turn key")
	}
	if k, _ := StreamingTTSTurnKey("s", 3, nil); k != "s:3" {
		t.Fatalf("generation key %q", k)
	}
	if k, _ := StreamingTTSTurnKey("s", nil, &MessageEvent{MessageID: &mid}); k != "s:m7" {
		t.Fatalf("message id key %q", k)
	}
	empty := ""
	if k, ok := StreamingTTSTurnKey("s", nil, &MessageEvent{MessageID: &empty, PlatformUpdateID: &upd}); !ok || k != "s:0" {
		t.Fatalf("update id key %q", k)
	}
	if _, ok := StreamingTTSTurnKey("s", nil, &MessageEvent{MessageID: &empty}); ok {
		t.Fatal("blank message id without update id has no key")
	}
	a := newMediaAdapter()
	if a.streamingTTSTurnCompleted("s", 3, nil) {
		t.Fatal("fresh turn is not completed")
	}
	a.markStreamingTTSCompletedTurn("s", 3, nil)
	if !a.streamingTTSTurnCompleted("s", 3, nil) || a.streamingTTSTurnCompleted("s", 4, nil) {
		t.Fatal("suppression is turn scoped")
	}
}

func TestFormatChoicePageClampsAndDescribes(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	opts := []int{1, 2, 3, 4, 5}
	page, meta := FormatChoicePage(context.Background(), opts, 9, 2)
	if !reflect.DeepEqual(page, []int{5}) || meta["page"] != 2 || meta["total_pages"] != 3 || meta["start"] != 4 || meta["end"] != 5 {
		t.Fatalf("%v %v", page, meta)
	}
	if meta["page_info"] != " (5–5 of 5)" {
		t.Fatalf("page info %q", meta["page_info"])
	}
	if _, meta := FormatChoicePage(context.Background(), opts[:2], 0, 5); meta["page_info"] != "" {
		t.Fatalf("one page %v", meta)
	}
}

func TestUnwrapEphemeralHandlesUnreadableConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("display:\n  ephemeral_system_ttl: boom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newMediaAdapter()
	text, ttl := a.unwrapEphemeral(context.Background(), NewEphemeralReply("bye", nil))
	if text != "bye" || ttl != 0 {
		t.Fatalf("%v %d", text, ttl)
	}
	five := 5
	if text, ttl := a.unwrapEphemeral(context.Background(), NewEphemeralReply("hi", &five)); text != "hi" || ttl != 5 {
		t.Fatalf("deleting adapter keeps ttl: %v %d", text, ttl)
	}
	plain := historyAdapter(nil)
	if _, ttl := plain.unwrapEphemeral(context.Background(), NewEphemeralReply("hi", &five)); ttl != 0 {
		t.Fatalf("non deleting adapter degrades to a normal send: %d", ttl)
	}
	if text, ttl := plain.unwrapEphemeral(context.Background(), "raw"); text != "raw" || ttl != 0 {
		t.Fatalf("plain response %v %d", text, ttl)
	}
}

type liveAdapterRunner struct{ live PlatformAdapter }

func (r liveAdapterRunner) DeliveryAdapterFor(context.Context, *SessionSource) (PlatformAdapter, error) {
	return r.live, nil
}

func TestFinalDeliveryAdapterFollowsSamePlatformReplacement(t *testing.T) {
	a, replacement, other := newMediaAdapter(), newMediaAdapter(), historyAdapter(nil)
	if a.finalDeliveryAdapter(context.Background(), nil) != PlatformAdapter(a) {
		t.Fatal("no runner keeps this adapter")
	}
	a.GatewayRunner = liveAdapterRunner{replacement}
	if a.finalDeliveryAdapter(context.Background(), nil) != PlatformAdapter(replacement) {
		t.Fatal("same platform replacement wins")
	}
	a.GatewayRunner = liveAdapterRunner{other}
	if a.finalDeliveryAdapter(context.Background(), nil) != PlatformAdapter(a) {
		t.Fatal("another platform never takes the send")
	}
	ctx, release := a.mediaDeliveryScope(context.Background(), &SessionSource{})
	release()
	if ctx != context.Background() {
		t.Fatal("no scope runner keeps the context")
	}
}
