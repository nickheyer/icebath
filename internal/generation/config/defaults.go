package config

import "github.com/nickheyer/icebath/internal/config"

// Registers the generation settings, the service choices offered from the services that make each kind
func Register(cfg *config.Config, images, videos func() []config.Option) {
	cfg.Register(config.Sections{{
		Key:   ImageKey,
		Title: "Images",
		About: "Which service makes images for the agent",
		Group: config.GroupMedia,
		Order: 10,
		Settings: []config.Setting{
			{Key: ImageProvider, Title: "Service", About: "Which service makes images when the agent asks for one.", Default: "", Options: images,
				Slash: "imagegen"},
			{Key: ImageModel, Title: "Model", About: "The model the image service makes images with.", Default: "", Zero: "The service's default",
				Slash: "imagegen"},
			{Key: ImageParallel, Title: "Most at once", About: "How many images are made at the same time when the agent asks for several.", Default: 4,
				Counts: "images", Least: config.Num(MinParallel), Most: config.Num(MaxParallel), Advanced: true},
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
			{Key: VideoEnabled, Title: "Make videos", About: "Let the agent make videos from text, a still image or reference images.",
				Help: "Picking a service under Service does the same.", Default: false, Slash: "videogen"},
			{Key: VideoProvider, Title: "Service", About: "Which service makes videos when the agent asks for one.", Default: "", Options: videos,
				Slash: "videogen"},
			{Key: VideoModel, Title: "Model", About: "The model the video service makes videos with.", Default: "", Zero: "The service's default",
				Slash: "videogen"},
		},
	}})
}
