package auxiliary

import (
	"time"

	"github.com/nickheyer/icebath/internal/auxiliary"
	"github.com/nickheyer/icebath/internal/config"
	visionconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/vision/config"
)

type tasks struct{}

func Register(registry *auxiliary.Registry) {
	registry.Register(tasks{})
}

func (tasks) Tasks() []auxiliary.Task {
	return []auxiliary.Task{
		{Name: visionconfig.Task, Title: "Image analysis", About: "Model that looks at images for the agent", Timeout: 120, Vision: true, Critical: true,
			Settings: []config.Setting{
				{Key: visionconfig.DownloadTimeout, Title: "Download timeout", About: "Seconds a download of an image or video may go without data", Default: 30 * time.Second},
				{Key: visionconfig.Temperature, Title: "Vision temperature", About: "How varied image and video descriptions may be, 0 to 2; low values give steadier ones", Default: 0.1},
				{Key: visionconfig.MaxConcurrency, Title: "Image workers", About: "How many images are prepared at the same time; 0 for one per processor core", Default: 0},
			}},
		{Name: visionconfig.VideoTask, Title: "Video analysis", About: "Model that watches videos for the agent", Inherit: visionconfig.Task, MinTimeout: 180, Vision: true, Critical: true},
	}
}
