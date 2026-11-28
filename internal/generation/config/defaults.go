package config

import "github.com/nickheyer/icebath/internal/config"

// Registers the generation settings, the service choices offered from the services that make each kind. The service in use and whether
// videos are made are changed on the pages of Images and Videos, and a model is a setting of each service's own
func Register(cfg *config.Config, images, videos func() []config.Option) {
	cfg.Register(config.Sections{{
		Key:   ImageKey,
		Title: "Images",
		About: "Which service makes images for the agent",
		Group: config.GroupMedia,
		Order: 10,
		Settings: []config.Setting{
			{Key: ImageProvider, Title: "Service", About: "Which service makes images when the agent asks for one.", Default: "", Options: images,
				Custom: true},
			{Key: ImageParallel, Title: "Most at once", About: "How many images are made at the same time when the agent asks for several.", Default: 4,
				Counts: "images", Least: config.Num(MinParallel), Most: config.Num(MaxParallel)},
			{Key: ImageDebug, Title: "Debug log", About: "Record each image request in a JSON file in the logs folder of the profile.", Default: false,
				Env: []string{"IMAGE_TOOLS_DEBUG"}, Advanced: true},
		},
	}})
	cfg.Register(config.Sections{{
		Key:   VideoKey,
		Title: "Videos",
		About: "Which service makes videos for the agent",
		Group: config.GroupMedia,
		Order: 20,
		Settings: []config.Setting{
			{Key: VideoEnabled, Title: "Make videos", About: "Let the agent make videos from text, a still image or reference images.", Default: false,
				Custom: true},
			{Key: VideoProvider, Title: "Service", About: "Which service makes videos when the agent asks for one.", Default: "", Options: videos,
				Custom: true},
		},
	}})
}
