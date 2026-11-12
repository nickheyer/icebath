package telegram

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/go-telegram/bot/models"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/mediacache"
)

// File the loopback getFile answers for an id, failing with a status when set
type fakeFile struct {
	path   string
	data   []byte
	status int
}

// Responder serving getFile and the file download of each id, other methods answered by next
func serveFiles(files map[string]fakeFile, next func(method string, params map[string]string) (int, string)) func(string, map[string]string) (int, string) {
	return func(method string, params map[string]string) (int, string) {
		if method == "getFile" {
			f, ok := files[params["file_id"]]
			if !ok {
				return botError(400, "Bad Request: invalid file_id")
			}
			if f.status != 0 {
				return botError(f.status, "Internal Server Error")
			}
			return 200, fmt.Sprintf(`{"ok":true,"result":{"file_id":%q,"file_unique_id":"u","file_size":%d,"file_path":%q}}`, params["file_id"], len(f.data), f.path)
		}
		for _, f := range files {
			if f.path != "" && path.Base(f.path) == method {
				return 200, string(f.data)
			}
		}
		if next != nil {
			return next(method, params)
		}
		return 0, ""
	}
}

// Private chat message of the hermes _make_message helper
func docMessage(caption string) *models.Message {
	return &models.Message{ID: 42, Caption: caption, Chat: models.Chat{ID: 100, Type: "private", FirstName: "Test", LastName: "User"},
		From: &models.User{ID: 1, FirstName: "Test", LastName: "User"}}
}

// Document of the hermes _make_document helper
func docOf(fileName, mimeType string, size int64) *models.Document {
	return &models.Document{FileID: "doc-file", FileUniqueID: "u", FileName: fileName, MimeType: mimeType, FileSize: size}
}

// Adapter of the documents fixture with a sink standing in for handle_message
func docAdapter(t *testing.T, files map[string]fakeFile) (*TelegramAdapter, *scriptedBot, *eventSink) {
	a, fake := newScriptedAdapter(t, nil)
	fake.respond = serveFiles(files, nil)
	return a, fake, newEventSink(a)
}

func handleMedia(t *testing.T, a *TelegramAdapter, m *models.Message) {
	t.Helper()
	if err := a.handleMediaMessage(context.Background(), updateOf(1, m)); err != nil {
		t.Fatal(err)
	}
}

func TestDocumentTypeDetectionDocumentDetectedExplicitly(t *testing.T) {
	a, _, sink := docAdapter(t, map[string]fakeFile{"doc-file": {path: "documents/file.pdf", data: []byte("hello")}})
	m := docMessage("")
	m.Document = docOf("report.pdf", "application/pdf", 1024)
	handleMedia(t, a, m)
	if event := sink.first(t); event.MessageType != gateway.MessageTypeDocument {
		t.Fatalf("type %v", event.MessageType)
	}
}

func TestDocumentDownloadBlockSupportedTxtInjectsContent(t *testing.T) {
	content := []byte("Hello from a text file")
	a, _, sink := docAdapter(t, map[string]fakeFile{"doc-file": {path: "documents/file.pdf", data: content}})
	m := docMessage("")
	m.Document = docOf("notes.txt", "text/plain", int64(len(content)))
	handleMedia(t, a, m)
	event := sink.first(t)
	if !strings.Contains(event.Text, "Hello from a text file") || !strings.Contains(event.Text, "[Content of notes.txt]") {
		t.Fatalf("text %q", event.Text)
	}
}

func TestDocumentDownloadBlockCaptionPreservedWithInjection(t *testing.T) {
	content := []byte("file text")
	a, _, sink := docAdapter(t, map[string]fakeFile{"doc-file": {path: "documents/file.pdf", data: content}})
	m := docMessage("Please summarize")
	m.Document = docOf("doc.txt", "text/plain", int64(len(content)))
	handleMedia(t, a, m)
	event := sink.first(t)
	if !strings.Contains(event.Text, "file text") || !strings.Contains(event.Text, "Please summarize") {
		t.Fatalf("text %q", event.Text)
	}
}

func TestDocumentDownloadBlockDocumentMarksMediaTextInlined(t *testing.T) {
	for _, tc := range []struct {
		id      string
		content []byte
		inlined bool
	}{{"small", []byte("small text"), true}, {"large", []byte(strings.Repeat("x", 200*1024)), false}} {
		t.Run(tc.id, func(t *testing.T) {
			a, _, sink := docAdapter(t, map[string]fakeFile{"doc-file": {path: "documents/file.pdf", data: tc.content}})
			m := docMessage("")
			m.Document = docOf("notes.txt", "text/plain", int64(len(tc.content)))
			handleMedia(t, a, m)
			event := sink.first(t)
			if strings.Contains(event.Text, "[Content of") != tc.inlined {
				t.Fatalf("text %q", event.Text)
			}
			if len(event.MediaTextInlined) != 1 || event.MediaTextInlined[0] == nil || *event.MediaTextInlined[0] != tc.inlined {
				t.Fatalf("inlined %v", event.MediaTextInlined)
			}
		})
	}
}

// Text of every reply the adapter sent through sendMessage
func replyTexts(fake *scriptedBot) []string {
	var out []string
	for _, c := range fake.callsOf("sendMessage") {
		out = append(out, c.params["text"])
	}
	return out
}

func TestDocumentDownloadBlockDocumentCacheFailureRepliesAndSignalsAgent(t *testing.T) {
	a, fake, sink := docAdapter(t, map[string]fakeFile{"doc-file": {status: 502}})
	m := docMessage("")
	m.Document = docOf("notes.md", "text/markdown", 100)
	handleMedia(t, a, m)
	replies := replyTexts(fake)
	if len(replies) != 1 || !strings.Contains(replies[0], "notes.md") || !strings.Contains(replies[0], "NetworkError") {
		t.Fatalf("replies %q", replies)
	}
	event := sink.first(t)
	if sink.count() != 1 || len(event.MediaURLs) != 0 {
		t.Fatalf("events %d media %v", sink.count(), event.MediaURLs)
	}
	if !strings.Contains(event.Text, "could not be downloaded") || !strings.Contains(event.Text, "notes.md") {
		t.Fatalf("text %q", event.Text)
	}
}

func TestDocumentDownloadBlockVoiceCacheFailureRepliesAndSignalsAgent(t *testing.T) {
	a, fake, sink := docAdapter(t, map[string]fakeFile{"voice-file": {status: 502}})
	m := docMessage("")
	m.Voice = &models.Voice{FileID: "voice-file", FileUniqueID: "v", FileSize: 100}
	handleMedia(t, a, m)
	if n := len(replyTexts(fake)); n != 1 {
		t.Fatalf("replies %d", n)
	}
	event := sink.first(t)
	if sink.count() != 1 || !strings.Contains(event.Text, "could not be downloaded") {
		t.Fatalf("text %q", event.Text)
	}
}

func TestDocumentDownloadBlockVideoNoteFailureReplyIsLocalized(t *testing.T) {
	a, fake, sink := docAdapter(t, map[string]fakeFile{"note-file": {status: 502}})
	t.Setenv("ICEBATH_LANGUAGE", "de")
	m := docMessage("")
	m.VideoNote = &models.VideoNote{FileID: "note-file", FileUniqueID: "n", FileSize: 100}
	handleMedia(t, a, m)
	replies := replyTexts(fake)
	if len(replies) != 1 || !strings.Contains(replies[0], "Videodatei") || strings.Contains(replies[0], "video note") {
		t.Fatalf("replies %q", replies)
	}
	if event := sink.first(t); !strings.Contains(event.Text, "send a video note") {
		t.Fatalf("text %q", event.Text)
	}
}

func TestVideoDownloadBlockNativeVideoIsCached(t *testing.T) {
	for _, attr := range []string{"video", "video_note"} {
		t.Run(attr, func(t *testing.T) {
			a, _, sink := docAdapter(t, map[string]fakeFile{"clip": {path: attr + "s/clip.mp4", data: []byte("fake-mp4")}})
			m := docMessage("")
			if attr == "video" {
				m.Video = &models.Video{FileID: "clip", FileUniqueID: "c"}
			} else {
				m.VideoNote = &models.VideoNote{FileID: "clip", FileUniqueID: "c"}
			}
			handleMedia(t, a, m)
			event := sink.first(t)
			if event.MessageType != gateway.MessageTypeVideo || len(event.MediaURLs) != 1 {
				t.Fatalf("event %v %v", event.MessageType, event.MediaURLs)
			}
			if _, err := os.Stat(event.MediaURLs[0]); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(event.MediaTypes, []string{mediacache.SupportedVideoTypes[".mp4"]}) {
				t.Fatalf("types %v", event.MediaTypes)
			}
		})
	}
}

func TestVideoDownloadBlockMediaHandlerAcceptsVideoNotes(t *testing.T) {
	a, _ := newScriptedAdapter(t, nil)
	app := NewTelegramApplication(1, BuildUpdateProcessor(a.Config.Extra, a.Name()), a, a.admission)
	a.registerHandlers(app)
	media := reflect.ValueOf(filterMedia).Pointer()
	var found *messageHandler
	for _, group := range app.groupSnapshot() {
		for _, h := range group {
			if mh, ok := h.(*messageHandler); ok && reflect.ValueOf(mh.filter).Pointer() == media {
				found = mh
			}
		}
	}
	if found == nil {
		t.Fatal("media handler not registered")
	}
	for name, m := range map[string]*models.Message{"VIDEO": {Video: &models.Video{FileID: "v"}}, "VIDEO_NOTE": {VideoNote: &models.VideoNote{FileID: "n"}}} {
		if !found.filter(m) {
			t.Fatalf("media filter rejects %s", name)
		}
	}
}

func TestVideoDownloadBlockReplyToAVideoNoteCachesIt(t *testing.T) {
	a, _, _ := docAdapter(t, map[string]fakeFile{"note": {path: "video_notes/file_1.mp4", data: []byte("fake-mp4")}})
	replied := docMessage("")
	replied.VideoNote = &models.VideoNote{FileID: "note", FileUniqueID: "n", FileSize: 1024}
	m := docMessage("")
	m.ReplyToMessage = replied
	event := gateway.NewMessageEvent("what is in this?")
	event.MessageType = gateway.MessageTypeText
	a.cacheRepliedMedia(context.Background(), m, event)
	if event.MessageType != gateway.MessageTypeVideo || !reflect.DeepEqual(event.MediaTypes, []string{"video/mp4"}) {
		t.Fatalf("event %v %v", event.MessageType, event.MediaTypes)
	}
	if _, err := os.Stat(event.MediaURLs[0]); err != nil {
		t.Fatal(err)
	}
}

func TestMediaGroupsNonAlbumPhotoBurstIsBufferedAndCombined(t *testing.T) {
	a, _, sink := docAdapter(t, map[string]fakeFile{"first": {path: "photos/first.jpg", data: []byte("first")},
		"second": {path: "photos/second.jpg", data: []byte("second")}})
	paths := []string{"/tmp/burst-one.jpg", "/tmp/burst-two.jpg"}
	var mu sync.Mutex
	saved := cacheImageFromBytesAsync
	cacheImageFromBytesAsync = func(context.Context, []byte, string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		next := paths[0]
		paths = paths[1:]
		return next, nil
	}
	t.Cleanup(func() { cacheImageFromBytesAsync = saved })
	m1 := docMessage("two images")
	m1.Photo = []models.PhotoSize{{FileID: "first", FileUniqueID: "f"}}
	m2 := docMessage("")
	m2.Photo = []models.PhotoSize{{FileID: "second", FileUniqueID: "s"}}
	handleMedia(t, a, m1)
	handleMedia(t, a, m2)
	if sink.count() != 0 {
		t.Fatal("burst dispatched before the batch delay")
	}
	event := sink.first(t)
	if sink.count() != 1 || event.Text != "two images" || !reflect.DeepEqual(event.MediaURLs, []string{"/tmp/burst-one.jpg", "/tmp/burst-two.jpg"}) ||
		len(event.MediaTypes) != 2 {
		t.Fatalf("event %q %v %v", event.Text, event.MediaURLs, event.MediaTypes)
	}
}

func TestSendVoiceFlacFallsBackToDocument(t *testing.T) {
	a, fake := newScriptedAdapter(t, nil)
	fake.respond = func(method string, _ map[string]string) (int, string) {
		if method == "sendDocument" {
			return 200, `{"ok":true,"result":{"message_id":101,"date":0,"chat":{"id":12345,"type":"private"}}}`
		}
		return 0, ""
	}
	audio := filepath.Join(t.TempDir(), "clip.flac")
	if err := os.WriteFile(audio, append([]byte("fLaC"), make([]byte, 32)...), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := a.SendVoice(context.Background(), "12345", audio, "Audio", "", nil, nil)
	if err != nil || !result.Success || result.MessageID != "101" {
		t.Fatalf("result %+v %v", result, err)
	}
	if len(fake.callsOf("sendDocument")) != 1 || len(fake.callsOf("sendAudio")) != 0 || len(fake.callsOf("sendVoice")) != 0 {
		t.Fatal("flac must go out as a document only")
	}
}

func TestSendDocumentApiErrorFallsBack(t *testing.T) {
	a, fake := newScriptedAdapter(t, nil)
	a.richSendDisabled.Store(true)
	fake.respond = func(method string, _ map[string]string) (int, string) {
		if method == "sendDocument" {
			return botError(400, "Bad Request: Telegram API error")
		}
		return 0, ""
	}
	file := filepath.Join(t.TempDir(), "file.pdf")
	if err := os.WriteFile(file, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := a.SendDocument(context.Background(), "12345", file, "", "", "", nil, nil)
	if err != nil || !result.Success || result.MessageID != "42" {
		t.Fatalf("result %+v %v", result, err)
	}
	if len(fake.callsOf("sendMessage")) != 1 {
		t.Fatal("fallback text send missing")
	}
}

func TestTelegramPhotoBatchingFlushPhotoBatchDoesNotDropNewerScheduledTask(t *testing.T) {
	a, _ := newScriptedAdapter(t, nil)
	a.mediaBatchDelaySeconds = 0
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	newTask := gateway.SpawnAdapterTask(context.Background(), func(context.Context) error { <-release; return nil })
	key := "session:photo-burst"
	pending := gateway.NewMessageEvent("")
	pending.MessageType = gateway.MessageTypePhoto
	pending.MediaURLs, pending.MediaTypes = []string{"/tmp/a.jpg"}, []string{"image/jpeg"}
	a.mu.Lock()
	a.pendingPhotoBatchTasks[key] = newTask
	a.pendingPhotoBatches[key] = pending
	a.mu.Unlock()
	oldTask := gateway.SpawnAdapterTask(context.Background(), func(c context.Context) error { return a.flushPhotoBatch(c, key) })
	if err := oldTask.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pendingPhotoBatchTasks[key] != newTask {
		t.Fatal("newer scheduled flush dropped")
	}
}

func TestSendVideoThreadId(t *testing.T) {
	a, fake := newScriptedAdapter(t, nil)
	clip := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(clip, append([]byte("\x00\x00\x00\x1cftyp"), make([]byte, 100)...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SendVideo(context.Background(), "12345", clip, "", "", map[string]any{"thread_id": "789"}, nil); err != nil {
		t.Fatal(err)
	}
	calls := fake.callsOf("sendVideo")
	if len(calls) != 1 || calls[0].params["message_thread_id"] != "789" {
		t.Fatalf("calls %+v", calls)
	}
}
