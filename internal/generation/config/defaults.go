package config

import "github.com/nickheyer/icebath/internal/config"

// Registers the generation settings, the service choices offered from the services that make each kind
func Register(cfg *config.Config, images, videos func() []config.Option) {
	cfg.Register(config.Sections{{
		Key:   ImageKey,
		Title: "Image generation",
		About: "Pictures the agent draws with image_generate: the service that draws them and its model",
		Settings: []config.Setting{
			{Key: ImageProvider, Title: "Image generation", About: "Which service draws pictures when the agent makes an image. Automatic takes the first service with a key", Default: "", Options: images},
			{Key: ImageModel, Title: "Image model", About: "Model the image service draws with. Empty uses the service's default", Default: ""},
			{Key: ImageParallel, Title: "Pictures at once", About: "Pictures drawn at the same time when the agent asks for several, 1 to 8", Default: 4},
			{Key: ImageDebug, Title: "Image debug log", About: "Record every image_generate call to a JSON file in the profile's logs folder", Default: false, Env: []string{"IMAGE_TOOLS_DEBUG"}},
		},
	}})
	cfg.Register(config.Sections{{
		Key:   VideoKey,
		Title: "Video generation",
		About: "Videos the agent makes with video_generate: whether it may, the service that makes them and its model",
		Settings: []config.Setting{
			{Key: VideoEnabled, Title: "Video generation", About: "Offer the agent video_generate, which makes videos from text, a still image or reference images, on the first available service. Choosing a video service offers it as well", Default: false},
			{Key: VideoProvider, Title: "Video service", About: "Plugin that makes videos when the agent asks for one. Automatic takes the first available service", Default: "", Options: videos},
			{Key: VideoModel, Title: "Video model", About: "Model the video service makes videos with. Empty uses the service's default", Default: ""},
		},
	}})
}
