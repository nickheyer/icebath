package config

import (
	"github.com/nickheyer/icebath/internal/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: toolsconfig.Key,
		Title:  "Vision",
		About:  "Images and videos the agent looks at",
		Order:  100,
		Settings: []config.Setting{
			{Key: Video, Title: "Video analysis", About: "Let the agent analyze videos with the video helper model.", Default: false},
			{Key: MaxCallsPerImage, Title: "Looks per image", About: "How many times the same image may be loaded in one session.",
				Help: "Until you set this, subagents get 3 looks per image.", Default: 0, Zero: "No limit", Counts: "looks", Least: config.Num(0)},
			{Key: EmbedTargetBytes, Title: "Image size", About: "The size that an image is shrunk to when the model looks at the image itself.", Default: 262144,
				Counts: config.Bytes, Least: config.Num(65536), Most: config.Num(4194304), Advanced: true},
			{Key: Debug, Title: "Debug log", About: "Record every image and video analysis in a JSON file in the logs folder.", Default: false,
				Env: []string{"VISION_TOOLS_DEBUG"}, Advanced: true},
		},
	}})
}
