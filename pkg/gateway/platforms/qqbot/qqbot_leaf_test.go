package qqbot

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestCoerceListNone(t *testing.T) {
	if got := CoerceList(nil); len(got) != 0 || got == nil {
		t.Fatalf("got %#v", got)
	}
}

func TestCoerceListString(t *testing.T) {
	if got := CoerceList("a, b ,c"); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("got %#v", got)
	}
}

func TestReadChunkShortReadRaises(t *testing.T) {
	f := filepath.Join(t.TempDir(), "x.bin")
	if err := os.WriteFile(f, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readFileChunk(f, 0, 100); err == nil {
		t.Fatal("short read accepted")
	}
}

func TestParsePrepareResponseWrappedInData(t *testing.T) {
	raw, _ := pyjson.Loads(`{"data": {"upload_id": "uid-42", "block_size": 4096, "parts": [
		{"part_index": 1, "presigned_url": "https://cos/1", "block_size": 4096}, {"index": 2, "url": "https://cos/2"}],
		"concurrency": 3, "retry_timeout": 90}}`)
	r, err := parsePrepareResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if r.uploadID != "uid-42" || r.blockSize != 4096 || len(r.parts) != 2 || r.parts[0].presignedURL != "https://cos/1" ||
		r.parts[1].index != 2 || r.concurrency != 3 || r.retryTimeout != 90.0 {
		t.Fatalf("got %+v", r)
	}
}

type apiCall struct {
	method, path string
	body         *pyjson.Object
}

func TestFullUploadTwoPartsSuccess(t *testing.T) {
	f := filepath.Join(t.TempDir(), "vid.mp4")
	if err := os.WriteFile(f, append([]byte(strings.Repeat("A", 5_000_000)), strings.Repeat("B", 3_000_000)...), 0o644); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var apiCalls []apiCall
	api := func(_ context.Context, method, path string, body *pyjson.Object, _ float64) (any, error) {
		mu.Lock()
		apiCalls = append(apiCalls, apiCall{method, path, body})
		mu.Unlock()
		switch {
		case strings.HasSuffix(path, "/upload_prepare"):
			return pyjson.Loads(`{"upload_id": "uid-xyz", "block_size": 5000000, "parts": [
				{"part_index": 1, "presigned_url": "https://cos.example/p1"}, {"part_index": 2, "presigned_url": "https://cos.example/p2"}],
				"concurrency": 1}`)
		case strings.HasSuffix(path, "/upload_part_finish"):
			return pyjson.NewObject(), nil
		}
		return pyjson.Loads(`{"file_info": "FILEINFO_TOKEN", "file_uuid": "u-1"}`)
	}
	type putCall struct {
		url     string
		size    int
		headers map[string]string
	}
	var puts []putCall
	put := func(_ context.Context, url string, data []byte, headers map[string]string) (PutResponse, error) {
		mu.Lock()
		puts = append(puts, putCall{url, len(data), headers})
		mu.Unlock()
		return PutResponse{StatusCode: 200}, nil
	}
	result, err := NewChunkedUploader(api, put, "QQBot:TEST").Upload(context.Background(), "c2c", "user-openid-1", f, MediaTypeVideo, "vid.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := result.(*pyjson.Object).Get("file_info"); v != "FILEINFO_TOKEN" {
		t.Fatalf("file_info = %v", v)
	}
	if len(puts) != 2 || puts[0].url != "https://cos.example/p1" || puts[1].url != "https://cos.example/p2" {
		t.Fatalf("puts = %+v", puts)
	}
	if puts[0].size != 5_000_000 || puts[1].size != 3_000_000 || puts[1].headers["Content-Length"] != "3000000" {
		t.Fatalf("put sizes = %+v", puts)
	}
	if len(apiCalls) != 4 || !strings.HasSuffix(apiCalls[0].path, "/upload_prepare") ||
		!strings.HasSuffix(apiCalls[1].path, "/upload_part_finish") || !strings.HasSuffix(apiCalls[2].path, "/upload_part_finish") ||
		!strings.HasSuffix(apiCalls[3].path, "/files") {
		t.Fatalf("api calls = %+v", apiCalls)
	}
	if body, _ := pyjson.Dumps(apiCalls[3].body); body != `{"upload_id": "uid-xyz"}` {
		t.Fatalf("complete body = %s", body)
	}
}

func TestGroupPaths(t *testing.T) {
	f := filepath.Join(t.TempDir(), "a.bin")
	if err := os.WriteFile(f, []byte(strings.Repeat("x", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	var seen []string
	api := func(_ context.Context, _, path string, _ *pyjson.Object, _ float64) (any, error) {
		seen = append(seen, path)
		switch {
		case strings.HasSuffix(path, "/upload_prepare"):
			return pyjson.Loads(`{"upload_id": "gid-1", "block_size": 100, "parts": [{"part_index": 1, "presigned_url": "https://cos/g1"}]}`)
		case strings.HasSuffix(path, "/upload_part_finish"):
			return pyjson.NewObject(), nil
		}
		return pyjson.Loads(`{"file_info": "GFILE"}`)
	}
	put := func(context.Context, string, []byte, map[string]string) (PutResponse, error) {
		return PutResponse{StatusCode: 200}, nil
	}
	if _, err := NewChunkedUploader(api, put, "QQBot:T").Upload(context.Background(), "group", "grp-openid-1", f, 4, "a.bin"); err != nil {
		t.Fatal(err)
	}
	prep, files := false, false
	for _, p := range seen {
		if !strings.Contains(p, "/v2/groups/") {
			t.Fatalf("path %s", p)
		}
		prep = prep || strings.HasSuffix(p, "/upload_prepare")
		files = files || strings.HasSuffix(p, "/files")
	}
	if !prep || !files {
		t.Fatalf("paths = %v", seen)
	}
}

func TestPrepareDailyLimitBecomesTypedError(t *testing.T) {
	f := filepath.Join(t.TempDir(), "a.bin")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	api := func(context.Context, string, string, *pyjson.Object, float64) (any, error) {
		return nil, &RuntimeError{"API error biz_code=40093002"}
	}
	_, err := NewChunkedUploader(api, nil, "T").Upload(context.Background(), "c2c", "u", f, 4, "a.bin")
	limit, ok := err.(*UploadDailyLimitExceededError)
	if !ok || limit.FileName != "a.bin" || limit.FileSizeHuman() != "1.0 B" {
		t.Fatalf("err = %#v", err)
	}
	if _, err := NewChunkedUploader(api, nil, "T").Upload(context.Background(), "guild", "u", f, 4, "a.bin"); err == nil || err.Error() != "ChunkedUploader: unsupported chat_type 'guild'" {
		t.Fatalf("err = %v", err)
	}
	if got := NewUploadFileTooLargeError("b", 2048, 1024, "").Error(); got != "File 'b' (2.0 KB) exceeds platform limit (1.0 KB)" {
		t.Fatalf("too large = %s", got)
	}
}

func TestApprovalButtonDataParse(t *testing.T) {
	key, decision, ok := ParseApprovalButtonData("approve:agent:main:qqbot:c2c:UID:allow-once")
	if !ok || key != "agent:main:qqbot:c2c:UID" || decision != "allow-once" {
		t.Fatalf("got %q %q %v", key, decision, ok)
	}
	if _, _, ok := ParseApprovalButtonData(""); ok {
		t.Fatal("empty data parsed")
	}
}

func TestUpdatePromptButtonDataParseYes(t *testing.T) {
	if v, ok := ParseUpdatePromptButtonData("update_prompt:y"); !ok || v != "y" {
		t.Fatalf("got %q %v", v, ok)
	}
}

func TestBuildApprovalKeyboardEmbedsSessionKey(t *testing.T) {
	kb := BuildApprovalKeyboard("agent:main:qqbot:c2c:UID", true)
	var datas []string
	for _, b := range kb.Content.Rows[0].Buttons {
		datas = append(datas, b.Action.Data)
	}
	want := []string{"approve:agent:main:qqbot:c2c:UID:allow-once", "approve:agent:main:qqbot:c2c:UID:allow-always", "approve:agent:main:qqbot:c2c:UID:deny"}
	if !reflect.DeepEqual(datas, want) {
		t.Fatalf("datas = %v", datas)
	}
	got, _ := pyjson.Dumps(BuildUpdatePromptKeyboard().ToDict(), pyjson.EnsureASCII(false))
	if !strings.HasPrefix(got, `{"content": {"rows": [{"buttons": [{"id": "yes", "render_data": {"label": "✓ 确认", "visited_label": "已确认", "style": 1}, "action": {"type": 1, "data": "update_prompt:y", "permission": {"type": 2}, "click_limit": 1}, "group_id": "update_prompt"}`) {
		t.Fatalf("dict = %s", got)
	}
}

func TestBuildApprovalTextTruncatesLongCommands(t *testing.T) {
	req := NewApprovalRequest("s", "t")
	req.CommandPreview, req.Cwd = strings.Repeat("x", 1000), "/x"
	xs := 0
	for _, line := range strings.Split(BuildApprovalText(req), "\n") {
		if line != "" && !strings.Contains(line, "```") {
			xs += strings.Count(line, "x")
		}
	}
	if xs > 301 {
		t.Fatalf("preview kept %d xs", xs)
	}
}

func TestParseC2CInteraction(t *testing.T) {
	raw, _ := pyjson.Loads(`{"id": "interaction-42", "chat_type": 2, "user_openid": "user-1",
		"data": {"type": 11, "resolved": {"button_data": "approve:sess:allow-once", "button_id": "allow"}}}`)
	ev, err := ParseInteractionEvent(raw)
	if err != nil {
		t.Fatal(err)
	}
	if ev.ID != "interaction-42" || ev.Scene != "c2c" || ev.ChatType != 2 || ev.UserOpenID != "user-1" ||
		ev.ButtonData != "approve:sess:allow-once" || ev.ButtonID != "allow" || ev.OperatorOpenID() != "user-1" || ev.Type != 11 {
		t.Fatalf("got %+v", ev)
	}
}

func TestDecryptSecretRoundTrip(t *testing.T) {
	keyB64, err := GenerateBindKey()
	if err != nil {
		t.Fatal(err)
	}
	key, _ := base64.StdEncoding.DecodeString(keyB64)
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := []byte("123456789012")
	sealed := append(append([]byte{}, nonce...), gcm.Seal(nil, nonce, []byte("s3cret"), nil)...)
	if got, err := DecryptSecret(base64.StdEncoding.EncodeToString(sealed), keyB64); err != nil || got != "s3cret" {
		t.Fatalf("got %q, %v", got, err)
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := DecryptSecret(base64.StdEncoding.EncodeToString(sealed), keyB64); err != ErrInvalidTag {
		t.Fatalf("tampered err = %v", err)
	}
}
