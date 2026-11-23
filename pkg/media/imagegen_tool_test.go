package media

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestImage25SelectionRoutesGenerationAndEdits(t *testing.T) {
	for _, variant := range []string{"flare", "sunburst"} {
		for aspect, size := range map[string]string{"landscape": "landscape_4_3", "square": "square_hd", "portrait": "portrait_4_3"} {
			model := "openai/gpt-image-2.5/" + variant + "/text-to-image"
			t.Setenv("FAL_KEY", "test-key")
			env := &tools.Env{Home: t.TempDir(), Config: config.Tree{"image_gen": map[string]any{"model": model}}}
			selected, meta := resolveFalModel(env)
			if selected != model {
				t.Fatalf("selected %s", selected)
			}
			var refs []string
			for i := range 17 {
				refs = append(refs, fmt.Sprintf("https://example.com/%d.png", i))
			}
			seed := 42
			for _, c := range []struct {
				sources  []string
				endpoint string
			}{{nil, model}, {refs, "openai/gpt-image-2.5/" + variant + "/edit"}} {
				actual, payload, err := prepareFalRequest(context.Background(), env, selected, meta, "a cup", aspect, seed, map[string]any{"guidance_scale": 9}, c.sources)
				if err != nil || actual != c.endpoint {
					t.Fatalf("%s %v", actual, err)
				}
				if payload["quality"] != "medium" || payload["image_size"] != size {
					t.Fatalf("payload %v", payload)
				}
				if _, ok := payload["seed"]; ok {
					t.Fatal("seed kept")
				}
				if _, ok := payload["guidance_scale"]; ok {
					t.Fatal("guidance kept")
				}
				urls, _ := payload["image_urls"].([]any)
				want := len(c.sources)
				if want > 16 {
					want = 16
				}
				if len(urls) != want {
					t.Fatalf("image_urls %d", len(urls))
				}
			}
			if meta["upscale"] != false {
				t.Fatal("upscale default")
			}
		}
	}
}

func TestFalCatalogEntriesHaveRequiredKeys(t *testing.T) {
	for id, meta := range falModels {
		for _, k := range []string{"display", "speed", "strengths", "price", "size_style", "sizes", "defaults", "supports", "upscale"} {
			if _, ok := meta[k]; !ok {
				t.Fatalf("%s lacks %s", id, k)
			}
		}
		if ep, ok := meta["edit_endpoint"].(string); ok {
			es, _ := meta["edit_supports"].(map[string]bool)
			param, _ := meta["edit_image_param"].(string)
			if param == "" {
				param = "image_urls"
			}
			if ep == "" || !es["prompt"] || !es[param] || meta["max_reference_images"].(int) < 1 {
				t.Fatalf("%s edit contract incomplete", id)
			}
		}
	}
}

func TestFalPayloadSizeFamiliesAndFilters(t *testing.T) {
	p, _ := buildFalPayload("fal-ai/flux-2/klein/9b", "hello", "landscape", nil, nil)
	if p["image_size"] != "landscape_16_9" {
		t.Fatalf("klein %v", p)
	}
	p, _ = buildFalPayload("fal-ai/nano-banana-pro", "hello", "landscape", nil, nil)
	if p["aspect_ratio"] != "16:9" || p["image_size"] != nil {
		t.Fatalf("nano %v", p)
	}
	p, _ = buildFalPayload("fal-ai/gpt-image-1.5", "hello", "landscape", nil, nil)
	if p["image_size"] != "1536x1024" {
		t.Fatalf("gpt %v", p)
	}
	p, _ = buildFalPayload("fal-ai/gpt-image-2", "hi", "square", nil, map[string]any{"openai_api_key": "sk-...", "guidance_scale": 7.5, "num_inference_steps": 50})
	for _, k := range []string{"openai_api_key", "guidance_scale", "num_inference_steps"} {
		if _, ok := p[k]; ok {
			t.Fatalf("gpt2 kept %s", k)
		}
	}
	p, _ = buildFalPayload("fal-ai/gpt-image-2", "hi", "landscape", nil, nil)
	if p["image_size"] != "landscape_4_3" {
		t.Fatalf("gpt2 %v", p)
	}
	seed := 7
	for id, meta := range falModels {
		p, err := buildFalPayload(id, "test", "landscape", &seed, map[string]any{"num_images": 2, "bogus": 1})
		if err != nil {
			t.Fatal(err)
		}
		sup := meta["supports"].(map[string]bool)
		for k := range p {
			if !sup[k] && k != "prompt" {
				t.Fatalf("%s sent %s", id, k)
			}
		}
	}
	p, _ = buildFalPayload("fal-ai/flux-2-pro", "x", "square", nil, map[string]any{"num_inference_steps": nil, "guidance_scale": nil})
	if p["num_inference_steps"] != 50 || p["guidance_scale"] != 4.5 {
		t.Fatalf("defaults replaced %v", p)
	}
	p, _ = buildFalPayload("fal-ai/flux-2/klein/9b", "x", "WIDE", nil, nil)
	if p["image_size"] != "landscape_16_9" {
		t.Fatalf("invalid aspect %v", p)
	}
	p, _ = buildFalEditPayload("fal-ai/kling-image/v3/text-to-image", "x", []string{"https://a/1.png", "https://a/2.png"}, "square", nil, nil)
	if p["image_url"] != "https://a/1.png" || p["aspect_ratio"] != "1:1" {
		t.Fatalf("kling edit %v", p)
	}
}

func TestFalModelResolution(t *testing.T) {
	env := &tools.Env{Home: t.TempDir(), Config: config.Tree{}}
	if id, _ := resolveFalModel(env); id != falDefaultModel {
		t.Fatal(id)
	}
	env.Config = config.Tree{"image_gen": map[string]any{"model": "fal-ai/flux-2-pro"}}
	if id, _ := resolveFalModel(env); id != "fal-ai/flux-2-pro" {
		t.Fatal(id)
	}
	env.Config = config.Tree{"image_gen": map[string]any{"model": "nope/model"}}
	if id, _ := resolveFalModel(env); id != falDefaultModel {
		t.Fatal(id)
	}
}

func TestImageWaitFalResultPollsAtQueueCadence(t *testing.T) {
	f := newFalFixture(t)
	f.replies = append(f.replies, f.accepted(), falReply{status: 200, body: map[string]any{"status": "IN_QUEUE", "queue_position": 0}},
		falReply{status: 200, body: map[string]any{"status": "IN_PROGRESS", "logs": nil}},
		falReply{status: 200, body: map[string]any{"status": "COMPLETED", "logs": nil}},
		falReply{status: 200, body: map[string]any{"status": "COMPLETED", "logs": nil}},
		falReply{status: 200, body: map[string]any{"images": []any{"done"}}})
	h, err := falSubmitRequest(context.Background(), f.mod.Client, f.mod.NewSyncClient("token"), f.srv.URL+"/app", map[string]any{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := waitFalResult(context.Background(), h, 0.01)
	if err != nil || got.(map[string]any)["images"].([]any)[0] != "done" {
		t.Fatalf("%v %v", got, err)
	}
	if len(f.sleeps) != 2 || f.sleeps[0] != falQueuePollSeconds || f.sleeps[1] != falQueuePollSeconds {
		t.Fatalf("sleeps %v", f.sleeps)
	}
}

func TestImageWaitFalResultAbandonsJobOnInterrupt(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	in := newFalClientInternals()
	in.MaybeRetryRequest = func(context.Context, *http.Client, string, string, falHTTPRequest) (*falResponse, error) {
		<-release
		return nil, errors.New("released")
	}
	h := in.SyncRequestHandle("r1", "https://q/result", "https://q/status", "https://q/cancel", http.DefaultClient)
	id := tools.NewWorkerID()
	ctx := tools.WithWorker(context.Background(), id)
	tools.SetInterrupt(id, true, "user")
	t.Cleanup(func() { tools.SetInterrupt(id, false, "") })
	_, err := waitFalResult(ctx, h, 0.01)
	var intr *imageGenerationInterrupted
	if !errors.As(err, &intr) || pyErrorTypeName(err) != "ImageGenerationInterrupted" || !strings.Contains(err.Error(), "interrupted by user") {
		t.Fatalf("got %v", err)
	}
}

// Request handle whose polls block until the test releases them, or fail with err
func blockingFalHandle(t *testing.T, err error) *falRequestHandle {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	in := newFalClientInternals()
	in.MaybeRetryRequest = func(context.Context, *http.Client, string, string, falHTTPRequest) (*falResponse, error) {
		if err != nil {
			return nil, err
		}
		<-release
		return nil, errors.New("released")
	}
	return in.SyncRequestHandle("r1", "https://q/result", "https://q/status", "https://q/cancel", http.DefaultClient)
}

func TestImageWaitFalResultPropagatesHandlerError(t *testing.T) {
	boom := &imageValueError{"boom"}
	if _, err := waitFalResult(context.Background(), blockingFalHandle(t, boom), 0.01); err != boom {
		t.Fatalf("got %v", err)
	}
}

func TestImageUpscaleInterruptPropagates(t *testing.T) {
	mod := newFalClientModule()
	mod.Submit = func(context.Context, *tools.Env, string, map[string]any, map[string]string) (*falRequestHandle, error) {
		return blockingFalHandle(t, nil), nil
	}
	imageFalClientMu.Lock()
	prev := imageFalClient
	imageFalClient = mod
	imageFalClientMu.Unlock()
	t.Cleanup(func() { imageFalClientMu.Lock(); imageFalClient = prev; imageFalClientMu.Unlock() })
	id := tools.NewWorkerID()
	ctx := tools.WithWorker(context.Background(), id)
	go func() {
		time.Sleep(200 * time.Millisecond)
		tools.SetInterrupt(id, true, "user")
	}()
	t.Cleanup(func() { tools.SetInterrupt(id, false, "") })
	up, err := upscaleImage(ctx, &tools.Env{Home: t.TempDir(), Config: config.Tree{}}, "https://example.com/x.png", "prompt")
	var intr *imageGenerationInterrupted
	if up != nil || !errors.As(err, &intr) {
		t.Fatalf("got %v %v", up, err)
	}
}
