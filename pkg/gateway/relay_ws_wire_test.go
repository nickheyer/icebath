package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Wire payload as the decoded frame object the transport reads
func relayWireObject(t *testing.T, payload map[string]any) *pyjson.Object {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := pyjson.Loads(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	obj, _ := pyjson.AsObject(parsed)
	return obj
}

func relayEventOf(t *testing.T, payload map[string]any) *MessageEvent {
	t.Helper()
	ev, err := relayEventFromWire(context.Background(), relayWireObject(t, payload))
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// Discord voice suite wire event with extra top level fields
func relayVoiceWire(messageType string, extra map[string]any) map[string]any {
	out := map[string]any{"text": "", "message_type": messageType, "source": map[string]any{"platform": "discord", "chat_id": "chan-1",
		"chat_type": "dm", "user_id": "u-1", "user_name": "ben"}}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func relayURLsOf(media []map[string]any) []any {
	out := make([]any, len(media))
	for i, m := range media {
		out[i] = m["url"]
	}
	return out
}

func TestWireVoiceParsesToMessageTypeVoice(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if ev := relayEventOf(t, relayVoiceWire("voice", nil)); ev.MessageType != MessageTypeVoice {
		t.Fatalf("type %s", ev.MessageType)
	}
}

func TestWireAudioStillParsesToMessageTypeAudio(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if ev := relayEventOf(t, relayVoiceWire("audio", nil)); ev.MessageType != MessageTypeAudio {
		t.Fatalf("type %s", ev.MessageType)
	}
}

func TestMediaMimesLandOnEventMediaTypes(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	media := []map[string]any{{"url": "https://cdn.discordapp.com/attachments/1/2/voice-message.ogg", "kind": "audio", "mime": "audio/ogg",
		"size": 19197, "filename": "voice-message.ogg"}}
	ev := relayEventOf(t, relayVoiceWire("voice", map[string]any{"media": media, "media_urls": relayURLsOf(media)}))
	if !slices.Equal(ev.MediaTypes, []string{"audio/ogg"}) || !slices.Equal(ev.MediaURLs, []string{media[0]["url"].(string)}) {
		t.Fatalf("types %v urls %v", ev.MediaTypes, ev.MediaURLs)
	}
}

func TestMediaTypesParallelToMediaURLsForMultipleAttachments(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	media := []map[string]any{{"url": "https://x/photo.png", "kind": "image", "mime": "image/png"},
		{"url": "https://x/doc.pdf", "kind": "document", "mime": "application/pdf"}}
	ev := relayEventOf(t, relayVoiceWire("image", map[string]any{"media": media, "media_urls": relayURLsOf(media)}))
	if !slices.Equal(ev.MediaTypes, []string{"image/png", "application/pdf"}) || len(ev.MediaTypes) != len(ev.MediaURLs) {
		t.Fatalf("types %v", ev.MediaTypes)
	}
}

func TestMissingMimeGivesEmptyStringAtThatIndex(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	media := []map[string]any{{"url": "https://x/photo.png", "kind": "image", "mime": "image/png"}, {"url": "https://x/blob", "kind": "document"}}
	ev := relayEventOf(t, relayVoiceWire("image", map[string]any{"media": media, "media_urls": relayURLsOf(media)}))
	if !slices.Equal(ev.MediaTypes, []string{"image/png", ""}) {
		t.Fatalf("types %v", ev.MediaTypes)
	}
}

func TestNoMediaFieldMeansEmptyMediaTypes(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if ev := relayEventOf(t, relayVoiceWire("text", nil)); len(ev.MediaTypes) != 0 || ev.MediaTypes == nil {
		t.Fatalf("types %v", ev.MediaTypes)
	}
}

func TestLengthMismatchResolvesByURLWithoutMisaligning(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	media := []map[string]any{{"url": "https://x/a.png", "kind": "image", "mime": "image/png"},
		{"url": "https://x/b.pdf", "kind": "document", "mime": "application/pdf"}}
	ev := relayEventOf(t, relayVoiceWire("image", map[string]any{"media": media, "media_urls": []any{"https://x/a.png"}}))
	if !slices.Equal(ev.MediaURLs, []string{"https://x/a.png"}) || !slices.Equal(ev.MediaTypes, []string{"image/png"}) {
		t.Fatalf("urls %v types %v", ev.MediaURLs, ev.MediaTypes)
	}
}

// User identity suite wire event with source overrides, nil values deleting the key
func relayIdentityWire(overrides map[string]any) map[string]any {
	src := map[string]any{"platform": "discord", "chat_id": "chan-1", "chat_type": "group", "user_id": "u-1", "user_name": "rawusername", "scope_id": "guild-1"}
	for k, v := range overrides {
		src[k] = v
	}
	return map[string]any{"text": "hello", "message_type": "text", "source": src}
}

func TestHandleIsLastResort(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ev := relayEventOf(t, relayIdentityWire(map[string]any{"user_name": nil, "user_handle": "ben#1234"}))
	if ev.Source.UserName == nil || *ev.Source.UserName != "ben#1234" {
		t.Fatalf("user name %v", ev.Source.UserName)
	}
}

func TestSessionKeyIsStableAcrossNameShapes(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	plain := relayEventOf(t, relayIdentityWire(nil))
	enriched := relayEventOf(t, relayIdentityWire(map[string]any{"user_display_name": "Ben Display", "user_handle": "ben#1234"}))
	a, errA := BuildSessionKey(ctx, plain.Source, true, false, "")
	b, errB := BuildSessionKey(ctx, enriched.Source, true, false, "")
	if errA != nil || errB != nil || a != b {
		t.Fatalf("keys %q %q errs %v %v", a, b, errA, errB)
	}
}

const relayRehost = "https://conn.example/relay/media/"

// Attachment suite slack document wire event
func relayFilenameWire(media []map[string]any, mediaURLs []any) map[string]any {
	if mediaURLs == nil {
		mediaURLs = relayURLsOf(media)
	}
	return map[string]any{"text": "file these", "message_type": "document",
		"source": map[string]any{"platform": "slack", "chat_id": "C1", "chat_type": "dm", "user_id": "U1"}, "media": media, "media_urls": mediaURLs}
}

func TestWireFilenamesLandOnTheEventByURLNotPosition(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a, b := relayRehost+"a", relayRehost+"b"
	ev := relayEventOf(t, relayFilenameWire([]map[string]any{
		{"url": b, "kind": "document", "mime": "application/pdf", "filename": "B report.pdf"},
		{"url": a, "kind": "document", "mime": "application/pdf", "filename": "A_notes.pdf"}}, []any{a, b, relayRehost + "orphan"}))
	if !slices.Equal(ev.MediaFilenames, []string{"A_notes.pdf", "B report.pdf", ""}) || len(ev.MediaFilenames) != len(ev.MediaURLs) {
		t.Fatalf("filenames %v", ev.MediaFilenames)
	}
}

func TestOldConnectorPayloadWithoutFilenamesStillParses(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ev := relayEventOf(t, relayFilenameWire([]map[string]any{{"url": relayRehost + "a", "mime": "application/pdf"}}, nil))
	if !slices.Equal(ev.MediaFilenames, []string{""}) {
		t.Fatalf("filenames %v", ev.MediaFilenames)
	}
	if ev := relayEventOf(t, relayFilenameWire([]map[string]any{}, []any{})); len(ev.MediaFilenames) != 0 {
		t.Fatalf("filenames %v", ev.MediaFilenames)
	}
}

func TestWireFilenameIsReducedToASafeBasename(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ev := relayEventOf(t, relayFilenameWire([]map[string]any{{"url": relayRehost + "a", "filename": "../../etc/pass\r\nwd.pdf"}}, nil))
	if !slices.Equal(ev.MediaFilenames, []string{"passwd.pdf"}) {
		t.Fatalf("filenames %v", ev.MediaFilenames)
	}
}

const relayPDF = "%PDF-1.4 fixture bytes"

// Media client against a loopback rehost answering the PDF with the headers
func relayPDFClient(t *testing.T, headers map[string]string) (*RelayMediaClient, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Write([]byte(relayPDF))
	}))
	t.Cleanup(srv.Close)
	return NewRelayMediaClient(srv.URL, "gw1", "sec"), srv.URL + "/relay/media/x"
}

func TestDownloadResolvesTheContentDispositionName(t *testing.T) {
	for header, expected := range map[string]string{
		`attachment; filename="2026.10.20_filename_probe_A.pdf"`:                         "2026.10.20_filename_probe_A.pdf",
		"attachment; filename=plain.pdf":                                                 "plain.pdf",
		"attachment; filename*=UTF-8''r%C3%A9sum%C3%A9%20final.pdf":                      "résumé final.pdf",
		"attachment; filename=\"fallback.pdf\"; filename*=UTF-8''%E6%8A%A5%E5%91%8A.pdf": "报告.pdf",
		"attachment; filename*=UTF-8''%E6%8A%A5%E5%91%8A.pdf; filename=\"fallback.pdf\"": "报告.pdf",
		`attachment; filename="../../etc/passwd.pdf"`:                                    "passwd.pdf",
		`attachment; filename="..\\..\\win.pdf"`:                                         "win.pdf",
		"attachment; filename*=UTF-8''evil%0D%0Aname%E2%80%AE.pdf":                       "evilname.pdf",
		"inline": "",
	} {
		t.Run(header, func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			client, url := relayPDFClient(t, map[string]string{"Content-Type": "application/pdf", "Content-Disposition": header})
			path, name, ok, err := client.DownloadWithName(context.Background(), url, nil)
			if !ok || err != nil || name != expected {
				t.Fatalf("ok %v err %v name %q want %q", ok, err, name, expected)
			}
			data, _ := os.ReadFile(path)
			if !strings.HasPrefix(filepath.Base(path), "relay_media_") || filepath.Ext(path) != ".pdf" || filepath.Dir(path) != tmp || string(data) != relayPDF {
				t.Fatalf("path %s", path)
			}
		})
	}
}

func TestSuggestedNameWinsOverTheHeader(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	client, url := relayPDFClient(t, map[string]string{"Content-Disposition": `attachment; filename="header.pdf"`})
	suggested := "From Slack.pdf"
	_, name, ok, err := client.DownloadWithName(context.Background(), url, &suggested)
	if !ok || err != nil || name != "From Slack.pdf" {
		t.Fatalf("name %q ok %v err %v", name, ok, err)
	}
	legacy, err := client.Download(context.Background(), url, nil)
	data, _ := os.ReadFile(legacy)
	if err != nil || string(data) != relayPDF {
		t.Fatalf("legacy %q err %v", legacy, err)
	}
}
