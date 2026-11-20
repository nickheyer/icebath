package media

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

func TestVideoInputFromPublicURLUsesURLField(t *testing.T) {
	u := "https://files-cdn.x.ai/kRQVP6PRQlioVAUNC3GAdg/file_1faca9c3-9411-46ad-bb41-b9b8527789e6.mp4"
	got, err := videoInputFromPublicURL(nil, u)
	if err != nil || !reflect.DeepEqual(got, map[string]any{"url": u}) {
		t.Fatal(got, err)
	}
}

func TestXAIVideoImageInputBlocksCredentialStoreSymlink(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	os.MkdirAll(h, 0o755)
	auth := filepath.Join(h, "auth.json")
	os.WriteFile(auth, []byte(`{"api_key":"sk-secret"}`), 0o600)
	link := filepath.Join(h, "leak.png")
	if err := os.Symlink(auth, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	_, err := imageRefToXAIInput(nil, link)
	if _, ok := err.(*imageValueError); !ok || !strings.Contains(err.Error(), "credential store") {
		t.Fatalf("%T %v", err, err)
	}
}

// Recording transport answering submits with a request id and polls with a finished job
type fakeXAIVideoClient struct {
	mu    sync.Mutex
	posts []orCall
}

func (c *fakeXAIVideoClient) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	reply := func(body string) *http.Response {
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: r}
	}
	if r.Method == http.MethodPost {
		raw, _ := io.ReadAll(r.Body)
		parsed, _ := pyjson.Loads(string(raw))
		m, _ := pyjson.Plain(parsed).(map[string]any)
		c.posts = append(c.posts, orCall{url: r.URL.String(), headers: r.Header.Clone(), json: m})
		return reply(`{"request_id": "req-123"}`), nil
	}
	model, _ := pyjson.Dumps(c.posts[len(c.posts)-1].json["model"])
	return reply(`{"status": "done", "video": {"url": "https://xai-cdn/out.mp4", "duration": 8}, "model": ` + model + `}`), nil
}

func (c *fakeXAIVideoClient) last(t *testing.T) orCall {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.posts) == 0 {
		t.Fatal("no submit")
	}
	return c.posts[len(c.posts)-1]
}

// xAI provider with a test key, the recording transport and no poll sleep
func xaiVideoProvider(t *testing.T) (*xaiVideoGenProvider, *fakeXAIVideoClient) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("XAI_API_KEY", "test-key")
	fake := &fakeXAIVideoClient{}
	prevClient, prevSleep := xaiVideoHTTPClient, xaiVideoSleep
	xaiVideoHTTPClient = func() *http.Client { return &http.Client{Transport: fake} }
	xaiVideoSleep = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(func() { xaiVideoHTTPClient, xaiVideoSleep = prevClient, prevSleep })
	return newXAIVideoGenProvider(), fake
}

func TestXAIEndpointTextToVideoHitsGenerations(t *testing.T) {
	p, fake := xaiVideoProvider(t)
	res, err := p.Generate(context.Background(), nil, VideoGenRequest{Prompt: "a dog on a skateboard"})
	if err != nil || res["success"] != true || res["modality"] != "text" || !strings.HasSuffix(fake.last(t).url, "/videos/generations") {
		t.Fatal(res, err)
	}
}

func TestXAIEndpointImageToVideoHitsGenerations(t *testing.T) {
	p, fake := xaiVideoProvider(t)
	res, err := p.Generate(context.Background(), nil, VideoGenRequest{Prompt: "animate this", ImageURL: "https://example.com/cat.png"})
	if err != nil || res["success"] != true || res["modality"] != "image" || !strings.HasSuffix(fake.last(t).url, "/videos/generations") {
		t.Fatal(res, err)
	}
}

func TestXAIPayloadLocalImagePathIsSentAsDataURI(t *testing.T) {
	p, fake := xaiVideoProvider(t)
	img := filepath.Join(t.TempDir(), "frame.png")
	os.WriteFile(img, []byte("\x89PNG\r\n\x1a\nfake"), 0o644)
	p.Generate(context.Background(), nil, VideoGenRequest{Prompt: "animate this", ImageURL: img})
	image, _ := fake.last(t).json["image"].(map[string]any)
	if u, _ := image["url"].(string); !strings.HasPrefix(u, "data:image/png;base64,") {
		t.Fatal(fake.last(t).json)
	}
}

func TestXAIPayloadReferenceImagesPayload(t *testing.T) {
	p, fake := xaiVideoProvider(t)
	p.Generate(context.Background(), nil, VideoGenRequest{Prompt: "keep this character",
		ReferenceImageURLs: []string{"https://example.com/a.png", "https://example.com/b.png"}})
	want := []any{map[string]any{"url": "https://example.com/a.png"}, map[string]any{"url": "https://example.com/b.png"}}
	if !reflect.DeepEqual(fake.last(t).json["reference_images"], want) {
		t.Fatal(fake.last(t).json)
	}
}

func TestXAIValidationMissingPromptRejects(t *testing.T) {
	p, fake := xaiVideoProvider(t)
	res, err := p.Generate(context.Background(), nil, VideoGenRequest{Prompt: ""})
	if err != nil || res["success"] != false || res["error_type"] != "missing_prompt" || len(fake.posts) != 0 {
		t.Fatal(res, err, fake.posts)
	}
}

func TestXAIValidationTooManyReferencesRejects(t *testing.T) {
	p, _ := xaiVideoProvider(t)
	var refs []string
	for i := range 8 {
		refs = append(refs, "https://example.com/r"+string(rune('0'+i))+".png")
	}
	res, err := p.Generate(context.Background(), nil, VideoGenRequest{Prompt: "x", ReferenceImageURLs: refs})
	if err != nil || res["success"] != false || res["error_type"] != "too_many_references" {
		t.Fatal(res, err)
	}
}

func TestXAIClampingDurationClampedTo15(t *testing.T) {
	p, fake := xaiVideoProvider(t)
	p.Generate(context.Background(), nil, VideoGenRequest{Prompt: "x", Duration: iptr(30)})
	if !pyrepr.Equal(fake.last(t).json["duration"], 15) {
		t.Fatal(fake.last(t).json)
	}
}

func TestXAIClampingInvalidAspectRatioSoftClamps(t *testing.T) {
	p, fake := xaiVideoProvider(t)
	p.Generate(context.Background(), nil, VideoGenRequest{Prompt: "x", AspectRatio: "21:9"})
	if fake.last(t).json["aspect_ratio"] != "16:9" {
		t.Fatal(fake.last(t).json)
	}
}
