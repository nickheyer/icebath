package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/internal/generation"
	xaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/generators/xai/config"
	xaigenerate "github.com/nickheyer/icebath/internal/plugins/defaults/generators/xai/generate"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Tools of xAI's own video workflows, which video_generate does not cover
const (
	Edit   = "xai_video_edit"
	Extend = "xai_video_extend"
)

type registrant struct {
	services *xaigenerate.Services
}

func Register(registry *tools.Registry, services *xaigenerate.Services) {
	registry.Register(registrant{services})
}

var videoURL = tools.Property("string", "Public HTTPS MP4 URL of the source video — the `video` or `public_url` from a prior xAI Imagine result.")

// Schema of an operation on a stored video
func schema(verb, noun, action string, extra map[string]any) map[string]any {
	properties := map[string]any{
		"prompt":    tools.Property("string", "Instruction for how xAI should "+action+" the source video."),
		"video_url": videoURL,
	}
	for name, property := range extra {
		properties[name] = property
	}
	return tools.Schema(fmt.Sprintf("%s an existing video with xAI Imagine. This is separate from `video_generate` because video %s is provider-specific. "+
		"`video_url` must be the public HTTPS MP4 URL from a prior Imagine result (`video` or `public_url` on files-cdn).", verb, noun), []string{"prompt", "video_url"}, properties)
}

func (r registrant) Tools() []tools.Def {
	check := func(env *tools.Env) bool {
		return generation.On(env.Config, generation.Video) && generation.Chosen(env.Config, generation.Video) == xaiconfig.Name && r.services.Ready(env.Config)
	}
	return []tools.Def{
		{Name: Edit, Toolset: "video_gen", Title: "Edit video", About: "Change a video that xAI made earlier", Group: "Videos",
			Icon: tools.IconEdit, Subject: "prompt", Schema: schema("Edit", "editing", "modify", nil), Handler: r.edit, Check: check, Lacks: lacks, Unfocused: true},
		{Name: Extend, Toolset: "video_gen", Title: "Extend video", About: "Make a video that xAI made earlier longer", Group: "Videos",
			Icon: tools.IconEdit, Subject: "prompt",
			Schema: schema("Extend", "extension", "continue", map[string]any{
				"duration": tools.Property("integer", "Desired extension duration in seconds. xAI clamps this to its supported range."),
			}), Handler: r.extend, Check: check, Lacks: lacks, Unfocused: true},
	}
}

func (r registrant) edit(ctx context.Context, call *tools.Call) string {
	return r.run(call, "edit", func(job *generation.Job, prompt, video string) generation.Result {
		return r.services.Edit(ctx, job, prompt, video)
	})
}

func (r registrant) extend(ctx context.Context, call *tools.Call) string {
	seconds := generation.Whole(call.Args["duration"])
	return r.run(call, "extend", func(job *generation.Job, prompt, video string) generation.Result {
		return r.services.Extend(ctx, job, prompt, video, seconds)
	})
}

// Checks the prompt, the source video and that xAI makes the profile's videos, then runs the operation
func (r registrant) run(call *tools.Call, operation string, do func(job *generation.Job, prompt, video string) generation.Result) string {
	prompt, _ := call.Args["prompt"].(string)
	video, _ := call.Args["video_url"].(string)
	prompt, video = strings.TrimSpace(prompt), strings.TrimSpace(video)
	lower := strings.ToLower(video)
	switch {
	case prompt == "":
		return tools.ErrorResult(fmt.Errorf("prompt is required for xAI video %s", operation))
	case !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://"):
		return tools.ErrorResult(errors.New("video_url must be a public HTTPS MP4 URL (the `video`/`public_url` from a prior Imagine result)"))
	case !generation.On(call.Env.Config, generation.Video) || generation.Chosen(call.Env.Config, generation.Video) != xaiconfig.Name:
		return tools.JSON(map[string]any{"success": false, "error": "xAI video edit/extend tools require videos to be on with `video_gen.provider` set to `xai`: " +
			"the user turns on Make videos and picks xAI with /videos.", "error_type": "provider_not_configured", "provider": xaiconfig.Name})
	}
	result := do(&generation.Job{Config: call.Env.Config}, prompt, video)
	if !result.OK {
		result.Prompt = strs.Filled(result.Prompt, prompt)
	}
	return tools.JSON(result.Envelope(generation.Video))
}

// What the xAI video tools lack to run: videos on, xAI chosen as their service, and set up
func lacks(*tools.Env) string {
	return "It needs Make videos on, with xAI as the video service and its account connected."
}
