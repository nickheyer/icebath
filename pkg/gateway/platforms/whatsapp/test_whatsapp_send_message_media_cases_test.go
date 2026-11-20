package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
)

// One recorded bridge call of an out of process send
type bridgeCall struct {
	url  string
	body map[string]any
}

// Answer of the recording transport
type cannedResp struct {
	status int
	body   string
}

// Transport recording every call, answering health with its own reply and posts from the list in order
type recordingTransport struct {
	calls  []bridgeCall
	posts  []cannedResp
	health cannedResp
	next   int
}

func (rt *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body map[string]any
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
	}
	rt.calls = append(rt.calls, bridgeCall{r.URL.String(), body})
	resp := rt.health
	if r.Method == http.MethodPost {
		resp = rt.posts[min(rt.next, len(rt.posts)-1)]
		rt.next++
	}
	return &http.Response{StatusCode: resp.status, Body: io.NopCloser(bytes.NewBufferString(resp.body)), Header: http.Header{}, Request: r}, nil
}

// Installs a recording transport for the out of process send
func recordSends(t *testing.T, health cannedResp, posts ...cannedResp) *recordingTransport {
	t.Helper()
	rt := &recordingTransport{posts: posts, health: health}
	saved := standaloneTransport
	standaloneTransport = rt
	t.Cleanup(func() { standaloneTransport = saved })
	return rt
}

// Temp file with the suffix
func tmpFile(t *testing.T, suffix string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f"+suffix)
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// URLs of the recorded calls
func callURLs(calls []bridgeCall) []string {
	out := []string{}
	for _, c := range calls {
		out = append(out, c.url)
	}
	return out
}

func TestBridgeMediaType(t *testing.T) {
	for _, tc := range []struct {
		path                   string
		isVoice, forceDocument bool
		want                   string
	}{
		{"a.png", false, false, "image"}, {"a.JPG", false, false, "image"}, {"a.jpeg", false, false, "image"},
		{"a.webp", false, false, "image"}, {"a.gif", false, false, "image"}, {"a.mp4", false, false, "video"},
		{"a.mov", false, false, "video"}, {"a.webm", false, false, "video"}, {"a.ogg", true, false, "audio"},
		{"a.opus", false, false, "audio"}, {"a.mp3", false, false, "audio"}, {"a.wav", false, false, "audio"},
		{"a.pdf", false, false, "document"}, {"a.zip", false, false, "document"}, {"a.png", false, true, "document"},
		{"a.mp4", false, true, "document"}, {"a.mp4", true, false, "audio"},
	} {
		if got := bridgeMediaType(tc.path, tc.isVoice, tc.forceDocument); got != tc.want {
			t.Fatalf("%s voice=%v doc=%v = %q", tc.path, tc.isVoice, tc.forceDocument, got)
		}
	}
}

func TestTextPlusMixedMediaRoutesNativeTypes(t *testing.T) {
	hermeticHome(t)
	img, vid, voice := tmpFile(t, ".png"), tmpFile(t, ".mp4"), tmpFile(t, ".ogg")
	rt := recordSends(t, cannedResp{200, "{}"}, cannedResp{200, `{"messageId":"t1"}`}, cannedResp{200, `{"messageId":"m1"}`},
		cannedResp{200, `{"messageId":"m2"}`}, cannedResp{200, `{"messageId":"m3"}`})
	res, err := standaloneSend(context.Background(), map[string]any{"pconfig": &gateway.PlatformConfig{Extra: map[string]any{"bridge_port": 3000}},
		"chat_id": "12345", "message": "hello", "media_files": []gateway.MediaRef{{Path: img}, {Path: vid}, {Path: voice, IsVoice: true}}})
	if err != nil || res["success"] != true {
		t.Fatalf("res = %v %v", res, err)
	}
	if !strings.HasSuffix(rt.calls[0].url, "/health") || !strings.HasSuffix(rt.calls[1].url, "/send") || rt.calls[1].body["message"] != "hello" {
		t.Fatalf("calls = %v", rt.calls)
	}
	kinds := []string{}
	for _, c := range rt.calls {
		if strings.HasSuffix(c.url, "/send-media") {
			kinds = append(kinds, c.body["mediaType"].(string))
		}
	}
	if !slices.Equal(kinds, []string{"image", "video", "audio"}) || !strings.Contains(rt.calls[1].body["chatId"].(string), "@") {
		t.Fatalf("kinds = %v chat = %v", kinds, rt.calls[1].body["chatId"])
	}
}

func TestMissingCaptionedFileFallsBackToText(t *testing.T) {
	hermeticHome(t)
	rt := recordSends(t, cannedResp{200, "{}"}, cannedResp{200, `{"messageId":"t1"}`})
	res, _ := standaloneSend(context.Background(), map[string]any{"pconfig": &gateway.PlatformConfig{Extra: map[string]any{"bridge_port": 3000}},
		"chat_id": "12345", "message": "", "media_files": []gateway.MediaRef{{Path: "/no/such/file.png"}}, "caption": "floor plan"})
	if e, _ := res["error"].(string); !strings.Contains(e, "not found") {
		t.Fatalf("res = %v", res)
	}
	if !slices.Equal(callURLs(rt.calls), []string{"http://localhost:3000/health", "http://localhost:3000/send"}) || rt.calls[1].body["message"] != "floor plan" {
		t.Fatalf("calls = %v", rt.calls)
	}
}

func TestStandaloneSendUsesPersistedSecondaryBridgePort(t *testing.T) {
	h := hermeticHome(t)
	record := filepath.Join(h, "platforms", "whatsapp", "bridge_port")
	if err := os.MkdirAll(filepath.Dir(record), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(record, []byte("3042"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := recordSends(t, cannedResp{200, "{}"}, cannedResp{200, `{"messageId":"t1"}`})
	res, _ := standaloneSend(context.Background(), map[string]any{"pconfig": &gateway.PlatformConfig{Extra: map[string]any{}}, "chat_id": "12345", "message": "hello"})
	if res["success"] != true || !slices.Equal(callURLs(rt.calls), []string{"http://localhost:3042/health", "http://localhost:3042/send"}) {
		t.Fatalf("res = %v calls = %v", res, rt.calls)
	}
}

func TestSecondaryStandaloneSendsUseActiveProfilePortForTextMediaAndMentions(t *testing.T) {
	for _, tc := range []struct {
		id        string
		persisted int
		extra     map[string]any
		want      int
	}{{"persisted", 3057, map[string]any{}, 3057}, {"explicit-wins", 3057, map[string]any{"bridge_port": 3061}, 3061}, {"unallocated-default", 0, map[string]any{}, 3000}} {
		t.Run(tc.id, func(t *testing.T) {
			root := t.TempDir()
			launch, secondary := filepath.Join(root, "launch"), filepath.Join(root, "secondary")
			record := filepath.Join(secondary, "platforms", "whatsapp", "bridge_port")
			if err := os.MkdirAll(secondary, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.persisted != 0 {
				if err := os.MkdirAll(filepath.Dir(record), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(record, []byte(strconv.Itoa(tc.persisted)), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("ICEBATH_HOME", launch)
			media := filepath.Join(root, "image.png")
			if err := os.WriteFile(media, []byte("image"), 0o644); err != nil {
				t.Fatal(err)
			}
			for _, step := range []struct {
				h     string
				extra map[string]any
				port  int
			}{{launch, map[string]any{}, 3000}, {secondary, tc.extra, tc.want}, {launch, map[string]any{}, 3000}} {
				rt := recordSends(t, cannedResp{200, `{"capabilities":{"outboundMentions":true}}`}, cannedResp{200, `{"messageId":"text"}`}, cannedResp{200, `{"messageId":"media"}`})
				ctx := home.WithOverride(context.Background(), step.h)
				res, _ := standaloneSend(ctx, map[string]any{"pconfig": &gateway.PlatformConfig{Extra: step.extra}, "chat_id": "12345", "message": "hello",
					"media_files": []gateway.MediaRef{{Path: media}}, "mentions": []string{"12345"}})
				if res["success"] != true {
					t.Fatalf("res = %v", res)
				}
				p := "http://localhost:" + strconv.Itoa(step.port)
				if !slices.Equal(callURLs(rt.calls), []string{p + "/health", p + "/send", p + "/send-media"}) {
					t.Fatalf("calls = %v", callURLs(rt.calls))
				}
				if m, _ := rt.calls[1].body["mentions"].([]any); len(m) != 1 || m[0] != "12345@s.whatsapp.net" {
					t.Fatalf("mentions = %v", rt.calls[1].body["mentions"])
				}
				if rt.calls[2].body["filePath"] != media {
					t.Fatalf("filePath = %v", rt.calls[2].body["filePath"])
				}
			}
			if _, err := os.Stat(filepath.Join(launch, "platforms", "whatsapp", "bridge_port")); err == nil {
				t.Fatal("launch profile allocated a port")
			}
			raw, err := os.ReadFile(record)
			if tc.persisted == 0 && err == nil {
				t.Fatal("unallocated profile gained a record")
			}
			if tc.persisted != 0 && string(raw) != strconv.Itoa(tc.persisted) {
				t.Fatalf("record = %q", raw)
			}
		})
	}
}

func TestStandaloneSendPostsOnlyThroughThisProfilesBridge(t *testing.T) {
	for _, reported := range []string{"own", "other", "", "unhealthy"} {
		t.Run("reported="+reported, func(t *testing.T) {
			hermeticHome(t)
			root := t.TempDir()
			own := filepath.Join(root, "b", "session")
			media := filepath.Join(root, "image.png")
			if err := os.WriteFile(media, []byte("image"), 0o644); err != nil {
				t.Fatal(err)
			}
			health := cannedResp{200, "{}"}
			switch reported {
			case "unhealthy":
				health = cannedResp{503, ""}
			case "own":
				health = cannedResp{200, `{"session":` + strconv.Quote(own) + `}`}
			case "other":
				health = cannedResp{200, `{"session":` + strconv.Quote(filepath.Join(root, "a", "session")) + `}`}
			}
			rt := recordSends(t, health, cannedResp{200, `{"messageId":"text"}`}, cannedResp{200, `{"messageId":"media"}`})
			res, _ := standaloneSend(context.Background(), map[string]any{"pconfig": &gateway.PlatformConfig{Extra: map[string]any{"bridge_port": 3000, "session_path": own}},
				"chat_id": "12345", "message": "hello", "media_files": []gateway.MediaRef{{Path: media}}})
			posted := []string{}
			for _, u := range callURLs(rt.calls) {
				if !strings.HasSuffix(u, "/health") {
					posted = append(posted, u)
				}
			}
			if reported == "other" || reported == "unhealthy" {
				if e, _ := res["error"].(string); !strings.Contains(e, "nothing was sent") || len(posted) != 0 {
					t.Fatalf("res = %v posted = %v", res, posted)
				}
				return
			}
			if res["success"] != true || !slices.Equal(posted, []string{"http://localhost:3000/send", "http://localhost:3000/send-media"}) {
				t.Fatalf("res = %v posted = %v", res, posted)
			}
		})
	}
}
