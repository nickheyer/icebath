package yuanbao

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestChunkMarkdownTextSplitsAtParagraphsAndKeepsFencesWhole(t *testing.T) {
	if got := chunkMarkdownText("hello world", 3000); !reflect.DeepEqual(got, []string{"hello world"}) {
		t.Fatalf("short text %v", got)
	}
	var paragraphs []string
	for i := range 10 {
		paragraphs = append(paragraphs, fmt.Sprintf("This is paragraph number %d", i)+strings.Repeat(". ", 50))
	}
	text := strings.Join(paragraphs, "\n\n")
	chunks := chunkMarkdownText(text, 500)
	if len(chunks) < 2 || float64(len(strings.Join(chunks, ""))) < float64(len(text))*0.95 {
		t.Fatalf("paragraph chunks %d", len(chunks))
	}
	long := chunkMarkdownText(strings.Repeat("a", 10000), 3000)
	if len(long) < 3 {
		t.Fatalf("long line chunks %d", len(long))
	}
	for _, chunk := range long {
		if len(chunk) > 3000 {
			t.Fatalf("chunk of %d chars", len(chunk))
		}
	}
	fenced := false
	for _, chunk := range chunkMarkdownText("```python\n"+strings.Repeat("x = 1\n", 200)+"```", 500) {
		if strings.Contains(chunk, "```python") {
			fenced = strings.Count(chunk, "```")%2 == 0
		}
	}
	if !fenced {
		t.Fatal("an oversized fence must stay in one chunk")
	}
}

func TestTruncateMessageStripsIndicatorsAndCronWrapperUnwraps(t *testing.T) {
	if got := truncateMessage("short (1/2)", 40); !reflect.DeepEqual(got, []string{"short (1/2)"}) {
		t.Fatalf("short content %v", got)
	}
	paged := strings.Repeat("a", 30) + " (1/2)\n\n" + strings.Repeat("b", 30) + " (2/2)"
	if got := truncateMessage(paged, 40); !reflect.DeepEqual(got, []string{strings.Repeat("a", 30), strings.Repeat("b", 30)}) {
		t.Fatalf("paged content %q", got)
	}
	header := cronWrapperHeaderPrefix + "daily\n(job_id: abc)"
	footer := cronWrapperFooterPrefix + `abc")`
	cases := map[string]string{
		header + cronWrapperDivider + "  the body  " + footer: "the body",
		"plain text": "plain text",
		cronWrapperHeaderPrefix + "daily" + cronWrapperDivider + "body" + footer: cronWrapperHeaderPrefix + "daily" + cronWrapperDivider + "body" + footer,
		header + cronWrapperDivider + "   " + footer:                             header + cronWrapperDivider + "   " + footer,
		header + cronWrapperDivider + "body":                                     header + cronWrapperDivider + "body",
	}
	for content, want := range cases {
		if got := stripCronWrapper(content); got != want {
			t.Fatalf("strip %q gave %q", content, got)
		}
	}
}

func TestSendsRefuseWithoutAConnection(t *testing.T) {
	adapter := makeAdapter(t, nil)
	ctx := context.Background()
	refused := gateway.SendResult{Success: false, Error: "Not connected", Retryable: true}
	if got, err := adapter.Send(ctx, "direct:alice", "hi", "", nil); err != nil || !reflect.DeepEqual(got, refused) {
		t.Fatalf("text send %+v %v", got, err)
	}
	if got, err := adapter.SendImageFile(ctx, "direct:alice", "/tmp/a.png", "", "", nil, nil); err != nil || !reflect.DeepEqual(got, refused) {
		t.Fatalf("media send %+v %v", got, err)
	}
	if err := adapter.SendTyping(ctx, "direct:alice", nil); err != nil {
		t.Fatal(err)
	}
	direct, err := adapter.outbound.sender.sendDirect(ctx, "direct:alice", "hi", nil)
	if err != nil || !reflect.DeepEqual(direct, map[string]any{"error": "Yuanbao send failed: Not connected"}) {
		t.Fatalf("direct send %v %v", direct, err)
	}
	empty, err := adapter.outbound.sender.sendDirect(ctx, "direct:alice", "   ", nil)
	if err != nil || !reflect.DeepEqual(empty, map[string]any{"error": "No deliverable text or media remained after processing"}) {
		t.Fatalf("empty direct send %v %v", empty, err)
	}
}

// Heartbeat values of the sent heartbeat frames of a cmd, read from the given request field
func heartbeatValues(t *testing.T, server *fakeServer, cmd string, field int64) []any {
	t.Helper()
	var values []any
	for _, frame := range server.sent(cmd) {
		fields, err := parseDict(frame.Vals["data"].([]byte))
		if err != nil {
			t.Fatal(err)
		}
		values = append(values, fields.GetVarint(field))
	}
	return values
}

func TestTypingHeartbeatRunsUntilStoppedAndTheSendFinishesIt(t *testing.T) {
	adapter, server := connectAdapter(t, nil)
	ctx := context.Background()
	if err := adapter.SendTyping(ctx, "direct:alice", nil); err != nil {
		t.Fatal(err)
	}
	if err := adapter.SendTyping(ctx, "group:grp-1", nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the running heartbeats", func() bool {
		return len(server.sent("send_private_heartbeat")) == 1 && len(server.sent("send_group_heartbeat")) == 1
	})
	if err := adapter.SendTyping(ctx, "direct:alice", nil); err != nil {
		t.Fatal(err)
	}
	for _, chatID := range []string{"direct:alice", "group:grp-1"} {
		if err := adapter.StopTyping(ctx, chatID, nil); err != nil {
			t.Fatal(err)
		}
	}
	adapter.outbound.heartbeat.mu.Lock()
	running := len(adapter.outbound.heartbeat.tasks)
	adapter.outbound.heartbeat.mu.Unlock()
	private, group := heartbeatValues(t, server, "send_private_heartbeat", 3), heartbeatValues(t, server, "send_group_heartbeat", 5)
	if running != 0 || !reflect.DeepEqual(private, []any{int64(WSHeartbeatRunning)}) || !reflect.DeepEqual(group, []any{int64(WSHeartbeatRunning)}) {
		t.Fatalf("stopped typing left %d workers and sent %v %v", running, private, group)
	}
	if result, err := adapter.Send(ctx, "direct:alice", "done", "", nil); err != nil || !result.Success {
		t.Fatalf("send %+v %v", result, err)
	}
	if private = heartbeatValues(t, server, "send_private_heartbeat", 3); !reflect.DeepEqual(private, []any{int64(WSHeartbeatRunning), int64(WSHeartbeatFinish)}) {
		t.Fatalf("heartbeats after the send %v", private)
	}
}

func TestStickerSendsBuildFaceElems(t *testing.T) {
	adapter, server := connectAdapter(t, nil)
	ctx := context.Background()
	name := StickerMap.Keys[0]
	if result, err := adapter.SendSticker(ctx, "direct:alice", &name, nil, "", nil); err != nil || !result.Success {
		t.Fatalf("named sticker %+v %v", result, err)
	}
	named := bodyElems(t, server.sent("send_c2c_message")[0], 5)
	data, _ := named[0].Vals["msg_content"].(*pyjson.Object).GetOr("data", "").(string)
	if len(named) != 1 || named[0].Vals["msg_type"] != "TIMFaceElem" || !strings.Contains(data, `"name":"`+name+`"`) {
		t.Fatalf("named sticker body %v", named)
	}
	faceIndex := 7
	if result, err := adapter.SendSticker(ctx, "group:grp-1", nil, &faceIndex, "ref-1", nil); err != nil || !result.Success {
		t.Fatalf("face sticker %+v %v", result, err)
	}
	frame := server.sent("send_group_message")[0]
	fields, err := parseDict(frame.Vals["data"].([]byte))
	if err != nil {
		t.Fatal(err)
	}
	face := bodyElems(t, frame, 6)
	if len(face) != 1 || !pyjson.Equal(face[0].Vals["msg_content"].(*pyjson.Object).GetOr("index", nil), int64(7)) || fields.GetString(7) != "ref-1" {
		t.Fatalf("face sticker body %v", face)
	}
	if result, err := adapter.SendSticker(ctx, "direct:alice", nil, nil, "", nil); err != nil || !result.Success {
		t.Fatalf("random sticker %+v %v", result, err)
	}
	blank := ""
	if result, err := adapter.SendSticker(ctx, "direct:alice", &blank, nil, "", nil); err != nil || result.Success || result.Error != "Sticker not found: ''" {
		t.Fatalf("blank sticker name %+v %v", result, err)
	}
}

// Sends every request of the client to the test server whatever host it names
type rerouteTransport struct{ target *url.URL }

func (r rerouteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rerouted := req.Clone(req.Context())
	rerouted.URL.Scheme, rerouted.URL.Host = r.target.Scheme, r.target.Host
	return http.DefaultTransport.RoundTrip(rerouted)
}

// Upload backend answering genUploadInfo with the current credentials and recording every uploaded body by its path
type cosBackend struct {
	mu          sync.Mutex
	credentials *pyjson.Object
	uploads     map[string]string
}

func (b *cosBackend) setCredentials(credentials *pyjson.Object) {
	b.mu.Lock()
	b.credentials = credentials
	b.mu.Unlock()
}

func (b *cosBackend) uploaded(path string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.uploads[path]
}

func (b *cosBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.Method == http.MethodPut {
		body, _ := io.ReadAll(r.Body)
		b.uploads[r.URL.Path] = string(body)
		return
	}
	if r.URL.Path != UploadInfoPath || r.Header.Get("X-Token") != "tok" || r.Header.Get("X-ID") != "bot_123" {
		http.Error(w, "unexpected request", http.StatusBadRequest)
		return
	}
	reply, _ := pyjson.Dumps(pyjson.ObjectOf("code", int64(0), "data", b.credentials))
	fmt.Fprint(w, reply)
}

// Starts the upload backend and routes every media request of the package to it
func startCOSBackend(t *testing.T, credentials *pyjson.Object) *cosBackend {
	t.Helper()
	backend := &cosBackend{credentials: credentials, uploads: map[string]string{}}
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	swap(t, &mediaClient, func() *http.Client { return &http.Client{Transport: rerouteTransport{target}} })
	return backend
}

func TestFileSendsUploadToCOSAndDispatchTheMediaBody(t *testing.T) {
	credentials := func(region any) *pyjson.Object {
		out := pyjson.ObjectOf("bucketName", "bucket-1", "location", "media/upload key.bin", "encryptTmpSecretId", "sid",
			"encryptTmpSecretKey", "skey", "encryptToken", "session", "resourceUrl", "https://cdn.example.com/res/1")
		if region != nil {
			out.Set("region", region)
		}
		return out
	}
	backend := startCOSBackend(t, credentials("ap-test"))
	adapter, server := connectAdapter(t, nil)
	ctx := context.Background()
	dir := t.TempDir()
	document, image := filepath.Join(dir, "report.txt"), filepath.Join(dir, "pic.png")
	png := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), 0, 0, 1, 0, 0, 0, 0, 0x80)
	for path, content := range map[string][]byte{document: []byte("hello"), image: png, filepath.Join(dir, "empty.txt"): nil} {
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	result, err := adapter.SendDocument(ctx, "direct:alice", document, "see attached", "renamed.txt", "", nil, map[string]any{"group_code": "grp-7"})
	if err != nil || !result.Success {
		t.Fatalf("document send %+v %v", result, err)
	}
	frame := server.sent("send_c2c_message")[0]
	fields, err := parseDict(frame.Vals["data"].([]byte))
	if err != nil {
		t.Fatal(err)
	}
	body := bodyElems(t, frame, 5)
	file := body[0].Vals["msg_content"].(*pyjson.Object)
	if len(body) != 2 || body[0].Vals["msg_type"] != "TIMFileElem" || file.GetOr("url", nil) != "https://cdn.example.com/res/1" ||
		file.GetOr("file_name", nil) != "renamed.txt" || !pyjson.Equal(file.GetOr("file_size", nil), int64(5)) || file.GetOr("uuid", nil) != MD5Hex([]byte("hello")) ||
		body[1].Vals["msg_content"].(*pyjson.Object).GetOr("text", nil) != "see attached" || fields.GetString(6) != "grp-7" {
		t.Fatalf("document body %v", body)
	}
	if got := backend.uploaded("/media/upload key.bin"); got != "hello" {
		t.Fatalf("uploaded %q", got)
	}
	if result, err := adapter.SendImageFile(ctx, "group:grp-1", image, "", "", nil, nil); err != nil || !result.Success {
		t.Fatalf("image send %+v %v", result, err)
	}
	picture := bodyElems(t, server.sent("send_group_message")[0], 6)[0]
	content := picture.Vals["msg_content"].(*pyjson.Object)
	info := content.GetOr("image_info_array", []any{}).([]any)
	if picture.Vals["msg_type"] != "TIMImageElem" || !pyjson.Equal(content.GetOr("image_format", nil), int64(3)) || len(info) != 1 ||
		!pyjson.Equal(info[0].(*pyjson.Object).GetOr("width", nil), int64(256)) || !pyjson.Equal(info[0].(*pyjson.Object).GetOr("height", nil), int64(128)) {
		t.Fatalf("image body %v", picture)
	}
	for path, want := range map[string]string{filepath.Join(dir, "missing.txt"): "File not found: " + filepath.Join(dir, "missing.txt"),
		filepath.Join(dir, "empty.txt"): "Empty file: empty.txt"} {
		if result, err := adapter.SendDocument(ctx, "direct:alice", path, "", "", "", nil, nil); err != nil || result.Success || result.Error != want {
			t.Fatalf("%s: %+v %v", path, result, err)
		}
	}
	direct, err := adapter.outbound.sender.sendDirect(ctx, "direct:alice", "note", []gateway.MediaRef{{Path: image}, {Path: document}})
	if err != nil || !reflect.DeepEqual(direct, map[string]any{"success": true, "platform": "yuanbao", "chat_id": "direct:alice", "message_id": ""}) {
		t.Fatalf("direct send %v %v", direct, err)
	}
	backend.setCredentials(credentials(nil))
	if result, err := adapter.SendDocument(ctx, "direct:alice", document, "", "", "", nil, nil); err != nil || result.Success || result.Error != "'region'" {
		t.Fatalf("credentials without a region: %+v %v", result, err)
	}
}

func TestValidateMediaRefusesEmptyAndOversizedFiles(t *testing.T) {
	if got := validateMedia(nil, "empty.bin"); got != "Empty file: empty.bin" {
		t.Fatalf("empty %q", got)
	}
	if got := validateMedia(make([]byte, mediaMaxSizeMB*1024*1024+1), "big.bin"); got != "File too large: big.bin (50.0MB > 50MB)" {
		t.Fatalf("oversized %q", got)
	}
	if got := validateMedia([]byte("x"), "ok.bin"); got != "" {
		t.Fatalf("valid %q", got)
	}
}
