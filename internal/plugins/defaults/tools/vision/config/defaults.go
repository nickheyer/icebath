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
		Settings: []config.Setting{
			{Key: EmbedTargetBytes, Title: "Image size budget", About: "Bytes an image is shrunk to when the model sees it itself, 65536 to 4194304", Default: 262144},
			{Key: MaxCallsPerImage, Title: "Looks per image", About: "Most times the same image may be loaded in one session; 0 for no limit", Default: 0},
			{Key: Video, Title: "Video analysis", About: "Offer the agent video_analyze, which sends videos to the video helper model", Default: false},
			{Key: Debug, Title: "Vision debug log", About: "Record every image and video analysis in a JSON file under logs", Default: false, Env: []string{"VISION_TOOLS_DEBUG"}},
		},
	}})
}
