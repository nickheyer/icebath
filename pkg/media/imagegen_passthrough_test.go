package media

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestFalPluginForwardsMistypedPassthroughVerbatim(t *testing.T) {
	t.Setenv("FAL_KEY", "test-key")
	env := imageEnv(t, config.Tree{"image_gen": map[string]any{"model": "fal-ai/flux-2-pro"}})
	capture := captureFalSubmit(t)
	upscaled := false
	prev := upscaleImageHook
	upscaleImageHook = func(context.Context, *tools.Env, string, string) (map[string]any, error) {
		upscaled = true
		return nil, nil
	}
	t.Cleanup(func() { upscaleImageHook = prev })
	kw := map[string]any{"num_inference_steps": 30.5, "guidance_scale": "high", "num_images": 2.5,
		"output_format": 7, "seed": 3.5, "upscale": "yes"}
	res, err := newFalImageGenProvider().Generate(context.Background(), env, ImageGenRequest{Prompt: "a cat", AspectRatio: "square", Kwargs: kw})
	if err != nil || res["success"] != true {
		t.Fatalf("%v %v", res, err)
	}
	args := (*capture)["arguments"].(map[string]any)
	for _, k := range []string{"num_inference_steps", "guidance_scale", "num_images", "output_format"} {
		if !reflect.DeepEqual(args[k], kw[k]) {
			t.Fatalf("%s %#v", k, args[k])
		}
	}
	if _, ok := args["seed"]; ok || !upscaled {
		t.Fatalf("float seed reached the payload or truthy upscale skipped %v %v", args, upscaled)
	}
}

func TestFalPayloadSeedFollowsIsinstanceInt(t *testing.T) {
	for _, c := range []struct {
		seed any
		want bool
	}{{7, true}, {int64(9), true}, {true, true}, {7.0, false}, {"7", false}, {nil, false}} {
		p, err := buildFalPayload("fal-ai/flux-2-pro", "a cat", "square", c.seed, nil)
		_, has := p["seed"]
		if err != nil || has != c.want || (has && p["seed"] != c.seed) {
			t.Fatalf("%#v %v %v", c.seed, p, err)
		}
	}
}

func TestImageToolUpscaleUsesPythonTruthiness(t *testing.T) {
	for _, c := range []struct {
		upscale any
		called  bool
	}{{0, false}, {"", false}, {1, true}, {[]any{1}, true}} {
		_, calls := runUpscaleOptIn(t, "fal-ai/flux-2/klein/9b", c.upscale, map[string]any{"url": "https://fal/up.png", "upscaled": true})
		hit := len(calls) > 0
		if hit != c.called {
			t.Fatalf("%#v %v", c.upscale, hit)
		}
	}
}
